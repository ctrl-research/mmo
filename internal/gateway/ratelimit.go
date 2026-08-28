package gateway

import "time"

// rateLimiter is a token bucket over a one-second window.
//
// Deliberately not a dependency: input rate limiting needs to be cheap enough
// to run on every message, and this is a handful of arithmetic with no
// allocation and no lock -- it is only ever touched by one session's reader
// goroutine.
type rateLimiter struct {
	perSecond float64
	tokens    float64
	last      time.Time
	now       func() time.Time
}

func newRateLimiter(perSecond int) *rateLimiter {
	now := time.Now
	return &rateLimiter{
		perSecond: float64(perSecond),
		// Start full so a legitimate burst on connect is not punished.
		tokens: float64(perSecond),
		last:   now(),
		now:    now,
	}
}

// allow consumes a token, reporting whether one was available.
func (r *rateLimiter) allow() bool {
	now := r.now()
	elapsed := now.Sub(r.last).Seconds()
	r.last = now

	r.tokens = min(r.tokens+elapsed*r.perSecond, r.perSecond)
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}
