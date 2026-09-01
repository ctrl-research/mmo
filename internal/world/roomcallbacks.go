package world

import (
	"context"

	"github.com/ctrl-research/mmo/internal/content"

	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"google.golang.org/protobuf/proto"
)

// Receiving what a room in another process sends back.
//
// The mirror of remoteSink and remoteEvents. Everything arrives on one subject
// per character, and is dispatched into exactly the methods a local room would
// have called directly -- Session already implements room.SessionEvents, so
// there is one set of handlers rather than a local set and a remote set that
// can disagree.
//
// Subscribed only when the character is actually in a room on another node.
// Always subscribing would be simpler, but it is a live subscription per
// logged-in player for something most of them never use.

// watchRoomCallbacks subscribes to this character's room callbacks.
//
// Idempotent: a character that transfers between two remote rooms keeps one
// subscription, because the subject is keyed by character rather than by room.
// It deliberately takes no context. A subscription lives as long as the
// character is in a remote room, which outlives the login or the transfer that
// created it -- subscribing with either caller's context made the subscription
// die when that call returned, and the symptom was a player standing in a room
// that had stopped talking to them.
func (s *Session) watchRoomCallbacks() {
	s.mu.Lock()
	if s.callbackSub != nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	ctx := s.node.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	subject := roomCallbackSubject(s.node.nodeID, s.characterID.String())
	sub, err := s.node.bus.Subscribe(ctx, subject,
		func(_ context.Context, _ string, payload []byte) {
			var cb mmov1.RoomCallback
			if err := proto.Unmarshal(payload, &cb); err != nil {
				s.log.Error("decoding a room callback", "err", err)
				return
			}
			s.applyRoomCallback(&cb)
		})
	if err != nil {
		// Without this the character is in a room whose output never arrives:
		// they would stand still on a black screen. Logged loudly; the caller
		// carries on, because the alternative is refusing a login for a
		// subscription that may succeed on the next transfer.
		s.log.Error("subscribing to room callbacks", "subject", subject, "err", err)
		return
	}

	s.mu.Lock()
	if s.callbackSub != nil {
		// Two attaches raced. Keep the first and drop this one rather than
		// leaking a subscription that nothing will ever close.
		s.mu.Unlock()
		sub.Close()
		return
	}
	s.callbackSub = sub
	s.mu.Unlock()
}

// stopRoomCallbacks drops the subscription, if there is one.
func (s *Session) stopRoomCallbacks() {
	s.mu.Lock()
	sub := s.callbackSub
	s.callbackSub = nil
	s.mu.Unlock()

	if sub != nil {
		sub.Close()
	}
}

// applyRoomCallback dispatches one callback from a remote room.
func (s *Session) applyRoomCallback(cb *mmov1.RoomCallback) {
	if !s.callbackIsCurrent(cb.GetEntityId()) {
		return
	}

	switch body := cb.GetBody().(type) {
	case *mmov1.RoomCallback_Send:
		var msg mmov1.ServerMessage
		if err := proto.Unmarshal(body.Send, &msg); err != nil {
			s.log.Error("decoding a server message from another node", "err", err)
			return
		}
		s.deliver(&msg)

	case *mmov1.RoomCallback_Close:
		s.mu.Lock()
		sink := s.sink
		s.mu.Unlock()
		if sink != nil {
			sink.Close(body.Close.GetCode(), body.Close.GetReason())
		}

	case *mmov1.RoomCallback_ClaimLoot:
		c := body.ClaimLoot
		s.ClaimLoot(room.LootClaim{
			Player:      room.EntityID(c.GetPlayer()),
			CharacterID: c.GetCharacterId(),
			DropID:      room.EntityID(c.GetDropId()),
			Instance:    decodeItem(c.GetInstance()),
			Tick:        c.GetTick(),
		})

	case *mmov1.RoomCallback_EnterPortal:
		req, ok := resolvePortalEvent(s.node.content, body.EnterPortal)
		if !ok {
			s.log.Error("a portal that does not resolve here",
				"map", body.EnterPortal.GetMapId(),
				"index", body.EnterPortal.GetPortalIndex())
			return
		}
		s.EnterPortal(req)

	case *mmov1.RoomCallback_DiscoverWaypoint:
		w := body.DiscoverWaypoint
		s.DiscoverWaypoint(room.EntityID(w.GetPlayer()), w.GetCharacterId(), w.GetWaypointId())

	case *mmov1.RoomCallback_OpenStation:
		st := body.OpenStation
		station, ok := s.node.content.Stations[st.GetStationId()]
		if !ok {
			s.log.Error("a station this node does not have", "station", st.GetStationId())
			return
		}
		levels := make(map[string]int, len(st.GetLevels()))
		for k, v := range st.GetLevels() {
			levels[k] = int(v)
		}
		s.OpenStation(room.StationRequest{
			Player:      room.EntityID(st.GetPlayer()),
			CharacterID: st.GetCharacterId(),
			Station:     station,
			Entity:      room.EntityID(st.GetEntityId()),
			Levels:      levels,
		})

	case *mmov1.RoomCallback_RunCraft:
		c := body.RunCraft
		recipe, ok := s.node.content.Recipes[c.GetRecipeId()]
		if !ok {
			s.log.Error("a recipe this node does not have", "recipe", c.GetRecipeId())
			return
		}
		s.RunCraft(room.CraftRequest{
			Player:      room.EntityID(c.GetPlayer()),
			CharacterID: c.GetCharacterId(),
			Recipe:      recipe,
			Tick:        c.GetTick(),
		})

	case *mmov1.RoomCallback_GrantGather:
		g := body.GrantGather
		s.GrantGather(room.GatherYield{
			Player:      room.EntityID(g.GetPlayer()),
			CharacterID: g.GetCharacterId(),
			Skill:       g.GetSkill(),
			ItemID:      g.GetItemId(),
			Qty:         int(g.GetQty()),
			Tick:        g.GetTick(),
		})

	case *mmov1.RoomCallback_EndRun:
		e := body.EndRun
		dungeon, ok := s.node.content.Dungeons[e.GetDungeonId()]
		if !ok {
			s.log.Error("a dungeon this node does not have", "dungeon", e.GetDungeonId())
			return
		}
		s.EndRun(room.RunResult{
			Player:      room.EntityID(e.GetPlayer()),
			CharacterID: e.GetCharacterId(),
			Dungeon:     dungeon,
			Cleared:     e.GetCleared(),
		})
	}
}

// remoteHandleFor builds a handle to a room in another process.
func (n *Node) remoteHandleFor(instance directory.InstanceID, node directory.NodeID, characterID string) *remoteRoom {
	return &remoteRoom{
		bus:      n.bus,
		node:     node,
		instance: instance,
		log:      n.log,
		callback: roomCallbackSubject(n.nodeID, characterID),
	}
}

// resolvePortalEvent turns a portal named on the wire back into a portal.
//
// Resolved from this node's own content rather than taken off the wire: two
// nodes running different builds would otherwise send a player somewhere their
// own content does not agree about. A separate function because the index is
// the whole point -- a version that ignored it and took the first portal would
// work on every map that has only one, which is most of them.
func resolvePortalEvent(c *content.Content, e *mmov1.PortalEvent) (room.PortalRequest, bool) {
	if c == nil || e == nil {
		return room.PortalRequest{}, false
	}
	m, ok := c.Maps[e.GetMapId()]
	if !ok {
		return room.PortalRequest{}, false
	}
	portal, ok := portalAt(m, e.GetPortalIndex())
	if !ok {
		return room.PortalRequest{}, false
	}
	return room.PortalRequest{
		Player:      room.EntityID(e.GetPlayer()),
		CharacterID: e.GetCharacterId(),
		Portal:      portal,
		Tick:        e.GetTick(),
	}, true
}

// callbackIsCurrent reports whether a callback is about the character's
// current body.
//
// The subject is per character, and a character outlives the rooms it passes
// through -- so a message published by the room they just left can arrive
// after they have arrived somewhere else, and would be rendered as the player
// briefly standing back where they came from.
//
// Zero means the room does not know the entity yet, which is the window
// between a remote Join being applied and its reply coming back. Those are
// delivered: there is nothing else the character could be at that point.
//
// This narrows the window rather than closing it -- entity ids are per room,
// so the old room could have handed out the same one -- and the cost of being
// wrong is a single stale frame that the next snapshot corrects.
func (s *Session) callbackIsCurrent(entity uint32) bool {
	if entity == 0 {
		return true
	}

	s.mu.Lock()
	current := s.entityID
	s.mu.Unlock()

	return room.EntityID(entity) == current
}
