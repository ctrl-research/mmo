package directory

import (
	"context"
	"strings"
	"sync"
)

// Who is online, and which node is holding them.
//
// This is the third seam, and it exists for the same reason as the other two:
// a whisper has to reach a character whose session is on some other node, and
// the only way to find it without a shared pointer is to look it up. Memory
// answers from one process; a Redis implementation answers across many, and
// nothing above this package knows which it has.
//
// Presence is deliberately ephemeral. Losing it costs everyone their friends
// list for a moment and nothing else -- the characters are still in their
// rooms, still leased, still checkpointing. That is what makes Redis the right
// home for it later and Postgres the wrong one.

// Online is one character currently in play.
type Online struct {
	CharacterID string

	// Name is the character's display name. It is what a player types to
	// whisper somebody, so it is the lookup key as well as a label.
	Name string

	// Node is the world node holding the session, and therefore the socket.
	Node NodeID

	// MapID is where they are, for a friends list worth looking at.
	MapID string

	// Away marks a character inside their reconnect window: still in the
	// world, still in their party, but with nobody reading their messages. A
	// whisper to an away character should say so rather than vanishing.
	Away bool
}

// Presence tracks which characters are in play and where.
//
// Implementations must be safe for concurrent use, and must treat names
// case-insensitively for lookup while preserving the case for display: a
// player who types "alice" means the character called "Alice".
//
// The read methods return errors for the same reason the Directory's do: an
// in-memory table cannot fail one, a network-backed table can, and "nobody by
// that name is online" is a wrong answer rather than a degraded one -- a whisper
// refused because Redis was briefly unreachable should say so.
type Presence interface {
	// Announce records a character as online, replacing any earlier entry.
	// Called again on a room transfer, since the node may have changed.
	Announce(ctx context.Context, who Online) error

	// Forget removes a character, on logout or when a reconnect window ends.
	Forget(ctx context.Context, characterID string) error

	// ForgetNode removes every character attributed to a node.
	//
	// Called by a node as it starts, because a node that has just started is
	// holding nobody: any presence still claiming otherwise is left over from
	// the process that died, and would route whispers at a socket that is gone.
	ForgetNode(ctx context.Context, node NodeID) error

	// ByName finds an online character by display name, case-insensitively.
	ByName(ctx context.Context, name string) (Online, bool, error)

	// ByID finds an online character by ID.
	ByID(ctx context.Context, characterID string) (Online, bool, error)

	// List returns everyone online, ordered by name so a roster is stable.
	List(ctx context.Context) ([]Online, error)

	Close() error
}

// MemoryPresence is a Presence held in one process.
type MemoryPresence struct {
	mu     sync.RWMutex
	byID   map[string]Online
	byName map[string]string // normalised name -> character ID
}

// NewMemoryPresence returns an empty presence table.
func NewMemoryPresence() *MemoryPresence {
	return &MemoryPresence{
		byID:   make(map[string]Online),
		byName: make(map[string]string),
	}
}

// NormaliseName is the lookup form of a character name.
//
// Lower-cased only: a player typing a friend's name should not have to match
// its capitalisation, and character names are already unique
// case-insensitively at creation.
func NormaliseName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Announce records a character as online.
func (m *MemoryPresence) Announce(_ context.Context, who Online) error {
	m.mu.Lock()

	// A rename or a re-announce under a different name would otherwise leave
	// the old name pointing at this character forever.
	if old, ok := m.byID[who.CharacterID]; ok && NormaliseName(old.Name) != NormaliseName(who.Name) {
		m.releaseNameLocked(who.CharacterID, old.Name)
	}

	m.byID[who.CharacterID] = who
	m.byName[NormaliseName(who.Name)] = who.CharacterID
	m.mu.Unlock()
	return nil
}

// Forget removes a character.
func (m *MemoryPresence) Forget(_ context.Context, characterID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if who, ok := m.byID[characterID]; ok {
		delete(m.byID, characterID)
		m.releaseNameLocked(characterID, who.Name)
	}
	return nil
}

// releaseNameLocked drops a name index only if it still points at this
// character.
//
// Unconditionally deleting it revokes a name another character has taken over,
// which happens whenever two announcements and a rename interleave across nodes.
// Character names are unique, so the window is narrow -- but "narrow" is not
// "impossible", and the symptom is a player unreachable by name until they
// re-announce.
func (m *MemoryPresence) releaseNameLocked(characterID, name string) {
	normalised := NormaliseName(name)
	if m.byName[normalised] == characterID {
		delete(m.byName, normalised)
	}
}

// ByName finds an online character by display name.
func (m *MemoryPresence) ByName(_ context.Context, name string) (Online, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	id, ok := m.byName[NormaliseName(name)]
	if !ok {
		return Online{}, false, nil
	}
	who, ok := m.byID[id]
	return who, ok, nil
}

// ByID finds an online character by ID.
func (m *MemoryPresence) ByID(_ context.Context, characterID string) (Online, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	who, ok := m.byID[characterID]
	return who, ok, nil
}

// List returns everyone online, ordered by name.
func (m *MemoryPresence) List(_ context.Context) ([]Online, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Online, 0, len(m.byID))
	for _, who := range m.byID {
		out = append(out, who)
	}
	// Map iteration order is random; a roster that reshuffles every refresh
	// is unusable.
	sortOnline(out)
	return out, nil
}

// ForgetNode removes every character attributed to a node.
func (m *MemoryPresence) ForgetNode(_ context.Context, node NodeID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, who := range m.byID {
		if who.Node != node {
			continue
		}
		delete(m.byID, id)
		m.releaseNameLocked(id, who.Name)
	}
	return nil
}

// Close releases nothing; the method exists to satisfy Presence.
func (m *MemoryPresence) Close() error { return nil }

func sortOnline(list []Online) {
	// Insertion sort: rosters are small, and this avoids pulling sort into a
	// package that is otherwise dependency-free.
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && NormaliseName(list[j].Name) < NormaliseName(list[j-1].Name); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

var _ Presence = (*MemoryPresence)(nil)
