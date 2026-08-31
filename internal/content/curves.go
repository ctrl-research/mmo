package content

import (
	"fmt"
	"io/fs"
	"math"

	"github.com/BurntSushi/toml"
)

// Curves holds the progression tables.
//
// Both are generated from formula parameters at load rather than being typed
// out: a 200-entry table is not something anyone should hand-maintain, and the
// parameters are what a designer actually wants to adjust.
type Curves struct {
	// MainExp[L] is the total experience required to advance from level L to
	// L+1. Index 0 is unused so the table reads by level directly.
	MainExp  []int64
	MaxLevel int

	// SecondaryExp[L] is the cumulative experience for level L in a secondary
	// skill, using Old School RuneScape's curve.
	//
	// Cumulative, unlike MainExp above, which is per level. Secondary
	// experience is never spent: the level is derived from the total rather
	// than the total being decremented as levels are taken, which is OSRS's
	// arrangement and the reason a secondary skill can never lose a level to a
	// rounding change in the curve.
	SecondaryExp  []int64
	MaxSkillLevel int
}

type curvesFile struct {
	Main struct {
		MaxLevel int     `toml:"max_level"`
		Scale    float64 `toml:"scale"`
		Exponent float64 `toml:"exponent"`
		Growth   float64 `toml:"growth"`
	} `toml:"main"`

	Secondary struct {
		MaxLevel int `toml:"max_level"`
	} `toml:"secondary"`
}

func (c *Content) loadCurves(fsys fs.FS, rec *hashRecorder) error {
	data, err := rec.readAndRecord("curves/exp.toml")
	if err != nil {
		return err
	}

	var f curvesFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("curves/exp.toml: %w", err)
	}

	if f.Main.MaxLevel < 2 {
		return fmt.Errorf("curves: main max_level must be at least 2")
	}
	if f.Main.Exponent <= 0 || f.Main.Scale <= 0 {
		return fmt.Errorf("curves: main scale and exponent must be positive")
	}

	c.Curves.MaxLevel = f.Main.MaxLevel
	c.Curves.MainExp = make([]int64, f.Main.MaxLevel+1)

	// expToNext(L) = scale * L^exponent * (1 + growth)^L
	//
	// The polynomial term shapes the early game and the exponential term the
	// late game, which is what gives a MapleStory-like curve: a new character
	// reaches interesting skills quickly, and levels still feel earned at 150.
	for level := 1; level < f.Main.MaxLevel; level++ {
		v := f.Main.Scale *
			math.Pow(float64(level), f.Main.Exponent) *
			math.Pow(1+f.Main.Growth, float64(level))
		c.Curves.MainExp[level] = int64(math.Round(v))
	}

	// Generated in floating point at load, then stored as integers. Every
	// comparison the simulation makes against these values is integer, so
	// nothing downstream depends on float behaviour.
	if err := c.validateMonotonic(); err != nil {
		return err
	}

	skillMax := f.Secondary.MaxLevel
	if skillMax < 2 {
		skillMax = 99
	}
	c.Curves.MaxSkillLevel = skillMax
	c.Curves.SecondaryExp = osrsCurve(skillMax)

	return nil
}

// validateMonotonic catches a parameter set that makes a later level cheaper
// than an earlier one, which would let a player gain two levels from one kill
// and then lose one.
func (c *Content) validateMonotonic() error {
	for level := 2; level < c.Curves.MaxLevel; level++ {
		if c.Curves.MainExp[level] < c.Curves.MainExp[level-1] {
			return fmt.Errorf(
				"curves: level %d requires less experience (%d) than level %d (%d); "+
					"the curve must not decrease",
				level, c.Curves.MainExp[level], level-1, c.Curves.MainExp[level-1])
		}
	}
	return nil
}

// osrsCurve generates Old School RuneScape's experience table.
//
//	xp(L) = floor( (1/4) * sum[i=1..L-1]( floor( i + 300 * 2^(i/7) ) ) )
//
// Reproduced rather than invented because it is well tuned and instantly
// legible to anyone who has played the game the secondary skills borrow from.
//
// The inner floor is load-bearing and easy to miss: truncating each term
// before summing, rather than only at the end, is what produces the published
// table. Getting it wrong yields values a handful of xp high at every level --
// close enough to look right, wrong enough that anyone who knows the game
// notices immediately.
func osrsCurve(maxLevel int) []int64 {
	out := make([]int64, maxLevel+1)
	var total int64
	for level := 1; level < maxLevel; level++ {
		total += int64(math.Floor(float64(level) + 300*math.Pow(2, float64(level)/7)))
		out[level+1] = total / 4
	}
	return out
}

// ExpToNext returns the experience needed to advance from level to level+1,
// and whether level is a valid, non-maximum level.
func (c Curves) ExpToNext(level int) (int64, bool) {
	if level < 1 || level >= c.MaxLevel {
		return 0, false
	}
	return c.MainExp[level], true
}

// IsMaxLevel reports whether a character can gain no further levels.
func (c Curves) IsMaxLevel(level int) bool { return level >= c.MaxLevel }
