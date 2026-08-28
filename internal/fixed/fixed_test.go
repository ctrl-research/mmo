package fixed

import "testing"

func TestFromIntRoundTrip(t *testing.T) {
	for _, i := range []int{0, 1, -1, 7, -7, 1000, -1000, 32768, -32768} {
		if got := FromInt(i).Int(); got != i {
			t.Errorf("FromInt(%d).Int() = %d, want %d", i, got, i)
		}
	}
}

func TestFromRatio(t *testing.T) {
	tests := []struct {
		num, den int
		want     F
	}{
		{1, 2, Half},
		{1, 1, One},
		{3, 4, One * 3 / 4},
		{-1, 2, -Half},
		{1, 256, Epsilon},
		{0, 5, Zero},
	}
	for _, tt := range tests {
		if got := FromRatio(tt.num, tt.den); got != tt.want {
			t.Errorf("FromRatio(%d, %d) = %v, want %v", tt.num, tt.den, got, tt.want)
		}
	}
}

func TestFromRatioZeroDenominatorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("FromRatio with zero denominator did not panic")
		}
	}()
	FromRatio(1, 0)
}

func TestMul(t *testing.T) {
	tests := []struct {
		a, b, want F
	}{
		{One, One, One},
		{FromInt(3), FromInt(4), FromInt(12)},
		{Half, Half, One / 4},
		{FromInt(-3), FromInt(4), FromInt(-12)},
		{FromInt(-3), FromInt(-4), FromInt(12)},
		{Zero, FromInt(99), Zero},
		{FromInt(100), Half, FromInt(50)},
	}
	for _, tt := range tests {
		if got := tt.a.Mul(tt.b); got != tt.want {
			t.Errorf("%v.Mul(%v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// Mul must not overflow for large operands: the intermediate is 64-bit.
func TestMulLargeOperandsDoNotOverflow(t *testing.T) {
	a := FromInt(100000)
	if got := a.Mul(FromInt(20)); got != FromInt(2000000) {
		t.Errorf("large Mul = %v, want %v", got, FromInt(2000000))
	}
}

func TestMulIsCommutative(t *testing.T) {
	vals := []F{Zero, One, Half, -Half, FromInt(37), FromInt(-37), FromRatio(1, 3), FromRatio(-7, 9)}
	for _, a := range vals {
		for _, b := range vals {
			if a.Mul(b) != b.Mul(a) {
				t.Errorf("Mul not commutative for %v, %v: %v vs %v", a, b, a.Mul(b), b.Mul(a))
			}
		}
	}
}

func TestDiv(t *testing.T) {
	tests := []struct {
		a, b, want F
	}{
		{FromInt(12), FromInt(4), FromInt(3)},
		{One, FromInt(2), Half},
		{FromInt(-12), FromInt(4), FromInt(-3)},
		{Zero, FromInt(5), Zero},
	}
	for _, tt := range tests {
		if got := tt.a.Div(tt.b); got != tt.want {
			t.Errorf("%v.Div(%v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestDivByZeroPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Div by zero did not panic")
		}
	}()
	One.Div(Zero)
}

// MulRatio keeps the ratio unrounded, so it is at least as accurate as
// rounding the ratio to Q24.8 first. One third is the classic case.
func TestMulRatioBeatsRoundedRatio(t *testing.T) {
	a := FromInt(3000)
	exact := FromInt(1000)
	viaRatio := a.MulRatio(1, 3)
	viaMul := a.Mul(FromRatio(1, 3))

	if viaRatio != exact {
		t.Errorf("MulRatio(1,3) of 3000 = %v, want exactly %v", viaRatio, exact)
	}
	if (viaRatio - exact).Abs() > (viaMul - exact).Abs() {
		t.Errorf("MulRatio (%v) less accurate than Mul-with-ratio (%v)", viaRatio, viaMul)
	}
}

func TestFloorCeilRoundFrac(t *testing.T) {
	tests := []struct {
		in                       F
		floor, ceil, round, frac F
	}{
		{FromInt(3), FromInt(3), FromInt(3), FromInt(3), Zero},
		{FromInt(3) + Half, FromInt(3), FromInt(4), FromInt(4), Half},
		{FromInt(3) + One/4, FromInt(3), FromInt(4), FromInt(3), One / 4},
		{-Half, FromInt(-1), Zero, Zero, Half},
		{FromInt(-3) - Half, FromInt(-4), FromInt(-3), FromInt(-3), Half},
	}
	for _, tt := range tests {
		if got := tt.in.Floor(); got != tt.floor {
			t.Errorf("%v.Floor() = %v, want %v", tt.in, got, tt.floor)
		}
		if got := tt.in.Ceil(); got != tt.ceil {
			t.Errorf("%v.Ceil() = %v, want %v", tt.in, got, tt.ceil)
		}
		if got := tt.in.Round(); got != tt.round {
			t.Errorf("%v.Round() = %v, want %v", tt.in, got, tt.round)
		}
		if got := tt.in.Frac(); got != tt.frac {
			t.Errorf("%v.Frac() = %v, want %v", tt.in, got, tt.frac)
		}
	}
}

// Frac must always land in [0, 1), including for negatives, so that
// Floor + Frac reconstructs the original.
func TestFracIsAlwaysNonNegative(t *testing.T) {
	for v := F(-1000); v < 1000; v++ {
		f := v.Frac()
		if f < 0 || f >= One {
			t.Fatalf("%v.Frac() = %v, outside [0, 1)", v, f)
		}
		if v.Floor()+f != v {
			t.Fatalf("Floor+Frac does not reconstruct %v", v)
		}
	}
}

func TestSignAndAbs(t *testing.T) {
	tests := []struct {
		in   F
		sign int
		abs  F
	}{
		{FromInt(5), 1, FromInt(5)},
		{FromInt(-5), -1, FromInt(5)},
		{Zero, 0, Zero},
		{Epsilon, 1, Epsilon},
		{-Epsilon, -1, Epsilon},
	}
	for _, tt := range tests {
		if got := tt.in.Sign(); got != tt.sign {
			t.Errorf("%v.Sign() = %d, want %d", tt.in, got, tt.sign)
		}
		if got := tt.in.Abs(); got != tt.abs {
			t.Errorf("%v.Abs() = %v, want %v", tt.in, got, tt.abs)
		}
	}
}

func TestClamp(t *testing.T) {
	lo, hi := FromInt(-10), FromInt(10)
	tests := []struct{ in, want F }{
		{FromInt(0), FromInt(0)},
		{FromInt(-50), lo},
		{FromInt(50), hi},
		{lo, lo},
		{hi, hi},
	}
	for _, tt := range tests {
		if got := Clamp(tt.in, lo, hi); got != tt.want {
			t.Errorf("Clamp(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestApproachZeroNeverOvershoots(t *testing.T) {
	step := FromInt(3)
	tests := []struct{ in, want F }{
		{FromInt(10), FromInt(7)},
		{FromInt(2), Zero},
		{FromInt(-2), Zero},
		{FromInt(-10), FromInt(-7)},
		{Zero, Zero},
	}
	for _, tt := range tests {
		if got := tt.in.ApproachZero(step); got != tt.want {
			t.Errorf("%v.ApproachZero(%v) = %v, want %v", tt.in, step, got, tt.want)
		}
	}
}

// Repeated deceleration must actually reach zero and stay there, rather than
// oscillating around it. A drifting character that never quite stops is the
// classic symptom of getting this wrong.
func TestApproachZeroConvergesAndStays(t *testing.T) {
	v := FromInt(97)
	step := FromRatio(7, 3)
	for i := 0; i < 1000; i++ {
		v = v.ApproachZero(step)
	}
	if v != Zero {
		t.Fatalf("ApproachZero did not converge to zero, got %v", v)
	}
}

func TestApproachNeverOvershoots(t *testing.T) {
	step := FromInt(3)
	tests := []struct{ in, target, want F }{
		{Zero, FromInt(10), FromInt(3)},
		{Zero, FromInt(2), FromInt(2)},
		{FromInt(10), Zero, FromInt(7)},
		{FromInt(10), FromInt(9), FromInt(9)},
		{FromInt(5), FromInt(5), FromInt(5)},
	}
	for _, tt := range tests {
		if got := tt.in.Approach(tt.target, step); got != tt.want {
			t.Errorf("%v.Approach(%v, %v) = %v, want %v", tt.in, tt.target, step, got, tt.want)
		}
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		in   F
		want string
	}{
		{Zero, "0.000"},
		{One, "1.000"},
		{Half, "0.500"},
		{-Half, "-0.500"},
		{FromInt(-3), "-3.000"},
		{FromInt(12) + One/4, "12.250"},
		{Epsilon, "0.004"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("String() of raw %d = %q, want %q", int32(tt.in), got, tt.want)
		}
	}
}

// The builtin min and max must work on F without wrappers.
func TestBuiltinMinMaxWorkOnF(t *testing.T) {
	a, b := FromInt(3), FromInt(7)
	if min(a, b) != a || max(a, b) != b {
		t.Error("builtin min/max do not behave as expected on F")
	}
}
