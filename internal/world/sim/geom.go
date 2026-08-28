package sim

import "github.com/ctrl-research/mmo/internal/fixed"

// Vec is a 2D point or vector in world units.
//
// The coordinate system is y-down: +X is right, +Y is down. This matches
// screen space, Tiled's map format, and PixiJS, so no axis flip is needed
// anywhere between the map file, the simulation, and the renderer. Gravity is
// therefore a positive Y acceleration.
type Vec struct {
	X, Y fixed.F
}

// Add returns v+o.
func (v Vec) Add(o Vec) Vec { return Vec{v.X + o.X, v.Y + o.Y} }

// Sub returns v-o.
func (v Vec) Sub(o Vec) Vec { return Vec{v.X - o.X, v.Y - o.Y} }

// Rect is an axis-aligned bounding box anchored at its top-left corner.
type Rect struct {
	X, Y, W, H fixed.F
}

// RectFromInts builds a Rect from whole world units, the form map data uses.
func RectFromInts(x, y, w, h int) Rect {
	return Rect{fixed.FromInt(x), fixed.FromInt(y), fixed.FromInt(w), fixed.FromInt(h)}
}

func (r Rect) Left() fixed.F   { return r.X }
func (r Rect) Right() fixed.F  { return r.X + r.W }
func (r Rect) Top() fixed.F    { return r.Y }
func (r Rect) Bottom() fixed.F { return r.Y + r.H }

// CenterX returns the horizontal midpoint, used to snap a climbing body to a
// rope or ladder.
func (r Rect) CenterX() fixed.F { return r.X + r.W/2 }

// Overlaps reports whether two rects share any interior area.
//
// The comparison is strict, so rects that merely touch edges do not overlap.
// This matters: a body resting exactly on a platform must not be considered
// intersecting it, or collision resolution oscillates every tick.
func (r Rect) Overlaps(o Rect) bool {
	return r.Left() < o.Right() && r.Right() > o.Left() &&
		r.Top() < o.Bottom() && r.Bottom() > o.Top()
}

// OverlapsInclusive is Overlaps but counts touching edges as overlapping. It
// is the right test for trigger volumes such as climbables, where standing at
// the exact edge of a rope should still let you grab it.
func (r Rect) OverlapsInclusive(o Rect) bool {
	return r.Left() <= o.Right() && r.Right() >= o.Left() &&
		r.Top() <= o.Bottom() && r.Bottom() >= o.Top()
}

// Contains reports whether p lies inside r, edges included.
func (r Rect) Contains(p Vec) bool {
	return p.X >= r.Left() && p.X <= r.Right() && p.Y >= r.Top() && p.Y <= r.Bottom()
}
