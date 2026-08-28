package directory

import (
	"context"
	"sync"
	"testing"
)

func sharedKey(mapID string) RoomKey {
	return RoomKey{MapID: mapID, Placement: PlacementShared}
}

func privateKey(mapID, owner string) RoomKey {
	return RoomKey{MapID: mapID, Placement: PlacementPrivate, OwnerKey: owner}
}

func TestRoomKeyValid(t *testing.T) {
	tests := []struct {
		key  RoomKey
		want bool
	}{
		{sharedKey("henesys"), true},
		{privateKey("dungeon", "party-7"), true},

		// A shared room must not name an owner, and a private one must.
		{RoomKey{MapID: "x", Placement: PlacementShared, OwnerKey: "someone"}, false},
		{RoomKey{MapID: "x", Placement: PlacementPrivate}, false},

		{RoomKey{Placement: PlacementShared}, false},
		{RoomKey{MapID: "x", Placement: "nonsense"}, false},
		{RoomKey{}, false},
	}
	for _, tt := range tests {
		if got := tt.key.Valid(); got != tt.want {
			t.Errorf("%+v.Valid() = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestJoinCreatesInstanceOnDemand(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()

	inst, err := d.Join(ctx, sharedKey("henesys"), 10)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if inst.Players != 1 {
		t.Errorf("players = %d, want 1", inst.Players)
	}
	if inst.Node != "node-1" {
		t.Errorf("node = %q, want node-1", inst.Node)
	}
	if len(d.List(ctx)) != 1 {
		t.Errorf("expected exactly one instance, got %d", len(d.List(ctx)))
	}
}

func TestJoinReusesInstanceUntilFull(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()
	key := sharedKey("henesys")

	for i := 1; i <= 3; i++ {
		inst, err := d.Join(ctx, key, 3)
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		if inst.Players != i {
			t.Errorf("join %d: players = %d, want %d", i, inst.Players, i)
		}
	}
	if n := len(d.List(ctx)); n != 1 {
		t.Errorf("expected 1 instance while capacity remained, got %d", n)
	}
}

// A full shared room scales out into another channel rather than rejecting.
func TestSharedRoomOpensNewChannelWhenFull(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()
	key := sharedKey("henesys")

	for i := 0; i < 2; i++ {
		if _, err := d.Join(ctx, key, 2); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	inst, err := d.Join(ctx, key, 2)
	if err != nil {
		t.Fatalf("third join should open a new channel, got: %v", err)
	}
	if inst.Players != 1 {
		t.Errorf("new channel has %d players, want 1", inst.Players)
	}
	if n := len(d.List(ctx)); n != 2 {
		t.Errorf("expected 2 instances, got %d", n)
	}
}

// A private room is one instance by definition; a second would split the party
// across two dungeons.
func TestPrivateRoomRejectsRatherThanSplitting(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()
	key := privateKey("dungeon", "party-7")

	for i := 0; i < 2; i++ {
		if _, err := d.Join(ctx, key, 2); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	if _, err := d.Join(ctx, key, 2); err != ErrNoCapacity {
		t.Errorf("join beyond a full private room = %v, want ErrNoCapacity", err)
	}
	if n := len(d.List(ctx)); n != 1 {
		t.Errorf("expected exactly 1 private instance, got %d", n)
	}
}

// Spreading players across channels keeps per-room tick cost down, which
// matters especially under per-player mob layering.
func TestJoinPicksTheEmptiestChannel(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()
	key := sharedKey("henesys")

	// Fill one channel, force a second, then free a slot in the first.
	var first InstanceID
	for i := 0; i < 2; i++ {
		inst, _ := d.Join(ctx, key, 2)
		first = inst.ID
	}
	second, _ := d.Join(ctx, key, 2) // opens channel 2
	if err := d.Leave(ctx, first); err != nil {
		t.Fatalf("leave: %v", err)
	}

	// Channel 1 now has 1 player and channel 2 has 1; the tie breaks by ID.
	// Free another slot in channel 1 so it is strictly emptier.
	d.Leave(ctx, first)

	got, err := d.Join(ctx, key, 2)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if got.ID == second.ID {
		t.Error("join chose the fuller channel; it should pick the emptiest")
	}
}

func TestLeaveFreesCapacity(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()
	key := sharedKey("henesys")

	inst, _ := d.Join(ctx, key, 1)
	if err := d.Leave(ctx, inst.ID); err != nil {
		t.Fatalf("leave: %v", err)
	}

	again, err := d.Join(ctx, key, 1)
	if err != nil {
		t.Fatalf("join after leave: %v", err)
	}
	if again.ID != inst.ID {
		t.Error("expected to rejoin the same instance after a slot was freed")
	}
}

// Leaving the last slot must not destroy the room: only the world node knows
// whether the room still has work to do.
func TestLeavingLastSlotKeepsInstanceAlive(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()

	inst, _ := d.Join(ctx, sharedKey("henesys"), 10)
	d.Leave(ctx, inst.ID)

	if _, ok := d.Lookup(ctx, inst.ID); !ok {
		t.Error("instance disappeared when its last player left")
	}
}

func TestLeaveDoesNotUnderflow(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()

	inst, _ := d.Join(ctx, sharedKey("henesys"), 10)
	for i := 0; i < 5; i++ {
		d.Leave(ctx, inst.ID)
	}

	got, _ := d.Lookup(ctx, inst.ID)
	if got.Players != 0 {
		t.Errorf("players = %d after excess leaves, want 0", got.Players)
	}
}

func TestReleaseRemovesInstance(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()
	key := sharedKey("henesys")

	inst, _ := d.Join(ctx, key, 10)
	if err := d.Release(ctx, inst.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, ok := d.Lookup(ctx, inst.ID); ok {
		t.Error("instance still present after release")
	}
	if n := len(d.List(ctx)); n != 0 {
		t.Errorf("expected no instances, got %d", n)
	}

	// A join after release must create a fresh instance, not resurrect one.
	next, _ := d.Join(ctx, key, 10)
	if next.ID == inst.ID {
		t.Error("instance IDs must never be reused")
	}
}

// A stale message addressed to a torn-down instance must fail to route rather
// than land in an unrelated room.
func TestInstanceIDsAreNeverReused(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()

	seen := make(map[InstanceID]bool)
	for i := 0; i < 50; i++ {
		inst, err := d.Join(ctx, sharedKey("henesys"), 1)
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		if seen[inst.ID] {
			t.Fatalf("instance ID %d reused", inst.ID)
		}
		seen[inst.ID] = true
		d.Release(ctx, inst.ID)
	}
}

func TestUnknownInstanceErrors(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()

	if err := d.Leave(ctx, 999); err != ErrUnknownInstance {
		t.Errorf("Leave(unknown) = %v, want ErrUnknownInstance", err)
	}
	if err := d.Release(ctx, 999); err != ErrUnknownInstance {
		t.Errorf("Release(unknown) = %v, want ErrUnknownInstance", err)
	}
	if _, ok := d.Lookup(ctx, 999); ok {
		t.Error("Lookup(unknown) reported found")
	}
}

func TestInvalidInputRejected(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()

	if _, err := d.Join(ctx, RoomKey{}, 10); err == nil {
		t.Error("join with an invalid key should fail")
	}
	if _, err := d.Join(ctx, sharedKey("x"), 0); err == nil {
		t.Error("join with zero capacity should fail")
	}
	if _, err := d.Join(ctx, sharedKey("x"), -1); err == nil {
		t.Error("join with negative capacity should fail")
	}
}

func TestListIsOrderedByID(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		d.Join(ctx, sharedKey("henesys"), 1)
	}
	list := d.List(ctx)
	for i := 1; i < len(list); i++ {
		if list[i-1].ID >= list[i].ID {
			t.Fatalf("List not ordered by ID at index %d", i)
		}
	}
}

// Placement and reservation are one atomic step; doing them separately lets
// two simultaneous joins claim the same last free slot.
func TestConcurrentJoinsNeverExceedCapacity(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()
	key := sharedKey("henesys")

	const capacity = 4
	const joiners = 200

	var wg sync.WaitGroup
	for i := 0; i < joiners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Join(ctx, key, capacity)
		}()
	}
	wg.Wait()

	total := 0
	for _, inst := range d.List(ctx) {
		if inst.Players > inst.Capacity {
			t.Errorf("instance %d holds %d players, over capacity %d",
				inst.ID, inst.Players, inst.Capacity)
		}
		total += inst.Players
	}
	if total != joiners {
		t.Errorf("placed %d players, want %d", total, joiners)
	}
}

func TestConcurrentJoinAndLeave(t *testing.T) {
	d := NewMemory("node-1")
	ctx := context.Background()
	key := sharedKey("henesys")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				inst, err := d.Join(ctx, key, 8)
				if err == nil {
					d.Leave(ctx, inst.ID)
				}
			}
		}()
	}
	wg.Wait()

	for _, inst := range d.List(ctx) {
		if inst.Players != 0 {
			t.Errorf("instance %d left with %d players, want 0", inst.ID, inst.Players)
		}
	}
}
