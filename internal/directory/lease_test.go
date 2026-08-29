package directory

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Both implementations are held to the same contract.
//
// They are meant to be interchangeable -- one process today, many nodes later
// -- so a behavioural difference between them is a bug that would only surface
// on the day the system is scaled out, which is the worst possible time to
// find it. Running one suite against both is what keeps them honest.
func eachLeases(t *testing.T, fn func(t *testing.T, l Leases)) {
	t.Helper()

	t.Run("memory", func(t *testing.T) {
		l := NewMemoryLeases()
		defer l.Close()
		fn(t, l)
	})

	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("MMO_TEST_REDIS_ADDR")
		if addr == "" {
			t.Skip("MMO_TEST_REDIS_ADDR is not set; skipping Redis lease tests")
		}

		client := redis.NewClient(&redis.Options{Addr: addr})
		ctx := context.Background()
		if err := client.Ping(ctx).Err(); err != nil {
			t.Fatalf("connecting to Redis: %v", err)
		}

		// A unique prefix per test, so parallel runs and leftover keys from a
		// previous run cannot interfere.
		prefix := "test:" + uuid.NewString()
		l := NewRedisLeases(client, prefix)
		t.Cleanup(func() {
			keys, _ := client.Keys(ctx, prefix+":*").Result()
			if len(keys) > 0 {
				client.Del(ctx, keys...)
			}
			l.Close()
		})
		fn(t, l)
	})
}

func TestAcquireGivesExclusiveOwnership(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()
		char := uuid.NewString()

		l, err := leases.Acquire(ctx, char, "node-a")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if l.Token <= 0 {
			t.Errorf("token is %d, want a positive value", l.Token)
		}
		if !l.Valid(time.Now()) {
			t.Error("a freshly acquired lease is not valid")
		}

		// This is the invariant: nobody else can take it.
		if _, err := leases.Acquire(ctx, char, "node-b"); !errors.Is(err, ErrLeaseHeld) {
			t.Errorf("second acquire returned %v, want ErrLeaseHeld", err)
		}
	})
}

func TestDifferentCharactersDoNotBlockEachOther(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()

		if _, err := leases.Acquire(ctx, uuid.NewString(), "node-a"); err != nil {
			t.Fatalf("first: %v", err)
		}
		if _, err := leases.Acquire(ctx, uuid.NewString(), "node-a"); err != nil {
			t.Errorf("a lease on one character blocked another: %v", err)
		}
	})
}

// Tokens increase globally and never repeat, which is what makes the database
// fencing check meaningful.
func TestTokensStrictlyIncrease(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()

		var last int64
		for i := 0; i < 10; i++ {
			char := uuid.NewString()
			l, err := leases.Acquire(ctx, char, "node-a")
			if err != nil {
				t.Fatalf("acquire %d: %v", i, err)
			}
			if l.Token <= last {
				t.Fatalf("token %d did not increase past %d", l.Token, last)
			}
			last = l.Token
			leases.Release(ctx, l)
		}
	})
}

// Reacquiring after a release must issue a *higher* token, not reuse the old
// one. The previous holder may still be running and about to write, and the
// gap between tokens is what gets its write rejected.
func TestReacquiringIssuesAHigherToken(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()
		char := uuid.NewString()

		first, err := leases.Acquire(ctx, char, "node-a")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if err := leases.Release(ctx, first); err != nil {
			t.Fatalf("release: %v", err)
		}

		second, err := leases.Acquire(ctx, char, "node-b")
		if err != nil {
			t.Fatalf("reacquire: %v", err)
		}
		if second.Token <= first.Token {
			t.Errorf("reacquired token %d is not above the previous %d; "+
				"a stale writer would not be fenced", second.Token, first.Token)
		}
	})
}

func TestRenewExtendsAHeldLease(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()
		char := uuid.NewString()

		l, err := leases.Acquire(ctx, char, "node-a")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}

		renewed, err := leases.Renew(ctx, l)
		if err != nil {
			t.Fatalf("renew: %v", err)
		}
		if renewed.Token != l.Token {
			t.Errorf("renewal changed the token from %d to %d", l.Token, renewed.Token)
		}
		if !renewed.ExpiresAt.After(l.ExpiresAt) && !renewed.ExpiresAt.Equal(l.ExpiresAt) {
			t.Error("renewal did not extend the expiry")
		}
	})
}

// Losing a renewal is never routine: it means ownership moved, and the session
// must end rather than reacquire and continue over the new owner's work.
func TestRenewAfterOwnershipMovesIsLost(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()
		char := uuid.NewString()

		stale, err := leases.Acquire(ctx, char, "node-a")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		leases.Release(ctx, stale)

		if _, err := leases.Acquire(ctx, char, "node-b"); err != nil {
			t.Fatalf("reacquire: %v", err)
		}

		if _, err := leases.Renew(ctx, stale); !errors.Is(err, ErrLeaseLost) {
			t.Errorf("renewing a lease that moved returned %v, want ErrLeaseLost", err)
		}
	})
}

func TestRenewingAnUnheldLeaseIsLost(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()

		phantom := Lease{CharacterID: uuid.NewString(), Node: "node-a", Token: 999}
		if _, err := leases.Renew(ctx, phantom); !errors.Is(err, ErrLeaseLost) {
			t.Errorf("renewing a lease nobody holds returned %v, want ErrLeaseLost", err)
		}
	})
}

// A straggler from a previous session must not be able to revoke the current
// owner's lease -- exactly the situation fencing exists to prevent.
func TestReleaseDoesNotRevokeANewerOwner(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()
		char := uuid.NewString()

		stale, _ := leases.Acquire(ctx, char, "node-a")
		leases.Release(ctx, stale)

		current, err := leases.Acquire(ctx, char, "node-b")
		if err != nil {
			t.Fatalf("reacquire: %v", err)
		}

		// The old holder, unaware ownership moved, releases again.
		if err := leases.Release(ctx, stale); err != nil {
			t.Fatalf("stale release errored: %v", err)
		}

		// node-b must still own it.
		if _, err := leases.Acquire(ctx, char, "node-c"); !errors.Is(err, ErrLeaseHeld) {
			t.Error("a stale release revoked the current owner's lease")
		}
		_ = current
	})
}

// Releasing something already gone is not an error: the desired state is
// reached either way, and a disconnect racing a shutdown makes this happen.
func TestReleaseIsIdempotent(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()
		char := uuid.NewString()

		l, _ := leases.Acquire(ctx, char, "node-a")
		for i := 0; i < 3; i++ {
			if err := leases.Release(ctx, l); err != nil {
				t.Fatalf("release %d: %v", i, err)
			}
		}
	})
}

// Only one of many simultaneous acquirers may win. This is the property that
// stops two nodes loading one character and duplicating its items.
func TestConcurrentAcquiresElectOneWinner(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()
		char := uuid.NewString()

		const contenders = 20
		var wg sync.WaitGroup
		won := make(chan Lease, contenders)

		for i := 0; i < contenders; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				if l, err := leases.Acquire(ctx, char, NodeID("node-"+string(rune('a'+n%26)))); err == nil {
					won <- l
				}
			}(i)
		}
		wg.Wait()
		close(won)

		if n := len(won); n != 1 {
			t.Errorf("%d of %d contenders acquired the same character, want exactly 1",
				n, contenders)
		}
	})
}

func TestAcquireRejectsAnEmptyCharacterID(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		if _, err := leases.Acquire(context.Background(), "", "node-a"); err == nil {
			t.Error("an empty character ID was accepted")
		}
	})
}

func TestLeaseHeldErrorNamesTheHolder(t *testing.T) {
	eachLeases(t, func(t *testing.T, leases Leases) {
		ctx := context.Background()
		char := uuid.NewString()

		if _, err := leases.Acquire(ctx, char, "node-alpha"); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		_, err := leases.Acquire(ctx, char, "node-beta")
		if err == nil {
			t.Fatal("expected the second acquire to fail")
		}
		// Naming the holder is what makes a stuck character diagnosable
		// without reading the lease store by hand.
		if !strings.Contains(err.Error(), "node-alpha") {
			t.Errorf("error %q does not name the holding node", err)
		}
	})
}

// --- expiry, memory implementation only -------------------------------------

// A crashed node must not strand a character forever, and reclaiming an
// expired lease must still issue a higher token -- the crashed process may not
// actually be dead.
func TestExpiredLeaseIsReclaimedWithAHigherToken(t *testing.T) {
	leases := NewMemoryLeases()
	ctx := context.Background()
	char := uuid.NewString()

	now := time.Now()
	leases.now = func() time.Time { return now }

	stale, err := leases.Acquire(ctx, char, "crashed-node")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Time passes; the holder never renewed.
	now = now.Add(LeaseTTL + time.Second)

	reclaimed, err := leases.Acquire(ctx, char, "healthy-node")
	if err != nil {
		t.Fatalf("reclaiming an expired lease: %v", err)
	}
	if reclaimed.Token <= stale.Token {
		t.Errorf("reclaimed token %d is not above the stale %d; the crashed node's "+
			"writes would not be fenced if it came back", reclaimed.Token, stale.Token)
	}
	if stale.Valid(now) {
		t.Error("the expired lease still reports itself valid")
	}
}

func TestHeldCountsOnlyUnexpiredLeases(t *testing.T) {
	leases := NewMemoryLeases()
	ctx := context.Background()

	now := time.Now()
	leases.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if _, err := leases.Acquire(ctx, uuid.NewString(), "node-a"); err != nil {
			t.Fatalf("acquire: %v", err)
		}
	}
	if got := leases.Held(); got != 3 {
		t.Errorf("Held() = %d, want 3", got)
	}

	now = now.Add(LeaseTTL + time.Second)
	if got := leases.Held(); got != 0 {
		t.Errorf("Held() = %d after every lease expired, want 0", got)
	}
}

func TestLeaseValidity(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		l    Lease
		want bool
	}{
		{"complete and unexpired", Lease{CharacterID: "c", Token: 1, ExpiresAt: now.Add(time.Minute)}, true},
		{"expired", Lease{CharacterID: "c", Token: 1, ExpiresAt: now.Add(-time.Second)}, false},
		{"no character", Lease{Token: 1, ExpiresAt: now.Add(time.Minute)}, false},
		{"no token", Lease{CharacterID: "c", ExpiresAt: now.Add(time.Minute)}, false},
		{"zero value", Lease{}, false},
	}
	for _, tt := range tests {
		if got := tt.l.Valid(now); got != tt.want {
			t.Errorf("%s: Valid() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
