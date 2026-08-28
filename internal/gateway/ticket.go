package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// TicketTTL is how long a ticket stays redeemable.
//
// Short by design: it exists only to carry an already-authenticated identity
// across the gap between an HTTP request and a WebSocket upgrade, which takes
// milliseconds. Anything longer widens the window in which a leaked ticket is
// useful for no benefit.
const TicketTTL = 30 * time.Second

// Ticket is a single-use credential for opening a game connection.
//
// Long-lived credentials must never appear in a WebSocket URL: URLs end up in
// proxy logs, browser history, and referrer headers. Instead the client
// obtains a ticket over authenticated HTTP and presents it in the first frame
// of the socket.
type Ticket struct {
	Name      string
	AccountID string
	ExpiresAt time.Time
}

// TicketStore issues and redeems tickets.
//
// Redemption is atomic and single-use, so a replayed ticket always fails. The
// in-memory implementation is fine while one gateway process handles every
// connection; with several gateways it moves to Redis, which is why redemption
// is expressed as delete-if-exists rather than read-then-delete.
type TicketStore struct {
	mu      sync.Mutex
	tickets map[string]Ticket
	now     func() time.Time
}

// NewTicketStore returns an empty store.
func NewTicketStore() *TicketStore {
	return &TicketStore{
		tickets: make(map[string]Ticket),
		now:     time.Now,
	}
}

// Issue mints a ticket for an authenticated identity.
func (s *TicketStore) Issue(accountID, name string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepLocked()
	s.tickets[id] = Ticket{
		Name:      name,
		AccountID: accountID,
		ExpiresAt: s.now().Add(TicketTTL),
	}
	return id, nil
}

// Redeem consumes a ticket, returning it and true if it was valid.
//
// A ticket is removed whether or not it had expired: an expired ticket is
// spent, not retryable, and leaving it behind would only let it be probed.
func (s *TicketStore) Redeem(id string) (Ticket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tickets[id]
	if !ok {
		return Ticket{}, false
	}
	delete(s.tickets, id)

	if s.now().After(t.ExpiresAt) {
		return Ticket{}, false
	}
	return t, true
}

// sweepLocked discards expired tickets. Called on issue, which is frequent
// enough to keep the map bounded without a background goroutine.
func (s *TicketStore) sweepLocked() {
	now := s.now()
	for id, t := range s.tickets {
		if now.After(t.ExpiresAt) {
			delete(s.tickets, id)
		}
	}
}

// Len reports how many unredeemed tickets are outstanding, for tests and
// diagnostics.
func (s *TicketStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tickets)
}
