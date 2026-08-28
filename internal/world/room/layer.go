package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/rng"
)

// layerState is one visibility layer's private world.
//
// Every player in a hunting zone gets their own layer, so they share the room
// -- and therefore see each other, chat, and party -- while hunting mobs that
// nobody else can touch. That removes spawn camping, kill stealing, and loot
// sniping by making them unrepresentable rather than by adding rules against
// them (docs/architecture.md).
//
// The cost is that simulation scales with layers x mobs rather than mobs, and
// that is the main pressure on the tick budget. It is why idle mobs tick on a
// slower beat and why room capacity is lower than it would otherwise be.
type layerState struct {
	id LayerID

	// refs counts the players currently using this layer. A layer with no
	// players still exists briefly -- long enough for its mobs to be cleaned
	// up in an orderly way rather than vanishing mid-tick.
	refs int

	// spawns holds this layer's private copy of every owner-layer spawn point,
	// each with its own independent timer. This is what "no contention" means
	// concretely: your slime respawning has nothing to do with anyone else's.
	spawns []*spawnState

	// rand is an independent stream, so one player's drop luck never depends
	// on how many other players happen to share the room.
	rand *rng.Source
}

// layerFor returns the layer a player belongs to, creating it if needed.
//
// From M5 the key is the party ID, so partying up merges views. Until parties
// exist, every player gets their own -- which is the same code path with a
// different key, not a placeholder to be replaced.
func (r *Room) layerFor(key LayerID) *layerState {
	if l, ok := r.layers[key]; ok {
		l.refs++
		return l
	}

	l := &layerState{
		id:   key,
		refs: 1,
		// Derived from the room seed and the layer key so a replay reproduces
		// each layer's rolls exactly, regardless of join order.
		rand: rng.NewStream(r.cfg.Seed, uint64(key)),
	}

	for i := range r.mapDef.MobSpawns {
		sp := &r.mapDef.MobSpawns[i]
		if sp.Layer != content.LayerOwner {
			continue
		}
		l.spawns = append(l.spawns, newSpawnState(sp, key))
	}

	r.layers[key] = l
	r.layerOrder = append(r.layerOrder, key)
	return l
}

// releaseLayer drops a player's claim on a layer and tears it down when the
// last one leaves.
func (r *Room) releaseLayer(key LayerID) {
	l, ok := r.layers[key]
	if !ok {
		return
	}

	l.refs--
	if l.refs > 0 {
		return
	}

	// Remove every entity that belonged to the layer. Leaving them would mean
	// simulating mobs and drops nobody can see -- pure cost, and a slow leak
	// in a long-lived room.
	for _, e := range r.entitiesInLayer(key) {
		r.removeEntity(e)
	}

	delete(r.layers, key)
	for i, id := range r.layerOrder {
		if id == key {
			r.layerOrder = append(r.layerOrder[:i:i], r.layerOrder[i+1:]...)
			break
		}
	}
}

// entitiesInLayer collects the IDs of every entity in a layer.
//
// Returns IDs rather than pointers because the caller is usually about to
// remove them, and removal reorders the entity slice underneath a pointer
// walk.
func (r *Room) entitiesInLayer(key LayerID) []EntityID {
	var out []EntityID
	for _, e := range r.entities {
		if e.Layer == key && e.Kind != KindPlayer {
			out = append(out, e.ID)
		}
	}
	return out
}

// activeLayers returns every layer in deterministic order.
//
// Iterating layerOrder rather than the map is not a style choice: Go
// randomises map iteration, and spawning in a different order across two runs
// of the same seed would break replay.
func (r *Room) activeLayers() []*layerState {
	out := make([]*layerState, 0, len(r.layerOrder))
	for _, id := range r.layerOrder {
		if l, ok := r.layers[id]; ok {
			out = append(out, l)
		}
	}
	return out
}

// visibleTo reports whether an entity is visible to a viewer's layer.
//
// Correctness rather than optimisation: a bug here leaks another player's mobs
// and loot into a client's view, and leaked loot is the kind of bug that
// becomes an economy problem.
func visibleTo(viewerLayer LayerID, e *Entity) bool {
	return e.Layer == SharedLayer || e.Layer == viewerLayer
}
