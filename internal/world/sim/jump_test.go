package sim

import (
	"testing"

	"github.com/ctrl-research/mmo/internal/fixed"
)

// The jump is sized against the maps, so the maps' standard gap is what it has
// to be tested against.
//
// This is not a hypothetical. The jump was tuned to a comment claiming "a
// roughly 96-unit jump" while actually clearing 84, and every platform in
// every map sits 96 above the surface below it -- so the tutorial's only exit
// was unreachable and had been since M1. Nothing failed, because nothing
// compared the two numbers.

// StandardGap is the vertical spacing the maps are built on: three 32-unit
// tiles.
const StandardGap = 96

func TestMaxJumpHeightMatchesTheSimulation(t *testing.T) {
	w := &World{
		Bounds: Rect{W: fixed.FromInt(2000), H: fixed.FromInt(2000)},
		Solids: []Rect{{Y: fixed.FromInt(608), W: fixed.FromInt(2000), H: fixed.FromInt(32)}},
	}
	tuning := DefaultTuning()

	b := NewBody(Vec{X: fixed.FromInt(100), Y: fixed.FromInt(608)}, PlayerSize.W, PlayerSize.H)
	Settle(&b, w, &tuning)

	start, highest := b.Pos.Y, b.Pos.Y
	for i := 0; i < 200; i++ {
		Step(&b, Input{Jump: true}, w, &tuning)
		if b.Pos.Y < highest {
			highest = b.Pos.Y
		}
		if i > 2 && b.Grounded {
			break
		}
	}

	measured := start - highest
	if got := MaxJumpHeight(&tuning); got != measured {
		t.Errorf("MaxJumpHeight says %v, the simulation reaches %v; content "+
			"validation checks maps against the closed form, so a disagreement "+
			"means maps are being checked against a jump nobody can make",
			got, measured)
	}
}

func TestAHeldJumpClearsTheStandardGap(t *testing.T) {
	tuning := DefaultTuning()

	gap := fixed.FromInt(StandardGap)
	height := MaxJumpHeight(&tuning)

	if height < gap {
		t.Fatalf("a held jump reaches %v and the maps are built on %v gaps; "+
			"nothing in the world is reachable", height, gap)
	}

	// Margin, not just clearance. Landing on a ledge means being above it with
	// somewhere still to go, and a jump that reaches exactly the ledge top
	// lands only from a pixel-perfect launch.
	if margin := height - gap; margin < fixed.FromInt(32) {
		t.Errorf("a held jump clears the standard gap by only %v; that is a "+
			"tile or less of margin and reads as the controls failing", margin)
	}
}

// The jumps that actually run out are diagonal. The tutorial's low ledges sit
// 96 across from the ledge above them as well as 96 below, so height alone
// does not make the map traversable -- there has to be air time left at that
// height to cross the gap.
func TestARunningJumpCrossesTheStandardGapDiagonally(t *testing.T) {
	w := &World{
		Bounds: Rect{W: fixed.FromInt(4000), H: fixed.FromInt(2000)},
		Solids: []Rect{{Y: fixed.FromInt(608), W: fixed.FromInt(4000), H: fixed.FromInt(32)}},
	}
	tuning := DefaultTuning()

	b := NewBody(Vec{X: fixed.FromInt(100), Y: fixed.FromInt(608)}, PlayerSize.W, PlayerSize.H)
	Settle(&b, w, &tuning)

	// Up to speed first: this is a running jump, not a standing one.
	for i := 0; i < 40; i++ {
		Step(&b, Input{MoveX: 1000}, w, &tuning)
	}

	start := b.Pos
	gap := fixed.FromInt(StandardGap)
	var reach fixed.F

	for i := 0; i < 200; i++ {
		Step(&b, Input{MoveX: 1000, Jump: true}, w, &tuning)
		if start.Y-b.Pos.Y >= gap {
			reach = b.Pos.X - start.X
		}
		if i > 2 && b.Grounded {
			break
		}
	}

	if reach < gap {
		t.Errorf("a running jump covers %v horizontally while %v above the "+
			"ground; the maps ask for %v, so their diagonal ledges cannot be "+
			"reached", reach, gap, gap)
	}
}
