package world

import (
	"context"
	"sync"
	"time"

	"github.com/ctrl-research/mmo/internal/directory"
)

// Leaving on purpose.
//
// The chaos work established what an unannounced death costs: every character
// waits out a lease TTL before anyone can pick it up, the rooms stay registered
// until something notices they are gone, and whatever happened since the last
// checkpoint is lost. None of that is necessary when the process knows it is
// going.
//
// A drain is the same shutdown with the order reversed. Stop being sent work,
// finish what is in hand, and only then stop existing.

// DrainTimeout bounds a drain.
//
// Kubernetes sends SIGKILL after terminationGracePeriodSeconds (30 by default),
// so a drain that has not finished by then is one that gets interrupted -- and
// an interrupted drain is worse than none, because it is a drain that told half
// its players to reconnect and abandoned the other half mid-checkpoint. This is
// deliberately under that.
const DrainTimeout = 20 * time.Second

// drainConcurrency is how many characters are seen off at once, when the
// database pool size is unknown.
//
// Each one is a final checkpoint, so they are database writes above all else.
// Doing more of them at once than there are connections does not make them
// faster: it makes them queue inside the pool, where the wait is invisible and
// counts against the drain's deadline just the same. A first attempt used
// sixteen against a pool of ten, and a quarter of the characters spent the
// entire budget waiting for a connection and were never seen off at all.
const drainConcurrency = 8

// characterBudget is how long one character may take.
//
// A slice of what is left rather than the whole of it. One character stuck on a
// database that is not answering must not spend the budget for the other
// forty-nine: a drain that sees off most of them is worth much more than one
// that gambles everything on the slowest.
func characterBudget(remaining time.Duration, characters, concurrency int) time.Duration {
	if characters <= 0 {
		return remaining
	}
	rounds := (characters + concurrency - 1) / concurrency
	per := remaining / time.Duration(rounds)

	// Never so small that a healthy write cannot finish. If the budget is too
	// tight for the number of characters, the drain will run out -- which is
	// the truth, and is logged -- rather than timing out every write.
	const floor = 2 * time.Second
	if per < floor {
		return floor
	}
	return per
}

// Drain sees this node's characters out before it stops.
//
// In order, because the order is the whole point:
//
//  1. Withdraw from placement, so nothing new arrives at a process that is
//     leaving.
//  2. Stop accepting characters handed over by other nodes, for the same
//     reason.
//  3. For each character: checkpoint it, release its lease, and only then tell
//     the player to reconnect.
//  4. Give up the rooms, so nothing is left registered on a node that has gone.
//
// Step 3 is the one with a trap in it. Telling the player first would be
// friendlier -- their client could be reconnecting while this node finishes
// writing -- but a client that reconnects fast lands on another node and
// acquires the lease before the checkpoint here has been written, and the
// fencing predicate then correctly rejects it. The player would lose exactly
// the progress a drain exists to protect. So: write, release, then speak.
func (n *Node) Drain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, DrainTimeout)
	defer cancel()

	started := time.Now()
	deadline := started.Add(DrainTimeout)

	// Nothing new. Withdrawing first means the count below is a count that
	// only shrinks.
	if err := n.dir.Withdraw(ctx); err != nil {
		n.log.Error("withdrawing from placement while draining", "err", err)
	}
	if n.enterSub != nil {
		n.enterSub.Close()
		n.enterSub = nil
	}
	if n.transferSub != nil {
		n.transferSub.Close()
		n.transferSub = nil
	}

	sessions := n.localSessions()
	n.log.Info("draining", "characters", len(sessions), "rooms", n.Rooms())

	// Every session is told to wind down before any of them is waited for.
	//
	// Closing a session waits for its own goroutine to finish what it is doing
	// -- a transfer halfway through moving a character must not be cut in half
	// -- and that wait is bounded by a transfer's timeout. Done per session
	// inside the loop below, a node holding fifty characters would need fifty
	// of those waits in series, which no grace period is long enough for. This
	// makes them concurrent: the wind-down starts everywhere at once, and the
	// loop below finds most sessions already still.
	for _, s := range sessions {
		s.stopWork()
	}

	// Sized to the database pool. See drainConcurrency.
	concurrency := drainConcurrency
	if n.store != nil {
		if max := n.store.MaxConns(); max > 0 {
			concurrency = max
		}
	}

	budget := characterBudget(time.Until(deadline), len(sessions), concurrency)

	var wg sync.WaitGroup
	slots := make(chan struct{}, concurrency)

	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			each, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			s.drain(each)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		// Reported rather than waited out. The process is about to be killed
		// either way, and a log line naming the number is what tells whoever
		// is looking at a rolling deploy that the grace period is too short.
		n.log.Error("drain did not finish in time; some characters were not seen off",
			"budget", DrainTimeout, "characters", len(sessions),
			"each", budget, "at_once", concurrency)
	}

	n.releaseRooms(ctx)
	n.log.Info("drained", "characters", len(sessions), "took", time.Since(started).Round(time.Millisecond))
}

// releaseRooms gives up every instance this node hosts.
//
// Otherwise they stay registered on a node that no longer exists, holding slot
// counts for players who are gone -- which is the state a killed node leaves
// behind and which placement has to reap on its way past. A node that is
// leaving on purpose can simply not leave it.
func (n *Node) releaseRooms(ctx context.Context) {
	n.mu.Lock()
	instances := make([]directory.InstanceID, 0, len(n.rooms))
	for id := range n.rooms {
		instances = append(instances, id)
	}
	n.mu.Unlock()

	for _, id := range instances {
		if err := n.dir.Release(ctx, id); err != nil {
			n.log.Warn("releasing a room while draining", "instance", uint64(id), "err", err)
		}
	}
}

// slowDrain is how long one character may take before it is worth saying so.
//
// A drain that runs out of budget needs to name what it was waiting for.
// "Some characters were not seen off" is a fact nobody can act on; the
// character and the time it took is a place to start.
const slowDrain = 2 * time.Second

// drain sees one character out: checkpointed, released, and told why.
func (s *Session) drain(ctx context.Context) {
	// Close does the durable half -- the final checkpoint, the room slot, the
	// presence entry, and the lease. Releasing the lease is what makes the
	// player's reconnect immediate instead of a wait for it to lapse.
	started := time.Now()
	s.Close(ctx)

	if took := time.Since(started); took > slowDrain {
		s.log.Warn("this character took a long time to see off",
			"took", took.Round(time.Millisecond))
	}

	// And only now the player, for the reason in Node.Drain.
	s.mu.Lock()
	onLost := s.onLost
	s.mu.Unlock()

	if onLost != nil {
		onLost("the server is restarting; reconnecting now")
	}
}
