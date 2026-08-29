package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// Boss encounters.
//
// The profile is Go and the encounter is content. A boss's mechanics are the
// encounter, so "which ability, at what health, after how long a wind-up" has
// to be authorable -- but the state machine that runs them is code, because
// expressing that as parameters means inventing a scripting language nobody
// asked for.
//
// Three things make a boss a fight rather than a damage race:
//
//   - Phases, entered as health falls and never returned to. A boss that went
//     back on being healed would be a boss whose fight resets on a mistake.
//   - Telegraphs. An attack is announced before it lands, so a player who
//     reads it can move. An attack that simply happens is a number, not a
//     mechanic.
//   - An enrage clock, so a fight cannot be won by attrition from a party that
//     cannot beat the mechanics.

// BossState is everything an encounter tracks.
type BossState struct {
	// Phase indexes Def.Phases. It only ever rises.
	Phase int

	// PhaseStartedAt is when the current phase began, which the enrage clock
	// counts from.
	PhaseStartedAt uint64

	// Enraged marks a phase whose clock has already run out, so the buff is
	// applied once rather than every tick after.
	Enraged bool

	// Ready maps an ability's index in the phase to the tick it may fire
	// again. Per phase, so entering a new one does not inherit cooldowns from
	// abilities that no longer exist.
	Ready map[int]uint64

	// Casting is the ability being wound up and CastAt is when it lands.
	//
	// A boss commits to a telegraphed attack the moment it starts one: it
	// stops moving, it stops turning, and the region the attack will cover is
	// fixed. That commitment is the mechanic. An attack that kept following
	// its target through the wind-up would be an attack nobody could move out
	// of, and the marker drawn for it would be a lie.
	Casting int
	CastAt  uint64

	// Marker is the telegraph entity, removed when the attack lands.
	Marker EntityID
}

// stepBoss runs one tick of an encounter.
func (r *Room) stepBoss(e *Entity) {
	m := e.Mob
	if m.Boss == nil {
		m.Boss = &BossState{Ready: map[int]uint64{}, PhaseStartedAt: r.tick}
		r.enterPhase(e, 0)
	}

	b := m.Boss
	phases := m.Def.Phases

	// Phases advance on health, in order. Checked before anything else, so a
	// hit that crosses a threshold changes the fight this tick rather than
	// after one more attack from the phase it just left.
	for next := b.Phase + 1; next < len(phases); next++ {
		if percentHP(e) > phases[next].AtHPPercent {
			break
		}
		r.enterPhase(e, next)
	}

	phase := phases[b.Phase]

	// The enrage clock. A boss that cannot be beaten on mechanics should end
	// the fight rather than continue it indefinitely.
	if !b.Enraged && phase.EnrageTicks > 0 && r.tick-b.PhaseStartedAt >= uint64(phase.EnrageTicks) {
		b.Enraged = true
		if buff := r.content.Buffs[phase.EnrageBuff]; buff != nil {
			r.applyBuff(e, e, buff, buff.MaxStacks, 0)
		}
		r.announceBoss(e, "enraged")
	}

	// A wind-up in flight resolves before anything new is started, or a boss
	// with a short cooldown would telegraph over its own telegraph.
	if b.CastAt != 0 {
		// Rooted, but still subject to gravity: a boss winding up over a ledge
		// falls like anything else. Zero horizontal intent is the commitment.
		r.applyMobPhysics(e, 0)
		if r.tick >= b.CastAt {
			r.resolveTelegraph(e)
		}
		return
	}

	// Bosses still move between abilities: one that stood still would be a
	// target rather than a fight.
	r.bossMove(e)

	for i, ability := range phase.Abilities {
		if ready, ok := b.Ready[i]; ok && r.tick < ready {
			continue
		}
		if r.beginBossAbility(e, i, ability) {
			b.Ready[i] = r.tick + uint64(ability.Cooldown)
			return
		}
	}
}

// bossMove walks the boss towards whoever it is fighting.
//
// Deliberately not the ordinary chase: a boss does not leash and does not heal
// itself by walking home. An arena fight that resets because the party pulled
// it four tiles too far is a fight decided by geometry rather than by play.
func (r *Room) bossMove(e *Entity) {
	m := e.Mob

	target := r.entity(m.Target)
	if target == nil || !isAlive(target) || !canInteract(e, target) {
		m.Target = r.findTarget(e)
		target = r.entity(m.Target)
	}
	if target == nil {
		r.applyMobPhysics(e, 0)
		return
	}

	// It stops when the target is in reach, measured on both axes.
	//
	// Horizontal distance alone is what an ordinary mob uses, and in a
	// platformer it is half an answer: a player standing on a ledge is a few
	// units away horizontally and completely out of reach. A boss that stopped
	// on that would plant itself underneath them and do nothing at all, for as
	// long as they cared to stand there.
	gap := horizontalGap(e.Body.Pos, target.Body.Pos)
	if gap <= m.Def.AI.AttackRange && verticalGap(e.Body.Pos, target.Body.Pos) <= m.Def.AI.AttackRange {
		e.Body.FacingLeft = target.Body.Bounds().CenterX() < e.Body.Bounds().CenterX()
		r.applyMobPhysics(e, 0)
		return
	}

	// Out of reach: close the gap, but not past it. A boss standing directly
	// under a target it cannot reach would otherwise flip direction every tick
	// and vibrate in place.
	if gap <= m.Def.Width {
		e.Body.FacingLeft = target.Body.Bounds().CenterX() < e.Body.Bounds().CenterX()
		r.applyMobPhysics(e, 0)
		return
	}
	r.applyMobPhysics(e, directionTo(e, target))
}

// enterPhase moves the encounter on.
func (r *Room) enterPhase(e *Entity, phase int) {
	b := e.Mob.Boss
	def := e.Mob.Def.Phases[phase]

	b.Phase = phase
	b.PhaseStartedAt = r.tick
	b.Enraged = false
	b.Ready = map[int]uint64{}
	b.CastAt = 0
	if b.Marker != 0 {
		r.removeEntity(b.Marker)
		b.Marker = 0
	}

	if def.OnEnter != "" {
		if skill := r.content.Skills[def.OnEnter]; skill != nil {
			r.castSkill(e, skill)
		}
	}

	r.announceBoss(e, def.Name)
}

// beginBossAbility starts a telegraphed attack, reporting whether it started.
func (r *Room) beginBossAbility(e *Entity, index int, ability content.BossAbility) bool {
	skill := r.content.Skills[ability.Skill]
	if skill == nil {
		return false
	}

	target := r.chooseBossTarget(e, ability.Target)
	if target == nil {
		// Nothing to aim at. Not an error: everyone may have died, or be out
		// of the room, and the boss should simply wait.
		return false
	}

	// Turn to face the target now, once, and then hold it. Everything the
	// marker promises follows from the boss not turning again: the region is
	// computed from this facing, so a boss that spun mid-wind-up would land
	// its attack somewhere the marker never covered.
	//
	// Done before the reachability check, because which way the boss is facing
	// is most of what decides whether the swing reaches anyone.
	facing := e.Body.FacingLeft
	if target.ID != e.ID {
		e.Body.FacingLeft = target.Body.Bounds().CenterX() < e.Body.Bounds().CenterX()
	}

	if !r.bossWouldLand(e, skill) {
		// Out of reach. Refusing to start is the whole of it: a boss that wound
		// up regardless would spend the fight rooted in place, telegraphing an
		// attack at a player standing on a ledge above it -- and a marker that
		// appears when nothing is in danger teaches players to ignore markers.
		e.Body.FacingLeft = facing
		return false
	}

	b := e.Mob.Boss
	b.Casting = index
	e.Mob.Target = target.ID

	if ability.TelegraphTicks <= 0 {
		// No wind-up: an ordinary swing, which needs no warning and no marker.
		b.CastAt = r.tick
		r.resolveTelegraph(e)
		return true
	}

	b.CastAt = r.tick + uint64(ability.TelegraphTicks)
	b.Marker = r.spawnTelegraph(e, skill, ability.TelegraphTicks)
	return true
}

// bossWouldLand reports whether a skill cast right now would reach anybody.
//
// The test is the skill's own hitbox, so it is exactly the question "will this
// hit something", asked with the geometry that will answer it -- not a range
// number kept alongside the skill and tuned separately from it. In a
// platformer the vertical half of that question is the one that matters: a
// ground-level swing must not commit to a player two platforms up, and a range
// check alone cannot tell those apart.
func (r *Room) bossWouldLand(e *Entity, skill *content.Skill) bool {
	if skill.Targeting.Kind == content.TargetSelf {
		// No region to be inside. A projectile finds its own target after it is
		// launched, so refusing to fire one on geometry the bolt has not
		// travelled yet would be refusing at the wrong moment.
		return true
	}
	return len(r.resolveTargets(e, skill)) > 0
}

// resolveTelegraph fires the ability that was wound up.
func (r *Room) resolveTelegraph(e *Entity) {
	b := e.Mob.Boss
	b.CastAt = 0
	if b.Marker != 0 {
		r.removeEntity(b.Marker)
		b.Marker = 0
	}

	phase := e.Mob.Def.Phases[b.Phase]
	if b.Casting >= len(phase.Abilities) {
		// The phase changed mid-wind-up. Dropping the attack is right: it
		// belonged to a fight that is no longer happening.
		return
	}

	skill := r.content.Skills[phase.Abilities[b.Casting].Skill]
	if skill == nil {
		return
	}

	r.castSkill(e, skill)
}

// chooseBossTarget picks what an ability aims at.
//
// Random and farthest exist so a fight is not one person standing still while
// everyone else ignores it: an ability that only ever hits whoever is closest
// is an ability that only ever concerns one player.
func (r *Room) chooseBossTarget(e *Entity, kind content.BossTarget) *Entity {
	switch kind {
	case content.BossTargetSelf:
		return e

	case content.BossTargetCurrent:
		if target := r.entity(e.Mob.Target); target != nil && isAlive(target) {
			return target
		}
		// Nobody engaged yet: fall back to anyone, so an opening attack is
		// still an attack.
		return r.anyBossTarget(e, false)

	case content.BossTargetRandom:
		return r.anyBossTarget(e, false)

	case content.BossTargetFarthest:
		return r.anyBossTarget(e, true)
	}
	return nil
}

// anyBossTarget returns a player the boss can reach: a random one, or the one
// furthest away.
func (r *Room) anyBossTarget(e *Entity, farthest bool) *Entity {
	var candidates []*Entity
	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.frozen || !isAlive(p.entity) || !canInteract(e, p.entity) {
			continue
		}
		candidates = append(candidates, p.entity)
	}
	if len(candidates) == 0 {
		return nil
	}

	if !farthest {
		// From the room's own stream, so a replay reproduces which player the
		// boss decided to aim at.
		return candidates[r.randFor(e.Layer).Range(0, len(candidates)-1)]
	}

	origin := e.Body.FeetCenter()
	best, bestDist := candidates[0], fixed.F(0)
	for _, c := range candidates {
		at := c.Body.FeetCenter()
		dist := (at.X - origin.X).Abs() + (at.Y - origin.Y).Abs()
		if dist > bestDist {
			best, bestDist = c, dist
		}
	}
	return best
}

// percentHP is how much of a boss's health is left, as a percentage.
func percentHP(e *Entity) int {
	if e.MaxHP == 0 {
		return 0
	}
	return int(int64(e.HP) * 100 / int64(e.MaxHP))
}

// announceBoss tells the room what the encounter is doing.
//
// To everyone who can see it, because a phase change is the fight telling a
// party to do something different and a party that misses it does not.
func (r *Room) announceBoss(e *Entity, what string) {
	r.emit(&mmov1.Event{Body: &mmov1.Event_BossPhase{BossPhase: &mmov1.BossPhase{
		EntityId: uint32(e.ID),
		Name:     e.Name,
		Phase:    what,
		Hp:       e.HP,
		HpMax:    e.MaxHP,
	}}}, e.Layer)
}
