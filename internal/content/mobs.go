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
)

var validAIProfiles = map[AIProfile]bool{
	AIPassive:         true,
	AIAggressiveMelee: true,
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

			c.Mobs[id] = m
		}
	}
	return nil
}
