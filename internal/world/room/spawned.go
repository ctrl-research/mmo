package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// The two effects that spawn entities: projectiles and ground areas.
//
// Both carry their own payload rather than pointing back at the skill that
// made them. A bolt in flight is unaffected by its caster dying, unequipping,
// or walking into another room -- which is what you want, and also what makes
// them safe to simulate without reaching for state that may be gone.

// ProjectileState is a bolt travelling until it hits something or expires.
type ProjectileState struct {
	// Owner is credited with whatever the payload does, so a kill by an arrow
	// grants experience to the archer.
	Owner EntityID

	// Effects are applied to whatever it hits.
	Effects []content.Effect

	// Pierce is how many more targets it may pass through. Zero stops at the
	// first, which is the common case; a piercing shot is a support away.
	Pierce int

	// ExpiresAt bounds its life, so a projectile that finds nothing does not
	// travel forever and a room's entity count stays bounded.
	ExpiresAt uint64

	// hit records what it has already struck, so a piercing projectile does
	// not hit the same target on consecutive ticks while passing through it.
	hit map[EntityID]bool
}

// AreaState is a patch of ground applying effects on a beat.
type AreaState struct {
	Owner   EntityID
	Effects []content.Effect

	Radius   fixed.F
	Interval int

	NextTick  uint64
	ExpiresAt uint64
}

// projectileSize is how big a bolt is. Small: it is a thing to dodge, and a
// large hitbox makes "I moved" indistinguishable from "it missed".
var projectileSize = fixed.FromInt(12)

// spawnProjectile launches a bolt in the caster's facing direction.
func (r *Room) spawnProjectile(caster *Entity, eff *content.Effect) {
	body := caster.Body.Bounds()

	at := sim.Vec{X: body.CenterX(), Y: body.Y + body.H/2}
	vel := eff.Speed
	if caster.Body.FacingLeft {
		vel = -vel
	}

	// Lifetime from the distance it is meant to cover, so content says "this
	// travels 400 units" rather than "this lives for 21 ticks".
	life := 2 * TickRate
	if eff.Distance > 0 && eff.Speed > 0 {
		life = int(eff.Distance/eff.Speed) + 1
	}

	projectile := sim.NewBody(at, projectileSize, projectileSize)
	projectile.Vel.X = vel
	projectile.FacingLeft = caster.Body.FacingLeft

	r.spawnEntity(&Entity{
		Kind: KindProjectile,
		// The caster's hunting layer, so a bolt is visible to and can only hit
		// the people who could see the mob it was aimed at.
		Layer:     huntLayer(caster),
		HuntLayer: huntLayer(caster),
		Body:      projectile,
		Name:      "",
		Projectile: &ProjectileState{
			Owner:     caster.ID,
			Effects:   eff.Effects,
			Pierce:    eff.Jumps,
			ExpiresAt: r.tick + uint64(life),
			hit:       make(map[EntityID]bool),
		},
	})
}

// spawnArea drops a patch of ground that keeps applying its effects.
func (r *Room) spawnArea(caster, target *Entity, eff *content.Effect) {
	// Centred on the target where there is one, and on the caster otherwise:
	// a firestorm goes where you aimed it, and a consecration goes under you.
	at := caster.Body.Bounds()
	if target != nil && target.ID != caster.ID {
		at = target.Body.Bounds()
	}

	size := eff.Radius * 2
	body := sim.NewBody(sim.Vec{X: at.CenterX(), Y: at.Y + at.H}, size, size)

	interval := eff.TickInterval
	if interval <= 0 {
		interval = TickRate / 2
	}

	r.spawnEntity(&Entity{
		Kind:      KindArea,
		Layer:     huntLayer(caster),
		HuntLayer: huntLayer(caster),
		Body:      body,
		Area: &AreaState{
			Owner:     caster.ID,
			Effects:   eff.Effects,
			Radius:    eff.Radius,
			Interval:  interval,
			NextTick:  r.tick,
			ExpiresAt: r.tick + uint64(eff.DurationTicks),
		},
	})
}

// phaseSpawned advances projectiles and ground areas.
//
// After movement and before AI, so a bolt fired last tick has moved before
// anything decides what to do about it -- and so a projectile that kills
// something stops it acting, rather than the mob getting a free swing from
// beyond the grave.
func (r *Room) phaseSpawned() {
	var expired []EntityID

	for _, e := range r.entities {
		switch {
		case e.Projectile != nil:
			if r.stepProjectile(e) {
				expired = append(expired, e.ID)
			}
		case e.Area != nil:
			if r.stepArea(e) {
				expired = append(expired, e.ID)
			}
		}
	}

	for _, id := range expired {
		r.removeEntity(id)
	}
}

// stepProjectile moves a bolt and resolves what it hits, reporting whether it
// is finished.
func (r *Room) stepProjectile(e *Entity) bool {
	p := e.Projectile

	if r.tick >= p.ExpiresAt {
		return true
	}

	// Physics like everything else, so a bolt stops at a wall rather than
	// passing through the map. Gravity off: a projectile that arcs is a
	// different weapon, and one that does should say so in content.
	tuning := r.cfg.Tuning
	tuning.Gravity = 0
	tuning.AirFric = 0
	sim.Step(&e.Body, sim.Input{}, r.cfg.World, &tuning)

	// A bolt that stopped moving hit geometry.
	if e.Body.Vel.X == 0 {
		return true
	}

	owner := r.entity(p.Owner)
	if owner == nil {
		// The caster has gone. The bolt is still in the air and still theirs,
		// but there is nobody to credit, so it simply expires.
		return true
	}

	ownerIsPlayer := owner.Kind == KindPlayer
	box := e.Body.Bounds()

	for _, target := range r.entities {
		if p.hit[target.ID] || target.ID == owner.ID || !isAlive(target) || r.isFrozen(target) {
			continue
		}
		if ownerIsPlayer == (target.Kind == KindPlayer) {
			continue
		}
		if !canInteract(owner, target) || !box.Overlaps(target.Body.Bounds()) {
			continue
		}

		p.hit[target.ID] = true
		for i := range p.Effects {
			r.applyEffect(owner, target, nil, &p.Effects[i], r.randFor(e.Layer))
		}

		if p.Pierce <= 0 {
			return true
		}
		p.Pierce--
	}

	return false
}

// stepArea applies a ground area's effects on its beat, reporting whether it
// is finished.
func (r *Room) stepArea(e *Entity) bool {
	a := e.Area

	if r.tick >= a.ExpiresAt {
		return true
	}
	if r.tick < a.NextTick {
		return false
	}
	a.NextTick = r.tick + uint64(a.Interval)

	owner := r.entity(a.Owner)
	if owner == nil {
		return true
	}

	ownerIsPlayer := owner.Kind == KindPlayer
	box := e.Body.Bounds()

	for _, target := range r.entities {
		if target.ID == owner.ID || !isAlive(target) || r.isFrozen(target) {
			continue
		}
		if ownerIsPlayer == (target.Kind == KindPlayer) {
			continue
		}
		if !canInteract(owner, target) || !box.Overlaps(target.Body.Bounds()) {
			continue
		}

		for i := range a.Effects {
			r.applyEffect(owner, target, nil, &a.Effects[i], r.randFor(e.Layer))
		}
	}

	return false
}
