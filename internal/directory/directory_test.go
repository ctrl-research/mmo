package directory

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The Directory contract, run against every implementation.
//
// A second implementation is only useful if it is genuinely interchangeable,
// and two suites -- one per implementation -- drift. The drift stays invisible
// until roles are actually split across nodes and a placement that worked in
// one process starts handing two members of one party their own dungeon. So
// every behaviour a caller relies on is asserted here, once, against both.
//
// What is deliberately *not* here: multi-node placement and node registration.
// Those are not on the interface -- Memory takes its nodes from Go calls and
// Redis takes them from registrations with a TTL -- so they are tested per
// implementation, further down.

// directoryUnderTest names one implementation and how to open a fresh one.
type directoryUnderTest struct {
	name string
	open func(t *testing.T) Directory
}

// implementations returns every directory available to test.
//
// Memory always. Redis only when MMO_TEST_REDIS_ADDR points at a server, the
// same arrangement the store and bus tests use: the behaviour that matters --
// whether a Lua script really is atomic -- is the server's, and a fake would
// assert that the Go code calls the commands it already calls.
func implementations() []directoryUnderTest {
	impls := []directoryUnderTest{{
		name: "memory",
		open: func(t *testing.T) Directory {
			t.Helper()
			d := NewMemory("node-a")
			t.Cleanup(func() { d.Close() })
			return d
		},
	}}

	if addr := os.Getenv("MMO_TEST_REDIS_ADDR"); addr != "" {
		impls = append(impls, directoryUnderTest{
			name: "redis",
			open: func(t *testing.T) Directory {
				t.Helper()
				return openRedisDirectory(t, addr, "node-a")
			},
		})
	}
	return impls
}

// openRedisDirectory returns a Redis directory with a namespace of its own.
//
// A prefix per test, because these run against one shared server and a
// directory is global by design: two tests sharing a prefix would see each
// other's instances, which looks exactly like a placement bug.
func openRedisDirectory(t *testing.T, addr string, node NodeID) *Redis {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: addr})
	prefix := "mmotest:" + strconv.FormatInt(time.Now().UnixNano(), 36)

	ctx := context.Background()
	d, err := NewRedis(ctx, client, prefix, node)
	if err != nil {
		t.Fatalf("open redis directory: %v", err)
	}
	t.Cleanup(func() {
		d.Close()
		// Left-behind keys would accumulate across runs on a long-lived
		// development server.
		if keys, err := client.Keys(ctx, prefix+"*").Result(); err == nil && len(keys) > 0 {
			client.Del(ctx, keys...)
		}
		client.Close()
	})
	return d
}

// eachDirectory runs fn against every implementation as its own subtest.
func eachDirectory(t *testing.T, fn func(t *testing.T, d Directory)) {
	t.Helper()
	for _, impl := range implementations() {
		t.Run(impl.name, func(t *testing.T) { fn(t, impl.open(t)) })
	}
}

// The read methods return errors so a network-backed directory can report one.
// These keep the test bodies about behaviour rather than about plumbing.

func mustList(t *testing.T, d Directory, ctx context.Context) []Instance {
	t.Helper()
	out, err := d.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return out
}

func mustInstancesFor(t *testing.T, d Directory, ctx context.Context, key RoomKey) []Instance {
	t.Helper()
	out, err := d.InstancesFor(ctx, key)
	if err != nil {
		t.Fatalf("InstancesFor(%s): %v", key, err)
	}
	return out
}

func mustLookup(t *testing.T, d Directory, ctx context.Context, id InstanceID) (Instance, bool) {
	t.Helper()
	inst, ok, err := d.Lookup(ctx, id)
	if err != nil {
		t.Fatalf("Lookup(%d): %v", id, err)
	}
	return inst, ok
}

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
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()

		inst, err := d.Join(ctx, sharedKey("henesys"), 10)
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		if inst.Players != 1 {
			t.Errorf("players = %d, want 1", inst.Players)
		}
		if inst.Node == "" {
			t.Error("the instance was placed on no node at all")
		}
		if len(mustList(t, d, ctx)) != 1 {
			t.Errorf("expected exactly one instance, got %d", len(mustList(t, d, ctx)))
		}
	})
}

func TestJoinReusesInstanceUntilFull(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
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
		if n := len(mustList(t, d, ctx)); n != 1 {
			t.Errorf("expected 1 instance while capacity remained, got %d", n)
		}
	})
}

// A full shared room scales out into another channel rather than rejecting.
func TestSharedRoomOpensNewChannelWhenFull(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
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
		if n := len(mustList(t, d, ctx)); n != 2 {
			t.Errorf("expected 2 instances, got %d", n)
		}
	})
}

// A private room is one instance by definition; a second would split the party
// across two dungeons.
func TestPrivateRoomRejectsRatherThanSplitting(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
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
		if n := len(mustList(t, d, ctx)); n != 1 {
			t.Errorf("expected exactly 1 private instance, got %d", n)
		}
	})
}

// Spreading players across channels keeps per-room tick cost down, which
// matters especially under per-player mob layering.
func TestJoinPicksTheEmptiestChannel(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
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
	})
}

func TestLeaveFreesCapacity(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
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
	})
}

// Leaving the last slot must not destroy the room: only the world node knows
// whether the room still has work to do.
func TestLeavingLastSlotKeepsInstanceAlive(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()

		inst, _ := d.Join(ctx, sharedKey("henesys"), 10)
		d.Leave(ctx, inst.ID)

		if _, ok := mustLookup(t, d, ctx, inst.ID); !ok {
			t.Error("instance disappeared when its last player left")
		}
	})
}

func TestLeaveDoesNotUnderflow(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()

		inst, _ := d.Join(ctx, sharedKey("henesys"), 10)
		for i := 0; i < 5; i++ {
			d.Leave(ctx, inst.ID)
		}

		got, _ := mustLookup(t, d, ctx, inst.ID)
		if got.Players != 0 {
			t.Errorf("players = %d after excess leaves, want 0", got.Players)
		}
	})
}

func TestReleaseRemovesInstance(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()
		key := sharedKey("henesys")

		inst, _ := d.Join(ctx, key, 10)
		if err := d.Release(ctx, inst.ID); err != nil {
			t.Fatalf("release: %v", err)
		}
		if _, ok := mustLookup(t, d, ctx, inst.ID); ok {
			t.Error("instance still present after release")
		}
		if n := len(mustList(t, d, ctx)); n != 0 {
			t.Errorf("expected no instances, got %d", n)
		}

		// A join after release must create a fresh instance, not resurrect one.
		next, _ := d.Join(ctx, key, 10)
		if next.ID == inst.ID {
			t.Error("instance IDs must never be reused")
		}
	})
}

// A stale message addressed to a torn-down instance must fail to route rather
// than land in an unrelated room.
func TestInstanceIDsAreNeverReused(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
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
	})
}

func TestUnknownInstanceErrors(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()

		if err := d.Leave(ctx, 999); err != ErrUnknownInstance {
			t.Errorf("Leave(unknown) = %v, want ErrUnknownInstance", err)
		}
		if err := d.Release(ctx, 999); err != ErrUnknownInstance {
			t.Errorf("Release(unknown) = %v, want ErrUnknownInstance", err)
		}
		if _, ok := mustLookup(t, d, ctx, 999); ok {
			t.Error("Lookup(unknown) reported found")
		}
	})
}

func TestInvalidInputRejected(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
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
	})
}

func TestListIsOrderedByID(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()

		for i := 0; i < 10; i++ {
			d.Join(ctx, sharedKey("henesys"), 1)
		}
		list := mustList(t, d, ctx)
		for i := 1; i < len(list); i++ {
			if list[i-1].ID >= list[i].ID {
				t.Fatalf("List not ordered by ID at index %d", i)
			}
		}
	})
}

// Placement and reservation are one atomic step; doing them separately lets
// two simultaneous joins claim the same last free slot.
func TestConcurrentJoinsNeverExceedCapacity(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
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
		for _, inst := range mustList(t, d, ctx) {
			if inst.Players > inst.Capacity {
				t.Errorf("instance %d holds %d players, over capacity %d",
					inst.ID, inst.Players, inst.Capacity)
			}
			total += inst.Players
		}
		if total != joiners {
			t.Errorf("placed %d players, want %d", total, joiners)
		}
	})
}

func TestConcurrentJoinAndLeave(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
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

		for _, inst := range mustList(t, d, ctx) {
			if inst.Players != 0 {
				t.Errorf("instance %d left with %d players, want 0", inst.ID, inst.Players)
			}
		}
	})
}

// --- multi-node placement ----------------------------------------------------

// Which node hosts a room is the directory's decision, and it is the decision
// that makes "hosted by a different world role" true rather than nominal.
func TestNewInstancesSpreadAcrossNodes(t *testing.T) {
	d := NewMemory("node-a")
	d.AddNode("node-b")
	ctx := context.Background()

	// Two maps, so each Join has to create an instance rather than filling one.
	a, err := d.Join(ctx, RoomKey{MapID: "one", Placement: PlacementShared}, 4)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	b, err := d.Join(ctx, RoomKey{MapID: "two", Placement: PlacementShared}, 4)
	if err != nil {
		t.Fatalf("join: %v", err)
	}

	if a.Node == b.Node {
		t.Errorf("both instances went to %s; the emptier node should have taken the second",
			a.Node)
	}
}

func TestAddNodeIsIdempotent(t *testing.T) {
	d := NewMemory("node-a")
	d.AddNode("node-a")
	d.AddNode("node-b")
	d.AddNode("node-b")

	if got := d.Nodes(); len(got) != 2 {
		t.Errorf("registered nodes are %v, want two distinct ones", got)
	}
}

// --- releasing ---------------------------------------------------------------

// Checking occupancy and releasing separately leaves a window in which a player
// joins the instance between the two, and the room they were placed in stops
// ticking a moment later.
func TestTryReleaseRefusesAnOccupiedInstance(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()

		inst, err := d.Join(ctx, RoomKey{MapID: "one", Placement: PlacementShared}, 4)
		if err != nil {
			t.Fatalf("join: %v", err)
		}

		released, err := d.TryRelease(ctx, inst.ID)
		if err != nil {
			t.Fatalf("try release: %v", err)
		}
		if released {
			t.Fatal("released an instance with a player in it")
		}

		if err := d.Leave(ctx, inst.ID); err != nil {
			t.Fatalf("leave: %v", err)
		}

		released, err = d.TryRelease(ctx, inst.ID)
		if err != nil || !released {
			t.Fatalf("TryRelease on an empty instance returned (%v, %v), want (true, nil)",
				released, err)
		}
		if _, ok := mustLookup(t, d, ctx, inst.ID); ok {
			t.Error("the instance is still listed after being released")
		}
	})
}

// Already gone is the outcome the caller wanted, not an error to handle.
func TestTryReleaseOnAnUnknownInstanceSucceeds(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {

		released, err := d.TryRelease(context.Background(), 999)
		if err != nil || !released {
			t.Errorf("TryRelease on an unknown instance returned (%v, %v), want (true, nil)",
				released, err)
		}
	})
}

// --- channels ----------------------------------------------------------------

// A player picking channel 3 means that instance and no other, so the placement
// that spreads players across the least-full channel is exactly wrong here.
func TestJoinInstanceTakesTheNamedChannel(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()
		key := RoomKey{MapID: "one", Placement: PlacementShared}

		busy, err := d.Join(ctx, key, 4)
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		quiet, err := d.NewInstance(ctx, key, 4)
		if err != nil {
			t.Fatalf("new instance: %v", err)
		}
		if err := d.Leave(ctx, quiet.ID); err != nil {
			t.Fatalf("leave: %v", err)
		}

		// The emptier channel is the one Join would pick, so asking for the busy
		// one proves the choice was honoured rather than coincidental.
		got, err := d.JoinInstance(ctx, busy.ID)
		if err != nil {
			t.Fatalf("join instance: %v", err)
		}
		if got.ID != busy.ID {
			t.Errorf("asked for instance %d, got %d", busy.ID, got.ID)
		}
		if got.Players != 2 {
			t.Errorf("the named channel holds %d players, want 2", got.Players)
		}
	})
}

func TestJoinInstanceRefusesAFullOrMissingChannel(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()

		inst, err := d.Join(ctx, RoomKey{MapID: "one", Placement: PlacementShared}, 1)
		if err != nil {
			t.Fatalf("join: %v", err)
		}

		if _, err := d.JoinInstance(ctx, inst.ID); err != ErrNoCapacity {
			t.Errorf("joining a full channel returned %v, want ErrNoCapacity", err)
		}
		if _, err := d.JoinInstance(ctx, 999); err != ErrUnknownInstance {
			t.Errorf("joining a missing channel returned %v, want ErrUnknownInstance", err)
		}
	})
}

// A second instance would split a party across two dungeons.
func TestNewInstanceRefusesAPrivateKey(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		key := RoomKey{MapID: "one", Placement: PlacementPrivate, OwnerKey: "party-1"}

		if _, err := d.NewInstance(context.Background(), key, 4); err != ErrNoCapacity {
			t.Errorf("creating a second private instance returned %v, want ErrNoCapacity", err)
		}
	})
}

// The channel list a player picks from.
func TestInstancesForListsEveryChannelOfAKey(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()
		key := RoomKey{MapID: "one", Placement: PlacementShared}

		if _, err := d.Join(ctx, key, 4); err != nil {
			t.Fatalf("join: %v", err)
		}
		if _, err := d.NewInstance(ctx, key, 4); err != nil {
			t.Fatalf("new instance: %v", err)
		}
		// A different map must not appear in this map's channel list.
		if _, err := d.Join(ctx, RoomKey{MapID: "two", Placement: PlacementShared}, 4); err != nil {
			t.Fatalf("join other map: %v", err)
		}

		got := mustInstancesFor(t, d, ctx, key)
		if len(got) != 2 {
			t.Fatalf("listed %d channels, want 2", len(got))
		}
		if got[0].ID > got[1].ID {
			t.Error("channels are not ordered by id, so the numbers a player sees would move")
		}
	})
}

// --- the Redis implementation's own properties ------------------------------
//
// Node registration is not on the Directory interface: Memory takes its nodes
// from Go calls and Redis takes them from registrations with a TTL. The
// behaviour that matters is the same in spirit -- placement spreads rooms and
// prefers a live node -- but only Redis can lose a node, and losing one is what
// this milestone is actually about.

func redisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("MMO_TEST_REDIS_ADDR is not set")
	}
	return addr
}

// Two directories on separate connections are two nodes, and they have to agree.
//
// This is the property the milestone rests on and the one Memory cannot
// demonstrate: an instance created by one node has to be visible to, and
// joinable by, another.
func TestRedisDirectoryIsSharedBetweenNodes(t *testing.T) {
	addr := redisAddr(t)
	ctx := context.Background()

	a := openRedisDirectory(t, addr, "node-a")

	// The second directory shares the first's namespace, which is what makes it
	// the same cluster rather than a separate one.
	b, err := NewRedis(ctx, a.client, a.prefix, "node-b")
	if err != nil {
		t.Fatalf("open second directory: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	key := sharedKey("henesys")
	created, err := a.Join(ctx, key, 4)
	if err != nil {
		t.Fatalf("join on node-a: %v", err)
	}

	// node-b sees it.
	seen, ok, err := b.Lookup(ctx, created.ID)
	if err != nil {
		t.Fatalf("lookup on node-b: %v", err)
	}
	if !ok {
		t.Fatal("node-b cannot see an instance node-a created")
	}
	if seen.Players != 1 {
		t.Errorf("node-b sees %d players, want 1", seen.Players)
	}

	// And joins it rather than making its own.
	joined, err := b.Join(ctx, key, 4)
	if err != nil {
		t.Fatalf("join on node-b: %v", err)
	}
	if joined.ID != created.ID {
		t.Errorf("node-b joined instance %d, want the existing %d", joined.ID, created.ID)
	}

	// A release on one is a release on the other.
	if err := b.Leave(ctx, created.ID); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if err := b.Leave(ctx, created.ID); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if released, err := a.TryRelease(ctx, created.ID); err != nil || !released {
		t.Fatalf("TryRelease on node-a = %v, %v; want it to release an instance node-b emptied", released, err)
	}
	if _, ok, _ := b.Lookup(ctx, created.ID); ok {
		t.Error("node-b still sees an instance node-a released")
	}
}

// Both nodes are placement targets, and rooms spread across them.
func TestRedisPlacementSpreadsAcrossLiveNodes(t *testing.T) {
	addr := redisAddr(t)
	ctx := context.Background()

	a := openRedisDirectory(t, addr, "node-a")
	b, err := NewRedis(ctx, a.client, a.prefix, "node-b")
	if err != nil {
		t.Fatalf("open second directory: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	// Four channels of one map, each created deliberately rather than by
	// filling: NewInstance is the path that always places.
	nodes := map[NodeID]int{}
	for i := 0; i < 4; i++ {
		inst, err := a.NewInstance(ctx, sharedKey("henesys"), 4)
		if err != nil {
			t.Fatalf("new instance %d: %v", i, err)
		}
		nodes[inst.Node]++
	}

	if len(nodes) != 2 {
		t.Errorf("four rooms landed on %d nodes (%v), want both", len(nodes), nodes)
	}
	for node, count := range nodes {
		if count != 2 {
			t.Errorf("node %s hosts %d of four rooms, want an even split", node, count)
		}
	}
}

// A node that stops heartbeating stops receiving rooms.
//
// This is the half of chaos-tolerance the directory owns: when a node dies, the
// rooms it was hosting are gone either way, but continuing to *place* new ones
// on it would mean players sent to a room nobody is running.
func TestRedisPlacementSkipsADeadNode(t *testing.T) {
	addr := redisAddr(t)
	ctx := context.Background()

	a := openRedisDirectory(t, addr, "node-a")
	b, err := NewRedis(ctx, a.client, a.prefix, "node-b")
	if err != nil {
		t.Fatalf("open second directory: %v", err)
	}

	if live, err := a.LiveNodes(ctx); err != nil || len(live) != 2 {
		t.Fatalf("live nodes = %v, %v; want both", live, err)
	}

	// node-b stops heartbeating and its liveness entry expires. Expired here
	// rather than waited out, because NodeTTL is fifteen seconds and a test
	// that sleeps that long is a test nobody runs.
	b.Close()
	if err := a.client.ZAdd(ctx, a.aliveKey(), redis.Z{
		Score:  float64(time.Now().Add(-time.Minute).UnixMilli()),
		Member: "node-b",
	}).Err(); err != nil {
		t.Fatalf("expire node-b: %v", err)
	}

	live, err := a.LiveNodes(ctx)
	if err != nil {
		t.Fatalf("live nodes: %v", err)
	}
	if len(live) != 1 || live[0] != "node-a" {
		t.Fatalf("live nodes = %v, want only node-a", live)
	}

	// Every new room now goes to the node that is still there.
	for i := 0; i < 4; i++ {
		inst, err := a.NewInstance(ctx, sharedKey("henesys"), 4)
		if err != nil {
			t.Fatalf("new instance %d: %v", i, err)
		}
		if inst.Node != "node-a" {
			t.Errorf("room %d was placed on %s, which is not heartbeating", i, inst.Node)
		}
	}
}

// With no live node at all, placement refuses rather than inventing one.
//
// Distinct from ErrNoCapacity: capacity means the world is full, this means
// there is no world, and a caller that conflated them would tell a player to
// try another channel.
func TestRedisPlacementWithNoLiveNodeIsRefused(t *testing.T) {
	addr := redisAddr(t)
	ctx := context.Background()

	d := openRedisDirectory(t, addr, "node-a")
	d.Close()
	if err := d.client.ZAdd(ctx, d.aliveKey(), redis.Z{
		Score:  float64(time.Now().Add(-time.Minute).UnixMilli()),
		Member: "node-a",
	}).Err(); err != nil {
		t.Fatalf("expire node-a: %v", err)
	}

	if _, err := d.Join(ctx, sharedKey("henesys"), 4); !errors.Is(err, ErrNoLiveNode) {
		t.Errorf("join with no live node = %v, want ErrNoLiveNode", err)
	}
}

// A node that restarts keeps its place in the ordering.
//
// The registration score breaks ties between equally loaded nodes. A node that
// re-registered to the front of the queue would take a disproportionate share of
// new rooms every time it restarted, which turns a rolling deploy into a load
// imbalance.
func TestRedisRegistrationOrderSurvivesARestart(t *testing.T) {
	addr := redisAddr(t)
	ctx := context.Background()

	a := openRedisDirectory(t, addr, "node-a")
	b, err := NewRedis(ctx, a.client, a.prefix, "node-b")
	if err != nil {
		t.Fatalf("open node-b: %v", err)
	}
	before, err := a.Nodes(ctx)
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	b.Close()

	// node-*a* comes back, not node-b. Re-registering the last node cannot
	// change the order however the score is assigned, so it would prove
	// nothing; re-registering the first is what a fresh score would move to the
	// end.
	again, err := NewRedis(ctx, a.client, a.prefix, "node-a")
	if err != nil {
		t.Fatalf("reopen node-a: %v", err)
	}
	t.Cleanup(func() { again.Close() })

	after, err := a.Nodes(ctx)
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("nodes = %v after a restart, want the same %v", after, before)
	}
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("node order = %v after a restart, want %v", after, before)
			break
		}
	}
}

// Releasing an instance leaves nothing behind.
//
// The interface cannot see this: a released instance is invisible to Lookup and
// skipped by the listings whether or not its bookkeeping was cleaned up. But the
// bookkeeping is what a long-running server accumulates, and a load counter that
// only ever goes up is worse than a leak -- it makes placement permanently
// wrong, because a node that hosted and released a thousand rooms looks like a
// node hosting a thousand rooms.
func TestRedisReleaseLeavesNoBookkeepingBehind(t *testing.T) {
	addr := redisAddr(t)
	ctx := context.Background()

	d := openRedisDirectory(t, addr, "node-a")
	key := sharedKey("henesys")

	inst, err := d.Join(ctx, key, 4)
	if err != nil {
		t.Fatalf("join: %v", err)
	}

	loadBefore, err := d.client.ZScore(ctx, d.loadKey(), "node-a").Result()
	if err != nil {
		t.Fatalf("load score: %v", err)
	}
	if loadBefore != 1 {
		t.Fatalf("node load = %v after one room, want 1", loadBefore)
	}

	if err := d.Leave(ctx, inst.ID); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if err := d.Release(ctx, inst.ID); err != nil {
		t.Fatalf("release: %v", err)
	}

	if n, err := d.client.ZCard(ctx, d.keySetKey(key)).Result(); err != nil || n != 0 {
		t.Errorf("the key set holds %d entries after a release, want 0 (err %v)", n, err)
	}
	if n, err := d.client.ZCard(ctx, d.allKey()).Result(); err != nil || n != 0 {
		t.Errorf("the instance list holds %d entries after a release, want 0 (err %v)", n, err)
	}
	if n, err := d.client.Exists(ctx, d.instKey(inst.ID)).Result(); err != nil || n != 0 {
		t.Errorf("the instance hash still exists after a release (err %v)", err)
	}

	loadAfter, err := d.client.ZScore(ctx, d.loadKey(), "node-a").Result()
	if err != nil {
		t.Fatalf("load score: %v", err)
	}
	if loadAfter != 0 {
		t.Errorf("node load = %v after releasing its only room, want 0; a counter "+
			"that only rises makes placement permanently wrong", loadAfter)
	}
}

// And so does TryRelease, which is the path an idle room actually takes.
func TestRedisTryReleaseLeavesNoBookkeepingBehind(t *testing.T) {
	addr := redisAddr(t)
	ctx := context.Background()

	d := openRedisDirectory(t, addr, "node-a")
	key := sharedKey("henesys")

	inst, err := d.Join(ctx, key, 4)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if err := d.Leave(ctx, inst.ID); err != nil {
		t.Fatalf("leave: %v", err)
	}
	released, err := d.TryRelease(ctx, inst.ID)
	if err != nil || !released {
		t.Fatalf("TryRelease = %v, %v; want it to release an empty instance", released, err)
	}

	if n, _ := d.client.ZCard(ctx, d.keySetKey(key)).Result(); n != 0 {
		t.Errorf("the key set holds %d entries after a TryRelease, want 0", n)
	}
	if n, _ := d.client.ZCard(ctx, d.allKey()).Result(); n != 0 {
		t.Errorf("the instance list holds %d entries after a TryRelease, want 0", n)
	}
	if load, _ := d.client.ZScore(ctx, d.loadKey(), "node-a").Result(); load != 0 {
		t.Errorf("node load = %v after a TryRelease, want 0", load)
	}
}

// A process that hosts nothing is never placed on.
//
// Registering is an offer to host rooms and placement takes the offer, so a
// gateway that registered would be chosen to run a map it has no simulation
// for. The player sent there waits out a timeout on a room nobody is going to
// start -- and a second gateway looking for somewhere to put a character would
// pick it too.
func TestRedisReaderIsNeverPlacedOn(t *testing.T) {
	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set MMO_TEST_REDIS_ADDR to run the Redis directory tests")
	}

	ctx := context.Background()
	world := openRedisDirectory(t, addr, "world-1")
	gateway := NewRedisReader(world.client, world.prefix)
	t.Cleanup(func() { gateway.Close() })

	live, err := gateway.LiveNodes(ctx)
	if err != nil {
		t.Fatalf("listing live nodes: %v", err)
	}
	for _, n := range live {
		if n != "world-1" {
			t.Errorf("a process that hosts nothing is listed as live: %s", n)
		}
	}
	if len(live) != 1 {
		t.Errorf("live nodes are %v, want only the world node", live)
	}

	// And placement, asked through the gateway's own directory, still lands on
	// the world node.
	inst, err := gateway.Join(ctx, RoomKey{MapID: "henesys", Placement: PlacementShared}, 10)
	if err != nil {
		t.Fatalf("placing a room: %v", err)
	}
	if inst.Node != "world-1" {
		t.Errorf("a room was placed on %q, which hosts nothing", inst.Node)
	}
}

// A room on a node that has gone is not handed out, and is not left behind.
//
// A room is hosted by a process; when that process dies the room dies with it,
// but its registration does not. It stays in the directory holding a slot count
// for players who no longer exist, and placement keeps handing it out --
// everyone sent there waits out a request to a node that will never answer, and
// the login fails. Forever, because nothing removed the registration.
//
// Found by killing a world node under load: two rooms stayed registered on it
// holding thirty and nineteen phantom players, and reconnecting bots were
// refused with "could not enter the world" for the rest of the run.

// stranded returns a directory and an instance on a node that has died.
//
// The node registers, takes a room, and then stops heartbeating without
// releasing anything -- which is what a killed process looks like from here.
func stranded(t *testing.T, key RoomKey) (*Redis, Instance) {
	t.Helper()

	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set MMO_TEST_REDIS_ADDR to run the Redis directory tests")
	}

	ctx := context.Background()
	alive := openRedisDirectory(t, addr, "world-alive")

	doomed, err := NewRedis(ctx, alive.client, alive.prefix, "world-doomed")
	if err != nil {
		t.Fatalf("second node: %v", err)
	}

	// A capacity of one means every join fills its instance and the next has to
	// create another, so two joins put one room on each node -- whichever order
	// placement picks them in.
	var onDoomed Instance
	for range 2 {
		inst, err := alive.Join(ctx, key, 1)
		if err != nil {
			t.Fatalf("placing a room: %v", err)
		}
		if inst.Node == "world-doomed" {
			onDoomed = inst
		}
	}
	if onDoomed.ID == 0 {
		t.Fatal("neither room landed on world-doomed, so there is nothing to strand")
	}

	doomed.Close()
	expireNode(t, alive, "world-doomed")
	return alive, onDoomed
}

func TestRedisPlacementReapsRoomsOnDeadNodes(t *testing.T) {
	key := sharedKey("henesys")
	dir, dead := stranded(t, key)
	ctx := context.Background()

	got, err := dir.Join(ctx, key, 1)
	if err != nil {
		t.Fatalf("placing after a node died: %v", err)
	}
	if got.Node == "world-doomed" {
		t.Fatalf("placed into a room on a node that has gone (instance %d)", got.ID)
	}

	// Gone, not merely skipped: left in place it keeps its slot count and its
	// share of the node's load forever, and every future placement has to step
	// over it again.
	if _, found, err := dir.Lookup(ctx, dead.ID); err != nil {
		t.Fatalf("looking up the reaped instance: %v", err)
	} else if found {
		t.Errorf("instance %d is still registered on a node that has gone", dead.ID)
	}

	// And its share of the node's load with it. This is the counter placement
	// decides by, and it only ever rises unless something takes rooms back off
	// it -- so a node that dies and comes back looks permanently busier than it
	// is, and stops being given work it could do.
	load, err := dir.client.ZScore(ctx, dir.loadKey(), "world-doomed").Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		t.Fatalf("reading the load counter: %v", err)
	}
	if load != 0 {
		t.Errorf("the dead node is still carrying a load of %v for a room that is gone", load)
	}
}

// A dead node's channels are not advertised.
//
// This is the list a player is shown when they open the channel picker, and
// offering a channel whose host has gone is offering a door that leads nowhere:
// they pick it, the switch fails, and nothing about it says why.
func TestRedisDeadChannelsAreNotAdvertised(t *testing.T) {
	key := sharedKey("henesys")
	dir, dead := stranded(t, key)

	// Asserted before anything reaps it, so this is really testing the listing
	// rather than the cleanup that placement happens to do first.
	channels, err := dir.InstancesFor(context.Background(), key)
	if err != nil {
		t.Fatalf("listing channels: %v", err)
	}
	for _, c := range channels {
		if c.ID == dead.ID {
			t.Errorf("channel %d on a node that has gone is still being advertised", c.ID)
		}
	}
	if len(channels) == 0 {
		t.Error("every channel was filtered out; the live one should still be listed")
	}
}

// Switching to a channel on a dead node is refused as unknown, not as full.
//
// Not full, because the room is not busy -- there is no room. A player told the
// channel is full waits and tries again; one told it does not exist picks
// another.
func TestRedisJoiningAChannelOnADeadNodeIsRefused(t *testing.T) {
	dir, dead := stranded(t, sharedKey("henesys"))

	_, err := dir.JoinInstance(context.Background(), dead.ID)
	if !errors.Is(err, ErrUnknownInstance) {
		t.Errorf("joining a channel on a node that has gone gave %v, want ErrUnknownInstance", err)
	}
}

// A private room whose only instance was on a dead node can be made again.
//
// A private key is one instance by definition, so the check that stops a second
// being created has to count the ones that still exist. Counting the dead one
// means a party whose dungeon node died can never start another -- the directory
// tells them their dungeon is full, forever.
func TestRedisPrivateRoomCanBeRemadeAfterItsNodeDies(t *testing.T) {
	key := privateKey("crypt", "party-1")

	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set MMO_TEST_REDIS_ADDR to run the Redis directory tests")
	}

	ctx := context.Background()
	alive := openRedisDirectory(t, addr, "world-alive")

	doomed, err := NewRedis(ctx, alive.client, alive.prefix, "world-doomed")
	if err != nil {
		t.Fatalf("second node: %v", err)
	}

	// Place the party's dungeon, then kill whichever node got it.
	inst, err := alive.Join(ctx, key, 6)
	if err != nil {
		t.Fatalf("placing the dungeon: %v", err)
	}
	doomed.Close()
	expireNode(t, alive, inst.Node)

	again, err := alive.Join(ctx, key, 6)
	if err != nil {
		t.Fatalf("the party could not start another dungeon after their node died: %v", err)
	}
	if again.ID == inst.ID {
		t.Errorf("placed back into the dungeon on the dead node (instance %d)", again.ID)
	}
}

// expireNode makes a node look like it stopped heartbeating.
func expireNode(t *testing.T, r *Redis, node NodeID) {
	t.Helper()

	// Its liveness entry is scored with the moment it expires, so scoring it in
	// the past is exactly what three missed heartbeats look like.
	if err := r.client.ZAdd(context.Background(), r.aliveKey(),
		redis.Z{Score: 1, Member: string(node)}).Err(); err != nil {
		t.Fatalf("expiring %s: %v", node, err)
	}
}

// A withdrawn directory offers its node no more work.
//
// The first step of a drain: a process that is shutting down still has rooms
// and characters to finish with, and a character arriving while it closes its
// sessions arrives at the one place that cannot look after it. Letting the
// liveness TTL do this instead means fifteen more seconds of new arrivals into
// a process that is leaving.
func TestWithdrawStopsPlacement(t *testing.T) {
	eachDirectory(t, func(t *testing.T, d Directory) {
		ctx := context.Background()

		// It works before withdrawing, so the refusal afterwards is the
		// withdrawal rather than something else being wrong.
		if _, err := d.Join(ctx, sharedKey("henesys"), 10); err != nil {
			t.Fatalf("placing before withdrawing: %v", err)
		}

		if err := d.Withdraw(ctx); err != nil {
			t.Fatalf("withdrawing: %v", err)
		}

		live, err := d.LiveNodes(ctx)
		if err != nil {
			t.Fatalf("listing live nodes: %v", err)
		}
		if len(live) != 0 {
			t.Errorf("a withdrawn directory still lists %v as live", live)
		}

		// A room that needs creating has nowhere to go. Refused as "no live
		// node" rather than "no capacity": the world is not full, there is no
		// world.
		_, err = d.NewInstance(ctx, sharedKey("ellinia"), 10)
		if !errors.Is(err, ErrNoLiveNode) {
			t.Errorf("placing after withdrawing gave %v, want ErrNoLiveNode", err)
		}

		// Idempotent, because a drain can be asked for twice and a shutdown
		// path is the worst place for a second call to be an error.
		if err := d.Withdraw(ctx); err != nil {
			t.Errorf("withdrawing twice: %v", err)
		}
	})
}
