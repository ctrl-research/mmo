package content

import (
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// EquipSlot is where an item is worn. Empty for things that are not equipment.
type EquipSlot string

const (
	SlotWeapon EquipSlot = "weapon"
	SlotHelmet EquipSlot = "helmet"
	SlotChest  EquipSlot = "chest"
	SlotGloves EquipSlot = "gloves"
	SlotBoots  EquipSlot = "boots"
	SlotRing   EquipSlot = "ring"
)

// EquipSlots lists every slot, in the order a paperdoll shows them.
var EquipSlots = []EquipSlot{
	SlotWeapon, SlotHelmet, SlotChest, SlotGloves, SlotBoots, SlotRing,
}

var validSlots = func() map[EquipSlot]bool {
	m := make(map[EquipSlot]bool, len(EquipSlots))
	for _, s := range EquipSlots {
		m[s] = true
	}
	return m
}()

// ImplicitMod is a modifier every instance of a base type carries.
//
// Rolled per instance like an affix, so two iron swords are not identical --
// which is what makes a base type worth re-examining rather than a solved
// quantity.
type ImplicitMod struct {
	Stat stats.StatID
	Kind stats.Kind
	Min  stats.Value
	Max  stats.Value
}

// Item is a base item type.
type Item struct {
	ID        string
	Name      string
	Kind      string // consumable | equipment | material | currency
	Stackable bool
	MaxStack  int

	// Level gates who can use the item and which affix tiers it can roll.
	Level int

	// Slot is where equipment is worn. Empty for everything else.
	Slot EquipSlot

	// Class decides which affixes may roll on it: "sword", "body_armour".
	// Separate from Slot because two slots can share an affix pool and one
	// slot can hold several classes.
	Class string

	// Implicits are rolled on every instance.
	Implicits []ImplicitMod

	// Tool is what this item does for a secondary skill, and nil for the
	// overwhelming majority of items that are not tools. See secondary.go.
	Tool *Tool
}

// IsEquipment reports whether the item can be worn.
func (i *Item) IsEquipment() bool { return i.Slot != "" }

type itemsFile struct {
	Item map[string]struct {
		Name      string `toml:"name"`
		Kind      string `toml:"kind"`
		Stackable bool   `toml:"stackable"`
		MaxStack  int    `toml:"max_stack"`
		Level     int    `toml:"level"`
		Slot      string `toml:"slot"`
		Class     string `toml:"class"`

		Implicit []struct {
			Stat string  `toml:"stat"`
			Kind string  `toml:"kind"`
			Min  float64 `toml:"min"`
			Max  float64 `toml:"max"`
		} `toml:"implicit"`

		Tool *struct {
			Skill string `toml:"skill"`
			Power int    `toml:"power"`
		} `toml:"tool"`
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

			item := &Item{
				ID:        id,
				Name:      raw.Name,
				Kind:      raw.Kind,
				Stackable: raw.Stackable,
				MaxStack:  maxStack,
				Level:     raw.Level,
				Slot:      EquipSlot(raw.Slot),
				Class:     raw.Class,
			}

			if item.Slot != "" && !validSlots[item.Slot] {
				return fmt.Errorf("%s: item %q has unknown slot %q", name, id, raw.Slot)
			}
			// Equipment that is stackable would have to share one set of
			// rolled affixes across the stack, which is incoherent.
			if item.IsEquipment() && item.Stackable {
				return fmt.Errorf("%s: item %q is equipment and stackable; "+
					"rolled affixes cannot be shared across a stack", name, id)
			}
			if item.Kind == "equipment" && item.Slot == "" {
				return fmt.Errorf("%s: item %q is equipment but names no slot", name, id)
			}

			if raw.Tool != nil {
				if raw.Tool.Skill == "" {
					return fmt.Errorf("%s: item %q is a tool for no skill", name, id)
				}
				if raw.Tool.Power <= 0 {
					return fmt.Errorf("%s: item %q is a tool with no power, which is the same as not being one",
						name, id)
				}
				// A tool that cannot be held is a tool that can never be used:
				// the check for one reads the equipment slots.
				if !item.IsEquipment() {
					return fmt.Errorf("%s: item %q is a tool but is not equipment, so it could never be in hand",
						name, id)
				}
				item.Tool = &Tool{Skill: raw.Tool.Skill, Power: raw.Tool.Power}
			}

			for i, im := range raw.Implicit {
				stat, ok := stats.Parse(im.Stat)
				if !ok {
					return fmt.Errorf("%s: item %q implicit %d references unknown stat %q",
						name, id, i, im.Stat)
				}
				kind, ok := stats.ParseKind(im.Kind)
				if !ok {
					return fmt.Errorf("%s: item %q implicit %d has modifier kind %q",
						name, id, i, im.Kind)
				}
				if im.Max < im.Min {
					return fmt.Errorf("%s: item %q implicit %d has max below min", name, id, i)
				}
				item.Implicits = append(item.Implicits, ImplicitMod{
					Stat: stat, Kind: kind,
					Min: stats.FromFloat(im.Min), Max: stats.FromFloat(im.Max),
				})
			}

			c.Items[id] = item
		}
	}
	return nil
}
