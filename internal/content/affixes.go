package content

import (
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// Affixes are the modifiers an item can roll.
//
// Each affix is one stat modifier available at several tiers. A tier bounds
// the value range and the item level at which it becomes available, which is
// what makes a high-level drop meaningfully different from a low-level one
// rather than merely bigger.
//
// Rolled values are stored on the item, never re-derived. Re-rolling from a
// seed at load time would mean a rebalance silently rewrites items already in
// players' stashes -- the fastest way to lose a playerbase.

// AffixType is where an affix sits in an item's name and which of the two
// pools it competes for.
type AffixType string

const (
	Prefix AffixType = "prefix"
	Suffix AffixType = "suffix"
)

// Affix is one modifier family.
type Affix struct {
	ID string

	// Name appears in the generated item name: prefixes before the base type,
	// suffixes after it. "Heavy Iron Sword of Force".
	Name string

	Type AffixType

	// Classes restricts which item classes may roll this. Empty means any.
	Classes []string

	Stat stats.StatID
	Kind stats.Kind

	// Tiers, ordered best first. Tier 1 is the strongest.
	Tiers []AffixTier
}

// AffixTier is one band of an affix.
type AffixTier struct {
	// Tier numbers count down in strength: 1 is best.
	Tier int

	// ItemLevel is the minimum item level at which this tier can appear. It is
	// what makes deep content worth farming.
	ItemLevel int

	// Min and Max bound the rolled value, inclusive.
	Min stats.Value
	Max stats.Value

	// Weight is the relative chance of this tier being chosen among those
	// available. Higher tiers are usually rarer.
	Weight int
}

// AppliesTo reports whether an affix can roll on an item class.
func (a *Affix) AppliesTo(class string) bool {
	if len(a.Classes) == 0 {
		return true
	}
	for _, c := range a.Classes {
		if c == class {
			return true
		}
	}
	return false
}

// TiersFor returns the tiers available at an item level, best first.
func (a *Affix) TiersFor(itemLevel int) []AffixTier {
	var out []AffixTier
	for _, t := range a.Tiers {
		if t.ItemLevel <= itemLevel {
			out = append(out, t)
		}
	}
	return out
}

type affixesFile struct {
	Affix map[string]struct {
		Name    string   `toml:"name"`
		Type    string   `toml:"type"`
		Classes []string `toml:"classes"`
		Stat    string   `toml:"stat"`
		Kind    string   `toml:"kind"`

		Tiers []struct {
			Tier      int     `toml:"tier"`
			ItemLevel int     `toml:"item_level"`
			Min       float64 `toml:"min"`
			Max       float64 `toml:"max"`
			Weight    int     `toml:"weight"`
		} `toml:"tiers"`
	} `toml:"affix"`
}

func (c *Content) loadAffixes(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "affixes", ".toml")
	if err != nil {
		return err
	}

	for _, name := range files {
		data, err := rec.readAndRecord(name)
		if err != nil {
			return err
		}

		var f affixesFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Affix {
			if _, dup := c.Affixes[id]; dup {
				return fmt.Errorf("%s: affix %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: affix %q has no name", name, id)
			}

			affixType := AffixType(raw.Type)
			if affixType != Prefix && affixType != Suffix {
				return fmt.Errorf("%s: affix %q has type %q, want prefix or suffix", name, id, raw.Type)
			}

			stat, ok := stats.Parse(raw.Stat)
			if !ok {
				return fmt.Errorf("%s: affix %q references unknown stat %q; known stats are %v",
					name, id, raw.Stat, stats.Names())
			}
			kind, ok := stats.ParseKind(raw.Kind)
			if !ok {
				return fmt.Errorf("%s: affix %q has modifier kind %q, want flat, increased, or more",
					name, id, raw.Kind)
			}

			a := &Affix{
				ID:      id,
				Name:    raw.Name,
				Type:    affixType,
				Classes: raw.Classes,
				Stat:    stat,
				Kind:    kind,
			}

			if len(raw.Tiers) == 0 {
				return fmt.Errorf("%s: affix %q has no tiers; it could never roll", name, id)
			}

			seenTiers := make(map[int]bool, len(raw.Tiers))
			for i, t := range raw.Tiers {
				if seenTiers[t.Tier] {
					return fmt.Errorf("%s: affix %q defines tier %d twice", name, id, t.Tier)
				}
				seenTiers[t.Tier] = true

				if t.Max < t.Min {
					return fmt.Errorf("%s: affix %q tier %d has max below min (%v < %v)",
						name, id, t.Tier, t.Max, t.Min)
				}
				if t.Weight <= 0 {
					// A zero weight is almost always a forgotten field, and it
					// is invisible in play: the tier simply never appears.
					return fmt.Errorf("%s: affix %q tier %d has weight %d; it could never be chosen",
						name, id, t.Tier, t.Weight)
				}

				a.Tiers = append(a.Tiers, AffixTier{
					Tier:      t.Tier,
					ItemLevel: t.ItemLevel,
					Min:       stats.FromFloat(t.Min),
					Max:       stats.FromFloat(t.Max),
					Weight:    t.Weight,
				})
				_ = i
			}

			// Sorted best first, so tier selection and display do not depend on
			// the order they happened to be written in.
			sortTiersByStrength(a.Tiers)

			// The lowest tier must be reachable at item level 1, or an affix
			// exists that nothing can ever roll.
			lowest := a.Tiers[len(a.Tiers)-1]
			if lowest.ItemLevel > 1 {
				return fmt.Errorf(
					"%s: affix %q has no tier available below item level %d, "+
						"so low-level items can never roll it",
					name, id, lowest.ItemLevel)
			}

			c.Affixes[id] = a
		}
	}
	return nil
}

// sortTiersByStrength orders tiers best first, by tier number ascending.
func sortTiersByStrength(tiers []AffixTier) {
	for i := 1; i < len(tiers); i++ {
		for j := i; j > 0 && tiers[j].Tier < tiers[j-1].Tier; j-- {
			tiers[j], tiers[j-1] = tiers[j-1], tiers[j]
		}
	}
}
