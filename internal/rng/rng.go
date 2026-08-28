// Package rng provides the simulation's deterministic random numbers.
//
// Every roll the game makes -- damage variance, critical hits, drop tables,
// elite modifiers, AI decisions -- comes from a room's own generator, advanced
// only inside its tick loop. Nothing here reads a global source, a clock, or
// crypto/rand.
//
// That constraint buys room replay: a room's entire history is reproducible
// from its seed plus its input log, tick for tick. Replay turns "a boss
// desynced last Tuesday" and "was that drop legitimate" from unanswerable
// questions into something you can run again and watch. It is also a free
// regression harness for the simulation.
//
// Two rules follow, and both are easy to break by accident:
//
//   - Never call math/rand or time.Now inside a tick. A single such call makes
//     every subsequent roll in that room unreproducible.
//   - Roll in a fixed order. Two rolls swapped between ticks produce a
//     different world from the same seed.
package rng

import "math/bits"

// Source is a PCG32 generator: a 64-bit LCG whose output is permuted before
// being returned.
//
// PCG is chosen over a plain LCG (whose low bits are notoriously
// non-random), over xorshift (which fails some statistical tests), and over
// math/rand (whose algorithm is not guaranteed stable across Go versions --
// which would silently invalidate every recorded replay on a toolchain
// upgrade). PCG32 is small, fast, has good statistical properties, and is
// specified precisely enough that this implementation is stable forever.
//
// A Source is not safe for concurrent use. It does not need to be: it belongs
// to exactly one room, which is single-goroutine by construction.
type Source struct {
	state uint64
	inc   uint64 // stream selector; must be odd
}

const (
	pcgMultiplier = 6364136223846793005
	defaultStream = 1442695040888963407
)

// New returns a generator seeded deterministically.
//
// The same seed always produces the same sequence, on every platform and every
// Go version. That is the whole point.
func New(seed uint64) *Source {
	return NewStream(seed, defaultStream)
}

// NewStream returns a generator on an independent stream.
//
// Distinct streams from the same seed produce unrelated sequences, which is
// how a room gives each layer its own spawn rolls without one player's drops
// depending on how many other players happen to be in the room.
func NewStream(seed, stream uint64) *Source {
	s := &Source{
		// The increment must be odd for the LCG to have full period.
		inc: (stream << 1) | 1,
	}
	s.next()
	s.state += seed
	s.next()
	return s
}

// next advances the state and returns the permuted output.
func (s *Source) next() uint32 {
	old := s.state
	s.state = old*pcgMultiplier + s.inc

	// The permutation is what makes the high-quality output: xorshift the
	// state, then rotate by an amount taken from its top bits.
	xorshifted := uint32(((old >> 18) ^ old) >> 27)
	rot := uint32(old >> 59)
	return bits.RotateLeft32(xorshifted, -int(rot))
}

// Uint32 returns a uniformly distributed value.
func (s *Source) Uint32() uint32 { return s.next() }

// Uint64 returns a uniformly distributed value, drawn as two Uint32s.
func (s *Source) Uint64() uint64 {
	hi := uint64(s.next())
	lo := uint64(s.next())
	return hi<<32 | lo
}

// IntN returns a uniformly distributed value in [0, n).
//
// The obvious implementation, Uint32() % n, is subtly biased whenever n does
// not divide 2^32: the low values occur slightly more often. For a 1-in-500
// unique drop that bias is invisible in testing and real in aggregate, so the
// rejection loop below is worth its cost.
//
// It panics for n <= 0, which is always a programming error rather than a
// value worth guessing at.
func (s *Source) IntN(n int) int {
	if n <= 0 {
		panic("rng: IntN requires a positive bound")
	}
	bound := uint32(n)

	// Reject the unusable low window so the remaining range divides evenly.
	threshold := (-bound) % bound
	for {
		v := s.next()
		if v >= threshold {
			return int(v % bound)
		}
	}
}

// Range returns a uniformly distributed value in [lo, hi], inclusive.
//
// Inclusive because that is how content is authored -- "damage 40 to 60" means
// 60 is attainable -- and an off-by-one here would quietly shave the top roll
// off every item and every hit in the game.
func (s *Source) Range(lo, hi int) int {
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo + s.IntN(hi-lo+1)
}

// Chance reports whether an event with probability num/den occurs.
//
// Probabilities are integer ratios rather than floats throughout the
// simulation. Content authors write 0.15 in TOML because that is readable, and
// the loader converts it once to parts-per-million; from then on the roll is
// integer arithmetic and reproduces exactly.
func (s *Source) Chance(num, den int) bool {
	if num <= 0 {
		return false
	}
	if num >= den {
		return true
	}
	return s.IntN(den) < num
}

// PPM reports whether an event with probability ppm parts-per-million occurs.
// It is the form drop tables use after loading.
func (s *Source) PPM(ppm int) bool { return s.Chance(ppm, 1_000_000) }

// Pick returns the index of one entry chosen in proportion to its weight.
//
// Returns -1 when there is nothing to choose from or every weight is zero,
// rather than picking arbitrarily -- an empty weighted table is a content bug,
// and silently returning index 0 would hide it.
func (s *Source) Pick(weights []int) int {
	total := 0
	for _, w := range weights {
		if w > 0 {
			total += w
		}
	}
	if total <= 0 {
		return -1
	}

	roll := s.IntN(total)
	for i, w := range weights {
		if w <= 0 {
			continue
		}
		roll -= w
		if roll < 0 {
			return i
		}
	}
	// Unreachable: the roll is bounded by the total.
	return len(weights) - 1
}

// Shuffle permutes a slice in place using Fisher-Yates.
func Shuffle[T any](s *Source, items []T) {
	for i := len(items) - 1; i > 0; i-- {
		j := s.IntN(i + 1)
		items[i], items[j] = items[j], items[i]
	}
}

// State returns the generator's full internal state.
//
// Recording this alongside a tick number lets a replay resume from the middle
// of a session rather than only from the beginning, which matters when the
// interesting event is twenty minutes in.
func (s *Source) State() (state, inc uint64) { return s.state, s.inc }

// Restore sets the generator's state, resuming a recorded sequence exactly.
func (s *Source) Restore(state, inc uint64) {
	s.state = state
	s.inc = inc
}

// Clone returns an independent copy positioned at the same point in the
// sequence, for speculative rolls that must not disturb the room's stream.
func (s *Source) Clone() *Source {
	c := *s
	return &c
}
