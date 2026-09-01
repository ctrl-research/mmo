package world

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"google.golang.org/protobuf/proto"
)

// Starting and stopping rooms.
//
// Which node hosts a room is the directory's decision, not the caller's. A
// gateway placing a player into a map may well be told the instance belongs to
// another node, and that node has to be running the room before anyone can
// join it -- so "make sure this room exists" is a message, like everything
// else that crosses a node boundary.

// hostSubject is where a node listens for requests to host a room.
func hostSubject(nodeID string) string {
	return "world.node." + sanitiseSubject(nodeID) + ".host"
}

// hostRoom makes sure inst is running, wherever the directory put it, and
// returns a handle to it.
func (n *Node) hostRoom(ctx context.Context, inst directory.Instance, m *content.Map, characterID string) (room.Handle, error) {
	if string(inst.Node) == n.nodeID {
		return n.ensureRoom(inst, m)
	}

	reply := &mmov1.HostReply{}
	err := n.bus.Request(ctx, hostSubject(string(inst.Node)), &mmov1.HostRequest{
		InstanceId: uint64(inst.ID),
		MapId:      m.ID,
	}, reply)
	if err != nil {
		return nil, fmt.Errorf("world: asking node %s to host instance %d: %w",
			inst.Node, inst.ID, err)
	}
	if !reply.GetHosting() {
		return nil, fmt.Errorf("world: node %s refused to host instance %d: %s",
			inst.Node, inst.ID, reply.GetError())
	}

	return n.resolve(inst.ID, inst.Node, characterID)
}

// serveHosting accepts requests to start hosting a room.
func (n *Node) serveHosting(ctx context.Context) error {
	sub, err := n.bus.Respond(ctx, hostSubject(n.nodeID),
		func(_ context.Context, _ string, payload []byte) (proto.Message, error) {
			var req mmov1.HostRequest
			if err := proto.Unmarshal(payload, &req); err != nil {
				return nil, err
			}

			m, ok := n.content.Maps[req.GetMapId()]
			if !ok {
				return &mmov1.HostReply{Error: "unknown map " + req.GetMapId()}, nil
			}

			inst := directory.Instance{
				ID:       directory.InstanceID(req.GetInstanceId()),
				Node:     directory.NodeID(n.nodeID),
				Capacity: m.Capacity,
			}
			if _, err := n.ensureRoom(inst, m); err != nil {
				return &mmov1.HostReply{Error: err.Error()}, nil
			}
			return &mmov1.HostReply{Hosting: true, NodeId: n.nodeID}, nil
		})
	if err != nil {
		return fmt.Errorf("world: subscribing to host requests: %w", err)
	}

	n.hostSub = sub
	return nil
}

// retire is what a room calls when it has stood empty long enough to stop.
//
// The directory release comes first and decides the outcome. Releasing the
// instance and then deregistering the room leaves no window in which a player
// can be placed here: after a successful TryRelease nothing can name this
// instance again, and a refusal means somebody already has, so the room keeps
// running.
func (n *Node) retire(inst directory.InstanceID, mapID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	n.mu.Lock()
	defer n.mu.Unlock()

	released, err := n.dir.TryRelease(ctx, inst)
	if err != nil {
		n.log.Error("releasing an idle instance", "instance", uint64(inst), "err", err)
		return false
	}
	if !released {
		// Somebody was placed here while the room was counting down. Their
		// join is on its way.
		return false
	}

	delete(n.rooms, inst)
	n.rooms3.Unregister(inst)
	n.reportInstancesLocked()
	if n.roomGone != nil {
		n.roomGone(mapID, uint64(inst))
	}

	n.log.Info("room instance retired after standing empty",
		"instance", uint64(inst), "map", mapID)
	return true
}

// placeAndJoin puts a character into a map, from placement through to the room
// accepting them.
//
// The retry exists for one narrow race: a room can retire between being handed
// out and being joined. It cannot happen once the directory has reserved a slot
// -- retirement refuses while anyone occupies one -- but a room shutting down
// for any other reason lands here too, and asking the directory again is always
// the right answer.
// onRemote is called when the chosen room turns out to be in another process,
// before anybody joins it. That is the session's cue to subscribe to the
// room's callbacks: the join itself produces the first snapshot, and a
// subscription taken afterwards would miss it.
func (n *Node) placeAndJoin(ctx context.Context, key directory.RoomKey, spec room.JoinSpec, onRemote func()) (
	room.Handle, directory.InstanceID, room.EntityID, error,
) {
	m, ok := n.content.Maps[key.MapID]
	if !ok {
		return nil, 0, 0, fmt.Errorf("%w: %s", ErrUnknownMap, key.MapID)
	}

	const attempts = 2
	for attempt := 0; ; attempt++ {
		inst, err := n.dir.Join(ctx, key, m.Capacity)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("world: placing player: %w", err)
		}

		handle, err := n.hostRoom(ctx, inst, m, spec.CharacterID)
		if err != nil {
			_ = n.dir.Leave(ctx, inst.ID)
			return nil, 0, 0, err
		}

		if _, remote := handle.(*remoteRoom); remote && onRemote != nil {
			onRemote()
		}

		entityID, err := handle.Join(ctx, spec)
		if err == nil {
			return handle, inst.ID, entityID, nil
		}

		_ = n.dir.Leave(ctx, inst.ID)
		if !errors.Is(err, room.ErrRoomClosed) || attempt+1 >= attempts {
			return nil, 0, 0, err
		}
	}
}

// roomKey names the logical room for a map.
//
// A private map is one instance per owner, and the owner is the *party*,
// falling back to the character when unpartied -- the same key that decides
// hostile-entity layering. Keying it by character would give every member of a
// party their own copy of a dungeon and they would never see each other, which
// is not an instance, it is six solo runs.
func roomKey(m *content.Map, ownerKey string) directory.RoomKey {
	key := directory.RoomKey{
		MapID:     m.ID,
		Placement: directory.Placement(m.Placement),
	}
	if key.Placement == directory.PlacementPrivate {
		key.OwnerKey = ownerKey
	}
	return key
}
