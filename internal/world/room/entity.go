package room

import (
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// EntityID identifies one entity within one room. IDs are unique for the
// lifetime of the room and are never reused, so a client that still holds a
// reference to something that died cannot have it silently rebound to an
// unrelated entity.
type EntityID uint32

// LayerID is a visibility layer.
//
// A player sees an entity if it is in the shared layer or in the player's own
// layer. Players are always shared-layer, so everyone in a room always sees
// everyone else; only hostile and lootable entities are layered. A layer key
// is the player's party ID, falling back to their character ID when unpartied.
//
// This is what removes spawn contention, kill stealing, and loot sniping: not
// by adding rules against them, but by making them unrepresentable. See
// docs/architecture.md.
type LayerID uint32

// SharedLayer is visible to every player in the room.
const SharedLayer LayerID = 0

// Kind distinguishes what an entity is, for rendering and for the rules that
// apply to it.
type Kind uint8

const (
	KindPlayer Kind = iota + 1
	KindMob
	KindDrop
	KindNPC
)

func (k Kind) wire() mmov1.EntityKind {
	switch k {
	case KindPlayer:
		return mmov1.EntityKind_ENTITY_KIND_PLAYER
	case KindMob:
		return mmov1.EntityKind_ENTITY_KIND_MOB
	case KindDrop:
		return mmov1.EntityKind_ENTITY_KIND_DROP
	case KindNPC:
		return mmov1.EntityKind_ENTITY_KIND_NPC
	default:
		return mmov1.EntityKind_ENTITY_KIND_UNSPECIFIED
	}
}

// Entity is anything in a room that a client can see.
//
// It is owned exclusively by its room's goroutine. Nothing outside the tick
// loop reads or writes one, which is what lets the whole simulation run
// without locks.
type Entity struct {
	ID    EntityID
	Kind  Kind
	Layer LayerID

	Body sim.Body

	Anim  uint32
	HP    uint32
	MaxHP uint32
	Name  string
}

// Entity flag bits, mirroring mmov1.EntityFlag. They are packed into a single
// wire field because sending six booleans separately costs more than the
// entire rest of a typical delta.
const (
	flagGrounded   = 1 << 0
	flagClimbing   = 1 << 1
	flagFacingLeft = 1 << 2
	flagJumpHeld   = 1 << 3
)

func (e *Entity) flags() uint32 {
	var f uint32
	if e.Body.Grounded {
		f |= flagGrounded
	}
	if e.Body.Climbing {
		f |= flagClimbing
	}
	if e.Body.FacingLeft {
		f |= flagFacingLeft
	}
	if e.Body.JumpHeld {
		f |= flagJumpHeld
	}
	return f
}

// state renders the entity as a complete wire message.
//
// includeSelf adds the prediction-only fields. They are sent exclusively to
// the entity's owner: no other client replays this body's inputs, so for
// everyone else they would be bytes per tick that nothing reads. Omitting them
// from the owner's copy, on the other hand, would make its replay diverge from
// the server -- which is why Body keeps no unexported state at all.
func (e *Entity) state(includeSelf bool) *mmov1.EntityState {
	s := &mmov1.EntityState{
		Id:    uint32(e.ID),
		Kind:  e.Kind.wire(),
		Layer: uint32(e.Layer),
		X:     int32(e.Body.Pos.X),
		Y:     int32(e.Body.Pos.Y),
		Vx:    int32(e.Body.Vel.X),
		Vy:    int32(e.Body.Vel.Y),
		W:     int32(e.Body.W),
		H:     int32(e.Body.H),
		Flags: e.flags(),
		Anim:  e.Anim,
		Hp:    e.HP,
		HpMax: e.MaxHP,
		Name:  e.Name,
	}
	if includeSelf {
		s.Coyote = uint32(e.Body.Coyote)
		s.JumpBuffer = uint32(e.Body.JumpBuffer)
		s.DropThrough = uint32(e.Body.DropThrough)
	}
	return s
}

// view is the subset of an entity that is delta-compressed, kept per viewer as
// the record of what they were last sent.
type view struct {
	x, y   int32
	vx, vy int32
	flags  uint32
	anim   uint32
	hp     uint32
	hpMax  uint32
}

func (e *Entity) view() view {
	return view{
		x:     int32(e.Body.Pos.X),
		y:     int32(e.Body.Pos.Y),
		vx:    int32(e.Body.Vel.X),
		vy:    int32(e.Body.Vel.Y),
		flags: e.flags(),
		anim:  e.Anim,
		hp:    e.HP,
		hpMax: e.MaxHP,
	}
}

// Field-mask bits, mirroring mmov1.EntityField.
const (
	fieldPos   = 1 << 0
	fieldVel   = 1 << 1
	fieldFlags = 1 << 2
	fieldAnim  = 1 << 3
	fieldHP    = 1 << 4
)

// delta returns what changed between two views, and whether anything did.
//
// Position and velocity are grouped rather than split per axis: an entity that
// moves almost always changes both components, so a per-axis mask would add
// bits far more often than it would save a field.
func (cur view) delta(id EntityID, prev view) (*mmov1.EntityDelta, bool) {
	d := &mmov1.EntityDelta{Id: uint32(id)}
	var mask uint32

	if cur.x != prev.x || cur.y != prev.y {
		mask |= fieldPos
		d.X, d.Y = cur.x, cur.y
	}
	if cur.vx != prev.vx || cur.vy != prev.vy {
		mask |= fieldVel
		d.Vx, d.Vy = cur.vx, cur.vy
	}
	if cur.flags != prev.flags {
		mask |= fieldFlags
		d.Flags = cur.flags
	}
	if cur.anim != prev.anim {
		mask |= fieldAnim
		d.Anim = cur.anim
	}
	if cur.hp != prev.hp || cur.hpMax != prev.hpMax {
		mask |= fieldHP
		d.Hp, d.HpMax = cur.hp, cur.hpMax
	}

	if mask == 0 {
		return nil, false
	}
	d.FieldMask = mask
	return d, true
}
