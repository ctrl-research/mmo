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

	// loot decides who may pick up what this layer drops, and lootCursor is
	// the round-robin position. Both are meaningless for a layer of one, which
	// is every layer until somebody parties up.
	loot       LootRule
	lootCursor int
}

// LootRule decides who may pick up a party's drops.
type LootRule uint8

const (
	// LootFreeForAll leaves every drop open to every member at once. The right
	// default: it is what a group of friends expects, and it needs no rules
	// to explain.
	LootFreeForAll LootRule = iota

	// LootRoundRobin assigns each drop to a member in turn, reserved for them
	// briefly before anyone may take it. Fairer over an evening, and worth
	// having for a party of strangers.
	LootRoundRobin
)

func (l LootRule) String() string {
	if l == LootRoundRobin {
		return "round-robin"
	}
	return "free-for-all"
}

// nextLooter returns the entity a round-robin drop belongs to.
//
// Cycling through the players actually present in the layer rather than the
// party roster, because a member in another room cannot pick anything up and
// assigning to them would be loot nobody can reach.
func (r *Room) nextLooter(layer LayerID, fallback EntityID) EntityID {
	l, ok := r.layers[layer]
	if !ok || l.loot != LootRoundRobin {
		return fallback
	}

	var present []EntityID
	for _, id := range r.playerOrder {
		if p := r.players[id]; p != nil && p.layer == layer && !p.frozen {
			present = append(present, id)
		}
	}
	if len(present) <= 1 {
		return fallback
	}

	// The cursor advances per drop rather than per kill, so a kill dropping
	// three items spreads them rather than giving all three to one member.
	l.lootCursor = (l.lootCursor + 1) % len(present)
	return present[l.lootCursor]
}

// setLootRule changes how a layer's drops are assigned.
func (r *Room) setLootRule(key string, rule LootRule) {
	id, ok := r.layerKeys[key]
	if !ok {
		return
	}
	if l, ok := r.layers[id]; ok {
		l.loot = rule
	}
}

// layerIDFor maps a layer key -- a party ID, or a character ID when unpartied
// -- to this room's internal numbering.
//
// Allocated sequentially rather than hashed. A 32-bit hash of a UUID collides
// rarely, and rarely is the wrong word for it: a collision merges two
// unrelated players' mob populations and loot, which is exactly the bug
// layering exists to make impossible. Allocation order follows join order,
// which is already part of the input log, so replay is unaffected.
func (r *Room) layerIDFor(key string) LayerID {
	if id, ok := r.layerKeys[key]; ok {
		return id
	}

	r.nextLayer++
	r.layerKeys[key] = r.nextLayer
	return r.nextLayer
}

// layerFor returns the layer a player belongs to, creating it if needed.
//
// The key is the party ID while partied and the character ID otherwise, so
// partying up merges views and leaving splits them. Same code path, different
// key.
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

	// An owner-layer point does not exist until somebody is there to own it,
	// so anything that gates spawn points has to be told about them here as
	// well as at startup. One left ungated produces its event's mobs
	// continuously, with no event.
	r.gateEventSpawns(l.spawns)

	r.layers[key] = l
	r.layerOrder = append(r.layerOrder, key)
	return l
}

// moveToLayer moves a player's hostile-entity layer.
//
// This is what partying up means inside a room: the members stop having a mob
// population each and start sharing one. Leaving splits it again. It is the
// same operation in both directions, because a layer key is just a key -- the
// party's ID while partied, the character's ID otherwise.
func (r *Room) moveToLayer(id EntityID, key string) {
	p, ok := r.players[id]
	if !ok || key == "" {
		return
	}

	to := r.layerIDFor(key)
	if p.layer == to || to == SharedLayer {
		return
	}

	from := p.layer

	// Claim the destination first. Releasing first would tear down the old
	// layer and, if the two happened to be the same, leave the player in a
	// layer that no longer exists.
	r.layerFor(to)

	p.layer = to
	p.entity.HuntLayer = to

	// Ground loot follows the player rather than being destroyed with the
	// layer they left. Mobs do not: the destination has its own population,
	// and moving them across would double it. Losing a drop because a friend
	// invited you to a party is the kind of thing players remember.
	if r.lastInLayer(from) {
		for _, e := range r.entities {
			if e.Layer == from && e.Drop != nil {
				e.Layer = to
			}
		}
	}

	r.releaseLayer(from)
}

// lastInLayer reports whether nobody else is using a layer.
func (r *Room) lastInLayer(key LayerID) bool {
	l, ok := r.layers[key]
	return ok && l.refs <= 1
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
