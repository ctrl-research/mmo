package world

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ctrl-research/mmo/internal/bus"
	"github.com/ctrl-research/mmo/internal/directory"

	"github.com/ctrl-research/mmo/internal/content"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// Serving characters to gateways in other processes.
//
// A gateway terminates the socket and knows nothing about rooms. It asks a
// world node to take a character, and from then on forwards the player's
// already-validated requests here and publishes nothing but what comes back.
//
// The validating stays on the gateway: that is the trust boundary, and it is
// where the untrusted bytes arrive. What crosses is a request that has already
// been clamped and bounds-checked, which is why these mirror the Go request
// types rather than the client's messages.

// enterSubject is where a node accepts characters from gateways.
func enterSubject(nodeID string) string {
	return "world.node." + sanitiseSubject(nodeID) + ".enter"
}

// sessionSubject is where a node accepts commands for the characters it holds.
func sessionSubject(nodeID string) string {
	return "world.node." + sanitiseSubject(nodeID) + ".session"
}

// serveSessions accepts characters and the commands that drive them.
func (n *Node) serveSessions(ctx context.Context) error {
	enter, err := n.bus.Respond(ctx, enterSubject(n.nodeID),
		func(reqCtx context.Context, _ string, payload []byte) (proto.Message, error) {
			var req mmov1.EnterRequest
			if err := proto.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return n.acceptEnter(reqCtx, &req), nil
		})
	if err != nil {
		return fmt.Errorf("world: subscribing to character entries: %w", err)
	}
	n.enterSub = enter

	commands, err := n.bus.Respond(ctx, sessionSubject(n.nodeID),
		func(reqCtx context.Context, _ string, payload []byte) (proto.Message, error) {
			var cmd mmov1.SessionCommand
			if err := proto.Unmarshal(payload, &cmd); err != nil {
				return nil, err
			}
			return n.applySessionCommand(reqCtx, &cmd), nil
		})
	if err != nil {
		return fmt.Errorf("world: subscribing to session commands: %w", err)
	}
	n.sessionSub = commands

	return nil
}

// acceptEnter takes ownership of a character on behalf of a gateway.
func (n *Node) acceptEnter(ctx context.Context, req *mmov1.EnterRequest) *mmov1.EnterReply {
	accountID, err := uuid.Parse(req.GetAccountId())
	if err != nil {
		return &mmov1.EnterReply{Error: "malformed account id"}
	}
	characterID, err := uuid.Parse(req.GetCharacterId())
	if err != nil {
		return &mmov1.EnterReply{Error: "malformed character id"}
	}
	if req.GetCallbackSubject() == "" {
		return &mmov1.EnterReply{Error: "a gateway must say where to send the player's messages"}
	}

	// The sink publishes to the gateway instead of writing to a socket. It is
	// the only part of a session that has to know the gateway exists.
	sink := &gatewaySink{bus: n.bus, subject: req.GetCallbackSubject(), log: n.log}

	session, err := n.Enter(ctx, accountID, characterID, sink)
	if err != nil {
		return &mmov1.EnterReply{Error: err.Error(), Busy: isCharacterBusy(err)}
	}

	// Losing the lease has to reach the gateway, which is the only thing that
	// can close the connection.
	session.OnOwnershipLost(func(reason string) {
		sink.ownershipLost(reason)
	})

	return &mmov1.EnterReply{
		Name:     session.Name(),
		EntityId: uint32(session.EntityID()),
	}
}

// applySessionCommand runs one command against a character this node holds.
func (n *Node) applySessionCommand(ctx context.Context, cmd *mmov1.SessionCommand) *mmov1.SessionReply {
	characterID, err := uuid.Parse(cmd.GetCharacterId())
	if err != nil {
		return &mmov1.SessionReply{Error: "malformed character id"}
	}

	s, ok := n.held(characterID)
	if !ok {
		// Not a failure of the call: the character has gone -- logged out,
		// transferred, or lost its lease. The gateway closes the connection
		// rather than retrying against a node that no longer has them.
		return &mmov1.SessionReply{Gone: true}
	}

	fail := func(err error) *mmov1.SessionReply {
		if err != nil {
			return &mmov1.SessionReply{Error: err.Error()}
		}
		return &mmov1.SessionReply{}
	}

	switch body := cmd.GetBody().(type) {
	case *mmov1.SessionCommand_Input:
		in := body.Input
		s.Input(ctx, in.GetSeq(), sim.Input{
			MoveX: in.GetMoveX(), Jump: in.GetJump(),
			Up: in.GetUp(), Down: in.GetDown(),
		})

	case *mmov1.SessionCommand_Cast:
		s.Cast(ctx, body.Cast.GetSkillId(), body.Cast.GetFacingLeft())

	case *mmov1.SessionCommand_Interact:
		s.Interact(ctx,
			room.EntityID(body.Interact.GetTarget()),
			room.InteractKind(body.Interact.GetKind()))

	case *mmov1.SessionCommand_Craft:
		s.Craft(ctx, room.EntityID(body.Craft.GetStation()), body.Craft.GetRecipeId())

	case *mmov1.SessionCommand_ItemAction:
		a := body.ItemAction
		itemID, err := uuid.Parse(a.GetItemId())
		if err != nil {
			return &mmov1.SessionReply{Error: "malformed item id"}
		}
		return fail(s.ApplyItemAction(ctx, ItemAction{
			Kind:      ItemActionKind(a.GetKind()),
			ItemID:    itemID,
			Slot:      int(a.GetSlot()),
			EquipSlot: content.EquipSlot(a.GetEquipSlot()),
		}))

	case *mmov1.SessionCommand_Chat:
		c := body.Chat
		return fail(s.SendChat(ctx, &mmov1.ChatSend{
			Channel: mmov1.ChatChannel(c.GetChannel()),
			Body:    c.GetBody(),
			Target:  c.GetTarget(),
		}))

	case *mmov1.SessionCommand_Party:
		return fail(s.Party(ctx, PartyRequest{
			Kind: PartyActionKind(body.Party.GetKind()), Target: body.Party.GetTarget(),
		}))

	case *mmov1.SessionCommand_Guild:
		return fail(s.Guild(ctx, GuildRequest{
			Kind: GuildActionKind(body.Guild.GetKind()), Target: body.Guild.GetTarget(),
		}))

	case *mmov1.SessionCommand_Social:
		return fail(s.Social(ctx, SocialRequest{
			Kind: SocialActionKind(body.Social.GetKind()), Target: body.Social.GetTarget(),
		}))

	case *mmov1.SessionCommand_Loadout:
		l := body.Loadout
		return fail(s.SetBarSlot(ctx, LoadoutRequest{
			Slot: int(l.GetSlot()), SkillID: l.GetSkillId(), Supports: l.GetSupports(),
		}))

	case *mmov1.SessionCommand_Passive:
		p := body.Passive
		return fail(s.Passive(ctx, PassiveRequest{
			Allocate: int(p.GetAllocate()), Refund: int(p.GetRefund()),
			RespecAll: p.GetRespecAll(),
		}))

	case *mmov1.SessionCommand_Travel:
		tr := body.Travel
		return fail(s.Travel(ctx, TravelRequest{
			WaypointID: tr.GetWaypointId(),
			Channel:    directoryInstance(tr.GetChannel()),
			NewChannel: tr.GetNewChannel(),
		}))

	case *mmov1.SessionCommand_WorldMap:
		raw, err := proto.Marshal(s.WorldMap(ctx))
		if err != nil {
			return &mmov1.SessionReply{Error: err.Error()}
		}
		return &mmov1.SessionReply{WorldMap: raw}

	case *mmov1.SessionCommand_Close:
		s.Close(ctx)

	case *mmov1.SessionCommand_Disconnect:
		s.Disconnect(ctx)
	}

	return &mmov1.SessionReply{}
}

// gatewaySink is a room.Sink that publishes to the gateway holding the socket.
//
// It is the only part of a session that knows a gateway exists. Everything
// else -- the room, the party, the checkpoint -- writes to a Sink and does not
// care whether the other end is a socket in this process or a subject.
type gatewaySink struct {
	bus     bus.Bus
	subject string
	log     *slog.Logger
}

func (g *gatewaySink) publish(cb *mmov1.SessionCallback) {
	// Background rather than a caller's context: a send can come from a room
	// mid-tick, and a cancellation there would drop a message for one player
	// because something unrelated timed out.
	ctx, cancel := context.WithTimeout(context.Background(), gatewayCallbackTimeout)
	defer cancel()

	if err := g.bus.Publish(ctx, g.subject, cb); err != nil {
		g.log.Error("sending to a gateway", "subject", g.subject, "err", err)
	}
}

func (g *gatewaySink) Send(msg *mmov1.ServerMessage) {
	raw, err := proto.Marshal(msg)
	if err != nil {
		g.log.Error("encoding a message for a gateway", "err", err)
		return
	}
	g.publish(&mmov1.SessionCallback{Body: &mmov1.SessionCallback_Send{Send: raw}})
}

func (g *gatewaySink) Close(code uint32, reason string) {
	g.publish(&mmov1.SessionCallback{Body: &mmov1.SessionCallback_Close{
		Close: &mmov1.SinkClose{Code: code, Reason: reason},
	}})
}

func (g *gatewaySink) ownershipLost(reason string) {
	g.publish(&mmov1.SessionCallback{
		Body: &mmov1.SessionCallback_OwnershipLost{OwnershipLost: reason},
	})
}

var _ room.Sink = (*gatewaySink)(nil)

// gatewayCallbackTimeout bounds a publish to a gateway.
const gatewayCallbackTimeout = 5 * time.Second

func isCharacterBusy(err error) bool { return errors.Is(err, ErrCharacterBusy) }

func directoryInstance(id uint64) directory.InstanceID { return directory.InstanceID(id) }
