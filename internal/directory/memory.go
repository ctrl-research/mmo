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
	node NodeID

	mu        sync.Mutex
	instances map[InstanceID]*Instance
	byKey     map[RoomKey][]InstanceID
	nextID    InstanceID
}

// NewMemory returns an empty directory that places every room on node.
func NewMemory(node NodeID) *Memory {
	return &Memory{
		node:      node,
		instances: make(map[InstanceID]*Instance),
		byKey:     make(map[RoomKey][]InstanceID),
	}
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

	m.nextID++
	inst := &Instance{
		ID:       m.nextID,
		Node:     m.node,
		Key:      key,
		Players:  1,
		Capacity: capacity,
	}
	m.instances[inst.ID] = inst
	m.byKey[key] = append(m.byKey[key], inst.ID)
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
	delete(m.instances, id)

	ids := m.byKey[inst.Key]
	for i, other := range ids {
		if other == id {
			m.byKey[inst.Key] = append(ids[:i:i], ids[i+1:]...)
			break
		}
	}
	if len(m.byKey[inst.Key]) == 0 {
		delete(m.byKey, inst.Key)
	}
	return nil
}

// Lookup returns one instance by ID.
func (m *Memory) Lookup(_ context.Context, id InstanceID) (Instance, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[id]
	if !ok {
		return Instance{}, false
	}
	return *inst, true
}

// List returns every live instance ordered by ID.
func (m *Memory) List(_ context.Context) []Instance {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, *inst)
	}
	// Map iteration order is random; callers get a stable sequence so that
	// metrics and tests do not depend on hash ordering.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
