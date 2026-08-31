package content

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
)

// Processing: turning what was gathered into something worth having.
//
// The other half of the secondary skills. Gathering answers "what is there to
// do in this zone"; processing answers "why did I pick any of that up", and it
// is the half that connects the OSRS side of the game to the Path of Exile
// side -- a smith who can make a sword is a smith whose evening of mining fed
// their build.
//
// It runs on the same 600 ms action tick as gathering, for the same reason: one
// clock in the room, and a beat a player can feel.

// Station is a fixture a player must stand at to process.
//
// A separate definition rather than a bare string on the recipe, so that a
// typo in a map is a load error rather than an anvil nobody can use. Stations
// have no timers and no population -- they are scenery that does something,
// which is what makes them cheaper than resource nodes.
type Station struct {
	ID   string
	Name string
}

// Recipe is one thing a station can make.
type Recipe struct {
	ID   string
	Name string

	// Skill is which secondary skill it raises, and Station where it is done.
	Skill   string
	Station string

	// Level is the skill level required.
	Level int

	// Exp is granted per output produced.
	Exp int64

	// Inputs are consumed per output. Order is the authored order, so a
	// refusal can name the first thing that ran out rather than an arbitrary
	// one.
	Inputs []RecipeInput

	// Output is what one run produces.
	Output string
	Qty    int

	// ActionTicks is how many action ticks one run takes.
	//
	// In action ticks rather than milliseconds because that is the unit it is
	// actually measured in -- a run that took 800 ms would be rounded to two
	// beats anyway, and authoring the rounded number is authoring what happens.
	ActionTicks int
}

// RecipeInput is one requirement.
type RecipeInput struct {
	Item string
	Qty  int
}

type stationsFile struct {
	Station map[string]struct {
		Name string `toml:"name"`
	} `toml:"station"`
}

type recipesFile struct {
	Recipe map[string]struct {
		Name    string `toml:"name"`
		Skill   string `toml:"skill"`
		Station string `toml:"station"`
		Level   int    `toml:"level"`
		Exp     int64  `toml:"exp"`
		Output  string `toml:"output"`
		Qty     int    `toml:"qty"`
		Ticks   int    `toml:"action_ticks"`

		Input []struct {
			Item string `toml:"item"`
			Qty  int    `toml:"qty"`
		} `toml:"input"`
	} `toml:"recipe"`
}

func (c *Content) loadStations(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "stations", ".toml")
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

		var f stationsFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Station {
			if _, dup := c.Stations[id]; dup {
				return fmt.Errorf("%s: station %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: station %q has no name", name, id)
			}
			c.Stations[id] = &Station{ID: id, Name: raw.Name}
		}
	}
	return nil
}

func (c *Content) loadRecipes(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "recipes", ".toml")
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

		var f recipesFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Recipe {
			if _, dup := c.Recipes[id]; dup {
				return fmt.Errorf("%s: recipe %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: recipe %q has no name", name, id)
			}
			if raw.Output == "" {
				return fmt.Errorf("%s: recipe %q produces nothing", name, id)
			}
			if len(raw.Input) == 0 {
				return fmt.Errorf("%s: recipe %q consumes nothing; a recipe that takes no materials is an item printer",
					name, id)
			}
			if raw.Exp <= 0 {
				return fmt.Errorf("%s: recipe %q grants no experience, so making it would never raise %s",
					name, id, raw.Skill)
			}
			if raw.Ticks <= 0 {
				return fmt.Errorf("%s: recipe %q takes no time; every recipe costs at least one action tick, or a full bag becomes a full bag of output in one",
					name, id)
			}

			r := &Recipe{
				ID:          id,
				Name:        raw.Name,
				Skill:       raw.Skill,
				Station:     raw.Station,
				Level:       max(raw.Level, 1),
				Exp:         raw.Exp,
				Output:      raw.Output,
				Qty:         max(raw.Qty, 1),
				ActionTicks: raw.Ticks,
			}

			for i, in := range raw.Input {
				if in.Item == "" {
					return fmt.Errorf("%s: recipe %q input %d names no item", name, id, i)
				}
				if in.Qty < 1 {
					return fmt.Errorf("%s: recipe %q needs %d of %q; an input with no quantity is not an input",
						name, id, in.Qty, in.Item)
				}
				if in.Item == raw.Output {
					return fmt.Errorf("%s: recipe %q consumes and produces %q, which is a recipe for nothing",
						name, id, in.Item)
				}
				r.Inputs = append(r.Inputs, RecipeInput{Item: in.Item, Qty: in.Qty})
			}

			c.Recipes[id] = r
		}
	}
	return nil
}

// validateRecipes checks every recipe against the rest of the graph.
//
// All of these load cleanly and then fail in play, which is the whole reason
// the loader is strict: a recipe naming a renamed bar is an anvil a player
// stands at with the right materials and gets nothing from.
func (c *Content) validateRecipes() error {
	for id, r := range c.Recipes {
		if _, ok := c.Secondary[r.Skill]; !ok {
			return fmt.Errorf("content: recipe %q raises unknown secondary skill %q", id, r.Skill)
		}
		if _, ok := c.Stations[r.Station]; !ok {
			return fmt.Errorf("content: recipe %q is made at unknown station %q", id, r.Station)
		}
		if _, ok := c.Items[r.Output]; !ok {
			return fmt.Errorf("content: recipe %q produces unknown item %q", id, r.Output)
		}
		for _, in := range r.Inputs {
			if _, ok := c.Items[in.Item]; !ok {
				return fmt.Errorf("content: recipe %q consumes unknown item %q", id, in.Item)
			}
		}
		if r.Level > c.Curves.MaxSkillLevel {
			return fmt.Errorf("content: recipe %q requires %s level %d, above the maximum of %d, so nobody could ever make it",
				id, r.Skill, r.Level, c.Curves.MaxSkillLevel)
		}

		// A recipe producing more than one of something unstackable would have
		// to invent slots for the rest. Refused rather than silently capped:
		// "qty = 3" on a sword is a designer who meant something else.
		out := c.Items[r.Output]
		if r.Qty > 1 && !out.Stackable {
			return fmt.Errorf("content: recipe %q makes %d of %q, which does not stack",
				id, r.Qty, r.Output)
		}
	}

	// Every station placed on a map must exist, and every station defined must
	// have at least one recipe -- an anvil nobody can make anything at is a
	// thing a player walks up to and learns nothing from.
	for mapID, m := range c.Maps {
		for _, spot := range m.Stations {
			if _, ok := c.Stations[spot.StationID]; !ok {
				return fmt.Errorf("content: map %q places unknown station %q", mapID, spot.StationID)
			}
		}
	}
	for id := range c.Stations {
		if len(c.RecipesAt(id)) == 0 {
			return fmt.Errorf("content: station %q has no recipes", id)
		}
	}
	return nil
}

// RecipesAt returns every recipe made at a station, in a stable order.
func (c *Content) RecipesAt(station string) []*Recipe {
	var out []*Recipe
	for _, id := range sortedKeys(c.Recipes) {
		if c.Recipes[id].Station == station {
			out = append(out, c.Recipes[id])
		}
	}
	return out
}
