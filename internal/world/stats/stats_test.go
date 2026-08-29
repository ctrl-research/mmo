package stats

import "testing"

// The distinction the whole model exists for. Getting these two cases right is
// the difference between builds being interesting and being a solved list of
// best-in-slot.
func TestIncreasedIsAdditiveAndMoreIsMultiplicative(t *testing.T) {
	forty := FromPercent(40)

	increased := NewBlock()
	increased.SetBase(Attack, FromInt(100))
	for i := 0; i < 3; i++ {
		increased.Add(Modifier{Stat: Attack, Kind: Increased, Value: forty})
	}

	more := NewBlock()
	more.SetBase(Attack, FromInt(100))
	for i := 0; i < 3; i++ {
		more.Add(Modifier{Stat: Attack, Kind: More, Value: forty})
	}

	// Three sources of +40% increased sum to +120%, so 100 becomes 220.
	if got := increased.Int(Attack); got != 220 {
		t.Errorf("three increased modifiers gave %d, want 220 (they must sum)", got)
	}

	// Three sources of 40% more multiply to 2.744x, so 100 becomes 274.
	if got := more.Int(Attack); got != 274 {
		t.Errorf("three more modifiers gave %d, want 274 (they must multiply)", got)
	}

	// And the difference is the point: more must be strictly better here.
	if more.Int(Attack) <= increased.Int(Attack) {
		t.Error("more is not outperforming increased; the distinction has been lost")
	}
}

func TestFullPipelineOrder(t *testing.T) {
	b := NewBlock()
	b.SetBase(Attack, FromInt(100))
	b.Add(Modifier{Stat: Attack, Kind: Flat, Value: FromInt(50)})
	b.Add(Modifier{Stat: Attack, Kind: Increased, Value: FromPercent(50)})
	b.Add(Modifier{Stat: Attack, Kind: More, Value: FromPercent(20)})

	// (100 + 50) x 1.5 x 1.2 = 270.
	//
	// Order matters: applying "more" before "increased" would give the same
	// answer here only because multiplication commutes, but flat must be added
	// before either, and that does not commute.
	if got := b.Int(Attack); got != 270 {
		t.Errorf("pipeline gave %d, want 270", got)
	}
}

func TestFlatAppliesBeforePercentages(t *testing.T) {
	withFlat := NewBlock()
	withFlat.SetBase(Attack, FromInt(100))
	withFlat.Add(Modifier{Stat: Attack, Kind: Flat, Value: FromInt(100)})
	withFlat.Add(Modifier{Stat: Attack, Kind: Increased, Value: Scale})

	// (100 + 100) x 2 = 400. If flat were applied after, it would be 300.
	if got := withFlat.Int(Attack); got != 400 {
		t.Errorf("got %d, want 400; flat must be added before percentages scale it", got)
	}
}

func TestEmptyBlockReturnsBase(t *testing.T) {
	b := NewBlock()
	b.SetBase(Armour, FromInt(42))

	if got := b.Int(Armour); got != 42 {
		t.Errorf("an unmodified stat gave %d, want its base of 42", got)
	}
}

// The identity for a product is one, not zero. A Block that started "more" at
// zero would silently annihilate every stat it touched.
func TestUnmodifiedMoreIsIdentity(t *testing.T) {
	b := NewBlock()
	for id := StatID(0); id < NumStats; id++ {
		b.SetBase(id, FromInt(10))
		if got := b.Int(id); got != 10 {
			t.Fatalf("stat %s gave %d with no modifiers, want 10", id, got)
		}
	}
}

func TestModifiersOnDifferentStatsDoNotInterfere(t *testing.T) {
	b := NewBlock()
	b.SetBase(Attack, FromInt(100))
	b.SetBase(Armour, FromInt(100))

	b.Add(Modifier{Stat: Attack, Kind: More, Value: Scale})

	if got := b.Int(Attack); got != 200 {
		t.Errorf("attack = %d, want 200", got)
	}
	if got := b.Int(Armour); got != 100 {
		t.Errorf("armour = %d, want 100; a modifier leaked across stats", got)
	}
}

// A keystone's drawback can drive a stat negative. It should read as zero
// rather than as a negative that flips the sign of whatever consumes it.
func TestNegativeModifiersAndClamping(t *testing.T) {
	b := NewBlock()
	b.SetBase(MaxLife, FromInt(100))
	b.Add(Modifier{Stat: MaxLife, Kind: Increased, Value: FromPercent(-150)})

	if got := b.Int(MaxLife); got >= 0 {
		t.Fatalf("expected a negative raw value, got %d", got)
	}
	if got := b.IntClampedNonNegative(MaxLife); got != 0 {
		t.Errorf("clamped value = %d, want 0", got)
	}
}

// "30% less" is expressed as a negative "more", so a keystone can trade one
// stat against another.
func TestNegativeMoreReducesMultiplicatively(t *testing.T) {
	b := NewBlock()
	b.SetBase(MaxLife, FromInt(100))
	b.Add(Modifier{Stat: MaxLife, Kind: More, Value: FromPercent(-30)})

	if got := b.Int(MaxLife); got != 70 {
		t.Errorf("30%% less gave %d, want 70", got)
	}
}

func TestAddAll(t *testing.T) {
	b := NewBlock()
	b.SetBase(Attack, FromInt(100))
	b.AddAll([]Modifier{
		{Stat: Attack, Kind: Flat, Value: FromInt(20)},
		{Stat: Attack, Kind: Increased, Value: FromPercent(25)},
	})

	// (100 + 20) x 1.25 = 150.
	if got := b.Int(Attack); got != 150 {
		t.Errorf("got %d, want 150", got)
	}
}

// The order modifiers arrive in must not change the result, or a character's
// stats would depend on the order they equipped things.
func TestModifierOrderDoesNotMatter(t *testing.T) {
	mods := []Modifier{
		{Stat: Attack, Kind: Flat, Value: FromInt(13)},
		{Stat: Attack, Kind: More, Value: FromPercent(15)},
		{Stat: Attack, Kind: Increased, Value: FromPercent(37)},
		{Stat: Attack, Kind: Flat, Value: FromInt(7)},
		{Stat: Attack, Kind: More, Value: FromPercent(22)},
		{Stat: Attack, Kind: Increased, Value: FromPercent(11)},
	}

	forward := NewBlock()
	forward.SetBase(Attack, FromInt(100))
	forward.AddAll(mods)

	reversed := NewBlock()
	reversed.SetBase(Attack, FromInt(100))
	for i := len(mods) - 1; i >= 0; i-- {
		reversed.Add(mods[i])
	}

	if forward.Value(Attack) != reversed.Value(Attack) {
		t.Errorf("order changed the result: %v then %v",
			forward.Value(Attack), reversed.Value(Attack))
	}
}

// Tooltips must be able to show the arithmetic, not only the answer: a player
// choosing between two items needs to see why one is better.
func TestExplainReportsEveryComponent(t *testing.T) {
	b := NewBlock()
	b.SetBase(Attack, FromInt(100))
	b.Add(Modifier{Stat: Attack, Kind: Flat, Value: FromInt(50)})
	b.Add(Modifier{Stat: Attack, Kind: Increased, Value: FromPercent(50)})
	b.Add(Modifier{Stat: Attack, Kind: More, Value: FromPercent(20)})

	e := b.Explain(Attack)

	if e.Base != FromInt(100) {
		t.Errorf("base = %v, want 100", e.Base)
	}
	if e.Flat != FromInt(50) {
		t.Errorf("flat = %v, want 50", e.Flat)
	}
	if e.Increased != FromPercent(50) {
		t.Errorf("increased = %v, want 0.5", e.Increased)
	}
	if e.More != FromPercent(120) {
		t.Errorf("more = %v, want 1.2", e.More)
	}
	if e.Final != b.Value(Attack) {
		t.Errorf("explained final %v does not match computed %v", e.Final, b.Value(Attack))
	}
}

func TestCloneIsIndependent(t *testing.T) {
	original := NewBlock()
	original.SetBase(Attack, FromInt(100))

	preview := original.Clone()
	preview.Add(Modifier{Stat: Attack, Kind: More, Value: Scale})

	if got := original.Int(Attack); got != 100 {
		t.Errorf("the original changed to %d when its clone was modified", got)
	}
	if got := preview.Int(Attack); got != 200 {
		t.Errorf("the clone = %d, want 200", got)
	}
}

func TestStatNamesRoundTrip(t *testing.T) {
	for id := StatID(0); id < NumStats; id++ {
		name := id.String()
		if name == "" {
			t.Fatalf("stat %d has no name", id)
		}

		parsed, ok := Parse(name)
		if !ok {
			t.Errorf("name %q does not parse back", name)
			continue
		}
		if parsed != id {
			t.Errorf("name %q parsed to %d, want %d", name, parsed, id)
		}
	}
}

func TestParseRejectsUnknownNames(t *testing.T) {
	for _, name := range []string{"", "nonsense", "Attack", "max life"} {
		if _, ok := Parse(name); ok {
			t.Errorf("Parse(%q) succeeded, want failure", name)
		}
	}
}

// Every stat needs a name, or content can reference one it cannot resolve and
// the omission is invisible until someone writes an item that uses it.
func TestEveryStatHasAName(t *testing.T) {
	seen := make(map[string]StatID, NumStats)
	for id := StatID(0); id < NumStats; id++ {
		name := names[id]
		if name == "" {
			t.Errorf("stat %d has no content-facing name", id)
			continue
		}
		if other, dup := seen[name]; dup {
			t.Errorf("stats %d and %d share the name %q", other, id, name)
		}
		seen[name] = id
	}
}

func TestKindRoundTrip(t *testing.T) {
	for _, k := range []Kind{Flat, Increased, More} {
		parsed, ok := ParseKind(k.String())
		if !ok || parsed != k {
			t.Errorf("kind %v did not round-trip", k)
		}
	}
	if _, ok := ParseKind("nonsense"); ok {
		t.Error("an unknown modifier kind parsed successfully")
	}
}

// Out-of-range IDs must not panic: content is validated at load, but a bug
// upstream should not take the tick loop down.
func TestOutOfRangeStatsAreIgnored(t *testing.T) {
	b := NewBlock()
	b.SetBase(NumStats, FromInt(10))
	b.Add(Modifier{Stat: NumStats + 5, Kind: Flat, Value: FromInt(10)})

	if got := b.Value(NumStats); got != 0 {
		t.Errorf("an out-of-range stat returned %v, want 0", got)
	}
}

func BenchmarkValue(b *testing.B) {
	block := NewBlock()
	block.SetBase(Attack, FromInt(100))
	for i := 0; i < 20; i++ {
		block.Add(Modifier{Stat: Attack, Kind: Increased, Value: FromPercent(5)})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = block.Int(Attack)
	}
}

// --- value representation ---------------------------------------------------

// The reason this type exists: Q24.8 cannot represent 40% exactly, and three
// modifiers compounding that error produce a tooltip that disagrees with the
// damage dealt.
func TestPercentagesAreExact(t *testing.T) {
	for percent := -100; percent <= 200; percent++ {
		v := FromPercent(percent)
		if got := v.Percent(); got != percent {
			t.Errorf("FromPercent(%d).Percent() = %d", percent, got)
		}
	}

	// The specific case that motivated this: 40% must be exactly 0.4.
	if FromPercent(40) != Scale*40/100 {
		t.Errorf("40%% is %v, not exact", FromPercent(40))
	}
}

func TestFromFloatRounds(t *testing.T) {
	tests := map[float64]Value{
		0.4:   400_000,
		0.15:  150_000,
		1.0:   1_000_000,
		0:     0,
		-0.25: -250_000,
		0.001: 1_000,
	}
	for in, want := range tests {
		if got := FromFloat(in); got != want {
			t.Errorf("FromFloat(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestValueRounding(t *testing.T) {
	tests := map[Value]int{
		FromInt(5):         5,
		Scale*5 + 499_999:  5,
		Scale*5 + 500_000:  6,
		Scale*5 + 999_999:  6,
		-Scale * 5:         -5,
		-Scale*5 - 500_000: -6,
	}
	for in, want := range tests {
		if got := in.Round(); got != want {
			t.Errorf("Value(%d).Round() = %d, want %d", in, got, want)
		}
	}
}

// Conversion to the simulation's representation happens once, at the boundary
// where a number is actually consumed.
func TestFixedConversion(t *testing.T) {
	if got := FromInt(100).Fixed().Int(); got != 100 {
		t.Errorf("100 converted to fixed and back = %d", got)
	}
	// Half should survive, since Q24.8 represents it exactly.
	if got := FromFloat(0.5).Fixed(); got != 128 {
		t.Errorf("0.5 in fixed = %d raw, want 128", got)
	}
}

func TestValueString(t *testing.T) {
	tests := map[Value]string{
		FromInt(5):       "5",
		FromPercent(40):  "0.400",
		FromFloat(1.25):  "1.250",
		FromInt(-3):      "-3",
		FromFloat(-0.75): "-0.750",
	}
	for in, want := range tests {
		if got := in.String(); got != want {
			t.Errorf("Value(%d).String() = %q, want %q", int64(in), got, want)
		}
	}
}

// A long modifier chain must not accumulate visible drift, which is the whole
// point of exact percentages.
func TestManyModifiersDoNotDrift(t *testing.T) {
	b := NewBlock()
	b.SetBase(Attack, FromInt(100))

	// Twenty sources of +5% increased is exactly +100%.
	for i := 0; i < 20; i++ {
		b.Add(Modifier{Stat: Attack, Kind: Increased, Value: FromPercent(5)})
	}

	if got := b.Int(Attack); got != 200 {
		t.Errorf("twenty +5%% modifiers gave %d, want exactly 200", got)
	}
}
