package room

import "testing"

// Regeneration.
//
// The rule these are protecting: a caster who spends their mana can always
// cast again eventually, and a fight is paced rather than waited out.

func TestManaComesBack(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	e.Player.MP = 0

	h.tick(3 * TickRate)

	if e.Player.MP == 0 {
		t.Error("a character who spent their mana never got any back, so a " +
			"caster who runs dry can never attack again")
	}
}

func TestManaRegeneratesSlowerInCombat(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	bob, _ := h.join("bob")
	h.tick(2)

	rested, fighting := h.entity(alice), h.entity(bob)
	rested.Player.MP, fighting.Player.MP = 0, 0

	for i := 0; i < 12*TickRate; i++ {
		// Kept in combat by being hit, which is what marks it -- one small hit
		// a second, well inside the combat window.
		if i%TickRate == 0 {
			h.room.damage(rested, fighting, 1, false, "physical")
		}
		h.tick(1)
	}

	if fighting.Player.MP >= rested.Player.MP {
		t.Errorf("in combat %d mana, out of combat %d; a fight should be "+
			"something to pace, not something to wait out",
			fighting.Player.MP, rested.Player.MP)
	}
	if fighting.Player.MP == 0 {
		t.Error("no mana at all in combat, so a long fight is unwinnable for a caster")
	}
}

// Health returns out of combat only. Regenerating mid-fight would undo the
// fight.
func TestHealthDoesNotComeBackInCombat(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	bob, _ := h.join("bob")
	h.tick(2)

	e := h.entity(alice)
	e.HP = e.MaxHP / 2
	hurt := e.HP

	for i := 0; i < 3*TickRate; i++ {
		if i%TickRate == 0 {
			// A hit that lands for nothing after mitigation still marks combat.
			h.room.damage(h.entity(bob), e, 1, false, "physical")
			hurt = e.HP
		}
		h.tick(1)
	}

	if e.HP > hurt {
		t.Errorf("healed from %d to %d while being hit", hurt, e.HP)
	}
}

func TestHealthComesBackOutOfCombat(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	e.HP = 1

	h.tick(h.room.content.Balance.Combat.CombatTicks + 4*TickRate)

	if e.HP <= 1 {
		t.Error("a character left alone never healed")
	}
}

// A small pool must still move. A level one character with 50 mana and a 0.4%
// in-combat rate would otherwise gain exactly zero, forever.
func TestASmallPoolStillRegenerates(t *testing.T) {
	// A fifth of a point a second: nothing on any single call, one point every
	// five. Rounding either way would make this rate meaningless.
	var carry int64
	mp := uint32(0)
	for i := 0; i < 4; i++ {
		mp = restoreShare(mp, 50, 4000, &carry)
	}
	if mp != 0 {
		t.Errorf("gained %d in four seconds at a fifth of a point a second", mp)
	}
	if mp = restoreShare(mp, 50, 4000, &carry); mp != 1 {
		t.Errorf("gained %d after five seconds, want exactly 1", mp)
	}
}

func TestRestoreShareCapsAndIgnoresZero(t *testing.T) {
	var carry int64
	if got := restoreShare(49, 50, 100_000, &carry); got != 50 {
		t.Errorf("restored to %d, want the cap of 50", got)
	}

	// Part-way to the next point, and then full. The fraction must be dropped
	// rather than banked: kept, it would hand back a free point the instant
	// anything was spent.
	carry = 900_000
	if got := restoreShare(50, 50, 4000, &carry); got != 50 {
		t.Errorf("a full pool went to %d", got)
	}
	if carry != 0 {
		t.Errorf("a full pool banked %d, which would burst the moment it was spent", carry)
	}
	if got := restoreShare(10, 50, 0, &carry); got != 10 {
		t.Errorf("a zero rate restored to %d", got)
	}
}

// The dead do not recover, and neither does somebody whose connection has
// gone -- coming back to a full bar for having been away would make dropping
// out the best way to rest.
func TestTheDownedAndTheAbsentDoNotRegenerate(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	bob, _ := h.join("bob")
	h.tick(2)

	down := h.down(alice)
	down.Player.MP = 0

	gone := h.entity(bob)
	gone.Player.MP = 0
	h.room.handle(freezeCmd{id: bob})

	// Large pools, so even the slow in-combat rate would move them visibly in
	// the window below. At the starting fifty, a fifth of a point a second
	// accrues to nothing here and the test would pass without the guard.
	for _, e := range []*Entity{down, gone} {
		e.Player.MaxMP = 5000
	}

	// Well inside the revive clock: coming back restores mana on purpose, and
	// this is about what happens while they are still down.
	h.tick(h.room.content.Balance.Combat.DownedTicks / 2)

	if !isDowned(down) {
		t.Fatal("the character revived mid-test, so this proved nothing")
	}
	if down.Player.MP != 0 {
		t.Errorf("a downed character regenerated to %d mana", down.Player.MP)
	}
	if gone.Player.MP != 0 {
		t.Errorf("a disconnected character regenerated to %d mana", gone.Player.MP)
	}
}
