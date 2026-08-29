package room

import (
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Death, and coming back from it.
//
// A character at zero health is *downed*: still in the room, still visible,
// but out of the fight. They come back at the map's spawn point once the
// clock runs out.
//
// The delay is the whole design. A fight you can rejoin the moment you lose it
// is a fight with no stakes, and an instant respawn on the spot makes every
// encounter a war of attrition that the player cannot lose. It is also the
// window a party has to finish what someone else started -- which is what
// makes dying in a group different from dying alone, before any revive
// mechanic exists at all.

// downPlayer puts a character out of the fight.
func (r *Room) downPlayer(e *Entity) {
	p := e.Player
	if p == nil || p.ReviveAt != 0 {
		// Already down. Reaching zero twice is not two deaths, and paying the
		// penalty twice for one of them would be a bug a player could feel.
		return
	}

	p.ReviveAt = r.tick + uint64(r.content.Balance.Combat.DownedTicks)

	// Stop dead rather than sliding: a body that keeps its momentum skates
	// across the floor for a second after it drops, which reads as a physics
	// glitch instead of a death.
	e.Body.Vel = sim.Vec{}

	// Queued inputs belong to a character who was alive when they were sent.
	// Replaying them on revive would teleport the body a second's walk from
	// wherever it came back.
	if pl := r.players[e.ID]; pl != nil {
		pl.queue = pl.queue[:0]
		pl.lastInput = sim.Input{}
	}

	lost := r.chargeDeathPenalty(p)

	// Buffs go with the fight they were part of. A character who kept a
	// stack of Rage through their own death would come back stronger for
	// having died, and one who kept Burning would come back on fire.
	clear(e.Buffs)
	e.Shield, e.ShieldUntil = 0, 0
	r.refreshBuffStats(e)
	r.emitBuffs(e)

	r.emitTo(e.ID, &mmov1.Event{Body: &mmov1.Event_Downed{Downed: &mmov1.Downed{
		EntityId: uint32(e.ID),
		// Milliseconds rather than a tick number: a client counting down to a
		// server tick would have to know the server's tick, and a countdown is
		// the one thing here that is purely presentational.
		ReviveInMs: uint32(r.content.Balance.Combat.DownedTicks * 1000 / TickRate),
		ExpLost:    uint64(lost),
	}}})

	r.log.Info("player downed", "entity", uint32(e.ID), "name", e.Name, "exp_lost", lost)
}

// chargeDeathPenalty takes a share of the progress made toward the current
// level, and reports how much it took.
//
// Of progress within the level rather than of total experience. That is what
// keeps a death from ever costing a level, and from costing a level 90
// character forty times what it costs a level 10 one -- which is how a death
// penalty stops being a setback and becomes a reason to stop playing.
func (r *Room) chargeDeathPenalty(p *PlayerState) int64 {
	ppm := int64(r.content.Balance.Combat.DeathExpPenalty)
	if ppm <= 0 || p.Exp <= 0 {
		return 0
	}

	// The penalty is a fraction below one and the progress is at least one, so
	// this can never exceed what there is to take and needs no clamp.
	lost := p.Exp * ppm / 1_000_000
	if lost <= 0 {
		// A non-zero penalty that rounds to nothing still takes something, so
		// the rule reads the same at every level rather than quietly switching
		// off for anyone who has only just levelled.
		lost = 1
	}
	p.Exp -= lost
	return lost
}

// phaseRevive brings back anyone whose clock has run out.
//
// After AI and before the snapshot, so a character returns with full health in
// the same tick the client is told where they are -- rather than being seen for
// one tick standing on the spawn point at zero.
func (r *Room) phaseRevive() {
	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.entity.Player == nil {
			continue
		}
		if at := p.entity.Player.ReviveAt; at == 0 || r.tick < at {
			continue
		}
		r.revive(p.entity)
	}
}

// revive returns a character to the map's spawn point at full strength.
func (r *Room) revive(e *Entity) {
	p := e.Player
	p.ReviveAt = 0

	e.HP = e.MaxHP
	p.MP = p.MaxMP
	e.Body = r.spawnBody()

	// A spawn point is a fixed place and something is often standing on it.
	// Without a moment's grace, coming back next to whatever killed you means
	// dying again before you can move, and paying the penalty each time.
	p.SafeUntil = r.tick + uint64(r.content.Balance.Combat.ReviveGraceTicks)

	// Portals are held off for the same reason they are after a transfer: a
	// spawn point that overlaps one would send a character who has just come
	// back straight somewhere else.
	if pl := r.players[e.ID]; pl != nil {
		pl.portalReadyAt = r.tick + portalCooldownTicks
	}
}

// isDowned reports whether a character is out of the fight.
func isDowned(e *Entity) bool {
	return e.Player != nil && e.Player.ReviveAt != 0
}

// isProtected reports whether a character is inside their revive grace.
func (r *Room) isProtected(e *Entity) bool {
	return e.Player != nil && r.tick < e.Player.SafeUntil
}

// dropProtection ends the revive grace, which attacking does.
//
// Grace that survived an attack would not be a chance to get clear, it would
// be a free opening -- walk into a fight untouchable, swing first, and only
// then become a target.
func (r *Room) dropProtection(e *Entity) {
	if e.Player != nil {
		e.Player.SafeUntil = 0
	}
}
