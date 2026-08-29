// Package world hosts room instances on one node.
//
// A node owns a set of rooms, each an independent goroutine running its own
// tick loop. Which rooms live on which node is the directory's decision, so
// distributing rooms across many nodes later changes configuration rather than
// code (see docs/architecture.md).
package world

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ctrl-research/mmo/internal/bus"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/store"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"github.com/google/uuid"
)

// Node hosts room instances.
type Node struct {
	dir      directory.Directory
	leases   directory.Leases
	store    *store.Store
	bus      bus.Bus
	rooms3   *Registry
	nodeID   string
	content  *content.Content
	log      *slog.Logger
	observer room.Observer

	// defaultMap is where a player with no saved location starts. Characters
	// remember their own map from M2 onward.
	defaultMap string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	seed      uint64
	grace     time.Duration
	idleTicks int

	mu    sync.Mutex
	rooms map[directory.InstanceID]*hosted

	// holding tracks characters kept in the world through a reconnect grace
	// window, so a returning player resumes the character they left rather
	// than being told it is already in play by their own dropped session.
	holdMu  sync.Mutex
	holding map[uuid.UUID]*Session

	// seenTokens is the highest fencing token accepted for each character, so
	// a replayed or delayed transfer cannot resurrect one in a room it has
	// already left.
	tokenMu    sync.Mutex
	seenTokens map[uuid.UUID]int64

	transferSub bus.Subscription
	hostSub     bus.Subscription
}

type hosted struct {
	room   *room.Room
	handle room.Handle
}

// Config configures a Node.
type Config struct {
	Directory directory.Directory
	Leases    directory.Leases
	Store     *store.Store
	NodeID    string

	// Bus carries transfers between nodes. Required: the transfer protocol
	// runs over it even when the destination is this same node, because a
	// local shortcut would mean the distributed path is never exercised.
	Bus bus.Bus

	// Rooms resolves an instance to a handle, wherever it is hosted. Shared
	// between nodes in one process so a transfer can reach a room another node
	// owns; across processes this becomes a bus-backed handle in M9.
	Rooms      *Registry
	Content    *content.Content
	DefaultMap string
	Logger     *slog.Logger
	Observer   room.Observer

	// Seed determines every roll every room on this node will make. Zero
	// draws a fresh one, which is right for a real server; a fixed value makes
	// a session reproducible, which is what tests and replay want.
	Seed uint64

	// IdleTicks overrides how long a room runs empty before it stops. Zero
	// uses the value from content; tests set it short so the teardown path is
	// reachable without waiting out a minute of wall clock.
	IdleTicks int

	// ReconnectGrace overrides how long a character is held after a dropped
	// connection. Zero uses the default; tests set it short so both the
	// resume path and the expiry path are reachable without waiting a minute.
	ReconnectGrace time.Duration
}

// NewNode builds a node. Call Start before use.
func NewNode(cfg Config) (*Node, error) {
	if cfg.Directory == nil {
		return nil, fmt.Errorf("world: Directory is required")
	}
	if cfg.Content == nil || len(cfg.Content.Maps) == 0 {
		return nil, fmt.Errorf("world: loaded content with at least one map is required")
	}
	if _, ok := cfg.Content.Maps[cfg.DefaultMap]; !ok {
		return nil, fmt.Errorf("world: default map %q was not loaded", cfg.DefaultMap)
	}
	if cfg.Bus == nil {
		return nil, fmt.Errorf("world: a Bus is required; transfers run over it even locally")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	rooms := cfg.Rooms
	if rooms == nil {
		rooms = NewRegistry()
	}

	grace := cfg.ReconnectGrace
	if grace <= 0 {
		grace = DefaultReconnectGrace
	}

	seed := cfg.Seed
	if seed == 0 {
		// A fresh seed per process, so two servers started from the same image
		// do not produce identical drops. Read once at startup rather than
		// inside a tick, where a non-deterministic source would break replay.
		seed = uint64(time.Now().UnixNano())
	}

	idleTicks := cfg.IdleTicks
	if idleTicks <= 0 {
		idleTicks = cfg.Content.Balance.Rooms.IdleTicks
	}

	nodeID := cfg.NodeID
	if nodeID == "" {
		nodeID = "node-1"
	}

	return &Node{
		dir:        cfg.Directory,
		leases:     cfg.Leases,
		store:      cfg.Store,
		nodeID:     nodeID,
		content:    cfg.Content,
		defaultMap: cfg.DefaultMap,
		log:        cfg.Logger,
		observer:   cfg.Observer,
		bus:        cfg.Bus,
		rooms3:     rooms,
		seed:       seed,
		grace:      grace,
		idleTicks:  idleTicks,
		rooms:      make(map[directory.InstanceID]*hosted),
		holding:    make(map[uuid.UUID]*Session),
		seenTokens: make(map[uuid.UUID]int64),
	}, nil
}

// Start begins hosting. Rooms are created lazily, when a player first needs
// one, so an empty world costs nothing.
func (n *Node) Start(ctx context.Context) error {
	n.ctx, n.cancel = context.WithCancel(ctx)

	// Registered before anything is served, so the directory can place rooms
	// here from the first request. A directory that does not track nodes --
	// there is only one implementation that does not, and it is a stub in a
	// test -- simply keeps its own idea of where rooms go.
	if reg, ok := n.dir.(nodeRegistrar); ok {
		reg.AddNode(directory.NodeID(n.nodeID))
	}

	// Listening before anything can arrive, or a character handed over during
	// startup would find nobody home.
	if err := n.serveHosting(n.ctx); err != nil {
		return err
	}
	return n.serveTransfers(n.ctx)
}

// nodeRegistrar is implemented by directories that choose which node hosts a
// room. Optional, so a directory can pin everything to one node without
// implementing it.
type nodeRegistrar interface {
	AddNode(directory.NodeID)
}

// Stop tears down every room and waits for their goroutines to exit.
func (n *Node) Stop() {
	if n.transferSub != nil {
		n.transferSub.Close()
	}
	if n.hostSub != nil {
		n.hostSub.Close()
	}
	if n.cancel != nil {
		n.cancel()
	}
	n.wg.Wait()
}

// resolve returns a handle to an instance, wherever it is hosted.
//
// Local rooms resolve directly. A room on another node in this process
// resolves through the shared registry, which is what lets a transfer reach a
// different world role. A room in another *process* needs a bus-backed handle,
// which is M9 -- and until then this reports that it cannot be reached rather
// than silently doing something else.
func (n *Node) resolve(instance directory.InstanceID, node directory.NodeID) (room.Handle, error) {
	n.mu.Lock()
	hosted, ok := n.rooms[instance]
	n.mu.Unlock()
	if ok {
		return hosted.handle, nil
	}

	if h, ok := n.rooms3.Lookup(instance); ok {
		return h, nil
	}

	return nil, fmt.Errorf("world: instance %d on node %s is not reachable from here; "+
		"cross-process room handles arrive in M9", instance, node)
}

// ensureRoom returns the room for an instance, starting it if this node is not
// already hosting it.
func (n *Node) ensureRoom(inst directory.Instance, m *content.Map) (room.Handle, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if h, ok := n.rooms[inst.ID]; ok {
		return h.handle, nil
	}

	if n.ctx == nil {
		return nil, fmt.Errorf("world: node not started")
	}

	spawn := m.DefaultSpawn()
	r := room.New(room.Config{
		InstanceID: inst.ID,
		MapID:      m.ID,
		Capacity:   m.Capacity,
		World:      m.World,
		Tuning:     sim.DefaultTuning(),
		Spawn:      spawn.At,
		Logger:     n.log,
		Observer:   n.observer,
		Content:    n.content,
		Map:        m,
		// Each instance gets its own seed derived from the node's, so two
		// channels of the same map are independent while the whole node stays
		// reproducible from one recorded value.
		Seed: n.seed ^ (uint64(inst.ID) * 0x9E3779B97F4A7C15),

		// An empty room is not free -- a goroutine, twenty wakeups a second,
		// and every mob it has spawned -- and a world of many maps is mostly
		// empty rooms.
		IdleTicks: n.idleTicks,
		Retire:    func() bool { return n.retire(inst.ID, m.ID) },
	})

	h := &hosted{room: r, handle: room.NewHandle(r)}
	n.rooms[inst.ID] = h

	// Registered so another node in this process can reach it. That is what
	// makes "hosted by a different world role" true rather than nominal.
	n.rooms3.Register(inst.ID, h.handle)

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		_ = r.Run(n.ctx)

		n.mu.Lock()
		delete(n.rooms, inst.ID)
		n.mu.Unlock()
		n.rooms3.Unregister(inst.ID)

		// Release the instance so the directory stops offering it to players.
		// Using a background context: the node's is already cancelled by the
		// time a room stops.
		_ = n.dir.Release(context.Background(), inst.ID)
	}()

	n.log.Info("room instance started", "instance", uint64(inst.ID), "map", m.ID)
	return h.handle, nil
}

// Rooms reports how many instances this node hosts.
func (n *Node) Rooms() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.rooms)
}

// Content returns the loaded game data this node simulates.
func (n *Node) Content() *content.Content { return n.content }

// hold registers a session as resumable.
func (n *Node) hold(characterID uuid.UUID, s *Session) {
	n.holdMu.Lock()
	defer n.holdMu.Unlock()
	n.holding[characterID] = s
}

// forget stops holding a character for reconnection.
func (n *Node) forget(characterID uuid.UUID) {
	n.holdMu.Lock()
	defer n.holdMu.Unlock()
	delete(n.holding, characterID)
}

// held returns the session holding a character, if any.
func (n *Node) held(characterID uuid.UUID) (*Session, bool) {
	n.holdMu.Lock()
	defer n.holdMu.Unlock()
	s, ok := n.holding[characterID]
	return s, ok
}

// Holding reports how many characters are waiting out a reconnect window.
func (n *Node) Holding() int {
	n.holdMu.Lock()
	defer n.holdMu.Unlock()
	return len(n.holding)
}
