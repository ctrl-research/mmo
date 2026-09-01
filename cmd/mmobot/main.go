// Command mmobot drives a population of headless players at a running server.
//
// It exists to answer one question the unit tests cannot: what happens to tick
// times when a thousand people are actually playing. Every bot takes the same
// path a browser does -- sign in, pick a character, ask for a ticket, open a
// socket, say hello -- so what the server sees is the real protocol at the
// real rate, not a synthetic hammer on one endpoint.
//
//	mmobot --server=http://localhost:8088 --bots=200 --duration=2m
//
// The server must be running with --dev-auth. That is the only way to get a
// ticket without a password per bot, and it is why this is a load tool rather
// than something to point at anything real.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mmobot: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	server    string
	bots      int
	ramp      time.Duration
	duration  time.Duration
	inputRate time.Duration
	report    time.Duration
	prefix    string
	reconnect bool
}

func run() error {
	var opt options
	flag.StringVar(&opt.server, "server", "http://localhost:8088",
		"base URL of the gateway to load")
	flag.IntVar(&opt.bots, "bots", 50, "how many players to simulate")
	flag.DurationVar(&opt.ramp, "ramp", 10*time.Second,
		"how long to take connecting them all; 0 connects everyone at once")
	flag.DurationVar(&opt.duration, "duration", time.Minute,
		"how long to hold the load once everyone is in; 0 runs until interrupted")
	flag.DurationVar(&opt.inputRate, "input-rate", 50*time.Millisecond,
		"how often each bot sends movement, matching a client's input tick")
	flag.DurationVar(&opt.report, "report", 5*time.Second, "how often to print a line")
	flag.BoolVar(&opt.reconnect, "reconnect", true,
		"come back after losing a connection, the way a real client does")
	flag.StringVar(&opt.prefix, "prefix", "bot",
		"name prefix; bots are <prefix>NNNN and their characters are reused between runs")
	flag.Parse()

	if opt.bots < 1 {
		return errors.New("--bots must be at least 1")
	}
	opt.server = strings.TrimSuffix(opt.server, "/")

	// Interrupt stops the run and still prints the summary. A load test killed
	// without its numbers is a load test that has to be run again.
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	if opt.duration > 0 {
		var cancel context.CancelFunc
		// The duration is time *under load*, so the ramp is added to it.
		// Otherwise asking for two minutes with a ninety second ramp measures
		// thirty seconds of the thing you asked about.
		ctx, cancel = context.WithTimeout(ctx, opt.ramp+opt.duration)
		defer cancel()
	}

	hs, err := askHandshake(ctx, opt.server)
	if err != nil {
		return err
	}

	fmt.Printf("loading %s with %d bots (ramp %s, then %s)\n",
		opt.server, opt.bots, opt.ramp, holdFor(opt.duration))
	fmt.Printf("  protocol %d, content %s\n", hs.Protocol, hs.Content)

	st := newStats()
	started := time.Now()

	done := make(chan struct{})
	go func() {
		defer close(done)
		reportLoop(ctx, st, started, opt.report)
	}()

	// The summary is measured from the end of the ramp, not from the start.
	//
	// A run that spends forty-five seconds connecting a thousand bots and then
	// ninety seconds at full load has a whole-run average well below the steady
	// rate, because most of that first window had most of the bots not yet
	// connected. The average was labelled as such and still read as the steady
	// figure -- by me, in two write-ups, as an unexplained shortfall in the
	// snapshot rate. It was the ramp.
	go func() {
		sleep(ctx, opt.ramp)
		if ctx.Err() == nil {
			st.markSteady()
		}
	}()

	var wg sync.WaitGroup
	for i := range opt.bots {
		select {
		case <-ctx.Done():
		default:
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			runBot(ctx, opt, i, hs, st)
		}()

		// Staggered, because a thousand simultaneous sign-ins is a load test
		// of the login path rather than of the game. Jittered so they do not
		// arrive in a metronome either.
		if opt.ramp > 0 && i < opt.bots-1 {
			gap := opt.ramp / time.Duration(opt.bots)
			sleep(ctx, gap/2+time.Duration(rand.Int64N(int64(gap)+1)))
		}
	}

	wg.Wait()
	stop()
	<-done

	final := st.summarise(started)
	fmt.Print(final.summary(opt.bots))

	// A run where nobody got in is a failure, however clean the exit looked.
	if final.Peak == 0 && final.Failed > 0 {
		return fmt.Errorf("no bot ever connected; first failure: %s", final.FirstError)
	}
	return nil
}

// runBot is one bot's whole life, including coming back after losing its
// connection.
//
// A real client reconnects, so a bot that gave up on its first disconnect would
// make a node dying look like permanent data loss -- the players gone because
// nobody tried to come back rather than because anything failed to recover. It
// is also what makes "characters recover" measurable rather than asserted.
func runBot(ctx context.Context, opt options, i int, hs handshake, st *stats) {
	b := newBot(fmt.Sprintf("%s%04d", opt.prefix, i), opt.server, hs, st)

	for attempt := 0; ctx.Err() == nil; attempt++ {
		if attempt > 0 {
			if !opt.reconnect {
				return
			}
			// Backoff, jittered. A node dying disconnects everyone it was
			// holding at once, and a thousand clients retrying in lockstep is a
			// thundering herd at the moment the cluster has least to spare --
			// which would measure the herd rather than the recovery.
			//
			// The first wait is deliberately longer than a lease TTL: until the
			// dead node's lease expires the character genuinely is still owned,
			// and asking sooner only collects refusals.
			wait := reconnectBackoff(attempt)
			sleep(ctx, wait/2+time.Duration(rand.Int64N(int64(wait)+1)))
			if ctx.Err() != nil {
				return
			}
			st.reconnects.Add(1)
		}

		if !enter(ctx, b, st, attempt) {
			continue
		}

		st.join()
		b.play(ctx, opt.inputRate)
		st.leave()
	}
}

// enter takes a bot from nothing to in the world, reporting whether it arrived.
func enter(ctx context.Context, b *bot, st *stats, attempt int) bool {
	st.connecting.Add(1)
	defer st.connecting.Add(-1)

	for _, step := range []func(context.Context) error{b.signIn, b.connect} {
		if err := step(ctx); err != nil {
			if ctx.Err() == nil {
				if attempt == 0 {
					st.fail(err)
				} else {
					// A failed *re*connect is a different fact from never
					// having got in: the cluster is mid-recovery, and refusing
					// a character whose lease has not expired is correct.
					st.retryFailed(err)
				}
			}
			return false
		}
	}
	return true
}

// reconnectBackoff grows to a ceiling, starting above a lease TTL.
func reconnectBackoff(attempt int) time.Duration {
	// A character whose node died is still owned until its lease lapses, so
	// there is nothing to be gained by asking before then.
	const first = 35 * time.Second
	const ceiling = 2 * time.Minute

	wait := first
	for range attempt - 1 {
		wait *= 2
		if wait >= ceiling {
			return ceiling
		}
	}
	return wait
}

func reportLoop(ctx context.Context, st *stats, started time.Time, every time.Duration) {
	if every <= 0 {
		<-ctx.Done()
		return
	}

	tick := time.NewTicker(every)
	defer tick.Stop()

	prev := &counters{}
	last := started

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			var r report
			r, prev = st.snapshot(now.Sub(started), now.Sub(last), prev)
			last = now
			fmt.Println(r.line())
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func holdFor(d time.Duration) string {
	if d <= 0 {
		return "until interrupted"
	}
	return d.String()
}

// randIntn is here so stats.go does not need its own import of math/rand.
func randIntn(n int) int { return rand.IntN(n) }
