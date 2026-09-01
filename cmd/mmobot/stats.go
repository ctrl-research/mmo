package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// What the load test actually reports.
//
// The server's own numbers -- tick duration, overruns, entities, players --
// come from its /metrics endpoint and are the ones that say whether it is
// keeping up. These are the other half: what the population looked like from
// outside, so a tick p99 can be read against the load that produced it.
//
// Counters are atomic and read without stopping anything. A report taken
// mid-flight can therefore be very slightly inconsistent between fields, which
// is the right trade for never pausing the thing being measured.
type stats struct {
	connected  atomic.Int64
	connecting atomic.Int64

	// peak is the most that were in at once.
	//
	// The summary is printed after the run has ended and every bot has hung
	// up, so `connected` is zero by then -- reporting it would say "0 of 1000
	// connected" for a run where all thousand played happily.
	peak        atomic.Int64
	failed      atomic.Int64
	disconnects atomic.Int64
	kicks       atomic.Int64

	reconnects atomic.Int64

	inputs    atomic.Uint64
	casts     atomic.Uint64
	snapshots atomic.Uint64
	frames    atomic.Uint64
	events    atomic.Uint64
	bytesIn   atomic.Uint64
	bytesOut  atomic.Uint64

	mu  sync.Mutex
	rtt latencies

	// lostExp is how much experience characters came back from a reconnect with
	// less of than they had when the connection went -- progress a checkpoint
	// had not yet written.
	//
	// Measured only across a reconnect. Experience drops in ordinary play too:
	// dying charges a share of it, and these bots die constantly.
	lostExp []uint64

	// firstError is kept because the hundredth failure is almost always the
	// first one repeated, and a wall of identical messages hides the one that
	// is different.
	firstError   string
	errorsByKind map[string]int

	// dropsByKind is why bots lost connections they had already made, which is
	// a different question from why they never got one.
	dropsByKind map[string]int

	// retriesByKind is why reconnects were refused, which during a recovery is
	// mostly the lease correctly not having expired yet.
	retriesByKind map[string]int
}

func newStats() *stats {
	return &stats{
		errorsByKind:  map[string]int{},
		dropsByKind:   map[string]int{},
		retriesByKind: map[string]int{},
	}
}

// join records a bot as connected and keeps the high-water mark.
func (s *stats) join() {
	now := s.connected.Add(1)
	for {
		peak := s.peak.Load()
		if now <= peak || s.peak.CompareAndSwap(peak, now) {
			return
		}
	}
}

func (s *stats) leave() { s.connected.Add(-1) }

// lostProgress records a character coming back with less than it had.
func (s *stats) lostProgress(exp uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lostExp = append(s.lostExp, exp)
}

func (s *stats) observeRTT(d time.Duration) {
	s.mu.Lock()
	s.rtt.add(d)
	s.mu.Unlock()
}

// dropped records a bot losing its connection mid-run, and why.
func (s *stats) dropped(err error) {
	s.disconnects.Add(1)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropsByKind[closeReason(err)]++
}

// kicked records the server closing a bot deliberately, with its stated reason.
func (s *stats) kicked(reason string) {
	s.kicks.Add(1)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropsByKind[reason]++
}

// closeReason pulls the interesting part out of a websocket error.
//
// The full text is a sentence of library plumbing wrapped around the one thing
// that matters -- the close code and what the server said -- and grouping on
// the whole string produces one bucket per bot.
func closeReason(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "status = "); i >= 0 {
		return msg[i+len("status = "):]
	}
	if len(msg) > 70 {
		return msg[:70]
	}
	return msg
}

// retryFailed records a reconnect attempt that did not get in.
//
// Counted apart from a first attempt: a cluster recovering from a dead node
// refusing a character whose lease has not expired yet is correct behaviour,
// and folding it in with "never got in" would report a healthy recovery as a
// wall of failures.
func (s *stats) retryFailed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retriesByKind[kindOf(err)]++
}

func (s *stats) fail(err error) {
	s.failed.Add(1)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstError == "" {
		s.firstError = err.Error()
	}
	s.errorsByKind[kindOf(err)]++
}

// kindOf collapses an error to something worth counting.
//
// Whole error strings carry a bot's name and a port number, so counting them
// raw produces one bucket per bot and a report nobody can read.
//
// A close status is preferred over the leading words, because when the server
// refuses a connection the interesting half is what it said on the way out.
// Grouping on the prefix instead produced 157 identical lines reading "waiting
// for the welcome" -- a count of how many bots were affected and no clue as to
// why, which is the one thing a load test is being run to find out.
func kindOf(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "status = "); i >= 0 {
		return msg[i+len("status = "):]
	}
	if i := strings.Index(msg, ":"); i > 0 && i < 40 {
		return msg[:i]
	}
	if len(msg) > 70 {
		return msg[:70]
	}
	return msg
}

// latencies is a bounded reservoir of round-trip samples.
//
// Bounded because a thousand bots pinging once a second for ten minutes is six
// hundred thousand samples, and the quantiles are just as good from a few
// thousand. Reservoir sampling rather than "keep the first N", which would
// describe only the ramp-up -- the least interesting part of the run.
type latencies struct {
	seen    int
	samples []time.Duration
}

const reservoirSize = 8192

func (l *latencies) add(d time.Duration) {
	l.seen++
	if len(l.samples) < reservoirSize {
		l.samples = append(l.samples, d)
		return
	}
	// Replace with probability reservoirSize/seen, which keeps every sample
	// equally likely to survive however long the run goes on.
	if j := randIntn(l.seen); j < reservoirSize {
		l.samples[j] = d
	}
}

func (l *latencies) quantile(q float64) time.Duration {
	if len(l.samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), l.samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// report is one line of what is happening, and the summary at the end.
type report struct {
	Elapsed     time.Duration
	Connected   int64
	Peak        int64
	Connecting  int64
	Failed      int64
	Disconnects int64
	Kicks       int64

	InputsPerSec    float64
	SnapshotsPerSec float64
	SnapshotsPerBot float64
	InPerSec        float64
	OutPerSec       float64

	Reconnects int64
	LostExp    []uint64

	RTTSamples int
	RTTp50     time.Duration
	RTTp99     time.Duration
	RTTmax     time.Duration

	FirstError    string
	ErrorsByKind  map[string]int
	DropsByKind   map[string]int
	RetriesByKind map[string]int
}

// snapshot takes a report over the window since the previous one.
func (s *stats) snapshot(elapsed, window time.Duration, prev *counters) (report, *counters) {
	now := &counters{
		inputs:    s.inputs.Load(),
		snapshots: s.snapshots.Load(),
		bytesIn:   s.bytesIn.Load(),
		bytesOut:  s.bytesOut.Load(),
	}

	per := func(a, b uint64) float64 {
		if window <= 0 {
			return 0
		}
		return float64(a-b) / window.Seconds()
	}

	connected := s.connected.Load()
	r := report{
		Elapsed:     elapsed.Round(time.Second),
		Connected:   connected,
		Peak:        s.peak.Load(),
		Connecting:  s.connecting.Load(),
		Failed:      s.failed.Load(),
		Disconnects: s.disconnects.Load(),
		Kicks:       s.kicks.Load(),
		Reconnects:  s.reconnects.Load(),

		InputsPerSec:    per(now.inputs, prev.inputs),
		SnapshotsPerSec: per(now.snapshots, prev.snapshots),
		InPerSec:        per(now.bytesIn, prev.bytesIn),
		OutPerSec:       per(now.bytesOut, prev.bytesOut),
	}
	// Per bot is the number that says whether the server is keeping up: the
	// total falls when bots drop, which looks like less work rather than worse
	// service. Measured against however many were actually in at the time --
	// the live count while running, the peak once everyone has hung up.
	against := connected
	if against == 0 {
		against = r.Peak
	}
	if against > 0 {
		r.SnapshotsPerBot = r.SnapshotsPerSec / float64(against)
	}

	s.mu.Lock()
	r.RTTSamples = s.rtt.seen
	r.RTTp50 = s.rtt.quantile(0.50)
	r.RTTp99 = s.rtt.quantile(0.99)
	r.RTTmax = s.rtt.quantile(1)
	r.FirstError = s.firstError
	r.ErrorsByKind = make(map[string]int, len(s.errorsByKind))
	for k, v := range s.errorsByKind {
		r.ErrorsByKind[k] = v
	}
	r.DropsByKind = make(map[string]int, len(s.dropsByKind))
	for k, v := range s.dropsByKind {
		r.DropsByKind[k] = v
	}
	r.LostExp = append([]uint64(nil), s.lostExp...)
	r.RetriesByKind = make(map[string]int, len(s.retriesByKind))
	for k, v := range s.retriesByKind {
		r.RetriesByKind[k] = v
	}
	s.mu.Unlock()

	return r, now
}

type counters struct {
	inputs, snapshots, bytesIn, bytesOut uint64
}

func (r report) line() string {
	return fmt.Sprintf(
		"%6s  bots %4d up / %3d connecting / %3d failed  "+
			"in %6.0f/s  snap %7.0f/s (%4.1f per bot)  "+
			"rtt p50 %5s p99 %6s  net %s/s up %s/s down",
		r.Elapsed, r.Connected, r.Connecting, r.Failed,
		r.InputsPerSec, r.SnapshotsPerSec, r.SnapshotsPerBot,
		round(r.RTTp50), round(r.RTTp99),
		bytes(r.OutPerSec), bytes(r.InPerSec),
	)
}

func (r report) summary(want int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n--- %s of load ---\n", r.Elapsed)
	fmt.Fprintf(&b, "  bots           %d of %d connected at once", r.Peak, want)
	if r.Failed > 0 {
		fmt.Fprintf(&b, ", %d never got in", r.Failed)
	}
	if r.Disconnects > 0 {
		fmt.Fprintf(&b, ", %d dropped mid-run", r.Disconnects)
	}
	if r.Kicks > 0 {
		fmt.Fprintf(&b, ", %d kicked", r.Kicks)
	}
	if r.Reconnects > 0 {
		fmt.Fprintf(&b, ", %d reconnected", r.Reconnects)
	}
	fmt.Fprintln(&b)

	if r.Reconnects > 0 || len(r.LostExp) > 0 {
		// The question a node dying asks: did anybody lose progress, and how
		// much. Silence here after a kill is the answer you want.
		if len(r.LostExp) == 0 {
			fmt.Fprintln(&b, "  progress       every character came back with everything it had")
		} else {
			var total, worst uint64
			for _, lost := range r.LostExp {
				total += lost
				if lost > worst {
					worst = lost
				}
			}
			fmt.Fprintf(&b, "  progress       %d of %d recoveries lost experience: "+
				"%d total, worst %d\n", len(r.LostExp), r.Reconnects, total, worst)
		}
	}

	fmt.Fprintf(&b, "  input          %.0f/s\n", r.InputsPerSec)
	fmt.Fprintf(&b, "  snapshots      %.0f/s, %.1f per bot per second\n",
		r.SnapshotsPerSec, r.SnapshotsPerBot)
	fmt.Fprintln(&b,
		"                 (rates are averaged over the whole run, ramp included)")
	fmt.Fprintf(&b, "  round trip     p50 %s, p99 %s, max %s (%d samples)\n",
		round(r.RTTp50), round(r.RTTp99), round(r.RTTmax), r.RTTSamples)
	fmt.Fprintf(&b, "  bandwidth      %s/s up, %s/s down\n",
		bytes(r.OutPerSec), bytes(r.InPerSec))

	if len(r.DropsByKind) > 0 {
		fmt.Fprintln(&b, "  dropped mid-run")
		for _, k := range byCount(r.DropsByKind) {
			fmt.Fprintf(&b, "    %4d  %s\n", r.DropsByKind[k], k)
		}
	}

	if len(r.RetriesByKind) > 0 {
		fmt.Fprintln(&b, "  reconnects refused")
		for _, k := range byCount(r.RetriesByKind) {
			fmt.Fprintf(&b, "    %4d  %s\n", r.RetriesByKind[k], k)
		}
	}

	if len(r.ErrorsByKind) > 0 {
		fmt.Fprintln(&b, "  never got in")
		for _, k := range byCount(r.ErrorsByKind) {
			fmt.Fprintf(&b, "    %4d  %s\n", r.ErrorsByKind[k], k)
		}
		fmt.Fprintf(&b, "  first          %s\n", r.FirstError)
	}

	fmt.Fprintln(&b,
		"\n  The server's own tick times are on its admin port at /metrics:\n"+
			"  mmo_room_tick_duration_seconds and mmo_room_tick_overruns_total say\n"+
			"  whether it kept up. These numbers say what it was keeping up with.")
	return b.String()
}

func round(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Millisecond:
		return d.Round(10 * time.Microsecond).String()
	case d < time.Second:
		return d.Round(100 * time.Microsecond).String()
	default:
		return d.Round(time.Millisecond).String()
	}
}

func bytes(perSec float64) string {
	const unit = 1024.0
	switch {
	case perSec < unit:
		return fmt.Sprintf("%.0fB", perSec)
	case perSec < unit*unit:
		return fmt.Sprintf("%.0fKiB", perSec/unit)
	default:
		return fmt.Sprintf("%.1fMiB", perSec/(unit*unit))
	}
}

// byCount orders keys by how often they happened, commonest first.
func byCount(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return counts[keys[i]] > counts[keys[j]] })
	return keys
}
