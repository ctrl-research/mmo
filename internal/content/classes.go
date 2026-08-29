package content

import (
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
)

// Classes.
//
// A class is deliberately thin: a name, where it starts on the passive tree,
// and what it can cast. It is not a package of hard-coded mechanics, because
// the mechanics are skills and passives, and those are data.
//
// That thinness is what makes "two characters of the same class play
// differently" possible at all. If a class carried its own behaviour, the
// class would be the build.

// Class is one playable archetype.
type Class struct {
	ID   string
	Name string

	// Description is what a player reads when choosing. One sentence: the
	// choice is made on feel, and a paragraph nobody reads is a paragraph
	// that misleads.
	Description string

	// StartingSkills are granted at level one, in bar order. Every class needs
	// at least one, or a new character cannot do anything.
	StartingSkills []string

	// TreeStart is the passive tree node this class begins at. Empty until the
	// tree exists; validated against it once it does.
	TreeStart int

	// PrimaryStat is the stat this class scales with, which decides what its
	// gear wants and is most of what distinguishes two classes with similar
	// skills.
	PrimaryStat string
}

type classesFile struct {
	Class map[string]struct {
		Name           string   `toml:"name"`
		Description    string   `toml:"description"`
		StartingSkills []string `toml:"starting_skills"`
		TreeStart      int      `toml:"tree_start"`
		PrimaryStat    string   `toml:"primary_stat"`
	} `toml:"class"`
}

func (c *Content) loadClasses(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "classes", ".toml")
	if err != nil {
		return err
	}

	for _, name := range files {
		data, err := rec.readAndRecord(name)
		if err != nil {
			return err
		}

		var f classesFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Class {
			if _, dup := c.Classes[id]; dup {
				return fmt.Errorf("%s: class %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: class %q has no name", name, id)
			}
			if len(raw.StartingSkills) == 0 {
				return fmt.Errorf("%s: class %q has no starting skills, so a new "+
					"character of it could do nothing at all", name, id)
			}

			c.Classes[id] = &Class{
				ID:             id,
				Name:           raw.Name,
				Description:    raw.Description,
				StartingSkills: raw.StartingSkills,
				TreeStart:      raw.TreeStart,
				PrimaryStat:    raw.PrimaryStat,
			}
		}
	}
	return nil
}

// SkillsFor returns the skills a class may learn.
//
// By the skill's own class field rather than a list on the class, so adding a
// skill to a class is one line in the skill rather than two files that can
// disagree.
func (c *Content) SkillsFor(classID string) []*Skill {
	var out []*Skill
	for _, s := range c.Skills {
		if s.Class == classID {
			out = append(out, s)
		}
	}
	sortSkills(out)
	return out
}

func sortSkills(list []*Skill) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].ID < list[j-1].ID; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
