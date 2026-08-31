package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Running zone events.
//
// An event borrows the same gate the dungeon stages use: its spawn points
// produce nothing until it starts, and stop again when it ends. That is the
// whole mechanism, and it is why an event needs no separate population
// tracking, no wave scheduler, and no new way for a mob to exist.
//
// What is left over when an event ends is cleared. An event that left its mobs
// behind would be indistinguishable from the zone simply getting harder every
// time one ran, which is how a map ends up unplayable by the evening.

// eventState is one event's clock on one room.
type eventState struct {
	def *content.Event

	// active is whether it is running, and until when.
	active  bool
	endsAt  uint64
	readyAt uint64
}

// startEvents prepares every event that happens on this room's map.
func (r *Room) startEvents() {
	for _, def := range r.content.EventsForMap(r.cfg.MapID) {
		state := &eventState{def: def}

		// A timed event waits out one full period before its first run, so a
		// room that has just started is not immediately in the middle of
		// something nobody chose to be in.
		if def.Trigger == content.TriggerTimer {
			state.readyAt = r.tick + uint64(def.EveryTicks)
		}
		r.events = append(r.events, state)
	}

	r.gateEventSpawns(r.allSpawns())
	r.spawnShrines()
}

// gateEventSpawns shuts every spawn point an event owns.
//
// Called for a layer's points as the layer is created, not only at startup: an
// owner-layer point does not exist until somebody is there to own it, and one
// created after the fact would have been left ungated -- producing the event's
// mobs continuously, with no event.
func (r *Room) gateEventSpawns(spawns []*spawnState) {
	if r.content == nil {
		return
	}
	for _, sp := range spawns {
		ev := r.content.EventForSpawn(r.cfg.MapID, sp.def.Name)
		if ev == nil {
			continue
		}
		sp.gated = true
		sp.event = ev.ID

		// Already running: a player who arrives mid-event joins the event
		// rather than an empty version of the zone.
		for _, state := range r.events {
			if state.def.ID == ev.ID && state.active {
				sp.open = true
			}
		}
	}
}

// spawnShrines puts a marker where each of the map's shrines is.
//
// An entity rather than only a rectangle: a trigger the player cannot see is a
// trigger they step into by accident, and an event nobody chose to start is
// not the point of having a shrine.
func (r *Room) spawnShrines() {
	if r.mapDef == nil {
		return
	}
	for i := range r.mapDef.Shrines {
		s := &r.mapDef.Shrines[i]
		ev := r.content.EventForShrine(r.cfg.MapID, s.Name)
		if ev == nil {
			continue
		}
		r.spawnEntity(&Entity{
			Kind:  KindShrine,
			Layer: SharedLayer,
			Body:  sim.Body{Pos: sim.Vec{X: s.Bounds.X, Y: s.Bounds.Y}, W: s.Bounds.W, H: s.Bounds.H},
			Name:  ev.Name,
		})
	}
}

// phaseEvents starts and ends events.
func (r *Room) phaseEvents() {
	for _, state := range r.events {
		switch {
		case state.active && r.tick >= state.endsAt:
			r.endEvent(state)

		case !state.active && state.def.Trigger == content.TriggerTimer &&
			r.tick >= state.readyAt:
			// Only with somebody there to see it. A timed event that ran in an
			// empty room would burn its cooldown on nobody, and a player
			// walking in would find the aftermath rather than the event.
			if len(r.playerOrder) > 0 {
				r.beginEvent(state)
			}
		}
	}
}

// touchShrine starts a shrine's event, if it is ready.
func (r *Room) touchShrine(e *Entity, shrine string) {
	for _, state := range r.events {
		if state.def.Trigger != content.TriggerShrine || state.def.Shrine != shrine {
			continue
		}
		if state.active || r.tick < state.readyAt {
			// Already running, or still cooling down. Silent: a shrine that
			// complained every tick somebody stood on it would be worse than
			// one that simply does nothing yet.
			return
		}
		r.beginEvent(state)
		r.log.Info("shrine touched", "event", state.def.ID, "by", e.Name)
		return
	}
}

func (r *Room) beginEvent(state *eventState) {
	state.active = true
	state.endsAt = r.tick + uint64(state.def.DurationTicks)

	for _, sp := range r.eventSpawns(state.def.ID) {
		sp.open = true
		// Reset, so a second run produces a full wave rather than remembering
		// that the first one already made its quota.
		sp.spawned = 0
		sp.nextSpawn = r.tick
	}

	r.announceEvent(state, true)
	r.log.Info("zone event began", "event", state.def.ID, "for", state.def.DurationTicks)
}

func (r *Room) endEvent(state *eventState) {
	state.active = false

	// One knob per trigger: a timed event has a period, a shrine has a
	// cooldown. Measured from the end rather than the start, so a long event
	// does not spend its own wait running.
	wait := state.def.CooldownTicks
	if state.def.Trigger == content.TriggerTimer {
		wait = state.def.EveryTicks
	}
	state.readyAt = r.tick + uint64(wait)

	var leftovers []EntityID
	for _, sp := range r.eventSpawns(state.def.ID) {
		sp.open = false
		for _, e := range r.entities {
			if e.Mob != nil && e.Mob.Spawn == sp {
				leftovers = append(leftovers, e.ID)
			}
		}
	}

	// Cleared rather than left standing. Mobs from an event that has finished
	// are indistinguishable from the zone simply getting harder every time one
	// runs, which is how a map ends up unplayable by the evening.
	for _, id := range leftovers {
		if e := r.entity(id); e != nil && e.Mob.Spawn != nil {
			e.Mob.Spawn.release()
		}
		r.removeEntity(id)
	}

	r.announceEvent(state, false)
	r.log.Info("zone event ended", "event", state.def.ID, "cleared", len(leftovers))
}

// eventSpawns returns the spawn points belonging to an event.
func (r *Room) eventSpawns(id string) []*spawnState {
	var out []*spawnState
	for _, sp := range r.allSpawns() {
		if sp.event == id {
			out = append(out, sp)
		}
	}
	return out
}

// announceEvent tells the room what is happening.
func (r *Room) announceEvent(state *eventState, starting bool) {
	ev := &mmov1.ZoneEvent{
		EventId: state.def.ID,
		Name:    state.def.Name,
		Active:  starting,
	}
	if starting {
		ev.Message = state.def.Announce
		ev.EndsInMs = uint32(state.def.DurationTicks * 1000 / TickRate)
	}

	r.emit(&mmov1.Event{Body: &mmov1.Event_Zone{Zone: ev}}, SharedLayer)
}
