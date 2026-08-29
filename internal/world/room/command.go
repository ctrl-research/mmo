package room

import (
	"context"
	"errors"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// ErrRoomClosed is returned when a command is sent to a room that has stopped.
var ErrRoomClosed = errors.New("room: closed")

// Handle is how everything outside a room talks to it.
//
// It exists so that callers cannot reach into room state directly. Its local
// implementation is a channel send; the implementation added in M9 for a room
// on another node publishes to the bus instead. Callers cannot tell the
// difference, which is the entire point -- and why even the in-process path
// goes through it (AGENTS.md invariants 1 and 2).
type Handle interface {
	// Join places a character and returns the entity it was given.
	Join(ctx context.Context, spec JoinSpec) (EntityID, error)

	// Capture reads a player's persistable state for checkpointing.
	Capture(ctx context.Context, id EntityID) (Snapshot, bool)

	// Freeze suspends a player whose connection dropped, leaving them in the
	// world but inert, so a brief network blip does not cost their position in
	// a fight.
	Freeze(ctx context.Context, id EntityID)

	// Thaw resumes a frozen player on a new connection, reporting whether they
	// were still present to resume.
	Thaw(ctx context.Context, id EntityID, sink Sink) bool

	// SetStats pushes a recomputed stat block in. The room never computes one
	// itself: that would mean knowing about items, and item state belongs
	// where it can be written durably.
	SetStats(ctx context.Context, id EntityID, block *stats.Block, maxHP uint32)

	// ResolveLoot completes a claim once persistence has finished.
	ResolveLoot(ctx context.Context, player, dropID EntityID, granted bool, reason string)

	// Leave removes a player. It is safe to call for an unknown ID, which
	// happens whenever a disconnect races a kick.
	Leave(ctx context.Context, id EntityID)

	// Input queues one tick of intent. It never blocks the caller: a room that
	// is mid-tick must not be able to stall a gateway goroutine.
	Input(ctx context.Context, id EntityID, seq uint32, in sim.Input)

	// Cast queues a skill request.
	Cast(ctx context.Context, id EntityID, skillID string, facingLeft bool)

	// Interact queues a request to act on a nearby entity.
	Interact(ctx context.Context, id EntityID, target EntityID, kind InteractKind)
}

// command is one message to the room goroutine.
type command interface{ isCommand() }

type joinCmd struct {
	spec   JoinSpec
	result chan joinResult
}

// captureCmd reads a player's persistable state on the room goroutine, which
// is what makes the result internally consistent.
type captureCmd struct {
	id     EntityID
	result chan captureResult
}

type captureResult struct {
	snapshot Snapshot
	ok       bool
}

// freezeCmd suspends a player whose connection dropped, without removing them.
type freezeCmd struct{ id EntityID }

// thawCmd resumes a frozen player on a new connection.
type thawCmd struct {
	id     EntityID
	sink   Sink
	result chan bool
}

// setStatsCmd pushes a recomputed stat block in from the session.
type setStatsCmd struct {
	id    EntityID
	stats *stats.Block
	maxHP uint32
}

// resolveLootCmd completes a claim once persistence has finished.
type resolveLootCmd struct {
	player  EntityID
	dropID  EntityID
	granted bool
	reason  string
}

type joinResult struct {
	id  EntityID
	err error
}

type leaveCmd struct{ id EntityID }

type inputCmd struct {
	id  EntityID
	seq uint32
	in  sim.Input
}

type castCmd struct {
	id  EntityID
	req castRequest
}

// castRequest is a client's request to use a skill. It carries no target and
// no damage: the server resolves what the swing reaches from its own state.
type castRequest struct {
	skillID    string
	facingLeft bool
}

type interactCmd struct {
	id     EntityID
	target EntityID
	kind   InteractKind
}

// InteractKind is what a player wants to do with a nearby entity.
type InteractKind uint8

const (
	InteractLoot InteractKind = iota + 1
)

func (joinCmd) isCommand()        {}
func (captureCmd) isCommand()     {}
func (freezeCmd) isCommand()      {}
func (thawCmd) isCommand()        {}
func (setStatsCmd) isCommand()    {}
func (resolveLootCmd) isCommand() {}
func (leaveCmd) isCommand()       {}
func (inputCmd) isCommand()       {}
func (castCmd) isCommand()        {}
func (interactCmd) isCommand()    {}

// handle dispatches one command. It runs on the room goroutine, so it may
// touch room state freely.
func (r *Room) handle(c command) {
	switch cmd := c.(type) {
	case joinCmd:
		id, err := r.join(cmd.spec)
		cmd.result <- joinResult{id: id, err: err}
	case captureCmd:
		snap, ok := r.capture(cmd.id)
		cmd.result <- captureResult{snapshot: snap, ok: ok}
	case freezeCmd:
		r.freeze(cmd.id)
	case thawCmd:
		cmd.result <- r.thaw(cmd.id, cmd.sink)
	case setStatsCmd:
		r.setStats(cmd.id, cmd.stats, cmd.maxHP)
	case resolveLootCmd:
		r.resolveLoot(cmd.dropID, cmd.player, cmd.granted, cmd.reason)
	case leaveCmd:
		r.leave(cmd.id)
	case inputCmd:
		r.input(cmd.id, cmd.seq, cmd.in)
	case castCmd:
		r.queueCast(cmd.id, cmd.req)
	case interactCmd:
		r.interact(cmd.id, cmd.target, cmd.kind)
	default:
		// Test-only commands implement the interface elsewhere in the package.
		if insp, ok := c.(interface{ run(*Room) }); ok {
			insp.run(r)
		}
	}
}

func (r *Room) join(spec JoinSpec) (EntityID, error) {
	if len(r.players) >= r.cfg.Capacity {
		return 0, ErrRoomFull
	}

	state := newPlayerState()
	name := spec.Name
	sink := spec.Sink

	e := r.spawnEntity(&Entity{
		Kind: KindPlayer,
		// Players are always shared-layer: everyone in a room sees everyone
		// else, chats with them, and can party with them. Only hostile and
		// lootable entities are layered.
		Layer:  SharedLayer,
		Body:   r.spawnBody(),
		HP:     MaxHPFor(state.Level),
		MaxHP:  MaxHPFor(state.Level),
		Name:   name,
		Player: state,
	})

	// Allocate the layer this player's mobs and drops live in. From M5 the key
	// is the party ID, so partying up merges views; until parties exist every
	// player gets their own, which is the same code path with a different key.
	// Restore saved progression and position before anything observes the
	// entity, so the first snapshot a client receives is already correct
	// rather than a spawn-point position corrected a tick later.
	r.applyCharacter(e, spec)

	r.nextLayer++
	layer := r.nextLayer
	r.layerFor(layer)
	e.HuntLayer = layer

	p := &player{
		entity:      e,
		sink:        sink,
		layer:       layer,
		characterID: spec.CharacterID,
		items:       spec.Items,
		sent:        make(map[EntityID]view),
		seenScratch: make(map[EntityID]struct{}),
	}
	r.players[e.ID] = p
	r.playerOrder = append(r.playerOrder, e.ID)

	sink.Send(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Welcome{Welcome: &mmov1.Welcome{
			EntityId:   uint32(e.ID),
			InstanceId: uint64(r.cfg.InstanceID),
			Tick:       r.tick,
			TickMs:     uint32(TickPeriod.Milliseconds()),
			MapId:      r.cfg.MapID,
			Self:       e.state(true),
		}},
	})

	r.broadcastExcept(e.ID, &mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_PlayerJoined{PlayerJoined: &mmov1.PlayerJoined{
				EntityId: uint32(e.ID),
				Name:     name,
			}},
		}},
	})

	r.log.Info("player joined",
		"entity", uint32(e.ID), "name", name, "character", spec.CharacterID,
		"level", state.Level, "players", len(r.players))
	return e.ID, nil
}

// capture reads a player's persistable state.
func (r *Room) capture(id EntityID) (Snapshot, bool) {
	p, ok := r.players[id]
	if !ok {
		return Snapshot{}, false
	}
	return captureCharacter(p.entity, r.cfg.MapID), true
}

func (r *Room) leave(id EntityID) {
	p, ok := r.players[id]
	if !ok {
		return
	}

	delete(r.players, id)
	for i, other := range r.playerOrder {
		if other == id {
			r.playerOrder = append(r.playerOrder[:i:i], r.playerOrder[i+1:]...)
			break
		}
	}
	r.removeEntity(id)

	// Releasing the layer tears down its mobs and drops once the last player
	// using it leaves. Without this a long-lived room accumulates populations
	// nobody can see -- pure tick cost, and a slow leak.
	r.releaseLayer(p.layer)

	r.broadcastExcept(id, &mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_PlayerLeft{PlayerLeft: &mmov1.PlayerLeft{
				EntityId: uint32(id),
			}},
		}},
	})

	r.log.Info("player left", "entity", uint32(id), "name", p.entity.Name, "players", len(r.players))
}

// input queues one tick of intent for a player.
func (r *Room) input(id EntityID, seq uint32, in sim.Input) {
	p, ok := r.players[id]
	if !ok {
		return
	}

	// Sequence numbers are strictly increasing per connection. A repeat or a
	// regression is a duplicate or a reordering, and applying it would rewind
	// the player's own prediction.
	if len(p.queue) > 0 && seq <= p.queue[len(p.queue)-1].seq {
		return
	}
	if seq <= p.ackSeq {
		return
	}

	// A client far enough ahead to overflow the queue is lagging badly or
	// misbehaving. Drop the oldest: stale intent is worth less than fresh.
	if len(p.queue) >= maxInputQueue {
		p.queue = p.queue[1:]
	}
	p.queue = append(p.queue, queuedInput{seq: seq, in: in})
}

// broadcastExcept sends to every player but one.
func (r *Room) broadcastExcept(skip EntityID, msg *mmov1.ServerMessage) {
	for _, id := range r.playerOrder {
		if id == skip {
			continue
		}
		if p := r.players[id]; p != nil {
			p.sink.Send(msg)
		}
	}
}

// localHandle is the in-process Handle: a channel send to the room goroutine.
type localHandle struct{ room *Room }

// NewHandle returns a Handle for a room in this process.
func NewHandle(r *Room) Handle { return &localHandle{room: r} }

func (h *localHandle) Join(ctx context.Context, spec JoinSpec) (EntityID, error) {
	result := make(chan joinResult, 1)
	select {
	case h.room.cmds <- joinCmd{spec: spec, result: result}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	select {
	case res := <-result:
		return res.id, res.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (h *localHandle) Leave(ctx context.Context, id EntityID) {
	select {
	case h.room.cmds <- leaveCmd{id: id}:
	case <-ctx.Done():
	}
}

func (h *localHandle) Cast(_ context.Context, id EntityID, skillID string, facingLeft bool) {
	select {
	case h.room.cmds <- castCmd{id: id, req: castRequest{skillID: skillID, facingLeft: facingLeft}}:
	default:
	}
}

func (h *localHandle) Interact(_ context.Context, id EntityID, target EntityID, kind InteractKind) {
	select {
	case h.room.cmds <- interactCmd{id: id, target: target, kind: kind}:
	default:
	}
}

func (h *localHandle) Input(_ context.Context, id EntityID, seq uint32, in sim.Input) {
	// Never block. If the room is saturated the input is dropped, and the
	// simulation repeats the player's last intent -- which reads as brief
	// input lag rather than as a stalled gateway.
	select {
	case h.room.cmds <- inputCmd{id: id, seq: seq, in: in}:
	default:
	}
}

var _ Handle = (*localHandle)(nil)

// maxQueuedCasts bounds how many casts one client may have pending.
//
// A player can legitimately queue at most one per tick; anything beyond a
// small buffer is a client sending faster than it simulates, and the excess is
// dropped rather than allowed to grow.
const maxQueuedCasts = 4

func (r *Room) queueCast(id EntityID, req castRequest) {
	p, ok := r.players[id]
	if !ok || len(p.casts) >= maxQueuedCasts {
		return
	}
	p.casts = append(p.casts, req)
}

func (r *Room) interact(id EntityID, target EntityID, kind InteractKind) {
	p, ok := r.players[id]
	if !ok {
		return
	}
	switch kind {
	case InteractLoot:
		r.tryLoot(p.entity, target)
	}
}

// freeze suspends a player without removing them.
//
// A frozen character stays visible so the world does not appear to blink
// people in and out on every transient disconnect, but takes no input, runs no
// physics, and cannot be harmed. Leaving them vulnerable would make a dropped
// connection worse than a clean logout, which is exactly backwards.
func (r *Room) freeze(id EntityID) {
	p, ok := r.players[id]
	if !ok {
		return
	}

	p.frozen = true
	p.queue = p.queue[:0]
	p.lastInput = sim.Input{}
	p.entity.Body.Vel = sim.Vec{}

	// Any mob chasing them loses interest, rather than standing over an
	// untouchable target forever.
	for _, e := range r.entities {
		if e.Mob != nil && e.Mob.Target == id {
			e.Mob.Target = 0
			e.Mob.State = aiLeash
		}
	}

	r.log.Info("player frozen after disconnect", "entity", uint32(id), "name", p.entity.Name)
}

// thaw resumes a frozen player on a new connection.
func (r *Room) thaw(id EntityID, sink Sink) bool {
	p, ok := r.players[id]
	if !ok {
		return false
	}

	p.frozen = false
	p.sink = sink

	// Forget what the previous connection was told: the new client has no
	// baseline, so every visible entity must be sent again in full rather than
	// as a delta against something it never received.
	clear(p.sent)
	p.ackSeq = 0

	sink.Send(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Welcome{Welcome: &mmov1.Welcome{
			EntityId:   uint32(id),
			InstanceId: uint64(r.cfg.InstanceID),
			Tick:       r.tick,
			TickMs:     uint32(TickPeriod.Milliseconds()),
			MapId:      r.cfg.MapID,
			Self:       p.entity.state(true),
		}},
	})

	r.log.Info("player resumed after reconnect", "entity", uint32(id), "name", p.entity.Name)
	return true
}

func (h *localHandle) Freeze(ctx context.Context, id EntityID) {
	select {
	case h.room.cmds <- freezeCmd{id: id}:
	case <-ctx.Done():
	}
}

func (h *localHandle) Thaw(ctx context.Context, id EntityID, sink Sink) bool {
	result := make(chan bool, 1)
	select {
	case h.room.cmds <- thawCmd{id: id, sink: sink, result: result}:
	case <-ctx.Done():
		return false
	}

	select {
	case ok := <-result:
		return ok
	case <-ctx.Done():
		return false
	}
}

// setStats installs a recomputed stat block.
func (r *Room) setStats(id EntityID, block *stats.Block, maxHP uint32) {
	p, ok := r.players[id]
	if !ok || p.entity.Player == nil {
		return
	}

	p.entity.Player.Stats = block

	if maxHP > 0 {
		previous := p.entity.MaxHP
		p.entity.MaxHP = maxHP

		// Current health scales with the maximum rather than staying put.
		// Unequipping a life item at full health should not leave someone
		// above their new maximum, and equipping one should not leave them
		// looking wounded.
		switch {
		case previous == 0:
			p.entity.HP = maxHP
		case p.entity.HP > maxHP:
			p.entity.HP = maxHP
		case maxHP > previous && p.entity.HP == previous:
			p.entity.HP = maxHP
		}
	}
}

func (h *localHandle) SetStats(ctx context.Context, id EntityID, block *stats.Block, maxHP uint32) {
	select {
	case h.room.cmds <- setStatsCmd{id: id, stats: block, maxHP: maxHP}:
	case <-ctx.Done():
	}
}

func (h *localHandle) ResolveLoot(ctx context.Context, player, dropID EntityID, granted bool, reason string) {
	select {
	case h.room.cmds <- resolveLootCmd{player: player, dropID: dropID, granted: granted, reason: reason}:
	case <-ctx.Done():
	}
}
