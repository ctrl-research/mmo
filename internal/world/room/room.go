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

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/rng"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/items"
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

// nullSink is where messages for a character with no connection go.
//
// A character can be in a room with nobody watching -- mid-transfer, or inside
// a reconnect window -- and every path that would otherwise need a nil check
// is inside the tick loop, where a nil dereference takes the room down for
// everybody in it.
type nullSink struct{}

func (nullSink) Send(*mmov1.ServerMessage) {}
func (nullSink) Close(uint32, string)      {}

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

	// events is this player's channel back to their session. Per player rather
	// than per room, because each player has their own session.
	events SessionEvents

	// transferring marks a player whose portal transfer is in flight. They
	// take no further portal, and cannot be transferred twice.
	transferring bool

	// portalReadyAt is the tick from which portals apply again, so arriving on
	// a spawn that overlaps the return portal does not bounce the character
	// straight back.
	portalReadyAt uint64

	// portalNoticeAt throttles the "you cannot enter" message, which would
	// otherwise be sent every tick a player stands in a gated portal.
	portalNoticeAt uint64

	// knownWaypoints avoids one database write per tick while a player stands
	// on a waypoint they have already unlocked.
	knownWaypoints map[string]bool

	// casts queued since the last tick, drained in the cast phase.
	casts []castRequest

	// layer is the visibility layer this player's mobs and drops live in.
	// From M5 it is the party ID; until then, one per player.
	layer LayerID

	// characterID is the durable identity this session is playing, used when
	// checkpointing and when releasing the ownership lease.
	characterID string

	// frozen marks a player whose connection dropped. They remain in the world
	// but take no input, run no physics, and cannot be harmed.
	frozen bool
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

	// Content is the loaded, immutable game data. Rooms read it concurrently
	// with no locking, which is part of what keeps the tick cheap.
	Content *content.Content

	// Map is this room's map definition, including its spawn points.
	Map *content.Map

	// Seed determines every roll this room will make. Recording it alongside
	// the input log makes the room's entire history reproducible.
	Seed uint64

	// IdleTicks is how long the room may run with nobody in it before it asks
	// Retire whether it may stop. Zero disables idle teardown.
	IdleTicks int

	// Observer receives per-tick statistics. Optional; the metrics package
	// provides one. Kept as an interface so the simulation has no dependency
	// on a metrics library.
	Observer Observer

	// Retire asks whether the room may stop after standing empty. It returns
	// false when somebody has just been placed here, in which case the room
	// keeps running and asks again later.
	//
	// A callback rather than the room deciding for itself, because only the
	// node can make "stop hosting" and "stop being offered to players" one
	// step. Optional: a room with no Retire runs until its context is
	// cancelled, which is what a test wants.
	Retire func() bool
}

// Observer receives per-tick measurements from a room.
type Observer interface {
	ObserveTick(mapID string, d time.Duration, entities, players int)
}

// SessionEvents is the room's channel back to a player's session.
//
// Everything here needs work the tick loop must not do -- a database write, a
// transfer negotiated over the bus -- so the room reports what happened and
// the session acts on it.
//
// Implementations must not block: these are called mid-tick, and anything
// slower than a channel send stalls the simulation for everyone in the room.
type SessionEvents interface {
	// ClaimLoot reports that a player has asked for an item. The drop stays in
	// the world until the session confirms it through ResolveLoot.
	ClaimLoot(claim LootClaim)

	// EnterPortal reports that a player is standing in a portal. The session
	// runs the transfer protocol, which is not something a tick can wait for.
	EnterPortal(req PortalRequest)

	// DiscoverWaypoint reports that a player touched a waypoint, so the
	// session can record the unlock.
	DiscoverWaypoint(player EntityID, characterID, waypointID string)
}

// PortalRequest is a player standing in a portal.
type PortalRequest struct {
	Player      EntityID
	CharacterID string
	Portal      content.Portal
	Tick        uint64
}

// LootClaim is a player's request for an item, pending persistence.
type LootClaim struct {
	Player      EntityID
	CharacterID string
	DropID      EntityID
	Instance    *items.Instance
	Tick        uint64
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

	// content and mapDef are the loaded game data this room simulates.
	content *content.Content
	mapDef  *content.Map

	// items generates the modifiers a drop rolls, from the room's own stream.
	items *items.Generator

	// rand is the room's own generator, advanced only inside the tick loop.
	// Shared-layer content rolls from here; each layer has its own stream.
	rand *rng.Source

	// layers hold per-player mob populations. layerOrder keeps iteration
	// deterministic, because Go randomises map order and spawning in a
	// different sequence would break replay.
	layers     map[LayerID]*layerState
	layerOrder []LayerID

	// sharedSpawns are the spawn points every player in the room shares.
	sharedSpawns []*spawnState

	// layerKeys maps a layer key -- a party ID, or a character ID when
	// unpartied -- to this room's internal numbering, and nextLayer allocates
	// the numbers.
	layerKeys map[string]LayerID
	nextLayer LayerID

	// pending accumulates events produced during a tick, flushed with the
	// snapshot so a client receives a tick's outcome as one frame.
	pending []pendingEvent

	// emptySince is the tick the last player left, or zero while anyone is
	// here. It is what the idle timeout measures from.
	emptySince uint64

	// retiring is set once the node has agreed the room may stop, so the loop
	// exits after finishing the tick it is in rather than mid-phase.
	retiring bool

	cmds chan command
}

// pendingEvent is an event awaiting delivery, with the audience it belongs to.
type pendingEvent struct {
	event *mmov1.Event

	// layer scopes the event to one layer's viewers. SharedLayer means
	// everyone in the room.
	layer LayerID

	// only, when non-zero, restricts delivery to a single entity's owner --
	// experience and loot are nobody else's business.
	only EntityID
}

// New builds a room. It does not start ticking; call Run.
func New(cfg Config) *Room {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Capacity <= 0 {
		cfg.Capacity = 30
	}
	r := &Room{
		cfg:       cfg,
		log:       cfg.Logger.With("instance", uint64(cfg.InstanceID), "map", cfg.MapID),
		index:     make(map[EntityID]int),
		players:   make(map[EntityID]*player),
		content:   cfg.Content,
		mapDef:    cfg.Map,
		items:     items.NewGenerator(cfg.Content),
		rand:      rng.New(cfg.Seed),
		layers:    make(map[LayerID]*layerState),
		layerKeys: make(map[string]LayerID),
		// Buffered so a burst of input from many clients never blocks a
		// gateway goroutine while the room is mid-tick.
		cmds: make(chan command, 1024),
	}

	// Shared-layer spawn points exist once for the whole room: the field boss
	// everyone fights, which is the counterweight to per-player mobs.
	if cfg.Map != nil {
		for i := range cfg.Map.MobSpawns {
			sp := &cfg.Map.MobSpawns[i]
			if sp.Layer == content.LayerShared {
				r.sharedSpawns = append(r.sharedSpawns, newSpawnState(sp, SharedLayer))
			}
		}
	}
	return r
}

// randFor returns the generator scoped to a layer.
//
// Per-layer streams mean one player's drop luck never depends on how many
// other players happen to be in the room, which would otherwise make loot
// subtly non-reproducible in a replay.
func (r *Room) randFor(layer LayerID) *rng.Source {
	if layer == SharedLayer {
		return r.rand
	}
	if l, ok := r.layers[layer]; ok {
		return l.rand
	}
	return r.rand
}

// entity returns an entity by ID, or nil.
func (r *Room) entity(id EntityID) *Entity {
	if i, ok := r.index[id]; ok {
		return r.entities[i]
	}
	return nil
}

// emit queues an event for everyone who can see the given layer.
func (r *Room) emit(ev *mmov1.Event, layer LayerID) {
	r.pending = append(r.pending, pendingEvent{event: ev, layer: layer})
}

// emitTo queues an event for one player only.
func (r *Room) emitTo(id EntityID, ev *mmov1.Event) {
	r.pending = append(r.pending, pendingEvent{event: ev, only: id})
}

// ID returns the instance ID.
func (r *Room) ID() directory.InstanceID { return r.cfg.InstanceID }

// Run drives the tick loop until ctx is cancelled. It must be called exactly
// once, and every field of Room is owned by this goroutine from here on.
func (r *Room) Run(ctx context.Context) error {
	ticker := time.NewTicker(TickPeriod)
	defer ticker.Stop()

	r.log.Info("room started", "capacity", r.cfg.Capacity, "tick_hz", TickRate)
	// Wrapped in a closure because a deferred call evaluates its arguments
	// now, not on return -- so logging r.tick directly reports zero for every
	// room that ever ran.
	defer func() { r.log.Info("room stopped", "ticks", r.tick) }()

	for {
		select {
		case <-ctx.Done():
			r.shutdown()
			return ctx.Err()

		case c := <-r.cmds:
			r.handle(c)

		case <-ticker.C:
			r.doTick()
			if r.retiring {
				// Nobody is here, and the node has already stopped offering
				// this instance to anyone, so there is nothing left to do.
				r.shutdown()
				return nil
			}
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

	// The phase order is part of the game's observable behaviour. It decides,
	// for example, whether a mob that dies this tick still lands the hit it
	// queued this tick -- it does not, because AI runs before its own death is
	// processed but after players have already acted.
	r.phaseIngestAndMove()
	// Portals and waypoints are checked against final positions for this tick,
	// before combat and AI: a player standing in a portal is leaving, and
	// should not take a hit from a mob on the way out.
	r.phasePortals()

	// Buffs and spawned entities resolve before anything acts. A
	// damage-over-time or a bolt that kills something this tick stops it
	// acting; the alternative is a mob that gets a free swing from beyond the
	// grave, which reads as the game cheating.
	r.phaseBuffs()
	r.phaseSpawned()

	r.phaseCasts()
	r.phaseAI()
	r.phaseDrops()
	r.phaseSpawns()
	r.phaseSnapshot()
	r.pending = r.pending[:0]

	r.checkIdle()

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

// checkIdle stops a room that has stood empty for long enough.
//
// An empty room is not free: it holds a goroutine, wakes 20 times a second,
// and keeps every mob it spawned. It is also not worthless -- a player who
// steps through a portal and straight back should find the room they left --
// so the timeout is generous rather than immediate.
//
// The Retire call blocks the tick loop. That is acceptable precisely because
// the room is empty: there is nobody whose tick it could delay, and it happens
// at most once per idle interval.
func (r *Room) checkIdle() {
	if r.cfg.Retire == nil || r.cfg.IdleTicks <= 0 {
		return
	}
	if len(r.players) > 0 {
		r.emptySince = 0
		return
	}
	if r.emptySince == 0 {
		r.emptySince = r.tick
		return
	}
	if r.tick-r.emptySince < uint64(r.cfg.IdleTicks) {
		return
	}

	if !r.cfg.Retire() {
		// Somebody has just been placed here and their join is on its way.
		// Restart the clock rather than asking again on the very next tick.
		r.emptySince = r.tick
		return
	}
	r.retiring = true
}

// phaseIngestAndMove drains each player's input queue and advances their body.
func (r *Room) phaseIngestAndMove() {
	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.frozen {
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

// rarityWeights returns the drop rarity distribution.
//
// Read from content so it is tunable without a deploy, with a fallback that
// keeps a room working if the section is absent.
func (r *Room) rarityWeights() items.RarityWeights {
	return items.DefaultRarityWeights
}
