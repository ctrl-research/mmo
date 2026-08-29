package content

import (
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
)

// Buffs, debuffs, damage over time, and auras.
//
// A buff is two things at once: effects that fire on a beat, and stat
// modifiers that feed the same pipeline as gear and passives. That is what
// makes "burning" and "+20% attack speed for ten seconds" the same mechanism
// rather than two -- and it is why a support that lengthens durations works on
// both without knowing what either does.
//
// Like skills, a buff is never Go code.

// Buff is a temporary state on an entity.
type Buff struct {
	ID   string
	Name string

	// Kind separates helpful from harmful, which decides what a dispel takes
	// off and what an enemy's cleanse removes.
	Kind BuffKind

	// DurationTicks is how long it lasts. Zero means it lasts until removed,
	// which is what an aura is.
	DurationTicks int

	// TickInterval is how often Effects fire, in ticks. Zero fires them once,
	// on application.
	TickInterval int

	// MaxStacks bounds how many copies can be held. Stacks multiply the stat
	// modifiers and the ticked effects alike.
	MaxStacks int

	// RefreshOnApply restarts the duration when re-applied. Without it a
	// re-application adds a stack and the original still expires on schedule,
	// which is the difference between a damage-over-time you can maintain and
	// one you cannot.
	RefreshOnApply bool

	// Dispellable marks a buff that can be removed by an effect rather than
	// only by time.
	Dispellable bool

	// Effects fire on the tick interval: damage over time, healing over time,
	// a resource drain.
	Effects []Effect

	// StatMods are the modifiers this buff contributes while it is held. They
	// are the same shape as an item's, so they go through the same stat
	// pipeline rather than a parallel one.
	StatMods []StatMod
}

// BuffKind separates helpful from harmful.
type BuffKind string

const (
	BuffHelpful BuffKind = "buff"
	BuffHarmful BuffKind = "debuff"
)

// StatMod is one modifier a buff contributes.
//
// Deliberately the same three channels as everything else: flat added, summed
// increases, and multiplied "more". A buff that modified stats through some
// fourth mechanism would be a buff whose interaction with gear nobody could
// predict.
type StatMod struct {
	Stat      string
	Flat      int
	Increased int // parts per million
	More      int // parts per million
}

type buffsFile struct {
	Buff map[string]struct {
		Name           string `toml:"name"`
		Kind           string `toml:"kind"`
		DurationMs     int    `toml:"duration_ms"`
		TickMs         int    `toml:"tick_ms"`
		MaxStacks      int    `toml:"max_stacks"`
		RefreshOnApply bool   `toml:"refresh_on_apply"`
		Dispellable    bool   `toml:"dispellable"`

		Effects []effectFile `toml:"effects"`

		StatMods []struct {
			Stat      string  `toml:"stat"`
			Flat      int     `toml:"flat"`
			Increased float64 `toml:"increased"`
			More      float64 `toml:"more"`
		} `toml:"stat_mods"`
	} `toml:"buff"`
}

func (c *Content) loadBuffs(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "buffs", ".toml")
	if err != nil {
		return err
	}

	for _, name := range files {
		data, err := rec.readAndRecord(name)
		if err != nil {
			return err
		}

		var f buffsFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Buff {
			if _, dup := c.Buffs[id]; dup {
				return fmt.Errorf("%s: buff %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: buff %q has no name", name, id)
			}

			kind := BuffKind(raw.Kind)
			if kind != BuffHelpful && kind != BuffHarmful {
				return fmt.Errorf("%s: buff %q has unknown kind %q; want %q or %q",
					name, id, raw.Kind, BuffHelpful, BuffHarmful)
			}

			maxStacks := raw.MaxStacks
			if maxStacks <= 0 {
				maxStacks = 1
			}

			b := &Buff{
				ID:             id,
				Name:           raw.Name,
				Kind:           kind,
				DurationTicks:  msToTicks(raw.DurationMs, TickRate),
				TickInterval:   msToTicks(raw.TickMs, TickRate),
				MaxStacks:      maxStacks,
				RefreshOnApply: raw.RefreshOnApply,
				Dispellable:    raw.Dispellable,
			}

			for i, e := range raw.Effects {
				eff, err := parseEffect(e)
				if err != nil {
					return fmt.Errorf("%s: buff %q effect %d: %w", name, id, i, err)
				}
				b.Effects = append(b.Effects, eff)
			}

			for _, m := range raw.StatMods {
				if m.Stat == "" {
					return fmt.Errorf("%s: buff %q has a stat modifier with no stat", name, id)
				}
				b.StatMods = append(b.StatMods, StatMod{
					Stat:      m.Stat,
					Flat:      m.Flat,
					Increased: modifierToPPM(m.Increased),
					More:      modifierToPPM(m.More),
				})
			}

			if len(b.Effects) == 0 && len(b.StatMods) == 0 {
				return fmt.Errorf("%s: buff %q has no effects and no stat modifiers, "+
					"so applying it would do nothing", name, id)
			}
			if len(b.Effects) > 0 && b.TickInterval <= 0 && b.DurationTicks > 0 {
				return fmt.Errorf("%s: buff %q lasts %d ticks and has effects but no "+
					"tick_ms, so they would fire once and the duration would mean nothing",
					name, id, b.DurationTicks)
			}

			c.Buffs[id] = b
		}
	}
	return nil
}
