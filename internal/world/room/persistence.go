package room

import (
	"context"
	"encoding/json"

	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// CharacterState is the slice of a player entity that outlives a session.
//
// It is deliberately explicit rather than a serialisation of the whole entity:
// most of what an entity holds -- its layer, its sink, its snapshot baselines
// -- is meaningful only within one room on one node, and persisting it would
// mean writing per-session detail into a durable store and then having to
// migrate it.
//
// The JSON tags are a wire format. Once characters exist in a database, a
// renamed field silently loses whatever it held, so treat these names as
// permanent and add rather than rename.
type CharacterState struct {
	// Position and motion, so logging back in resumes exactly in place rather
	// than at the map's spawn point.
	X  fixed.F `json:"x"`
	Y  fixed.F `json:"y"`
	VX fixed.F `json:"vx"`
	VY fixed.F `json:"vy"`

	Facing bool `json:"facing_left"`

	HP    uint32 `json:"hp"`
	MaxHP uint32 `json:"hp_max"`
	MP    uint32 `json:"mp"`
	MaxMP uint32 `json:"mp_max"`
}

// Progress is the durable progression that has its own database columns.
type Progress struct {
	Level int
	Exp   int64
	Gold  int64
	MapID string
}

// Attachment is everything a room needs about the live session driving a
// character.
//
// It is grouped rather than passed as three arguments because it is one idea:
// the connection, the channel back to the session, and what that session
// already knows. None of it can cross a process boundary, which is exactly why
// it is handed over separately from the transfer that moved the character.
type Attachment struct {
	Sink   Sink
	Events SessionEvents

	// Loadout is what this character may cast: the skills on their bar, the
	// rank each is known at, and the supports linked to it.
	//
	// Sent by the session rather than looked up by the room, for the same
	// reason the stat block is: it lives in the database, and a room that read
	// it would be a room that knows about persistence.
	Loadout []LoadoutSlot

	// LayerKey decides which hostile-entity layer the character joins: the
	// party ID while partied, and the character ID otherwise. Empty falls back
	// to the character ID, so a caller that has not heard of parties still
	// gets a layer of its own rather than sharing one with everybody.
	LayerKey string

	// KnownWaypoints are the ones already unlocked, so standing on one does
	// not generate a database write every tick. Nil leaves the room's existing
	// set alone.
	KnownWaypoints []string

	// Secondary is cumulative experience per secondary skill. Nil leaves what
	// the room already has, which is what a reconnect to a character still
	// standing in the room wants: the room's copy is the newer one.
	Secondary SecondaryProgress
}

// JoinSpec describes the character entering a room.
type JoinSpec struct {
	// CharacterID identifies the character across nodes and sessions.
	CharacterID string

	Name string

	Progress Progress

	// State is the saved body and vitals. A zero value means a character that
	// has never played, which spawns at the map's spawn point at full health.
	State CharacterState

	// Fresh marks a character with no saved state, so the room places it at
	// the spawn point rather than at the origin.
	Fresh bool

	Sink Sink

	// Events is this player's channel back to their session, where work the
	// tick loop must not do -- database writes, transfers -- happens.
	Events SessionEvents

	// Loadout is what this character may cast: the skills on their bar, the
	// rank each is known at, and the supports linked to it.
	//
	// Sent by the session rather than looked up by the room, for the same
	// reason the stat block is: it lives in the database, and a room that read
	// it would be a room that knows about persistence.
	Loadout []LoadoutSlot

	// LayerKey decides which hostile-entity layer the character joins: the
	// party ID while partied, and the character ID otherwise. Empty falls back
	// to the character ID, so a caller that has not heard of parties still
	// gets a layer of its own rather than sharing one with everybody.
	LayerKey string

	// KnownWaypoints are the ones already unlocked, so standing on one does
	// not generate a write every tick.
	KnownWaypoints []string

	// Secondary is cumulative experience per secondary skill, read from the
	// database by the session. The room is authoritative for the session's
	// lifetime and reports every gain, but it has to start from somewhere.
	Secondary SecondaryProgress

	// Spawn overrides where a fresh character is placed, so a portal can land
	// them at a named entrance rather than the map's default.
	Spawn sim.Vec

	// Arrived marks a character coming through a portal, which starts the
	// portal cooldown so they do not immediately take the one they landed on.
	Arrived bool
}

// Snapshot is a character's current persistable state, taken on the room
// goroutine so it is internally consistent -- a checkpoint assembled from
// outside the tick could catch a body mid-update and save a position that
// never existed.
type Snapshot struct {
	Progress Progress
	State    CharacterState

	// Secondary is cumulative experience per secondary skill.
	//
	// In the checkpoint rather than written on every gain: a gather yields
	// every few seconds for as long as a player keeps at it, and one write per
	// log would make woodcutting the busiest table in the database. The gain
	// that carries the *item* has to go out immediately because an item must
	// exist somewhere, but experience can wait for the same interval
	// everything else waits for.
	Secondary SecondaryProgress
}

// captureCharacter reads a player entity's persistable state.
func captureCharacter(e *Entity, mapID string) Snapshot {
	snap := Snapshot{
		Progress: Progress{MapID: mapID},
		State: CharacterState{
			X:      e.Body.Pos.X,
			Y:      e.Body.Pos.Y,
			VX:     e.Body.Vel.X,
			VY:     e.Body.Vel.Y,
			Facing: e.Body.FacingLeft,
			HP:     e.HP,
			MaxHP:  e.MaxHP,
		},
	}
	if e.Player != nil {
		snap.Progress.Level = e.Player.Level
		snap.Progress.Exp = e.Player.Exp
		snap.Progress.Gold = e.Player.Gold
		snap.State.MP = e.Player.MP
		snap.State.MaxMP = e.Player.MaxMP

		// Copied rather than shared: the caller is on another goroutine by the
		// time it reads this, and the room keeps gathering into its own map.
		if len(e.Player.Secondary) > 0 {
			snap.Secondary = make(SecondaryProgress, len(e.Player.Secondary))
			for skill, exp := range e.Player.Secondary {
				snap.Secondary[skill] = exp
			}
		}
	}
	return snap
}

// applyCharacter restores saved state onto a freshly spawned entity.
func (r *Room) applyCharacter(e *Entity, spec JoinSpec) {
	p := e.Player

	if spec.Progress.Level > 0 {
		p.Level = spec.Progress.Level
	}
	p.Exp = spec.Progress.Exp
	p.Gold = spec.Progress.Gold

	if p.Secondary == nil {
		p.Secondary = make(map[string]int64, len(spec.Secondary))
	}
	for skill, exp := range spec.Secondary {
		p.Secondary[skill] = exp
	}

	e.MaxHP = MaxHPFor(p.Level)
	p.MaxMP = uint32(50 + (p.Level-1)*10)

	if spec.Fresh {
		// A new character starts at a spawn point, at full health. A named
		// spawn overrides the map default, which is how a portal decides where
		// its arrivals land.
		if spec.Spawn != (sim.Vec{}) {
			e.Body.SetFeetCenter(spec.Spawn)
			tuning := r.cfg.Tuning
			sim.Settle(&e.Body, r.cfg.World, &tuning)
		}
		e.HP = e.MaxHP
		p.MP = p.MaxMP
		return
	}

	e.Body.Pos.X = spec.State.X
	e.Body.Pos.Y = spec.State.Y
	e.Body.Vel.X = spec.State.VX
	e.Body.Vel.Y = spec.State.VY
	e.Body.FacingLeft = spec.State.Facing

	// Contact state is recomputed rather than restored: the map's geometry may
	// have changed since the character was saved, and a body that believes it
	// is grounded on a platform that no longer exists would hang in the air.
	tuning := r.cfg.Tuning
	sim.Settle(&e.Body, r.cfg.World, &tuning)

	e.HP = clampVital(spec.State.HP, e.MaxHP)
	p.MP = clampVital(spec.State.MP, p.MaxMP)

	// A character saved at zero HP would spawn dead with nothing to do. Death
	// handling belongs in the world, not in the loader, so restore them just
	// alive instead.
	if e.HP == 0 {
		e.HP = 1
	}
}

// clampVital bounds a restored vital by its current maximum.
//
// Maximums are derived from level, and a rebalance can lower them. Restoring
// a saved value unchecked would leave a character permanently above their own
// maximum, which every HP bar in the game would then render wrong.
func clampVital(v, max uint32) uint32 {
	if v > max {
		return max
	}
	return v
}

// MarshalState encodes a character's state for the database.
func MarshalState(s CharacterState) (json.RawMessage, error) {
	return json.Marshal(s)
}

// UnmarshalState decodes stored state.
//
// An empty or malformed value yields a zero state rather than an error: a
// character whose saved position cannot be read should spawn at the map's
// entrance, not be unplayable.
func UnmarshalState(raw json.RawMessage) CharacterState {
	var s CharacterState
	if len(raw) == 0 {
		return s
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return CharacterState{}
	}
	return s
}

// Capture returns one player's persistable state.
//
// It runs on the room goroutine, which is what makes the result consistent.
func (h *localHandle) Capture(ctx context.Context, id EntityID) (Snapshot, bool) {
	result := make(chan captureResult, 1)
	select {
	case h.room.cmds <- captureCmd{id: id, result: result}:
	case <-ctx.Done():
		return Snapshot{}, false
	}

	select {
	case res := <-result:
		return res.snapshot, res.ok
	case <-ctx.Done():
		return Snapshot{}, false
	}
}
