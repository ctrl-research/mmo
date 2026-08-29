package items

import (
	"testing"

	gamedata "github.com/ctrl-research/mmo/content"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/rng"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

func testGenerator(t *testing.T) *Generator {
	t.Helper()
	c, err := content.Load(gamedata.FS)
	if err != nil {
		t.Fatalf("load content: %v", err)
	}
	return NewGenerator(c)
}

// alwaysRare forces the rarity roll, so affix behaviour can be tested without
// generating thousands of items to find one.
var alwaysRare = RarityWeights{Rare: 1}

func TestRollProducesAValidInstance(t *testing.T) {
	g := testGenerator(t)
	source := rng.New(1)

	inst, err := g.Roll(source, "weapon.iron_sword", 20, alwaysRare)
	if err != nil {
		t.Fatalf("roll: %v", err)
	}

	if inst.BaseID != "weapon.iron_sword" {
		t.Errorf("base = %q", inst.BaseID)
	}
	if inst.ItemLevel != 20 {
		t.Errorf("item level = %d, want 20", inst.ItemLevel)
	}
	if len(inst.Implicits) == 0 {
		t.Error("the base type has an implicit, but none was rolled")
	}
}

func TestUnknownBaseIsAnError(t *testing.T) {
	g := testGenerator(t)
	if _, err := g.Roll(rng.New(1), "no.such.item", 1, alwaysRare); err == nil {
		t.Error("an unknown base rolled successfully")
	}
}

// Only equipment rolls modifiers: a stack of potions with affixes would have
// to share one set of rolls across the whole stack.
func TestNonEquipmentRollsNoModifiers(t *testing.T) {
	g := testGenerator(t)
	inst, err := g.Roll(rng.New(1), "potion.red_small", 50, alwaysRare)
	if err != nil {
		t.Fatalf("roll: %v", err)
	}

	if len(inst.Affixes) != 0 || len(inst.Implicits) != 0 {
		t.Errorf("a consumable rolled %d implicits and %d affixes",
			len(inst.Implicits), len(inst.Affixes))
	}
	if inst.Rarity != Normal {
		t.Errorf("a consumable rolled rarity %q", inst.Rarity)
	}
}

func TestRarityDeterminesAffixCount(t *testing.T) {
	g := testGenerator(t)

	for rarity, weights := range map[Rarity]RarityWeights{
		Normal: {Normal: 1},
		Magic:  {Magic: 1},
		Rare:   {Rare: 1},
	} {
		limits := affixLimits[rarity]

		for seed := uint64(0); seed < 40; seed++ {
			inst, err := g.Roll(rng.New(seed), "weapon.iron_sword", 40, weights)
			if err != nil {
				t.Fatalf("roll: %v", err)
			}
			if inst.Rarity != rarity {
				t.Fatalf("seed %d rolled %q, want %q", seed, inst.Rarity, rarity)
			}
			if n := len(inst.Affixes); n > limits.maxTotal {
				t.Errorf("%s rolled %d affixes, above the cap of %d", rarity, n, limits.maxTotal)
			}
		}
	}
}

// The prefix and suffix pools are separate and each capped, so a rare cannot be
// all prefixes. That constraint is what makes rares a question of combination
// rather than of count.
func TestPrefixAndSuffixCapsAreRespected(t *testing.T) {
	g := testGenerator(t)
	c := g.content

	for seed := uint64(0); seed < 200; seed++ {
		inst, err := g.Roll(rng.New(seed), "weapon.iron_sword", 40, alwaysRare)
		if err != nil {
			t.Fatalf("roll: %v", err)
		}

		prefixes, suffixes := 0, 0
		for _, m := range inst.Affixes {
			a, ok := c.Affixes[m.AffixID]
			if !ok {
				t.Fatalf("rolled an affix %q that is not in content", m.AffixID)
			}
			if a.Type == content.Prefix {
				prefixes++
			} else {
				suffixes++
			}
		}

		limits := affixLimits[Rare]
		if prefixes > limits.maxPrefix {
			t.Fatalf("seed %d rolled %d prefixes, above the cap of %d", seed, prefixes, limits.maxPrefix)
		}
		if suffixes > limits.maxSuffix {
			t.Fatalf("seed %d rolled %d suffixes, above the cap of %d", seed, suffixes, limits.maxSuffix)
		}
	}
}

// Two "+12 Attack" modifiers on one sword read as a bug, whatever the
// arithmetic says.
func TestAnItemNeverRollsTheSameAffixTwice(t *testing.T) {
	g := testGenerator(t)

	for seed := uint64(0); seed < 300; seed++ {
		inst, err := g.Roll(rng.New(seed), "armour.iron_plate", 40, alwaysRare)
		if err != nil {
			t.Fatalf("roll: %v", err)
		}

		seen := make(map[string]bool, len(inst.Affixes))
		for _, m := range inst.Affixes {
			if seen[m.AffixID] {
				t.Fatalf("seed %d rolled affix %q twice", seed, m.AffixID)
			}
			seen[m.AffixID] = true
		}
	}
}

// Item level gates tiers, which is what makes deep content worth farming.
func TestItemLevelGatesTiers(t *testing.T) {
	g := testGenerator(t)

	// The top tier of weapon_flat_attack needs item level 25.
	for seed := uint64(0); seed < 300; seed++ {
		inst, err := g.Roll(rng.New(seed), "weapon.rusty_sword", 5, alwaysRare)
		if err != nil {
			t.Fatalf("roll: %v", err)
		}
		for _, m := range inst.Affixes {
			if m.AffixID != "weapon_flat_attack" {
				continue
			}
			if m.Tier == 1 {
				t.Fatalf("seed %d rolled a tier 1 affix on an item level 5 item", seed)
			}
		}
	}

	// And at a high item level, the top tier does appear.
	sawTopTier := false
	for seed := uint64(0); seed < 600 && !sawTopTier; seed++ {
		inst, _ := g.Roll(rng.New(seed), "weapon.iron_sword", 60, alwaysRare)
		for _, m := range inst.Affixes {
			if m.AffixID == "weapon_flat_attack" && m.Tier == 1 {
				sawTopTier = true
			}
		}
	}
	if !sawTopTier {
		t.Error("the top tier never appeared at item level 60")
	}
}

// Rolled values must land inside the tier's authored range, or the content
// file is not describing what the game actually produces.
func TestRolledValuesStayInRange(t *testing.T) {
	g := testGenerator(t)
	c := g.content

	for seed := uint64(0); seed < 300; seed++ {
		inst, err := g.Roll(rng.New(seed), "weapon.iron_sword", 60, alwaysRare)
		if err != nil {
			t.Fatalf("roll: %v", err)
		}

		for _, m := range inst.Affixes {
			affix := c.Affixes[m.AffixID]
			var tier *content.AffixTier
			for i := range affix.Tiers {
				if affix.Tiers[i].Tier == m.Tier {
					tier = &affix.Tiers[i]
				}
			}
			if tier == nil {
				t.Fatalf("rolled tier %d of %q, which is not defined", m.Tier, m.AffixID)
			}
			if m.Value < tier.Min || m.Value > tier.Max {
				t.Fatalf("%s tier %d rolled %v, outside its range %v..%v",
					m.AffixID, m.Tier, m.Value, tier.Min, tier.Max)
			}
		}
	}
}

// Affixes must respect their item classes, or a ring rolls movement speed and
// the class system means nothing.
func TestAffixesRespectItemClass(t *testing.T) {
	g := testGenerator(t)
	c := g.content

	for _, baseID := range []string{"jewellery.copper_ring", "weapon.iron_sword", "armour.leather_boots"} {
		base := c.Items[baseID]
		for seed := uint64(0); seed < 150; seed++ {
			inst, err := g.Roll(rng.New(seed), baseID, 40, alwaysRare)
			if err != nil {
				t.Fatalf("roll: %v", err)
			}
			for _, m := range inst.Affixes {
				affix := c.Affixes[m.AffixID]
				if !affix.AppliesTo(base.Class) {
					t.Fatalf("%s (class %q) rolled %q, which does not apply to it",
						baseID, base.Class, m.AffixID)
				}
			}
		}
	}
}

// The property every drop rests on: a replay must produce the same item.
func TestRollIsDeterministic(t *testing.T) {
	g := testGenerator(t)

	for seed := uint64(0); seed < 20; seed++ {
		first, err := g.Roll(rng.New(seed), "weapon.iron_sword", 40, DefaultRarityWeights)
		if err != nil {
			t.Fatalf("roll: %v", err)
		}
		second, err := g.Roll(rng.New(seed), "weapon.iron_sword", 40, DefaultRarityWeights)
		if err != nil {
			t.Fatalf("roll: %v", err)
		}

		if first.Rarity != second.Rarity || len(first.Affixes) != len(second.Affixes) {
			t.Fatalf("seed %d produced different items", seed)
		}
		for i := range first.Affixes {
			if first.Affixes[i] != second.Affixes[i] {
				t.Fatalf("seed %d affix %d differs: %+v vs %+v",
					seed, i, first.Affixes[i], second.Affixes[i])
			}
		}
	}
}

func TestModifiersIncludeImplicitsAndAffixes(t *testing.T) {
	g := testGenerator(t)

	inst, err := g.Roll(rng.New(7), "weapon.iron_sword", 40, alwaysRare)
	if err != nil {
		t.Fatalf("roll: %v", err)
	}

	mods := inst.Modifiers()
	if len(mods) != len(inst.Implicits)+len(inst.Affixes) {
		t.Errorf("Modifiers() returned %d, want %d implicits plus %d affixes",
			len(mods), len(inst.Implicits), len(inst.Affixes))
	}
}

// The name says what the item does before the tooltip is opened.
func TestDisplayName(t *testing.T) {
	g := testGenerator(t)

	plain, _ := g.Roll(rng.New(1), "weapon.iron_sword", 10, RarityWeights{Normal: 1})
	if got := g.DisplayName(plain); got != "Iron Sword" {
		t.Errorf("a normal item is named %q, want the plain base name", got)
	}

	// A rare should pick up at most one prefix and one suffix, so the name
	// stays readable rather than listing every modifier.
	for seed := uint64(0); seed < 50; seed++ {
		inst, _ := g.Roll(rng.New(seed), "weapon.iron_sword", 40, alwaysRare)
		name := g.DisplayName(inst)
		if len(inst.Affixes) > 0 && name == "Iron Sword" {
			continue // rolled only affixes whose names are already used
		}
		// "Tempered Iron Sword of the Bull" is six words and exactly the
		// intended style. The bound catches genuine runaway -- every affix
		// concatenated -- rather than a long suffix.
		if got := countWords(name); got > 9 {
			t.Errorf("seed %d produced an unwieldy name %q", seed, name)
		}
	}
}

func countWords(s string) int {
	n, inWord := 0, false
	for _, r := range s {
		if r == ' ' {
			inWord = false
			continue
		}
		if !inWord {
			n++
			inWord = true
		}
	}
	return n
}

// Values are rolled in stat millionths, so what is stored is exactly what is
// shown -- a tooltip and a damage number disagreeing by a rounding step is
// precisely what players notice.
func TestRollValueBoundsAreInclusive(t *testing.T) {
	source := rng.New(3)

	// A range of three millionths, not three whole units: the value space is
	// in millionths, so a range of two whole units spans two million values
	// and hitting an exact endpoint in any practical number of draws is
	// vanishingly unlikely. The narrow range tests the boundary arithmetic,
	// which is what could actually be off by one.
	sawMin, sawMax := false, false
	min, max := stats.Value(5), stats.Value(7)
	for i := 0; i < 2000; i++ {
		v := rollValue(source, min, max)
		if v < min || v > max {
			t.Fatalf("rolled %v, outside %v..%v", v, min, max)
		}
		if v == min {
			sawMin = true
		}
		if v == max {
			sawMax = true
		}
	}
	if !sawMin || !sawMax {
		t.Error("the range is not inclusive at both ends")
	}
}

func TestRollValueHandlesAZeroWidthRange(t *testing.T) {
	source := rng.New(3)
	v := stats.FromInt(5)
	if got := rollValue(source, v, v); got != v {
		t.Errorf("a zero-width range rolled %v, want %v", got, v)
	}
}

// A rare that appears constantly is not rare, and the moment of finding one
// stops meaning anything.
func TestDefaultRarityDistribution(t *testing.T) {
	g := testGenerator(t)
	source := rng.New(11)

	counts := map[Rarity]int{}
	const draws = 4000
	for i := 0; i < draws; i++ {
		inst, err := g.Roll(source, "weapon.iron_sword", 20, DefaultRarityWeights)
		if err != nil {
			t.Fatalf("roll: %v", err)
		}
		counts[inst.Rarity]++
	}

	rareRate := float64(counts[Rare]) / draws
	if rareRate > 0.10 {
		t.Errorf("rares are %.1f%% of drops, which is too common to feel rare", rareRate*100)
	}
	if counts[Normal] < counts[Magic] || counts[Magic] < counts[Rare] {
		t.Errorf("rarity is not ordered by frequency: %v", counts)
	}
}
