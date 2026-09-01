// Package stats computes derived character statistics.
//
// The model is Path of Exile's, and specifically its distinction between
// additive and multiplicative modifiers, because that distinction is the
// reason PoE builds are interesting rather than a solved list of
// best-in-slot:
//
//	final = (base + Σ flat) × (1 + Σ increased) × Π (1 + more)
//
// Quantities are exact scaled integers rather than the simulation's Q24.8
// fixed-point -- see value.go for why a stat system needs the precision that
// positions do not.
//
// Three sources of "+40% increased damage" give +120%, total. Three sources of
// "40% more damage" give 2.74×. That is what makes "more" multipliers
// build-defining and rare, and "increased" the common currency of gear -- and
// it gives players a real optimisation problem instead of a single best item
// at every slot.
//
// Getting this wrong is expensive: every item and every skill inherits the
// semantics, so a mistake here has to be unwound everywhere at once.
package stats

import "fmt"

// StatID identifies one derived statistic.
//
// A compile-time enum rather than a string key: these are read on the hot path
// when damage is resolved, and an array index costs nothing where a map lookup
// and a string hash would. Content refers to them by name, and the loader
// resolves the name once.
type StatID uint8

const (
	// Attributes.
	Strength StatID = iota
	Dexterity
	Intelligence

	// Offence.
	Attack
	AttackSpeed
	CritChance
	CritMultiplier

	// Defence.
	Armour
	MaxLife
	MaxMana

	// Elemental resistances, each capped by balance.
	FireResistance
	ColdResistance
	LightningResistance

	// Utility.
	MovementSpeed
	LifeLeech

	// NumStats bounds the arrays below. It must stay last.
	NumStats
)

// names maps each stat to the identifier content uses.
//
// Content-facing names are snake_case and stable: renaming one silently
// invalidates every item and passive that referenced it, so treat these as
// permanent.
var names = [NumStats]string{
	Strength:            "strength",
	Dexterity:           "dexterity",
	Intelligence:        "intelligence",
	Attack:              "attack",
	AttackSpeed:         "attack_speed",
	CritChance:          "crit_chance",
	CritMultiplier:      "crit_multiplier",
	Armour:              "armour",
	MaxLife:             "max_life",
	MaxMana:             "max_mana",
	FireResistance:      "fire_resistance",
	ColdResistance:      "cold_resistance",
	LightningResistance: "lightning_resistance",
	MovementSpeed:       "movement_speed",
	LifeLeech:           "life_leech",
}

var byName = func() map[string]StatID {
	m := make(map[string]StatID, NumStats)
	for id := StatID(0); id < NumStats; id++ {
		m[names[id]] = id
	}
	return m
}()

// String returns the content-facing name.
func (s StatID) String() string {
	if s >= NumStats {
		return fmt.Sprintf("stat(%d)", uint8(s))
	}
	return names[s]
}

// Parse resolves a content-facing stat name.
func Parse(name string) (StatID, bool) {
	id, ok := byName[name]
	return id, ok
}

// Names returns every stat name, for error messages that can list the options.
func Names() []string {
	out := make([]string, 0, NumStats)
	for id := StatID(0); id < NumStats; id++ {
		out = append(out, names[id])
	}
	return out
}

// Kind describes how a modifier combines with others of its kind.
type Kind uint8

const (
	// Flat adds to the base value. "+15 to Attack".
	Flat Kind = iota

	// Increased sums with every other increased modifier before being applied
	// once. "+20% increased Attack".
	Increased

	// More multiplies independently of every other more modifier. "20% more
	// Attack". Rare and build-defining, precisely because it does not dilute.
	More
)

func (k Kind) String() string {
	switch k {
	case Flat:
		return "flat"
	case Increased:
		return "increased"
	case More:
		return "more"
	default:
		return "unknown"
	}
}

// ParseKind resolves a content-facing modifier kind.
func ParseKind(s string) (Kind, bool) {
	switch s {
	case "flat":
		return Flat, true
	case "increased":
		return Increased, true
	case "more":
		return More, true
	default:
		return 0, false
	}
}

// Modifier is one contribution to one stat.
type Modifier struct {
	Stat  StatID
	Kind  Kind
	Value Value
}

// Block accumulates modifiers and computes final values.
//
// A Block is rebuilt from scratch whenever equipment, passives, or buffs
// change, rather than being edited incrementally. Removing a modifier from a
// running product is lossy, and an incremental path that drifts from the
// rebuilt one produces stats that depend on the order a player equipped
// things -- which is the kind of bug that gets reported as "my damage is wrong
// after relogging".
type Block struct {
	base      [NumStats]Value
	flat      [NumStats]Value
	increased [NumStats]Value
	more      [NumStats]Value
}

// NewBlock returns a Block with every "more" multiplier at 1.0, which is the
// identity for a product.
func NewBlock() *Block {
	b := &Block{}
	for i := range b.more {
		b.more[i] = Scale
	}
	return b
}

// SetBase sets the intrinsic value of a stat, before any modifier.
func (b *Block) SetBase(stat StatID, value Value) {
	if stat < NumStats {
		b.base[stat] = value
	}
}

// AddBase adds to the intrinsic value, for contributions that are genuinely
// part of the character rather than a modifier -- level scaling, for instance.
func (b *Block) AddBase(stat StatID, value Value) {
	if stat < NumStats {
		b.base[stat] += value
	}
}

// Add applies one modifier.
func (b *Block) Add(m Modifier) {
	if m.Stat >= NumStats {
		return
	}
	switch m.Kind {
	case Flat:
		b.flat[m.Stat] += m.Value
	case Increased:
		b.increased[m.Stat] += m.Value
	case More:
		// Accumulated as a running product because that is exactly what the
		// formula asks for, and because a Block is always rebuilt rather than
		// edited -- so nothing ever needs to divide one back out.
		b.more[m.Stat] = b.more[m.Stat].Mul(Scale + m.Value)
	}
}

// AddAll applies several modifiers.
func (b *Block) AddAll(mods []Modifier) {
	for _, m := range mods {
		b.Add(m)
	}
}

// Value returns the final computed value of a stat.
func (b *Block) Value(stat StatID) Value {
	if stat >= NumStats {
		return 0
	}

	total := b.base[stat] + b.flat[stat]
	total = total.Mul(Scale + b.increased[stat])
	return total.Mul(b.more[stat])
}

// Int returns the final value rounded to a whole number, which is what damage,
// armour, and life are expressed in.
//
// Rounded rather than truncated: a computed 219.9995 should read as 220. A
// tooltip and a damage number that disagree by one, consistently, is exactly
// the kind of thing players notice and report.
func (b *Block) Int(stat StatID) int { return b.Value(stat).Round() }

// IntClampedNonNegative returns the final value as a whole number, never below
// zero.
//
// A stat driven negative by a keystone's drawback should read as zero rather
// than as a negative that then flips the sign of whatever consumes it.
func (b *Block) IntClampedNonNegative(stat StatID) int {
	if v := b.Int(stat); v > 0 {
		return v
	}
	return 0
}

// Breakdown is how a final value was reached.
//
// Returned so a tooltip can show the arithmetic rather than only the result.
// A player deciding between two items needs to see *why* one is better, and a
// stat system whose reasoning is invisible is one players cannot plan around.
type Breakdown struct {
	Base      Value
	Flat      Value
	Increased Value
	More      Value
	Final     Value
}

// Explain returns the components behind a stat's final value.
func (b *Block) Explain(stat StatID) Breakdown {
	if stat >= NumStats {
		return Breakdown{More: Scale}
	}
	return Breakdown{
		Base:      b.base[stat],
		Flat:      b.flat[stat],
		Increased: b.increased[stat],
		More:      b.more[stat],
		Final:     b.Value(stat),
	}
}

// Clone returns an independent copy, for previewing a change without
// disturbing the live block -- comparing an item against what is equipped, for
// instance.
func (b *Block) Clone() *Block {
	c := *b
	return &c
}

// Layers returns the block's four modifier arrays, in the order Rebuild takes
// them: base, flat, increased, more.
//
// Exported for one reason: a stat block computed on the node holding a
// character's equipment has to reach the room simulating them, which may be in
// another process. Everything else about a block is derived from these four
// arrays, so they are what a copy has to carry.
//
// The slices are copies. Handing out the arrays themselves would let a caller
// change a live block by writing to what looks like a read.
func (b *Block) Layers() (base, flat, increased, more []Value) {
	clone := func(a [NumStats]Value) []Value {
		out := make([]Value, NumStats)
		copy(out, a[:])
		return out
	}
	return clone(b.base), clone(b.flat), clone(b.increased), clone(b.more)
}

// Rebuild reconstructs a block from Layers.
//
// A wrong-length slice is refused rather than padded: it means the sender was
// built against a different stat list, and padding would silently give every
// stat past the end a value of zero -- which for the "more" layer is not a
// missing multiplier but a total one, and would zero the character's damage.
func Rebuild(base, flat, increased, more []Value) (*Block, bool) {
	for _, layer := range [][]Value{base, flat, increased, more} {
		if len(layer) != int(NumStats) {
			return nil, false
		}
	}

	b := &Block{}
	copy(b.base[:], base)
	copy(b.flat[:], flat)
	copy(b.increased[:], increased)
	copy(b.more[:], more)
	return b, true
}
