package world

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ctrl-research/mmo/internal/bus"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"google.golang.org/protobuf/proto"
)

// Driving a room in another process.
//
// A session lives on the node the player is connected to, and the room it is
// in may be hosted anywhere -- that is the whole point of placing rooms by load
// rather than by who happened to log in where. Until this existed the two had
// to be in the same process, and Node.resolve said so in an error.
//
// The split is asymmetric on purpose. Commands that return something are
// request/reply; the rest are published and not waited for. Input arrives every
// client tick, and a round trip per input would put the network between a
// keypress and the simulation -- the room already treats input as a queue that
// may starve, which is the same failure a dropped publish produces and is
// already handled by replaying the last one.

// roomCommandTimeout bounds the three commands that wait for an answer.
//
// Shorter than a transfer, because these are on the login and checkpoint paths
// rather than a player-visible action, and a caller that waits fifteen seconds
// for a Capture has already missed the checkpoint it was for.
const roomCommandTimeout = 5 * time.Second

// roomSubject is where a node listens for commands addressed to the rooms it
// hosts.
//
// Keyed by node rather than by instance: a node knows which instances it has,
// and a subscription per room would mean subscribing and unsubscribing on the
// room lifecycle -- one more thing to get wrong on a path that already has
// enough of them.
func roomSubject(nodeID string) string {
	return "world.node." + sanitiseSubject(nodeID) + ".room"
}

// roomCallbackSubject is where one attached character's callbacks are sent.
//
// Per character rather than per node, so a node subscribes only to the
// characters it is actually holding a connection for, and a node that is sent
// callbacks for somebody else's character never sees them.
func roomCallbackSubject(nodeID, characterID string) string {
	return "world.node." + sanitiseSubject(nodeID) + ".callback." + sanitiseSubject(characterID)
}

// remoteRoom is a room.Handle for a room hosted by another process.
type remoteRoom struct {
	bus      bus.Bus
	node     directory.NodeID
	instance directory.InstanceID
	log      *slog.Logger

	// callback is the subject this node listens on for everything the room
	// sends back. Set by whoever attaches a connection.
	callback string
}

// send publishes a command without waiting.
func (r *remoteRoom) send(ctx context.Context, cmd *mmov1.RoomCommand) {
	cmd.InstanceId = uint64(r.instance)
	// Notify rather than Publish: the subject is served by Respond, which
	// reads every message as an envelope. See bus.Notify.
	if err := bus.Notify(ctx, r.bus, roomSubject(string(r.node)), cmd); err != nil {
		// Logged and dropped. There is nothing a caller of Input or Leave can
		// do about a bus failure, and returning an error from methods whose
		// whole contract is "never block the caller" would push the decision
		// somewhere with even less to do about it.
		r.log.Error("sending a room command",
			"instance", r.instance, "node", r.node, "err", err)
	}
}

// call sends a command and waits for its reply.
func (r *remoteRoom) call(ctx context.Context, cmd *mmov1.RoomCommand) (*mmov1.RoomCommandReply, error) {
	cmd.InstanceId = uint64(r.instance)

	ctx, cancel := context.WithTimeout(ctx, roomCommandTimeout)
	defer cancel()

	reply := &mmov1.RoomCommandReply{}
	if err := r.bus.Request(ctx, roomSubject(string(r.node)), cmd, reply); err != nil {
		return nil, fmt.Errorf("world: room %d on node %s: %w", r.instance, r.node, err)
	}
	if reply.GetClosed() {
		// Reported as the error the local join path already retries on, so a
		// room that closed under a remote caller behaves the same as one that
		// closed under a local one.
		return reply, room.ErrRoomClosed
	}
	if e := reply.GetError(); e != "" {
		return reply, errors.New(e)
	}
	return reply, nil
}

func (r *remoteRoom) Join(ctx context.Context, spec room.JoinSpec) (room.EntityID, error) {
	reply, err := r.call(ctx, &mmov1.RoomCommand{
		CallbackSubject: r.callback,
		Body: &mmov1.RoomCommand_Join{
			Join: &mmov1.JoinCommand{Spec: encodeJoinSpec(spec)},
		},
	})
	if err != nil {
		return 0, err
	}
	return room.EntityID(reply.GetEntityId()), nil
}

func (r *remoteRoom) Capture(ctx context.Context, id room.EntityID) (room.Snapshot, bool) {
	reply, err := r.call(ctx, &mmov1.RoomCommand{
		Body: &mmov1.RoomCommand_Capture{
			Capture: &mmov1.CaptureCommand{EntityId: uint32(id)},
		},
	})
	if err != nil {
		// Reported as "not there", which is what a caller does with a false
		// anyway: skip the checkpoint. Inventing a snapshot would write
		// zeroes over a character's progress.
		r.log.Error("capturing a remote character",
			"instance", r.instance, "entity", id, "err", err)
		return room.Snapshot{}, false
	}
	if !reply.GetFound() {
		return room.Snapshot{}, false
	}
	return decodeSnapshot(reply.GetSnapshot()), true
}

func (r *remoteRoom) Attach(ctx context.Context, id room.EntityID, a room.Attachment) bool {
	reply, err := r.call(ctx, &mmov1.RoomCommand{
		CallbackSubject: r.callback,
		Body: &mmov1.RoomCommand_Attach{
			Attach: &mmov1.AttachCommand{
				EntityId:       uint32(id),
				Loadout:        encodeLoadout(a.Loadout),
				LayerKey:       a.LayerKey,
				KnownWaypoints: a.KnownWaypoints,
				Secondary:      encodeSecondary(a.Secondary),
				// Nil and empty mean different things -- "leave what the room
				// has" against "set it to nothing" -- and proto3 cannot tell
				// them apart on the far side.
				HasSecondary: a.Secondary != nil,
				HasWaypoints: a.KnownWaypoints != nil,
			},
		},
	})
	if err != nil {
		r.log.Error("attaching to a remote room",
			"instance", r.instance, "entity", id, "err", err)
		return false
	}
	return reply.GetAttached()
}

func (r *remoteRoom) Freeze(ctx context.Context, id room.EntityID) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_Freeze{
		Freeze: &mmov1.FreezeCommand{EntityId: uint32(id)},
	}})
}

func (r *remoteRoom) SetLoadout(ctx context.Context, id room.EntityID, slots []room.LoadoutSlot) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_SetLoadout{
		SetLoadout: &mmov1.SetLoadoutCommand{
			EntityId: uint32(id), Slots: encodeLoadout(slots),
		},
	}})
}

func (r *remoteRoom) SetStats(ctx context.Context, id room.EntityID, d room.Derived) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_SetStats{
		SetStats: &mmov1.SetStatsCommand{
			EntityId: uint32(id), Derived: encodeDerived(d),
		},
	}})
}

func (r *remoteRoom) ResolveLoot(ctx context.Context, player, dropID room.EntityID, granted bool, reason string) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_ResolveLoot{
		ResolveLoot: &mmov1.ResolveLootCommand{
			Player: uint32(player), DropId: uint32(dropID),
			Granted: granted, Reason: reason,
		},
	}})
}

func (r *remoteRoom) AbortTransfer(ctx context.Context, id room.EntityID, reason string) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_AbortTransfer{
		AbortTransfer: &mmov1.AbortTransferCommand{
			EntityId: uint32(id), Reason: reason,
		},
	}})
}

func (r *remoteRoom) Leave(ctx context.Context, id room.EntityID) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_Leave{
		Leave: &mmov1.LeaveCommand{EntityId: uint32(id)},
	}})
}

func (r *remoteRoom) Input(ctx context.Context, id room.EntityID, seq uint32, in sim.Input) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_Input{
		Input: &mmov1.InputCommand{
			EntityId: uint32(id), Seq: seq,
			MoveX: int32(in.MoveX), Jump: in.Jump, Up: in.Up, Down: in.Down,
		},
	}})
}

func (r *remoteRoom) Cast(ctx context.Context, id room.EntityID, skillID string, facingLeft bool) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_Cast{
		Cast: &mmov1.CastCommand{
			EntityId: uint32(id), SkillId: skillID, FacingLeft: facingLeft,
		},
	}})
}

func (r *remoteRoom) Interact(ctx context.Context, id, target room.EntityID, kind room.InteractKind) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_Interact{
		Interact: &mmov1.InteractCommand{
			EntityId: uint32(id), Target: uint32(target), Kind: uint32(kind),
		},
	}})
}

func (r *remoteRoom) Craft(ctx context.Context, id, station room.EntityID, recipe string) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_Craft{
		Craft: &mmov1.CraftCommand{
			EntityId: uint32(id), Station: uint32(station), RecipeId: recipe,
		},
	}})
}

func (r *remoteRoom) ResolveCraft(ctx context.Context, player room.EntityID, made bool, reason string) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_ResolveCraft{
		ResolveCraft: &mmov1.ResolveCraftCommand{
			Player: uint32(player), Made: made, Reason: reason,
		},
	}})
}

func (r *remoteRoom) Say(ctx context.Context, id room.EntityID, body string, atMillis int64) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_Say{
		Say: &mmov1.SayCommand{
			EntityId: uint32(id), Body: body, AtMillis: atMillis,
		},
	}})
}

func (r *remoteRoom) SetLayer(ctx context.Context, id room.EntityID, key string, loot room.LootRule) {
	r.send(ctx, &mmov1.RoomCommand{Body: &mmov1.RoomCommand_SetLayer{
		SetLayer: &mmov1.SetLayerCommand{
			EntityId: uint32(id), Key: key, Loot: uint32(loot),
		},
	}})
}

var _ room.Handle = (*remoteRoom)(nil)

// remoteSink forwards a room's outbound messages to the node holding the
// socket.
//
// It carries the ServerMessage already encoded, which is what the room would
// have produced anyway on its way to a client -- so the extra hop costs a
// publish, not a re-encode.
type remoteSink struct {
	bus     bus.Bus
	subject string
	log     *slog.Logger

	// entity is atomic because a join sets it from the caller's goroutine
	// after the room has already been given the sink -- the room is ticking by
	// then, and may be sending through it.
	entity atomic.Uint32
}

func (s *remoteSink) publish(body *mmov1.RoomCallback) {
	body.EntityId = s.entity.Load()

	// Background rather than the tick's context: the room is mid-tick and a
	// cancellation here would drop a snapshot for everybody, not just this
	// player.
	ctx, cancel := context.WithTimeout(context.Background(), roomCommandTimeout)
	defer cancel()

	if err := s.bus.Publish(ctx, s.subject, body); err != nil {
		s.log.Error("forwarding a room callback", "subject", s.subject, "err", err)
	}
}

func (s *remoteSink) Send(msg *mmov1.ServerMessage) {
	raw, err := proto.Marshal(msg)
	if err != nil {
		s.log.Error("encoding a server message for another node", "err", err)
		return
	}
	s.publish(&mmov1.RoomCallback{Body: &mmov1.RoomCallback_Send{Send: raw}})
}

func (s *remoteSink) Close(code uint32, reason string) {
	s.publish(&mmov1.RoomCallback{Body: &mmov1.RoomCallback_Close{
		Close: &mmov1.SinkClose{Code: code, Reason: reason},
	}})
}

var _ room.Sink = (*remoteSink)(nil)

// remoteEvents forwards a room's session callbacks to the node holding the
// session.
//
// Same subject as the sink, because the two are ordered with respect to each
// other: a loot claim that overtook the snapshot showing the drop would be a
// claim for something the player has not been shown.
type remoteEvents struct {
	sink *remoteSink

	// m is the map the room is running, so a portal can be named by its
	// position in it rather than shipped as a struct.
	m *content.Map
}

func (e *remoteEvents) ClaimLoot(claim room.LootClaim) {
	e.sink.publish(&mmov1.RoomCallback{Body: &mmov1.RoomCallback_ClaimLoot{
		ClaimLoot: &mmov1.LootClaimEvent{
			Player: uint32(claim.Player), CharacterId: claim.CharacterID,
			DropId: uint32(claim.DropID), Instance: encodeItem(claim.Instance),
			Tick: claim.Tick,
		},
	}})
}

func (e *remoteEvents) EnterPortal(req room.PortalRequest) {
	index, ok := portalIndex(e.m, req.Portal)
	if !ok {
		// A portal the map does not contain cannot be named, and the session
		// would have nothing to resolve. It means the room and this code
		// disagree about the map, which is worth saying loudly rather than
		// sending a transfer to portal zero.
		e.sink.log.Error("a portal that is not in its own map",
			"map", e.m.ID, "portal", req.Portal.Name)
		return
	}
	e.sink.publish(&mmov1.RoomCallback{Body: &mmov1.RoomCallback_EnterPortal{
		EnterPortal: &mmov1.PortalEvent{
			Player: uint32(req.Player), CharacterId: req.CharacterID,
			MapId: e.m.ID, PortalIndex: index, Tick: req.Tick,
		},
	}})
}

func (e *remoteEvents) DiscoverWaypoint(player room.EntityID, characterID, waypointID string) {
	e.sink.publish(&mmov1.RoomCallback{Body: &mmov1.RoomCallback_DiscoverWaypoint{
		DiscoverWaypoint: &mmov1.WaypointEvent{
			Player: uint32(player), CharacterId: characterID, WaypointId: waypointID,
		},
	}})
}

func (e *remoteEvents) OpenStation(req room.StationRequest) {
	levels := make(map[string]int32, len(req.Levels))
	for k, v := range req.Levels {
		levels[k] = int32(v)
	}
	var station string
	if req.Station != nil {
		station = req.Station.ID
	}
	e.sink.publish(&mmov1.RoomCallback{Body: &mmov1.RoomCallback_OpenStation{
		OpenStation: &mmov1.StationEvent{
			Player: uint32(req.Player), CharacterId: req.CharacterID,
			StationId: station, EntityId: uint32(req.Entity), Levels: levels,
		},
	}})
}

func (e *remoteEvents) RunCraft(req room.CraftRequest) {
	var recipe string
	if req.Recipe != nil {
		recipe = req.Recipe.ID
	}
	e.sink.publish(&mmov1.RoomCallback{Body: &mmov1.RoomCallback_RunCraft{
		RunCraft: &mmov1.CraftEvent{
			Player: uint32(req.Player), CharacterId: req.CharacterID,
			RecipeId: recipe, Tick: req.Tick,
		},
	}})
}

func (e *remoteEvents) GrantGather(y room.GatherYield) {
	e.sink.publish(&mmov1.RoomCallback{Body: &mmov1.RoomCallback_GrantGather{
		GrantGather: &mmov1.GatherEvent{
			Player: uint32(y.Player), CharacterId: y.CharacterID,
			Skill: y.Skill, ItemId: y.ItemID, Qty: int32(y.Qty), Tick: y.Tick,
		},
	}})
}

func (e *remoteEvents) EndRun(res room.RunResult) {
	var dungeon string
	if res.Dungeon != nil {
		dungeon = res.Dungeon.ID
	}
	e.sink.publish(&mmov1.RoomCallback{Body: &mmov1.RoomCallback_EndRun{
		EndRun: &mmov1.RunEndEvent{
			Player: uint32(res.Player), CharacterId: res.CharacterID,
			DungeonId: dungeon, Cleared: res.Cleared,
		},
	}})
}

var _ room.SessionEvents = (*remoteEvents)(nil)
