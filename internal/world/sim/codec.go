package sim

import (
	"encoding/binary"

	"github.com/ctrl-research/mmo/internal/fixed"
)

// Binary encoding for crossing the WebAssembly boundary.
//
// The client runs this same package compiled to WASM so its prediction matches
// the server exactly. Passing state as JavaScript objects would mean a
// conversion on every call, on the hot path, in the one place where a rounding
// difference is most expensive. Instead both sides agree on a fixed byte
// layout of little-endian integers, which is a copy rather than a conversion.
//
// The layout is versioned by BodySize: if a field is added, the size changes,
// and the TypeScript side fails loudly at startup rather than silently reading
// misaligned values.

// BodySize is the encoded size of a Body in bytes.
const BodySize = 28

// WorldHeaderSize is the encoded size of a World's header: three section
// counts plus the bounds rect.
const WorldHeaderSize = 12 + rectSize

const rectSize = 16

// Body flag bits in the encoded form.
const (
	encGrounded = 1 << 0
	encClimbing = 1 << 1
	encFacing   = 1 << 2
	encJumpHeld = 1 << 3
)

// MarshalBody writes b into dst, which must be at least BodySize long.
func MarshalBody(dst []byte, b *Body) {
	_ = dst[BodySize-1] // bounds check once

	le := binary.LittleEndian
	le.PutUint32(dst[0:], uint32(b.Pos.X))
	le.PutUint32(dst[4:], uint32(b.Pos.Y))
	le.PutUint32(dst[8:], uint32(b.Vel.X))
	le.PutUint32(dst[12:], uint32(b.Vel.Y))
	le.PutUint32(dst[16:], uint32(b.W))
	le.PutUint32(dst[20:], uint32(b.H))

	var flags uint8
	if b.Grounded {
		flags |= encGrounded
	}
	if b.Climbing {
		flags |= encClimbing
	}
	if b.FacingLeft {
		flags |= encFacing
	}
	if b.JumpHeld {
		flags |= encJumpHeld
	}
	dst[24] = flags
	dst[25] = b.Coyote
	dst[26] = b.JumpBuffer
	dst[27] = b.DropThrough
}

// UnmarshalBody reads a Body from src, which must be at least BodySize long.
func UnmarshalBody(src []byte, b *Body) {
	_ = src[BodySize-1]

	le := binary.LittleEndian
	b.Pos.X = fixedFrom(le.Uint32(src[0:]))
	b.Pos.Y = fixedFrom(le.Uint32(src[4:]))
	b.Vel.X = fixedFrom(le.Uint32(src[8:]))
	b.Vel.Y = fixedFrom(le.Uint32(src[12:]))
	b.W = fixedFrom(le.Uint32(src[16:]))
	b.H = fixedFrom(le.Uint32(src[20:]))

	flags := src[24]
	b.Grounded = flags&encGrounded != 0
	b.Climbing = flags&encClimbing != 0
	b.FacingLeft = flags&encFacing != 0
	b.JumpHeld = flags&encJumpHeld != 0

	b.Coyote = src[25]
	b.JumpBuffer = src[26]
	b.DropThrough = src[27]
}

// MarshalWorld encodes the collision geometry for upload to the client.
//
// Layout: three int32 counts, the bounds rect, then the solid, platform, and
// climbable rects in that order, each four int32 values.
func MarshalWorld(w *World) []byte {
	n := WorldHeaderSize + rectSize*(len(w.Solids)+len(w.Platforms)+len(w.Climbables))
	buf := make([]byte, n)

	le := binary.LittleEndian
	le.PutUint32(buf[0:], uint32(len(w.Solids)))
	le.PutUint32(buf[4:], uint32(len(w.Platforms)))
	le.PutUint32(buf[8:], uint32(len(w.Climbables)))
	putRect(buf[12:], w.Bounds)

	off := WorldHeaderSize
	for _, group := range [][]Rect{w.Solids, w.Platforms, w.Climbables} {
		for _, r := range group {
			putRect(buf[off:], r)
			off += rectSize
		}
	}
	return buf
}

// UnmarshalWorld decodes geometry produced by MarshalWorld. It reports whether
// the input was well formed; a short or inconsistent buffer means the two
// sides disagree about the layout, which must fail rather than be guessed at.
func UnmarshalWorld(src []byte) (*World, bool) {
	if len(src) < WorldHeaderSize {
		return nil, false
	}
	le := binary.LittleEndian
	nSolid := int(le.Uint32(src[0:]))
	nPlat := int(le.Uint32(src[4:]))
	nClimb := int(le.Uint32(src[8:]))

	total := nSolid + nPlat + nClimb
	if total < 0 || len(src) != WorldHeaderSize+rectSize*total {
		return nil, false
	}

	w := &World{
		Bounds:     getRect(src[12:]),
		Solids:     make([]Rect, nSolid),
		Platforms:  make([]Rect, nPlat),
		Climbables: make([]Rect, nClimb),
	}

	off := WorldHeaderSize
	for _, group := range [][]Rect{w.Solids, w.Platforms, w.Climbables} {
		for i := range group {
			group[i] = getRect(src[off:])
			off += rectSize
		}
	}
	return w, true
}

func putRect(dst []byte, r Rect) {
	le := binary.LittleEndian
	le.PutUint32(dst[0:], uint32(r.X))
	le.PutUint32(dst[4:], uint32(r.Y))
	le.PutUint32(dst[8:], uint32(r.W))
	le.PutUint32(dst[12:], uint32(r.H))
}

func getRect(src []byte) Rect {
	le := binary.LittleEndian
	return Rect{
		X: fixedFrom(le.Uint32(src[0:])),
		Y: fixedFrom(le.Uint32(src[4:])),
		W: fixedFrom(le.Uint32(src[8:])),
		H: fixedFrom(le.Uint32(src[12:])),
	}
}

// EncodeInput packs an Input into a single int32, so the WASM call takes only
// scalar arguments and needs no second buffer.
//
// Layout: move_x in the low 16 bits as a signed value offset by 1000 to keep
// it non-negative, then one bit each for jump, up, and down.
func EncodeInput(in Input) int32 {
	v := int32(in.clampMoveX()+1000) & 0xFFFF
	if in.Jump {
		v |= 1 << 16
	}
	if in.Up {
		v |= 1 << 17
	}
	if in.Down {
		v |= 1 << 18
	}
	return v
}

// DecodeInput reverses EncodeInput.
func DecodeInput(v int32) Input {
	return Input{
		MoveX: (v & 0xFFFF) - 1000,
		Jump:  v&(1<<16) != 0,
		Up:    v&(1<<17) != 0,
		Down:  v&(1<<18) != 0,
	}
}

// fixedFrom reinterprets an encoded word as a fixed-point value. The
// conversion is a bit reinterpretation, not a numeric one: the encoding stores
// the raw Q24.8 representation so nothing is rounded in either direction.
func fixedFrom(v uint32) fixed.F { return fixed.F(int32(v)) }
