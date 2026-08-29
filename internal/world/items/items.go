// Package items generates and describes item instances.
//
// The central rule: rolled values are stored, never re-derived. Deriving stats
// from a seed at load time would mean a content rebalance silently rewrites
// items already sitting in players' stashes -- which is both a betrayal of the
// time they spent getting them and the fastest way to lose a playerbase.
//
// So an instance carries its own rolled numbers, and content files describe
// only what *can* be rolled in future.
package items

import (
	"fmt"
	"strings"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/rng"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// Rarity decides how many affixes an item carries.
type Rarity string

const (
	Normal Rarity = "normal"
	Magic  Rarity = "magic"
	Rare   Rarity = "rare"
)

// Affix limits per rarity, following Path of Exile's structure.
//
// The prefix and suffix pools are separate and each capped, so a rare cannot
// be six prefixes. That constraint is what makes rares interesting: the
// question is not "how many good modifiers" but "which combination".
var affixLimits = map[Rarity]struct{ minTotal, maxTotal, maxPrefix, maxSuffix int }{
	Normal: {0, 0, 0, 0},
	Magic:  {1, 2, 1, 1},
	Rare:   {3, 4, 3, 3},
}

// RolledMod is one modifier with its value already decided.
type RolledMod struct {
	// AffixID is empty for an implicit, which belongs to the base type rather
	// than to a rolled affix.
	AffixID string `json:"affix,omitempty"`

	// Tier is 0 for implicits.
	Tier int `json:"tier,omitempty"`

	Stat stats.StatID `json:"stat"`
	Kind stats.Kind   `json:"kind"`

	// Value is the rolled amount, in stat millionths.
	Value stats.Value `json:"value"`
}

// Instance is one specific item, with its rolls fixed.
type Instance struct {
	BaseID string `json:"base"`
	Rarity Rarity `json:"rarity"`

	// ItemLevel decided which affix tiers were available when it was rolled.
	// Kept so a tooltip can explain why an item could not have rolled better.
	ItemLevel int `json:"ilvl"`

	Implicits []RolledMod `json:"implicits,omitempty"`
	Affixes   []RolledMod `json:"affixes,omitempty"`

	// Stack is the quantity for stackable items, and 1 otherwise.
	Stack int `json:"stack,omitempty"`
}

// Modifiers returns every modifier the item contributes.
func (i *Instance) Modifiers() []stats.Modifier {
	out := make([]stats.Modifier, 0, len(i.Implicits)+len(i.Affixes))
	for _, m := range append(append([]RolledMod{}, i.Implicits...), i.Affixes...) {
		out = append(out, stats.Modifier{Stat: m.Stat, Kind: m.Kind, Value: m.Value})
	}
	return out
}

// Generator rolls items from content.
type Generator struct {
	content *content.Content
}

// NewGenerator returns a generator over loaded content.
func NewGenerator(c *content.Content) *Generator { return &Generator{content: c} }

// RarityWeights decides how often each rarity appears.
type RarityWeights struct {
	Normal int
	Magic  int
	Rare   int
}

// DefaultRarityWeights is the fallback distribution.
//
// Most drops are plain, because a rare that appears constantly is not rare and
// the moment of finding one stops meaning anything.
var DefaultRarityWeights = RarityWeights{Normal: 70, Magic: 25, Rare: 5}

// Roll generates one instance of a base type.
//
// Every random decision comes from the caller's generator, which is the room's
// seeded stream -- so a drop is reproducible from a replay, and "was that
// legitimate?" is a question with an answer rather than a judgement call.
func (g *Generator) Roll(source *rng.Source, baseID string, itemLevel int, weights RarityWeights) (*Instance, error) {
	base, ok := g.content.Items[baseID]
	if !ok {
		return nil, fmt.Errorf("items: unknown base %q", baseID)
	}

	inst := &Instance{BaseID: baseID, ItemLevel: itemLevel, Stack: 1, Rarity: Normal}

	// Only equipment rolls modifiers. A stack of potions with affixes would
	// have to share one set of rolls across the stack, which is incoherent.
	if !base.IsEquipment() {
		return inst, nil
	}

	for _, im := range base.Implicits {
		inst.Implicits = append(inst.Implicits, RolledMod{
			Stat:  im.Stat,
			Kind:  im.Kind,
			Value: rollValue(source, im.Min, im.Max),
		})
	}

	inst.Rarity = rollRarity(source, weights)
	g.rollAffixes(source, inst, base, itemLevel)

	return inst, nil
}

// RollBase generates a plain instance with no affixes, for vendor stock and
// quest rewards where a random rare would be surprising.
func (g *Generator) RollBase(source *rng.Source, baseID string, itemLevel int) (*Instance, error) {
	return g.Roll(source, baseID, itemLevel, RarityWeights{Normal: 1})
}

func rollRarity(source *rng.Source, w RarityWeights) Rarity {
	weights := []int{w.Normal, w.Magic, w.Rare}
	switch source.Pick(weights) {
	case 1:
		return Magic
	case 2:
		return Rare
	default:
		return Normal
	}
}

// rollAffixes fills an item's affix slots.
func (g *Generator) rollAffixes(source *rng.Source, inst *Instance, base *content.Item, itemLevel int) {
	limits, ok := affixLimits[inst.Rarity]
	if !ok || limits.maxTotal == 0 {
		return
	}

	count := source.Range(limits.minTotal, limits.maxTotal)

	// Candidates are ordered deterministically, because the pool comes from a
	// map and Go randomises map iteration -- without sorting, the same seed
	// would produce different items on different runs and replay would break.
	prefixes := g.candidates(base.Class, content.Prefix, itemLevel)
	suffixes := g.candidates(base.Class, content.Suffix, itemLevel)

	used := make(map[string]bool, count)
	prefixCount, suffixCount := 0, 0

	for len(inst.Affixes) < count {
		// Which pool to draw from is itself a roll, bounded by the per-pool
		// caps, so a rare is a mix rather than whichever pool is listed first.
		canPrefix := prefixCount < limits.maxPrefix && hasUnused(prefixes, used)
		canSuffix := suffixCount < limits.maxSuffix && hasUnused(suffixes, used)

		var pool []*content.Affix
		switch {
		case canPrefix && canSuffix:
			if source.IntN(2) == 0 {
				pool = prefixes
			} else {
				pool = suffixes
			}
		case canPrefix:
			pool = prefixes
		case canSuffix:
			pool = suffixes
		default:
			// The pool is exhausted. An item with fewer affixes than its
			// rarity allows is correct here -- the alternative is repeating a
			// modifier, which reads as a bug.
			return
		}

		affix := pickAffix(source, pool, used)
		if affix == nil {
			return
		}

		tier := pickTier(source, affix, itemLevel)
		if tier == nil {
			used[affix.ID] = true
			continue
		}

		inst.Affixes = append(inst.Affixes, RolledMod{
			AffixID: affix.ID,
			Tier:    tier.Tier,
			Stat:    affix.Stat,
			Kind:    affix.Kind,
			Value:   rollValue(source, tier.Min, tier.Max),
		})

		used[affix.ID] = true
		if affix.Type == content.Prefix {
			prefixCount++
		} else {
			suffixCount++
		}
	}
}

// candidates returns the affixes eligible for an item class, in a stable order.
func (g *Generator) candidates(class string, kind content.AffixType, itemLevel int) []*content.Affix {
	var out []*content.Affix
	for _, a := range g.content.Affixes {
		if a.Type != kind || !a.AppliesTo(class) {
			continue
		}
		if len(a.TiersFor(itemLevel)) == 0 {
			continue
		}
		out = append(out, a)
	}

	// Sorted by ID, so the candidate order does not depend on map iteration.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func hasUnused(pool []*content.Affix, used map[string]bool) bool {
	for _, a := range pool {
		if !used[a.ID] {
			return true
		}
	}
	return false
}

// pickAffix chooses uniformly among the unused affixes in a pool.
//
// An item never rolls the same affix twice: two separate "+12 Attack"
// modifiers on one sword read as a bug, whatever the arithmetic says.
func pickAffix(source *rng.Source, pool []*content.Affix, used map[string]bool) *content.Affix {
	var available []*content.Affix
	for _, a := range pool {
		if !used[a.ID] {
			available = append(available, a)
		}
	}
	if len(available) == 0 {
		return nil
	}
	return available[source.IntN(len(available))]
}

// pickTier chooses a tier by weight among those available at the item level.
func pickTier(source *rng.Source, affix *content.Affix, itemLevel int) *content.AffixTier {
	tiers := affix.TiersFor(itemLevel)
	if len(tiers) == 0 {
		return nil
	}

	weights := make([]int, len(tiers))
	for i, t := range tiers {
		weights[i] = t.Weight
	}

	i := source.Pick(weights)
	if i < 0 {
		return nil
	}
	return &tiers[i]
}

// rollValue picks a value in an inclusive range.
//
// Rolled in stat millionths rather than as a float, so the value stored is
// exactly the value shown -- a tooltip and a damage number that disagree by a
// rounding step is precisely the kind of thing players notice.
func rollValue(source *rng.Source, min, max stats.Value) stats.Value {
	if max <= min {
		return min
	}
	return min + stats.Value(source.Range(0, int(max-min)))
}

// DisplayName builds the name shown to a player.
//
// Prefix, base type, suffix -- "Heavy Iron Sword of Force" -- so the name says
// what the item does before the tooltip is opened.
func (g *Generator) DisplayName(inst *Instance) string {
	base, ok := g.content.Items[inst.BaseID]
	if !ok {
		return inst.BaseID
	}
	if inst.Rarity == Normal || len(inst.Affixes) == 0 {
		return base.Name
	}

	var prefix, suffix string
	for _, m := range inst.Affixes {
		a, ok := g.content.Affixes[m.AffixID]
		if !ok {
			continue
		}
		// A rare has several of each, and only the first is used for the name.
		// Listing them all would produce something unreadable.
		if a.Type == content.Prefix && prefix == "" {
			prefix = a.Name
		}
		if a.Type == content.Suffix && suffix == "" {
			suffix = a.Name
		}
	}

	parts := make([]string, 0, 3)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	parts = append(parts, base.Name)
	if suffix != "" {
		parts = append(parts, suffix)
	}
	return strings.Join(parts, " ")
}

// Base returns the base type of an instance.
func (g *Generator) Base(inst *Instance) (*content.Item, bool) {
	b, ok := g.content.Items[inst.BaseID]
	return b, ok
}
