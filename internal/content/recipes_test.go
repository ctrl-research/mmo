package content

import (
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// The recipe loader's job beyond parsing is refusing content that loads cleanly
// and then does nothing in play: a recipe naming a renamed bar is an anvil a
// player stands at with the right materials and gets nothing from.

const stationsTOML = `
[station.forge]
name = "Forge"
`

const recipesTOML = `
[recipe.bar]
name = "Copper Bar"
skill = "chopping"
station = "forge"
level = 1
exp = 30
output = "test.plank"
qty = 1
action_ticks = 3

[[recipe.bar.input]]
item = "test.item"
qty = 2
`

const recipeItemsTOML = `
[item."test.plank"]
name = "Plank"
kind = "material"
stackable = true
max_stack = 100
level = 1

[item."test.chair"]
name = "Chair"
kind = "equipment"
slot = "weapon"
class = "sword"
level = 1
`

func recipeFS() fstest.MapFS {
	f := secondaryFS()
	f["stations/test.toml"] = file(stationsTOML)
	f["recipes/test.toml"] = file(recipesTOML)
	f["items/planks.toml"] = file(recipeItemsTOML)
	return f
}

func TestRecipeContentLoads(t *testing.T) {
	c, err := Load(recipeFS())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := len(c.Stations); got != 1 {
		t.Fatalf("loaded %d stations, want 1", got)
	}
	rec, ok := c.Recipes["bar"]
	if !ok {
		t.Fatal("the bar recipe did not load")
	}
	if rec.ActionTicks != 3 {
		t.Errorf("action_ticks = %d, want 3", rec.ActionTicks)
	}
	if len(rec.Inputs) != 1 || rec.Inputs[0].Item != "test.item" || rec.Inputs[0].Qty != 2 {
		t.Errorf("inputs = %+v, want 2 of test.item", rec.Inputs)
	}

	at := c.RecipesAt("forge")
	if len(at) != 1 || at[0].ID != "bar" {
		t.Errorf("RecipesAt(forge) = %v, want the bar recipe", ids(at))
	}
	if got := c.RecipesAt("no_such_station"); got != nil {
		t.Errorf("RecipesAt for an unknown station = %v, want nothing", ids(got))
	}
}

// A station's recipes come back in a stable order, so a menu does not shuffle
// between openings.
func TestRecipesAtIsStablyOrdered(t *testing.T) {
	f := recipeFS()
	f["recipes/test.toml"] = file(recipesTOML + `
[recipe.aaa_first]
name = "First"
skill = "chopping"
station = "forge"
level = 1
exp = 10
output = "test.plank"
qty = 1
action_ticks = 1

[[recipe.aaa_first.input]]
item = "test.item"
qty = 1
`)
	c, err := Load(f)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	first := ids(c.RecipesAt("forge"))
	if !sort.StringsAreSorted(first) {
		t.Errorf("RecipesAt = %v, want it sorted", first)
	}
	for i := 0; i < 5; i++ {
		if got := ids(c.RecipesAt("forge")); !equal(got, first) {
			t.Fatalf("RecipesAt gave %v then %v; the order is not stable", first, got)
		}
	}
}

func TestBrokenRecipeContentIsRejected(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(fstest.MapFS)
		wantErr string
	}{
		{
			name: "a recipe raises a skill that does not exist",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Replace(recipesTOML,
					`skill = "chopping"`, `skill = "no_such_skill"`, 1))
			},
			wantErr: "unknown secondary skill",
		},
		{
			name: "a recipe is made at a station that does not exist",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Replace(recipesTOML,
					`station = "forge"`, `station = "no_such_station"`, 1))
			},
			wantErr: "unknown station",
		},
		{
			name: "a recipe produces an item that does not exist",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Replace(recipesTOML,
					`output = "test.plank"`, `output = "test.ghost"`, 1))
			},
			wantErr: "produces unknown item",
		},
		{
			name: "a recipe consumes an item that does not exist",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Replace(recipesTOML,
					`item = "test.item"`, `item = "test.ghost"`, 1))
			},
			wantErr: "consumes unknown item",
		},
		{
			name: "a recipe consumes nothing",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Split(recipesTOML, "[[recipe.bar.input]]")[0])
			},
			wantErr: "is an item printer",
		},
		{
			name: "a recipe consumes what it produces",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Replace(recipesTOML,
					`item = "test.item"`, `item = "test.plank"`, 1))
			},
			wantErr: "a recipe for nothing",
		},
		{
			name: "an input with no quantity",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Replace(recipesTOML,
					"qty = 2", "qty = 0", 1))
			},
			wantErr: "is not an input",
		},
		{
			name: "a recipe takes no time",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Replace(recipesTOML,
					"action_ticks = 3", "action_ticks = 0", 1))
			},
			wantErr: "takes no time",
		},
		{
			name: "a recipe grants no experience",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Replace(recipesTOML,
					"exp = 30", "exp = 0", 1))
			},
			wantErr: "grants no experience",
		},
		{
			name: "a recipe needs a level nobody can reach",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Replace(recipesTOML,
					"level = 1", "level = 200", 1))
			},
			wantErr: "above the maximum",
		},
		{
			name: "a recipe makes several of something unstackable",
			mutate: func(f fstest.MapFS) {
				f["recipes/test.toml"] = file(strings.Replace(strings.Replace(recipesTOML,
					`output = "test.plank"`, `output = "test.chair"`, 1),
					"qty = 1", "qty = 3", 1))
			},
			wantErr: "does not stack",
		},
		{
			name: "a station has no recipes",
			mutate: func(f fstest.MapFS) {
				f["stations/test.toml"] = file(stationsTOML + `
[station.orphan]
name = "Orphan Bench"
`)
			},
			wantErr: `station "orphan" has no recipes`,
		},
		{
			name: "a station with no name",
			mutate: func(f fstest.MapFS) {
				f["stations/test.toml"] = file(strings.Replace(stationsTOML,
					`name = "Forge"`, `name = ""`, 1))
			},
			wantErr: "has no name",
		},
		{
			name: "a map places a station that does not exist",
			mutate: func(f fstest.MapFS) {
				f["maps/test.tmj"] = file(strings.Replace(mapTMJ,
					`{"id": 4, "class": "mob_spawn", "name": "boss"`,
					`{"id": 9, "class": "station", "name": "anvil", "x": 300, "y": 288,
       "width": 48, "height": 48,
       "properties": [{"name": "station_id", "type": "string", "value": "no_such_station"}]},
      {"id": 4, "class": "mob_spawn", "name": "boss"`, 1))
			},
			wantErr: "unknown station",
		},
		{
			name: "a station object with no station_id",
			mutate: func(f fstest.MapFS) {
				f["maps/test.tmj"] = file(strings.Replace(mapTMJ,
					`{"id": 4, "class": "mob_spawn", "name": "boss"`,
					`{"id": 9, "class": "station", "name": "anvil", "x": 300, "y": 288,
       "width": 48, "height": 48},
      {"id": 4, "class": "mob_spawn", "name": "boss"`, 1))
			},
			wantErr: "has no station_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := recipeFS()
			before := snapshotFS(f)
			tt.mutate(f)

			// The same guard the secondary table has: a strings.Replace whose
			// anchor has drifted is a no-op, and a no-op mutation makes the
			// subtest vacuous while still looking like it passed.
			if snapshotFS(f) == before {
				t.Fatal("the mutation changed nothing; its anchor no longer matches the fixture")
			}

			if _, err := Load(f); err == nil {
				t.Fatal("expected a load error, got none")
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func ids(recipes []*Recipe) []string {
	out := make([]string, 0, len(recipes))
	for _, r := range recipes {
		out = append(out, r.ID)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
