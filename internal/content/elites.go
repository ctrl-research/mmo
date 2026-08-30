package content

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/BurntSushi/toml"

	"github.com/ctrl-research/mmo/internal/fixed"
)

// Elite modifiers.
//
// A champion or rare mob is an ordinary mob that rolled one or more of these.
// Each is a small change -- harder hits, thicker skin, something unpleasant
// when it dies -- and the interesting fights come from the combinations rather
// than from any single entry. Two slimes that rolled Brutal and Volatile are a
// different fight from two that did not, at no content-authoring cost, which
// is the whole reason this is a pool of modifiers and not a list of mobs.

// Elite is one modifier a champion or rare mob can roll.
type Elite struct {
	ID   string
	Name string

	// Weight is its share of the roll. Rarer modifiers are the more dramatic
	// ones, so meeting something genuinely nasty stays an event.
	Weight int

	// Attack, Armour, Life and MoveSpeed are increases, as fixed-point
	// multipliers applied on top of the mob's own numbers. Zero leaves the
	// stat alone.
	Attack    fixed.F
	Armour    fixed.F
	Life      fixed.F
	MoveSpeed fixed.F

	// OnDeath fires where the mob died. The ordinary effect vocabulary, so a
	// modifier that leaves a burning patch behind is content rather than code.
	OnDeath []Effect
}

type elitesFile struct {
	Elite map[string]struct {
		Name      string  `toml:"name"`
		Weight    int     `toml:"weight"`
		Attack    float64 `toml:"attack"`
		Armour    float64 `toml:"armour"`
		Life      float64 `toml:"life"`
		MoveSpeed float64 `toml:"move_speed"`

		OnDeath []effectFile `toml:"on_death"`
	} `toml:"elite"`
}

func (c *Content) loadElites(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "elites", ".toml")
	if errors.Is(err, fs.ErrNotExist) {
		// A content set with no elite modifiers is valid: every mob is what it
		// says it is, which is where this game started.
		return nil
	}
	if err != nil {
		return err
	}

	for _, name := range files {
		data, err := rec.readAndRecord(name)
		if err != nil {
			return err
		}

		var f elitesFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Elite {
			if _, dup := c.Elites[id]; dup {
				return fmt.Errorf("%s: elite modifier %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: elite modifier %q has no name", name, id)
			}
			if raw.Weight <= 0 {
				return fmt.Errorf("%s: elite modifier %q has weight %d, so it could never be rolled",
					name, id, raw.Weight)
			}

			e := &Elite{
				ID:        id,
				Name:      raw.Name,
				Weight:    raw.Weight,
				Attack:    toFixedValue(raw.Attack),
				Armour:    toFixedValue(raw.Armour),
				Life:      toFixedValue(raw.Life),
				MoveSpeed: toFixedValue(raw.MoveSpeed),
			}

			for i, raw := range raw.OnDeath {
				eff, err := parseEffect(raw)
				if err != nil {
					return fmt.Errorf("%s: elite modifier %q on_death %d: %w", name, id, i+1, err)
				}
				e.OnDeath = append(e.OnDeath, eff)
			}

			if e.Attack == 0 && e.Armour == 0 && e.Life == 0 && e.MoveSpeed == 0 && len(e.OnDeath) == 0 {
				return fmt.Errorf("%s: elite modifier %q does nothing at all", name, id)
			}

			c.Elites[id] = e
		}
	}
	return nil
}

// EliteOrder returns every modifier in a stable order, with its weight.
//
// Sorted by id rather than in map order, because the roll comes from the
// room's own generator and a replay has to make the same choice -- which it
// cannot do if the candidates arrive in a different order each run.
func (c *Content) EliteOrder() ([]*Elite, []int) {
	ids := make([]string, 0, len(c.Elites))
	for id := range c.Elites {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]*Elite, 0, len(ids))
	weights := make([]int, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.Elites[id])
		weights = append(weights, c.Elites[id].Weight)
	}
	return out, weights
}
