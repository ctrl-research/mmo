package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Telegraphs.
//
// A telegraph is the ground marker a boss puts down before an attack lands. It
// is the difference between a mechanic and a number: an attack that simply
// happens can only be survived by having enough health, while an attack that
// is announced can be survived by moving, which is a thing a player does
// rather than a thing their gear does.
//
// It is an entity because everything else is. It culls, layers, and enters and
// leaves a client's view through exactly the same path as a mob or a bolt, so
// there is no second visibility system to keep in step with the first.

// TelegraphState is a wind-up in progress.
type TelegraphState struct {
	// Owner is the boss winding up, so a client can tie the marker to the
	// thing that made it.
	Owner EntityID

	// Skill is what is coming, for rendering.
	Skill string

	// ExpiresAt is when the attack lands and the marker goes away. The boss
	// removes it; this bound only guarantees a marker cannot outlive its
	// encounter if the boss dies mid-wind-up.
	ExpiresAt uint64
}

// spawnTelegraph puts down the marker for an attack about to land, returning
// the entity so the boss can clear it when the attack resolves.
//
// The region is computed from the skill's own targeting, anchored where the
// boss is standing and facing the direction it has committed to. That is what
// makes the marker honest: it is not an approximation of the danger zone drawn
// alongside it, it is the same geometry the swing will use. A boss does not
// move during a wind-up, so what is drawn is what will be hit.
func (r *Room) spawnTelegraph(caster *Entity, skill *content.Skill, ticks int) EntityID {
	region, ok := telegraphRegion(caster, skill)
	if !ok {
		// Nothing to dodge -- a self-buff, or a skill with no reach. Drawing a
		// marker for it would teach players to ignore markers.
		return 0
	}

	body := sim.Body{Pos: sim.Vec{X: region.X, Y: region.Y}, W: region.W, H: region.H}
	body.FacingLeft = caster.Body.FacingLeft

	marker := r.spawnEntity(&Entity{
		Kind:      KindTelegraph,
		Layer:     huntLayer(caster),
		HuntLayer: huntLayer(caster),
		Body:      body,
		Name:      skill.Name,

		// A telegraph's health is its wind-up. Both are set once and never
		// touched again: the client is told how long it has at the moment the
		// marker appears and fills the bar from its own clock, so a wind-up
		// costs exactly one message rather than one per tick. Sending the
		// remainder rather than the elapsed time is what makes a player who
		// arrives mid-wind-up see the right amount of time left.
		HP:    uint32(ticks),
		MaxHP: uint32(ticks),

		Telegraph: &TelegraphState{
			Owner: caster.ID,
			Skill: skill.ID,
			// A tick of slack, so the marker is cleared by the attack landing
			// in the ordinary case and by this only if the boss is gone.
			ExpiresAt: r.tick + uint64(ticks) + 1,
		},
	})
	return marker.ID
}

// telegraphRegion is the area a skill is about to affect, or false when there
// is nothing to draw.
//
// It calls the same hitbox the cast will, which is the point. A marker
// computed separately from the swing is a marker that drifts out of step with
// it the first time either is tuned, and a marker that lies about where an
// attack lands is worse than no marker at all.
func telegraphRegion(caster *Entity, skill *content.Skill) (sim.Rect, bool) {
	// A self-targeted skill covers no ground: a buff, or a projectile that
	// finds its own target after it is launched. The bolt in flight is its own
	// warning, and drawing a patch of floor for it would teach players that
	// markers can be ignored.
	if skill.Targeting.Kind == content.TargetSelf || skill.Targeting.Range <= 0 {
		return sim.Rect{}, false
	}
	return hitbox(caster, skill), true
}

// phaseTelegraphs clears markers whose owner died mid-wind-up.
//
// In the ordinary case the boss removes its own marker as the attack lands.
// This is the other case: a boss killed during a wind-up leaves a marker for
// an attack that is never coming, and a marker that lies is worse than no
// marker at all.
func (r *Room) phaseTelegraphs() {
	var expired []EntityID
	for _, e := range r.entities {
		if e.Telegraph == nil {
			continue
		}
		if r.tick >= e.Telegraph.ExpiresAt || !isAlive(r.entity(e.Telegraph.Owner)) {
			expired = append(expired, e.ID)
		}
	}
	for _, id := range expired {
		r.removeEntity(id)
	}
}
