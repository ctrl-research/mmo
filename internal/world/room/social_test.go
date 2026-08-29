package room

import (
	"testing"
)

// What a party changes inside a room: one mob population instead of one each,
// experience split with whoever was there, and a say who may pick loot up.

// expGained totals the experience a player was told they earned.
//
// Read from the events rather than from Player.Exp, because levelling
// subtracts the cost of the level from the running total -- so the field
// answers "how far into this level" and not "how much did that kill pay".
func (h *harness) expGained(id EntityID) uint64 {
	sink := h.sinks[id]
	if sink == nil {
		return 0
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()

	var total uint64
	for _, m := range sink.msgs {
		ev := m.GetEvent()
		if ev == nil {
			continue
		}
		if g := ev.GetExpGained(); g != nil {
			total += g.GetAmount()
		}
	}
	return total
}

// partyUp puts two players in one layer, which is what partying means here.
func (h *harness) partyUp(a, b EntityID, key string) {
	h.t.Helper()
	h.room.moveToLayer(a, key)
	h.room.moveToLayer(b, key)
}

func TestPartyingUpMergesMobPopulations(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	bob, _ := h.join("bob")
	h.tick(5)

	before := len(h.mobs(0, true))
	if before == 0 {
		t.Fatal("setup: no mobs spawned")
	}

	h.partyUp(alice, bob, "party-1")
	h.tick(5)

	if h.entity(alice).HuntLayer != h.entity(bob).HuntLayer {
		t.Fatal("party members are still in separate layers")
	}

	// One population where there were two. The merged layer spawns its own,
	// so the count is not exactly halved, but it must be well under the two
	// separate populations it replaced.
	after := len(h.mobs(0, true))
	if after >= before {
		t.Errorf("the room holds %d mobs after partying and %d before; merging "+
			"layers should have removed a population, not added one", after, before)
	}
}

func TestLeavingAPartyGivesBackAPrivatePopulation(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	bob, _ := h.join("bob")

	h.partyUp(alice, bob, "party-1")
	h.tick(5)
	shared := h.entity(alice).HuntLayer

	h.room.moveToLayer(bob, "char-bob")
	h.tick(5)

	if h.entity(bob).HuntLayer == shared {
		t.Error("a player who left the party is still in its layer")
	}
	if h.entity(alice).HuntLayer != shared {
		t.Error("the member who stayed was moved out of the party's layer")
	}
}

// Losing a drop because a friend invited you to a party is the kind of thing
// players remember.
func TestGroundLootFollowsAPlayerBetweenLayers(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(3)

	// Kill something to put loot on the ground, then keep killing until a
	// drop lands: drop tables are chance-based.
	var dropped bool
	for attempt := 0; attempt < 60 && !dropped; attempt++ {
		mobs := h.mobs(h.entity(alice).HuntLayer, false)
		if len(mobs) == 0 {
			h.tick(20)
			continue
		}
		target := mobs[0]
		h.placeAt(alice, target.Body.FeetCenter().X.Int(), target.Body.FeetCenter().Y.Int())
		for i := 0; i < 40 && target.Mob.State != aiDead; i++ {
			h.cast(alice, "slash", false)
			h.tick(3)
		}
		dropped = len(h.drops()) > 0
		h.tick(5)
	}
	if !dropped {
		t.Skip("no drop rolled in the attempts available; nothing to migrate")
	}

	before := len(h.drops())
	h.room.moveToLayer(alice, "party-1")

	if after := len(h.drops()); after != before {
		t.Errorf("%d drops survived the layer change, want all %d", after, before)
	}
	for _, d := range h.drops() {
		if d.Layer != h.entity(alice).HuntLayer {
			t.Error("a drop was left behind in the layer the player left")
		}
	}
}

// --- experience --------------------------------------------------------------

func TestExperienceIsSharedWithinAPartyLayer(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	bob, _ := h.join("bob")

	h.partyUp(alice, bob, "party-1")
	h.tick(5)

	mobs := h.mobs(h.entity(alice).HuntLayer, false)
	if len(mobs) == 0 {
		t.Fatal("setup: the party layer has no mobs")
	}
	victim := mobs[0]

	// Both standing on the kill, so both are inside the share radius.
	at := victim.Body.FeetCenter()
	h.placeAt(alice, at.X.Int(), at.Y.Int())
	h.placeAt(bob, at.X.Int(), at.Y.Int())

	h.room.awardKill(h.entity(alice), victim)
	h.tick(1)

	aliceExp := h.expGained(alice)
	bobExp := h.expGained(bob)
	if bobExp == 0 {
		t.Fatal("a party member standing on the kill earned nothing")
	}
	if aliceExp < bobExp {
		t.Errorf("the killer earned %d and the other member %d; the remainder "+
			"should favour the killer", aliceExp, bobExp)
	}
	if total := int64(aliceExp + bobExp); total != victim.Mob.Def.Exp {
		t.Errorf("the party earned %d between them for a mob worth %d",
			total, victim.Mob.Def.Exp)
	}
}

// A party member who fast-travelled away mid-fight did not help.
func TestExperienceIsNotSharedOutOfRange(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	bob, _ := h.join("bob")

	h.partyUp(alice, bob, "party-1")
	h.tick(5)

	mobs := h.mobs(h.entity(alice).HuntLayer, false)
	if len(mobs) == 0 {
		t.Fatal("setup: the party layer has no mobs")
	}
	victim := mobs[0]
	at := victim.Body.FeetCenter()

	h.placeAt(alice, at.X.Int(), at.Y.Int())
	reach := h.game.Balance.Party.ExpShareRange.Int()
	h.placeAt(bob, at.X.Int()+reach+64, at.Y.Int())

	h.room.awardKill(h.entity(alice), victim)
	h.tick(1)

	if got := h.expGained(bob); got != 0 {
		t.Errorf("a member %d units away earned %d experience", reach+64, got)
	}
	if got := int64(h.expGained(alice)); got != victim.Mob.Def.Exp {
		t.Errorf("the killer earned %d of a mob worth %d; nobody else was in "+
			"range, so it should all be theirs", got, victim.Mob.Def.Exp)
	}
}

// Nobody outside the party earns from its kills, however close they stand.
func TestExperienceIsNotSharedAcrossLayers(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	bob, _ := h.join("bob")
	h.tick(5)

	mobs := h.mobs(h.entity(alice).HuntLayer, false)
	if len(mobs) == 0 {
		t.Fatal("setup: no mobs in Alice's layer")
	}
	victim := mobs[0]

	at := victim.Body.FeetCenter()
	h.placeAt(bob, at.X.Int(), at.Y.Int())

	h.room.awardKill(h.entity(alice), victim)
	h.tick(1)

	if got := h.expGained(bob); got != 0 {
		t.Errorf("an unpartied bystander earned %d experience from somebody "+
			"else's kill", got)
	}
}

// --- loot rules --------------------------------------------------------------

func TestRoundRobinAssignsDropsInTurn(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	bob, _ := h.join("bob")

	h.partyUp(alice, bob, "party-1")
	h.room.setLootRule("party-1", LootRoundRobin)
	h.tick(3)

	layer := h.entity(alice).HuntLayer
	seen := map[EntityID]int{}
	for i := 0; i < 8; i++ {
		seen[h.room.nextLooter(layer, alice)]++
	}

	if len(seen) != 2 {
		t.Fatalf("round-robin handed drops to %d of 2 members", len(seen))
	}
	if seen[alice] == 0 || seen[bob] == 0 {
		t.Errorf("the turns went %v; both members should get some", seen)
	}
}

// Free-for-all is the default, and a layer of one has nobody to take turns
// with, so both leave the drop with its killer and no lock.
func TestFreeForAllLeavesDropsWithTheKiller(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	bob, _ := h.join("bob")

	h.partyUp(alice, bob, "party-1")
	h.tick(3)

	layer := h.entity(alice).HuntLayer
	for i := 0; i < 4; i++ {
		if got := h.room.nextLooter(layer, alice); got != alice {
			t.Fatalf("free-for-all assigned a drop to %d, want the killer %d", got, alice)
		}
	}
}

func TestRoundRobinSkipsMembersWhoAreNotHere(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.room.moveToLayer(alice, "party-1")
	h.room.setLootRule("party-1", LootRoundRobin)
	h.tick(3)

	// A party whose other members are in another room cannot take turns with
	// them: a drop assigned to somebody who is not here is loot nobody can
	// reach.
	layer := h.entity(alice).HuntLayer
	if got := h.room.nextLooter(layer, alice); got != alice {
		t.Errorf("a drop went to %d with only one member present", got)
	}
}
