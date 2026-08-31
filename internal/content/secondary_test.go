package content

import (
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// The secondary-skill loader's job beyond parsing is refusing content that
// loads cleanly and then does nothing in play: a node yielding a renamed item
// is a tree a player can chop forever and never fill a bag from, and a node
// above the maximum skill level is scenery with a level requirement on it.

const secondaryTOML = `
[skill.chopping]
name = "Chopping"
tool_class = "axe"

[skill.picking]
name = "Picking"

# A class that is not a word needs prose to show a player.
[skill.angling]
name = "Angling"
tool_class = "fishing_rod"
tool_name = "fishing rod"
`

const resourcesTOML = `
[node.oak]
name = "Oak"
skill = "chopping"
level = 1
exp = 25
item = "test.item"
qty = 1
chance_at_level = 20
chance_at_max = 60
min_tool_power = 1
yields = 3
respawn_ms = 8000

[node.berries]
name = "Berries"
skill = "picking"
level = 1
exp = 10
item = "test.item"
qty = 2
chance_at_level = 30
chance_at_max = 70
yields = 2
respawn_ms = 4000
`

const toolTOML = `
[item."test.axe"]
name = "Test Axe"
kind = "equipment"
slot = "weapon"
class = "axe"
level = 1

[item."test.axe".tool]
skill = "chopping"
power = 2
`

func secondaryFS() fstest.MapFS {
	f := minimalFS()
	f["secondary/test.toml"] = file(secondaryTOML)
	f["resources/test.toml"] = file(resourcesTOML)
	f["items/tools.toml"] = file(toolTOML)
	return f
}

func TestSecondaryContentLoads(t *testing.T) {
	c, err := Load(secondaryFS())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := len(c.Secondary); got != 3 {
		t.Fatalf("loaded %d secondary skills, want 3", got)
	}
	if got := c.SecondaryOrder(); got[0] != "angling" || got[1] != "chopping" || got[2] != "picking" {
		t.Errorf("SecondaryOrder = %v, want a stable alphabetical order", got)
	}
	if got := c.Secondary["picking"].ToolClass; got != "" {
		t.Errorf("picking wants tool class %q, want none", got)
	}
	// A class that is already a word needs no separate name.
	if got := c.Secondary["chopping"].ToolName; got != "axe" {
		t.Errorf("chopping's tool name = %q, want it to default to the class", got)
	}
	if got := c.Secondary["angling"].ToolName; got != "fishing rod" {
		t.Errorf("angling's tool name = %q, want the authored prose; the class id "+
			"%q is an embarrassing thing to show a player",
			got, c.Secondary["angling"].ToolClass)
	}

	oak, ok := c.Nodes["oak"]
	if !ok {
		t.Fatal("the oak node did not load")
	}
	// Percentages are millionths by the time anything rolls against them.
	if oak.ChancePPM != 200_000 || oak.MaxChancePPM != 600_000 {
		t.Errorf("chances = %d and %d, want 200000 and 600000",
			oak.ChancePPM, oak.MaxChancePPM)
	}
	if oak.RespawnTicks != 8000/(1000/TickRate) {
		t.Errorf("respawn = %d ticks, want %d", oak.RespawnTicks, 8000/(1000/TickRate))
	}

	if tool := c.Items["test.axe"].Tool; tool == nil || tool.Skill != "chopping" || tool.Power != 2 {
		t.Errorf("the axe's tool block = %+v, want chopping power 2", tool)
	}
}

func TestSecondaryLevelsFollowTheOSRSCurve(t *testing.T) {
	c, err := Load(secondaryFS())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The published table: level 2 at 83, level 3 at 174, level 99 at 13034431.
	tests := []struct {
		exp   int64
		level int
	}{
		{0, 1},
		{82, 1},
		{83, 2},
		{173, 2},
		{174, 3},
		{13_034_430, 98},
		{13_034_431, 99},
		{99_999_999, 99},
	}
	for _, tt := range tests {
		if got := c.Curves.SecondaryLevelFor(tt.exp); got != tt.level {
			t.Errorf("SecondaryLevelFor(%d) = %d, want %d", tt.exp, got, tt.level)
		}
	}

	if got := c.Curves.SecondaryExpAt(2); got != 83 {
		t.Errorf("SecondaryExpAt(2) = %d, want 83", got)
	}
	// Past the top is clamped rather than out of range, because a maxed skill
	// asks for "the next level" exactly like every other one.
	if got := c.Curves.SecondaryExpAt(c.Curves.MaxSkillLevel + 5); got != c.Curves.SecondaryExpAt(99) {
		t.Errorf("above the maximum SecondaryExpAt gives %d, want it clamped", got)
	}
}

func TestBrokenSecondaryContentIsRejected(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(fstest.MapFS)
		wantErr string
	}{
		{
			name: "a node raises a skill that does not exist",
			mutate: func(f fstest.MapFS) {
				f["resources/test.toml"] = file(strings.Replace(resourcesTOML,
					`skill = "chopping"`, `skill = "no_such_skill"`, 1))
			},
			wantErr: "unknown secondary skill",
		},
		{
			name: "a node yields an item that does not exist",
			mutate: func(f fstest.MapFS) {
				f["resources/test.toml"] = file(strings.Replace(resourcesTOML,
					`item = "test.item"`, `item = "test.ghost"`, 1))
			},
			wantErr: "yields unknown item",
		},
		{
			name: "a node needs a level nobody can reach",
			mutate: func(f fstest.MapFS) {
				f["resources/test.toml"] = file(strings.Replace(resourcesTOML,
					"level = 1", "level = 200", 1))
			},
			wantErr: "above the maximum",
		},
		{
			name: "a node demands tool power from a bare-handed skill",
			mutate: func(f fstest.MapFS) {
				f["resources/test.toml"] = file(strings.Replace(resourcesTOML,
					"yields = 2\nrespawn_ms = 4000", "min_tool_power = 3\nyields = 2\nrespawn_ms = 4000", 1))
			},
			wantErr: "needs no tool",
		},
		{
			name: "a node gets slower as the skill rises",
			mutate: func(f fstest.MapFS) {
				f["resources/test.toml"] = file(strings.Replace(resourcesTOML,
					"chance_at_max = 60", "chance_at_max = 10", 1))
			},
			wantErr: "levelling must not make a node worse",
		},
		{
			name: "a node never depletes",
			mutate: func(f fstest.MapFS) {
				f["resources/test.toml"] = file(strings.Replace(resourcesTOML,
					"yields = 3", "yields = 0", 1))
			},
			wantErr: "one player standing still forever",
		},
		{
			name: "a node never comes back",
			mutate: func(f fstest.MapFS) {
				f["resources/test.toml"] = file(strings.Replace(resourcesTOML,
					"respawn_ms = 8000", "respawn_ms = 0", 1))
			},
			wantErr: "gone for good",
		},
		{
			name: "a node never yields",
			mutate: func(f fstest.MapFS) {
				f["resources/test.toml"] = file(strings.Replace(resourcesTOML,
					"chance_at_level = 20", "chance_at_level = 0", 1))
			},
			wantErr: "would never yield",
		},
		{
			name: "a node grants no experience",
			mutate: func(f fstest.MapFS) {
				f["resources/test.toml"] = file(strings.Replace(resourcesTOML,
					"exp = 25", "exp = 0", 1))
			},
			wantErr: "is scenery",
		},
		{
			name: "a bare-handed skill names a tool",
			mutate: func(f fstest.MapFS) {
				f["secondary/test.toml"] = file(strings.Replace(secondaryTOML,
					`name = "Picking"`, "name = \"Picking\"\ntool_name = \"trowel\"", 1))
			},
			wantErr: "needs no tool class",
		},
		{
			name: "a tool for a skill that does not exist",
			mutate: func(f fstest.MapFS) {
				f["items/tools.toml"] = file(strings.Replace(toolTOML,
					`skill = "chopping"`, `skill = "no_such_skill"`, 1))
			},
			wantErr: "tool for unknown secondary skill",
		},
		{
			name: "a tool of the wrong class for its skill",
			mutate: func(f fstest.MapFS) {
				f["items/tools.toml"] = file(strings.Replace(toolTOML,
					`class = "axe"`, `class = "sword"`, 1))
			},
			wantErr: `needs class "axe"`,
		},
		{
			name: "a tool that cannot be equipped",
			mutate: func(f fstest.MapFS) {
				f["items/tools.toml"] = file(strings.Replace(toolTOML,
					`kind = "equipment"
slot = "weapon"`, `kind = "material"`, 1))
			},
			wantErr: "could never be in hand",
		},
		{
			name: "a tool with no power",
			mutate: func(f fstest.MapFS) {
				f["items/tools.toml"] = file(strings.Replace(toolTOML,
					"power = 2", "power = 0", 1))
			},
			wantErr: "the same as not being one",
		},
		{
			name: "a map places a node that does not exist",
			mutate: func(f fstest.MapFS) {
				f["maps/test.tmj"] = file(strings.Replace(mapTMJ,
					`{"id": 4, "class": "mob_spawn", "name": "boss"`,
					`{"id": 8, "class": "resource_node", "name": "tree", "x": 300, "y": 288,
       "properties": [{"name": "node_id", "type": "string", "value": "no_such_node"}]},
      {"id": 4, "class": "mob_spawn", "name": "boss"`, 1))
			},
			wantErr: "unknown resource node",
		},
		{
			name: "a resource node with no node_id",
			mutate: func(f fstest.MapFS) {
				f["maps/test.tmj"] = file(strings.Replace(mapTMJ,
					`{"id": 4, "class": "mob_spawn", "name": "boss"`,
					`{"id": 8, "class": "resource_node", "name": "tree", "x": 300, "y": 288},
      {"id": 4, "class": "mob_spawn", "name": "boss"`, 1))
			},
			wantErr: "has no node_id",
		},
		{
			name: "a resource node with an unknown layer",
			mutate: func(f fstest.MapFS) {
				f["maps/test.tmj"] = file(strings.Replace(mapTMJ,
					`{"id": 4, "class": "mob_spawn", "name": "boss"`,
					`{"id": 8, "class": "resource_node", "name": "tree", "x": 300, "y": 288,
       "properties": [
         {"name": "node_id", "type": "string", "value": "oak"},
         {"name": "layer", "type": "string", "value": "everyone"}
       ]},
      {"id": 4, "class": "mob_spawn", "name": "boss"`, 1))
			},
			wantErr: "want owner or shared",
		},
		{
			name: "two objects share an id",
			mutate: func(f fstest.MapFS) {
				// Exactly the mistake hand-editing a TMJ produces: an object
				// copied to add another one keeps the id it was copied from.
				f["maps/test.tmj"] = file(strings.Replace(mapTMJ,
					`{"id": 4, "class": "mob_spawn", "name": "boss"`,
					`{"id": 3, "class": "resource_node", "name": "tree", "x": 300, "y": 288,
       "properties": [{"name": "node_id", "type": "string", "value": "oak"}]},
      {"id": 4, "class": "mob_spawn", "name": "boss"`, 1))
			},
			wantErr: "is used twice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := secondaryFS()
			before := snapshotFS(f)
			tt.mutate(f)

			// A strings.Replace whose anchor has drifted is a no-op, and a
			// no-op mutation makes the whole subtest vacuous while still
			// looking like it passed. This caught exactly that while the table
			// was being written.
			if changed := snapshotFS(f); changed == before {
				t.Fatal("the mutation changed nothing; its anchor no longer matches the fixture")
			}

			_, err := Load(f)
			if err == nil {
				t.Fatal("expected a load error, got none")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// snapshotFS renders a fixture filesystem to a comparable string, so a test can
// tell whether a mutation actually mutated anything.
func snapshotFS(f fstest.MapFS) string {
	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(0)
		b.Write(f[name].Data)
		b.WriteByte(0)
	}
	return b.String()
}
