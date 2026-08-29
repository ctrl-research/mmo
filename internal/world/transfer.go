package world

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ctrl-research/mmo/internal/bus"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/store"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// Moving a character between rooms.
//
// This is the protocol the whole scaling design rests on. It runs over the bus
// whether the destination is on this node or another one, and there is
// deliberately no local fast path: the shortcut would work today and mean the
// distributed path is never exercised until the day it has to (AGENTS.md
// invariant 2, docs/architecture.md).
//
// The ordering matters at every step:
//
//   1. Freeze the character, so no further input is applied to a body that is
//      about to be serialised.
//   2. Capture and checkpoint, so the state exists durably before it moves.
//   3. Place in the destination through the directory, which decides which
//      instance and which node.
//   4. Ask that node to take the character, over the bus, and wait.
//   5. Only on success, leave the source room and release its slot.
//
// A failure at any point before step 5 leaves the character exactly where it
// was, unfrozen and playable. That is why the source is not torn down first.

// TransferTimeout bounds how long a transfer may take.
//
// Generous, because it covers a database write and a round trip. If it
// expires, the character stays where it is rather than being left in limbo.
const TransferTimeout = 15 * time.Second

// Transfer errors.
var (
	ErrTransferRefused = errors.New("world: destination refused the transfer")
	ErrUnknownMap      = errors.New("world: unknown map")
)

// transferSubject is where a node listens for characters being handed to it.
func transferSubject(nodeID string) string {
	return "world.node." + sanitiseSubject(nodeID) + ".transfer"
}

// sanitiseSubject makes a node id safe as a subject token.
//
// Subjects are dot-separated, so a node id containing a dot would silently
// change the routing rather than fail.
func sanitiseSubject(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "unnamed"
	}
	return string(out)
}

// EnterPortal begins a transfer. Called from the room's tick loop, so it must
// not block.
func (s *Session) EnterPortal(req room.PortalRequest) {
	select {
	case s.portals <- req:
	default:
		// The queue is full, which means transfers are badly backed up.
		// Refusing leaves the character where it is, which is safe.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		handle, entityID := s.Where()
		handle.AbortTransfer(ctx, entityID, "the server is busy; try again")
		cancel()
	}
}

// DiscoverWaypoint records a waypoint unlock. Also called mid-tick.
func (s *Session) DiscoverWaypoint(_ room.EntityID, characterID, waypointID string) {
	select {
	case s.waypoints <- waypointID:
	default:
		// Losing an unlock is a small cost; blocking the tick is not.
		s.log.Warn("dropped a waypoint unlock", "waypoint", waypointID)
	}
	_ = characterID
}

// handlePortal runs the transfer protocol for one portal entry.
func (s *Session) handlePortal(req room.PortalRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), TransferTimeout)
	defer cancel()

	handle, entityID := s.Where()

	target, ok := s.node.content.Maps[req.Portal.TargetMap]
	if !ok {
		handle.AbortTransfer(ctx, entityID, transferMessage(ErrUnknownMap))
		return
	}

	// A dungeon's own rules, checked at the door. Both answers live outside
	// the tick -- the level on the session, the lockout in the database --
	// which is why the room lets the character walk into the portal and this
	// is where they are turned back.
	if d := s.node.content.DungeonForMap(target.ID); d != nil {
		if err := s.checkDungeonEntry(ctx, d); err != nil {
			s.log.Info("dungeon entry refused", "dungeon", d.ID, "err", err)
			handle.AbortTransfer(ctx, entityID, err.Error())
			return
		}
	}

	err := s.transfer(ctx, target, arrival{spawnPoint: req.Portal.TargetSpawn},
		func(ctx context.Context) (directory.Instance, error) {
			return s.node.dir.Join(ctx,
				roomKey(target, s.layerKey()), target.Capacity)
		})
	if err != nil {
		s.log.Warn("transfer failed", "target", target.ID, "err", err)

		// The character is still in the source room, held only by the
		// transferring flag. Releasing it lets them walk away and try again.
		handle.AbortTransfer(ctx, entityID, transferMessage(err))
	}
}

// arrival says where in the destination map the character appears.
//
// All three cases are the same transfer with a different landing point, which
// is why they share a protocol rather than each getting one.
type arrival struct {
	// spawnPoint names an entrance, for a portal.
	spawnPoint string

	// waypoint names a fast-travel destination. Resolved by the destination
	// from its own content rather than sent as coordinates: the client, and
	// for that matter the source node, has no business choosing where in a map
	// a character materialises.
	waypoint string

	// keepPosition leaves the character exactly where they were standing,
	// which is what a channel switch means: the same map, the same spot, a
	// different instance.
	keepPosition bool
}

// transfer moves the character into another room instance.
//
// pick chooses the destination instance: a portal asks the directory to place
// the character, and a channel switch names one outright. Everything after
// that is identical, which is the point -- there is one handoff protocol, and
// every way of moving through the world goes down it.
func (s *Session) transfer(ctx context.Context, target *content.Map, at arrival,
	pick func(context.Context) (directory.Instance, error),
) error {
	// 1. Freeze, so no further input is applied to a body about to be
	//    serialised, and 2. capture what to send.
	sourceHandle, sourceEntity := s.Where()
	sourceHandle.Freeze(ctx, sourceEntity)

	snap, ok := sourceHandle.Capture(ctx, sourceEntity)
	if !ok {
		return errors.New("world: the character is no longer in its room")
	}

	stateJSON, err := room.MarshalState(snap.State)
	if err != nil {
		return fmt.Errorf("world: encoding character state: %w", err)
	}

	// 3. Checkpoint before moving. If everything after this fails, the
	//    character is recoverable from here rather than from the last periodic
	//    write.
	if err := s.node.store.Checkpoint(ctx, store.Character{
		ID:    s.characterID,
		Level: snap.Progress.Level,
		Exp:   snap.Progress.Exp,
		Gold:  snap.Progress.Gold,
		MapID: target.ID,
		State: stateJSON,
	}, s.lease.Token); err != nil {
		return fmt.Errorf("world: checkpointing before transfer: %w", err)
	}

	// 4. Place in the destination.
	inst, err := pick(ctx)
	if err != nil {
		return fmt.Errorf("world: placing at destination: %w", err)
	}

	// 5. Hand the character over, through the bus. No local shortcut, even
	//    when the destination node is this one.
	reply := &mmov1.TransferReply{}
	err = s.node.bus.Request(ctx, transferSubject(string(inst.Node)), &mmov1.TransferRequest{
		CharacterId: s.characterID.String(),
		AccountId:   s.accountID.String(),
		Name:        s.name,
		MapId:       target.ID,
		SpawnPoint:  at.spawnPoint,
		WaypointId:  at.waypoint,
		InstanceId:  uint64(inst.ID),
		Level:       uint32(snap.Progress.Level),
		Exp:         snap.Progress.Exp,
		Gold:        snap.Progress.Gold,
		State:       stateJSON,
		LeaseToken:  s.lease.Token,
		LayerKey:    s.layerKey(),
	}, reply)

	if err != nil {
		_ = s.node.dir.Leave(ctx, inst.ID)
		return err
	}
	if !reply.GetAccepted() {
		_ = s.node.dir.Leave(ctx, inst.ID)
		return fmt.Errorf("%w: %s", ErrTransferRefused, reply.GetError())
	}

	// 6. Only now is the source torn down. Everything before this point could
	//    fail and leave the character exactly where it was.
	newHandle, err := s.node.resolve(inst.ID, inst.Node)
	if err != nil {
		// The destination accepted but cannot be reached from here, which
		// means a room on another process -- the case M9 completes. The
		// character is now in that room, so the session must end rather than
		// pretend otherwise.
		s.log.Error("destination room is not reachable from this node",
			"instance", inst.ID, "node", inst.Node, "err", err)
		return err
	}

	s.mu.Lock()
	previousInstance := s.instance
	s.handle = newHandle
	s.entityID = room.EntityID(reply.GetEntityId())
	s.instance = inst.ID
	s.mapID = target.ID
	s.mu.Unlock()

	sourceHandle.Leave(ctx, sourceEntity)
	_ = s.node.dir.Leave(ctx, previousInstance)

	// Attach the connection to the room the character is now in.
	//
	// The socket lives on the node the player is connected to, not on the one
	// hosting the room, so it cannot travel in the transfer request -- the
	// destination accepts a character with nobody watching, and the sink is
	// handed over afterwards. It is the same path a reconnect takes, and it
	// sends the Welcome that tells the client it is somewhere else.
	s.attach(ctx, target)

	s.log.Info("character transferred",
		"to", target.ID, "instance", inst.ID, "node", inst.Node)
	return nil
}

// attach hands the player's connection to the room they have just arrived in.
func (s *Session) attach(ctx context.Context, target *content.Map) {
	s.mu.Lock()
	sink := s.sink
	s.mu.Unlock()

	if sink == nil {
		// Disconnected mid-transfer. The character stays in the destination
		// frozen, which is exactly where a reconnect will find them.
		return
	}

	if !s.attachTo(ctx, sink) {
		s.log.Error("the destination room lost the character before the connection attached",
			"map", target.ID)
		return
	}

	// The map changed, and so may the node holding them, so a whisper aimed
	// here would otherwise be addressed to where they used to be. The layer
	// key has to be re-applied too: the destination room joined them under
	// their character ID, which is wrong for somebody in a party.
	s.announcePresence(ctx, false)
	s.applyLayer(ctx)

	// Stats and inventory again: the client keeps them across a map change,
	// but the room it just joined has its own copy of the character and the
	// two must not drift.
	s.refreshStats(ctx, s.characterLevel(ctx))
}

// serveTransfers accepts characters handed to this node.
func (n *Node) serveTransfers(ctx context.Context) error {
	sub, err := n.bus.Respond(ctx, transferSubject(n.nodeID),
		func(reqCtx context.Context, _ string, payload []byte) (proto.Message, error) {
			return n.acceptTransfer(reqCtx, payload)
		})
	if err != nil {
		return fmt.Errorf("world: subscribing to transfers: %w", err)
	}

	n.transferSub = sub
	return nil
}

// acceptTransfer places an incoming character into a room on this node.
func (n *Node) acceptTransfer(ctx context.Context, payload []byte) (proto.Message, error) {
	var req mmov1.TransferRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, err
	}

	m, ok := n.content.Maps[req.GetMapId()]
	if !ok {
		return &mmov1.TransferReply{Error: "unknown map " + req.GetMapId()}, nil
	}

	characterID, err := uuid.Parse(req.GetCharacterId())
	if err != nil {
		return &mmov1.TransferReply{Error: "malformed character id"}, nil
	}

	// The fencing token must not go backwards. A replayed or delayed transfer
	// carrying an old token would otherwise resurrect a character in a room it
	// has already left.
	if !n.acceptToken(characterID, req.GetLeaseToken()) {
		return &mmov1.TransferReply{
			Error: "stale transfer: a newer one has already been accepted",
		}, nil
	}

	instance := directory.InstanceID(req.GetInstanceId())
	handle, err := n.ensureRoom(directory.Instance{
		ID: instance, Node: directory.NodeID(n.nodeID), Capacity: m.Capacity,
	}, m)
	if err != nil {
		return &mmov1.TransferReply{Error: err.Error()}, nil
	}

	var state room.CharacterState
	if len(req.GetState()) > 0 {
		state = room.UnmarshalState(req.GetState())
	}

	// Where the character lands. Resolved here, from this node's own content,
	// rather than taken as coordinates off the wire.
	spawn := m.DefaultSpawn().At
	placed := true
	switch {
	case req.GetWaypointId() != "":
		w, ok := n.content.Waypoints[req.GetWaypointId()]
		if !ok || w.MapID != m.ID {
			return &mmov1.TransferReply{Error: "unknown waypoint " + req.GetWaypointId()}, nil
		}
		spawn = w.At
	case req.GetSpawnPoint() != "":
		spawn = m.SpawnNamed(req.GetSpawnPoint()).At
	default:
		// A channel switch: the character keeps the position they had, which
		// is carried in the state.
		placed = false
	}

	entityID, err := handle.Join(ctx, room.JoinSpec{
		CharacterID: req.GetCharacterId(),
		Name:        req.GetName(),
		Progress: room.Progress{
			Level: int(req.GetLevel()),
			Exp:   req.GetExp(),
			Gold:  req.GetGold(),
			MapID: m.ID,
		},
		State: state,
		// The party while partied, the character otherwise. Sent rather than
		// looked up, because the destination node may not be the one that can
		// reach this character's party.
		LayerKey: req.GetLayerKey(),
		// Arriving through a portal places the character at the named spawn,
		// not where they stood in the map they left -- those coordinates mean
		// nothing here. A channel switch is the opposite: same map, same spot.
		Fresh:   placed,
		Spawn:   spawn,
		Arrived: true,
	})
	if err != nil {
		return &mmov1.TransferReply{Error: err.Error()}, nil
	}

	n.log.Info("accepted a character",
		"character", req.GetCharacterId(), "map", m.ID, "instance", instance)

	return &mmov1.TransferReply{
		Accepted: true,
		EntityId: uint32(entityID),
		NodeId:   n.nodeID,
	}, nil
}

// acceptToken records the highest fencing token seen for a character and
// reports whether this one may proceed.
func (n *Node) acceptToken(characterID uuid.UUID, token int64) bool {
	n.tokenMu.Lock()
	defer n.tokenMu.Unlock()

	if seen, ok := n.seenTokens[characterID]; ok && token < seen {
		return false
	}
	n.seenTokens[characterID] = token
	return true
}

// transferMessage turns a transfer failure into something worth showing.
func transferMessage(err error) string { return TravelMessage(err) }

// TravelMessage turns a travel failure into something worth showing a player.
//
// Every one of these is a thing a player can do by accident, so each gets its
// own sentence: "could not travel there" for a full channel and for a level
// gate would make both look like a bug.
func TravelMessage(err error) string {
	switch {
	case errors.Is(err, directory.ErrNoCapacity):
		return "that area is full"
	case errors.Is(err, bus.ErrNoResponder):
		return "that area is unavailable"
	case errors.Is(err, bus.ErrRequestTimeout):
		return "the server did not respond; try again"
	case errors.Is(err, ErrUnknownMap):
		return "that area does not exist"
	case errors.Is(err, ErrTravelBusy):
		return "you are already on your way"
	case errors.Is(err, ErrWaypointLocked):
		return "you have not been there yet"
	case errors.Is(err, ErrUnknownChannel), errors.Is(err, ErrNotHere):
		return "that channel is not available"
	case errors.Is(err, ErrSameChannel):
		return "you are already in that channel"
	default:
		return "could not travel there"
	}
}
