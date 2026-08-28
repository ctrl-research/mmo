package bus

import "sync/atomic"

// atomic64 is a tiny wrapper so counters read clearly at their use sites.
type atomic64 struct{ v atomic.Uint64 }

func (a *atomic64) add(n uint64) { a.v.Add(n) }
func (a *atomic64) load() uint64 { return a.v.Load() }
