package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// Buffs, debuffs, and damage over time.
//
// A buff is two things at once, and keeping them one mechanism is the point:
// effects that fire on a beat, and stat modifiers that feed the same pipeline
// as gear and passives. "Burning" and "+20% attack for ten seconds" are the
// same kind of thing, which is why a support that lengthens durations works on
// both without knowing what either does.
//
// Held on the entity and owned by the room's goroutine like everything else.
// The stat modifiers are recomputed into the entity's block whenever the set
// changes rather than on every read: reading happens per hit and changing
// happens per application, and those differ by orders of magnitude.

// activeBuff is one buff on one entity.
type activeBuff struct {
	def *content.Buff

	// Stacks multiplies both the stat modifiers and the ticked effects, so a
	// buff at three stacks hits three times as hard rather than three times as
	// often.
	stacks int

	// expiresAt is the tick it falls off. Zero means it does not expire on its
	// own, which is what an aura is.
	expiresAt uint64

	// nextTick is when its effects fire again.
	nextTick uint64

	// source is who applied it, so damage over time credits the right player
	// for the kill and the experience.
	source EntityID
}

// remaining is how long the buff has left, for the client.
func (b *activeBuff) remaining(now uint64) uint64 {
	if b.expiresAt == 0 || b.expiresAt <= now {
		return 0
	}
	return b.expiresAt - now
}

// applyBuff puts a buff on an entity, or refreshes and stacks one already
// there.
func (r *Room) applyBuff(source, target *Entity, def *content.Buff, stacks, durationTicks int) {
	if def == nil || !isAlive(target) {
		return
	}
	if stacks <= 0 {
		stacks = 1
	}

	duration := def.DurationTicks
	if durationTicks > 0 {
		// A skill may apply a shorter or longer version of a shared buff. The
		// override is on the effect rather than a second buff definition, so
		// "burning, but briefly" is not a whole second buff to maintain.
		duration = durationTicks
	}

	if target.Buffs == nil {
		target.Buffs = make(map[string]*activeBuff)
	}

	existing, held := target.Buffs[def.ID]
	if !held {
		existing = &activeBuff{def: def, source: source.id()}
		target.Buffs[def.ID] = existing
		existing.nextTick = r.tick + uint64(max(def.TickInterval, 1))
	}

	existing.stacks = min(existing.stacks+stacks, def.MaxStacks)
	existing.source = source.id()

	// Refreshing is what separates a buff you maintain from one you build up
	// and spend. Without it a re-application adds a stack while the original
	// still expires on schedule.
	if !held || def.RefreshOnApply {
		if duration > 0 {
			existing.expiresAt = r.tick + uint64(duration)
		} else {
			existing.expiresAt = 0
		}
	}

	r.refreshBuffStats(target)
	r.emitBuffs(target)
}

// removeBuff takes a buff off, if it is there.
func (r *Room) removeBuff(target *Entity, id string) {
	if target.Buffs == nil {
		return
	}
	if _, ok := target.Buffs[id]; !ok {
		return
	}

	delete(target.Buffs, id)
	r.refreshBuffStats(target)
	r.emitBuffs(target)
}

// phaseBuffs ticks every buff and drops the expired ones.
//
// Before AI and casts, so a damage-over-time that kills something this tick
// stops it acting -- the alternative is a mob that gets a free swing after the
// hit that killed it.
func (r *Room) phaseBuffs() {
	for _, e := range r.entities {
		if len(e.Buffs) == 0 {
			continue
		}
		r.tickBuffs(e)
	}
}

func (r *Room) tickBuffs(e *Entity) {
	var expired []string
	changed := false

	// Sorted iteration, because Go randomises map order and the order buffs
	// tick in decides which one lands the killing blow.
	for _, id := range e.buffOrder() {
		b := e.Buffs[id]
		if b == nil {
			continue
		}

		if b.expiresAt != 0 && r.tick >= b.expiresAt {
			expired = append(expired, id)
			continue
		}

		if len(b.def.Effects) == 0 || b.def.TickInterval <= 0 {
			continue
		}
		if r.tick < b.nextTick {
			continue
		}
		b.nextTick = r.tick + uint64(b.def.TickInterval)

		source := r.entity(b.source)
		if source == nil {
			// Whoever applied it has gone. The effect still lands -- a
			// poisoned mob does not recover because the archer logged out --
			// and it is credited to the target itself so nothing is orphaned.
			source = e
		}

		for i := range b.def.Effects {
			for s := 0; s < b.stacks; s++ {
				r.applyEffect(source, e, nil, &b.def.Effects[i], r.randFor(e.Layer))
			}
		}
		changed = true
	}

	for _, id := range expired {
		delete(e.Buffs, id)
		changed = true
	}

	if len(expired) > 0 {
		r.refreshBuffStats(e)
	}
	if changed {
		r.emitBuffs(e)
	}
}

// refreshBuffStats recomputes the buff contribution to an entity's stats.
//
// Rebuilt from scratch rather than added and subtracted: removing a modifier
// from a running product is lossy, and an incremental path that drifts gives
// stats that depend on the order buffs happened to be applied.
func (r *Room) refreshBuffStats(e *Entity) {
	if e.Player == nil {
		// Mobs carry their numbers in content and have no stat block to feed.
		// Their buffs still tick; they simply have nothing to modify yet.
		return
	}

	block := stats.NewBlock()
	if e.Player.BaseStats != nil {
		*block = *e.Player.BaseStats
	}

	for _, id := range e.buffOrder() {
		b := e.Buffs[id]
		if b == nil {
			continue
		}
		for _, mod := range b.def.StatMods {
			stat, ok := stats.Parse(mod.Stat)
			if !ok {
				continue
			}
			// Stacks multiply the modifier, so three stacks of +8% attack is
			// +24% and not three separate 8% entries nobody can read.
			for s := 0; s < b.stacks; s++ {
				block.AddAll([]stats.Modifier{
					{Stat: stat, Kind: stats.Flat, Value: stats.FromInt(mod.Flat)},
					{Stat: stat, Kind: stats.Increased, Value: stats.Value(mod.Increased)},
					{Stat: stat, Kind: stats.More, Value: stats.Value(mod.More)},
				})
			}
		}
	}

	e.Player.Stats = block
	r.applyMaxLife(e)
}

// applyMaxLife recomputes maximum health from the combined stat block and
// carries current health across the change.
//
// One place owns this, and it is here rather than where equipment arrives,
// because a buff can grant maximum life just as an item can -- and if
// equipment set the maximum directly, equipping a ring while buffed would
// quietly drop the buff's contribution.
//
// Health scales with the maximum rather than staying put: gaining maximum life
// at full health should leave you at full, and losing it should not leave you
// above your new maximum or, worse, kill you.
func (r *Room) applyMaxLife(e *Entity) {
	if e.Player == nil {
		return
	}

	previous := e.MaxHP
	e.MaxHP = maxLifeFrom(e.Player)

	switch {
	case previous == 0:
		e.HP = e.MaxHP
	case e.HP > e.MaxHP:
		e.HP = e.MaxHP
	case e.MaxHP > previous && e.HP == previous:
		e.HP = e.MaxHP
	}
}

// emitBuffs tells the owner what they are carrying.
//
// To the owner alone: a buff bar is the player's own business, and
// broadcasting every stack of every debuff on every mob to everyone in the
// room is a great deal of traffic for something nobody reads.
func (r *Room) emitBuffs(e *Entity) {
	if e.Player == nil {
		return
	}

	state := &mmov1.BuffState{EntityId: uint32(e.ID)}
	for _, id := range e.buffOrder() {
		b := e.Buffs[id]
		if b == nil {
			continue
		}
		state.Buffs = append(state.Buffs, &mmov1.BuffInstance{
			BuffId:      id,
			Name:        b.def.Name,
			Stacks:      uint32(b.stacks),
			RemainingMs: uint32(b.remaining(r.tick)) * uint32(TickPeriod.Milliseconds()),
			Harmful:     b.def.Kind == content.BuffHarmful,
		})
	}

	r.emitTo(e.ID, &mmov1.Event{Body: &mmov1.Event_Buffs{Buffs: state}})
}
