package room

import (
	"testing"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/rng"
)

// Champions and rares.
//
// The rule these protect: a champion is a real change to a fight rather than a
// name, an ordinary mob is left entirely alone, and nothing a champion does
// leaks into the shared definition every other room is reading.

// eliteMob forces a tier onto a freshly spawned mob and returns it.
func (h *harness) eliteMob(tier Tier) *Entity {
	h.t.Helper()

	def := h.game.Mobs["test_dummy"]
	e := h.room.spawnEntity(&Entity{
		Kind: KindMob, Layer: SharedLayer,
		Body: h.room.spawnBody(), HP: uint32(def.HP), MaxHP: uint32(def.HP),
		Name: def.Name,
		Mob: &MobState{
			Def: def, State: aiIdle,
			Attack: def.Attack, Armour: def.Armour,
			MoveSpeed: def.AI.MoveSpeed, Exp: def.Exp,
		},
	})

	// A stream rigged to always upgrade: the odds are what balance decides,
	// and a test that spawned mobs until one happened to be a champion would
	// be a slow test that fails on a bad seed.
	h.room.applyTier(e, tier, 2, rng.New(1))
	return e
}

func TestAChampionIsStrongerThanWhatItCameFrom(t *testing.T) {
	h := newHarness(t)
	h.join("alice")
	h.tick(1)

	def := h.game.Mobs["test_dummy"]
	e := h.eliteMob(TierChampion)

	if e.Mob.Tier != TierChampion {
		t.Fatalf("tier is %s", e.Mob.Tier)
	}
	if len(e.Mob.Elites) == 0 {
		t.Fatal("a champion rolled no modifiers, so it is a name and nothing else")
	}
	if e.MaxHP <= uint32(def.HP) {
		t.Errorf("health %d is not above the definition's %d", e.MaxHP, def.HP)
	}
	if e.HP != e.MaxHP {
		t.Errorf("spawned at %d of %d health", e.HP, e.MaxHP)
	}
	if e.Mob.Exp <= def.Exp {
		t.Errorf("worth %d experience against the definition's %d; a harder "+
			"fight that paid the same would be one to walk past", e.Mob.Exp, def.Exp)
	}
	if e.Name == def.Name {
		t.Error("named exactly like an ordinary mob, so nobody can tell before " +
			"it reaches them")
	}
}

// The definition is shared, immutable content that every room on the node
// reads concurrently. A champion that raised its own attack by editing it
// would raise it for every mob of that kind in the game.
func TestAChampionDoesNotEditItsDefinition(t *testing.T) {
	h := newHarness(t)
	h.join("alice")
	h.tick(1)

	def := h.game.Mobs["test_dummy"]
	attack, armour, hp, exp := def.Attack, def.Armour, def.HP, def.Exp
	speed := def.AI.MoveSpeed

	e := h.eliteMob(TierRare)

	if def.Attack != attack || def.Armour != armour || def.HP != hp ||
		def.Exp != exp || def.AI.MoveSpeed != speed {
		t.Error("rolling a rare changed the shared mob definition, which every " +
			"room on the node reads concurrently")
	}
	if e.Mob.Attack == def.Attack && e.Mob.Armour == def.Armour {
		t.Error("the champion's own numbers match the definition, so the copy " +
			"is being read instead of the copy being modified")
	}
}

// An ordinary mob is left completely alone. Anything else means the whole zone
// quietly drifts.
func TestAnOrdinaryMobIsUntouched(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	def := h.game.Mobs["test_dummy"]
	mob := h.aMob(alice)

	if mob.Mob.Tier != TierNormal {
		t.Skip("this mob happened to roll a tier; nothing to check")
	}
	if mob.Mob.Attack != def.Attack || mob.Mob.Armour != def.Armour {
		t.Errorf("an ordinary mob has attack %d armour %d, want %d and %d",
			mob.Mob.Attack, mob.Mob.Armour, def.Attack, def.Armour)
	}
	if mob.Name != def.Name {
		t.Errorf("an ordinary mob is called %q, want %q", mob.Name, def.Name)
	}
	if len(mob.Mob.Elites) != 0 {
		t.Errorf("an ordinary mob rolled %d modifiers", len(mob.Mob.Elites))
	}
}

// Modifiers are drawn without replacement. Two copies of Brutal is one
// modifier wasted and a name that reads like a bug.
func TestModifiersAreNotRolledTwice(t *testing.T) {
	h := newHarness(t)

	for seed := uint64(1); seed <= 50; seed++ {
		got := h.room.pickElites(len(h.game.Elites), rng.New(seed))
		seen := map[string]bool{}
		for _, e := range got {
			if seen[e.ID] {
				t.Fatalf("seed %d rolled %s twice", seed, e.ID)
			}
			seen[e.ID] = true
		}
	}
}

// Asking for more modifiers than exist gives every one of them rather than
// looping forever looking for another.
func TestAskingForMoreModifiersThanExist(t *testing.T) {
	h := newHarness(t)

	got := h.room.pickElites(len(h.game.Elites)+5, rng.New(7))
	if len(got) != len(h.game.Elites) {
		t.Errorf("got %d of %d modifiers", len(got), len(h.game.Elites))
	}
}

// A boss is its own design. Handing it Brutal on top of three phases would add
// randomness to the one fight that is supposed to be learnable.
func TestABossNeverRollsElite(t *testing.T) {
	h := newHarness(t)
	h.join("alice")
	h.tick(1)

	def := h.game.Mobs["test_boss"]
	e := h.room.spawnEntity(&Entity{
		Kind: KindMob, Layer: SharedLayer,
		Body: h.room.spawnBody(), HP: uint32(def.HP), MaxHP: uint32(def.HP),
		Name: def.Name,
		Mob: &MobState{
			Def: def, State: aiIdle,
			Attack: def.Attack, Armour: def.Armour,
			MoveSpeed: def.AI.MoveSpeed, Exp: def.Exp,
		},
	})

	// A stream that would upgrade anything else.
	for seed := uint64(1); seed <= 200; seed++ {
		h.room.rollElite(e, rng.New(seed))
		if e.Mob.Tier != TierNormal {
			t.Fatalf("a boss rolled %s on seed %d", e.Mob.Tier, seed)
		}
	}
}

// What a modifier does when the mob dies is the ordinary effect vocabulary,
// and it lands where the mob fell.
func TestAVolatileMobLeavesSomethingBehind(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	volatile := h.game.Elites["test_volatile"]
	if volatile == nil {
		t.Skip("the test content set has no on-death modifier")
	}

	mob := h.aMob(alice)
	mob.Mob.Elites = []*content.Elite{volatile}

	before := len(h.areas())
	h.room.damage(h.entity(alice), mob, int(mob.HP), false, "physical")
	h.tick(1)

	if len(h.areas()) <= before {
		t.Error("a Volatile mob died and left nothing behind")
	}
}
