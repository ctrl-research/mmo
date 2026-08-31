package world

import (
	"context"
	"testing"

	"github.com/ctrl-research/mmo/internal/content/contenttest"
	"github.com/ctrl-research/mmo/internal/rng"
	"github.com/ctrl-research/mmo/internal/store/storetest"
	"github.com/ctrl-research/mmo/internal/world/items"
	"github.com/google/uuid"
)

// Granting into the inventory.
//
// The rule these protect is stacking, and it is a rule that only became
// load-bearing with gathering. While loot was the only source of materials a
// slot per unit was invisible -- a boar drops one hide -- and it fills a
// 24-slot bag in about two minutes once a player can chop for twenty.

func testInventory(t *testing.T) (*Inventory, uuid.UUID) {
	t.Helper()

	st := storetest.New(t)
	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load content: %v", err)
	}

	account, _, err := st.UpsertIdentity(context.Background(), "test", "granter", "granter@example.test")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	ch, err := st.CreateCharacter(context.Background(), account.ID, "Granter", "warrior", "test")
	if err != nil {
		t.Fatalf("character: %v", err)
	}

	inv, err := LoadInventory(context.Background(), st, game, ch.ID)
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	return inv, ch.ID
}

// grant rolls and grants one unit of a base item.
func grant(t *testing.T, inv *Inventory, base string, qty int) uuid.UUID {
	t.Helper()

	gen := inv.Generator()
	inst, err := gen.RollBase(rng.New(1), base, 1)
	if err != nil {
		t.Fatalf("roll %s: %v", base, err)
	}
	inst.Stack = qty

	id, err := inv.Grant(context.Background(), inst, 1)
	if err != nil {
		t.Fatalf("grant %s: %v", base, err)
	}
	return id
}

// carried returns the stack size of every slot, keyed by base id.
func carried(inv *Inventory) map[string][]int {
	slots, _ := inv.Snapshot()
	out := make(map[string][]int)
	for _, s := range slots {
		if s == nil || s.Instance == nil {
			continue
		}
		out[s.Instance.BaseID] = append(out[s.Instance.BaseID], s.Instance.Stack)
	}
	return out
}

// The whole point: repeated grants of a stackable material occupy one slot.
func TestGrantingAStackableMaterialFillsOneSlot(t *testing.T) {
	inv, _ := testInventory(t)

	for i := 0; i < 10; i++ {
		grant(t, inv, "test.log", 1)
	}

	stacks := carried(inv)["test.log"]
	if len(stacks) != 1 {
		t.Fatalf("ten logs occupy %d slots (%v), want 1", len(stacks), stacks)
	}
	if stacks[0] != 10 {
		t.Errorf("the stack holds %d, want 10", stacks[0])
	}
}

// The in-memory view has to follow, or the client is told the stack is still
// at one while the database says ten.
func TestGrantingAStackKeepsTheInMemoryViewInStep(t *testing.T) {
	inv, _ := testInventory(t)

	grant(t, inv, "test.log", 3)
	grant(t, inv, "test.log", 4)

	if got := carried(inv)["test.log"]; len(got) != 1 || got[0] != 7 {
		t.Errorf("the in-memory view holds %v, want a single stack of 7", got)
	}

	// And it matches what a reload from the database produces, which is the
	// authority.
	if err := inv.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := carried(inv)["test.log"]; len(got) != 1 || got[0] != 7 {
		t.Errorf("after a reload the view holds %v, want a single stack of 7", got)
	}
}

// Equipment never stacks. Two swords sharing one set of rolled affixes is
// incoherent, which is why the loader refuses a stackable equipment base --
// this checks the grant path agrees.
func TestGrantingEquipmentNeverStacks(t *testing.T) {
	inv, _ := testInventory(t)

	first := grant(t, inv, "test.sword", 1)
	second := grant(t, inv, "test.sword", 1)

	if first == second {
		t.Fatal("two swords were granted as one item")
	}
	if got := carried(inv)["test.sword"]; len(got) != 2 {
		t.Errorf("two swords occupy %d slots, want 2", len(got))
	}
}

// Different materials do not merge, however stackable both are.
func TestDifferentMaterialsDoNotMerge(t *testing.T) {
	inv, _ := testInventory(t)

	grant(t, inv, "test.log", 2)
	grant(t, inv, "test.gem", 2)

	held := carried(inv)
	if len(held["test.log"]) != 1 || held["test.log"][0] != 2 {
		t.Errorf("logs = %v, want one stack of 2", held["test.log"])
	}
	if len(held["test.gem"]) != 1 || held["test.gem"][0] != 2 {
		t.Errorf("gems = %v, want one stack of 2", held["test.gem"])
	}
}

// A stack at its maximum starts a new one rather than growing past it.
func TestAFullStackStartsAnother(t *testing.T) {
	inv, _ := testInventory(t)

	max := 0
	if base, ok := inv.content.Items["test.log"]; ok {
		max = base.MaxStack
	}
	if max < 2 {
		t.Fatalf("the fixture's log has max_stack %d; this test needs a stackable one", max)
	}

	for i := 0; i < max+1; i++ {
		grant(t, inv, "test.log", 1)
	}

	stacks := carried(inv)["test.log"]
	if len(stacks) != 2 {
		t.Fatalf("%d units occupy %d slots (%v), want 2", max+1, len(stacks), stacks)
	}
	total := stacks[0] + stacks[1]
	if total != max+1 {
		t.Errorf("%d units survive, want %d", total, max+1)
	}
	for _, n := range stacks {
		if n > max {
			t.Errorf("a stack holds %d, over the maximum of %d", n, max)
		}
	}
}

// A full bag still refuses, and stacking must not paper over that: a bag full
// of *different* things has nowhere to put a new one.
func TestAFullBagStillRefuses(t *testing.T) {
	inv, _ := testInventory(t)

	// Fill every slot with equipment, which cannot stack.
	for i := 0; i < inv.Capacity(); i++ {
		grant(t, inv, "test.sword", 1)
	}

	gen := inv.Generator()
	inst, err := gen.RollBase(rng.New(2), "test.sword", 1)
	if err != nil {
		t.Fatalf("roll: %v", err)
	}
	if _, err := inv.Grant(context.Background(), inst, 1); err != ErrInventoryFull {
		t.Errorf("granting into a full bag returned %v, want ErrInventoryFull", err)
	}

	// But a material that has a stack in it still fits, because it needs no
	// slot. Deliberate: it is what lets a gatherer keep going with a bag that
	// looks full.
	inv2, _ := testInventory(t)
	grant(t, inv2, "test.log", 1)
	for i := 0; i < inv2.Capacity()-1; i++ {
		grant(t, inv2, "test.sword", 1)
	}
	var stacked *items.Instance
	stacked, err = gen.RollBase(rng.New(3), "test.log", 1)
	if err != nil {
		t.Fatalf("roll: %v", err)
	}
	if _, err := inv2.Grant(context.Background(), stacked, 1); err != nil {
		t.Errorf("granting onto an existing stack in a full bag: %v", err)
	}
}
