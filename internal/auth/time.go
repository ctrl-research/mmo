package auth

import "time"

// nowUTC is the single clock reference outside the simulation.
//
// The simulation itself never reads a clock -- its only notion of time is the
// tick number, which is what makes replay possible. Authentication is the
// opposite: expiry is inherently wall-clock, and there is nothing to
// reproduce.
func nowUTC() time.Time { return time.Now().UTC() }
