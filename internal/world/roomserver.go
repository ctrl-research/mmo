package world

import (
	"context"
	"errors"
	"fmt"

	"github.com/ctrl-research/mmo/internal/content"

	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"google.golang.org/protobuf/proto"
)

// Serving the rooms this node hosts to sessions on other nodes.
//
// The mirror of remoteroom.go. Everything here runs on the node holding the
// room; everything there runs on the node holding the connection.
//
// Commands are applied to the local handle exactly as a local caller would
// apply them, which is the point: there is one room implementation and one set
// of rules, and being driven from another process is not a different mode.

// serveRooms accepts commands for the rooms this node hosts.
func (n *Node) serveRooms(ctx context.Context) error {
	// Subscribed rather than Responded, because most commands are one-way and
	// only three want an answer. The bus delivers both through the same
	// handler, and replying is decided per command below.
	sub, err := n.bus.Respond(ctx, roomSubject(n.nodeID),
		func(reqCtx context.Context, _ string, payload []byte) (proto.Message, error) {
			var cmd mmov1.RoomCommand
			if err := proto.Unmarshal(payload, &cmd); err != nil {
				return nil, err
			}
			return n.applyRoomCommand(reqCtx, &cmd), nil
		})
	if err != nil {
		return fmt.Errorf("world: subscribing to room commands: %w", err)
	}

	n.roomSub = sub
	return nil
}

// applyRoomCommand runs one command against a locally hosted room.
func (n *Node) applyRoomCommand(ctx context.Context, cmd *mmov1.RoomCommand) *mmov1.RoomCommandReply {
	instance := directory.InstanceID(cmd.GetInstanceId())

	n.mu.Lock()
	hosted, ok := n.rooms[instance]
	n.mu.Unlock()
	if !ok {
		// Reported as closed rather than as an error: from the caller's side
		// these are the same situation -- the room is not there any more --
		// and the join path already knows to re-place on a closed room.
		return &mmov1.RoomCommandReply{Closed: true}
	}
	handle, m := hosted.handle, hosted.m

	switch body := cmd.GetBody().(type) {
	case *mmov1.RoomCommand_Join:
		spec := decodeJoinSpec(body.Join.GetSpec())
		// The connection is attached in the same step as the join, unlike a
		// local caller which already holds the sink it is passing. A remote
		// one cannot: its sink is a subject, and the subject is only useful
		// once there is an entity to address callbacks to.
		if sub := cmd.GetCallbackSubject(); sub != "" {
			sink, events := n.callbackPair(sub, 0, m)
			spec.Sink, spec.Events = sink, events
		}

		entityID, err := handle.Join(ctx, spec)
		if err != nil {
			if errors.Is(err, room.ErrRoomClosed) {
				return &mmov1.RoomCommandReply{Closed: true}
			}
			return &mmov1.RoomCommandReply{Error: err.Error()}
		}
		// The sink was built before the entity id existed, so it is told now.
		if sink, ok := spec.Sink.(*remoteSink); ok {
			sink.entity.Store(uint32(entityID))
		}
		return &mmov1.RoomCommandReply{EntityId: uint32(entityID)}

	case *mmov1.RoomCommand_Capture:
		snap, found := handle.Capture(ctx, room.EntityID(body.Capture.GetEntityId()))
		if !found {
			return &mmov1.RoomCommandReply{}
		}
		return &mmov1.RoomCommandReply{Snapshot: encodeSnapshot(snap), Found: true}

	case *mmov1.RoomCommand_Attach:
		a := body.Attach
		id := room.EntityID(a.GetEntityId())

		sink, events := n.callbackPair(cmd.GetCallbackSubject(), id, m)
		attachment := attachmentFrom(a)
		attachment.Sink, attachment.Events = sink, events
		return &mmov1.RoomCommandReply{Attached: handle.Attach(ctx, id, attachment)}

	case *mmov1.RoomCommand_Freeze:
		handle.Freeze(ctx, room.EntityID(body.Freeze.GetEntityId()))

	case *mmov1.RoomCommand_SetLoadout:
		handle.SetLoadout(ctx,
			room.EntityID(body.SetLoadout.GetEntityId()),
			decodeLoadout(body.SetLoadout.GetSlots()))

	case *mmov1.RoomCommand_SetStats:
		derived, ok := decodeDerived(body.SetStats.GetDerived())
		if !ok {
			// A block that will not decode is refused rather than applied.
			// See decodeStats: an empty block is not a neutral one.
			n.log.Error("a stat block that did not decode",
				"instance", instance, "entity", body.SetStats.GetEntityId())
			break
		}
		handle.SetStats(ctx, room.EntityID(body.SetStats.GetEntityId()), derived)

	case *mmov1.RoomCommand_ResolveLoot:
		r := body.ResolveLoot
		handle.ResolveLoot(ctx,
			room.EntityID(r.GetPlayer()), room.EntityID(r.GetDropId()),
			r.GetGranted(), r.GetReason())

	case *mmov1.RoomCommand_AbortTransfer:
		handle.AbortTransfer(ctx,
			room.EntityID(body.AbortTransfer.GetEntityId()),
			body.AbortTransfer.GetReason())

	case *mmov1.RoomCommand_Leave:
		handle.Leave(ctx, room.EntityID(body.Leave.GetEntityId()))

	case *mmov1.RoomCommand_Input:
		in := body.Input
		handle.Input(ctx, room.EntityID(in.GetEntityId()), in.GetSeq(), sim.Input{
			MoveX: in.GetMoveX(),
			Jump:  in.GetJump(), Up: in.GetUp(), Down: in.GetDown(),
		})

	case *mmov1.RoomCommand_Cast:
		handle.Cast(ctx,
			room.EntityID(body.Cast.GetEntityId()),
			body.Cast.GetSkillId(), body.Cast.GetFacingLeft())

	case *mmov1.RoomCommand_Interact:
		i := body.Interact
		handle.Interact(ctx,
			room.EntityID(i.GetEntityId()), room.EntityID(i.GetTarget()),
			room.InteractKind(i.GetKind()))

	case *mmov1.RoomCommand_Craft:
		c := body.Craft
		handle.Craft(ctx,
			room.EntityID(c.GetEntityId()), room.EntityID(c.GetStation()),
			c.GetRecipeId())

	case *mmov1.RoomCommand_ResolveCraft:
		handle.ResolveCraft(ctx,
			room.EntityID(body.ResolveCraft.GetPlayer()),
			body.ResolveCraft.GetMade(), body.ResolveCraft.GetReason())

	case *mmov1.RoomCommand_Say:
		handle.Say(ctx,
			room.EntityID(body.Say.GetEntityId()),
			body.Say.GetBody(), body.Say.GetAtMillis())

	case *mmov1.RoomCommand_SetLayer:
		handle.SetLayer(ctx,
			room.EntityID(body.SetLayer.GetEntityId()),
			body.SetLayer.GetKey(), room.LootRule(body.SetLayer.GetLoot()))
	}

	return &mmov1.RoomCommandReply{}
}

// callbackPair builds the sink and event forwarder for one attached character.
func (n *Node) callbackPair(subject string, id room.EntityID, m *content.Map) (room.Sink, room.SessionEvents) {
	sink := &remoteSink{bus: n.bus, subject: subject, log: n.log}
	sink.entity.Store(uint32(id))
	return sink, &remoteEvents{sink: sink, m: m}
}

// attachmentFrom rebuilds an Attachment, minus the two references that cannot
// travel.
//
// Its whole job is the distinction proto3 cannot express: absent means "leave
// what the room already has", which is what a reconnect to a character still
// standing in the room wants, while present but empty means "set it to
// nothing". Collapsing the two makes a reconnect wipe the room's copy, and the
// symptom is a player rediscovering waypoints they unlocked months ago.
func attachmentFrom(a *mmov1.AttachCommand) room.Attachment {
	if a == nil {
		return room.Attachment{}
	}

	out := room.Attachment{
		Loadout:  decodeLoadout(a.GetLoadout()),
		LayerKey: a.GetLayerKey(),
	}
	if a.GetHasWaypoints() {
		out.KnownWaypoints = a.GetKnownWaypoints()
		if out.KnownWaypoints == nil {
			out.KnownWaypoints = []string{}
		}
	}
	if a.GetHasSecondary() {
		out.Secondary = decodeSecondary(a.GetSecondary())
		if out.Secondary == nil {
			out.Secondary = room.SecondaryProgress{}
		}
	}
	return out
}
