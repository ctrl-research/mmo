package room

import (
	"strings"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/rng"
)

// Champions and rares.
//
// Most of what makes a zone interesting the tenth time through is that it is
// not the same zone. A champion is an ordinary mob that rolled modifiers from
// a shared pool, so the variety is combinatorial and costs nothing to author:
// two slimes that rolled Brutal and Volatile are a different fight from two
// that did not, and nobody wrote that fight.

// Tier is how dangerous a mob turned out to be.
type Tier uint8

const (
	TierNormal Tier = iota
	TierChampion
	TierRare
)

func (t Tier) String() string {
	switch t {
	case TierChampion:
		return "champion"
	case TierRare:
		return "rare"
	default:
		return "normal"
	}
}

// rollElite decides a mob's tier and modifiers, and applies them.
//
// Rolled at spawn from the layer's own generator, so a replay produces the
// same monsters -- and so one player's luck with champions never depends on
// how many other people happen to be in the room.
func (r *Room) rollElite(e *Entity, source *rng.Source) {
	// A boss is its own design. Handing it Brutal on top of three phases would
	// be adding randomness to the one fight that is supposed to be learnable.
	if e.Mob.Def.AI.Profile == content.AIBoss || len(r.content.Elites) == 0 {
		return
	}

	balance := r.content.Balance.Elites

	// Rare first, and its chance is a subset of champion's rather than a
	// competitor: rolling them independently would make "rare" mean "champion
	// that also passed a second check", which is a different number from the
	// one written in the balance file.
	switch {
	case source.PPM(balance.RareChance):
		r.applyTier(e, TierRare, source.Range(balance.RareMods[0], balance.RareMods[1]), source)
	case source.PPM(balance.ChampionChance):
		r.applyTier(e, TierChampion, source.Range(balance.ChampionMods[0], balance.ChampionMods[1]), source)
	}
}

// applyTier upgrades a mob to a tier with a given number of modifiers.
//
// Separate from the roll so that what a champion *is* can be tested without
// fighting the odds of becoming one -- a test that spawned mobs until one
// happened to be a champion would be slow and would fail on a bad seed.
func (r *Room) applyTier(e *Entity, tier Tier, count int, source *rng.Source) {
	balance := r.content.Balance.Elites
	m := e.Mob
	m.Tier = tier

	life, exp := balance.ChampionLife, balance.ChampionExp
	if tier == TierRare {
		life, exp = balance.RareLife, balance.RareExp
	}

	m.Elites = r.pickElites(count, source)
	for _, elite := range m.Elites {
		m.Attack = increase(m.Attack, elite.Attack)
		m.Armour = increase(m.Armour, elite.Armour)
		m.MoveSpeed = increaseF(m.MoveSpeed, elite.MoveSpeed)
		life += elite.Life
	}

	// Health last, so the tier's multiplier and every modifier's contribution
	// are applied to the mob's own maximum exactly once.
	e.MaxHP = uint32(increase(int(e.MaxHP), life))
	e.HP = e.MaxHP
	m.Exp = int64(increase(int(m.Exp), exp))

	e.Name = eliteName(e.Name, m.Elites)
}

// pickElites draws distinct modifiers by weight.
func (r *Room) pickElites(count int, source *rng.Source) []*content.Elite {
	pool, weights := r.content.EliteOrder()

	out := make([]*content.Elite, 0, min(count, len(pool)))
	for len(out) < count {
		// Zero-weighted after being drawn, so a modifier is never rolled
		// twice -- two copies of Brutal is one modifier wasted and a name that
		// reads like a bug. Asking for more than exist therefore exhausts the
		// pool and Pick reports that there is nothing left, which is also what
		// ends the loop.
		i := source.Pick(weights)
		if i < 0 {
			break
		}
		out = append(out, pool[i])
		weights[i] = 0
	}
	return out
}

// eliteName prefixes a mob's name with what it rolled.
//
// The name is the whole of the warning. A player who can read "Brutal Swift
// Green Slime" before it reaches them can decide not to fight it, and that
// decision is the point of the tier existing.
func eliteName(base string, elites []*content.Elite) string {
	if len(elites) == 0 {
		return base
	}

	var b strings.Builder
	for _, e := range elites {
		b.WriteString(e.Name)
		b.WriteByte(' ')
	}
	b.WriteString(base)
	return b.String()
}

// eliteDeath runs whatever the modifiers do when the mob dies.
func (r *Room) eliteDeath(victim *Entity) {
	for _, elite := range victim.Mob.Elites {
		for i := range elite.OnDeath {
			// The victim is both caster and target: an area lands where it
			// fell, which is the whole point of Volatile.
			r.applyEffect(victim, victim, nil, &elite.OnDeath[i], r.randFor(victim.Layer))
		}
	}
}

// increase applies a fixed-point increase to a whole number.
func increase(base int, by fixed.F) int {
	if by == 0 {
		return base
	}
	return (fixed.FromInt(base) + fixed.FromInt(base).Mul(by)).Int()
}

// increaseF applies a fixed-point increase to a fixed-point value.
func increaseF(base, by fixed.F) fixed.F {
	if by == 0 {
		return base
	}
	return base + base.Mul(by)
}
