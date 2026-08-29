package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// PlayerState is a character's progression and derived stats.
//
// It lives on the entity rather than in the session so that everything the
// simulation needs is reachable from the entity list, and so a character can
// be serialised for room handoff as one object.
type PlayerState struct {
	Level int
	Exp   int64
	MP    uint32
	MaxMP uint32
	Gold  int64

	// Cooldowns maps a skill to the tick from which it may be cast again.
	Cooldowns map[string]uint64

	// Stats is the character's derived statistics, computed by the session
	// from level and equipment and pushed in whenever either changes.
	//
	// The room never computes it: doing so would mean the room knowing about
	// items, and item state belongs where it can be written durably.
	Stats *stats.Block
}

func newPlayerState() *PlayerState {
	return &PlayerState{
		Level:     1,
		MP:        50,
		MaxMP:     50,
		Cooldowns: make(map[string]uint64),
		Stats:     stats.NewBlock(),
	}
}

// Attack is the player's offensive stat, after equipment.
func (p *PlayerState) Attack() int {
	if p.Stats == nil {
		return 5 + p.Level*2
	}
	return p.Stats.IntClampedNonNegative(stats.Attack)
}

// Armour is the player's mitigation, after equipment.
func (p *PlayerState) Armour() int {
	if p.Stats == nil {
		return p.Level
	}
	return p.Stats.IntClampedNonNegative(stats.Armour)
}

// CritChance is the probability of a critical hit, in stat millionths.
func (p *PlayerState) CritChance() stats.Value {
	if p.Stats == nil {
		return 0
	}
	return p.Stats.Value(stats.CritChance)
}

// CritMultiplier is the damage multiplier on a critical hit.
func (p *PlayerState) CritMultiplier() stats.Value {
	if p.Stats == nil {
		return stats.FromPercent(150)
	}
	return p.Stats.Value(stats.CritMultiplier)
}

// MaxHPFor returns the hit points a character has at a level, before any
// equipment. The stat block is authoritative once one exists.
func MaxHPFor(level int) uint32 { return uint32(100 + (level-1)*20) }

// maxLifeFrom returns the hit points a character has, including equipment.
func maxLifeFrom(p *PlayerState) uint32 {
	if p.Stats == nil {
		return MaxHPFor(p.Level)
	}
	if v := p.Stats.IntClampedNonNegative(stats.MaxLife); v > 0 {
		return uint32(v)
	}
	return MaxHPFor(p.Level)
}

// awardKill grants experience for a kill and handles any resulting levels.
//
// Shared with whoever was there to help. Partied members hunt in one layer, so
// "in the killer's layer and within range" is exactly the set of people who
// could have contributed -- no damage tracking, no tap rules, and nothing to
// tune, because the layer already answers the question.
func (r *Room) awardKill(killer *Entity, victim *Entity) {
	if killer.Player == nil || victim.Mob == nil {
		return
	}

	amount := victim.Mob.Def.Exp
	if amount <= 0 {
		return
	}

	share := r.expShare(killer, victim)

	// Split evenly, with the remainder to the killer. Splitting evenly is the
	// point: a party that has to argue about contribution is not a party.
	each := amount / int64(len(share))
	remainder := amount - each*int64(len(share))

	for _, e := range share {
		gained := each
		if e.ID == killer.ID {
			gained += remainder
		}
		if gained <= 0 {
			continue
		}

		e.Player.Exp += gained
		r.emitTo(e.ID, &mmov1.Event{Body: &mmov1.Event_ExpGained{ExpGained: &mmov1.ExpGained{
			Amount: uint64(gained),
			Total:  uint64(e.Player.Exp),
			// Named so a player can tell a share from a solo kill, which is
			// the difference between "the numbers look wrong" and "my party
			// is nearby".
			Shared: len(share) > 1,
		}}})

		r.applyLevels(e)
	}
}

// expShare returns everyone who earns from a kill: the killer, plus any player
// sharing their layer who was close enough to have been part of the fight.
//
// Range rather than the whole room, because a party member who fast-travelled
// away mid-fight did not help, and a party that earns exp for being logged in
// is a party that plays itself.
func (r *Room) expShare(killer, victim *Entity) []*Entity {
	share := []*Entity{killer}
	if killer.HuntLayer == SharedLayer {
		// A field boss is shared by everyone in the room, so layer says
		// nothing about who was helping. Credit stays with the killer until
		// there is damage attribution to do better.
		return share
	}

	reach := r.content.Balance.Party.ExpShareRange
	at := victim.Body.FeetCenter()

	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.frozen || id == killer.ID {
			continue
		}
		if p.entity.HuntLayer != killer.HuntLayer {
			continue
		}
		feet := p.entity.Body.FeetCenter()
		if (feet.X-at.X).Abs() > reach || (feet.Y-at.Y).Abs() > reach {
			continue
		}
		share = append(share, p.entity)
	}
	return share
}

// applyLevels advances a character through as many levels as their experience
// allows.
//
// A loop rather than a single check, because one kill can cross more than one
// level at low levels, and awarding only one would silently swallow the rest.
func (r *Room) applyLevels(e *Entity) {
	p := e.Player
	curves := r.content.Curves

	for {
		need, ok := curves.ExpToNext(p.Level)
		if !ok || p.Exp < need {
			break
		}

		p.Exp -= need
		p.Level++

		// Levelling restores the character, which is both a reward and what
		// makes a level-up mid-fight feel like one.
		e.MaxHP = maxLifeFrom(p)
		e.HP = e.MaxHP
		p.MaxMP = uint32(50 + (p.Level-1)*10)
		p.MP = p.MaxMP

		next, _ := curves.ExpToNext(p.Level)
		r.emitTo(e.ID, &mmov1.Event{Body: &mmov1.Event_LevelUp{LevelUp: &mmov1.LevelUp{
			Level:     uint32(p.Level),
			ExpToNext: uint64(next),
		}}})

		r.log.Info("player levelled", "entity", uint32(e.ID), "name", e.Name, "level", p.Level)
	}
}

// expToNext reports the requirement for a character's current level.
func (r *Room) expToNext(p *PlayerState) int64 {
	need, ok := r.content.Curves.ExpToNext(p.Level)
	if !ok {
		return 0
	}
	return need
}

// canCast validates a cast request. Every reason a cast can fail is checked
// here, server-side, against the server's own state.
func (r *Room) canCast(e *Entity, skill *content.Skill) bool {
	if e.Player == nil || !isAlive(e) {
		return false
	}
	if ready, ok := e.Player.Cooldowns[skill.ID]; ok && r.tick < ready {
		// A cast arriving a tick early is a normal consequence of latency, not
		// an error worth reporting -- the client will try again.
		return false
	}
	if e.Player.MP < uint32(skill.CostMP) {
		return false
	}
	return true
}

// beginCast pays a skill's cost and starts its cooldown.
func (r *Room) beginCast(e *Entity, skill *content.Skill) {
	e.Player.MP -= uint32(skill.CostMP)
	e.Player.Cooldowns[skill.ID] = r.tick + uint64(skill.Cooldown)
}

// classSkills returns the skills a class may cast.
//
// M1 grants every character the one starter skill. The skill tree that decides
// this properly arrives in M6.
func starterSkill(c *content.Content) *content.Skill {
	return c.Skills["slash"]
}
