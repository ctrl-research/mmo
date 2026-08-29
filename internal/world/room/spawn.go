package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/rng"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// spawnState tracks one spawn point's population and timer.
//
// Each spawn point owns its own nextSpawn tick rather than the room running a
// shared schedule. That is MapleStory and OSRS behaviour, and it falls out of
// storing the interval per object -- there is no wave logic to write.
type spawnState struct {
	def   *content.MobSpawn
	layer LayerID

	alive int

	// nextSpawn is the tick at which another mob may appear. Zero means
	// immediately, which is what makes a room populate on creation rather than
	// staying empty for one respawn interval.
	nextSpawn uint64
}

func newSpawnState(def *content.MobSpawn, layer LayerID) *spawnState {
	return &spawnState{def: def, layer: layer}
}

// cullInterval is how often distant mobs are swept up.
//
// Not every tick: the sweep is O(mobs x players) and nothing about it is
// urgent. Once a second is far more often than a player can cross the
// activation range.
const cullInterval = TickRate

// phaseSpawns tops up every spawn point that is below its cap and off cooldown.
func (r *Room) phaseSpawns() {
	if r.tick%cullInterval == 0 {
		r.cullDistantMobs()
	}

	// Shared-layer spawns exist once for the whole room: the field boss
	// everyone fights, which is the counterweight to per-player mobs.
	for _, sp := range r.sharedSpawns {
		r.serviceSpawn(sp, r.rand)
	}

	for _, l := range r.activeLayers() {
		for _, sp := range l.spawns {
			r.serviceSpawn(sp, l.rand)
		}
	}
}

func (r *Room) serviceSpawn(sp *spawnState, source *rng.Source) {
	if sp.alive >= sp.def.MaxAlive || r.tick < sp.nextSpawn {
		return
	}
	if !r.spawnIsNeeded(sp) {
		// Nobody who could see this mob is anywhere near it. The timer is not
		// reset, so walking into an area finds it already populated rather
		// than empty for one respawn interval.
		return
	}

	def, ok := r.content.Mobs[sp.def.MobID]
	if !ok {
		// Content is verified at load, so this cannot happen without a bug in
		// the loader. Skipping quietly beats panicking inside a tick loop that
		// other players are relying on.
		r.log.Error("spawn references an unknown mob", "mob", sp.def.MobID)
		return
	}

	r.spawnMob(def, sp, source)
	sp.alive++

	// The timer restarts per spawn rather than per wave, so a cap of three
	// trickles in one at a time instead of all three at once.
	sp.nextSpawn = r.tick + uint64(sp.def.RespawnTicks)
}

// cullDistantMobs removes mobs nobody is near.
//
// This is the other half of proximity spawning, and without it the saving is
// only ever for ground a player has not walked yet: cross a map once and every
// spawn point on it is populated for as long as the room lives.
//
// Only idle and leashing mobs are eligible. A mob that is chasing or attacking
// is by definition next to a player, and one that has just been hit and is
// walking home must not evaporate while its attacker watches.
func (r *Room) cullDistantMobs() {
	reach := r.activationRange()
	if reach <= 0 {
		return
	}

	var cull []EntityID
	for _, e := range r.entities {
		m := e.Mob
		if m == nil || m.State != aiIdle && m.State != aiLeash {
			continue
		}
		if r.someoneNear(e.Layer, e.Body.FeetCenter(), reach) {
			continue
		}
		cull = append(cull, e.ID)
	}

	for _, id := range cull {
		e := r.entity(id)
		if e == nil {
			continue
		}
		if sp := e.Mob.Spawn; sp != nil {
			sp.release()
			// Cleared rather than left running: the population was removed
			// for the engine's benefit, not by a player, so returning to the
			// area should find it repopulated rather than on a respawn timer
			// nobody triggered.
			sp.nextSpawn = 0
		}
		r.removeEntity(id)
	}
}

// spawnIsNeeded reports whether anyone who could see this spawn point is
// close enough for it to be worth populating.
//
// Under per-player layering a room holds roughly layers x mobs entities, and
// most of them are in parts of the map their owner is not in. Gating on
// distance is the difference between a layer costing one map's worth of mobs
// and costing one screen's worth, and it is the main thing that keeps the tick
// budget reachable as capacity rises.
func (r *Room) spawnIsNeeded(sp *spawnState) bool {
	reach := r.activationRange()
	if reach <= 0 {
		return true
	}
	return r.someoneNear(sp.layer, sp.def.At, reach)
}

// someoneNear reports whether a player who can see a layer is within reach of
// a point.
//
// A shared-layer entity is visible to everyone; an owner-layer one only to the
// layer it belongs to, so a player standing next to somebody else's mob must
// not keep it alive.
func (r *Room) someoneNear(layer LayerID, at sim.Vec, reach fixed.F) bool {
	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.frozen {
			continue
		}
		if layer != SharedLayer && p.layer != layer {
			continue
		}
		feet := p.entity.Body.FeetCenter()
		if (feet.X-at.X).Abs() <= reach && (feet.Y-at.Y).Abs() <= reach {
			return true
		}
	}
	return false
}

// activationRange is how close a player must be for a spawn point to produce
// mobs, from content.
func (r *Room) activationRange() fixed.F {
	if r.content == nil {
		return 0
	}
	return r.content.Balance.Rooms.SpawnActivationRange
}

// spawnMob places one mob at its spawn point.
func (r *Room) spawnMob(def *content.Mob, sp *spawnState, source *rng.Source) *Entity {
	at := sp.def.At

	// Scatter within the spawn radius so a cap of three is three mobs to fight
	// rather than one unhittable column of overlapping boxes.
	if sp.def.Radius > 0 {
		offset := source.Range(-sp.def.Radius.Int(), sp.def.Radius.Int())
		at.X += fixed.FromInt(offset)
	}

	body := sim.NewBody(at, def.Width, def.Height)
	// Settle so a mob spawned on a platform is grounded on its first tick
	// rather than spending one tick believing it is falling.
	tuning := r.tuningFor(def)
	sim.Settle(&body, r.cfg.World, &tuning)

	e := r.spawnEntity(&Entity{
		Kind:  KindMob,
		Layer: sp.layer,
		Body:  body,
		HP:    uint32(def.HP),
		MaxHP: uint32(def.HP),
		Name:  def.Name,
		Mob: &MobState{
			Def:   def,
			Home:  at,
			State: aiIdle,
			Spawn: sp,
		},
	})
	return e
}

// tuningFor gives a mob its own movement constants.
//
// Mobs run the same physics as players -- gravity, platforms, collision -- so
// they behave correctly on the map geometry without a second movement
// implementation. Only their speed differs, so the tuning is the player's with
// RunSpeed replaced.
func (r *Room) tuningFor(def *content.Mob) sim.Tuning {
	t := r.cfg.Tuning
	t.RunSpeed = def.AI.MoveSpeed
	// Mobs accelerate to their speed quickly; a slow ramp reads as sliding.
	t.GroundAccel = def.AI.MoveSpeed
	t.AirAccel = def.AI.MoveSpeed
	return t
}

// releaseSpawn frees a slot when a mob is removed, and starts the timer if the
// mob was killed rather than despawned.
//
// The timer starts on death rather than on removal so the corpse's linger time
// does not silently extend every respawn in the game.
func (sp *spawnState) release() {
	if sp.alive > 0 {
		sp.alive--
	}
}
