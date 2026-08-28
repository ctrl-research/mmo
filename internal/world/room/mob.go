package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// aiState is a mob's current behaviour.
type aiState uint8

const (
	aiIdle aiState = iota
	aiChase
	aiAttack
	aiLeash
	aiDead
)

func (s aiState) String() string {
	switch s {
	case aiIdle:
		return "idle"
	case aiChase:
		return "chase"
	case aiAttack:
		return "attack"
	case aiLeash:
		return "leash"
	case aiDead:
		return "dead"
	default:
		return "unknown"
	}
}

// MobState is everything a mob has that other entities do not.
type MobState struct {
	Def *content.Mob

	State  aiState
	Target EntityID

	// Home is where the mob spawned, and where it returns when it leashes.
	Home sim.Vec

	// Spawn is the spawn point that owns this mob's population slot.
	Spawn *spawnState

	// AttackReady is the tick from which the mob may attack again.
	AttackReady uint64

	// RemoveAt is when a corpse is cleared, giving the client time to play a
	// death animation before the entity disappears.
	RemoveAt uint64

	// HitFlash counts down the ticks a mob reads as recently damaged.
	HitFlash int

	// Killer is credited with the kill for experience and loot ownership.
	Killer EntityID
}

// phaseAI steps every mob's behaviour.
func (r *Room) phaseAI() {
	for _, e := range r.entities {
		if e.Mob == nil {
			continue
		}
		r.stepMob(e)
	}
}

func (r *Room) stepMob(e *Entity) {
	m := e.Mob

	if m.HitFlash > 0 {
		m.HitFlash--
	}

	if m.State == aiDead {
		if r.tick >= m.RemoveAt {
			r.removeEntity(e.ID)
		}
		return
	}

	// Idle mobs run their behaviour on a slower beat. Under per-player
	// layering a room holds roughly layers x mobs entities and most are idle
	// at any moment, so this is the largest saving available -- and it is
	// invisible in play, because an idle mob has nothing to decide.
	//
	// Staggered by entity ID so a room full of mobs does not run every
	// behaviour on the same tick and spike the budget once per interval.
	interval := uint64(m.Def.AI.IdleTickInterval)
	if m.State == aiIdle && interval > 1 && (r.tick+uint64(e.ID))%interval != 0 {
		r.applyMobPhysics(e, 0)
		return
	}

	if m.Def.AI.Profile == content.AIPassive {
		r.applyMobPhysics(e, 0)
		return
	}

	switch m.State {
	case aiIdle:
		r.mobIdle(e)
	case aiChase:
		r.mobChase(e)
	case aiAttack:
		r.mobAttack(e)
	case aiLeash:
		r.mobLeash(e)
	}
}

func (r *Room) mobIdle(e *Entity) {
	if target := r.findTarget(e); target != 0 {
		e.Mob.State = aiChase
		e.Mob.Target = target
	}
	r.applyMobPhysics(e, 0)
}

func (r *Room) mobChase(e *Entity) {
	m := e.Mob

	target := r.entity(m.Target)
	if target == nil || !isAlive(target) {
		m.State = aiLeash
		m.Target = 0
		r.applyMobPhysics(e, 0)
		return
	}

	// Two ways to give up, and both are needed.
	//
	// Distance from home stops a mob being dragged across the map. Distance to
	// the target stops it trudging after someone who is already far out of
	// reach: with only the home check, a mob whose target crosses the zone
	// walks the full leash radius before turning round, which reads as
	// mindless rather than as giving up.
	if horizontalGap(e.Body.Pos, m.Home) > m.Def.AI.LeashRange ||
		horizontalGap(e.Body.Pos, target.Body.Pos) > m.Def.AI.LeashRange {
		m.State = aiLeash
		m.Target = 0
		r.applyMobPhysics(e, 0)
		return
	}

	gap := horizontalGap(e.Body.Pos, target.Body.Pos)
	if gap <= m.Def.AI.AttackRange {
		m.State = aiAttack
		r.applyMobPhysics(e, 0)
		return
	}

	r.applyMobPhysics(e, directionTo(e, target))
}

func (r *Room) mobAttack(e *Entity) {
	m := e.Mob

	target := r.entity(m.Target)
	if target == nil || !isAlive(target) {
		m.State = aiLeash
		m.Target = 0
		r.applyMobPhysics(e, 0)
		return
	}

	if horizontalGap(e.Body.Pos, target.Body.Pos) > m.Def.AI.AttackRange {
		m.State = aiChase
		r.applyMobPhysics(e, directionTo(e, target))
		return
	}

	// Face the target before swinging, or a mob attacks behind itself.
	e.Body.FacingLeft = target.Body.Pos.X < e.Body.Pos.X

	if r.tick >= m.AttackReady && len(m.Def.Abilities) > 0 {
		ability := r.pickAbility(e)
		if ability != nil {
			if skill, ok := r.content.Skills[ability.Skill]; ok {
				r.castSkill(e, skill)
				m.AttackReady = r.tick + uint64(ability.Cooldown)
			}
		}
	}

	r.applyMobPhysics(e, 0)
}

func (r *Room) mobLeash(e *Entity) {
	m := e.Mob

	if horizontalGap(e.Body.Pos, m.Home) <= fixed.FromInt(8) {
		m.State = aiIdle
		// Healing on leash is deliberate: without it a player can whittle a
		// mob down across several pulls, which turns every fight into a war of
		// attrition the mob cannot win.
		e.HP = e.MaxHP
		r.applyMobPhysics(e, 0)
		return
	}

	dir := int32(1000)
	if m.Home.X < e.Body.Pos.X {
		dir = -1000
	}
	r.applyMobPhysics(e, dir)
}

// pickAbility chooses among a mob's abilities by weight.
func (r *Room) pickAbility(e *Entity) *content.MobAbility {
	abilities := e.Mob.Def.Abilities
	if len(abilities) == 1 {
		return &abilities[0]
	}

	weights := make([]int, len(abilities))
	for i := range abilities {
		weights[i] = abilities[i].Weight
	}
	// Layer-scoped stream, so one player's mobs choosing attacks does not
	// perturb another player's drop rolls.
	i := r.randFor(e.Layer).Pick(weights)
	if i < 0 {
		return nil
	}
	return &abilities[i]
}

// findTarget returns the nearest eligible player within aggro range.
//
// Eligibility is layer-scoped: a mob in a player's own layer can only ever see
// that player, and a shared-layer mob can see everyone. That single rule is
// what stops a mob from chasing someone who cannot even see it.
func (r *Room) findTarget(e *Entity) EntityID {
	var best EntityID
	bestGap := e.Mob.Def.AI.AggroRange

	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || !isAlive(p.entity) {
			continue
		}
		if !canInteract(e, p.entity) {
			continue
		}

		gap := horizontalGap(e.Body.Pos, p.entity.Body.Pos)
		if gap > bestGap {
			continue
		}
		// Vertical reach is bounded separately: in a platformer, a mob two
		// floors below should not aggro onto someone directly overhead.
		if verticalGap(e.Body.Pos, p.entity.Body.Pos) > e.Mob.Def.AI.AggroRange {
			continue
		}

		bestGap = gap
		best = p.entity.ID
	}
	return best
}

// applyMobPhysics advances a mob through the same simulation players use, so
// it walks off ledges, lands on platforms, and obeys gravity without a second
// movement implementation.
func (r *Room) applyMobPhysics(e *Entity, moveX int32) {
	tuning := r.tuningFor(e.Mob.Def)
	if moveX != 0 {
		e.Body.FacingLeft = moveX < 0
	}
	sim.Step(&e.Body, sim.Input{MoveX: moveX}, r.cfg.World, &tuning)
}

func directionTo(from, to *Entity) int32 {
	if to.Body.Pos.X < from.Body.Pos.X {
		return -1000
	}
	return 1000
}

func horizontalGap(a, b sim.Vec) fixed.F { return (a.X - b.X).Abs() }
func verticalGap(a, b sim.Vec) fixed.F   { return (a.Y - b.Y).Abs() }

func isAlive(e *Entity) bool {
	if e == nil {
		return false
	}
	if e.Mob != nil && e.Mob.State == aiDead {
		return false
	}
	return e.HP > 0
}
