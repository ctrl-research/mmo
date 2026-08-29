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
// wrong -- and it is the vocabulary being small and general that makes support
// modifiers work at all, since a support transforms effects and cannot know
// about kinds invented for one skill.
type EffectKind string

const (
	// Offence and restoration.
	EffectDamage  EffectKind = "damage"
	EffectHeal    EffectKind = "heal"
	EffectRestore EffectKind = "restore"

	// EffectSplitDamage is one hit divided evenly among everyone it lands on.
	//
	// It is the vocabulary's answer to "this needs more than one person". A
	// hit that would kill anyone alone is survivable by six standing together,
	// so the counterplay is a party moving as a group rather than a party with
	// better gear. Nothing else in the vocabulary can express that, because
	// every other effect resolves against one target without knowing whether
	// there are others.
	EffectSplitDamage EffectKind = "split_damage"

	// State, applied to a target for a while.
	EffectApplyBuff  EffectKind = "apply_buff"
	EffectRemoveBuff EffectKind = "remove_buff"
	EffectShield     EffectKind = "shield"

	// Movement, resolved by the simulation.
	EffectDash      EffectKind = "dash"
	EffectKnockback EffectKind = "knockback"

	// Composition: effects that reach further than the cast that produced
	// them. These are what make one skill feel unlike another, and they are
	// also where the combinatorial appeal of supports comes from.
	EffectChain        EffectKind = "chain"
	EffectTriggerSkill EffectKind = "trigger_skill"
	EffectProjectile   EffectKind = "spawn_projectile"
	EffectArea         EffectKind = "area_persist"
)

// validEffectKinds is the whole vocabulary. Anything else in a file is a load
// error rather than a silent no-op: a skill that will not work should fail the
// build, not fail quietly in play.
var validEffectKinds = map[EffectKind]bool{
	EffectDamage:       true,
	EffectSplitDamage:  true,
	EffectHeal:         true,
	EffectRestore:      true,
	EffectApplyBuff:    true,
	EffectRemoveBuff:   true,
	EffectShield:       true,
	EffectDash:         true,
	EffectKnockback:    true,
	EffectChain:        true,
	EffectTriggerSkill: true,
	EffectProjectile:   true,
	EffectArea:         true,
}

// Effect is one step in a skill's resolution.
//
// One struct for the whole vocabulary rather than an interface per kind. That
// is deliberate: a support modifier rewrites effects it has never heard of, so
// every effect has to be inspectable and copyable without knowing its type.
// The cost is fields that mean nothing for most kinds, which is visible and
// harmless; the alternative costs the entire support system.
type Effect struct {
	Kind    EffectKind
	Element string

	// BaseMin and BaseMax bound the roll, inclusive: damage, healing, the
	// resource restored, or the size of a shield.
	BaseMin int
	BaseMax int

	// ScaleAttack is how much of the caster's attack stat is added, as a
	// fixed-point multiplier.
	ScaleAttack fixed.F

	// PerRankPct increases the base roll by this percentage per rank above 1.
	PerRankPct int

	// Chance is the probability the effect applies at all, in parts per
	// million to match the drop tables. Zero means always.
	Chance int

	// BuffID names the buff to apply or remove.
	BuffID string

	// DurationTicks overrides a buff's own duration, for a skill that applies
	// a shorter or longer version of a shared effect.
	DurationTicks int

	// Stacks is how many stacks to apply at once, for a skill that lands
	// several at a time.
	Stacks int

	// SkillID names the skill a trigger casts. Recursion is refused at load,
	// because a skill that triggers itself is an infinite loop inside a tick.
	SkillID string

	// Speed and Distance are movement: how fast a dash or a projectile
	// travels, and how far. For knockback, Speed is the impulse.
	Speed    fixed.F
	Distance fixed.F

	// Jumps and Falloff describe a chain: how many extra targets it reaches
	// and how much weaker each hop is, as a fixed-point multiplier.
	Jumps   int
	Falloff fixed.F

	// Radius bounds a persistent area or a chain's hop distance.
	Radius fixed.F

	// TickInterval is how often a persistent area applies its effects.
	TickInterval int

	// Effects are the effects a projectile or an area applies. Nested rather
	// than referencing another skill, so a projectile's payload is written
	// where the projectile is.
	Effects []Effect
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

		Effects []effectFile `toml:"effects"`
	} `toml:"skill"`
}

// effectFile is one effect as written. Recursive, because a projectile or a
// ground area carries the effects it applies rather than pointing at a skill
// that does -- a payload written where the thing that delivers it is written.
type effectFile struct {
	Type    string `toml:"type"`
	Element string `toml:"element"`

	Base *struct {
		Min int `toml:"min"`
		Max int `toml:"max"`
	} `toml:"base"`

	Scaling *struct {
		Attack float64 `toml:"attack"`
	} `toml:"scaling"`

	PerRank *struct {
		BasePct int `toml:"base_pct"`
	} `toml:"per_rank"`

	Chance float64 `toml:"chance"`

	Buff       string `toml:"buff"`
	DurationMs int    `toml:"duration_ms"`
	Stacks     int    `toml:"stacks"`

	Skill string `toml:"skill"`

	Speed    float64 `toml:"speed"`
	Distance float64 `toml:"distance"`

	Jumps   int     `toml:"jumps"`
	Falloff float64 `toml:"falloff"`

	Radius  float64      `toml:"radius"`
	TickMs  int          `toml:"tick_ms"`
	Effects []effectFile `toml:"effects"`
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
				eff, err := parseEffect(e)
				if err != nil {
					return fmt.Errorf("%s: skill %q effect %d: %w", name, id, i, err)
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

// parseEffect turns one written effect into a resolved one.
//
// Every kind is validated for the fields it actually needs, because an effect
// missing them does nothing at all -- and an effect that silently does nothing
// is the hardest kind of content bug to find: the skill casts, the animation
// plays, and the number never appears.
func parseEffect(e effectFile) (Effect, error) {
	kind := EffectKind(e.Type)
	if !validEffectKinds[kind] {
		return Effect{}, fmt.Errorf("unknown effect type %q", e.Type)
	}

	eff := Effect{
		Kind:          kind,
		Element:       e.Element,
		BuffID:        e.Buff,
		SkillID:       e.Skill,
		Stacks:        e.Stacks,
		Jumps:         e.Jumps,
		Chance:        ratioToPPM(e.Chance),
		DurationTicks: msToTicks(e.DurationMs, TickRate),
		TickInterval:  msToTicks(e.TickMs, TickRate),
		Speed:         toFixedValue(e.Speed),
		Distance:      toFixedValue(e.Distance),
		Falloff:       toFixedValue(e.Falloff),
		Radius:        toFixedValue(e.Radius),
	}

	if e.Base != nil {
		if e.Base.Min < 0 || e.Base.Max < e.Base.Min {
			return Effect{}, fmt.Errorf("invalid base range %d-%d", e.Base.Min, e.Base.Max)
		}
		eff.BaseMin, eff.BaseMax = e.Base.Min, e.Base.Max
	}
	if e.Scaling != nil {
		eff.ScaleAttack = toFixedValue(e.Scaling.Attack)
	}
	if e.PerRank != nil {
		eff.PerRankPct = e.PerRank.BasePct
	}

	for i, nested := range e.Effects {
		child, err := parseEffect(nested)
		if err != nil {
			return Effect{}, fmt.Errorf("nested effect %d: %w", i, err)
		}
		eff.Effects = append(eff.Effects, child)
	}

	return eff, validateEffect(eff)
}

// validateEffect checks that an effect has what its kind needs to do anything.
func validateEffect(e Effect) error {
	switch e.Kind {
	case EffectDamage, EffectSplitDamage, EffectHeal, EffectRestore, EffectShield:
		if e.BaseMax <= 0 && e.ScaleAttack <= 0 {
			return fmt.Errorf("%s has no base amount and no scaling, so it would do nothing", e.Kind)
		}

	case EffectApplyBuff, EffectRemoveBuff:
		if e.BuffID == "" {
			return fmt.Errorf("%s names no buff", e.Kind)
		}

	case EffectTriggerSkill:
		if e.SkillID == "" {
			return fmt.Errorf("trigger_skill names no skill")
		}

	case EffectDash, EffectKnockback:
		if e.Speed <= 0 {
			return fmt.Errorf("%s has no speed", e.Kind)
		}

	case EffectChain:
		if e.Jumps <= 0 {
			return fmt.Errorf("chain has no jumps")
		}
		if e.Radius <= 0 {
			return fmt.Errorf("chain has no radius, so it could never find a second target")
		}

	case EffectProjectile:
		if e.Speed <= 0 {
			return fmt.Errorf("spawn_projectile has no speed, so it would sit where it was cast")
		}
		if len(e.Effects) == 0 {
			return fmt.Errorf("spawn_projectile carries no effects, so hitting something would do nothing")
		}

	case EffectArea:
		if e.Radius <= 0 {
			return fmt.Errorf("area_persist has no radius")
		}
		if e.DurationTicks <= 0 {
			return fmt.Errorf("area_persist has no duration, so it would vanish the tick it appeared")
		}
		if len(e.Effects) == 0 {
			return fmt.Errorf("area_persist carries no effects")
		}
	}
	return nil
}
