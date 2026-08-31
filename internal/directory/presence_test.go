package directory

import (
	"context"
	"os"
	"slices"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The Presence contract, run against every implementation.
//
// Worth noting that presence had no unit tests at all before this: it was
// exercised only through the cluster suite, which meant a whisper reaching the
// wrong node would have been diagnosed as a chat bug. So this is new coverage
// rather than parity work, and the two implementations get it at once.

type presenceUnderTest struct {
	name string
	open func(t *testing.T) Presence
}

func presenceImplementations() []presenceUnderTest {
	impls := []presenceUnderTest{{
		name: "memory",
		open: func(t *testing.T) Presence {
			t.Helper()
			p := NewMemoryPresence()
			t.Cleanup(func() { p.Close() })
			return p
		},
	}}

	if addr := os.Getenv("MMO_TEST_REDIS_ADDR"); addr != "" {
		impls = append(impls, presenceUnderTest{
			name: "redis",
			open: func(t *testing.T) Presence { return openRedisPresence(t, addr) },
		})
	}
	return impls
}

// openRedisPresence returns a Redis presence table with a namespace of its own.
func openRedisPresence(t *testing.T, addr string) *RedisPresence {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: addr})
	prefix := "mmopres:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx := context.Background()

	t.Cleanup(func() {
		if keys, err := client.Keys(ctx, prefix+"*").Result(); err == nil && len(keys) > 0 {
			client.Del(ctx, keys...)
		}
		client.Close()
	})
	return NewRedisPresence(client, prefix)
}

func eachPresence(t *testing.T, fn func(t *testing.T, p Presence)) {
	t.Helper()
	for _, impl := range presenceImplementations() {
		t.Run(impl.name, func(t *testing.T) { fn(t, impl.open(t)) })
	}
}

func online(id, name, node, mapID string) Online {
	return Online{CharacterID: id, Name: name, Node: NodeID(node), MapID: mapID}
}

func mustByName(t *testing.T, p Presence, ctx context.Context, name string) (Online, bool) {
	t.Helper()
	who, ok, err := p.ByName(ctx, name)
	if err != nil {
		t.Fatalf("ByName(%q): %v", name, err)
	}
	return who, ok
}

func mustByID(t *testing.T, p Presence, ctx context.Context, id string) (Online, bool) {
	t.Helper()
	who, ok, err := p.ByID(ctx, id)
	if err != nil {
		t.Fatalf("ByID(%q): %v", id, err)
	}
	return who, ok
}

func mustOnlineList(t *testing.T, p Presence, ctx context.Context) []Online {
	t.Helper()
	out, err := p.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return out
}

// --- announcing and finding --------------------------------------------------

func TestPresenceAnnounceThenFind(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		if err := p.Announce(ctx, online("char-1", "Alice", "node-a", "henesys")); err != nil {
			t.Fatalf("announce: %v", err)
		}

		who, ok := mustByID(t, p, ctx, "char-1")
		if !ok {
			t.Fatal("ByID did not find a character that was just announced")
		}
		if who.Name != "Alice" || who.Node != "node-a" || who.MapID != "henesys" {
			t.Errorf("record = %+v, want Alice on node-a in henesys", who)
		}

		byName, ok := mustByName(t, p, ctx, "Alice")
		if !ok || byName.CharacterID != "char-1" {
			t.Errorf("ByName gave %+v (%v), want char-1", byName, ok)
		}
	})
}

// A player typing a friend's name should not have to match its capitalisation,
// and the display case has to survive the round trip.
func TestPresenceLooksUpNamesCaseInsensitively(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()
		p.Announce(ctx, online("char-1", "Alice", "node-a", "henesys"))

		for _, typed := range []string{"alice", "ALICE", "AlIcE", "  alice  "} {
			who, ok := mustByName(t, p, ctx, typed)
			if !ok {
				t.Errorf("ByName(%q) found nobody", typed)
				continue
			}
			if who.Name != "Alice" {
				t.Errorf("ByName(%q) gave name %q, want the stored case Alice", typed, who.Name)
			}
		}
	})
}

// Names are not ASCII-only, and the lookup form has to be the same one the
// implementation stored. A Lua `string.lower` cannot reproduce Go's, which is
// why the Redis records carry their normalised name rather than deriving it.
func TestPresenceHandlesNonASCIINames(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()
		if err := p.Announce(ctx, online("char-1", "Ünicörn", "node-a", "henesys")); err != nil {
			t.Fatalf("announce: %v", err)
		}

		if _, ok := mustByName(t, p, ctx, "ünicörn"); !ok {
			t.Error("a lower-cased non-ASCII name found nobody")
		}

		// And renaming still drops the old index, which is the case that breaks
		// if the two normalisations disagree.
		if err := p.Announce(ctx, online("char-1", "Renamed", "node-a", "henesys")); err != nil {
			t.Fatalf("re-announce: %v", err)
		}
		if who, ok := mustByName(t, p, ctx, "ünicörn"); ok {
			t.Errorf("the old non-ASCII name still resolves to %+v", who)
		}
	})
}

func TestPresenceMissingCharacterIsNotAnError(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		if _, ok := mustByID(t, p, ctx, "nobody"); ok {
			t.Error("ByID found a character that was never announced")
		}
		if _, ok := mustByName(t, p, ctx, "Nobody"); ok {
			t.Error("ByName found a character that was never announced")
		}
	})
}

// Announcing again replaces the record: it is what a transfer does, and the node
// is the field that changed.
func TestPresenceReannounceUpdatesTheRecord(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		p.Announce(ctx, online("char-1", "Alice", "node-a", "henesys"))
		p.Announce(ctx, online("char-1", "Alice", "node-b", "ellinia"))

		who, ok := mustByID(t, p, ctx, "char-1")
		if !ok {
			t.Fatal("the character vanished on re-announce")
		}
		if who.Node != "node-b" || who.MapID != "ellinia" {
			t.Errorf("record = %+v, want node-b in ellinia", who)
		}

		if got := mustOnlineList(t, p, ctx); len(got) != 1 {
			t.Errorf("re-announcing produced %d records, want 1", len(got))
		}
	})
}

// A rename must not leave the old name pointing at the character forever: a
// whisper to it would reach somebody who no longer exists under that name.
func TestPresenceRenameDropsTheOldName(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		p.Announce(ctx, online("char-1", "Alice", "node-a", "henesys"))
		p.Announce(ctx, online("char-1", "Alicia", "node-a", "henesys"))

		if who, ok := mustByName(t, p, ctx, "Alice"); ok {
			t.Errorf("the old name still resolves to %+v", who)
		}
		if who, ok := mustByName(t, p, ctx, "Alicia"); !ok || who.CharacterID != "char-1" {
			t.Errorf("the new name gave %+v (%v), want char-1", who, ok)
		}
	})
}

// A name freed by one character and taken by another belongs to the new one,
// and the old character leaving must not revoke it.
func TestPresenceANameTakenOverIsNotRevokedByTheOldHolder(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		p.Announce(ctx, online("char-1", "Alice", "node-a", "henesys"))
		// char-1 renames, freeing the name, and char-2 takes it.
		p.Announce(ctx, online("char-1", "Alicia", "node-a", "henesys"))
		p.Announce(ctx, online("char-2", "Alice", "node-b", "ellinia"))

		// Now the original leaves.
		if err := p.Forget(ctx, "char-1"); err != nil {
			t.Fatalf("forget: %v", err)
		}

		who, ok := mustByName(t, p, ctx, "Alice")
		if !ok {
			t.Fatal("char-1 leaving revoked a name char-2 had taken over")
		}
		if who.CharacterID != "char-2" {
			t.Errorf("Alice resolves to %s, want char-2", who.CharacterID)
		}
	})
}

// Two live records can name the same character-name in flight: names are
// unique, so this needs an announce and a rename to interleave across nodes --
// narrow, but the symptom is a player nobody can whisper.
//
// Unlike the test above there is no rename first, so the leaver's *own* name is
// the contested one. That is what makes the "does it still point at me?" check
// load-bearing rather than decorative.
func TestPresenceForgetDoesNotRevokeAContestedName(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		p.Announce(ctx, online("char-1", "Alice", "node-a", "henesys"))
		p.Announce(ctx, online("char-2", "Alice", "node-b", "ellinia"))

		// The older record leaves; the name is no longer its to give up.
		if err := p.Forget(ctx, "char-1"); err != nil {
			t.Fatalf("forget: %v", err)
		}

		who, ok := mustByName(t, p, ctx, "Alice")
		if !ok {
			t.Fatal("char-1 leaving took a name that had moved to char-2")
		}
		if who.CharacterID != "char-2" {
			t.Errorf("Alice resolves to %s, want char-2", who.CharacterID)
		}
	})
}

// The same contest, resolved by a rename rather than a departure.
func TestPresenceRenameDoesNotRevokeAContestedName(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		p.Announce(ctx, online("char-1", "Alice", "node-a", "henesys"))
		p.Announce(ctx, online("char-2", "Alice", "node-b", "ellinia"))
		// char-1 renames. Dropping its old name would drop char-2's.
		p.Announce(ctx, online("char-1", "Alicia", "node-a", "henesys"))

		who, ok := mustByName(t, p, ctx, "Alice")
		if !ok {
			t.Fatal("renaming char-1 took the name char-2 answers to")
		}
		if who.CharacterID != "char-2" {
			t.Errorf("Alice resolves to %s, want char-2", who.CharacterID)
		}
		if who, ok := mustByName(t, p, ctx, "Alicia"); !ok || who.CharacterID != "char-1" {
			t.Errorf("Alicia gave %+v (%v), want char-1", who, ok)
		}
	})
}

// --- forgetting -------------------------------------------------------------

func TestPresenceForgetRemovesBothIndexes(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		p.Announce(ctx, online("char-1", "Alice", "node-a", "henesys"))
		if err := p.Forget(ctx, "char-1"); err != nil {
			t.Fatalf("forget: %v", err)
		}

		if _, ok := mustByID(t, p, ctx, "char-1"); ok {
			t.Error("ByID still finds a forgotten character")
		}
		if _, ok := mustByName(t, p, ctx, "Alice"); ok {
			t.Error("ByName still finds a forgotten character; the name index leaked")
		}
		if got := mustOnlineList(t, p, ctx); len(got) != 0 {
			t.Errorf("List returns %d records after forgetting the only one", len(got))
		}
	})
}

func TestPresenceForgettingSomebodyUnknownIsNotAnError(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		if err := p.Forget(context.Background(), "nobody"); err != nil {
			t.Errorf("forgetting an unknown character = %v, want nil", err)
		}
	})
}

// A node that has just started is holding nobody. Presence still claiming
// otherwise is left over from the process that died, and would route whispers
// at a socket that is gone.
func TestPresenceForgetNodeClearsOnlyThatNode(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		p.Announce(ctx, online("char-1", "Alice", "node-a", "henesys"))
		p.Announce(ctx, online("char-2", "Bob", "node-a", "henesys"))
		p.Announce(ctx, online("char-3", "Carol", "node-b", "ellinia"))

		if err := p.ForgetNode(ctx, "node-a"); err != nil {
			t.Fatalf("forget node: %v", err)
		}

		if _, ok := mustByID(t, p, ctx, "char-1"); ok {
			t.Error("char-1 survived its node being cleared")
		}
		if _, ok := mustByName(t, p, ctx, "Bob"); ok {
			t.Error("Bob's name index survived its node being cleared")
		}

		who, ok := mustByID(t, p, ctx, "char-3")
		if !ok {
			t.Fatal("clearing node-a also cleared node-b")
		}
		if who.Name != "Carol" {
			t.Errorf("remaining record = %+v, want Carol", who)
		}

		if got := mustOnlineList(t, p, ctx); len(got) != 1 {
			t.Errorf("List returns %d records, want only Carol", len(got))
		}
	})
}

// --- the roster -------------------------------------------------------------

// A roster that reshuffles every refresh is unusable.
func TestPresenceListIsOrderedByName(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		for i, name := range []string{"Carol", "alice", "Bob"} {
			p.Announce(ctx, online("char-"+strconv.Itoa(i), name, "node-a", "henesys"))
		}

		got := mustOnlineList(t, p, ctx)
		if len(got) != 3 {
			t.Fatalf("List returned %d records, want 3", len(got))
		}
		want := []string{"alice", "Bob", "Carol"}
		for i, name := range want {
			if got[i].Name != name {
				t.Errorf("List[%d] = %q, want %q (order = %v)", i, got[i].Name, name, names(got))
				break
			}
		}
	})
}

// Away is a real state, not a synonym for offline: a whisper to an away
// character should say so rather than vanishing.
func TestPresenceCarriesTheAwayFlag(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		who := online("char-1", "Alice", "node-a", "henesys")
		who.Away = true
		if err := p.Announce(ctx, who); err != nil {
			t.Fatalf("announce: %v", err)
		}

		got, ok := mustByName(t, p, ctx, "Alice")
		if !ok {
			t.Fatal("an away character is not online at all")
		}
		if !got.Away {
			t.Error("the away flag did not survive the round trip")
		}
	})
}

// --- concurrency ------------------------------------------------------------

// Announcing and forgetting from several goroutines must leave the two indexes
// agreeing: a name resolving to a character that is gone is a whisper into
// nothing.
func TestPresenceConcurrentAnnouncesAndForgets(t *testing.T) {
	eachPresence(t, func(t *testing.T, p Presence) {
		ctx := context.Background()

		const characters = 40
		var wg sync.WaitGroup
		for i := 0; i < characters; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				id := "char-" + strconv.Itoa(n)
				name := "Player" + strconv.Itoa(n)
				p.Announce(ctx, online(id, name, "node-a", "henesys"))
				if n%2 == 0 {
					p.Forget(ctx, id)
				}
			}(i)
		}
		wg.Wait()

		// Every name still in the index has to resolve to a character still in
		// the table.
		for i := 0; i < characters; i++ {
			name := "Player" + strconv.Itoa(i)
			who, ok := mustByName(t, p, ctx, name)
			if !ok {
				continue
			}
			if _, present := mustByID(t, p, ctx, who.CharacterID); !present {
				t.Errorf("%q resolves to %s, which is not in the table",
					name, who.CharacterID)
			}
		}
	})
}

func names(list []Online) []string {
	out := make([]string, 0, len(list))
	for _, who := range list {
		out = append(out, who.Name)
	}
	return out
}

// --- the Redis implementation's own properties ------------------------------

// Two presence tables on separate connections are two nodes, and a character
// announced on one has to be findable from the other. This is the property the
// whole whisper path depends on and the one Memory cannot demonstrate.
func TestRedisPresenceIsSharedBetweenNodes(t *testing.T) {
	addr := redisAddr(t)
	ctx := context.Background()

	a := openRedisPresence(t, addr)
	b := NewRedisPresence(a.client, a.prefix)

	if err := a.Announce(ctx, online("char-1", "Alice", "node-a", "henesys")); err != nil {
		t.Fatalf("announce on node-a: %v", err)
	}

	who, ok, err := b.ByName(ctx, "alice")
	if err != nil {
		t.Fatalf("ByName on node-b: %v", err)
	}
	if !ok {
		t.Fatal("node-b cannot find a character node-a announced")
	}
	if who.Node != "node-a" {
		t.Errorf("node-b thinks Alice is on %s, want node-a", who.Node)
	}

	// And forgetting on one is forgetting on the other.
	if err := b.Forget(ctx, "char-1"); err != nil {
		t.Fatalf("forget on node-b: %v", err)
	}
	if _, ok, _ := a.ByID(ctx, "char-1"); ok {
		t.Error("node-a still sees a character node-b forgot")
	}
}

// A dropped record must take its name index with it.
//
// Redis-only because it is not observable through the interface: ByName
// resolves through ByID, so an orphaned name reads as "nobody online" exactly
// like a name that was never there. The leak is silent and unbounded -- one
// entry per name the server has ever seen -- which is the kind of thing that
// only shows up as memory a year later.
func TestRedisPresenceLeavesNoOrphanedNames(t *testing.T) {
	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set MMO_TEST_REDIS_ADDR to run the Redis presence tests")
	}

	ctx := context.Background()
	p := openRedisPresence(t, addr)

	names := func() []string {
		t.Helper()
		keys, err := p.client.HKeys(ctx, p.byNameKey()).Result()
		if err != nil {
			t.Fatalf("reading the name index: %v", err)
		}
		sort.Strings(keys)
		return keys
	}

	p.Announce(ctx, online("char-1", "Alice", "node-a", "henesys"))
	p.Announce(ctx, online("char-2", "Bob", "node-a", "henesys"))
	p.Announce(ctx, online("char-3", "Carol", "node-b", "ellinia"))

	if err := p.Forget(ctx, "char-1"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got := names(); !slices.Equal(got, []string{"bob", "carol"}) {
		t.Errorf("after Forget the name index holds %v, want [bob carol]", got)
	}

	if err := p.ForgetNode(ctx, "node-a"); err != nil {
		t.Fatalf("forget node: %v", err)
	}
	if got := names(); !slices.Equal(got, []string{"carol"}) {
		t.Errorf("after ForgetNode the name index holds %v, want [carol]", got)
	}

	if err := p.ForgetNode(ctx, "node-b"); err != nil {
		t.Fatalf("forget node: %v", err)
	}
	if got := names(); len(got) != 0 {
		t.Errorf("the name index still holds %v with nobody online", got)
	}
}
