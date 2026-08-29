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
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// Content is everything the game is made of, loaded and validated.
//
// It is immutable once built and shared by every room on the node, read
// concurrently with no locking. Nothing here is written after Load returns.
type Content struct {
	Balance Balance
	Curves  Curves

	Items    map[string]*Item
	Affixes  map[string]*Affix
	Mobs     map[string]*Mob
	Drops    map[string]*DropTable
	Skills   map[string]*Skill
	Buffs    map[string]*Buff
	Supports map[string]*Support
	Classes  map[string]*Class

	// Passives is the tree. One graph, shared by every class, with each class
	// starting somewhere different on it.
	Passives *PassiveTree
	Maps     map[string]*Map

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
		Items:    make(map[string]*Item),
		Affixes:  make(map[string]*Affix),
		Mobs:     make(map[string]*Mob),
		Drops:    make(map[string]*DropTable),
		Skills:   make(map[string]*Skill),
		Buffs:    make(map[string]*Buff),
		Supports: make(map[string]*Support),
		Classes:  make(map[string]*Class),
		Maps:     make(map[string]*Map),
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
		// Buffs before skills: a skill that applies one is checked against the
		// buff actually existing, and an unloaded reference is exactly the
		// kind of silent no-op the vocabulary is validated to prevent.
		{"buffs", c.loadBuffs},
		{"skills", c.loadSkills},
		{"supports", c.loadSupports},
		{"classes", c.loadClasses},
		{"passives", c.loadPassives},
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

	// Skills and buffs reference each other, and every one of those references
	// is silent at load and baffling in play: the skill casts, the animation
	// plays, and nothing happens.
	for id, sk := range c.Skills {
		if err := c.checkEffects("skill "+id, sk.Effects, map[string]bool{id: true}); err != nil {
			return err
		}
		c.resolveEffects(sk.Effects)
	}
	for id, b := range c.Buffs {
		if err := c.checkEffects("buff "+id, b.Effects, nil); err != nil {
			return err
		}
		c.resolveEffects(b.Effects)
		for _, mod := range b.StatMods {
			if _, ok := stats.Parse(mod.Stat); !ok {
				return fmt.Errorf("content: buff %q modifies unknown stat %q", id, mod.Stat)
			}
		}
	}

	// A class whose starting skill does not exist produces a character who
	// cannot act, and the symptom is a button that does nothing.
	for id, class := range c.Classes {
		for _, skillID := range class.StartingSkills {
			skill, ok := c.Skills[skillID]
			if !ok {
				return fmt.Errorf("content: class %q starts with unknown skill %q", id, skillID)
			}
			if skill.Class != "" && skill.Class != id {
				return fmt.Errorf("content: class %q starts with %q, which belongs to %q",
					id, skillID, skill.Class)
			}
		}
		if class.PrimaryStat != "" {
			if _, ok := stats.Parse(class.PrimaryStat); !ok {
				return fmt.Errorf("content: class %q has unknown primary stat %q",
					id, class.PrimaryStat)
			}
		}
	}

	if err := c.validatePassives(); err != nil {
		return err
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

// modifierToPPM converts an authored stat modifier to parts-per-million.
//
// Signed, unlike a probability: a keystone's drawback, a chill's slow, and a
// support's "25% less damage" are all negative, and clamping them to zero
// silently discards the half of the design that makes them choices. This was
// exactly that bug -- every keystone in the tree gave its upside for free
// until the test that asked "does each keystone trade something" was written.
func modifierToPPM(v float64) int {
	const ppm = 1_000_000

	if v > 1 {
		// Above +100% is legitimate and unbounded; the clamp below is only
		// for the probability case.
		return int(v*ppm + 0.5)
	}
	if v < 0 {
		return int(v*ppm - 0.5)
	}
	return int(v*ppm + 0.5)
}

// ratioToPPM converts an authored probability to parts-per-million.
//
// Content is authored with floats because "chance = 0.15" is what a designer
// wants to write. The simulation rolls in integers because a float comparison
// is one more place client and server could disagree, and because replay must
// reproduce exactly.
//
// Clamped to [0, 1], which is right for a probability and wrong for anything
// else -- use modifierToPPM for stat modifiers, where negatives are the point.
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

// checkEffects walks an effect tree and resolves every reference in it.
//
// The `casting` set is the chain of skills that led here, so a trigger loop is
// caught at load rather than as an infinite recursion inside a tick with
// players in the room.
func (c *Content) checkEffects(owner string, effects []Effect, casting map[string]bool) error {
	for i, e := range effects {
		switch e.Kind {
		case EffectApplyBuff, EffectRemoveBuff:
			if _, ok := c.Buffs[e.BuffID]; !ok {
				return fmt.Errorf("content: %s effect %d references unknown buff %q",
					owner, i, e.BuffID)
			}

		case EffectTriggerSkill:
			target, ok := c.Skills[e.SkillID]
			if !ok {
				return fmt.Errorf("content: %s effect %d triggers unknown skill %q",
					owner, i, e.SkillID)
			}
			if casting[e.SkillID] {
				return fmt.Errorf("content: %s effect %d triggers %q, which is already "+
					"casting; a trigger loop would not terminate inside a tick",
					owner, i, e.SkillID)
			}

			// Walk into it with this skill added to the chain, so a loop of
			// any length is caught rather than only a skill triggering itself.
			next := make(map[string]bool, len(casting)+1)
			for k := range casting {
				next[k] = true
			}
			next[e.SkillID] = true

			if err := c.checkEffects("skill "+e.SkillID, target.Effects, next); err != nil {
				return err
			}
		}

		if err := c.checkEffects(owner, e.Effects, casting); err != nil {
			return err
		}
	}
	return nil
}

// resolveEffects fills in what an effect left implicit.
//
// Specifically: an apply_buff that does not name a duration inherits the
// buff's own. Resolved here rather than at application time so that a support
// modifying durations has a concrete number to scale -- otherwise a support
// that lengthens buffs would silently do nothing to every skill that used a
// buff's default duration, which is nearly all of them.
func (c *Content) resolveEffects(effects []Effect) {
	for i := range effects {
		e := &effects[i]

		if e.Kind == EffectApplyBuff && e.DurationTicks == 0 {
			if buff, ok := c.Buffs[e.BuffID]; ok {
				e.DurationTicks = buff.DurationTicks
			}
		}
		c.resolveEffects(e.Effects)
	}
}
