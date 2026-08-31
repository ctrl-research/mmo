package store

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func testCharacter(t *testing.T, s *Store) uuid.UUID {
	t.Helper()
	acct := newAccount(t, s, "itemowner")
	c, err := s.CreateCharacter(context.Background(), acct, "Holder", "warrior", "tutorial")
	if err != nil {
		t.Fatalf("create character: %v", err)
	}
	return c.ID
}

func testContainers(t *testing.T, s *Store) (uuid.UUID, Container, Container) {
	t.Helper()
	char := testCharacter(t, s)
	inv, equip, err := s.EnsureContainers(context.Background(), char, 24, 6)
	if err != nil {
		t.Fatalf("ensure containers: %v", err)
	}
	return char, inv, equip
}

func sampleItem(base string) ItemRow {
	return ItemRow{
		BaseID:    base,
		Rarity:    "rare",
		ItemLevel: 20,
		Mods:      json.RawMessage(`{"affixes":[{"stat":3,"value":1500000}]}`),
		StackSize: 1,
	}
}

func TestEnsureContainersIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char := testCharacter(t, s)

	first, firstEquip, err := s.EnsureContainers(ctx, char, 24, 6)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// Called on every login, so it must not create a second set.
	second, secondEquip, err := s.EnsureContainers(ctx, char, 24, 6)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first.ID != second.ID || firstEquip.ID != secondEquip.ID {
		t.Error("a second call created new containers")
	}
}

func TestInsertAndLoadItem(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	id, err := s.InsertItem(ctx, inv.ID, 0, sampleItem("weapon.iron_sword"), char, EventCreate, 100)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	loaded, err := s.LoadItem(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.BaseID != "weapon.iron_sword" || loaded.Rarity != "rare" || loaded.ItemLevel != 20 {
		t.Errorf("loaded %+v", loaded)
	}

	// Rolled modifiers must survive exactly: re-deriving them would let a
	// rebalance rewrite items already in players' stashes.
	var mods map[string]any
	if err := json.Unmarshal(loaded.Mods, &mods); err != nil {
		t.Fatalf("mods did not round-trip: %v", err)
	}
	if _, ok := mods["affixes"]; !ok {
		t.Errorf("rolled affixes were lost: %s", loaded.Mods)
	}
}

// The database refuses two items in one slot. Not an assertion in Go that a
// new code path can skip.
func TestSlotUniquenessIsEnforcedByTheDatabase(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	if _, err := s.InsertItem(ctx, inv.ID, 3, sampleItem("weapon.iron_sword"), char, EventCreate, 1); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := s.InsertItem(ctx, inv.ID, 3, sampleItem("armour.iron_plate"), char, EventCreate, 1); err != ErrSlotOccupied {
		t.Errorf("second insert into the same slot returned %v, want ErrSlotOccupied", err)
	}
}

// Concurrent inserts into one slot: exactly one may win, or the constraint is
// not doing its job.
func TestConcurrentInsertsIntoOneSlot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	const contenders = 8
	var wg sync.WaitGroup
	succeeded := make(chan uuid.UUID, contenders)

	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := s.InsertItem(ctx, inv.ID, 0, sampleItem("weapon.iron_sword"), char, EventCreate, 1); err == nil {
				succeeded <- id
			}
		}()
	}
	wg.Wait()
	close(succeeded)

	if n := len(succeeded); n != 1 {
		t.Errorf("%d of %d concurrent inserts into one slot succeeded, want 1", n, contenders)
	}
}

func TestMoveItemBetweenContainers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, equip := testContainers(t, s)

	id, err := s.InsertItem(ctx, inv.ID, 0, sampleItem("weapon.iron_sword"), char, EventCreate, 1)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.MoveItem(ctx, id, equip.ID, 0, char, EventEquip, 42); err != nil {
		t.Fatalf("move: %v", err)
	}

	loaded, _ := s.LoadItem(ctx, id)
	if loaded.ContainerID != equip.ID || loaded.Slot != 0 {
		t.Errorf("item is in container %s slot %d after the move", loaded.ContainerID, loaded.Slot)
	}

	// The inventory slot must actually be free, not merely appear so.
	if _, err := s.InsertItem(ctx, inv.ID, 0, sampleItem("armour.iron_plate"), char, EventCreate, 1); err != nil {
		t.Errorf("the vacated slot is still occupied: %v", err)
	}
}

// A move is an UPDATE, so the item keeps its identity -- and therefore its
// history. A delete-then-insert would produce a new id and orphan the journal.
func TestMovePreservesIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, equip := testContainers(t, s)

	id, _ := s.InsertItem(ctx, inv.ID, 0, sampleItem("weapon.iron_sword"), char, EventCreate, 1)
	s.MoveItem(ctx, id, equip.ID, 0, char, EventEquip, 2)
	s.MoveItem(ctx, id, inv.ID, 5, char, EventUnequip, 3)

	loaded, err := s.LoadItem(ctx, id)
	if err != nil {
		t.Fatalf("the item lost its identity across moves: %v", err)
	}
	if loaded.ID != id {
		t.Errorf("id changed from %s to %s", id, loaded.ID)
	}

	history, err := s.ItemHistory(ctx, id)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	want := []string{EventCreate, EventEquip, EventUnequip}
	if len(history) != len(want) {
		t.Fatalf("history has %d entries, want %d: %+v", len(history), len(want), history)
	}
	for i, kind := range want {
		if history[i].Kind != kind {
			t.Errorf("event %d is %q, want %q", i, history[i].Kind, kind)
		}
	}
}

// The journal is what makes a duplication investigation possible at all.
func TestEveryMutationIsJournalled(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, equip := testContainers(t, s)

	id, _ := s.InsertItem(ctx, inv.ID, 0, sampleItem("weapon.iron_sword"), char, EventCreate, 1)
	s.MoveItem(ctx, id, equip.ID, 0, char, EventEquip, 2)
	s.DestroyItem(ctx, id, char, 3)

	history, err := s.ItemHistory(ctx, id)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("journal has %d entries, want 3", len(history))
	}

	// The record of destruction must outlive the item it describes, which is
	// why item_events has no foreign key to item_instances.
	if history[len(history)-1].Kind != EventDestroy {
		t.Errorf("the last event is %q, want %q", history[len(history)-1].Kind, EventDestroy)
	}
	if _, err := s.LoadItem(ctx, id); err != ErrNotFound {
		t.Errorf("the item still exists after destruction: %v", err)
	}
}

func TestMoveIntoAnOccupiedSlotFails(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	first, _ := s.InsertItem(ctx, inv.ID, 0, sampleItem("weapon.iron_sword"), char, EventCreate, 1)
	_, err := s.InsertItem(ctx, inv.ID, 1, sampleItem("armour.iron_plate"), char, EventCreate, 1)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.MoveItem(ctx, first, inv.ID, 1, char, EventMove, 2); err != ErrSlotOccupied {
		t.Errorf("moving onto an occupied slot returned %v, want ErrSlotOccupied", err)
	}
}

// Swapping has to be atomic: the unique constraint makes a two-step swap
// impossible, and an interrupted one would lose track of where things belong.
func TestSwapItems(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, equip := testContainers(t, s)

	inInventory, _ := s.InsertItem(ctx, inv.ID, 4, sampleItem("weapon.iron_sword"), char, EventCreate, 1)
	equipped, _ := s.InsertItem(ctx, equip.ID, 0, sampleItem("weapon.rusty_sword"), char, EventCreate, 1)

	if err := s.SwapItems(ctx, inInventory, equipped, char, 10); err != nil {
		t.Fatalf("swap: %v", err)
	}

	a, _ := s.LoadItem(ctx, inInventory)
	b, _ := s.LoadItem(ctx, equipped)

	if a.ContainerID != equip.ID || a.Slot != 0 {
		t.Errorf("the first item is at %s slot %d after the swap", a.ContainerID, a.Slot)
	}
	if b.ContainerID != inv.ID || b.Slot != 4 {
		t.Errorf("the second item is at %s slot %d after the swap", b.ContainerID, b.Slot)
	}

	// Both sides are journalled, so a swap is as traceable as a move.
	for _, id := range []uuid.UUID{inInventory, equipped} {
		history, _ := s.ItemHistory(ctx, id)
		if len(history) < 2 {
			t.Errorf("item %s has %d journal entries after a swap", id, len(history))
		}
	}
}

func TestFreeSlotFindsTheLowestGap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	for _, slot := range []int{0, 1, 3} {
		if _, err := s.InsertItem(ctx, inv.ID, slot, sampleItem("potion.red_small"), char, EventCreate, 1); err != nil {
			t.Fatalf("insert into slot %d: %v", slot, err)
		}
	}

	slot, err := s.FreeSlot(ctx, inv.ID)
	if err != nil {
		t.Fatalf("free slot: %v", err)
	}
	if slot != 2 {
		t.Errorf("free slot = %d, want 2 (the lowest gap)", slot)
	}
}

func TestFreeSlotReportsAFullContainer(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char := testCharacter(t, s)

	small, _, err := s.EnsureContainers(ctx, char, 2, 6)
	if err != nil {
		t.Fatalf("containers: %v", err)
	}

	for slot := 0; slot < 2; slot++ {
		if _, err := s.InsertItem(ctx, small.ID, slot, sampleItem("potion.red_small"), char, EventCreate, 1); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	if _, err := s.FreeSlot(ctx, small.ID); err != ErrContainerFull {
		t.Errorf("a full container returned %v, want ErrContainerFull", err)
	}
}

func TestLoadContainerIsOrderedBySlot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	for _, slot := range []int{5, 0, 3, 1} {
		s.InsertItem(ctx, inv.ID, slot, sampleItem("potion.red_small"), char, EventCreate, 1)
	}

	items, err := s.LoadContainer(ctx, inv.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("loaded %d items, want 4", len(items))
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].Slot >= items[i].Slot {
			t.Fatalf("items are not ordered by slot: %v", items)
		}
	}
}

func TestOperationsOnAMissingItem(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)
	missing := uuid.New()

	if _, err := s.LoadItem(ctx, missing); err != ErrNotFound {
		t.Errorf("LoadItem = %v, want ErrNotFound", err)
	}
	if err := s.MoveItem(ctx, missing, inv.ID, 0, char, EventMove, 1); err != ErrNotFound {
		t.Errorf("MoveItem = %v, want ErrNotFound", err)
	}
	if err := s.DestroyItem(ctx, missing, char, 1); err != ErrNotFound {
		t.Errorf("DestroyItem = %v, want ErrNotFound", err)
	}
}

// Deleting a character takes its containers and their items with it, so a
// deleted character does not leave orphaned rows behind forever.
func TestDeletingContainersCascades(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	id, _ := s.InsertItem(ctx, inv.ID, 0, sampleItem("weapon.iron_sword"), char, EventCreate, 1)

	if _, err := s.pool.Exec(ctx, `DELETE FROM containers WHERE id = $1`, inv.ID); err != nil {
		t.Fatalf("delete container: %v", err)
	}
	if _, err := s.LoadItem(ctx, id); err != ErrNotFound {
		t.Errorf("the item survived its container: %v", err)
	}

	// The journal outlives both, which is what makes an investigation possible
	// after the fact.
	history, err := s.ItemHistory(ctx, id)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) == 0 {
		t.Error("the journal was lost with the container")
	}
}

// Two concurrent moves of one item must serialise, or an item could end up
// recorded in two places.
func TestConcurrentMovesOfOneItemSerialise(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, equip := testContainers(t, s)

	id, _ := s.InsertItem(ctx, inv.ID, 0, sampleItem("weapon.iron_sword"), char, EventCreate, 1)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				s.MoveItem(ctx, id, equip.ID, 0, char, EventEquip, uint64(n))
			} else {
				s.MoveItem(ctx, id, inv.ID, 0, char, EventUnequip, uint64(n))
			}
		}(i)
	}
	wg.Wait()

	// Exactly one row, in exactly one place.
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM item_instances WHERE id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("the item exists %d times after concurrent moves", count)
	}
}

// Stacking.
//
// Without it a stackable material takes one slot per unit, which nobody
// notices while loot is the only source -- a boar drops one hide -- and which
// fills a 24-slot bag in about two minutes once gathering exists.

func stackable(base string, qty int) ItemRow {
	return ItemRow{
		BaseID:    base,
		Rarity:    "normal",
		ItemLevel: 1,
		Mods:      json.RawMessage(`{}`),
		StackSize: qty,
	}
}

func TestStackIntoAddsToAnExistingStack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	first, err := s.InsertItem(ctx, inv.ID, 0, stackable("material.oak_log", 3), char, EventPickup, 1)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	id, total, err := s.StackInto(ctx, inv.ID, "material.oak_log", 2, 500, char, 2)
	if err != nil {
		t.Fatalf("stack: %v", err)
	}
	if id != first {
		t.Errorf("stacked into %v, want the existing stack %v", id, first)
	}
	if total != 5 {
		t.Errorf("stack is now %d, want 5", total)
	}

	// And it is one row, not two.
	rows, err := s.LoadContainer(ctx, inv.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the container holds %d rows, want 1", len(rows))
	}
	if rows[0].StackSize != 5 {
		t.Errorf("row stack is %d, want 5", rows[0].StackSize)
	}
}

// Nothing to merge into is the ordinary case for the first of anything, and
// must not be an error -- the caller inserts a new stack instead.
func TestStackIntoReportsNoStackToMergeWith(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	id, _, err := s.StackInto(ctx, inv.ID, "material.oak_log", 1, 500, char, 1)
	if err != nil {
		t.Fatalf("stack into an empty container: %v", err)
	}
	if id != uuid.Nil {
		t.Errorf("stacked into %v, want the zero UUID", id)
	}

	// A different base is not a stack to merge with either. Two materials that
	// merged because they were both stackable would be an item transmuting.
	if _, err := s.InsertItem(ctx, inv.ID, 0, stackable("material.copper_ore", 4), char, EventPickup, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _, err = s.StackInto(ctx, inv.ID, "material.oak_log", 1, 500, char, 2)
	if err != nil {
		t.Fatalf("stack: %v", err)
	}
	if id != uuid.Nil {
		t.Errorf("oak logs merged into copper ore (%v)", id)
	}
}

// A stack does not exceed its maximum, so the caller starts a new one.
func TestStackIntoRespectsTheMaximum(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	if _, err := s.InsertItem(ctx, inv.ID, 0, stackable("material.oak_log", 99), char, EventPickup, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// One more fits.
	if id, total, err := s.StackInto(ctx, inv.ID, "material.oak_log", 1, 100, char, 2); err != nil {
		t.Fatalf("stack: %v", err)
	} else if id == uuid.Nil || total != 100 {
		t.Fatalf("stacked to %d (id %v), want 100", total, id)
	}

	// The next does not.
	if id, _, err := s.StackInto(ctx, inv.ID, "material.oak_log", 1, 100, char, 3); err != nil {
		t.Fatalf("stack: %v", err)
	} else if id != uuid.Nil {
		t.Errorf("a full stack accepted another unit (%v)", id)
	}
}

// Concurrent grants of the same material must not lose any: two that both read
// the same stack size and write the same total is the arithmetic version of
// destroying an item.
func TestConcurrentStacksDoNotLoseAnything(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	if _, err := s.InsertItem(ctx, inv.ID, 0, stackable("material.oak_log", 1), char, EventPickup, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const grants = 16
	var wg sync.WaitGroup
	for i := 0; i < grants; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.StackInto(ctx, inv.ID, "material.oak_log", 1, 500, char, 2); err != nil {
				t.Errorf("stack: %v", err)
			}
		}()
	}
	wg.Wait()

	rows, err := s.LoadContainer(ctx, inv.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var total int
	for _, r := range rows {
		total += r.StackSize
	}
	if total != 1+grants {
		t.Errorf("%d units survived %d concurrent grants onto a stack of 1, want %d",
			total, grants, 1+grants)
	}
}

// A stack growing is journalled, because an investigation that could not see
// quantity change would be an investigation with a gap in it.
func TestStackingIsJournalled(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	id, err := s.InsertItem(ctx, inv.ID, 0, stackable("material.oak_log", 1), char, EventPickup, 1)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := s.StackInto(ctx, inv.ID, "material.oak_log", 1, 500, char, 2); err != nil {
		t.Fatalf("stack: %v", err)
	}

	history, err := s.ItemHistory(ctx, id)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 {
		t.Errorf("the journal holds %d entries after an insert and a stack, want 2", len(history))
	}
}

// Concurrent grants onto a nearly-full stack must not push it over its maximum.
//
// This is the interesting half of stacking. The sequential test above says
// nothing about it: the cap holds under contention because READ COMMITTED
// re-runs the statement's sub-select when the target row has been modified
// underneath it, so the losers find no stack with room rather than adding to a
// full one. Sixty-four contenders for one unit of room, and exactly one may
// win.
func TestConcurrentStacksDoNotExceedTheMaximum(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char, inv, _ := testContainers(t, s)

	const max = 100
	if _, err := s.InsertItem(ctx, inv.ID, 0, stackable("material.oak_log", max-1), char, EventPickup, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const grants = 64
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		merged int
	)
	for i := 0; i < grants; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, err := s.StackInto(ctx, inv.ID, "material.oak_log", 1, max, char, 2)
			if err != nil {
				t.Errorf("stack: %v", err)
				return
			}
			if id != uuid.Nil {
				mu.Lock()
				merged++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if merged != 1 {
		t.Errorf("%d of %d concurrent grants merged into a stack with room for one, want 1", merged, grants)
	}

	rows, err := s.LoadContainer(ctx, inv.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, r := range rows {
		if r.StackSize > max {
			t.Errorf("a stack holds %d, over its maximum of %d", r.StackSize, max)
		}
	}
}
