package room

// Regeneration.
//
// Health and mana come back on their own. Without this a caster who spends
// their mana can never attack again -- there is no potion, no rest, and no
// other source -- which turns running dry into a permanent state rather than a
// cost.
//
// The rates are deliberately different in and out of combat, and health only
// returns out of it. A fight should be paced rather than waited out, and the
// pacing should be recovered by stepping away rather than by logging out.

// regenInterval is how often regeneration is applied.
//
// Once a second rather than every tick: the rates are authored per second, a
// twentieth of a percent of a mana bar rounds to nothing, and a room full of
// players does not need this work twenty times a second to be correct.
const regenInterval = TickRate

// phaseRegen restores a share of health and mana to everyone who is up.
func (r *Room) phaseRegen() {
	if r.tick%regenInterval != 0 {
		return
	}

	balance := r.content.Balance.Combat
	for _, id := range r.playerOrder {
		p := r.players[id]
		// The dead do not recover, and neither does somebody whose connection
		// has gone: they are frozen and invulnerable, and coming back to a
		// full bar for having been away would make dropping out the best way
		// to rest.
		if p == nil || p.frozen || !isAlive(p.entity) || isDowned(p.entity) {
			continue
		}

		e := p.entity
		fighting := r.tick < e.Player.InCombatUntil

		mana := balance.ManaRegen
		if fighting {
			mana = balance.ManaRegenInCombat
		}
		e.Player.MP = restoreShare(e.Player.MP, e.Player.MaxMP, mana, &e.Player.ManaCarry)

		if !fighting {
			e.HP = restoreShare(e.HP, e.MaxHP, balance.LifeRegen, &e.Player.LifeCarry)
		}
	}
}

// restoreShare adds a fraction of a maximum to a current value, capped,
// carrying the remainder between calls.
//
// The carry is what makes the rates mean anything at low levels. A fifth of a
// point per second either rounds to zero -- and a level one caster with fifty
// mana regenerates nothing, forever -- or is rounded up to one, which makes
// the in-combat and out-of-combat rates identical at that pool size and the
// distinction between them a lie. Keeping the fraction and spending it when it
// reaches a whole point is exact at any size.
func restoreShare(current, max uint32, ppm int, carry *int64) uint32 {
	if ppm <= 0 || current >= max {
		// A full pool banks nothing: carrying while full would hand back a
		// burst the moment it was spent.
		*carry = 0
		return current
	}

	*carry += int64(max) * int64(ppm)
	gain := *carry / 1_000_000
	*carry -= gain * 1_000_000

	if gain <= 0 {
		return current
	}
	return min(current+uint32(gain), max)
}

// markInCombat starts or extends a character's combat window.
//
// Taking damage is what counts, not dealing it. A player hitting something
// that cannot fight back is not in a fight, and making them wait as though
// they were would punish clearing a zone.
func (r *Room) markInCombat(e *Entity) {
	if e.Player == nil {
		return
	}
	e.Player.InCombatUntil = r.tick + uint64(r.content.Balance.Combat.CombatTicks)
}
