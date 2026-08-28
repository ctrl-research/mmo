package content

import (
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
)

// DropTable decides what a mob leaves behind.
//
// Probabilities are stored as parts-per-million rather than the floats they
// are authored with, so every roll is integer arithmetic and reproduces
// exactly in a replay.
type DropTable struct {
	ID string

	GoldMin    int
	GoldMax    int
	GoldChance int // parts-per-million

	Entries []DropEntry
}

// DropEntry is one possible drop, rolled independently of the others.
//
// Independent rolls rather than one weighted pick, because a mob dropping two
// different things at once is normal and a weighted pick would make it
// impossible.
type DropEntry struct {
	Item   string
	Chance int // parts-per-million
	QtyMin int
	QtyMax int

	// Announce broadcasts the drop to the whole server. Reserved for genuinely
	// rare items; it is noise on anything common.
	Announce bool
}

type dropsFile struct {
	DropTable map[string]struct {
		Gold *struct {
			Min    int     `toml:"min"`
			Max    int     `toml:"max"`
			Chance float64 `toml:"chance"`
		} `toml:"gold"`

		Entries []struct {
			Item     string  `toml:"item"`
			Chance   float64 `toml:"chance"`
			Announce bool    `toml:"announce"`
			Qty      *struct {
				Min int `toml:"min"`
				Max int `toml:"max"`
			} `toml:"qty"`
		} `toml:"entries"`
	} `toml:"drop_table"`
}

func (c *Content) loadDrops(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "droptables", ".toml")
	if err != nil {
		return err
	}

	for _, name := range files {
		data, err := rec.readAndRecord(name)
		if err != nil {
			return err
		}

		var f dropsFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.DropTable {
			if _, dup := c.Drops[id]; dup {
				return fmt.Errorf("%s: drop table %q is defined twice", name, id)
			}

			t := &DropTable{ID: id}

			if raw.Gold != nil {
				if raw.Gold.Min < 0 || raw.Gold.Max < raw.Gold.Min {
					return fmt.Errorf("%s: drop table %q has an invalid gold range %d-%d",
						name, id, raw.Gold.Min, raw.Gold.Max)
				}
				t.GoldMin = raw.Gold.Min
				t.GoldMax = raw.Gold.Max
				t.GoldChance = ratioToPPM(raw.Gold.Chance)
			}

			for i, e := range raw.Entries {
				if e.Item == "" {
					return fmt.Errorf("%s: drop table %q entry %d names no item", name, id, i)
				}
				// A zero chance is almost always a forgotten field rather than
				// a deliberate never-drops entry, and it is invisible in play.
				if e.Chance <= 0 {
					return fmt.Errorf("%s: drop table %q entry %d (%s) has chance %v; it could never drop",
						name, id, i, e.Item, e.Chance)
				}

				entry := DropEntry{
					Item:     e.Item,
					Chance:   ratioToPPM(e.Chance),
					Announce: e.Announce,
					QtyMin:   1,
					QtyMax:   1,
				}
				if e.Qty != nil {
					if e.Qty.Min < 1 || e.Qty.Max < e.Qty.Min {
						return fmt.Errorf("%s: drop table %q entry %d has an invalid quantity range %d-%d",
							name, id, i, e.Qty.Min, e.Qty.Max)
					}
					entry.QtyMin, entry.QtyMax = e.Qty.Min, e.Qty.Max
				}
				t.Entries = append(t.Entries, entry)
			}

			c.Drops[id] = t
		}
	}
	return nil
}
