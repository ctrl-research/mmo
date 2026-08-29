package content

import (
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
	"github.com/ctrl-research/mmo/internal/fixed"
)

// AIProfile names a behaviour state machine implemented in Go.
//
// Profiles are code and parameters are content. A handful of profiles covers
// most mobs; bosses get bespoke ones in M7, because a boss's mechanics *are*
// the encounter and expressing them as parameters would mean inventing a
// scripting language nobody asked for.
type AIProfile string

const (
	// AIPassive never attacks and never chases.
	AIPassive AIProfile = "passive"

	// AIAggressiveMelee chases anything that comes within aggro range and
	// attacks in contact, leashing home if pulled too far.
	AIAggressiveMelee AIProfile = "aggressive_melee"

	// AIBoss runs an encounter: phases entered at health thresholds, abilities
	// that telegraph before they land, and an enrage timer.
	//
	// The profile is Go and the encounter is content. A boss's mechanics are
	// the encounter, and expressing "which ability, at what health, after how
	// long a wind-up" as parameters is authorable; expressing the state
	// machine that runs them would mean inventing a scripting language nobody
	// asked for.
	AIBoss AIProfile = "boss"
)

var validAIProfiles = map[AIProfile]bool{
	AIPassive:         true,
	AIAggressiveMelee: true,
	AIBoss:            true,
}

// Mob is a hostile entity type.
type Mob struct {
	ID    string
	Name  string
	Level int

	HP     int
	Attack int
	Armour int
	Exp    int64

	// Width and Height are the collision box, which is also the hitbox. One
	// box rather than two: a visual hitbox that disagrees with the collision
	// box is a bug report generator.
	Width  fixed.F
	Height fixed.F

	DropTable string

	AI        MobAI
	Abilities []MobAbility

	// Phases are the encounter, for a boss. Empty for everything else.
	//
	// Entered in order as health falls, never gone back to: a boss that
	// returned to an earlier phase on being healed would be a boss whose
	// fight could be reset by a mistake.
	Phases []BossPhase
}

// BossPhase is one stage of an encounter.
type BossPhase struct {
	Name string

	// AtHPPercent is the health at or below which this phase begins, as a
	// percentage. The first phase is 100.
	AtHPPercent int

	// EnrageTicks is how long the boss may stay in this phase before it gains
	// the enrage buff. Zero never enrages.
	//
	// An enrage is a clock, not a punishment: it stops a fight being won by
	// attrition from a party that cannot beat the mechanics.
	EnrageTicks int

	// EnrageBuff is applied when the clock runs out.
	EnrageBuff string

	// Abilities are what this phase can do, each with its own cooldown and
	// wind-up.
	Abilities []BossAbility

	// OnEnter is cast once when the phase begins: a shield going up, adds
	// being summoned, a shout. A phase change that is only a number is a
	// phase change nobody notices.
	OnEnter string
}

// BossAbility is one telegraphed attack.
type BossAbility struct {
	Skill string

	// Cooldown is how long between uses, in ticks.
	Cooldown int

	// TelegraphTicks is the wind-up before it lands.
	//
	// This is what makes a boss fight a fight rather than a damage race: the
	// attack is announced, and a player who reads it can move. Zero means it
	// lands immediately, which is right for a basic swing and wrong for
	// anything worth dodging.
	TelegraphTicks int

	// Target decides what the ability aims at.
	Target BossTarget
}

// BossTarget is how an ability chooses what it aims at.
type BossTarget string

const (
	// BossTargetCurrent aims at whoever the boss is already fighting.
	BossTargetCurrent BossTarget = "current"

	// BossTargetRandom aims at any player in the room, which is what stops a
	// fight being one person standing still while everyone else ignores it.
	BossTargetRandom BossTarget = "random"

	// BossTargetFarthest aims at whoever is furthest away, which is what
	// reaches the people who thought distance was safety.
	BossTargetFarthest BossTarget = "farthest"

	// BossTargetSelf centres on the boss.
	BossTargetSelf BossTarget = "self"
)

var validBossTargets = map[BossTarget]bool{
	BossTargetCurrent:  true,
	BossTargetRandom:   true,
	BossTargetFarthest: true,
	BossTargetSelf:     true,
}

// MobAI holds the tuning for a profile.
type MobAI struct {
	Profile AIProfile

	AggroRange  fixed.F
	LeashRange  fixed.F
	AttackRange fixed.F

	// MoveSpeed is per tick, converted at load from the per-second value
	// content authors write.
	MoveSpeed fixed.F

	// IdleTickInterval is how often a mob with no target runs its behaviour.
	//
	// Under per-player mob layering a room holds roughly layers x mobs
	// entities, and most of them are idle at any moment. Ticking those on a
	// slower beat is the single largest saving available, and it is invisible
	// in play because an idle mob has nothing to decide.
	IdleTickInterval int
}

// MobAbility is a skill a mob may use, with its own independent cooldown.
type MobAbility struct {
	Skill    string
	Weight   int
	Cooldown int // ticks
}

type mobsFile struct {
	Mob map[string]struct {
		Name      string  `toml:"name"`
		Level     int     `toml:"level"`
		HP        int     `toml:"hp"`
		Attack    int     `toml:"attack"`
		Armour    int     `toml:"armour"`
		Exp       int64   `toml:"exp"`
		Width     float64 `toml:"width"`
		Height    float64 `toml:"height"`
		DropTable string  `toml:"drop_table"`

		AI struct {
			Profile          string  `toml:"profile"`
			AggroRange       float64 `toml:"aggro_range"`
			LeashRange       float64 `toml:"leash_range"`
			AttackRange      float64 `toml:"attack_range"`
			MoveSpeed        float64 `toml:"move_speed"`
			IdleTickInterval int     `toml:"idle_tick_interval"`
		} `toml:"ai"`

		Abilities []struct {
			Skill      string `toml:"skill"`
			Weight     int    `toml:"weight"`
			CooldownMs int    `toml:"cooldown_ms"`
		} `toml:"abilities"`

		Phases []struct {
			Name          string `toml:"name"`
			AtHPPercent   int    `toml:"at_hp_percent"`
			EnrageAfterMs int    `toml:"enrage_after_ms"`
			EnrageBuff    string `toml:"enrage_buff"`
			OnEnter       string `toml:"on_enter"`

			Abilities []struct {
				Skill       string `toml:"skill"`
				CooldownMs  int    `toml:"cooldown_ms"`
				TelegraphMs int    `toml:"telegraph_ms"`
				Target      string `toml:"target"`
			} `toml:"abilities"`
		} `toml:"phases"`
	} `toml:"mob"`
}

func (c *Content) loadMobs(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "mobs", ".toml")
	if err != nil {
		return err
	}

	for _, name := range files {
		data, err := rec.readAndRecord(name)
		if err != nil {
			return err
		}

		var f mobsFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Mob {
			if _, dup := c.Mobs[id]; dup {
				return fmt.Errorf("%s: mob %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: mob %q has no name", name, id)
			}
			if raw.HP <= 0 {
				return fmt.Errorf("%s: mob %q has %d HP; it would die on spawn", name, id, raw.HP)
			}
			if raw.Width <= 0 || raw.Height <= 0 {
				return fmt.Errorf("%s: mob %q has a zero-sized body (%vx%v); nothing could hit it",
					name, id, raw.Width, raw.Height)
			}

			profile := AIProfile(raw.AI.Profile)
			if !validAIProfiles[profile] {
				return fmt.Errorf("%s: mob %q has unknown AI profile %q", name, id, raw.AI.Profile)
			}
			if profile != AIPassive {
				if raw.AI.LeashRange > 0 && raw.AI.LeashRange < raw.AI.AggroRange {
					return fmt.Errorf(
						"%s: mob %q has leash_range (%v) below aggro_range (%v); "+
							"it would aggro and immediately leash, forever",
						name, id, raw.AI.LeashRange, raw.AI.AggroRange)
				}
			}

			idleInterval := raw.AI.IdleTickInterval
			if idleInterval <= 0 {
				idleInterval = 8
			}

			m := &Mob{
				ID:        id,
				Name:      raw.Name,
				Level:     raw.Level,
				HP:        raw.HP,
				Attack:    raw.Attack,
				Armour:    raw.Armour,
				Exp:       raw.Exp,
				Width:     toFixedValue(raw.Width),
				Height:    toFixedValue(raw.Height),
				DropTable: raw.DropTable,
				AI: MobAI{
					Profile:          profile,
					AggroRange:       toFixedValue(raw.AI.AggroRange),
					LeashRange:       toFixedValue(raw.AI.LeashRange),
					AttackRange:      toFixedValue(raw.AI.AttackRange),
					MoveSpeed:        perSecondToPerTick(raw.AI.MoveSpeed, TickRate),
					IdleTickInterval: idleInterval,
				},
			}

			for i, a := range raw.Abilities {
				if a.Skill == "" {
					return fmt.Errorf("%s: mob %q ability %d names no skill", name, id, i)
				}
				weight := a.Weight
				if weight <= 0 {
					weight = 1
				}
				m.Abilities = append(m.Abilities, MobAbility{
					Skill:    a.Skill,
					Weight:   weight,
					Cooldown: msToTicks(a.CooldownMs, TickRate),
				})
			}

			for i, p := range raw.Phases {
				if p.Name == "" {
					return fmt.Errorf("%s: mob %q phase %d has no name; a phase change "+
						"nobody can see is a phase change nobody notices", name, id, i)
				}
				if p.AtHPPercent <= 0 || p.AtHPPercent > 100 {
					return fmt.Errorf("%s: mob %q phase %q begins at %d%% health, which "+
						"is outside 1-100", name, id, p.Name, p.AtHPPercent)
				}
				if i > 0 && p.AtHPPercent >= raw.Phases[i-1].AtHPPercent {
					// Out of order means a phase that can never be entered, or
					// one entered immediately. Both look like the fight being
					// broken rather than like a content mistake.
					return fmt.Errorf("%s: mob %q phase %q begins at %d%%, no lower than "+
						"the phase before it; phases are entered in order as health falls",
						name, id, p.Name, p.AtHPPercent)
				}
				if len(p.Abilities) == 0 {
					return fmt.Errorf("%s: mob %q phase %q has no abilities, so entering "+
						"it would stop the fight", name, id, p.Name)
				}

				phase := BossPhase{
					Name:        p.Name,
					AtHPPercent: p.AtHPPercent,
					EnrageTicks: msToTicks(p.EnrageAfterMs, TickRate),
					EnrageBuff:  p.EnrageBuff,
					OnEnter:     p.OnEnter,
				}
				if phase.EnrageTicks > 0 && phase.EnrageBuff == "" {
					return fmt.Errorf("%s: mob %q phase %q has an enrage timer and no "+
						"enrage buff, so nothing would happen when it expired",
						name, id, p.Name)
				}

				for j, a := range p.Abilities {
					target := BossTarget(a.Target)
					if target == "" {
						target = BossTargetCurrent
					}
					if !validBossTargets[target] {
						return fmt.Errorf("%s: mob %q phase %q ability %d has unknown "+
							"target %q", name, id, p.Name, j, a.Target)
					}

					phase.Abilities = append(phase.Abilities, BossAbility{
						Skill:          a.Skill,
						Cooldown:       msToTicks(a.CooldownMs, TickRate),
						TelegraphTicks: msToTicks(a.TelegraphMs, TickRate),
						Target:         target,
					})
				}
				m.Phases = append(m.Phases, phase)
			}

			if m.AI.Profile == AIBoss && len(m.Phases) == 0 {
				return fmt.Errorf("%s: mob %q runs the boss profile and has no phases, "+
					"so it would stand there", name, id)
			}
			if m.AI.Profile != AIBoss && len(m.Phases) > 0 {
				return fmt.Errorf("%s: mob %q has phases but does not run the boss "+
					"profile, so nothing would use them", name, id)
			}

			c.Mobs[id] = m
		}
	}
	return nil
}
