package content

import (
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
	"github.com/ctrl-research/mmo/internal/fixed"
)

// Support modifiers.
//
// A support is a transformation applied to a skill's resolved effect list at
// compute time, never at authoring time. That is the whole design, and it is
// the reason the effect vocabulary is small and closed: because supports
// operate on the list rather than on knowledge of particular skills, a new
// support works with every existing compatible skill and a new skill works
// with every existing compatible support.
//
// That combinatorial property is the entire appeal, and it only exists if
// supports never special-case a skill by name. There is deliberately no field
// here for doing so.

// Support is one modifier that can be attached to a skill.
type Support struct {
	ID   string
	Name string

	// Tags are what a skill must carry for this support to attach. All of
	// them, not any: "melee" and "attack" together mean melee attacks, and
	// matching either would attach a melee support to a fireball.
	Tags []string

	// ManaMult scales the skill's cost. Every support costs something, or the
	// decision of which to use is not a decision.
	ManaMult fixed.F

	// Modifiers are applied in order to the effects the skill produces.
	Modifiers []SupportModifier
}

// SupportTarget names which effects a modifier applies to.
//
// By kind rather than by index: a support cannot know how many effects a skill
// has, and "the second effect" would mean something different for every skill
// it attached to.
type SupportTarget struct {
	// Kind restricts to one effect kind. Empty means every effect.
	Kind EffectKind

	// Element restricts to one element, so "increased fire damage" is a
	// support rather than a stat.
	Element string
}

// SupportModifier is one transformation of a matching effect.
type SupportModifier struct {
	Target SupportTarget

	// More scales magnitude multiplicatively, as a fixed-point multiplier
	// where one is unchanged. Negative-sounding supports are expressed as a
	// multiplier below one, so "25% less damage" is 0.75.
	More fixed.F

	// Repeat casts the effect this many times in total. A support that halves
	// damage and repeats three times is a real trade, and it is expressible
	// only because effects are data.
	Repeat int

	// AddChance raises the effect's chance to apply, in parts per million.
	AddChance int

	// DurationMult scales a buff's duration.
	DurationMult fixed.F

	// AddJumps adds chain hops or projectile pierces, both of which are
	// counted in the same field on an effect.
	AddJumps int
}

type supportsFile struct {
	Support map[string]struct {
		Name     string   `toml:"name"`
		Tags     []string `toml:"tags"`
		ManaMult float64  `toml:"mana_mult"`

		Modify []struct {
			Kind         string  `toml:"kind"`
			Element      string  `toml:"element"`
			More         float64 `toml:"more"`
			Repeat       int     `toml:"repeat"`
			AddChance    float64 `toml:"add_chance"`
			DurationMult float64 `toml:"duration_mult"`
			AddJumps     int     `toml:"add_jumps"`
		} `toml:"modify"`
	} `toml:"support"`
}

func (c *Content) loadSupports(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "supports", ".toml")
	if err != nil {
		return err
	}

	for _, name := range files {
		data, err := rec.readAndRecord(name)
		if err != nil {
			return err
		}

		var f supportsFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Support {
			if _, dup := c.Supports[id]; dup {
				return fmt.Errorf("%s: support %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: support %q has no name", name, id)
			}
			if len(raw.Tags) == 0 {
				return fmt.Errorf("%s: support %q has no tags, so it would attach to "+
					"every skill in the game", name, id)
			}
			if len(raw.Modify) == 0 {
				return fmt.Errorf("%s: support %q changes nothing", name, id)
			}

			manaMult := toFixedValue(raw.ManaMult)
			if manaMult <= 0 {
				// A support that costs nothing is a support everyone uses. One
				// is the floor rather than the default, so leaving it out is
				// visible as "free" rather than silently sensible.
				manaMult = fixed.One
			}

			s := &Support{ID: id, Name: raw.Name, Tags: raw.Tags, ManaMult: manaMult}

			for i, m := range raw.Modify {
				kind := EffectKind(m.Kind)
				if kind != "" && !validEffectKinds[kind] {
					return fmt.Errorf("%s: support %q modifier %d targets unknown effect "+
						"kind %q", name, id, i, m.Kind)
				}
				if m.Repeat < 0 {
					return fmt.Errorf("%s: support %q modifier %d repeats %d times",
						name, id, i, m.Repeat)
				}

				more := toFixedValue(m.More)
				if more <= 0 {
					more = fixed.One
				}
				duration := toFixedValue(m.DurationMult)
				if duration <= 0 {
					duration = fixed.One
				}

				s.Modifiers = append(s.Modifiers, SupportModifier{
					Target:       SupportTarget{Kind: kind, Element: m.Element},
					More:         more,
					Repeat:       m.Repeat,
					AddChance:    ratioToPPM(m.AddChance),
					DurationMult: duration,
					AddJumps:     m.AddJumps,
				})
			}

			c.Supports[id] = s
		}
	}
	return nil
}

// Attaches reports whether a support may be linked to a skill.
//
// Every tag, not any: "melee" and "attack" together mean melee attacks, and
// matching either would let a melee support attach to a fireball.
func (s *Support) Attaches(skill *Skill) bool {
	for _, tag := range s.Tags {
		if !skill.HasTag(tag) {
			return false
		}
	}
	return true
}

// Apply transforms an effect list.
//
// Returns a new list rather than editing in place: the skill definition is
// shared by every room on the node and read without locking, and a support is
// a per-cast view of it.
func (s *Support) Apply(effects []Effect) []Effect {
	out := make([]Effect, 0, len(effects))

	for _, e := range effects {
		repeat := 1

		for _, mod := range s.Modifiers {
			if !mod.matches(e) {
				continue
			}

			e.BaseMin = fixed.FromInt(e.BaseMin).Mul(mod.More).Int()
			e.BaseMax = fixed.FromInt(e.BaseMax).Mul(mod.More).Int()
			e.ScaleAttack = e.ScaleAttack.Mul(mod.More)

			if e.DurationTicks > 0 {
				e.DurationTicks = fixed.FromInt(e.DurationTicks).Mul(mod.DurationMult).Int()
			}
			if mod.AddChance > 0 && e.Chance > 0 {
				e.Chance = min(e.Chance+mod.AddChance, oneMillion)
			}
			e.Jumps += mod.AddJumps

			if mod.Repeat > repeat {
				repeat = mod.Repeat
			}
		}

		// Nested effects -- a projectile's payload, an area's tick -- are
		// transformed too. A support that increases fire damage should work on
		// a fireball whether the fire is applied directly or carried by
		// something the skill launches.
		if len(e.Effects) > 0 {
			e.Effects = s.Apply(e.Effects)
		}

		for i := 0; i < repeat; i++ {
			out = append(out, e)
		}
	}

	return out
}

// oneMillion is a probability of one, in parts per million.
const oneMillion = 1_000_000

// matches reports whether a modifier applies to an effect.
func (m SupportModifier) matches(e Effect) bool {
	if m.Target.Kind != "" && m.Target.Kind != e.Kind {
		return false
	}
	if m.Target.Element != "" && m.Target.Element != e.Element {
		return false
	}
	return true
}
