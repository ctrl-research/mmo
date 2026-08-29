package stats

import (
	"strconv"

	"github.com/ctrl-research/mmo/internal/fixed"
)

// Value is a stat quantity in millionths.
//
// The simulation uses Q24.8 fixed-point, which is right there: it is exact
// enough for positions, and it is what makes the client's WebAssembly build
// agree with the server bit for bit. It is the wrong representation for stats.
//
// Q24.8 resolves to 1/256, so "+40% increased" becomes 102/256 = 39.84%. One
// modifier being 0.4% light is barely visible; three of them compounding is a
// tooltip that disagrees with the damage dealt, and a stat system players
// cannot plan against. Percentages are authored as exact decimals and should
// stay exact.
//
// Millionths in an int64 represent every authored percentage precisely, leave
// room for products of several modifiers, and convert to Q24.8 once at the
// boundary where the simulation actually consumes a number.
type Value int64

// Scale is the number of millionths in 1.0.
const Scale Value = 1_000_000

// FromInt returns a whole number as a Value.
func FromInt(v int) Value { return Value(v) * Scale }

// FromPercent returns a percentage. FromPercent(40) is +40%, exactly.
func FromPercent(percent int) Value { return Value(percent) * (Scale / 100) }

// FromMillionths returns a raw millionths value, which is how content stores a
// percentage after the loader converts it once.
func FromMillionths(v int64) Value { return Value(v) }

// FromFloat converts an authored decimal.
//
// The single conversion point from the floats content is written in. Rounding
// here rather than truncating means 0.4 becomes exactly 400000 rather than
// 399999, which would reintroduce the drift this type exists to avoid.
func FromFloat(v float64) Value {
	if v >= 0 {
		return Value(v*float64(Scale) + 0.5)
	}
	return Value(v*float64(Scale) - 0.5)
}

// Mul returns a*b, where both are scaled.
//
// The intermediate is int64: with values bounded by the sanity limits content
// enforces, a product of two stats cannot overflow. Dividing after multiplying
// is what preserves precision -- dividing first would discard it.
func (a Value) Mul(b Value) Value { return a * b / Scale }

// Int truncates to a whole number.
func (a Value) Int() int { return int(a / Scale) }

// Round returns the nearest whole number.
//
// Used wherever a value is shown or consumed as an integer, so a computed
// 219.9995 reads as 220 rather than 219 -- the difference between a tooltip
// that looks right and one that looks broken.
func (a Value) Round() int {
	if a >= 0 {
		return int((a + Scale/2) / Scale)
	}
	return int((a - Scale/2) / Scale)
}

// Percent returns the value as a whole percentage, for display.
//
// Rounded symmetrically. Integer division truncates toward zero, so adding
// half a unit unconditionally rounds negatives the wrong way -- and negative
// percentages are not an edge case here: resistances, keystone drawbacks, and
// "reduced" modifiers are all negative.
func (a Value) Percent() int {
	scaled := a * 100
	if scaled >= 0 {
		return int((scaled + Scale/2) / Scale)
	}
	return int((scaled - Scale/2) / Scale)
}

// Fixed converts to the simulation's Q24.8 representation.
//
// The boundary between the two number systems. Precision is lost here, which
// is correct: the simulation only needs enough to position a body and resolve
// a hit, and it needs its own representation to stay deterministic across the
// WebAssembly build.
func (a Value) Fixed() fixed.F {
	return fixed.F((int64(a)*int64(fixed.One) + int64(Scale)/2) / int64(Scale))
}

// String formats a value with up to three decimal places, for logs and test
// failures.
func (a Value) String() string {
	whole := a / Scale
	frac := a % Scale
	if frac == 0 {
		return strconv.FormatInt(int64(whole), 10)
	}
	if frac < 0 {
		frac = -frac
	}

	// Three decimals is enough to distinguish any authored percentage.
	milli := (frac + 500) / 1000
	s := strconv.FormatInt(int64(whole), 10)
	if whole == 0 && a < 0 {
		s = "-0"
	}
	return s + "." + pad3(int64(milli))
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
