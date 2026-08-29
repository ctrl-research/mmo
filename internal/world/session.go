package world

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/store"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/stats"
	"github.com/google/uuid"
)

// Character sessions.
//
// A session is one character being played: the lease that makes this node its
// exclusive owner, the room it is in, and the checkpoint loop that keeps its
// progress durable. Its lifetime is bounded by the lease -- if ownership is
// ever lost, the session ends rather than continuing to write over whoever
// owns the character now.

// ReconnectGrace is how long a character stays in the world after an
// unexpected disconnect.
//
// Without it, every transient network blip during a boss fight is a wipe: the
// character leaves the room, the party loses them mid-encounter, and they
// return at a spawn point. The character is frozen and invulnerable for this
// window, so the grace cannot be abused to survive a fight by pulling a cable.
const DefaultReconnectGrace = 60 * time.Second

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

	// Disconnect holds the character in the world for a grace period after a
	// dropped connection, so a transient blip is not a wipe.
	Disconnect(ctx context.Context)

	// ApplyItemAction performs an inventory request. It writes to the database
	// before the result reaches the room, so the two cannot disagree about
	// where an item is.
	ApplyItemAction(ctx context.Context, action ItemAction) error
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

	// inventory is owned here rather than in the room, because item writes
	// must be durable the moment they happen and the room must never block.
	inventory *Inventory

	// claims carries loot requests from the tick loop to this session's
	// goroutine, where the database can be reached.
	claims chan room.LootClaim

	// sink delivers inventory updates to the connected client.
	sink room.Sink

	closeOnce sync.Once
	done      chan struct{}
	log       *slog.Logger

	mu           sync.Mutex
	disconnected bool
	closedFlag   bool
	graceTimer   *time.Timer
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

	// A character still inside its reconnect window is resumed rather than
	// re-entered. Without this check the player's own dropped session would
	// hold the lease and they would be told their character is already in
	// play -- by themselves.
	if held, ok := n.held(characterID); ok {
		if held.accountID != accountID {
			// The lease belongs to a different account's session, which should
			// be impossible, but resuming would hand them someone else's
			// character.
			return nil, ErrCharacterBusy
		}
		if held.Resume(ctx, sink) {
			return held, nil
		}
		// The session finished tearing down between the lookup and the resume;
		// fall through and enter normally.
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

	inventory, err := LoadInventory(ctx, n.store, n.content, characterID)
	if err != nil {
		n.dir.Leave(ctx, instance)
		release()
		return nil, fmt.Errorf("world: loading inventory: %w", err)
	}

	s := &Session{
		node:        n,
		accountID:   accountID,
		characterID: characterID,
		name:        character.Name,
		handle:      handle,
		instance:    instance,
		lease:       lease,
		inventory:   inventory,
		sink:        sink,
		// Buffered so the tick loop never blocks handing a claim over; a
		// player cannot legitimately claim faster than this.
		claims: make(chan room.LootClaim, 16),
		done:   make(chan struct{}),
		log: n.log.With(
			"character", characterID.String(), "name", character.Name, "account", accountID.String()),
	}
	spec.Items = s

	entityID, err := handle.Join(ctx, spec)
	if err != nil {
		n.dir.Leave(ctx, instance)
		release()
		return nil, err
	}
	s.entityID = entityID

	// The stat block and the client's first inventory view, before anything
	// else observes the character.
	s.refreshStats(ctx, character.Level)

	n.hold(characterID, s)
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

		case claim := <-s.claims:
			s.handleClaim(claim)

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
	s.mu.Lock()
	s.closedFlag = true
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	s.mu.Unlock()
	s.node.forget(s.characterID)

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

// Disconnect handles a dropped connection, keeping the character in the world
// for a grace period.
//
// The lease is deliberately *not* released: the character is still in play as
// far as the world is concerned, and releasing would let another session claim
// it while this one is still holding its entity.
func (s *Session) Disconnect(ctx context.Context) {
	if s.closed() {
		return
	}

	s.handle.Freeze(ctx, s.entityID)

	// Checkpoint now rather than waiting for the grace period to end. If the
	// process dies during the window, the character is recoverable from here
	// rather than from up to a full interval earlier.
	checkpointCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := s.checkpoint(checkpointCtx)
	cancel()
	if err != nil && !errors.Is(err, store.ErrStaleWrite) {
		s.log.Error("checkpoint on disconnect failed", "err", err)
	}

	s.mu.Lock()
	s.disconnected = true
	s.graceTimer = time.AfterFunc(s.node.grace, func() {
		s.log.Info("reconnect window expired")
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.Close(closeCtx)
	})
	s.mu.Unlock()

	s.log.Info("connection dropped; holding the character", "grace", s.node.grace)
}

// Resume rebinds a reconnecting player to their held character.
//
// It reports false if the session has already been torn down, in which case
// the caller enters the world normally.
func (s *Session) Resume(ctx context.Context, sink room.Sink) bool {
	s.mu.Lock()
	if !s.disconnected || s.closedFlag {
		s.mu.Unlock()
		return false
	}
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	s.disconnected = false
	s.mu.Unlock()

	if !s.handle.Thaw(ctx, s.entityID, sink) {
		return false
	}

	s.log.Info("player reconnected within the grace window")
	return true
}

func (s *Session) closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closedFlag
}

// Close ends the session cleanly: final checkpoint, then release.
func (s *Session) Close(ctx context.Context) {
	s.mu.Lock()
	if s.closedFlag {
		s.mu.Unlock()
		return
	}
	s.closedFlag = true
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	s.mu.Unlock()

	s.closeOnce.Do(func() { close(s.done) })

	// The node stops holding this character for reconnection.
	s.node.forget(s.characterID)

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

// ClaimLoot receives a loot claim from the tick loop.
//
// Called mid-tick, so it must not block: it hands the claim to this session's
// own goroutine and returns immediately.
func (s *Session) ClaimLoot(claim room.LootClaim) {
	select {
	case s.claims <- claim:
	default:
		// The queue is full, which means persistence is badly backed up.
		// Refusing returns the drop to the ground rather than losing it.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		s.handle.ResolveLoot(ctx, claim.Player, claim.DropID, false, "the server is busy; try again")
		cancel()
	}
}

// handleClaim persists a claimed item and confirms or returns it.
func (s *Session) handleClaim(claim room.LootClaim) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := s.inventory.Grant(ctx, claim.Instance, claim.Tick)
	switch {
	case errors.Is(err, ErrInventoryFull):
		// Returned to the ground, so the player can make room and come back
		// for it rather than losing it.
		s.handle.ResolveLoot(ctx, claim.Player, claim.DropID, false, "your inventory is full")
		return
	case err != nil:
		s.log.Error("granting loot", "err", err)
		s.handle.ResolveLoot(ctx, claim.Player, claim.DropID, false, "could not pick that up")
		return
	}

	s.handle.ResolveLoot(ctx, claim.Player, claim.DropID, true, "")
	s.pushInventory(ctx)
}

// refreshStats recomputes the stat block and pushes it to the room and client.
//
// Rebuilt from scratch on every change: removing a modifier from a running
// product is lossy, and an incremental path that drifts produces stats that
// depend on the order things were equipped.
func (s *Session) refreshStats(ctx context.Context, level int) {
	block := s.inventory.StatBlock(level)

	maxLife := block.IntClampedNonNegative(stats.MaxLife)
	if maxLife < 1 {
		maxLife = 1
	}

	s.handle.SetStats(ctx, s.entityID, block, uint32(maxLife))
	s.pushInventoryWithStats(ctx, block)
}

// ApplyItemAction performs a player's inventory request.
//
// Every action goes to the database first and the in-memory view is rebuilt
// from the result, because the database is the authority on where an item is
// -- and a divergence between the two is how an item comes to appear twice.
func (s *Session) ApplyItemAction(ctx context.Context, action ItemAction) error {
	level := s.characterLevel(ctx)

	var err error
	switch action.Kind {
	case ItemMove:
		err = s.inventory.Move(ctx, action.ItemID, action.Slot, 0)
	case ItemEquip:
		err = s.inventory.Equip(ctx, action.ItemID, level, 0)
	case ItemUnequip:
		err = s.inventory.Unequip(ctx, action.EquipSlot, 0)
	case ItemDestroy:
		err = s.inventory.Destroy(ctx, action.ItemID, 0)
	default:
		return fmt.Errorf("world: unknown item action")
	}
	if err != nil {
		return err
	}

	s.refreshStats(ctx, level)
	return nil
}

// characterLevel reads the level the room currently has for this character.
func (s *Session) characterLevel(ctx context.Context) int {
	if snap, ok := s.handle.Capture(ctx, s.entityID); ok && snap.Progress.Level > 0 {
		return snap.Progress.Level
	}
	return 1
}

// ItemActionKind is what a player wants done with an item.
type ItemActionKind uint8

const (
	ItemMove ItemActionKind = iota + 1
	ItemEquip
	ItemUnequip
	ItemDestroy
)

// ItemAction is a request to change the inventory.
type ItemAction struct {
	Kind      ItemActionKind
	ItemID    uuid.UUID
	Slot      int
	EquipSlot content.EquipSlot
}
