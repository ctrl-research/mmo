package content

import (
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
	"github.com/ctrl-research/mmo/internal/fixed"
)

// Skill is an active ability: targeting plus an ordered list of effects.
//
// A skill is never Go code. That is what makes hundreds of them tractable, and
// it is the precondition for PoE-style support modifiers in M6: a support
// transforms a skill's effect list, so a new support works with every existing
// compatible skill and a new skill works with every existing compatible
// support. Neither is possible if skills are functions.
//
// M1 implements only the `damage` effect. Unknown effect types are a load
// error rather than a silent no-op, so a skill that will not work fails the
// build instead of failing quietly in play.
type Skill struct {
	ID        string
	Name      string
	Class     string
	MaxRank   int
	CostMP    int
	Cooldown  int // ticks
	CastTime  int // ticks
	Animation string

	Targeting Targeting
	Effects   []Effect
	Tags      []string
}

// TargetKind is how a skill selects what it affects.
type TargetKind string

const (
	// TargetSelf affects only the caster.
	TargetSelf TargetKind = "self"

	// TargetCone is a wedge in front of the caster: the MapleStory melee
	// swing, and the shape M1 implements.
	TargetCone TargetKind = "cone"

	// TargetCircle is centred on the caster.
	TargetCircle TargetKind = "circle"
)

type Targeting struct {
	Kind TargetKind

	// Range is the reach, in world units.
	Range fixed.F

	// HalfHeight bounds a cone vertically. A 2D platformer needs this: without
	// it a ground-level swing hits anything on the platform above.
	HalfHeight fixed.F

	// MaxTargets caps how many entities one cast may hit, so a crowded room
	// cannot turn one swing into unbounded work inside the tick.
	MaxTargets int
}

// EffectKind names one entry in the effect vocabulary.
//
// New kinds are Go code and should be rare. New skills are TOML and should be
// constant. Adding an effect kind per skill means the vocabulary is factored
// wrong.
type EffectKind string

const (
	EffectDamage EffectKind = "damage"
)

// Effect is one step in a skill's resolution.
type Effect struct {
	Kind    EffectKind
	Element string

	// BaseMin and BaseMax bound the damage roll, inclusive.
	BaseMin int
	BaseMax int

	// ScaleAttack is how much of the caster's attack stat is added, as a
	// fixed-point multiplier.
	ScaleAttack fixed.F

	// PerRankPct increases the base roll by this percentage per rank above 1.
	PerRankPct int
}

type skillsFile struct {
	Skill map[string]struct {
		Name       string   `toml:"name"`
		Class      string   `toml:"class"`
		MaxRank    int      `toml:"max_rank"`
		CostMP     int      `toml:"cost_mp"`
		CooldownMs int      `toml:"cooldown_ms"`
		CastTimeMs int      `toml:"cast_time_ms"`
		Animation  string   `toml:"animation"`
		Tags       []string `toml:"tags"`

		Targeting struct {
			Kind       string  `toml:"kind"`
			Range      float64 `toml:"range"`
			HalfHeight float64 `toml:"half_height"`
			MaxTargets int     `toml:"max_targets"`
		} `toml:"targeting"`

		Effects []struct {
			Type    string `toml:"type"`
			Element string `toml:"element"`
			Base    *struct {
				Min int `toml:"min"`
				Max int `toml:"max"`
			} `toml:"base"`
			Scaling *struct {
				Attack float64 `toml:"attack"`
			} `toml:"scaling"`
			PerRank *struct {
				BasePct int `toml:"base_pct"`
			} `toml:"per_rank"`
		} `toml:"effects"`
	} `toml:"skill"`
}

var validTargetKinds = map[TargetKind]bool{
	TargetSelf:   true,
	TargetCone:   true,
	TargetCircle: true,
}

func (c *Content) loadSkills(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "skills", ".toml")
	if err != nil {
		return err
	}

	for _, name := range files {
		data, err := rec.readAndRecord(name)
		if err != nil {
			return err
		}

		var f skillsFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Skill {
			if _, dup := c.Skills[id]; dup {
				return fmt.Errorf("%s: skill %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: skill %q has no name", name, id)
			}

			kind := TargetKind(raw.Targeting.Kind)
			if !validTargetKinds[kind] {
				return fmt.Errorf("%s: skill %q has unknown targeting kind %q", name, id, raw.Targeting.Kind)
			}
			if kind != TargetSelf && raw.Targeting.Range <= 0 {
				return fmt.Errorf("%s: skill %q has range %v; it could never reach anything",
					name, id, raw.Targeting.Range)
			}

			maxTargets := raw.Targeting.MaxTargets
			if maxTargets <= 0 {
				maxTargets = 1
			}
			maxRank := raw.MaxRank
			if maxRank <= 0 {
				maxRank = 1
			}

			s := &Skill{
				ID:        id,
				Name:      raw.Name,
				Class:     raw.Class,
				MaxRank:   maxRank,
				CostMP:    raw.CostMP,
				Cooldown:  msToTicks(raw.CooldownMs, TickRate),
				CastTime:  msToTicks(raw.CastTimeMs, TickRate),
				Animation: raw.Animation,
				Tags:      raw.Tags,
				Targeting: Targeting{
					Kind:       kind,
					Range:      toFixedValue(raw.Targeting.Range),
					HalfHeight: toFixedValue(raw.Targeting.HalfHeight),
					MaxTargets: maxTargets,
				},
			}

			if len(raw.Effects) == 0 {
				return fmt.Errorf("%s: skill %q has no effects; casting it would do nothing", name, id)
			}

			for i, e := range raw.Effects {
				if EffectKind(e.Type) != EffectDamage {
					return fmt.Errorf(
						"%s: skill %q effect %d has type %q. M1 implements only %q; "+
							"the rest of the vocabulary arrives in M6",
						name, id, i, e.Type, EffectDamage)
				}
				if e.Base == nil {
					return fmt.Errorf("%s: skill %q effect %d has no base damage range", name, id, i)
				}
				if e.Base.Min < 0 || e.Base.Max < e.Base.Min {
					return fmt.Errorf("%s: skill %q effect %d has an invalid base range %d-%d",
						name, id, i, e.Base.Min, e.Base.Max)
				}

				eff := Effect{
					Kind:    EffectDamage,
					Element: e.Element,
					BaseMin: e.Base.Min,
					BaseMax: e.Base.Max,
				}
				if e.Scaling != nil {
					eff.ScaleAttack = toFixedValue(e.Scaling.Attack)
				}
				if e.PerRank != nil {
					eff.PerRankPct = e.PerRank.BasePct
				}
				s.Effects = append(s.Effects, eff)
			}

			c.Skills[id] = s
		}
	}
	return nil
}

// HasTag reports whether the skill carries a tag. Support modifiers in M6
// attach by tag rather than by naming individual skills, which is what makes
// them compose.
func (s *Skill) HasTag(tag string) bool {
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}
