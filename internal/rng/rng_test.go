package rng

import (
	"encoding/json"
	"flag"
	"math"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden sequence fixture")

// The property the entire replay system rests on.
func TestSameSeedProducesSameSequence(t *testing.T) {
	a, b := New(12345), New(12345)
	for i := 0; i < 1000; i++ {
		if x, y := a.Uint32(), b.Uint32(); x != y {
			t.Fatalf("draw %d diverged: %d vs %d", i, x, y)
		}
	}
}

func TestDifferentSeedsProduceDifferentSequences(t *testing.T) {
	a, b := New(1), New(2)
	same := 0
	for i := 0; i < 1000; i++ {
		if a.Uint32() == b.Uint32() {
			same++
		}
	}
	// A handful of coincidental collisions is fine; hundreds means the seed is
	// not actually reaching the state.
	if same > 5 {
		t.Errorf("%d of 1000 draws matched across different seeds", same)
	}
}

// Layers each get their own stream so one player's drops do not depend on how
// many other players happen to share the room.
func TestStreamsAreIndependent(t *testing.T) {
	a, b := NewStream(42, 1), NewStream(42, 2)
	same := 0
	for i := 0; i < 1000; i++ {
		if a.Uint32() == b.Uint32() {
			same++
		}
	}
	if same > 5 {
		t.Errorf("%d of 1000 draws matched across different streams of one seed", same)
	}
}

// The sequence must not change across Go versions or refactors: every recorded
// replay would be invalidated silently. math/rand offers no such guarantee,
// which is why this package exists.
func TestGoldenSequenceIsStable(t *testing.T) {
	const path = "testdata/sequence.json"

	got := make([]uint32, 64)
	s := New(0xDEADBEEF)
	for i := range got {
		got[i] = s.Uint32()
	}

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		blob, _ := json.MarshalIndent(got, "", " ")
		if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Logf("updated %s", path)
		return
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture (run with -update to create it): %v", err)
	}
	var want []uint32
	if err := json.Unmarshal(blob, &want); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("draw %d = %d, want %d\n"+
				"The generator changed. Every recorded replay is now invalid.\n"+
				"Only regenerate this fixture if that was the deliberate intent.",
				i, got[i], want[i])
		}
	}
}

func TestIntNStaysInRange(t *testing.T) {
	s := New(7)
	for _, n := range []int{1, 2, 3, 7, 100, 1000, 65536} {
		for i := 0; i < 2000; i++ {
			v := s.IntN(n)
			if v < 0 || v >= n {
				t.Fatalf("IntN(%d) = %d, outside [0, %d)", n, v, n)
			}
		}
	}
}

func TestIntNPanicsOnNonPositive(t *testing.T) {
	for _, n := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("IntN(%d) did not panic", n)
				}
			}()
			New(1).IntN(n)
		}()
	}
}

// Uint32()%n is biased whenever n does not divide 2^32. For a rare drop that
// bias is invisible in testing and real in aggregate, so the rejection loop
// has to actually work.
func TestIntNIsUnbiased(t *testing.T) {
	const (
		n      = 3 // does not divide 2^32
		draws  = 300000
		expect = draws / n
	)
	counts := make([]int, n)
	s := New(99)
	for i := 0; i < draws; i++ {
		counts[s.IntN(n)]++
	}

	// Chi-squared would be more rigorous, but a 2% band over 300k draws
	// catches modulo bias comfortably while staying stable.
	tolerance := float64(expect) * 0.02
	for i, c := range counts {
		if math.Abs(float64(c-expect)) > tolerance {
			t.Errorf("bucket %d got %d draws, want %d +/- %.0f", i, c, expect, tolerance)
		}
	}
}

// Content authors write "40 to 60" meaning 60 is attainable. An off-by-one
// would silently shave the top roll off every hit and every item in the game.
func TestRangeIsInclusive(t *testing.T) {
	s := New(3)
	sawLo, sawHi := false, false
	for i := 0; i < 5000; i++ {
		v := s.Range(40, 60)
		if v < 40 || v > 60 {
			t.Fatalf("Range(40, 60) = %d, out of bounds", v)
		}
		if v == 40 {
			sawLo = true
		}
		if v == 60 {
			sawHi = true
		}
	}
	if !sawLo {
		t.Error("Range never produced its lower bound")
	}
	if !sawHi {
		t.Error("Range never produced its upper bound; the range is exclusive at the top")
	}
}

func TestRangeHandlesSingleValueAndReversedBounds(t *testing.T) {
	s := New(4)
	if v := s.Range(5, 5); v != 5 {
		t.Errorf("Range(5, 5) = %d, want 5", v)
	}
	for i := 0; i < 100; i++ {
		if v := s.Range(60, 40); v < 40 || v > 60 {
			t.Fatalf("Range with reversed bounds = %d, out of range", v)
		}
	}
}

func TestChanceEdges(t *testing.T) {
	s := New(5)
	for i := 0; i < 100; i++ {
		if s.Chance(0, 100) {
			t.Fatal("Chance(0, n) should never occur")
		}
		if !s.Chance(100, 100) {
			t.Fatal("Chance(n, n) should always occur")
		}
		if s.Chance(-5, 100) {
			t.Fatal("a negative numerator should never occur")
		}
	}
}

func TestChanceApproximatesItsRatio(t *testing.T) {
	const draws = 200000
	s := New(11)
	hits := 0
	for i := 0; i < draws; i++ {
		if s.Chance(15, 100) {
			hits++
		}
	}
	rate := float64(hits) / draws
	if math.Abs(rate-0.15) > 0.005 {
		t.Errorf("Chance(15, 100) fired %.4f of the time, want ~0.15", rate)
	}
}

func TestPPM(t *testing.T) {
	const draws = 400000
	s := New(13)
	hits := 0
	for i := 0; i < draws; i++ {
		if s.PPM(2500) { // 0.25%
			hits++
		}
	}
	rate := float64(hits) / draws
	if math.Abs(rate-0.0025) > 0.001 {
		t.Errorf("PPM(2500) fired %.5f of the time, want ~0.0025", rate)
	}
}

func TestPickRespectsWeights(t *testing.T) {
	const draws = 120000
	weights := []int{1, 3, 8} // 8.3%, 25%, 66.7%
	counts := make([]int, len(weights))

	s := New(17)
	for i := 0; i < draws; i++ {
		counts[s.Pick(weights)]++
	}

	total := 0
	for _, w := range weights {
		total += w
	}
	for i, w := range weights {
		want := float64(w) / float64(total)
		got := float64(counts[i]) / draws
		if math.Abs(got-want) > 0.01 {
			t.Errorf("index %d picked %.4f of the time, want %.4f", i, got, want)
		}
	}
}

// An empty or all-zero weighted table is a content bug. Returning 0 would hide
// it behind a plausible-looking result.
func TestPickReportsAnUnusableTable(t *testing.T) {
	s := New(19)
	for _, w := range [][]int{nil, {}, {0, 0, 0}, {-1, -2}} {
		if got := s.Pick(w); got != -1 {
			t.Errorf("Pick(%v) = %d, want -1", w, got)
		}
	}
}

func TestPickSkipsZeroWeightEntries(t *testing.T) {
	s := New(23)
	weights := []int{0, 5, 0}
	for i := 0; i < 500; i++ {
		if got := s.Pick(weights); got != 1 {
			t.Fatalf("Pick chose index %d, but only index 1 has weight", got)
		}
	}
}

func TestShuffleIsAPermutation(t *testing.T) {
	s := New(29)
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}
	Shuffle(s, items)

	seen := make(map[int]bool, len(items))
	for _, v := range items {
		if seen[v] {
			t.Fatalf("value %d appeared twice after shuffling", v)
		}
		seen[v] = true
	}
	if len(seen) != 50 {
		t.Errorf("shuffle produced %d distinct values, want 50", len(seen))
	}
}

func TestShuffleActuallyReorders(t *testing.T) {
	s := New(31)
	items := make([]int, 100)
	for i := range items {
		items[i] = i
	}
	Shuffle(s, items)

	inPlace := 0
	for i, v := range items {
		if i == v {
			inPlace++
		}
	}
	// Expect about one fixed point on average; 20 means it barely moved.
	if inPlace > 20 {
		t.Errorf("%d of 100 items kept their position", inPlace)
	}
}

// Saving and restoring state lets a replay resume from the middle of a
// session, which matters when the interesting event is twenty minutes in.
func TestStateRoundTripResumesTheSequence(t *testing.T) {
	s := New(37)
	for i := 0; i < 100; i++ {
		s.Uint32()
	}

	state, inc := s.State()
	want := make([]uint32, 20)
	for i := range want {
		want[i] = s.Uint32()
	}

	resumed := New(0)
	resumed.Restore(state, inc)
	for i := range want {
		if got := resumed.Uint32(); got != want[i] {
			t.Fatalf("draw %d after restore = %d, want %d", i, got, want[i])
		}
	}
}

func TestCloneDoesNotDisturbTheOriginal(t *testing.T) {
	s := New(41)
	for i := 0; i < 10; i++ {
		s.Uint32()
	}

	c := s.Clone()
	for i := 0; i < 50; i++ {
		c.Uint32()
	}

	// The original must be exactly where it was left.
	fresh := New(41)
	for i := 0; i < 10; i++ {
		fresh.Uint32()
	}
	for i := 0; i < 20; i++ {
		if got, want := s.Uint32(), fresh.Uint32(); got != want {
			t.Fatalf("draw %d after cloning = %d, want %d", i, got, want)
		}
	}
}

func TestUint64UsesFullWidth(t *testing.T) {
	s := New(43)
	var anyHigh bool
	for i := 0; i < 100; i++ {
		if s.Uint64()>>32 != 0 {
			anyHigh = true
			break
		}
	}
	if !anyHigh {
		t.Error("Uint64 never set any high bits")
	}
}

func BenchmarkUint32(b *testing.B) {
	s := New(1)
	for i := 0; i < b.N; i++ {
		_ = s.Uint32()
	}
}

func BenchmarkIntN(b *testing.B) {
	s := New(1)
	for i := 0; i < b.N; i++ {
		_ = s.IntN(100)
	}
}
