package directory

import (
	"context"
	"sort"
	"sync"
)

// Memory is a Directory held in one process's memory.
//
// It is the hobby-scale implementation and the one tests use. Everything it
// holds is reconstructible: losing it costs players a disconnect, not data,
// which is the same property the Redis implementation will have.
type Memory struct {
	mu sync.Mutex

	// nodes is every world node available to host a room, in registration
	// order. Order is kept so that placement is deterministic when several
	// nodes are equally loaded, which is what makes a multi-node test
	// reproducible rather than flaky.
	nodes []NodeID

	instances map[InstanceID]*Instance
	byKey     map[RoomKey][]InstanceID
	nextID    InstanceID
}

// NewMemory returns an empty directory that places rooms on node.
//
// More nodes can be added with AddNode. One is the hobby-scale case; the
// interesting one is two, because that is what proves a transfer between world
// roles goes over the bus rather than through a local shortcut.
func NewMemory(node NodeID) *Memory {
	return &Memory{
		nodes:     []NodeID{node},
		instances: make(map[InstanceID]*Instance),
		byKey:     make(map[RoomKey][]InstanceID),
	}
}

// AddNode registers another world node as a placement target.
//
// Idempotent, because a node that restarts and re-registers must not be
// counted twice and given half the world.
func (m *Memory) AddNode(node NodeID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, existing := range m.nodes {
		if existing == node {
			return
		}
	}
	m.nodes = append(m.nodes, node)
}

// Nodes returns the registered nodes in registration order.
func (m *Memory) Nodes() []NodeID {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]NodeID(nil), m.nodes...)
}

// placeLocked picks the node to host a new instance.
//
// Fewest rooms wins, ties broken by registration order. Counting rooms rather
// than players is deliberate: a room costs a goroutine and a tick loop whether
// or not anyone is in it, and an empty room on an overloaded node still burns
// its share of the budget.
func (m *Memory) placeLocked() NodeID {
	load := make(map[NodeID]int, len(m.nodes))
	for _, inst := range m.instances {
		load[inst.Node]++
	}

	best := m.nodes[0]
	for _, node := range m.nodes[1:] {
		if load[node] < load[best] {
			best = node
		}
	}
	return best
}

// Join reserves a slot, creating an instance if necessary.
func (m *Memory) Join(_ context.Context, key RoomKey, capacity int) (Instance, error) {
	if !key.Valid() {
		return Instance{}, &KeyError{Key: key}
	}
	if capacity <= 0 {
		return Instance{}, &CapacityError{Capacity: capacity}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if inst := m.leastFullLocked(key); inst != nil {
		inst.Players++
		return *inst, nil
	}

	// A private room is a single instance by definition: if the one that
	// exists is full, the party is full, and creating a second would split the
	// group across two dungeons.
	if key.Placement == PlacementPrivate && len(m.byKey[key]) > 0 {
		return Instance{}, ErrNoCapacity
	}

	return *m.createLocked(key, capacity), nil
}

// createLocked adds an instance with one slot reserved.
func (m *Memory) createLocked(key RoomKey, capacity int) *Instance {
	m.nextID++
	inst := &Instance{
		ID:       m.nextID,
		Node:     m.placeLocked(),
		Key:      key,
		Players:  1,
		Capacity: capacity,
	}
	m.instances[inst.ID] = inst
	m.byKey[key] = append(m.byKey[key], inst.ID)
	return inst
}

// NewInstance creates an additional instance for a key.
func (m *Memory) NewInstance(_ context.Context, key RoomKey, capacity int) (Instance, error) {
	if !key.Valid() {
		return Instance{}, &KeyError{Key: key}
	}
	if capacity <= 0 {
		return Instance{}, &CapacityError{Capacity: capacity}
	}
	if key.Placement == PlacementPrivate {
		// One instance per owner is what private means. A second would split
		// a party across two dungeons.
		return Instance{}, ErrNoCapacity
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.createLocked(key, capacity), nil
}

// JoinInstance reserves a slot in one named instance.
func (m *Memory) JoinInstance(_ context.Context, id InstanceID) (Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[id]
	if !ok {
		return Instance{}, ErrUnknownInstance
	}
	if inst.Full() {
		return Instance{}, ErrNoCapacity
	}
	inst.Players++
	return *inst, nil
}

// leastFullLocked returns the emptiest instance for key that still has room,
// or nil if there is none.
//
// Filling the emptiest rather than the fullest spreads players across channels
// instead of packing them, which keeps any one room's tick cost down. That
// matters more than it would in a conventional MMO: under per-player mob
// layering, simulation cost scales with the number of distinct layers in a
// room, so a packed room is disproportionately expensive.
func (m *Memory) leastFullLocked(key RoomKey) *Instance {
	var best *Instance
	for _, id := range m.byKey[key] {
		inst := m.instances[id]
		if inst == nil || inst.Full() {
			continue
		}
		if best == nil || inst.Players < best.Players || (inst.Players == best.Players && inst.ID < best.ID) {
			best = inst
		}
	}
	return best
}

// Leave releases one reserved slot.
func (m *Memory) Leave(_ context.Context, id InstanceID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[id]
	if !ok {
		return ErrUnknownInstance
	}
	if inst.Players > 0 {
		inst.Players--
	}
	return nil
}

// Release removes an instance entirely.
func (m *Memory) Release(_ context.Context, id InstanceID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[id]
	if !ok {
		return ErrUnknownInstance
	}
	m.releaseLocked(inst)
	return nil
}

func (m *Memory) releaseLocked(inst *Instance) {
	delete(m.instances, inst.ID)

	ids := m.byKey[inst.Key]
	for i, other := range ids {
		if other == inst.ID {
			m.byKey[inst.Key] = append(ids[:i:i], ids[i+1:]...)
			break
		}
	}
	if len(m.byKey[inst.Key]) == 0 {
		delete(m.byKey, inst.Key)
	}
}

// InstancesFor returns every live instance satisfying a key, ordered by ID.
//
// This is the channel list: for a shared map the entries are the channels a
// player may switch between, and their occupancy is what makes one worth
// choosing over another.
func (m *Memory) InstancesFor(_ context.Context, key RoomKey) ([]Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := m.byKey[key]
	out := make([]Instance, 0, len(ids))
	for _, id := range ids {
		if inst, ok := m.instances[id]; ok {
			out = append(out, *inst)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// TryRelease removes an instance only if it is unoccupied.
func (m *Memory) TryRelease(_ context.Context, id InstanceID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[id]
	if !ok {
		// Already gone, which is the outcome the caller wanted.
		return true, nil
	}
	if inst.Players > 0 {
		return false, nil
	}
	m.releaseLocked(inst)
	return true, nil
}

// Lookup returns one instance by ID.
func (m *Memory) Lookup(_ context.Context, id InstanceID) (Instance, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[id]
	if !ok {
		return Instance{}, false, nil
	}
	return *inst, true, nil
}

// List returns every live instance ordered by ID.
func (m *Memory) List(_ context.Context) ([]Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, *inst)
	}
	// Map iteration order is random; callers get a stable sequence so that
	// metrics and tests do not depend on hash ordering.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Close releases nothing; the method exists to satisfy Directory.
func (m *Memory) Close() error { return nil }

// KeyError reports a malformed RoomKey.
type KeyError struct{ Key RoomKey }

func (e *KeyError) Error() string { return "directory: invalid room key " + e.Key.String() }

// CapacityError reports a non-positive capacity, which would create an
// instance nobody can ever join.
type CapacityError struct{ Capacity int }

func (e *CapacityError) Error() string { return "directory: capacity must be positive" }

// Compile-time check that Memory satisfies the interface.
var _ Directory = (*Memory)(nil)
