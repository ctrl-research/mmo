package room

import (
	"strconv"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Gathering: the action-tick half of the game.
//
// Everything here runs on the 600 ms action tick rather than the 50 ms
// simulation tick. It is a derived beat -- "every twelfth tick" -- rather than
// a second loop, because there is then one clock in the room and no way for
// two to drift apart. Movement and combat stay at 20 Hz around it, which is
// the whole reason for deriving rather than slowing the room down.
//
// The action tick is what makes gathering feel like OSRS instead of like
// combat: a player commits to a node and the game resolves it on a slow,
// legible beat, and the interesting decision is which node rather than how
// fast they can press a key.

// resourceState is one placement of a resource node, with its own clocks.
//
// Per placement rather than per node definition, and per layer rather than per
// room, for the same reason mob spawns are: two players chopping "the oak by
// the west platform" must not be chopping the same tree, or the whole
// contention problem that layering removes comes straight back in a form
// nobody expected.
type resourceState struct {
	def  *content.ResourceNode
	spot *content.ResourceSpot

	// entity is the node currently standing here, and zero while it is spent.
	entity EntityID

	// remaining counts yields left before it is spent.
	remaining int

	// readyAt is the tick a spent node comes back.
	readyAt uint64
}

// gatherState is one player's commitment to a node.
//
// Held on the player rather than the node because it is the player who is
// doing something: a node has no idea who is working it, which is what makes a
// per-layer node need no owner field and no contention rules.
type gatherState struct {
	// node is the entity being gathered, zero when the player is idle.
	node EntityID

	// skill and def are cached from the node so an interruption message can
	// name the skill without another lookup, and so a node removed mid-swing
	// still ends cleanly.
	skill string
	def   *content.ResourceNode
}

// startResources creates every shared-layer node on the map.
//
// Owner-layer ones are created per layer in layerFor, exactly like mob spawns:
// a per-player node does not exist until there is a player to own it.
func (r *Room) startResources() {
	if r.mapDef == nil {
		return
	}
	for i := range r.mapDef.Resources {
		spot := &r.mapDef.Resources[i]
		if spot.Layer != content.LayerShared {
			continue
		}
		if state := r.newResourceState(spot, SharedLayer); state != nil {
			r.sharedResources = append(r.sharedResources, state)
		}
	}
}

// newResourceState prepares one placement, or nil if its node is unknown.
func (r *Room) newResourceState(spot *content.ResourceSpot, layer LayerID) *resourceState {
	def, ok := r.content.Nodes[spot.NodeID]
	if !ok {
		// Content is verified at load, so this cannot happen without a bug in
		// the loader. Skipping quietly beats panicking inside a room that
		// other players are relying on.
		r.log.Error("map places an unknown resource node", "node", spot.NodeID)
		return nil
	}
	return &resourceState{def: def, spot: spot, remaining: def.Yields}
}

// layerResources builds a layer's private copy of every owner-layer node.
func (r *Room) layerResources(layer LayerID) []*resourceState {
	if r.mapDef == nil {
		return nil
	}
	var out []*resourceState
	for i := range r.mapDef.Resources {
		spot := &r.mapDef.Resources[i]
		if spot.Layer != content.LayerOwner {
			continue
		}
		if state := r.newResourceState(spot, layer); state != nil {
			out = append(out, state)
		}
	}
	return out
}

// phaseResources keeps node entities in step with their timers.
//
// On the simulation tick rather than the action tick: a respawn is something
// the world does, not something a player does, and a tree that only reappeared
// on a 600 ms boundary would visibly pop in late for no reason a player could
// see.
func (r *Room) phaseResources() {
	for _, state := range r.sharedResources {
		r.serviceResource(state, SharedLayer)
	}
	for _, l := range r.activeLayers() {
		for _, state := range l.resources {
			r.serviceResource(state, l.id)
		}
	}
}

// serviceResource spawns a node that is due and does nothing otherwise.
func (r *Room) serviceResource(state *resourceState, layer LayerID) {
	if state.entity != 0 {
		return
	}
	if r.tick < state.readyAt {
		return
	}
	state.remaining = state.def.Yields
	state.entity = r.spawnResourceEntity(state, layer).ID
}

// resourceSize is how large a node's body is.
//
// Fixed rather than authored per node, because it is a hitbox for a "walk up
// to this and press a key" interaction rather than anything the physics cares
// about, and one size means a designer placing a tree does not also have to
// decide how big a tree is.
var resourceSize = sim.Vec{X: fixed.FromInt(40), Y: fixed.FromInt(56)}

func (r *Room) spawnResourceEntity(state *resourceState, layer LayerID) *Entity {
	body := sim.NewBody(state.spot.At, resourceSize.X, resourceSize.Y)
	return r.spawnEntity(&Entity{
		Kind:  KindResource,
		Layer: layer,
		Body:  body,
		Name:  state.def.Name,
		Resource: &ResourceEntity{
			State: state,
			// Carried on the entity so the snapshot can send the requirement
			// without the client knowing anything about content: a client that
			// had to look up "what level is an oak tree" would be a client
			// that needs the content files.
			Skill: state.def.Skill,
			Level: state.def.Level,
		},
	})
}

// ResourceEntity is the node half of a resource placement: what a client is
// told about it, plus the way back to the state that owns its clocks.
type ResourceEntity struct {
	State *resourceState

	Skill string
	Level int
}

// beginGather is a player asking to work a node.
//
// Every reason it can fail is checked here, server-side, against the server's
// own state -- and each one says why, because "nothing happened" is the single
// most confusing thing a gathering skill can do. A player who walks up to a
// tree with a pickaxe out and gets silence has no way to learn anything.
func (r *Room) beginGather(p *player, nodeID EntityID) {
	e := p.entity
	node := r.entity(nodeID)
	if node == nil || node.Resource == nil || e.Player == nil {
		return
	}
	if isDowned(e) {
		return
	}

	// Layer visibility is the whole access rule, exactly as it is for loot: a
	// node in another player's layer was never sent to this client, so a
	// request for it is a stale ID or a forged one, and both are refused the
	// same way.
	if !canInteract(e, node) {
		return
	}

	if !r.withinGatherRange(e, node) {
		r.refuseGather(e.ID, "you are too far away")
		return
	}

	def := node.Resource.State.def
	skill, ok := r.content.Secondary[def.Skill]
	if !ok {
		return
	}

	level := r.secondaryLevel(e.Player, def.Skill)
	if level < def.Level {
		r.refuseGather(e.ID, "you need "+skill.Name+" level "+strconv.Itoa(def.Level))
		return
	}

	if skill.ToolClass != "" {
		power := e.Player.ToolPower[def.Skill]
		if power <= 0 {
			r.refuseGather(e.ID, "you need "+aOrAn(skill.ToolName)+" in hand")
			return
		}
		if power < def.MinToolPower {
			r.refuseGather(e.ID, "your "+skill.ToolName+" is not good enough for that")
			return
		}
	}

	// Already on this node: a repeated request is what holding the key
	// produces, and restarting would reset nothing but would emit a second
	// "you begin" line every tick.
	if p.gather.node == nodeID {
		return
	}

	// One action at a time. A character mining and smithing at once would be
	// two runs against one bag, and the bag would lose -- so starting either
	// ends the other, silently, because the player just said which they meant.
	r.stopCraft(p, "")

	p.gather = gatherState{node: nodeID, skill: def.Skill, def: def}
	r.emitTo(e.ID, &mmov1.Event{Body: &mmov1.Event_Gathering{Gathering: &mmov1.Gathering{
		EntityId: uint32(nodeID),
		Skill:    def.Skill,
		Active:   true,
	}}})
}

// stopGather ends a player's action, telling them why if there is a reason
// worth hearing.
//
// Silent for the ordinary endings -- they walked away, they asked to stop --
// and spoken for the ones a player would otherwise have to guess at.
func (r *Room) stopGather(p *player, reason string) {
	if p.gather.node == 0 {
		return
	}
	p.gather = gatherState{}
	r.emitTo(p.entity.ID, &mmov1.Event{Body: &mmov1.Event_Gathering{Gathering: &mmov1.Gathering{
		Active: false,
		Reason: reason,
	}}})
}

func (r *Room) refuseGather(id EntityID, reason string) {
	r.emitTo(id, &mmov1.Event{Body: &mmov1.Event_Gathering{Gathering: &mmov1.Gathering{
		Active: false,
		Reason: reason,
	}}})
}

// phaseActions resolves every player's gathering, on the action tick.
//
// The interruption checks run every simulation tick and the roll runs only on
// the action tick. That split is deliberate: a player who walks away should
// stop immediately rather than up to 600 ms later, and a player who stands
// still should be rolled for on a beat they can feel.
func (r *Room) phaseActions() {
	r.phaseCrafting()

	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.gather.node == 0 {
			continue
		}
		if reason, stop := r.gatherInterrupted(p); stop {
			r.stopGather(p, reason)
		}
	}

	if r.tick%content.ActionTicks != 0 {
		return
	}

	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.gather.node == 0 {
			continue
		}
		r.resolveGather(p)
	}
}

// gatherInterrupted reports whether a player's action must end, and why.
func (r *Room) gatherInterrupted(p *player) (string, bool) {
	e := p.entity
	if p.frozen || isDowned(e) {
		// No message: a downed player has a death overlay in front of them,
		// and a frozen one has no connection to send to.
		return "", true
	}

	node := r.entity(p.gather.node)
	if node == nil || node.Resource == nil {
		// The node was spent, by this player or -- in a shared layer -- by
		// somebody else. Spoken, because a tree vanishing is the one ending a
		// player might read as a bug.
		return "it is used up", true
	}
	if !r.withinGatherRange(e, node) {
		return "", true
	}
	if r.tick < e.Player.InCombatUntil {
		// Being hit stops you. Otherwise the safest place in the game to fight
		// from would be behind a tree, and every gathering node next to a mob
		// spawn would be a free tank.
		return "you are under attack", true
	}
	return "", false
}

// withinGatherRange reports whether a player is close enough to work a node.
//
// The pickup range, reused rather than given a constant of its own: they are
// the same question -- "am I standing at this thing" -- and two numbers would
// eventually be tuned apart for no reason anyone could state.
func (r *Room) withinGatherRange(e, node *Entity) bool {
	reach := r.content.Balance.Drops.PickupRange
	at := node.Body.FeetCenter()
	feet := e.Body.FeetCenter()
	return horizontalGap(feet, at) <= reach && verticalGap(feet, at) <= reach
}

// resolveGather rolls one action tick of gathering for one player.
func (r *Room) resolveGather(p *player) {
	e := p.entity
	node := r.entity(p.gather.node)
	if node == nil || node.Resource == nil {
		return
	}

	state := node.Resource.State
	def := state.def

	level := r.secondaryLevel(e.Player, def.Skill)
	chance := r.content.GatherChancePPM(def, level, e.Player.ToolPower[def.Skill])

	// The layer's own stream, so one player's luck at a tree never depends on
	// how many other people happen to be in the room.
	source := r.rand
	if l, ok := r.layers[node.Layer]; ok && l.rand != nil {
		source = l.rand
	}
	if !source.PPM(int(chance)) {
		return
	}

	// Experience first, then the item. A full inventory must not cost the
	// experience: the swing landed, and the only thing that failed was
	// storage. Doing it the other way round makes a full bag silently delete
	// progress, which is the kind of bug a player reports as "my woodcutting
	// stopped going up" three hours later.
	r.awardSecondary(e, def.Skill, def.Exp)

	if p.events != nil {
		p.events.GrantGather(GatherYield{
			Player:      e.ID,
			CharacterID: p.characterID,
			Skill:       def.Skill,
			ItemID:      def.Item,
			Qty:         def.Qty,
			Tick:        r.tick,
		})
	}

	state.remaining--
	if state.remaining > 0 {
		return
	}

	// Spent. The entity goes and the timer starts, and everyone working it is
	// told -- in a shared layer that is more than one person.
	r.depleteResource(state)
}

// depleteResource removes a spent node and starts its respawn.
func (r *Room) depleteResource(state *resourceState) {
	id := state.entity
	state.entity = 0
	state.readyAt = r.tick + uint64(state.def.RespawnTicks)

	// Everybody working it stops, before the entity goes: after removal the
	// interruption would still be caught next tick, but it would be reported
	// as "it is used up" for the player who used it up, which reads as an
	// error rather than as success.
	for _, pid := range r.playerOrder {
		p := r.players[pid]
		if p != nil && p.gather.node == id {
			r.stopGather(p, "")
		}
	}
	r.removeEntity(id)
}

// awardSecondary grants experience in a secondary skill and reports any level.
//
// Secondary experience is cumulative and never spent, unlike the main level:
// the level is derived from the total rather than the total being decremented
// as levels are taken. That is OSRS's arrangement and it is the reason a
// secondary skill can never lose a level to a rounding change in the curve.
func (r *Room) awardSecondary(e *Entity, skillID string, amount int64) {
	if amount <= 0 || e.Player == nil {
		return
	}

	curves := r.content.Curves
	if e.Player.Secondary == nil {
		e.Player.Secondary = make(map[string]int64)
	}

	before := curves.SecondaryLevelFor(e.Player.Secondary[skillID])

	// Capped at the experience for the maximum level, so a maxed skill stops
	// accumulating a number nothing will ever read again.
	total := e.Player.Secondary[skillID] + amount
	if max := curves.SecondaryExpAt(curves.MaxSkillLevel); total > max {
		total = max
	}
	e.Player.Secondary[skillID] = total

	after := curves.SecondaryLevelFor(total)

	r.emitTo(e.ID, &mmov1.Event{Body: &mmov1.Event_SecondaryExp{SecondaryExp: &mmov1.SecondaryExp{
		Skill:   skillID,
		Gained:  uint64(amount),
		Total:   uint64(total),
		Level:   uint32(after),
		LevelAt: uint64(curves.SecondaryExpAt(after)),
		NextAt:  uint64(curves.SecondaryNextAt(after)),
		LevelUp: after > before,
	}}})

	if after > before {
		r.log.Info("secondary level", "entity", uint32(e.ID), "skill", skillID, "level", after)
	}
}

// secondaryLevel reads a character's level in one secondary skill.
func (r *Room) secondaryLevel(p *PlayerState, skillID string) int {
	if p == nil {
		return 1
	}
	return r.content.Curves.SecondaryLevelFor(p.Secondary[skillID])
}

// GatherYield is one successful gather, for the session to store.
//
// It goes out through SessionEvents rather than being written here for the
// same reason loot does: the tick loop must not touch a database. Unlike loot
// there is nothing to hold in the world while the write is in flight -- the
// experience is already granted and the item did not exist until now -- so
// there is no matching Resolve. A failed write costs the player one log and
// says so, which is the right cost for a full bag.
type GatherYield struct {
	Player      EntityID
	CharacterID string
	Skill       string
	ItemID      string
	Qty         int
	Tick        uint64
}

// SecondaryProgress is a character's secondary experience as the session knows
// it, pushed in on join and captured on checkpoint.
type SecondaryProgress map[string]int64

// aOrAn picks the article for a tool name, so the refusal reads like English.
func aOrAn(word string) string {
	if word == "" {
		return "a tool"
	}
	switch word[0] {
	case 'a', 'e', 'i', 'o', 'u':
		return "an " + word
	}
	return "a " + word
}
