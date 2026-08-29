package world

import (
	"sync"

	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/world/room"
)

// Registry maps room instances to handles within one process.
//
// It exists so that two world nodes running in one process can reach each
// other's rooms, which is what makes "transferred to a room hosted by a
// different world role" a real claim rather than a nominal one -- and what
// lets the transfer protocol be exercised end to end before there are
// genuinely separate processes.
//
// Across processes this is not enough: a handle there means routing every
// command over the bus, which is M9. Until then, resolving an instance that is
// not in this process fails loudly rather than silently doing something else.
type Registry struct {
	mu      sync.RWMutex
	handles map[directory.InstanceID]room.Handle
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{handles: make(map[directory.InstanceID]room.Handle)}
}

// Register records a room's handle.
func (r *Registry) Register(id directory.InstanceID, h room.Handle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handles[id] = h
}

// Unregister removes a room, once it has been torn down.
func (r *Registry) Unregister(id directory.InstanceID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.handles, id)
}

// Lookup returns a room's handle.
func (r *Registry) Lookup(id directory.InstanceID) (room.Handle, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handles[id]
	return h, ok
}

// Len reports how many rooms are registered, for metrics and tests.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.handles)
}
