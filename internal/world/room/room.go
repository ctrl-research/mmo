// Package room runs the simulation.
//
// A room instance is one map, populated, advanced by a single goroutine on a
// fixed 20 Hz tick. It owns every piece of mutable state inside it and takes
// no locks, because nothing else touches that state. Rooms are independent:
// two rooms never block on each other, which is what makes a room the unit of
// scale, of failure, and of migration.
//
// Everything that crosses a room boundary goes over the bus. A room must never
// read or write another room's state directly, even when both are goroutines
// in one process -- the shortcut always works today and breaks the day there
// are two world nodes (AGENTS.md invariant 1).
package room

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Tick timing.
//
// 20 Hz is fast enough that MapleStory-style action combat feels responsive
// once client prediction is layered on, and slow enough to leave real headroom
// in the tick budget. OSRS's 600 ms cadence would feel wrong here, so secondary
// skills instead run on a derived action tick every 12 sim ticks.
const (
	TickRate   = 20
	TickPeriod = time.Second / TickRate

	// TickBudget is the wall-clock time one tick may take. Exceeding it means
	// the room is falling behind real time, which no amount of extra hardware
	// fixes -- the room must be split or its capacity lowered.
	TickBudget = TickPeriod
)

// Input queue limits.
const (
	// maxInputQueue caps how far ahead of the server a client may run. A
	// client that exceeds it is lagging badly or misbehaving; the oldest
	// inputs are dropped, since stale intent is worth less than fresh.
	maxInputQueue = 8

	// catchUpThreshold is the queue depth above which the room applies two
	// inputs in one tick instead of one, to drain a backlog caused by network
	// jitter. The client has already predicted these, so consuming them faster
	// converges on its prediction rather than diverging from it.
	catchUpThreshold = 3

	// maxInputsPerTick bounds that catch-up so a large backlog cannot let one
	// player move several times faster than everyone else.
	maxInputsPerTick = 2
)

// ErrRoomFull is returned when a join would exceed the room's capacity.
var ErrRoomFull = errors.New("room: at capacity")

// Sink delivers server messages to one connected player.
//
// Implementations must not block: Send is called from inside the tick loop,
// and a sink that waits on a slow socket would stall the simulation for
// everyone else in the room. The gateway's implementation enqueues onto a
// buffered channel and drops for a client that has fallen too far behind.
type Sink interface {
	Send(msg *mmov1.ServerMessage)
	Close(code uint32, reason string)
}

// player is the room's per-connection state.
type player struct {
	entity *Entity
	sink   Sink

	// Pending inputs, oldest first, each with the client sequence number that
	// is echoed back to drive reconciliation.
	queue []queuedInput

	// lastInput is replayed when the queue starves, so a brief network gap
	// reads as continued motion rather than an abrupt stop.
	lastInput sim.Input

	ackSeq uint32

	// sent records what this player was last told about each entity, and is
	// the baseline for delta compression.
	sent map[EntityID]view

	// starved counts consecutive ticks with no input, for diagnostics.
	starved int

	// seenScratch is reused by the snapshot builder each tick to avoid
	// allocating a map per player per tick.
	seenScratch map[EntityID]struct{}
}

type queuedInput struct {
	seq uint32
	in  sim.Input
}

// Config describes a room instance at construction.
type Config struct {
	InstanceID directory.InstanceID
	MapID      string
	Capacity   int
	World      *sim.World
	Tuning     sim.Tuning
	Spawn      sim.Vec
	Logger     *slog.Logger

	// Observer receives per-tick statistics. Optional; the metrics package
	// provides one. Kept as an interface so the simulation has no dependency
	// on a metrics library.
	Observer Observer
}

// Observer receives per-tick measurements from a room.
type Observer interface {
	ObserveTick(mapID string, d time.Duration, entities, players int)
}

// Room is one live instance of one map.
type Room struct {
	cfg Config
	log *slog.Logger

	tick uint64

	// entities is the authoritative, deterministically ordered list. The
	// simulation iterates this slice and never a map: Go randomises map
	// iteration order, which would make the tick non-reproducible and break
	// both replay and client prediction.
	entities []*Entity
	index    map[EntityID]int

	// playerOrder keeps player iteration deterministic for the same reason.
	players     map[EntityID]*player
	playerOrder []EntityID

	nextEntityID EntityID

	cmds chan command
}

// New builds a room. It does not start ticking; call Run.
func New(cfg Config) *Room {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Capacity <= 0 {
		cfg.Capacity = 30
	}
	return &Room{
		cfg:     cfg,
		log:     cfg.Logger.With("instance", uint64(cfg.InstanceID), "map", cfg.MapID),
		index:   make(map[EntityID]int),
		players: make(map[EntityID]*player),
		// Buffered so a burst of input from many clients never blocks a
		// gateway goroutine while the room is mid-tick.
		cmds: make(chan command, 1024),
	}
}

// ID returns the instance ID.
func (r *Room) ID() directory.InstanceID { return r.cfg.InstanceID }

// Run drives the tick loop until ctx is cancelled. It must be called exactly
// once, and every field of Room is owned by this goroutine from here on.
func (r *Room) Run(ctx context.Context) error {
	ticker := time.NewTicker(TickPeriod)
	defer ticker.Stop()

	r.log.Info("room started", "capacity", r.cfg.Capacity, "tick_hz", TickRate)
	defer r.log.Info("room stopped", "ticks", r.tick)

	for {
		select {
		case <-ctx.Done():
			r.shutdown()
			return ctx.Err()

		case c := <-r.cmds:
			r.handle(c)

		case <-ticker.C:
			r.doTick()
		}
	}
}

// doTick advances the simulation by one step.
//
// The phase order is part of the game's observable behaviour, not an
// implementation detail: it decides, for example, whether a mob that dies this
// tick still lands the hit it queued this tick. Treat it as a spec.
func (r *Room) doTick() {
	start := time.Now()
	r.tick++

	r.phaseIngestAndMove()

	// M1 adds: abilities, AI, effects, rewards, spawns -- between movement and
	// snapshot, in that order.

	r.phaseSnapshot()

	elapsed := time.Since(start)
	if r.cfg.Observer != nil {
		r.cfg.Observer.ObserveTick(r.cfg.MapID, elapsed, len(r.entities), len(r.players))
	}
	if elapsed > TickBudget {
		// Over budget means the room is falling behind real time. More nodes
		// do not fix this; the room needs splitting or a lower capacity.
		r.log.Warn("tick over budget",
			"took", elapsed, "budget", TickBudget,
			"entities", len(r.entities), "players", len(r.players))
	}
}

// phaseIngestAndMove drains each player's input queue and advances their body.
func (r *Room) phaseIngestAndMove() {
	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil {
			continue
		}
		for _, in := range r.takeInputs(p) {
			sim.Step(&p.entity.Body, in, r.cfg.World, &r.cfg.Tuning)
		}
	}
}

// takeInputs decides how many queued inputs to apply this tick.
//
// Normally exactly one: the client simulates at the same 20 Hz the server
// does and sends one intent per simulated tick, so one in equals one out.
// Jitter makes that arrive unevenly, so a backlog drains slightly faster and a
// starved queue repeats the last input. Repeating beats freezing -- a player
// whose packet is 50 ms late should keep running, not stutter.
func (r *Room) takeInputs(p *player) []sim.Input {
	if len(p.queue) == 0 {
		p.starved++
		return []sim.Input{p.lastInput}
	}
	p.starved = 0

	n := 1
	if len(p.queue) > catchUpThreshold {
		n = min(maxInputsPerTick, len(p.queue))
	}

	out := make([]sim.Input, 0, n)
	for i := 0; i < n; i++ {
		q := p.queue[i]
		out = append(out, q.in)
		p.ackSeq = q.seq
		p.lastInput = q.in
	}
	p.queue = p.queue[n:]
	return out
}

// spawnEntity registers an entity and returns it. Called only from the room
// goroutine.
func (r *Room) spawnEntity(e *Entity) *Entity {
	r.nextEntityID++
	e.ID = r.nextEntityID
	r.index[e.ID] = len(r.entities)
	r.entities = append(r.entities, e)
	return e
}

// removeEntity unregisters an entity, preserving the relative order of the
// rest so iteration stays deterministic. A swap-with-last removal would be
// cheaper but would reorder the simulation, which changes results.
func (r *Room) removeEntity(id EntityID) {
	i, ok := r.index[id]
	if !ok {
		return
	}
	r.entities = append(r.entities[:i:i], r.entities[i+1:]...)
	delete(r.index, id)
	for j := i; j < len(r.entities); j++ {
		r.index[r.entities[j].ID] = j
	}
}

// visible reports whether viewer can see e.
//
// Correctness, not optimisation: a bug here leaks another player's mobs and
// loot into a client's view.
func visible(viewer, e *Entity) bool {
	return e.Layer == SharedLayer || e.Layer == viewer.Layer
}

// shutdown closes every connection when the room stops.
func (r *Room) shutdown() {
	for _, id := range r.playerOrder {
		if p := r.players[id]; p != nil {
			p.sink.Close(CloseServerShutdown, "server shutting down")
		}
	}
}

// Close codes, mirroring the table in docs/protocol.md. They tell the client
// whether to retry, re-authenticate, or give up, so a transient blip and a ban
// are not indistinguishable.
const (
	CloseTicketInvalid   = 4000
	CloseProtocolVersion = 4001
	CloseContentHash     = 4002
	CloseNotAllowed      = 4003
	CloseKicked          = 4004
	CloseLeaseLost       = 4005
	CloseRateLimited     = 4006
	CloseServerShutdown  = 4007
)

// spawnBody returns a player-sized body resting at the room's spawn point.
func (r *Room) spawnBody() sim.Body {
	b := sim.NewBody(r.cfg.Spawn, sim.PlayerSize.W, sim.PlayerSize.H)
	// Settle so a player who presses jump on their very first tick actually
	// jumps, instead of being treated as airborne for one tick.
	sim.Settle(&b, r.cfg.World, &r.cfg.Tuning)
	return b
}

func (r *Room) String() string {
	return fmt.Sprintf("room(%d, %s, %d players)", r.cfg.InstanceID, r.cfg.MapID, len(r.players))
}
