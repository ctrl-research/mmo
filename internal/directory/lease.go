package directory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Character ownership leases.
//
// The invariant: at any instant, exactly one process may mutate a given
// character. Item duplication -- the defining bug of every MMO that has ever
// shipped one -- is almost always a violation of it. Two processes load the
// same character, both believe they own its inventory, both write, and one
// sword becomes two.
//
// A lease alone is not sufficient. A node can be paused by GC or a network
// partition for longer than the lease TTL, have its lease granted elsewhere,
// then wake believing it still holds one. So every lease carries a
// monotonically increasing *fencing token*, which is carried into the database
// and checked on write: `UPDATE ... WHERE lease_token <= $token`. Redis
// provides mutual exclusion most of the time; the fencing check provides
// correctness always. See docs/data-model.md.

// Lease errors.
var (
	// ErrLeaseHeld means another node currently owns the character.
	ErrLeaseHeld = errors.New("directory: character is leased elsewhere")

	// ErrLeaseLost means this node no longer holds the lease it thought it
	// did. It is never routine: the only safe response is to discard the
	// in-memory character and end the session, never to reacquire and
	// continue, which would overwrite a newer owner's work.
	ErrLeaseLost = errors.New("directory: lease lost")
)

// Lease timing.
const (
	// LeaseTTL is how long a lease survives without renewal. Long enough to
	// ride out a GC pause or a brief network blip; short enough that a crashed
	// node does not strand a character for minutes.
	LeaseTTL = 30 * time.Second

	// LeaseRenewInterval is how often a holder should renew. A third of the
	// TTL, so two consecutive renewals can fail before the lease lapses.
	LeaseRenewInterval = 10 * time.Second
)

// Lease is proof of exclusive ownership of one character.
type Lease struct {
	CharacterID string
	Node        NodeID

	// Token is monotonically increasing across every lease ever issued. It is
	// what makes ownership verifiable at the database rather than merely
	// probable at the lock.
	Token int64

	ExpiresAt time.Time
}

// Valid reports whether the lease is non-empty and unexpired.
func (l Lease) Valid(now time.Time) bool {
	return l.CharacterID != "" && l.Token > 0 && now.Before(l.ExpiresAt)
}

// Leases issues and tracks character ownership.
//
// Kept separate from Directory because the two answer different questions --
// where a room lives, versus who owns a character -- and a caller usually
// needs one or the other. Both have an in-memory implementation for a single
// process and a Redis implementation for many.
type Leases interface {
	// Acquire takes exclusive ownership, returning ErrLeaseHeld if another
	// node holds it.
	Acquire(ctx context.Context, characterID string, node NodeID) (Lease, error)

	// Renew extends a lease the caller still holds. It returns ErrLeaseLost if
	// ownership has moved, which must end the session rather than be retried.
	Renew(ctx context.Context, l Lease) (Lease, error)

	// Release relinquishes ownership. Releasing a lease that has already moved
	// on is not an error -- the desired state is reached either way -- but it
	// must not revoke the new owner's lease.
	Release(ctx context.Context, l Lease) error

	// Close releases resources.
	Close() error
}

// MemoryLeases is a Leases held in one process.
//
// Correct for a single node, which is the whole point: at hobby scale there is
// only one process, and it still exercises the same acquire/renew/release
// protocol and the same fencing tokens as the Redis implementation. The
// database-side fencing check is what actually enforces the invariant, and
// that is identical either way.
type MemoryLeases struct {
	mu     sync.Mutex
	held   map[string]Lease
	nextID int64
	now    func() time.Time
}

// NewMemoryLeases returns an empty lease table.
//
// Tokens start at one. That is wrong for a server that has run before: the
// characters in the database carry tokens from the last run, and handing out
// lower ones makes every returning character's first checkpoint fail the
// fencing predicate -- correctly, since the predicate cannot tell a restarted
// counter from a stale writer. Seed it with the highest token already stored.
func NewMemoryLeases() *MemoryLeases {
	return &MemoryLeases{
		held: make(map[string]Lease),
		now:  time.Now,
	}
}

// Seed raises the token counter above tokens already issued.
//
// Called at startup with the high-water mark from the database. The Redis
// implementation does not need this: its counter lives in Redis and outlives
// the process, which is the same property being restored here.
func (m *MemoryLeases) Seed(highest int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if highest > m.nextID {
		m.nextID = highest
	}
}

// Acquire takes ownership if the character is free or its lease has lapsed.
func (m *MemoryLeases) Acquire(_ context.Context, characterID string, node NodeID) (Lease, error) {
	if characterID == "" {
		return Lease{}, errors.New("directory: character ID is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if existing, ok := m.held[characterID]; ok && now.Before(existing.ExpiresAt) {
		return Lease{}, fmt.Errorf("%w: held by %s until %s",
			ErrLeaseHeld, existing.Node, existing.ExpiresAt.Format(time.RFC3339))
	}

	// The token increases even when reclaiming an expired lease. That is the
	// entire point: the previous holder may still be alive and about to write,
	// and its now-lower token is what gets its write rejected.
	m.nextID++
	l := Lease{
		CharacterID: characterID,
		Node:        node,
		Token:       m.nextID,
		ExpiresAt:   now.Add(LeaseTTL),
	}
	m.held[characterID] = l
	return l, nil
}

// Renew extends a lease this caller still holds.
func (m *MemoryLeases) Renew(_ context.Context, l Lease) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.held[l.CharacterID]
	// Compare the token, not the node: the same node can legitimately hold a
	// second, newer lease on the same character after a reconnect, and the
	// older session must not be able to extend it.
	if !ok || current.Token != l.Token {
		return Lease{}, ErrLeaseLost
	}

	current.ExpiresAt = m.now().Add(LeaseTTL)
	m.held[l.CharacterID] = current
	return current, nil
}

// Release relinquishes ownership, if this caller still holds it.
func (m *MemoryLeases) Release(_ context.Context, l Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Only delete our own lease. Releasing unconditionally would let a
	// straggler from a previous session revoke the current owner's lease --
	// exactly the situation fencing exists to prevent.
	if current, ok := m.held[l.CharacterID]; ok && current.Token == l.Token {
		delete(m.held, l.CharacterID)
	}
	return nil
}

// Close releases nothing; the method exists to satisfy Leases.
func (m *MemoryLeases) Close() error { return nil }

// Held reports how many leases are outstanding, for metrics and tests.
func (m *MemoryLeases) Held() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	n := 0
	for _, l := range m.held {
		if now.Before(l.ExpiresAt) {
			n++
		}
	}
	return n
}

var _ Leases = (*MemoryLeases)(nil)
