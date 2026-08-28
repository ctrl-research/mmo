package content

import (
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
)

// Item is a base item type.
//
// M1 needs only enough to put something on the ground and pick it up. The
// affix pools, tiers, rarity rolls, and equipment slots that make items
// interesting arrive in M3; this struct is expected to grow, and the fields
// here keep their meaning when it does.
type Item struct {
	ID        string
	Name      string
	Kind      string // consumable | equipment | material | currency
	Stackable bool
	MaxStack  int

	// Level gates who can use the item and, from M3, which affixes it can roll.
	Level int
}

type itemsFile struct {
	Item map[string]struct {
		Name      string `toml:"name"`
		Kind      string `toml:"kind"`
		Stackable bool   `toml:"stackable"`
		MaxStack  int    `toml:"max_stack"`
		Level     int    `toml:"level"`
	} `toml:"item"`
}

var validItemKinds = map[string]bool{
	"consumable": true,
	"equipment":  true,
	"material":   true,
	"currency":   true,
}

func (c *Content) loadItems(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "items", ".toml")
	if err != nil {
		return err
	}

	for _, name := range files {
		data, err := rec.readAndRecord(name)
		if err != nil {
			return err
		}

		var f itemsFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Item {
			if _, dup := c.Items[id]; dup {
				return fmt.Errorf("%s: item %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: item %q has no name", name, id)
			}
			if !validItemKinds[raw.Kind] {
				return fmt.Errorf("%s: item %q has unknown kind %q", name, id, raw.Kind)
			}

			maxStack := raw.MaxStack
			if !raw.Stackable {
				maxStack = 1
			} else if maxStack < 1 {
				return fmt.Errorf("%s: item %q is stackable but has max_stack %d", name, id, maxStack)
			}

			c.Items[id] = &Item{
				ID:        id,
				Name:      raw.Name,
				Kind:      raw.Kind,
				Stackable: raw.Stackable,
				MaxStack:  maxStack,
				Level:     raw.Level,
			}
		}
	}
	return nil
}
