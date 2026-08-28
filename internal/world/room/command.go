package room

import (
	"context"
	"errors"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
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
	// Join adds a player and returns their entity ID.
	Join(ctx context.Context, name string, sink Sink) (EntityID, error)

	// Leave removes a player. It is safe to call for an unknown ID, which
	// happens whenever a disconnect races a kick.
	Leave(ctx context.Context, id EntityID)

	// Input queues one tick of intent. It never blocks the caller: a room that
	// is mid-tick must not be able to stall a gateway goroutine.
	Input(ctx context.Context, id EntityID, seq uint32, in sim.Input)
}

// command is one message to the room goroutine.
type command interface{ isCommand() }

type joinCmd struct {
	name   string
	sink   Sink
	result chan joinResult
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

func (joinCmd) isCommand()  {}
func (leaveCmd) isCommand() {}
func (inputCmd) isCommand() {}

// handle dispatches one command. It runs on the room goroutine, so it may
// touch room state freely.
func (r *Room) handle(c command) {
	switch cmd := c.(type) {
	case joinCmd:
		id, err := r.join(cmd.name, cmd.sink)
		cmd.result <- joinResult{id: id, err: err}
	case leaveCmd:
		r.leave(cmd.id)
	case inputCmd:
		r.input(cmd.id, cmd.seq, cmd.in)
	default:
		// Test-only commands implement the interface elsewhere in the package.
		if insp, ok := c.(interface{ run(*Room) }); ok {
			insp.run(r)
		}
	}
}

func (r *Room) join(name string, sink Sink) (EntityID, error) {
	if len(r.players) >= r.cfg.Capacity {
		return 0, ErrRoomFull
	}

	e := r.spawnEntity(&Entity{
		Kind: KindPlayer,
		// Players are always shared-layer: everyone in a room sees everyone
		// else. Only hostile and lootable entities are layered.
		Layer: SharedLayer,
		Body:  r.spawnBody(),
		HP:    100,
		MaxHP: 100,
		Name:  name,
	})

	p := &player{
		entity:      e,
		sink:        sink,
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

	r.log.Info("player joined", "entity", uint32(e.ID), "name", name, "players", len(r.players))
	return e.ID, nil
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

func (h *localHandle) Join(ctx context.Context, name string, sink Sink) (EntityID, error) {
	result := make(chan joinResult, 1)
	select {
	case h.room.cmds <- joinCmd{name: name, sink: sink, result: result}:
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
