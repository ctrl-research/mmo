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
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Node hosts room instances.
type Node struct {
	dir      directory.Directory
	content  *content.Content
	log      *slog.Logger
	observer room.Observer

	// defaultMap is where a player with no saved location starts. Characters
	// remember their own map from M2 onward.
	defaultMap string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	seed uint64

	mu    sync.Mutex
	rooms map[directory.InstanceID]*hosted
}

type hosted struct {
	room   *room.Room
	handle room.Handle
}

// Config configures a Node.
type Config struct {
	Directory  directory.Directory
	Content    *content.Content
	DefaultMap string
	Logger     *slog.Logger
	Observer   room.Observer

	// Seed determines every roll every room on this node will make. Zero
	// draws a fresh one, which is right for a real server; a fixed value makes
	// a session reproducible, which is what tests and replay want.
	Seed uint64
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

	seed := cfg.Seed
	if seed == 0 {
		// A fresh seed per process, so two servers started from the same image
		// do not produce identical drops. Read once at startup rather than
		// inside a tick, where a non-deterministic source would break replay.
		seed = uint64(time.Now().UnixNano())
	}

	return &Node{
		dir:        cfg.Directory,
		content:    cfg.Content,
		defaultMap: cfg.DefaultMap,
		log:        cfg.Logger,
		observer:   cfg.Observer,
		seed:       seed,
		rooms:      make(map[directory.InstanceID]*hosted),
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

// Handle places a connecting player and returns a handle to their room,
// satisfying gateway.RoomProvider.
//
// The flow is the same one M4 uses across many nodes: ask the directory for an
// instance, then ensure a room exists for it. Today the answer is always local;
// when it is not, this returns a remote handle and nothing above it changes.
func (n *Node) Handle(ctx context.Context) (room.Handle, error) {
	m := n.content.Maps[n.defaultMap]

	key := directory.RoomKey{
		MapID:     m.ID,
		Placement: directory.Placement(m.Placement),
	}

	inst, err := n.dir.Join(ctx, key, m.Capacity)
	if err != nil {
		return nil, fmt.Errorf("world: placing player: %w", err)
	}

	h, err := n.ensureRoom(inst, m)
	if err != nil {
		// Release the slot we reserved, or capacity leaks on every failure.
		_ = n.dir.Leave(ctx, inst.ID)
		return nil, err
	}

	// Wrap so that a player leaving the room also frees their directory slot.
	// Without this the directory believes rooms are fuller than they are and
	// eventually opens channels nobody needs.
	return &trackedHandle{Handle: h, dir: n.dir, instance: inst.ID}, nil
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

// trackedHandle frees a directory slot when its player leaves.
type trackedHandle struct {
	room.Handle
	dir      directory.Directory
	instance directory.InstanceID
}

func (h *trackedHandle) Leave(ctx context.Context, id room.EntityID) {
	h.Handle.Leave(ctx, id)
	_ = h.dir.Leave(ctx, h.instance)
}

// Content returns the loaded game data this node simulates.
func (n *Node) Content() *content.Content { return n.content }
