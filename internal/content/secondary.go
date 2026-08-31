package content

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
)

// Secondary skills and the resource nodes that raise them.
//
// These are the OSRS half of the game: woodcutting, mining, fishing and the
// rest rise from *use* rather than from combat experience, on OSRS's own
// curve, and they resolve on the 600 ms action tick rather than the 50 ms
// simulation tick (see architecture.md § The tick loop). Nothing about them is
// combat, and that is the point -- a player who does not want to fight for an
// evening still has something to do.

// ActionTicks is how many simulation ticks make one action tick.
//
// 12 x 50 ms = 600 ms, deliberately matching OSRS. A derived beat rather than
// a second loop: gathering is the only thing that runs on it, and running it
// as "every twelfth tick" means there is one clock in the room and no way for
// two to drift apart.
const ActionTicks = 12

// SecondarySkill is one gatherable or craftable discipline.
type SecondarySkill struct {
	ID   string
	Name string

	// ToolClass is the item class a character must have equipped to act on
	// this skill's nodes: "axe" for woodcutting, "pickaxe" for mining. Empty
	// means bare hands are enough, which is what fishing spots and herbs are.
	ToolClass string

	// ToolName is what to call the tool when telling a player they need one.
	//
	// Separate from ToolClass because a class is an identifier and this is
	// prose: "fishing_rod" is a perfectly good class and an embarrassing thing
	// to show somebody. Authored rather than derived by replacing underscores,
	// because a derive rule is a rule that is right until the first class it
	// is wrong for, and by then it is load-bearing.
	ToolName string
}

// ResourceNode is one thing on a map that can be gathered.
type ResourceNode struct {
	ID   string
	Name string

	// Skill is which secondary skill this node raises.
	Skill string

	// Level is the skill level required to gather it at all.
	Level int

	// Exp is the experience granted per yield. Per *yield* rather than per
	// action tick, so a node that takes longer is worth more -- otherwise the
	// fastest node at any level is always the best one, and the level
	// requirement decides nothing.
	Exp int64

	// Item and Qty are what one yield produces.
	Item string
	Qty  int

	// ChancePPM and MaxChancePPM bound the per-action-tick probability of a
	// yield, in millionths: the first at the node's own required level, the
	// second at the maximum skill level. In between it interpolates linearly.
	//
	// Two points rather than a formula because two points are what a designer
	// can reason about: "a new player gets a log every few seconds, a maxed
	// one nearly every tick" is a sentence, and a curve parameter is not.
	ChancePPM    int64
	MaxChancePPM int64

	// MinToolPower gates the node on equipment rather than on level, which is
	// what makes a better axe worth buying rather than just faster.
	MinToolPower int

	// Yields is how many times the node can be gathered before it is spent.
	// A node that never depleted would mean one player standing still forever,
	// which is the failure mode OSRS avoids by making trees fall over.
	Yields int

	// RespawnTicks is how long a spent node takes to come back.
	RespawnTicks int
}

// Tool is what an item contributes to gathering.
//
// On the item rather than in a separate table because it is a property of the
// item -- a bronze axe is a bronze axe wherever it is referenced -- and
// because the session already reads every equipped item to build a stat block.
type Tool struct {
	// Skill is which secondary skill it works for.
	Skill string

	// Power both gates and speeds: a node may demand a minimum, and whatever
	// is above that is added to the per-action-tick chance in millionths.
	//
	// One number doing both jobs deliberately. Separate "tier" and "speed"
	// numbers would let content define a tool that unlocks a node it is then
	// too slow to work, which is a tool nobody would ever equip.
	Power int
}

type secondaryFile struct {
	Skill map[string]struct {
		Name      string `toml:"name"`
		ToolClass string `toml:"tool_class"`
		ToolName  string `toml:"tool_name"`
	} `toml:"skill"`
}

type resourcesFile struct {
	Node map[string]struct {
		Name         string  `toml:"name"`
		Skill        string  `toml:"skill"`
		Level        int     `toml:"level"`
		Exp          int64   `toml:"exp"`
		Item         string  `toml:"item"`
		Qty          int     `toml:"qty"`
		ChancePct    float64 `toml:"chance_at_level"`
		MaxChancePct float64 `toml:"chance_at_max"`
		MinToolPower int     `toml:"min_tool_power"`
		Yields       int     `toml:"yields"`
		RespawnMs    int     `toml:"respawn_ms"`
	} `toml:"node"`
}

func (c *Content) loadSecondary(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "secondary", ".toml")
	if errors.Is(err, fs.ErrNotExist) {
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

		var f secondaryFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Skill {
			if _, dup := c.Secondary[id]; dup {
				return fmt.Errorf("%s: secondary skill %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: secondary skill %q has no name", name, id)
			}
			if raw.ToolClass == "" && raw.ToolName != "" {
				return fmt.Errorf("%s: secondary skill %q names a tool %q but needs no tool class",
					name, id, raw.ToolName)
			}

			toolName := raw.ToolName
			if toolName == "" {
				// Most classes are already a word: "axe", "pickaxe". Only the
				// ones that are not need naming.
				toolName = raw.ToolClass
			}

			c.Secondary[id] = &SecondarySkill{
				ID:        id,
				Name:      raw.Name,
				ToolClass: raw.ToolClass,
				ToolName:  toolName,
			}
		}
	}
	return nil
}

func (c *Content) loadResources(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "resources", ".toml")
	if errors.Is(err, fs.ErrNotExist) {
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

		var f resourcesFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Node {
			if _, dup := c.Nodes[id]; dup {
				return fmt.Errorf("%s: resource node %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: resource node %q has no name", name, id)
			}
			if raw.Item == "" {
				return fmt.Errorf("%s: resource node %q yields no item, so gathering it would do nothing",
					name, id)
			}
			if raw.Exp <= 0 {
				return fmt.Errorf("%s: resource node %q grants no experience; a node that never raises its skill is scenery",
					name, id)
			}
			if raw.ChancePct <= 0 {
				return fmt.Errorf("%s: resource node %q has no chance_at_level, so it would never yield",
					name, id)
			}
			if raw.MaxChancePct < raw.ChancePct {
				return fmt.Errorf("%s: resource node %q is slower at the maximum level (%g%%) than at %d (%g%%); levelling must not make a node worse",
					name, id, raw.MaxChancePct, raw.Level, raw.ChancePct)
			}
			if raw.Yields <= 0 {
				return fmt.Errorf("%s: resource node %q has no yields; a node that never depletes is one player standing still forever",
					name, id)
			}
			if raw.RespawnMs <= 0 {
				return fmt.Errorf("%s: resource node %q has no respawn_ms, so it would be gone for good the first time anyone used it",
					name, id)
			}

			level := raw.Level
			if level < 1 {
				level = 1
			}
			qty := raw.Qty
			if qty < 1 {
				qty = 1
			}

			c.Nodes[id] = &ResourceNode{
				ID:           id,
				Name:         raw.Name,
				Skill:        raw.Skill,
				Level:        level,
				Exp:          raw.Exp,
				Item:         raw.Item,
				Qty:          qty,
				ChancePPM:    int64(ratioToPPM(raw.ChancePct / 100)),
				MaxChancePPM: int64(ratioToPPM(raw.MaxChancePct / 100)),
				MinToolPower: raw.MinToolPower,
				Yields:       raw.Yields,
				RespawnTicks: msToTicks(raw.RespawnMs, TickRate),
			}
		}
	}
	return nil
}

// validateSecondary checks every resource node against the rest of the graph.
//
// All of these load cleanly and then fail in play: a node naming a renamed
// item is a tree a player can chop forever and never fill a bag from.
func (c *Content) validateSecondary() error {
	for id, node := range c.Nodes {
		skill, ok := c.Secondary[node.Skill]
		if !ok {
			return fmt.Errorf("content: resource node %q raises unknown secondary skill %q",
				id, node.Skill)
		}
		if _, ok := c.Items[node.Item]; !ok {
			return fmt.Errorf("content: resource node %q yields unknown item %q", id, node.Item)
		}
		if node.Level > c.Curves.MaxSkillLevel {
			return fmt.Errorf("content: resource node %q requires %s level %d, above the maximum of %d, so nobody could ever gather it",
				id, node.Skill, node.Level, c.Curves.MaxSkillLevel)
		}
		if node.MinToolPower > 0 && skill.ToolClass == "" {
			return fmt.Errorf("content: resource node %q demands tool power %d but %s needs no tool, so the requirement could never be met",
				id, node.MinToolPower, node.Skill)
		}
	}

	// Tools are checked from the item side, because an item claiming a skill
	// that does not exist is dead weight in the affix pool with nothing to
	// point at it.
	for id, item := range c.Items {
		if item.Tool == nil {
			continue
		}
		skill, ok := c.Secondary[item.Tool.Skill]
		if !ok {
			return fmt.Errorf("content: item %q is a tool for unknown secondary skill %q",
				id, item.Tool.Skill)
		}
		if skill.ToolClass != "" && item.Class != skill.ToolClass {
			return fmt.Errorf("content: item %q is a tool for %s, which needs class %q, but the item is class %q",
				id, skill.ID, skill.ToolClass, item.Class)
		}
	}

	// Every map's resource nodes must name a node that exists. The other
	// direction is deliberately not checked: a node defined but not yet placed
	// on any map is a normal step in authoring one.
	for mapID, m := range c.Maps {
		for _, spot := range m.Resources {
			if _, ok := c.Nodes[spot.NodeID]; !ok {
				return fmt.Errorf("content: map %q places unknown resource node %q",
					mapID, spot.NodeID)
			}
		}
	}
	return nil
}

// SecondaryLevelFor returns the level a given amount of experience reaches.
//
// The table is cumulative, so this is a scan from the top rather than a
// subtraction: unlike the main level, secondary experience is never spent.
func (c Curves) SecondaryLevelFor(exp int64) int {
	level := 1
	for l := 2; l <= c.MaxSkillLevel; l++ {
		if exp < c.SecondaryExp[l] {
			break
		}
		level = l
	}
	return level
}

// SecondaryExpAt returns the cumulative experience a level begins at.
func (c Curves) SecondaryExpAt(level int) int64 {
	if level < 1 {
		return 0
	}
	if level > c.MaxSkillLevel {
		level = c.MaxSkillLevel
	}
	return c.SecondaryExp[level]
}

// SecondaryNextAt returns the cumulative experience the *next* level begins
// at, and zero at the maximum level.
//
// Zero rather than the maximum's own total, so "there is no next level" is
// representable at all. Returning the same number twice would make a maxed
// skill's progress bar a division by zero at the one point every player
// eventually reaches.
func (c Curves) SecondaryNextAt(level int) int64 {
	if level >= c.MaxSkillLevel {
		return 0
	}
	return c.SecondaryExpAt(level + 1)
}

// GatherChancePPM returns the per-action-tick chance of a yield, in millionths.
//
// Interpolated between the node's two authored points by level, then raised by
// whatever tool power is above what the node demands. Clamped at one, because
// a chance above certainty is the same as certainty and a designer who wrote
// one should not get an out-of-range roll.
func (c *Content) GatherChancePPM(node *ResourceNode, level, toolPower int) int64 {
	if level < node.Level {
		return 0
	}

	span := c.Curves.MaxSkillLevel - node.Level
	chance := node.ChancePPM
	if span > 0 {
		over := level - node.Level
		if over > span {
			over = span
		}
		chance += (node.MaxChancePPM - node.ChancePPM) * int64(over) / int64(span)
	}

	// Tool power above the requirement, as millionths per point. A tool that
	// only just meets the requirement adds nothing, which is what makes the
	// next one up worth buying.
	if extra := toolPower - node.MinToolPower; extra > 0 {
		chance += int64(extra) * toolPowerPPM
	}

	if chance > oneMillion {
		chance = oneMillion
	}
	return chance
}

// toolPowerPPM is what one point of tool power above a node's requirement adds
// to the per-action-tick chance: one percentage point.
const toolPowerPPM = oneMillion / 100

// SecondaryOrder returns every secondary skill id in a stable order, so the
// skills panel and the database agree on what order to list them in.
func (c *Content) SecondaryOrder() []string { return sortedKeys(c.Secondary) }
