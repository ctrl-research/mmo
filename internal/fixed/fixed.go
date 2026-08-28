// Package fixed implements deterministic fixed-point arithmetic.
//
// The simulation uses fixed-point rather than floating-point throughout.
// Floating-point results can differ between architectures, compilers, and
// optimisation levels; since the client runs the same simulation code compiled
// to WebAssembly and compares its prediction against the server's authority,
// any such difference shows up as rubber-banding that is very hard to trace.
// Integers do not have that problem.
//
// The representation is Q24.8: a signed 32-bit integer with 8 fractional bits.
//
//   - Precision: 1/256 of a world unit.
//   - Range:     roughly +/-8.3 million world units.
//
// Q24.8 is deliberately the same representation the wire protocol uses for
// positions, so serialisation is a plain copy with no rounding step, removing
// one more place where client and server could disagree.
package fixed

import "strconv"

// F is a Q24.8 fixed-point number. Add and subtract with + and -; use the
// methods below for multiply and divide, which need wider intermediates.
type F int32

const (
	// FracBits is the number of fractional bits in the representation.
	FracBits = 8

	// One is the value 1.0.
	One F = 1 << FracBits

	// Half is the value 0.5.
	Half F = One / 2

	// Zero is the value 0.0.
	Zero F = 0

	// Epsilon is the smallest representable positive value, 1/256.
	Epsilon F = 1

	// Max and Min are the representable extremes.
	Max F = 1<<31 - 1
	Min F = -1 << 31
)

// FromInt returns i as a fixed-point value.
func FromInt(i int) F { return F(i) << FracBits }

// FromRatio returns num/den as a fixed-point value. It is the intended way to
// write fractional constants, since a literal like 0.15 cannot appear in code
// that must avoid floating point. FromRatio(15, 100) is 0.15.
//
// It panics on a zero denominator, which is always a programming error.
func FromRatio(num, den int) F {
	if den == 0 {
		panic("fixed: division by zero in FromRatio")
	}
	return F((int64(num) << FracBits) / int64(den))
}

// Mul returns a*b.
//
// The intermediate is computed in 64 bits, so it cannot overflow for any pair
// of representable operands. The result is truncated toward negative infinity
// (arithmetic shift), which is uniform across all inputs and therefore
// deterministic; Div truncates toward zero instead, so the two are not exact
// inverses. That asymmetry is harmless as long as it is consistent, and it is.
func (a F) Mul(b F) F { return F((int64(a) * int64(b)) >> FracBits) }

// Div returns a/b, truncated toward zero. It panics on a zero divisor.
func (a F) Div(b F) F {
	if b == 0 {
		panic("fixed: division by zero")
	}
	return F((int64(a) << FracBits) / int64(b))
}

// MulRatio returns a*num/den, keeping full precision in the intermediate. It
// is more accurate than a.Mul(FromRatio(num, den)) because the ratio is never
// rounded to Q24.8 first, so prefer it when scaling by a known fraction.
func (a F) MulRatio(num, den int) F {
	if den == 0 {
		panic("fixed: division by zero in MulRatio")
	}
	return F((int64(a) * int64(num)) / int64(den))
}

// Int truncates toward negative infinity and returns the whole part.
func (a F) Int() int { return int(a >> FracBits) }

// Floor returns the largest whole value <= a.
func (a F) Floor() F { return a &^ (One - 1) }

// Ceil returns the smallest whole value >= a.
func (a F) Ceil() F { return (a + One - 1).Floor() }

// Round returns a rounded to the nearest whole value, halves toward positive
// infinity.
func (a F) Round() F { return (a + Half).Floor() }

// Frac returns the fractional part of a, always in [0, 1).
func (a F) Frac() F { return a - a.Floor() }

// Abs returns the absolute value of a.
//
// Abs(Min) is not representable and returns Min unchanged, matching the
// behaviour of two's-complement integer negation. Sim code must not produce
// values anywhere near Min, so this is a theoretical edge rather than a
// practical one.
func (a F) Abs() F {
	if a < 0 {
		return -a
	}
	return a
}

// Sign returns -1, 0, or +1.
func (a F) Sign() int {
	switch {
	case a < 0:
		return -1
	case a > 0:
		return 1
	default:
		return 0
	}
}

// F is an ordered type, so the builtin min and max work on it directly; this
// package deliberately does not wrap them.

// Clamp constrains a to [lo, hi].
func Clamp(a, lo, hi F) F {
	if a < lo {
		return lo
	}
	if a > hi {
		return hi
	}
	return a
}

// ApproachZero moves a toward zero by at most step, without overshooting. It
// is the standard shape for friction and deceleration.
func (a F) ApproachZero(step F) F {
	if a > 0 {
		if a < step {
			return 0
		}
		return a - step
	}
	if a > -step {
		return 0
	}
	return a + step
}

// Approach moves a toward target by at most step, without overshooting.
func (a F) Approach(target, step F) F {
	if a < target {
		if target-a < step {
			return target
		}
		return a + step
	}
	if a-target < step {
		return target
	}
	return a - step
}

// String formats a with three decimal places, for logs and test failures.
//
// It is the only place in this package that produces a decimal representation.
// Do not use it, or any float conversion, inside the simulation.
func (a F) String() string {
	neg := a < 0
	v := a
	if neg {
		v = -a
	}
	whole := int64(v >> FracBits)
	// 1/256 needs three decimals to round-trip visually; scale and round.
	milli := (int64(v&(One-1))*1000 + int64(Half)) >> FracBits
	if milli >= 1000 {
		whole++
		milli -= 1000
	}
	s := strconv.FormatInt(whole, 10) + "." + pad3(milli)
	if neg {
		return "-" + s
	}
	return s
}

func pad3(v int64) string {
	switch {
	case v >= 100:
		return strconv.FormatInt(v, 10)
	case v >= 10:
		return "0" + strconv.FormatInt(v, 10)
	default:
		return "00" + strconv.FormatInt(v, 10)
	}
}
