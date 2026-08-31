package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
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

	// Secondary is cumulative experience per secondary skill: woodcutting,
	// mining, and the rest. Cumulative and never spent, unlike Exp above --
	// the level is derived from the total rather than the total being
	// decremented as levels are taken, which is OSRS's arrangement and the
	// reason a secondary skill can never lose a level to a rounding change in
	// the curve.
	//
	// Held here rather than in the session for the same reason as everything
	// else on this struct: a gather resolves inside the tick, and the tick
	// cannot read a database. The session loads it on join and persists what
	// the room reports.
	Secondary map[string]int64

	// ToolPower is the power of the tool in hand per secondary skill, pushed
	// in by the session alongside the stat block.
	//
	// The room never derives it: that would mean knowing which item is in
	// which slot, and item state belongs where it can be written durably. It
	// arrives with the stat block because it changes on exactly the same event
	// -- somebody equipped something.
	ToolPower map[string]int

	// Cooldowns maps a skill to the tick from which it may be cast again.
	Cooldowns map[string]uint64

	// Loadout is what this character may cast, keyed by skill id: the rank it
	// is known at, and the supports linked to it.
	//
	// Held here rather than looked up per cast because a cast happens inside
	// the tick and the loadout lives in the database. The session pushes it in
	// on login and whenever it changes, the same arrangement as stats.
	Loadout map[string]*Linked

	// BaseStats is what the session computed from level, equipment, and
	// passives, pushed in whenever any of those change. The room never
	// computes it: that would mean the room knowing about items, and item
	// state belongs where it can be written durably.
	BaseStats *stats.Block

	// ReviveAt is the tick from which a downed character may return, and zero
	// for one who is up. See death.go.
	ReviveAt uint64

	// SafeUntil is the tick until which a character who has just come back
	// cannot be harmed. Ends early the moment they attack.
	SafeUntil uint64

	// InCombatUntil is the tick until which this character counts as fighting,
	// which slows mana regeneration and stops health regeneration entirely.
	// Extended by taking damage. See regen.go.
	InCombatUntil uint64

	// ManaCarry and LifeCarry hold the fraction of a point regeneration has
	// accumulated but not yet handed over, in millionths. Without them a rate
	// that works out to less than one point a second is either zero or one,
	// and at low levels every rate works out to less than one point a second.
	ManaCarry int64
	LifeCarry int64

	// Stats is BaseStats with the character's buffs layered on, recomputed
	// inside the room whenever a buff is applied or expires.
	//
	// Two blocks rather than one because they change on completely different
	// clocks: equipment changes when a player equips something, and buffs
	// change several times a second in a fight. Rebuilding the whole thing
	// from items every time a stack of Burning ticked would mean the room
	// asking the session for stats mid-tick.
	Stats *stats.Block
}

func newPlayerState() *PlayerState {
	return &PlayerState{
		Level:     1,
		MP:        50,
		MaxMP:     50,
		Secondary: make(map[string]int64),
		ToolPower: make(map[string]int),
		Cooldowns: make(map[string]uint64),
		Loadout:   make(map[string]*Linked),
		BaseStats: stats.NewBlock(),
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

	amount := victim.Mob.Exp
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
func (r *Room) canCast(e *Entity, linked *Linked) bool {
	if e.Player == nil || !isAlive(e) {
		return false
	}
	if ready, ok := e.Player.Cooldowns[linked.Skill.ID]; ok && r.tick < ready {
		// A cast arriving a tick early is a normal consequence of latency, not
		// an error worth reporting -- the client will try again.
		return false
	}
	// The cost after supports, not the skill's own: every support costs
	// something, and charging the base cost would make them free.
	if e.Player.MP < uint32(linked.CostMP) {
		return false
	}
	return true
}

// beginCast pays a skill's cost and starts its cooldown.
func (r *Room) beginCast(e *Entity, linked *Linked) {
	e.Player.MP -= uint32(linked.CostMP)
	e.Player.Cooldowns[linked.Skill.ID] = r.tick + uint64(linked.Skill.Cooldown)
}

// installLoadout resolves a loadout onto an entity that is being joined.
//
// Separate from setLoadout because that one looks the player up by entity ID,
// and during a join the player is not in the room's table yet.
func (r *Room) installLoadout(e *Entity, slots []LoadoutSlot) {
	if e.Player == nil {
		return
	}

	loadout := make(map[string]*Linked, len(slots))
	for _, slot := range slots {
		if linked := r.resolveSlot(slot); linked != nil {
			loadout[slot.SkillID] = linked
		}
	}
	e.Player.Loadout = loadout
}

// resolveSlot turns ids into a resolved, supported skill.
//
// Unknown ids are skipped rather than refused: content changes under saved
// characters, and a bar entry for a skill that no longer exists should cost a
// player one button rather than the ability to log in.
func (r *Room) resolveSlot(slot LoadoutSlot) *Linked {
	skill, ok := r.content.Skills[slot.SkillID]
	if !ok {
		return nil
	}

	supports := make([]*content.Support, 0, len(slot.Supports))
	for _, supportID := range slot.Supports {
		if support, ok := r.content.Supports[supportID]; ok {
			supports = append(supports, support)
		}
	}
	return link(skill, slot.Rank, supports)
}

// setLoadout installs what a character may cast.
//
// Resolved here, once, rather than per cast: applying every support to every
// effect on every swing would be real work inside the tick for an answer that
// changes only when somebody rearranges their bar.
func (r *Room) setLoadout(id EntityID, slots []LoadoutSlot) {
	p, ok := r.players[id]
	if !ok || p.entity.Player == nil {
		return
	}

	r.installLoadout(p.entity, slots)
}

// LoadoutSlot is one entry on a character's skill bar, as the session knows
// it: ids rather than resolved content, because the session reads it from the
// database and the room owns the content.
type LoadoutSlot struct {
	SkillID  string
	Rank     int
	Supports []string
}

// Linked is one skill as the character has it set up.
//
// Effects are resolved once, when the loadout is pushed in, rather than per
// cast: applying every support to every effect on every swing would be real
// work inside the tick for an answer that only changes when somebody
// rearranges their bar.
type Linked struct {
	Skill *content.Skill
	Rank  int

	// Supports are the modifiers linked to this skill, in the order they
	// apply. Order matters: two supports that both scale damage compose
	// differently depending on which repeats first.
	Supports []*content.Support

	// Effects is the skill's effect list with every support applied.
	Effects []content.Effect

	// CostMP is the skill's cost after the supports' multipliers. Every
	// support costs something, or the choice of which to use is not a choice.
	CostMP int
}

// link resolves a skill and its supports into what a cast actually does.
func link(skill *content.Skill, rank int, supports []*content.Support) *Linked {
	if rank <= 0 {
		rank = 1
	}
	if rank > skill.MaxRank {
		rank = skill.MaxRank
	}

	l := &Linked{Skill: skill, Rank: rank, Supports: supports}

	// Rank first, then supports. A support that multiplies damage should
	// multiply the ranked damage, not the rank-one damage -- otherwise a
	// support is worth progressively less the more a skill is levelled, which
	// is the opposite of what anybody expects.
	effects := make([]content.Effect, len(skill.Effects))
	copy(effects, skill.Effects)
	for i := range effects {
		effects[i] = applyRank(effects[i], rank)
	}

	cost := fixed.FromInt(skill.CostMP)
	for _, support := range supports {
		if support == nil || !support.Attaches(skill) {
			// Refused rather than applied: a client can ask for any link, and
			// the tags are the rule that makes a support a choice.
			continue
		}
		effects = support.Apply(effects)
		cost = cost.Mul(support.ManaMult)
	}

	l.Effects = effects
	l.CostMP = cost.Int()
	return l
}

// applyRank scales an effect for the rank the skill is known at.
//
// Recursive, so a projectile's payload gains rank with the skill that launches
// it -- otherwise levelling a projectile skill would raise nothing at all.
func applyRank(e content.Effect, rank int) content.Effect {
	if rank > 1 && e.PerRankPct > 0 {
		bonus := fixed.One + fixed.FromRatio(e.PerRankPct*(rank-1), 100)
		e.BaseMin = fixed.FromInt(e.BaseMin).Mul(bonus).Int()
		e.BaseMax = fixed.FromInt(e.BaseMax).Mul(bonus).Int()
	}

	if len(e.Effects) > 0 {
		nested := make([]content.Effect, len(e.Effects))
		for i := range e.Effects {
			nested[i] = applyRank(e.Effects[i], rank)
		}
		e.Effects = nested
	}
	return e
}
