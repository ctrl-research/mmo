package room

import (
	"strconv"

	"github.com/ctrl-research/mmo/internal/content"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Crafting: the room's half.
//
// The same split as gathering, for the same reason -- the room owns the clock
// and the "are you still standing here" state, and the session owns anything
// durable. But the two halves divide differently, and the difference is worth
// naming because it decides the whole shape of this file.
//
// Gathering *produces* from nothing: the roll succeeds or it does not, the
// experience is earned the moment it does, and the item is a consequence the
// session stores afterwards. Nothing can fail once the roll has landed.
//
// Crafting *spends*. The materials are in the inventory, which the room cannot
// see and must not guess at, and consuming them is a database write that can
// legitimately come back "you do not have those any more". So a run completes
// in two steps: the room asks, the session consumes and produces atomically,
// and the room grants the experience only when it hears back. A room that
// granted experience on asking would pay a smith for bars they never made.

// craftState is one player's crafting run.
type craftState struct {
	// station is the entity being used, and recipe what is being made. Both
	// zero when idle.
	station EntityID
	recipe  *content.Recipe

	// startedAt is the action tick the current run began on. A run takes
	// several beats, and this is what makes a longer recipe actually longer
	// rather than a faster one with a different label.
	startedAt uint64

	// pending marks a run whose consume-and-produce is in flight with the
	// session. Another run must not start while one is: the second would ask
	// for materials the first has already claimed, and both would be told yes.
	pending bool
}

// startStations creates the station entities on the map.
//
// Always shared-layer and with no timers: a station has nothing to run out of,
// so there is nothing for two players at one anvil to contend over. That is
// what makes it cheaper than a resource node rather than a special case.
func (r *Room) startStations() {
	if r.mapDef == nil {
		return
	}
	for i := range r.mapDef.Stations {
		spot := &r.mapDef.Stations[i]
		def, ok := r.content.Stations[spot.StationID]
		if !ok {
			// Content is verified at load, so this cannot happen without a bug
			// in the loader.
			r.log.Error("map places an unknown station", "station", spot.StationID)
			continue
		}
		r.spawnEntity(&Entity{
			Kind:  KindStation,
			Layer: SharedLayer,
			Body: sim.Body{
				Pos: sim.Vec{X: spot.Bounds.X, Y: spot.Bounds.Y},
				W:   spot.Bounds.W,
				H:   spot.Bounds.H,
			},
			Name:    def.Name,
			Station: &StationEntity{ID: def.ID},
		})
	}
}

// StationEntity is the station half of a placement: the content id, so the
// client knows which one it walked up to without matching on a display name.
type StationEntity struct {
	ID string
}

// stationMenu answers "what can I make here".
//
// Answered on request rather than pushed, because it depends on the
// character's level and on what is in their bag. The bag is the session's, so
// the room asks it -- this is the one question in the file the room cannot
// answer alone and does not pretend to.
func (r *Room) stationMenu(p *player, stationID EntityID) {
	e := r.entity(stationID)
	if e == nil || e.Station == nil || p.entity.Player == nil {
		return
	}
	if !r.withinStationRange(p.entity, e) {
		r.refuseCraft(p.entity.ID, "you are too far away")
		return
	}
	if p.events == nil {
		return
	}

	def := r.content.Stations[e.Station.ID]
	if def == nil {
		return
	}

	// Levels are the room's, materials are the session's. Sending the levels
	// with the request saves a round trip back for something the room already
	// knows and the session would have to ask for.
	recipes := r.content.RecipesAt(def.ID)
	levels := make(map[string]int, len(recipes))
	for _, rec := range recipes {
		levels[rec.Skill] = r.secondaryLevel(p.entity.Player, rec.Skill)
	}

	p.events.OpenStation(StationRequest{
		Player:      p.entity.ID,
		CharacterID: p.characterID,
		Station:     def,
		Entity:      stationID,
		Levels:      levels,
	})
}

// beginCraft is a player asking to make something.
//
// Every reason it can fail is checked here except the one the room cannot see:
// whether the materials are there. That one is answered by the first run, and
// its answer stops the action with a reason rather than being pre-empted by a
// guess the room would have to keep in step with the inventory.
func (r *Room) beginCraft(p *player, stationID EntityID, recipeID string) {
	if recipeID == "" {
		r.stopCraft(p, "")
		return
	}

	e := p.entity
	station := r.entity(stationID)
	if station == nil || station.Station == nil || e.Player == nil {
		return
	}
	if isDowned(e) {
		return
	}
	if !r.withinStationRange(e, station) {
		r.refuseCraft(e.ID, "you are too far away")
		return
	}

	rec, ok := r.content.Recipes[recipeID]
	if !ok {
		return
	}
	if rec.Station != station.Station.ID {
		// A recipe asked for at the wrong station. A client can send any pair,
		// so this is a real request to refuse rather than one that cannot
		// happen -- and saying which station is what makes it actionable.
		if def := r.content.Stations[rec.Station]; def != nil {
			r.refuseCraft(e.ID, "that is made at "+aOrAn(def.Name))
		}
		return
	}

	skill, ok := r.content.Secondary[rec.Skill]
	if !ok {
		return
	}
	if level := r.secondaryLevel(e.Player, rec.Skill); level < rec.Level {
		r.refuseCraft(e.ID, "you need "+skill.Name+" level "+strconv.Itoa(rec.Level))
		return
	}

	// Already making this. A repeated request is what re-clicking the same
	// recipe produces, and restarting would throw away the progress of the run
	// in flight.
	if p.craft.station == stationID && p.craft.recipe == rec {
		return
	}

	// One action at a time; see the matching note in beginGather.
	r.stopGather(p, "")

	p.craft = craftState{station: stationID, recipe: rec, startedAt: r.actionTick()}
	r.emitTo(e.ID, &mmov1.Event{Body: &mmov1.Event_Crafting{Crafting: &mmov1.Crafting{
		RecipeId: rec.ID,
		Name:     rec.Name,
		Skill:    rec.Skill,
		Active:   true,
	}}})
}

// stopCraft ends a run, saying why when there is a reason worth hearing.
func (r *Room) stopCraft(p *player, reason string) {
	if p.craft.station == 0 {
		return
	}
	recipe := p.craft.recipe
	p.craft = craftState{}

	msg := &mmov1.Crafting{Active: false, Reason: reason}
	if recipe != nil {
		msg.RecipeId = recipe.ID
		msg.Name = recipe.Name
		msg.Skill = recipe.Skill
	}
	r.emitTo(p.entity.ID, &mmov1.Event{Body: &mmov1.Event_Crafting{Crafting: msg}})
}

func (r *Room) refuseCraft(id EntityID, reason string) {
	r.emitTo(id, &mmov1.Event{Body: &mmov1.Event_Crafting{Crafting: &mmov1.Crafting{
		Active: false,
		Reason: reason,
	}}})
}

// actionTick is how many action ticks the room has run.
//
// Derived rather than counted, so it cannot drift from the simulation tick it
// is derived from -- which is the entire reason the action tick is a division
// rather than a second clock.
func (r *Room) actionTick() uint64 { return r.tick / content.ActionTicks }

// phaseCrafting advances every player's crafting run.
//
// Called from phaseActions alongside gathering, so the two share one beat and
// one set of interruption rules. A player cannot do both at once, because both
// live in the same "what is this character doing" slot -- see startAction.
func (r *Room) phaseCrafting() {
	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.craft.station == 0 {
			continue
		}
		if reason, stop := r.craftInterrupted(p); stop {
			r.stopCraft(p, reason)
		}
	}

	// A short-circuit, not the thing that makes the timing right. serviceCraft
	// compares elapsed *action* ticks against the recipe's duration, so it
	// already only fires on a beat; this just skips eleven twelfths of the walk
	// over players. Deleting it changes no behaviour, which is exactly why it
	// is described as what it is rather than as a correctness guard.
	if r.tick%content.ActionTicks != 0 {
		return
	}

	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.craft.station == 0 || p.craft.pending {
			continue
		}
		r.serviceCraft(p)
	}
}

// craftInterrupted reports whether a run must end, and why.
//
// Deliberately fewer reasons than gathering has. Being hit does not stop
// smithing: a station is somewhere a player has chosen to stand still, usually
// in a camp, and a mob wandering past should not cost them the bar. Walking
// away does stop it, because that is the player deciding.
func (r *Room) craftInterrupted(p *player) (string, bool) {
	e := p.entity
	if p.frozen || isDowned(e) {
		return "", true
	}
	station := r.entity(p.craft.station)
	if station == nil || station.Station == nil {
		return "", true
	}
	if !r.withinStationRange(e, station) {
		return "", true
	}
	return "", false
}

// withinStationRange reports whether a player is standing at a station.
//
// The station's own bounds, widened by the pickup range: a designer draws the
// anvil, and standing next to what they drew is what "at the anvil" means. A
// point-to-point distance would make a wide station harder to use at one end
// than the other for no reason a player could see.
func (r *Room) withinStationRange(e, station *Entity) bool {
	reach := r.content.Balance.Drops.PickupRange
	feet := e.Body.FeetCenter()
	box := station.Body.Bounds()

	return feet.X >= box.X-reach && feet.X <= box.X+box.W+reach &&
		feet.Y >= box.Y-reach && feet.Y <= box.Y+box.H+reach
}

// serviceCraft advances one player's run by one action tick.
func (r *Room) serviceCraft(p *player) {
	rec := p.craft.recipe
	if rec == nil {
		return
	}

	// Still working. A recipe of three beats takes three, which is what makes
	// a longer recipe longer rather than a faster one with a bigger number on
	// it.
	if r.actionTick()-p.craft.startedAt < uint64(rec.ActionTicks) {
		return
	}

	if p.events == nil {
		r.stopCraft(p, "")
		return
	}

	// In flight until the session answers. Another beat must not start a
	// second run: both would ask for the same materials and both would be told
	// yes by a database that had already spent them once.
	p.craft.pending = true
	p.events.RunCraft(CraftRequest{
		Player:      p.entity.ID,
		CharacterID: p.characterID,
		Recipe:      rec,
		Tick:        r.tick,
	})
}

// resolveCraft completes a run once the session has done the durable work.
//
// The experience is granted here rather than when the run was asked for,
// because until the session answers there is no reason to believe the
// materials were there. A room that paid on asking would pay a smith for bars
// they never made.
func (r *Room) resolveCraft(playerID EntityID, made bool, reason string) {
	p := r.players[playerID]
	if p == nil || p.craft.station == 0 {
		return
	}

	p.craft.pending = false
	rec := p.craft.recipe
	if rec == nil {
		return
	}

	if !made {
		// Running out is the ordinary end of a crafting run rather than a
		// failure, and the reason says which it was.
		r.stopCraft(p, reason)
		return
	}

	r.awardSecondary(p.entity, rec.Skill, rec.Exp)
	r.emitTo(playerID, &mmov1.Event{Body: &mmov1.Event_Crafting{Crafting: &mmov1.Crafting{
		RecipeId: rec.ID,
		Name:     rec.Name,
		Skill:    rec.Skill,
		Active:   true,
		Produced: true,
	}}})

	// Straight into the next one, from this beat. Holding a key is not required
	// and would be wrong: a run is a commitment several seconds long, and the
	// decision a player made was "make these", not "make one".
	p.craft.startedAt = r.actionTick()
}

// StationRequest is a player asking what a station can make.
type StationRequest struct {
	Player      EntityID
	CharacterID string
	Station     *content.Station
	Entity      EntityID

	// Levels is the character's level in each skill the station serves, so the
	// session does not have to ask the room back for what the room already
	// holds.
	Levels map[string]int
}

// CraftRequest is one run of a recipe, for the session to carry out.
//
// Unlike GatherYield this is a request rather than a report: the materials may
// not be there, and only the session can find out. Its answer comes back
// through ResolveCraft.
type CraftRequest struct {
	Player      EntityID
	CharacterID string
	Recipe      *content.Recipe
	Tick        uint64
}
