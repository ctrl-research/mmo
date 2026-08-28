package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
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
}

func newPlayerState() *PlayerState {
	return &PlayerState{
		Level:     1,
		MP:        50,
		MaxMP:     50,
		Cooldowns: make(map[string]uint64),
	}
}

// Attack is the player's offensive stat.
//
// Derived from level alone until equipment and the full base/increased/more
// pipeline arrive in M3. Keeping it behind a method means the call sites do
// not change when the real stat engine replaces this.
func (p *PlayerState) Attack() int { return 5 + p.Level*2 }

// Armour is the player's mitigation, likewise a placeholder for M3.
func (p *PlayerState) Armour() int { return p.Level }

// MaxHPFor returns the hit points a character has at a level.
func MaxHPFor(level int) uint32 { return uint32(100 + (level-1)*20) }

// awardKill grants experience for a kill and handles any resulting levels.
func (r *Room) awardKill(killer *Entity, victim *Entity) {
	if killer.Player == nil || victim.Mob == nil {
		return
	}

	amount := victim.Mob.Def.Exp
	if amount <= 0 {
		return
	}

	p := killer.Player
	p.Exp += amount

	r.emitTo(killer.ID, &mmov1.Event{Body: &mmov1.Event_ExpGained{ExpGained: &mmov1.ExpGained{
		Amount: uint64(amount),
		Total:  uint64(p.Exp),
	}}})

	r.applyLevels(killer)
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
		e.MaxHP = MaxHPFor(p.Level)
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
