package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/rng"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// The effects that are not damage.
//
// Each is small on purpose. The vocabulary earns its keep by being composable
// -- a skill is a list of these, and a support rewrites the list -- and every
// one of them that grows a special case for a particular skill takes a bite
// out of that.

// heal restores hit points, capped at the maximum.
func (r *Room) heal(target *Entity, amount int) {
	if amount <= 0 || !isAlive(target) {
		return
	}

	before := target.HP
	target.HP = min(target.HP+uint32(amount), target.MaxHP)
	if target.HP == before {
		return
	}

	// Reported as an event rather than left for the client to infer from a
	// rising health bar, for the same reason damage is: a bar cannot
	// distinguish one large heal from two small ones, and the difference is
	// most of what a healer is reading.
	r.emit(&mmov1.Event{Body: &mmov1.Event_Healed{Healed: &mmov1.Healed{
		EntityId: uint32(target.ID),
		Amount:   target.HP - before,
	}}}, target.Layer)
}

// restore gives back mana.
func (r *Room) restore(target *Entity, amount int) {
	if amount <= 0 || target.Player == nil {
		return
	}
	target.Player.MP = min(target.Player.MP+uint32(amount), target.Player.MaxMP)
}

// applyShield adds an absorption pool.
//
// Absorption sits in front of health rather than adding to it, which is what
// makes a shield different from a heal: it expires unspent, and it does not
// bring anyone back from a low bar.
func (r *Room) applyShield(source, target *Entity, amount, durationTicks int) {
	if amount <= 0 || !isAlive(target) {
		return
	}

	if durationTicks <= 0 {
		durationTicks = defaultShieldTicks
	}

	// Larger wins rather than adding: two shields stacking to an arbitrary
	// pool is how a defensive skill becomes the only skill.
	if target.Shield < uint32(amount) {
		target.Shield = uint32(amount)
	}
	target.ShieldUntil = r.tick + uint64(durationTicks)

	r.emit(&mmov1.Event{Body: &mmov1.Event_Shielded{Shielded: &mmov1.Shielded{
		EntityId: uint32(target.ID),
		Amount:   target.Shield,
	}}}, target.Layer)
}

// defaultShieldTicks is how long a shield lasts when the effect does not say.
// Ten seconds: long enough to be worth casting before a fight, short enough
// that it is not simply always on.
const defaultShieldTicks = 10 * TickRate

// dash moves the caster in the direction they are facing.
//
// Velocity rather than teleportation, so the simulation resolves the movement
// and the dash stops at a wall like everything else. A dash that teleports is
// a dash that goes through geometry.
func (r *Room) dash(caster *Entity, eff *content.Effect) {
	speed := eff.Speed
	if caster.Body.FacingLeft {
		speed = -speed
	}
	caster.Body.Vel.X = speed

	// Off the ground slightly, or a dash along a floor is eaten by friction
	// before it covers any distance.
	if caster.Body.Grounded && eff.Distance > 0 {
		caster.Body.Vel.Y = -eff.Distance
		caster.Body.Grounded = false
	}
}

// knockback pushes a target away from the caster.
func (r *Room) knockback(caster, target *Entity, eff *content.Effect) {
	if !isAlive(target) {
		return
	}

	// Away from the caster, decided by where they actually are rather than by
	// facing: a target that walked behind mid-swing should be pushed back the
	// way it came.
	away := eff.Speed
	if target.Body.Bounds().CenterX() < caster.Body.Bounds().CenterX() {
		away = -away
	}

	target.Body.Vel.X = away
	if eff.Distance > 0 {
		target.Body.Vel.Y = -eff.Distance
		target.Body.Grounded = false
	}
}

// chain re-applies a skill's damage to nearby targets, weaker each hop.
//
// The falloff is what keeps a chain from being strictly better than hitting
// one thing, and the hop limit is what keeps it bounded inside a tick.
func (r *Room) chain(caster, from *Entity, skill *content.Skill, eff *content.Effect, source *rng.Source) {
	hit := map[EntityID]bool{from.ID: true}
	current := from
	falloff := fixed.One

	for jump := 0; jump < eff.Jumps; jump++ {
		next := r.nearestTarget(caster, current, eff.Radius, hit)
		if next == nil {
			// Nothing else in range. A chain that runs out is a chain that
			// runs out; there is nothing to report.
			return
		}
		hit[next.ID] = true

		if eff.Falloff > 0 {
			falloff = falloff.Mul(eff.Falloff)
		}

		// The chain carries the effects it was given, scaled down. Nested
		// rather than re-casting the skill, because re-casting would pay its
		// cost and its cooldown again.
		for i := range eff.Effects {
			scaled := eff.Effects[i]
			scaled.BaseMin = fixed.FromInt(scaled.BaseMin).Mul(falloff).Int()
			scaled.BaseMax = fixed.FromInt(scaled.BaseMax).Mul(falloff).Int()
			scaled.ScaleAttack = scaled.ScaleAttack.Mul(falloff)

			r.applyEffect(caster, next, skill, &scaled, source)
		}

		current = next
	}
}

// nearestTarget finds the closest valid target to an entity, skipping any
// already hit.
func (r *Room) nearestTarget(caster, from *Entity, radius fixed.F, skip map[EntityID]bool) *Entity {
	casterIsPlayer := caster.Kind == KindPlayer
	origin := from.Body.FeetCenter()

	var best *Entity
	var bestDist fixed.F

	for _, e := range r.entities {
		if skip[e.ID] || e.ID == caster.ID || !isAlive(e) || r.isFrozen(e) {
			continue
		}
		if casterIsPlayer == (e.Kind == KindPlayer) {
			continue
		}
		if !canInteract(caster, e) {
			continue
		}

		at := e.Body.FeetCenter()
		dx, dy := (at.X - origin.X).Abs(), (at.Y - origin.Y).Abs()
		if dx > radius || dy > radius {
			continue
		}

		// Manhattan rather than Euclidean: no square roots in the tick, and
		// the difference is invisible at the scale a chain hops.
		dist := dx + dy
		if best == nil || dist < bestDist {
			best, bestDist = e, dist
		}
	}
	return best
}
