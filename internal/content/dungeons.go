package content

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
)

// Dungeons.
//
// A dungeon is a private map with a shape: you go in as a party, you clear it
// in order, and you come out. Everything that makes it different from an
// ordinary map is here, and all of it is content -- the runtime in
// internal/world/room knows about stages and clears, not about slimes.
//
// The ordering rule is the whole of the progression. A stage's mobs do not
// exist until the stage before it is cleared, so "kill the guards, then the
// king" needs no doors, no keys, and no geometry that changes underfoot --
// which also means the client's collision never has to be told anything, and
// prediction cannot drift over a wall that is there on one side and not the
// other.

// Dungeon is one instanced encounter.
type Dungeon struct {
	ID   string
	Name string

	// Map is the map this dungeon runs on. It must be privately placed, or it
	// would not be an instance: two parties would walk into each other's run.
	Map string

	// MinLevel gates the entrance.
	MinLevel int

	// LockoutTicks is how long after a clear a character may not enter again.
	//
	// Per character rather than per party, so a group carrying a friend
	// through does not spend the friend's lockout, and so leaving a party does
	// not launder one.
	LockoutTicks int

	// ExitMap and ExitSpawn are where a run ends, cleared or wiped. A dungeon
	// with no way out but the way in is a dungeon a wiped party is stuck in.
	ExitMap   string
	ExitSpawn string

	// Stages are cleared in order.
	Stages []Stage
}

// Stage is one step of a dungeon.
type Stage struct {
	Name string

	// Spawns names the map's mob spawn points that belong to this stage.
	// Nothing they hold exists until the stage begins, and the stage is
	// cleared when every one of them has produced its whole population and
	// none of it is left alive.
	Spawns []string
}

type dungeonsFile struct {
	Dungeon map[string]struct {
		Name      string `toml:"name"`
		Map       string `toml:"map"`
		MinLevel  int    `toml:"min_level"`
		LockoutMs int    `toml:"lockout_ms"`
		ExitMap   string `toml:"exit_map"`
		ExitSpawn string `toml:"exit_spawn"`

		Stages []struct {
			Name   string   `toml:"name"`
			Spawns []string `toml:"spawns"`
		} `toml:"stages"`
	} `toml:"dungeon"`
}

func (c *Content) loadDungeons(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "dungeons", ".toml")
	if errors.Is(err, fs.ErrNotExist) {
		// A content set with no dungeons is a valid one -- every map is a
		// field map, and validateDungeons still refuses a privately placed
		// map that nothing runs on.
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

		var f dungeonsFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Dungeon {
			if _, dup := c.Dungeons[id]; dup {
				return fmt.Errorf("%s: dungeon %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: dungeon %q has no name", name, id)
			}
			if raw.Map == "" {
				return fmt.Errorf("%s: dungeon %q names no map", name, id)
			}
			if raw.ExitMap == "" {
				return fmt.Errorf("%s: dungeon %q names no exit map; a wiped party would have nowhere to go", name, id)
			}
			if len(raw.Stages) == 0 {
				return fmt.Errorf("%s: dungeon %q has no stages, so it could never be cleared", name, id)
			}

			d := &Dungeon{
				ID:           id,
				Name:         raw.Name,
				Map:          raw.Map,
				MinLevel:     raw.MinLevel,
				LockoutTicks: msToTicks(raw.LockoutMs, TickRate),
				ExitMap:      raw.ExitMap,
				ExitSpawn:    raw.ExitSpawn,
			}

			for i, s := range raw.Stages {
				if s.Name == "" {
					return fmt.Errorf("%s: dungeon %q stage %d has no name", name, id, i+1)
				}
				if len(s.Spawns) == 0 {
					return fmt.Errorf("%s: dungeon %q stage %q names no spawn points, so it would clear the instant it began",
						name, id, s.Name)
				}
				d.Stages = append(d.Stages, Stage{Name: s.Name, Spawns: s.Spawns})
			}

			c.Dungeons[id] = d
		}
	}
	return nil
}

// validateDungeons checks each dungeon against the map it runs on.
//
// Every one of these is a mistake that loads cleanly and then fails in play,
// which is the worst kind: a stage naming a spawn point that was renamed
// leaves a party standing in an empty room with nothing to kill and no way to
// progress.
func (c *Content) validateDungeons() error {
	for id, d := range c.Dungeons {
		m, ok := c.Maps[d.Map]
		if !ok {
			return fmt.Errorf("content: dungeon %q runs on unknown map %q", id, d.Map)
		}
		if m.Placement != "private" {
			return fmt.Errorf("content: dungeon %q runs on map %q, which is %s-placed; a dungeon that is not an instance is two parties in one run",
				id, d.Map, m.Placement)
		}
		if _, ok := c.Maps[d.ExitMap]; !ok {
			return fmt.Errorf("content: dungeon %q exits to unknown map %q", id, d.ExitMap)
		}

		// Every spawn point on the map must belong to exactly one stage.
		// Belonging to none would mean mobs that appear before the dungeon
		// starts and are part of no stage's clear condition -- which reads in
		// play as a stage that will not finish.
		claimed := make(map[string]string, len(m.MobSpawns))
		for _, stage := range d.Stages {
			for _, spawn := range stage.Spawns {
				if !m.HasMobSpawn(spawn) {
					return fmt.Errorf("content: dungeon %q stage %q names spawn point %q, which map %q does not have",
						id, stage.Name, spawn, d.Map)
				}
				if other, dup := claimed[spawn]; dup {
					return fmt.Errorf("content: dungeon %q gives spawn point %q to both %q and %q",
						id, spawn, other, stage.Name)
				}
				claimed[spawn] = stage.Name
			}
		}
		for _, sp := range m.MobSpawns {
			if _, ok := claimed[sp.Name]; !ok {
				return fmt.Errorf("content: dungeon %q leaves spawn point %q out of every stage; its mobs would appear before the run began and belong to no clear condition",
					id, sp.Name)
			}
		}
	}

	// The other direction: a privately placed map with no dungeon is almost
	// certainly a dungeon someone forgot to define, and it would have no
	// entry rules, no progression, and no way out.
	for id, m := range c.Maps {
		if m.Placement != "private" {
			continue
		}
		if c.DungeonForMap(id) == nil {
			return fmt.Errorf("content: map %q is privately placed but no dungeon runs on it", id)
		}
	}
	return nil
}

// DungeonForMap returns the dungeon running on a map, or nil.
func (c *Content) DungeonForMap(mapID string) *Dungeon {
	for _, d := range c.Dungeons {
		if d.Map == mapID {
			return d
		}
	}
	return nil
}

// StageFor returns the index of the stage a spawn point belongs to, and
// whether it belongs to one at all.
func (d *Dungeon) StageFor(spawn string) (int, bool) {
	for i, stage := range d.Stages {
		for _, s := range stage.Spawns {
			if s == spawn {
				return i, true
			}
		}
	}
	return 0, false
}
