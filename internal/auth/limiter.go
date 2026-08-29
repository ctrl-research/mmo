package auth

import (
	"sync"
	"time"
)

// attemptLimiter throttles sign-in attempts by source address.
//
// This sits alongside the per-account lockout, and the two catch different
// things. A per-account counter never sees a broad sweep across many
// usernames from one source; an address limit never sees a distributed attack
// on one account. Neither alone is enough.
//
// Deliberately not a dependency: this is a map and a timestamp, and the
// behaviour worth getting right -- that a legitimate mistyped password does
// not lock someone out of their own game -- is a tuning decision, not an
// algorithm.
type attemptLimiter struct {
	mu      sync.Mutex
	buckets map[string]*attemptBucket

	limit  int
	window time.Duration
	now    func() time.Time
}

type attemptBucket struct {
	count int
	start time.Time
}

// Address-level limits.
//
// Generous, because several people behind one household or office address
// share a bucket. The per-account lockout is the tighter control; this one
// only blunts a sweep.
const (
	loginAttemptLimit  = 30
	loginAttemptWindow = time.Minute
)

func newAttemptLimiter(limit int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{
		buckets: make(map[string]*attemptBucket),
		limit:   limit,
		window:  window,
		now:     time.Now,
	}
}

// allow records an attempt and reports whether it may proceed.
func (l *attemptLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) > l.window {
		l.buckets[key] = &attemptBucket{count: 1, start: now}
		return true
	}

	b.count++
	return b.count <= l.limit
}

// reset clears an address's count after a successful sign-in, so someone who
// mistyped a few times is not throttled once they get it right.
func (l *attemptLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// sweepLocked discards expired buckets.
//
// Called on every attempt, which keeps the map bounded without a background
// goroutine. Without it the map grows with every distinct source address --
// which is to say, unboundedly, under exactly the attack it exists to blunt.
func (l *attemptLimiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.start) > l.window {
			delete(l.buckets, k)
		}
	}
}

// tracked reports how many addresses are being tracked, for tests.
func (l *attemptLimiter) tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
