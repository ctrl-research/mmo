package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/rng"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Combat resolution.
//
// Everything here runs server-side inside the tick. The client asks to swing
// and is told what happened; it never decides what it hit or for how much.
// That is what makes damage hacks unrepresentable rather than merely detected.

// phaseCasts resolves every cast requested since the last tick.
//
// Casts resolve after movement so a swing lands where the player actually is
// this tick, not where they were before their input was applied. That ordering
// is observable: it decides whether a player who steps forward and attacks in
// the same tick reaches something at the edge of their range.
func (r *Room) phaseCasts() {
	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || len(p.casts) == 0 {
			continue
		}

		for _, req := range p.casts {
			r.resolveCastRequest(p.entity, req)
		}
		p.casts = p.casts[:0]
	}
}

func (r *Room) resolveCastRequest(caster *Entity, req castRequest) {
	// Against the character's own loadout, not the whole content set. A client
	// may ask for any skill id, and this is what stops it casting a mob
	// ability, a skill for another class, or one it has not learned.
	linked := r.linkedSkill(caster, req.skillID)
	if linked == nil {
		return
	}
	if !r.canCast(caster, linked) {
		return
	}

	caster.Body.FacingLeft = req.facingLeft
	r.beginCast(caster, linked)
	r.castLinked(caster, linked)
}

// linkedSkill returns a caster's set-up version of a skill, or nil if they
// cannot cast it.
//
// Mobs have no loadout: their abilities come from their content definition,
// which is the server's own data rather than anything a client can name.
func (r *Room) linkedSkill(caster *Entity, skillID string) *Linked {
	if caster.Player != nil {
		return caster.Player.Loadout[skillID]
	}

	skill, ok := r.content.Skills[skillID]
	if !ok {
		return nil
	}
	return link(skill, 1, nil)
}

// castSkill resolves a skill with no supports linked.
//
// For everything the simulation casts on its own behalf: a mob ability, and a
// trigger_skill effect. A triggered skill deliberately does not inherit the
// triggering skill's supports -- a support that repeated an effect would
// repeat the trigger, and a support chain that fed itself would be a loop the
// content check cannot see.
func (r *Room) castSkill(caster *Entity, skill *content.Skill) {
	r.castLinked(caster, link(skill, 1, nil))
}

// castLinked resolves one cast from a caster that has already been validated.
func (r *Room) castLinked(caster *Entity, linked *Linked) {
	skill := linked.Skill

	r.emit(&mmov1.Event{Body: &mmov1.Event_SkillCast{SkillCast: &mmov1.SkillCast{
		CasterId:   uint32(caster.ID),
		SkillId:    skill.ID,
		FacingLeft: caster.Body.FacingLeft,
	}}}, caster.Layer)

	// Self-targeted skills resolve against the caster; everything else needs
	// something in range. A skill whose whole payload is a projectile is
	// self-targeted, because the bolt finds its own target later.
	targets := r.resolveTargets(caster, skill)
	source := r.randFor(caster.Layer)

	for _, target := range targets {
		for i := range linked.Effects {
			r.applyEffect(caster, target, skill, &linked.Effects[i], source)
		}
	}
}

// resolveTargets finds what a cast hits.
//
// Layer filtering happens here rather than at damage time, so a cast can never
// touch an entity in another player's layer even through a bug in the geometry
// -- cross-layer damage is not prevented by a check, it is unreachable.
func (r *Room) resolveTargets(caster *Entity, skill *content.Skill) []*Entity {
	if skill.Targeting.Kind == content.TargetSelf {
		return []*Entity{caster}
	}

	box := hitbox(caster, skill)
	casterIsPlayer := caster.Kind == KindPlayer

	var out []*Entity
	for _, e := range r.entities {
		if e.ID == caster.ID || !isAlive(e) || r.isFrozen(e) {
			continue
		}
		// Players hit mobs and mobs hit players. PvP is explicitly out of
		// scope, so same-kind targeting is not a rule to configure yet.
		if casterIsPlayer == (e.Kind == KindPlayer) {
			continue
		}
		if !canInteract(caster, e) {
			continue
		}
		if !box.Overlaps(e.Body.Bounds()) {
			continue
		}

		out = append(out, e)
		if len(out) >= skill.Targeting.MaxTargets {
			break
		}
	}
	return out
}

// hitbox builds the volume a cast covers.
//
// A "cone" in a 2D side-scroller is a box extending in the facing direction:
// there is no third dimension for an angle to sweep through, and a true wedge
// would only make close-range hits feel unreliable. HalfHeight is what keeps a
// ground-level swing from hitting everything standing on the platform above,
// which is the difference between melee that reads correctly and melee that
// feels like it has a mind of its own.
func hitbox(caster *Entity, skill *content.Skill) sim.Rect {
	body := caster.Body.Bounds()
	centreY := body.Y + body.H/2

	halfH := skill.Targeting.HalfHeight
	if halfH <= 0 {
		halfH = body.H / 2
	}

	switch skill.Targeting.Kind {
	case content.TargetCircle:
		rad := skill.Targeting.Range
		return sim.Rect{
			X: body.CenterX() - rad,
			Y: centreY - rad,
			W: rad * 2,
			H: rad * 2,
		}

	default: // cone
		reach := skill.Targeting.Range
		x := body.Right()
		if caster.Body.FacingLeft {
			x = body.Left() - reach
		}
		return sim.Rect{X: x, Y: centreY - halfH, W: reach, H: halfH * 2}
	}
}

// huntLayer returns the layer an entity interacts within.
//
// For mobs and drops this is simply where they live. For players it is their
// own hunting layer rather than the shared layer they are visible in --
// otherwise every player would match every layer and could hit anything in the
// room.
func huntLayer(e *Entity) LayerID {
	if e.Player != nil {
		return e.HuntLayer
	}
	return e.Layer
}

// canInteract reports whether two entities may affect each other.
//
// Shared-layer entities interact with everyone, which is what makes a field
// boss a genuine rally point. Everything else is confined to its own layer.
func canInteract(a, b *Entity) bool {
	la, lb := huntLayer(a), huntLayer(b)
	return la == lb || la == SharedLayer || lb == SharedLayer
}

// applyEffect resolves one effect against one target.
//
// The interpreter. Every skill in the game is a list of these, and every
// support modifier is a transformation of such a list -- which is only
// possible because the vocabulary is small, general, and closed. An effect
// kind added for one skill would be an effect kind no support knows about.
//
// Depth is bounded by the caller rather than here: a chain re-enters with one
// fewer jump, a trigger re-enters with the triggering skill marked, and both
// are checked at content load so a loop cannot reach this at all.
func (r *Room) applyEffect(caster, target *Entity, skill *content.Skill, eff *content.Effect, source *rng.Source) {
	// A chance roll comes from the room's stream like every other roll, so a
	// replay reproduces which of a skill's effects happened to land.
	if eff.Chance > 0 && !source.PPM(eff.Chance) {
		return
	}

	switch eff.Kind {
	case content.EffectDamage:
		amount, critical := r.rollDamage(caster, target, eff, source)
		r.damage(caster, target, amount, critical, eff.Element)

	case content.EffectHeal:
		r.heal(target, r.rollAmount(caster, eff, source))

	case content.EffectRestore:
		r.restore(target, r.rollAmount(caster, eff, source))

	case content.EffectApplyBuff:
		r.applyBuff(caster, target, r.content.Buffs[eff.BuffID], eff.Stacks, eff.DurationTicks)

	case content.EffectRemoveBuff:
		r.removeBuff(target, eff.BuffID)

	case content.EffectShield:
		r.applyShield(caster, target, r.rollAmount(caster, eff, source), eff.DurationTicks)

	case content.EffectDash:
		// The caster moves, not the target: a dash is how the caster gets
		// somewhere, and applying it per target would move them once per
		// enemy hit.
		r.dash(caster, eff)

	case content.EffectKnockback:
		r.knockback(caster, target, eff)

	case content.EffectChain:
		r.chain(caster, target, skill, eff, source)

	case content.EffectTriggerSkill:
		if triggered, ok := r.content.Skills[eff.SkillID]; ok {
			r.castSkill(caster, triggered)
		}

	case content.EffectProjectile:
		r.spawnProjectile(caster, eff)

	case content.EffectArea:
		r.spawnArea(caster, target, eff)
	}
}

// rollAmount rolls a non-damage magnitude: healing, a resource, a shield.
//
// The same shape as damage without the mitigation, so a heal scales with the
// caster's stats the way a hit does and a support that increases magnitude
// works on both.
func (r *Room) rollAmount(caster *Entity, eff *content.Effect, source *rng.Source) int {
	base := source.Range(eff.BaseMin, eff.BaseMax)
	total := fixed.FromInt(base) + fixed.FromInt(attackStat(caster)).Mul(eff.ScaleAttack)

	amount := total.Int()
	if amount < 0 {
		return 0
	}
	return amount
}

// rollDamage runs the damage pipeline.
//
// Every roll comes from the room's generator, so a replay reproduces the fight
// exactly -- which is how "was that hit legitimate" becomes a question with an
// answer rather than a judgement call.
func (r *Room) rollDamage(caster, target *Entity, eff *content.Effect, source *rng.Source) (int, bool) {
	bal := r.content.Balance.Combat

	base := source.Range(eff.BaseMin, eff.BaseMax)

	// Attack scaling. Mobs carry their attack stat in content; players get
	// theirs from level until equipment and the full stat pipeline land in M3.
	attack := attackStat(caster)
	total := fixed.FromInt(base) + fixed.FromInt(attack).Mul(eff.ScaleAttack)

	// Critical hits, now that equipment can grant the chance. Rolled from the
	// room's stream like everything else, so a replay reproduces the hit.
	critical := false
	if caster.Player != nil {
		chance := caster.Player.CritChance()
		if chance > 0 && source.PPM(int(chance)) {
			critical = true
			total = total.Mul(caster.Player.CritMultiplier().Fixed())
		}
	}

	// Armour: reduction = armour / (armour + divisor * incoming). Strong
	// against many small hits, weak against one large one, which is what gives
	// armour a different role from resistance rather than being a second name
	// for it.
	incoming := total.Int()
	if incoming < 1 {
		incoming = 1
	}
	armour := armourStat(target)
	if armour > 0 {
		denom := armour + bal.ArmourDivisor*incoming
		if denom > 0 {
			reduced := incoming - (incoming*armour)/denom
			incoming = reduced
		}
	}

	if incoming < bal.MinDamage {
		incoming = bal.MinDamage
	}
	return incoming, critical
}

func attackStat(e *Entity) int {
	if e.Mob != nil {
		return e.Mob.Def.Attack
	}
	if e.Player != nil {
		return e.Player.Attack()
	}
	return 0
}

func armourStat(e *Entity) int {
	if e.Mob != nil {
		return e.Mob.Def.Armour
	}
	if e.Player != nil {
		return e.Player.Armour()
	}
	return 0
}

// damage applies a resolved amount and handles death.
func (r *Room) damage(source, target *Entity, amount int, critical bool, element string) {
	if amount <= 0 || !isAlive(target) {
		return
	}

	// A shield absorbs before health does, and reports what it soaked -- a
	// hit that vanishes into a pool the player cannot see reads as the attack
	// having missed.
	if absorbed := r.absorb(target, amount); absorbed > 0 {
		r.emit(&mmov1.Event{Body: &mmov1.Event_Absorbed{Absorbed: &mmov1.Absorbed{
			EntityId:  uint32(target.ID),
			Amount:    uint32(absorbed),
			Remaining: target.Shield,
		}}}, target.Layer)

		amount -= absorbed
		if amount <= 0 {
			return
		}
	}

	if uint32(amount) >= target.HP {
		target.HP = 0
	} else {
		target.HP -= uint32(amount)
	}

	if target.Mob != nil {
		target.Mob.HitFlash = r.content.Balance.Combat.HitFlashTicks

		// Being hit pulls a mob's attention, so a player who walks up and
		// swings gets a fight rather than an indifferent target.
		if target.Mob.State == aiIdle && source.Kind == KindPlayer {
			target.Mob.State = aiChase
			target.Mob.Target = source.ID
		}
	}

	// Damage is an event, not something the client infers from a falling HP
	// number: two hits of 100 and one of 200 look identical in state, and they
	// are not the same fight.
	r.emit(&mmov1.Event{Body: &mmov1.Event_Damage{Damage: &mmov1.DamageDealt{
		SourceId: uint32(source.ID),
		TargetId: uint32(target.ID),
		Amount:   uint32(amount),
		Critical: critical,
		Element:  element,
	}}}, target.Layer)

	if target.HP == 0 {
		r.kill(source, target)
	}
}

// kill handles a death: rewards, loot, and the corpse.
func (r *Room) kill(killer, victim *Entity) {
	r.emit(&mmov1.Event{Body: &mmov1.Event_Died{Died: &mmov1.EntityDied{
		EntityId: uint32(victim.ID),
		KillerId: uint32(killer.ID),
	}}}, victim.Layer)

	switch {
	case victim.Mob != nil:
		m := victim.Mob
		m.State = aiDead
		m.Killer = killer.ID
		m.Target = 0
		victim.Body.Vel = sim.Vec{}

		// The corpse lingers so the client can play a death animation, but the
		// spawn slot frees immediately -- otherwise every respawn in the game
		// is silently extended by the corpse duration.
		m.RemoveAt = r.tick + uint64(r.content.Balance.Combat.CorpseTicks)
		if m.Spawn != nil {
			m.Spawn.release()
		}

		if killer.Kind == KindPlayer {
			r.awardKill(killer, victim)
		}
		r.rollDrops(killer, victim)

	case victim.Player != nil:
		// Death handling for players -- respawn, penalty -- lands with
		// persistence in M2. Until then a downed player is restored in place
		// rather than left at zero HP with nothing to do.
		victim.HP = victim.MaxHP
		if p := r.players[victim.ID]; p != nil {
			victim.Body = r.spawnBody()
		}
	}
}

// isFrozen reports whether an entity belongs to a disconnected player.
//
// Frozen characters are invulnerable. Freezing only the AI's choice of target
// would still leave them killable by an area effect, which is the case that
// would actually cost somebody a character.
func (r *Room) isFrozen(e *Entity) bool {
	if e.Kind != KindPlayer {
		return false
	}
	p, ok := r.players[e.ID]
	return ok && p.frozen
}

// absorb takes what it can from a shield, returning how much it soaked.
//
// Expiry is checked here rather than on a tick, so a shield that has run out
// costs nothing until something tries to use it.
func (r *Room) absorb(target *Entity, amount int) int {
	if target.Shield == 0 {
		return 0
	}
	if r.tick >= target.ShieldUntil {
		target.Shield = 0
		return 0
	}

	soaked := min(uint32(amount), target.Shield)
	target.Shield -= soaked
	return int(soaked)
}
