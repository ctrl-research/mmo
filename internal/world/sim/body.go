package sim

import "github.com/ctrl-research/mmo/internal/fixed"

// Input is one tick of player intent.
//
// It carries what buttons are held, never a position or an outcome. The server
// derives everything else. This is what makes teleport and speed hacks
// unrepresentable rather than merely detected (see docs/protocol.md).
type Input struct {
	// MoveX is horizontal intent in thousandths, -1000 (full left) to +1000
	// (full right). Thousandths rather than a fixed.F so that a keyboard, an
	// analogue stick, and the wire format all agree on the same integer range.
	MoveX int32

	Jump bool
	Up   bool
	Down bool
}

// clampMoveX defends against a hostile or buggy client. The gateway clamps
// too; doing it here as well means the simulation is safe to call from
// anywhere, including tests and the WASM client build.
func (in Input) clampMoveX() int32 {
	if in.MoveX > 1000 {
		return 1000
	}
	if in.MoveX < -1000 {
		return -1000
	}
	return in.MoveX
}

// Body is the complete physical state of one entity.
//
// Every field is exported, and that is a deliberate requirement rather than an
// oversight. Client-side prediction works by snapping to the server's
// authoritative Body and replaying unacknowledged inputs through Step. Any
// state Step reads that is not part of Body — or is part of Body but is not
// transmitted — makes that replay diverge from the server, which surfaces as
// rubber-banding that is very hard to trace back to its cause.
//
// So: no hidden state, and no state that lives outside this struct.
type Body struct {
	// Pos is the top-left corner of the axis-aligned bounding box.
	Pos Vec

	// Vel is per tick, not per second.
	Vel Vec

	// W and H are the collision box size. Sprites are larger and are offset by
	// the renderer; only this box collides.
	W, H fixed.F

	Grounded   bool
	Climbing   bool
	FacingLeft bool

	// JumpHeld is the previous tick's jump button, needed to distinguish a
	// fresh press from a held one.
	JumpHeld bool

	// Coyote counts down the ticks after walking off a ledge during which a
	// jump still works.
	Coyote uint8

	// JumpBuffer counts down the ticks a jump press stays queued, so a press
	// slightly before landing still fires on touchdown.
	JumpBuffer uint8

	// DropThrough counts down the ticks during which one-way platforms are
	// ignored, letting a body fall through the floor it is standing on.
	DropThrough uint8
}

// Bounds returns the body's collision box in world space.
func (b *Body) Bounds() Rect { return Rect{b.Pos.X, b.Pos.Y, b.W, b.H} }

// FeetCenter is the bottom-centre point, which is the natural anchor for
// spawn points, sprites, and ground queries.
func (b *Body) FeetCenter() Vec { return Vec{b.Pos.X + b.W/2, b.Pos.Y + b.H} }

// SetFeetCenter positions the body so its bottom-centre sits at p. Spawn
// points are authored as feet positions, so this is how a body is placed.
func (b *Body) SetFeetCenter(p Vec) {
	b.Pos.X = p.X - b.W/2
	b.Pos.Y = p.Y - b.H
}

// NewBody returns a body of the given size with its feet at p and no motion.
func NewBody(p Vec, w, h fixed.F) Body {
	b := Body{W: w, H: h}
	b.SetFeetCenter(p)
	return b
}

// PlayerSize is the collision box of a player character: 24 wide, 48 tall,
// which is three quarters of a tile by one and a half tiles at 32-unit tiles.
var PlayerSize = struct{ W, H fixed.F }{fixed.FromInt(24), fixed.FromInt(48)}
