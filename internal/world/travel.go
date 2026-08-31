package world

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// Moving without walking: fast travel and channel switching.
//
// Both are the room handoff protocol with a different destination, which is
// the reason they are three dozen lines rather than a subsystem each. A
// waypoint is a portal whose exit the player chose; a channel switch is a
// portal that leads back to the same map.

// Travel errors, all reported to the player rather than logged and swallowed.
var (
	ErrWaypointLocked = errors.New("world: waypoint not unlocked")
	ErrUnknownChannel = errors.New("world: no such channel")
	ErrSameChannel    = errors.New("world: already in that channel")
	ErrNotHere        = errors.New("world: that channel is on a different map")
)

// TravelRequest names a destination. Exactly one field is set.
type TravelRequest struct {
	WaypointID string
	Channel    directory.InstanceID

	// NewChannel asks for any channel but the current one, creating it if
	// every existing channel is one the player is already in.
	NewChannel bool
}

// ErrTravelBusy means a transfer for this character is already in flight.
var ErrTravelBusy = errors.New("world: already travelling")

// Travel queues a move. It returns as soon as the request is accepted, not
// when the character arrives.
//
// Queued rather than performed here because the work is a database write and a
// bus round trip. Running it on the session's own goroutine, the one that
// already handles portals, is what stops a client that spams the button from
// starting several transfers of the same character at once -- which is exactly
// how a character ends up in two rooms.
func (s *Session) Travel(_ context.Context, req TravelRequest) error {
	if req.WaypointID == "" && req.Channel == 0 && !req.NewChannel {
		return errors.New("world: travel names no destination")
	}

	select {
	case s.travels <- req:
		return nil
	case <-s.done:
		return errors.New("world: session has ended")
	default:
		return ErrTravelBusy
	}
}

// handleTravel performs one queued move, on the session goroutine.
func (s *Session) handleTravel(req TravelRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), TransferTimeout)
	defer cancel()

	var err error
	switch {
	case req.WaypointID != "":
		err = s.travelToWaypoint(ctx, req.WaypointID)
	case req.Channel != 0:
		err = s.switchChannel(ctx, req.Channel)
	case req.NewChannel:
		err = s.switchToAnyChannel(ctx)
	}
	if err == nil {
		return
	}

	s.log.Info("travel refused", "err", err)
	s.refuse(TravelMessage(err))
}

// refuse tells the client why they did not go anywhere.
//
// A travel request that silently does nothing reads as a broken button, and
// most of the reasons -- a full channel, a waypoint not yet found -- are
// things a player does by accident rather than bugs.
func (s *Session) refuse(reason string) {
	s.mu.Lock()
	sink := s.sink
	s.mu.Unlock()

	if sink == nil {
		return
	}
	sink.Send(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_PortalRefused{
				PortalRefused: &mmov1.PortalRefused{Reason: reason},
			},
		}},
	})
}

// travelToWaypoint moves the character to a waypoint they have unlocked.
func (s *Session) travelToWaypoint(ctx context.Context, waypointID string) error {
	w, ok := s.node.content.Waypoints[waypointID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownMap, waypointID)
	}

	// Checked against the database, not against what the room believes: the
	// room's copy exists to avoid a write per tick, and a client naming a
	// waypoint it has never seen must not be taken at its word.
	unlocked, err := s.node.store.CharacterWaypoints(ctx, s.characterID)
	if err != nil {
		return fmt.Errorf("world: checking unlocked waypoints: %w", err)
	}
	found := false
	for _, id := range unlocked {
		if id == waypointID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrWaypointLocked, waypointID)
	}

	target := s.node.content.Maps[w.MapID]
	if target == nil {
		return fmt.Errorf("%w: %s", ErrUnknownMap, w.MapID)
	}

	return s.transfer(ctx, target, arrival{waypoint: waypointID},
		func(ctx context.Context) (directory.Instance, error) {
			return s.node.dir.Join(ctx,
				roomKey(target, s.layerKey()), target.Capacity)
		})
}

// switchChannel moves the character to another instance of the map they are
// already in, keeping their position.
func (s *Session) switchChannel(ctx context.Context, target directory.InstanceID) error {
	current := s.MapID()
	if target == s.Instance() {
		return ErrSameChannel
	}

	m := s.node.content.Maps[current]
	if m == nil {
		return fmt.Errorf("%w: %s", ErrUnknownMap, current)
	}

	inst, ok, err := s.node.dir.Lookup(ctx, target)
	if err != nil {
		// An unreachable directory is not "no such channel". Refusing with the
		// real reason beats telling a player their channel does not exist when
		// it is running fine.
		return fmt.Errorf("looking up channel %d: %w", target, err)
	}
	if !ok {
		return fmt.Errorf("%w: %d", ErrUnknownChannel, target)
	}
	// A client may name any instance id, and the ones on other maps are real.
	// Without this, "switch channel" is a teleport to anywhere in the world.
	if inst.Key.MapID != current {
		return fmt.Errorf("%w: %d is %s", ErrNotHere, target, inst.Key.MapID)
	}

	return s.transfer(ctx, m, arrival{keepPosition: true},
		func(ctx context.Context) (directory.Instance, error) {
			return s.node.dir.JoinInstance(ctx, target)
		})
}

// switchToAnyChannel moves the character to some other channel of the map they
// are in, creating one if every live channel is the one they are already in.
func (s *Session) switchToAnyChannel(ctx context.Context) error {
	current, currentInstance := s.MapID(), s.Instance()

	m := s.node.content.Maps[current]
	if m == nil {
		return fmt.Errorf("%w: %s", ErrUnknownMap, current)
	}
	if directory.Placement(m.Placement) != directory.PlacementShared {
		// A dungeon has one instance by definition; there is nothing to switch
		// to, and silently doing nothing would read as a broken button.
		return ErrUnknownChannel
	}
	key := roomKey(m, "")

	return s.transfer(ctx, m, arrival{keepPosition: true},
		func(ctx context.Context) (directory.Instance, error) {
			existing, err := s.node.dir.InstancesFor(ctx, key)
			if err != nil {
				return directory.Instance{}, err
			}
			for _, inst := range existing {
				if inst.ID == currentInstance || inst.Full() {
					continue
				}
				if got, err := s.node.dir.JoinInstance(ctx, inst.ID); err == nil {
					return got, nil
				}
				// Filled up between the listing and the join. Try the next.
			}
			return s.node.dir.NewInstance(ctx, key, m.Capacity)
		})
}

// Instance returns the room instance the character is in.
func (s *Session) Instance() directory.InstanceID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instance
}

// WorldMap describes where the character can go and where they are.
func (s *Session) WorldMap(ctx context.Context) *mmov1.WorldMap {
	currentMap, currentInstance := s.MapID(), s.Instance()

	out := &mmov1.WorldMap{
		CurrentMapId:      currentMap,
		CurrentInstanceId: uint64(currentInstance),
	}

	// Every map, so a player can see where they are meant to go next. Sorted
	// by level so the list reads as a progression rather than as hash order.
	for _, m := range s.node.content.Maps {
		out.Maps = append(out.Maps, &mmov1.MapSummary{
			MapId:    m.ID,
			Name:     m.DisplayName,
			MinLevel: int32(m.MinLevel),
			MaxLevel: int32(m.MaxLevel),
			Private:  directory.Placement(m.Placement) == directory.PlacementPrivate,
		})
	}
	sort.Slice(out.Maps, func(i, j int) bool {
		if out.Maps[i].GetMinLevel() != out.Maps[j].GetMinLevel() {
			return out.Maps[i].GetMinLevel() < out.Maps[j].GetMinLevel()
		}
		return out.Maps[i].GetMapId() < out.Maps[j].GetMapId()
	})

	// Only unlocked waypoints. The world map is a record of where a player has
	// been, and listing the rest would give away what they have not found.
	unlocked, err := s.node.store.CharacterWaypoints(ctx, s.characterID)
	if err != nil {
		s.log.Error("loading waypoints for the world map", "err", err)
	}
	for _, id := range unlocked {
		w, ok := s.node.content.Waypoints[id]
		if !ok {
			// Content changed under a save. Skipping beats showing a
			// destination that no longer exists.
			continue
		}
		out.Waypoints = append(out.Waypoints, &mmov1.WaypointSummary{
			WaypointId: id,
			Name:       w.Name,
			MapId:      w.MapID,
		})
	}

	// Channels, for the map the player is in. A private map has exactly one
	// instance by definition, so there is nothing to switch between.
	if m := s.node.content.Maps[currentMap]; m != nil &&
		directory.Placement(m.Placement) == directory.PlacementShared {

		channels, err := s.node.dir.InstancesFor(ctx, roomKey(m, ""))
		if err != nil {
			// The world map is worth showing without its channel list: fast
			// travel still works, and a blank panel is worse than a partial
			// one. Logged rather than returned for that reason.
			s.log.Warn("listing channels for the world map", "map", currentMap, "err", err)
		}
		for i, inst := range channels {
			out.Channels = append(out.Channels, &mmov1.ChannelSummary{
				InstanceId: uint64(inst.ID),
				// The position in the list, not the instance id: ids are
				// never reused and grow forever, which makes a poor label.
				Channel:  uint32(i + 1),
				Players:  uint32(inst.Players),
				Capacity: uint32(inst.Capacity),
				Current:  inst.ID == currentInstance,
			})
		}
	}

	return out
}
