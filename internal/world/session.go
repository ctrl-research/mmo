package world

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/store"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/google/uuid"
)

// Character sessions.
//
// A session is one character being played: the lease that makes this node its
// exclusive owner, the room it is in, and the checkpoint loop that keeps its
// progress durable. Its lifetime is bounded by the lease -- if ownership is
// ever lost, the session ends rather than continuing to write over whoever
// owns the character now.

// CheckpointInterval is how often live state is written back.
//
// Position is worthless a second later, so writing it at tick rate would melt
// the database for nothing. The window this leaves is acceptable for position
// and combat state; anything a player would file a ticket over losing is
// written through immediately instead (docs/data-model.md).
const CheckpointInterval = 30 * time.Second

// Session errors.
var (
	// ErrCharacterBusy means the character is already being played, here or on
	// another node. It is the visible face of the single-writer invariant.
	ErrCharacterBusy = errors.New("world: character is already in play")
)

// PlayerSession is one character in play, as seen from outside this package.
//
// The gateway holds one per connection and closes it when the socket ends; it
// deliberately knows nothing about leases, checkpoints, or which node hosts
// which room.
type PlayerSession interface {
	Handle() room.Handle
	EntityID() room.EntityID
	Name() string

	// OnOwnershipLost registers a callback for losing the character's lease,
	// so the connection can be closed rather than left playing a character
	// this node no longer owns.
	OnOwnershipLost(fn func(reason string))

	// Close checkpoints and releases the character.
	Close(ctx context.Context)
}

// Session is one character in the world.
type Session struct {
	node *Node

	accountID   uuid.UUID
	characterID uuid.UUID
	name        string

	handle   room.Handle
	entityID room.EntityID
	instance directory.InstanceID

	lease directory.Lease

	// onLost is called when ownership is lost, so the gateway can close the
	// connection rather than leave a client playing a character it no longer
	// owns.
	onLost func(reason string)

	closeOnce sync.Once
	done      chan struct{}
	log       *slog.Logger
}

// Handle returns the room this session is in.
func (s *Session) Handle() room.Handle { return s.handle }

// EntityID returns the entity the character was given.
func (s *Session) EntityID() room.EntityID { return s.entityID }

// Name returns the character's name.
func (s *Session) Name() string { return s.name }

// OnOwnershipLost registers a callback for losing the lease.
func (s *Session) OnOwnershipLost(fn func(reason string)) { s.onLost = fn }

// Enter loads a character, takes ownership of it, and places it in a room.
//
// The order matters. The lease is taken *before* the character is read, so two
// nodes cannot both load the same character and then both believe they own it
// -- which is how one sword becomes two.
func (n *Node) Enter(ctx context.Context, accountID, characterID uuid.UUID, sink room.Sink) (PlayerSession, error) {
	if n.store == nil || n.leases == nil {
		return nil, errors.New("world: persistence is not configured")
	}

	lease, err := n.leases.Acquire(ctx, characterID.String(), directory.NodeID(n.nodeID))
	if err != nil {
		if errors.Is(err, directory.ErrLeaseHeld) {
			return nil, fmt.Errorf("%w: %v", ErrCharacterBusy, err)
		}
		return nil, fmt.Errorf("world: acquiring character lease: %w", err)
	}

	// From here on, any failure must release the lease, or a character is
	// stranded until the lease expires.
	release := func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		n.leases.Release(releaseCtx, lease)
	}

	character, err := n.store.LoadCharacter(ctx, accountID, characterID)
	if err != nil {
		release()
		return nil, fmt.Errorf("world: loading character: %w", err)
	}

	mapID := character.MapID
	if _, ok := n.content.Maps[mapID]; !ok {
		// The character's map may have been renamed or removed since they last
		// played. Falling back beats refusing to let them in.
		n.log.Warn("character's map no longer exists; using the default",
			"character", characterID, "map", mapID, "default", n.defaultMap)
		mapID = n.defaultMap
	}

	handle, instance, err := n.placeIn(ctx, mapID)
	if err != nil {
		release()
		return nil, err
	}

	state := room.UnmarshalState(character.State)
	spec := room.JoinSpec{
		CharacterID: characterID.String(),
		Name:        character.Name,
		Progress: room.Progress{
			Level: character.Level,
			Exp:   character.Exp,
			Gold:  character.Gold,
			MapID: mapID,
		},
		State: state,
		// A character with no saved position has never played, and should
		// start at the map's spawn point rather than at the origin.
		Fresh: state.MaxHP == 0,
		Sink:  sink,
	}

	entityID, err := handle.Join(ctx, spec)
	if err != nil {
		n.dir.Leave(ctx, instance)
		release()
		return nil, err
	}

	s := &Session{
		node:        n,
		accountID:   accountID,
		characterID: characterID,
		name:        character.Name,
		handle:      handle,
		entityID:    entityID,
		instance:    instance,
		lease:       lease,
		done:        make(chan struct{}),
		log: n.log.With(
			"character", characterID.String(), "name", character.Name, "account", accountID.String()),
	}

	go s.maintain()

	s.log.Info("character entered the world", "map", mapID, "lease", lease.Token)
	return s, nil
}

// maintain renews the lease and checkpoints on an interval.
//
// Both run on one goroutine so a checkpoint can never overlap the loss of the
// lease that authorises it.
func (s *Session) maintain() {
	renew := time.NewTicker(directory.LeaseRenewInterval)
	defer renew.Stop()

	checkpoint := time.NewTicker(CheckpointInterval)
	defer checkpoint.Stop()

	for {
		select {
		case <-s.done:
			return

		case <-renew.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			lease, err := s.node.leases.Renew(ctx, s.lease)
			cancel()

			if err != nil {
				// Losing a renewal is never routine: ownership has moved, and
				// continuing would mean writing over the new owner's work.
				s.log.Warn("character ownership lost", "err", err)
				s.loseOwnership("your character was claimed elsewhere")
				return
			}
			s.lease = lease

		case <-checkpoint.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := s.checkpoint(ctx)
			cancel()

			if errors.Is(err, store.ErrStaleWrite) {
				// The fencing check rejected the write, which means the lease
				// moved without the renewal noticing yet. Same response.
				s.log.Warn("checkpoint fenced; ownership was lost")
				s.loseOwnership("your character was claimed elsewhere")
				return
			}
			if err != nil {
				// A transient database error should not end a session; the
				// next checkpoint will try again.
				s.log.Error("checkpoint failed", "err", err)
			}
		}
	}
}

// checkpoint captures live state and writes it back, fenced by the lease.
func (s *Session) checkpoint(ctx context.Context) error {
	snap, ok := s.handle.Capture(ctx, s.entityID)
	if !ok {
		// The player has already left the room; nothing to write.
		return nil
	}

	stateJSON, err := room.MarshalState(snap.State)
	if err != nil {
		return fmt.Errorf("world: encoding character state: %w", err)
	}

	return s.node.store.Checkpoint(ctx, store.Character{
		ID:    s.characterID,
		Level: snap.Progress.Level,
		Exp:   snap.Progress.Exp,
		Gold:  snap.Progress.Gold,
		MapID: snap.Progress.MapID,
		State: stateJSON,
	}, s.lease.Token)
}

// loseOwnership ends the session because the lease moved.
func (s *Session) loseOwnership(reason string) {
	// Deliberately no final checkpoint: this node no longer owns the
	// character, and its write would be rejected by the fencing predicate
	// anyway. Attempting one would only risk overwriting a newer owner if the
	// predicate were ever wrong.
	s.closeOnce.Do(func() { close(s.done) })

	if s.onLost != nil {
		s.onLost(reason)
	}
	s.detach()
}

// Close ends the session cleanly: final checkpoint, then release.
func (s *Session) Close(ctx context.Context) {
	var alreadyClosed bool
	s.closeOnce.Do(func() { close(s.done) })
	if alreadyClosed {
		return
	}

	// The final checkpoint is what makes logging out lossless, rather than
	// discarding up to a whole interval of progress.
	if err := s.checkpoint(ctx); err != nil && !errors.Is(err, store.ErrStaleWrite) {
		s.log.Error("final checkpoint failed", "err", err)
	}

	s.detach()

	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.node.leases.Release(releaseCtx, s.lease); err != nil {
		s.log.Error("releasing lease", "err", err)
	}

	s.log.Info("character left the world")
}

// detach removes the character from its room and frees its directory slot.
func (s *Session) detach() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.handle.Leave(ctx, s.entityID)
	s.node.dir.Leave(ctx, s.instance)
}

var _ PlayerSession = (*Session)(nil)
