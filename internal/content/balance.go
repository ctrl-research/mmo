package content

import (
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
	"github.com/ctrl-research/mmo/internal/fixed"
)

// TickRate must match room.TickRate.
//
// It is duplicated here rather than imported because internal/content must not
// depend on internal/world/room -- the room depends on content, and the cycle
// would be the wrong way round. The constant is asserted equal in the room's
// tests, so the two cannot drift silently.
const TickRate = 20

// Balance holds every tunable constant.
//
// A magic number in Go is a number nobody can tune without a deploy, and every
// one of them eventually needs tuning. Nothing in the simulation should reach
// for a literal that a designer might want to change.
type Balance struct {
	Combat CombatBalance
	Drops  DropBalance
	Party  PartyBalance
	Rooms  RoomBalance
	Chat   ChatBalance
}

// ChatBalance limits what a player can say and how often.
//
// Per channel, because they cost different amounts: a global line reaches
// everyone online and a local one reaches a room, so one deserves a much
// tighter budget than the other. A single shared limit either strangles local
// chat or leaves global wide open.
type ChatBalance struct {
	// MaxLength is the longest message accepted, in characters (not bytes --
	// a limit in bytes silently gives players of some languages less to say).
	MaxLength int

	// PerMinute is the sustained rate for each channel. A player starts with
	// a full bucket, so a burst on arriving is not punished.
	PerMinute map[string]int
}

// ChatLimit returns the per-minute allowance for a channel, falling back to
// the local rate for anything unnamed.
func (b ChatBalance) ChatLimit(channel string) int {
	if n, ok := b.PerMinute[channel]; ok {
		return n
	}
	return b.PerMinute["local"]
}

// RoomBalance tunes the two things that keep a layered room inside its tick
// budget, and the point at which an empty one stops costing anything.
type RoomBalance struct {
	// SpawnActivationRange is how close a player must be to a spawn point for
	// it to produce mobs.
	//
	// Under per-player layering a room holds roughly layers x mobs entities,
	// and most of them are somewhere the owner is not. Gating on distance
	// means a layer only populates the part of the map its player is actually
	// in. Set it wider than half a viewport, or mobs appear on screen out of
	// nothing.
	SpawnActivationRange fixed.F

	// IdleTicks is how long a room runs with nobody in it before it stops.
	//
	// Not zero: a player walking through a portal and straight back should
	// find the room they left, not a fresh one with every mob respawned. Not
	// long either, since an empty room still costs a goroutine and 20 wakeups
	// a second.
	IdleTicks int
}

type CombatBalance struct {
	// CritMultiplier is applied to damage on a critical hit.
	CritMultiplier fixed.F

	// ResistanceCap bounds elemental resistance, so stacking resistance can
	// never reach immunity.
	ResistanceCap fixed.F

	// ArmourDivisor shapes the armour curve: reduction is
	// armour / (armour + divisor * incoming). Strong against many small hits,
	// weak against one large one, which is what gives armour and resistance
	// genuinely different roles instead of being two words for the same thing.
	ArmourDivisor int

	// MinDamage is the floor after all mitigation, so a hit always registers.
	MinDamage int

	// HitFlashTicks is how long a damaged entity reads as hit.
	HitFlashTicks int

	// CorpseTicks is how long a dead mob remains before being removed, giving
	// the client time to play a death animation.
	CorpseTicks int

	// DownedTicks is how long a character lies at zero health before they may
	// return.
	//
	// A delay rather than an instant respawn, because dying should cost
	// something even where there is nothing to lose: a fight you can rejoin
	// the moment you lose it is a fight with no stakes. Short enough not to be
	// a punishment for its own sake.
	DownedTicks int

	// ManaRegen and ManaRegenInCombat are the fractions of maximum mana
	// restored per second, in stat millionths.
	//
	// Two rates rather than one. Without any regeneration a caster who spends
	// their mana can never attack again, which is where this started. With a
	// single generous rate there is no reason to ever stop casting. The lower
	// in-combat rate makes a long fight something to pace, and the higher
	// out-of-combat one means the pacing is recovered by stepping away rather
	// than by logging out.
	ManaRegen         int
	ManaRegenInCombat int

	// LifeRegen is the fraction of maximum health restored per second, out of
	// combat only. Regenerating health mid-fight would undo the fight.
	LifeRegen int

	// CombatTicks is how long after taking damage a character counts as being
	// in combat.
	CombatTicks int

	// ReviveGraceTicks is how long a character who has just come back cannot
	// be harmed.
	//
	// A spawn point is a fixed place, and something is often standing on it.
	// Without this, coming back next to whatever killed you means dying again
	// before you can move, and paying the penalty each time -- a loop the
	// player has no way out of. It ends the moment they attack, so it buys a
	// chance to leave rather than a free opening.
	ReviveGraceTicks int

	// DeathExpPenalty is the fraction of the progress made toward the current
	// level that is lost on death, in stat millionths.
	//
	// Of progress *within* the level rather than of total experience, so a
	// death never costs a level and never costs a high-level character
	// disproportionately more than a low-level one. Zero disables it.
	DeathExpPenalty int
}

type DropBalance struct {
	// GroundTicks is how long a drop lies on the ground before vanishing.
	GroundTicks int

	// PickupRange is how close a player must be to loot.
	PickupRange fixed.F

	// ScatterRange is how far drops spread from the kill, so a stack of loot
	// is not one unclickable pile.
	ScatterRange fixed.F
}

type PartyBalance struct {
	ExpShareRange fixed.F
	MaxSize       int

	// LootLockTicks is how long a round-robin drop is reserved for the member
	// it was assigned to. After it, anyone in the party may take it, so a
	// member who has stepped away does not leave loot on the floor forever.
	LootLockTicks int
}

type balanceFile struct {
	Combat struct {
		CritMultiplier float64 `toml:"crit_multiplier"`
		ResistanceCap  float64 `toml:"resistance_cap"`
		ArmourDivisor  int     `toml:"armour_divisor"`
		MinDamage      int     `toml:"min_damage"`
		HitFlashMs     int     `toml:"hit_flash_ms"`
		CorpseMs       int     `toml:"corpse_ms"`
		DownedMs       int     `toml:"downed_ms"`
		ReviveGraceMs  int     `toml:"revive_grace_ms"`
		ManaRegen      float64 `toml:"mana_regen"`
		ManaRegenFight float64 `toml:"mana_regen_in_combat"`
		LifeRegen      float64 `toml:"life_regen"`
		CombatMs       int     `toml:"combat_ms"`
		DeathExpPct    float64 `toml:"death_exp_penalty"`
	} `toml:"combat"`

	Drops struct {
		GroundMs     int     `toml:"ground_ms"`
		PickupRange  float64 `toml:"pickup_range"`
		ScatterRange float64 `toml:"scatter_range"`
	} `toml:"drops"`

	Party struct {
		ExpShareRange float64 `toml:"exp_share_range"`
		MaxSize       int     `toml:"max_size"`
		LootLockMs    int     `toml:"loot_lock_ms"`
	} `toml:"party"`

	Rooms struct {
		SpawnActivationRange float64 `toml:"spawn_activation_range"`
		IdleMs               int     `toml:"idle_ms"`
	} `toml:"rooms"`

	Chat struct {
		MaxLength int            `toml:"max_length"`
		PerMinute map[string]int `toml:"per_minute"`
	} `toml:"chat"`
}

func (c *Content) loadBalance(fsys fs.FS, rec *hashRecorder) error {
	data, err := rec.readAndRecord("balance.toml")
	if err != nil {
		return err
	}

	var f balanceFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("balance.toml: %w", err)
	}

	c.Balance = Balance{
		Combat: CombatBalance{
			CritMultiplier:    toFixedValue(f.Combat.CritMultiplier),
			ResistanceCap:     toFixedValue(f.Combat.ResistanceCap),
			ArmourDivisor:     f.Combat.ArmourDivisor,
			MinDamage:         f.Combat.MinDamage,
			HitFlashTicks:     msToTicks(f.Combat.HitFlashMs, TickRate),
			CorpseTicks:       msToTicks(f.Combat.CorpseMs, TickRate),
			DownedTicks:       msToTicks(f.Combat.DownedMs, TickRate),
			ReviveGraceTicks:  msToTicks(f.Combat.ReviveGraceMs, TickRate),
			ManaRegen:         ratioToPPM(f.Combat.ManaRegen),
			ManaRegenInCombat: ratioToPPM(f.Combat.ManaRegenFight),
			LifeRegen:         ratioToPPM(f.Combat.LifeRegen),
			CombatTicks:       msToTicks(f.Combat.CombatMs, TickRate),
			DeathExpPenalty:   ratioToPPM(f.Combat.DeathExpPct),
		},
		Drops: DropBalance{
			GroundTicks:  msToTicks(f.Drops.GroundMs, TickRate),
			PickupRange:  toFixedValue(f.Drops.PickupRange),
			ScatterRange: toFixedValue(f.Drops.ScatterRange),
		},
		Party: PartyBalance{
			ExpShareRange: toFixedValue(f.Party.ExpShareRange),
			MaxSize:       f.Party.MaxSize,
			LootLockTicks: msToTicks(f.Party.LootLockMs, TickRate),
		},
		Rooms: RoomBalance{
			SpawnActivationRange: toFixedValue(f.Rooms.SpawnActivationRange),
			IdleTicks:            msToTicks(f.Rooms.IdleMs, TickRate),
		},
		Chat: ChatBalance{
			MaxLength: f.Chat.MaxLength,
			PerMinute: f.Chat.PerMinute,
		},
	}

	return c.Balance.validate()
}

func (b Balance) validate() error {
	switch {
	case b.Combat.CritMultiplier < fixed.One:
		return fmt.Errorf("balance: crit_multiplier is %v; below 1.0 a critical hit would deal less damage", b.Combat.CritMultiplier)
	case b.Combat.ResistanceCap < 0 || b.Combat.ResistanceCap >= fixed.One:
		return fmt.Errorf("balance: resistance_cap must be in [0, 1); at 1.0 resistance becomes immunity")
	case b.Combat.ArmourDivisor <= 0:
		return fmt.Errorf("balance: armour_divisor must be positive")
	case b.Combat.MinDamage < 0:
		return fmt.Errorf("balance: min_damage cannot be negative")
	case b.Combat.CorpseTicks < 0:
		return fmt.Errorf("balance: corpse_ms cannot be negative")
	case b.Combat.DownedTicks <= 0:
		return fmt.Errorf("balance: downed_ms must be positive, or death would be a flicker rather than a setback")
	case b.Combat.ReviveGraceTicks < 0:
		return fmt.Errorf("balance: revive_grace_ms cannot be negative")
	case b.Combat.ManaRegen <= 0:
		return fmt.Errorf("balance: mana_regen must be positive, or a caster who runs dry can never cast again")
	case b.Combat.ManaRegenInCombat < 0:
		return fmt.Errorf("balance: mana_regen_in_combat cannot be negative")
	case b.Combat.ManaRegenInCombat > b.Combat.ManaRegen:
		return fmt.Errorf("balance: mana_regen_in_combat is above the out-of-combat rate, so leaving a fight would slow recovery")
	case b.Combat.LifeRegen < 0:
		return fmt.Errorf("balance: life_regen cannot be negative")
	case b.Combat.CombatTicks <= 0:
		return fmt.Errorf("balance: combat_ms must be positive, or nobody would ever count as being in combat")
	case b.Combat.DeathExpPenalty < 0 || b.Combat.DeathExpPenalty >= 1_000_000:
		return fmt.Errorf("balance: death_exp_penalty must be in [0, 1); at 1.0 a death would erase the whole level")
	case b.Drops.GroundTicks <= 0:
		return fmt.Errorf("balance: ground_ms must be positive, or drops would vanish instantly")
	case b.Drops.PickupRange <= 0:
		return fmt.Errorf("balance: pickup_range must be positive, or nothing could be looted")
	case b.Party.MaxSize <= 0:
		return fmt.Errorf("balance: party max_size must be positive")
	case b.Party.LootLockTicks < 0:
		return fmt.Errorf("balance: party loot_lock_ms cannot be negative")
	case b.Rooms.SpawnActivationRange <= 0:
		return fmt.Errorf("balance: spawn_activation_range must be positive, or no mob would ever spawn")
	case b.Rooms.IdleTicks <= 0:
		return fmt.Errorf("balance: rooms idle_ms must be positive, or a room would be torn down the tick it empties")
	case b.Chat.MaxLength <= 0:
		return fmt.Errorf("balance: chat max_length must be positive, or nobody could say anything")
	}

	// Every channel needs a rate, including local -- it is the fallback, so a
	// missing entry there would silently give every unnamed channel zero.
	for _, channel := range []string{"local", "global", "whisper", "party", "guild"} {
		if b.Chat.PerMinute[channel] <= 0 {
			return fmt.Errorf("balance: chat per_minute.%s must be positive", channel)
		}
	}
	return nil
}
