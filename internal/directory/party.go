package directory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Party membership, shared across nodes.
//
// A party spans rooms and nodes by definition -- that is most of what makes it
// worth having -- so its state cannot live in a room, and it cannot live on
// one node's heap. It lives here, behind the same kind of interface as the
// room directory and the leases, so that Memory answers from one process today
// and Redis answers across many later.
//
// Parties are ephemeral on purpose. Losing them costs everyone a regroup and
// nothing more; nobody's items, progress, or position depend on one existing.
// That is what makes Redis the right home for them and Postgres the wrong one.

// Party errors.
var (
	ErrNoParty        = errors.New("directory: no such party")
	ErrPartyFull      = errors.New("directory: party is full")
	ErrAlreadyInParty = errors.New("directory: already in a party")
	ErrNotInParty     = errors.New("directory: not in that party")
	ErrNotLeader      = errors.New("directory: only the leader may do that")
	ErrNoInvite       = errors.New("directory: no invitation")
)

// PartyID identifies one party for its lifetime.
type PartyID string

// Loot rules, as they travel.
const (
	LootFreeForAll = "free-for-all"
	LootRoundRobin = "round-robin"
)

// ValidLootRule reports whether a rule is one the server knows.
func ValidLootRule(rule string) bool {
	return rule == LootFreeForAll || rule == LootRoundRobin
}

// Member is one character in a party.
type Member struct {
	CharacterID string
	Name        string
}

// Party is a group of characters.
type Party struct {
	ID PartyID

	// Leader is the character who may invite, kick, and disband. It is the
	// first member, and moves on leaving rather than the party disbanding.
	Leader string

	// Members are in join order, so the member frames a player sees do not
	// reshuffle when somebody's ping changes.
	Members []Member

	// Loot is how drops are assigned: "free-for-all" or "round-robin". A
	// string rather than an enum because it crosses the bus and ends up in a
	// Redis hash, and a number that means something different after a deploy
	// is a bug nobody sees until loot goes to the wrong person.
	Loot string
}

// Has reports whether a character is in the party.
func (p Party) Has(characterID string) bool {
	for _, m := range p.Members {
		if m.CharacterID == characterID {
			return true
		}
	}
	return false
}

// Names returns the member names, for a message worth reading.
func (p Party) Names() []string {
	out := make([]string, 0, len(p.Members))
	for _, m := range p.Members {
		out = append(out, m.Name)
	}
	return out
}

// Parties owns party membership.
//
// Every method is atomic with respect to the others: two simultaneous joins
// must not both take the last slot, and a leader leaving while somebody is
// invited must not leave the invitation pointing at a party that no longer
// exists.
type Parties interface {
	// Create starts a party with one member, who becomes its leader.
	Create(ctx context.Context, founder Member) (Party, error)

	// Invite records an invitation from a party member to a character,
	// creating a party for the inviter if they are not already in one.
	//
	// The inviter arrives as a Member rather than an ID because inviting is
	// how most parties come into existence, and a party founded from an ID
	// alone has a leader nobody can name -- which shows up later as being
	// unable to kick or promote them.
	Invite(ctx context.Context, from Member, to string) (Party, error)

	// Accept turns an invitation into membership.
	Accept(ctx context.Context, who Member) (Party, error)

	// Decline discards an invitation.
	Decline(ctx context.Context, characterID string) error

	// Leave removes a character. The leader leaving passes leadership to the
	// next member; the last member leaving disbands the party.
	Leave(ctx context.Context, characterID string) (Party, error)

	// Kick removes another member. Only the leader may.
	Kick(ctx context.Context, leader, target string) (Party, error)

	// Promote transfers leadership. Only the leader may.
	Promote(ctx context.Context, leader, target string) (Party, error)

	// SetLoot changes how drops are assigned. Only the leader may.
	SetLoot(ctx context.Context, leader, rule string) (Party, error)

	// Of returns the party a character is in.
	//
	// Reports an error for the reason the directory's Lookup does: an
	// unreachable Redis answering "not in a party" is not a degraded answer
	// but a wrong one, and a caller acts on it by dropping somebody's party
	// chat on the floor.
	Of(ctx context.Context, characterID string) (Party, bool, error)

	// Rename updates a member's display name, for a character that was renamed
	// or whose name was not known when they joined.
	Rename(ctx context.Context, characterID, name string) error

	Close() error
}

// InviteTTL is how long an invitation stands.
//
// Short: an invitation is a question asked in the moment, and one that lingers
// for an hour gets accepted long after everyone has moved on.
const InviteTTL = 60 * time.Second

// MemoryParties is a Parties held in one process.
type MemoryParties struct {
	maxSize int

	// inviteTTL is per-instance so tests can use a short one and watch a real
	// invitation expire. Both implementations take it the same way.
	inviteTTL time.Duration

	mu       sync.Mutex
	parties  map[PartyID]*Party
	memberOf map[string]PartyID
	invites  map[string]invitation
	now      func() time.Time
}

type invitation struct {
	party   PartyID
	from    string
	expires time.Time
}

// NewMemoryParties returns an empty party table.
func NewMemoryParties(maxSize int) *MemoryParties {
	if maxSize <= 0 {
		maxSize = 6
	}
	return &MemoryParties{
		maxSize:   maxSize,
		inviteTTL: InviteTTL,
		parties:   make(map[PartyID]*Party),
		memberOf:  make(map[string]PartyID),
		invites:   make(map[string]invitation),
		now:       time.Now,
	}
}

// Create starts a party with one member.
func (m *MemoryParties) Create(_ context.Context, founder Member) (Party, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.memberOf[founder.CharacterID]; ok {
		return Party{}, ErrAlreadyInParty
	}
	return *m.createLocked(founder), nil
}

func (m *MemoryParties) createLocked(founder Member) *Party {
	p := &Party{
		ID:      PartyID(uuid.NewString()),
		Leader:  founder.CharacterID,
		Members: []Member{founder},
		// The default a group of friends expects, and the one that needs no
		// rules to explain.
		Loot: LootFreeForAll,
	}
	m.parties[p.ID] = p
	m.memberOf[founder.CharacterID] = p.ID
	return p
}

// Invite records an invitation, creating a party for the inviter if they are
// not already in one.
//
// Creating on invite rather than requiring an explicit "make a party" step:
// nobody wants a party of one, and the only reason to make one is to invite
// somebody to it.
func (m *MemoryParties) Invite(_ context.Context, from Member, to string) (Party, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if from.CharacterID == to {
		return Party{}, fmt.Errorf("directory: cannot invite yourself")
	}
	if _, ok := m.memberOf[to]; ok {
		return Party{}, ErrAlreadyInParty
	}

	var p *Party
	if id, ok := m.memberOf[from.CharacterID]; ok {
		p = m.parties[id]
		if p == nil {
			return Party{}, ErrNoParty
		}
		// Anyone may invite, but only the leader may kick. Inviting is how a
		// party grows and gatekeeping it through one person makes grouping
		// tedious for no safety gain.
		if len(p.Members) >= m.maxSize {
			return Party{}, ErrPartyFull
		}
	} else {
		p = m.createLocked(from)
	}

	m.invites[to] = invitation{
		party: p.ID, from: from.CharacterID, expires: m.now().Add(m.inviteTTL),
	}
	return *p, nil
}

// Accept turns an invitation into membership.
func (m *MemoryParties) Accept(_ context.Context, who Member) (Party, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	inv, ok := m.invites[who.CharacterID]
	if !ok || m.now().After(inv.expires) {
		delete(m.invites, who.CharacterID)
		return Party{}, ErrNoInvite
	}
	delete(m.invites, who.CharacterID)

	if _, ok := m.memberOf[who.CharacterID]; ok {
		return Party{}, ErrAlreadyInParty
	}

	p, ok := m.parties[inv.party]
	if !ok {
		// The party disbanded between the invitation and the answer.
		return Party{}, ErrNoParty
	}
	if len(p.Members) >= m.maxSize {
		return Party{}, ErrPartyFull
	}

	p.Members = append(p.Members, who)
	m.memberOf[who.CharacterID] = p.ID
	return *p, nil
}

// Decline discards an invitation.
func (m *MemoryParties) Decline(_ context.Context, characterID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.invites[characterID]; !ok {
		return ErrNoInvite
	}
	delete(m.invites, characterID)
	return nil
}

// Leave removes a character from their party.
//
// The returned party is the one they left, after the removal, so a caller can
// tell the remaining members what happened. A disbanded party comes back with
// no members.
func (m *MemoryParties) Leave(_ context.Context, characterID string) (Party, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.removeLocked(characterID)
}

func (m *MemoryParties) removeLocked(characterID string) (Party, error) {
	id, ok := m.memberOf[characterID]
	if !ok {
		return Party{}, ErrNotInParty
	}
	p := m.parties[id]
	if p == nil {
		delete(m.memberOf, characterID)
		return Party{}, ErrNoParty
	}

	delete(m.memberOf, characterID)
	for i, member := range p.Members {
		if member.CharacterID == characterID {
			p.Members = append(p.Members[:i:i], p.Members[i+1:]...)
			break
		}
	}

	// A party of one is not a party. Disbanding rather than leaving somebody
	// in a group by themselves, wondering why their loot rules changed.
	if len(p.Members) <= 1 {
		for _, member := range p.Members {
			delete(m.memberOf, member.CharacterID)
		}
		left := *p
		left.Members = nil
		delete(m.parties, id)
		return left, nil
	}

	// Leadership moves rather than the party dissolving, so one person
	// logging out does not scatter five others.
	if p.Leader == characterID {
		p.Leader = p.Members[0].CharacterID
	}
	return *p, nil
}

// Kick removes another member.
func (m *MemoryParties) Kick(_ context.Context, leader, target string) (Party, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, ok := m.memberOf[leader]
	if !ok {
		return Party{}, ErrNotInParty
	}
	p := m.parties[id]
	if p == nil {
		return Party{}, ErrNoParty
	}
	if p.Leader != leader {
		return Party{}, ErrNotLeader
	}
	if target == leader || !p.Has(target) {
		return Party{}, ErrNotInParty
	}
	return m.removeLocked(target)
}

// Promote transfers leadership.
func (m *MemoryParties) Promote(_ context.Context, leader, target string) (Party, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, ok := m.memberOf[leader]
	if !ok {
		return Party{}, ErrNotInParty
	}
	p := m.parties[id]
	if p == nil {
		return Party{}, ErrNoParty
	}
	if p.Leader != leader {
		return Party{}, ErrNotLeader
	}
	if !p.Has(target) {
		return Party{}, ErrNotInParty
	}

	p.Leader = target
	return *p, nil
}

// SetLoot changes how drops are assigned.
func (m *MemoryParties) SetLoot(_ context.Context, leader, rule string) (Party, error) {
	if !ValidLootRule(rule) {
		return Party{}, fmt.Errorf("directory: unknown loot rule %q", rule)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	id, ok := m.memberOf[leader]
	if !ok {
		return Party{}, ErrNotInParty
	}
	p := m.parties[id]
	if p == nil {
		return Party{}, ErrNoParty
	}
	if p.Leader != leader {
		return Party{}, ErrNotLeader
	}

	p.Loot = rule
	return *p, nil
}

// Of returns the party a character is in.
func (m *MemoryParties) Of(_ context.Context, characterID string) (Party, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, ok := m.memberOf[characterID]
	if !ok {
		return Party{}, false, nil
	}
	p, ok := m.parties[id]
	if !ok {
		return Party{}, false, nil
	}
	return *p, true, nil
}

// Rename updates a member's display name.
func (m *MemoryParties) Rename(_ context.Context, characterID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, ok := m.memberOf[characterID]
	if !ok {
		return ErrNotInParty
	}
	p := m.parties[id]
	if p == nil {
		return ErrNoParty
	}
	for i := range p.Members {
		if p.Members[i].CharacterID == characterID {
			p.Members[i].Name = name
			return nil
		}
	}
	return ErrNotInParty
}

// Close releases nothing; the method exists to satisfy Parties.
func (m *MemoryParties) Close() error { return nil }

var _ Parties = (*MemoryParties)(nil)
