package content

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/ctrl-research/mmo/internal/fixed"
)

// Content is everything the game is made of, loaded and validated.
//
// It is immutable once built and shared by every room on the node, read
// concurrently with no locking. Nothing here is written after Load returns.
type Content struct {
	Balance Balance
	Curves  Curves

	Items   map[string]*Item
	Affixes map[string]*Affix
	Mobs    map[string]*Mob
	Drops   map[string]*DropTable
	Skills  map[string]*Skill
	Maps    map[string]*Map

	// Waypoints indexes every fast-travel destination by its global ID.
	//
	// Fast travel names a waypoint without naming its map -- that is the point
	// of a world map -- so the lookup has to exist somewhere. Building it once
	// at load beats scanning every map on every teleport.
	Waypoints map[string]WaypointRef

	// Hash identifies this exact set of content. The client sends it at the
	// handshake and a mismatch is refused: a client that thinks a mob has 400
	// HP when the server says 900 produces bug reports that are nearly
	// impossible to read.
	Hash string
}

// Load reads, validates, links, and hashes every content file.
//
// It fails on the first problem rather than starting with a partial world.
// A server that silently comes up with a broken drop table produces bug
// reports weeks later that trace back to a warning nobody read.
func Load(fsys fs.FS) (*Content, error) {
	c := &Content{
		Items:   make(map[string]*Item),
		Affixes: make(map[string]*Affix),
		Mobs:    make(map[string]*Mob),
		Drops:   make(map[string]*DropTable),
		Skills:  make(map[string]*Skill),
		Maps:    make(map[string]*Map),
	}

	hasher := sha256.New()

	// Loaded in dependency order so that a reference is always resolvable
	// against something already present.
	steps := []struct {
		name string
		fn   func(fs.FS, *hashRecorder) error
	}{
		{"balance", c.loadBalance},
		{"curves", c.loadCurves},
		{"items", c.loadItems},
		{"affixes", c.loadAffixes},
		{"drop tables", c.loadDrops},
		{"skills", c.loadSkills},
		{"mobs", c.loadMobs},
		{"maps", c.loadMaps},
	}

	rec := &hashRecorder{h: hasher, fsys: fsys}
	for _, step := range steps {
		if err := step.fn(fsys, rec); err != nil {
			return nil, fmt.Errorf("content: loading %s: %w", step.name, err)
		}
	}

	if err := c.verify(); err != nil {
		return nil, err
	}

	c.Hash = hex.EncodeToString(hasher.Sum(nil))[:16]
	return c, nil
}

// verify checks every cross-reference across the whole content graph.
//
// These are the errors that actually happen: a drop table naming an item that
// was renamed, a mob pointing at a deleted table, a map spawning a mob that no
// longer exists. Each is silent at load time and baffling at play time.
func (c *Content) verify() error {
	for id, m := range c.Mobs {
		if m.DropTable != "" {
			if _, ok := c.Drops[m.DropTable]; !ok {
				return fmt.Errorf("content: mob %q references unknown drop table %q", id, m.DropTable)
			}
		}
		for _, a := range m.Abilities {
			if _, ok := c.Skills[a.Skill]; !ok {
				return fmt.Errorf("content: mob %q references unknown skill %q", id, a.Skill)
			}
		}
	}

	// Every equipment base must have at least one affix able to roll on it, or
	// it can only ever drop as a plain white item -- which looks like a bug in
	// the drop system rather than a deliberate design choice.
	for id, item := range c.Items {
		if !item.IsEquipment() {
			continue
		}
		usable := 0
		for _, a := range c.Affixes {
			if a.AppliesTo(item.Class) {
				usable++
			}
		}
		if usable == 0 {
			return fmt.Errorf("content: item %q (class %q) has no affixes that can roll on it",
				id, item.Class)
		}
	}

	for id, t := range c.Drops {
		for i, e := range t.Entries {
			if e.Item == "" {
				continue
			}
			if _, ok := c.Items[e.Item]; !ok {
				return fmt.Errorf("content: drop table %q entry %d references unknown item %q", id, i, e.Item)
			}
		}
	}

	// Portals must lead somewhere that exists. A portal to a renamed map is
	// invisible until someone walks into it and goes nowhere.
	for id, m := range c.Maps {
		for _, p := range m.Portals {
			target, ok := c.Maps[p.TargetMap]
			if !ok {
				return fmt.Errorf("content: map %q portal %q leads to unknown map %q",
					id, p.Name, p.TargetMap)
			}
			if p.TargetSpawn == "" {
				continue
			}
			found := false
			for _, sp := range target.Spawns {
				if sp.Name == p.TargetSpawn {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("content: map %q portal %q targets spawn %q, which map %q does not have",
					id, p.Name, p.TargetSpawn, p.TargetMap)
			}
		}
	}

	// Waypoint ids are global, since fast travel names them across maps.
	c.Waypoints = make(map[string]WaypointRef)
	for id, m := range c.Maps {
		for _, w := range m.Waypoints {
			if other, dup := c.Waypoints[w.ID]; dup {
				return fmt.Errorf("content: waypoint %q is defined on both %q and %q",
					w.ID, other.MapID, id)
			}
			c.Waypoints[w.ID] = WaypointRef{MapID: id, Waypoint: w}
		}
	}

	for id, m := range c.Maps {
		for _, sp := range m.MobSpawns {
			if _, ok := c.Mobs[sp.MobID]; !ok {
				return fmt.Errorf("content: map %q spawn %q references unknown mob %q", id, sp.Name, sp.MobID)
			}
		}
	}

	return nil
}

// hashRecorder accumulates a stable hash over every file that is read.
//
// Files are hashed in the order they are loaded, and each loader sorts its
// directory listing, so the hash depends only on content -- never on
// filesystem iteration order, which would make it differ between machines and
// break the handshake for no reason.
type hashRecorder struct {
	h    interface{ Write([]byte) (int, error) }
	fsys fs.FS
}

func (r *hashRecorder) record(name string, data []byte) {
	r.h.Write([]byte(name))
	r.h.Write(data)
}

// readAndRecord reads a file and folds it into the content hash.
func (r *hashRecorder) readAndRecord(name string) ([]byte, error) {
	data, err := fs.ReadFile(r.fsys, name)
	if err != nil {
		return nil, err
	}
	r.record(name, data)
	return data, nil
}

// listFiles returns the sorted names of files under dir with the given
// extension. Sorting is what makes the content hash reproducible.
func listFiles(fsys fs.FS, dir, ext string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		out = append(out, path.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// ratioToPPM converts an authored probability to parts-per-million.
//
// Content is authored with floats because "chance = 0.15" is what a designer
// wants to write. The simulation rolls in integers because a float comparison
// is one more place client and server could disagree, and because replay must
// reproduce exactly. This is the single conversion point between the two.
func ratioToPPM(v float64) int {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 1_000_000
	}
	return int(v*1_000_000 + 0.5)
}

// toFixedValue converts an authored decimal to fixed-point.
func toFixedValue(v float64) fixed.F {
	return fixed.F(int32(v*float64(fixed.One) + 0.5))
}

// perSecondToPerTick converts an authored speed into the simulation's units.
//
// Content states speeds per second because that is how a designer thinks about
// them; the simulation works per tick so it never divides by a tick rate at
// runtime.
func perSecondToPerTick(v float64, tickRate int) fixed.F {
	return toFixedValue(v / float64(tickRate))
}

// msToTicks converts an authored duration into whole ticks, rounding up so a
// cooldown is never shorter than authored.
func msToTicks(ms int, tickRate int) int {
	if ms <= 0 {
		return 0
	}
	msPerTick := 1000 / tickRate
	return (ms + msPerTick - 1) / msPerTick
}

// WaypointRef is a waypoint together with the map it is on.
type WaypointRef struct {
	MapID string
	Waypoint
}
