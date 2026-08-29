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

	seed  uint64
	grace time.Duration

	mu    sync.Mutex
	rooms map[directory.InstanceID]*hosted

	// holding tracks characters kept in the world through a reconnect grace
	// window, so a returning player resumes the character they left rather
	// than being told it is already in play by their own dropped session.
	holdMu  sync.Mutex
	holding map[uuid.UUID]*Session
}

type hosted struct {
	room   *room.Room
	handle room.Handle
}

// Config configures a Node.
type Config struct {
	Directory  directory.Directory
	Leases     directory.Leases
	Store      *store.Store
	NodeID     string
	Content    *content.Content
	DefaultMap string
	Logger     *slog.Logger
	Observer   room.Observer

	// Seed determines every roll every room on this node will make. Zero
	// draws a fresh one, which is right for a real server; a fixed value makes
	// a session reproducible, which is what tests and replay want.
	Seed uint64

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
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
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
		seed:       seed,
		grace:      grace,
		rooms:      make(map[directory.InstanceID]*hosted),
		holding:    make(map[uuid.UUID]*Session),
	}, nil
}

// Start begins hosting. Rooms are created lazily, when a player first needs
// one, so an empty world costs nothing.
func (n *Node) Start(ctx context.Context) {
	n.ctx, n.cancel = context.WithCancel(ctx)
}

// Stop tears down every room and waits for their goroutines to exit.
func (n *Node) Stop() {
	if n.cancel != nil {
		n.cancel()
	}
	n.wg.Wait()
}

// placeIn asks the directory for an instance of a map and ensures a room
// exists for it.
//
// This is the same flow M4 uses across many nodes: ask where the player
// belongs, then make sure the room is running. Today the answer is always
// local; when it is not, this returns a remote handle and nothing above it
// changes.
func (n *Node) placeIn(ctx context.Context, mapID string) (room.Handle, directory.InstanceID, error) {
	m, ok := n.content.Maps[mapID]
	if !ok {
		return nil, 0, fmt.Errorf("world: unknown map %q", mapID)
	}

	key := directory.RoomKey{
		MapID:     m.ID,
		Placement: directory.Placement(m.Placement),
	}

	inst, err := n.dir.Join(ctx, key, m.Capacity)
	if err != nil {
		return nil, 0, fmt.Errorf("world: placing player: %w", err)
	}

	handle, err := n.ensureRoom(inst, m)
	if err != nil {
		// Release the slot we reserved, or capacity leaks on every failure.
		_ = n.dir.Leave(ctx, inst.ID)
		return nil, 0, err
	}
	return handle, inst.ID, nil
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
	})

	h := &hosted{room: r, handle: room.NewHandle(r)}
	n.rooms[inst.ID] = h

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		_ = r.Run(n.ctx)

		n.mu.Lock()
		delete(n.rooms, inst.ID)
		n.mu.Unlock()

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
