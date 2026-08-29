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
type Presence interface {
	// Announce records a character as online, replacing any earlier entry.
	// Called again on a room transfer, since the node may have changed.
	Announce(ctx context.Context, who Online) error

	// Forget removes a character, on logout or when a reconnect window ends.
	Forget(ctx context.Context, characterID string) error

	// ByName finds an online character by display name, case-insensitively.
	ByName(ctx context.Context, name string) (Online, bool)

	// ByID finds an online character by ID.
	ByID(ctx context.Context, characterID string) (Online, bool)

	// List returns everyone online, ordered by name so a roster is stable.
	List(ctx context.Context) []Online

	Close() error
}

// MemoryPresence is a Presence held in one process.
type MemoryPresence struct {
	mu      sync.RWMutex
	byID    map[string]Online
	byName  map[string]string // normalised name -> character ID
	watcher func(Online, bool)
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
	if old, ok := m.byID[who.CharacterID]; ok && old.Name != who.Name {
		delete(m.byName, NormaliseName(old.Name))
	}

	m.byID[who.CharacterID] = who
	m.byName[NormaliseName(who.Name)] = who.CharacterID
	watcher := m.watcher
	m.mu.Unlock()

	if watcher != nil {
		watcher(who, true)
	}
	return nil
}

// Forget removes a character.
func (m *MemoryPresence) Forget(_ context.Context, characterID string) error {
	m.mu.Lock()
	who, ok := m.byID[characterID]
	if ok {
		delete(m.byID, characterID)
		delete(m.byName, NormaliseName(who.Name))
	}
	watcher := m.watcher
	m.mu.Unlock()

	if ok && watcher != nil {
		watcher(who, false)
	}
	return nil
}

// ByName finds an online character by display name.
func (m *MemoryPresence) ByName(_ context.Context, name string) (Online, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	id, ok := m.byName[NormaliseName(name)]
	if !ok {
		return Online{}, false
	}
	who, ok := m.byID[id]
	return who, ok
}

// ByID finds an online character by ID.
func (m *MemoryPresence) ByID(_ context.Context, characterID string) (Online, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	who, ok := m.byID[characterID]
	return who, ok
}

// List returns everyone online, ordered by name.
func (m *MemoryPresence) List(_ context.Context) []Online {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Online, 0, len(m.byID))
	for _, who := range m.byID {
		out = append(out, who)
	}
	// Map iteration order is random; a roster that reshuffles every refresh
	// is unusable.
	sortOnline(out)
	return out
}

// Watch registers a callback for someone coming online or going offline.
//
// One watcher, not a list: the only subscriber is the node that owns this
// presence table, and it fans out from there over the bus. A second in-process
// watcher would be a component reaching around the bus to reach another.
func (m *MemoryPresence) Watch(fn func(who Online, online bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watcher = fn
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
