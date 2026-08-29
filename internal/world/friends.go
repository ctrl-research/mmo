package world

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ctrl-research/mmo/internal/store"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// Friends.
//
// A durable list crossed with live presence: the store says who is on it, and
// presence says which of them are around. Neither is much use without the
// other, and keeping them apart is what lets the list survive a restart while
// the online flags do not have to.

// SocialActionKind is what a player wants done with their list.
type SocialActionKind uint8

const (
	FriendAdd SocialActionKind = iota + 1
	FriendRemove
	FriendList
)

// SocialRequest is a queued friends-list action.
type SocialRequest struct {
	Kind SocialActionKind

	// Target is a character name.
	Target string
}

// Social queues a friends-list action.
func (s *Session) Social(_ context.Context, req SocialRequest) error {
	select {
	case s.socialReqs <- req:
		return nil
	case <-s.done:
		return errors.New("world: session has ended")
	default:
		return errors.New("world: too many requests")
	}
}

// handleSocialRequest performs one action, on the session goroutine.
func (s *Session) handleSocialRequest(req SocialRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch req.Kind {
	case FriendAdd, FriendRemove:
		if err := s.changeFriend(ctx, req); err != nil {
			s.notify(mmov1.ChatChannel_CHAT_CHANNEL_UNSPECIFIED, err.Error())
			return
		}
	}
	s.sendFriends(ctx)
}

// changeFriend adds or removes one entry.
//
// Resolved against the whole character table rather than presence, because
// adding somebody who happens to be offline is the normal case: a friends list
// is for people you are not currently standing next to.
func (s *Session) changeFriend(ctx context.Context, req SocialRequest) error {
	name := strings.TrimSpace(req.Target)

	who, err := s.node.store.CharacterByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("there is no character called %s", name)
	}
	if err != nil {
		return errors.New("could not look that name up")
	}
	if who.ID == s.characterID {
		return errors.New("you are already with yourself")
	}

	if req.Kind == FriendRemove {
		return s.node.store.RemoveFriend(ctx, s.characterID, who.ID)
	}

	if err := s.node.store.AddFriend(ctx, s.characterID, who.ID); err != nil {
		if errors.Is(err, store.ErrFriendLimit) {
			return fmt.Errorf("your friends list is full at %d", store.MaxFriends)
		}
		return errors.New("could not add that friend")
	}
	return nil
}

// sendFriends sends the whole list with live online status.
func (s *Session) sendFriends(ctx context.Context) {
	list, err := s.node.store.Friends(ctx, s.characterID)
	if err != nil {
		s.log.Error("reading friends", "err", err)
		return
	}

	out := &mmov1.FriendList{}
	for _, f := range list {
		entry := &mmov1.FriendEntry{
			CharacterId: f.CharacterID.String(),
			Name:        f.Name,
			Level:       uint32(f.Level),
		}
		// Presence is the live half. An away character -- inside their
		// reconnect window -- reads as offline, because from a friend's point
		// of view that is what they are.
		if s.node.presence != nil {
			if who, ok := s.node.presence.ByID(ctx, f.CharacterID.String()); ok && !who.Away {
				entry.Online = true
				entry.MapId = who.MapID
			}
		}
		out.Friends = append(out.Friends, entry)
	}

	s.deliver(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_Friends{Friends: out},
		}},
	})
}
