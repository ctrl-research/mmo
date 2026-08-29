package room

import (
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// Local chat.
//
// The one channel that never touches the bus: everyone who can hear it is
// already in this room, so sending it anywhere else would be routing a message
// out of the process and back in. Every other channel crosses node boundaries
// and belongs to the session, not the room.

// say delivers a local line to everyone in the room.
//
// Including the speaker: a client that renders its own message optimistically
// and then receives the real one shows it twice, and one that never receives
// it cannot tell a delivered message from a dropped one.
func (r *Room) say(from EntityID, body string, at int64) {
	p, ok := r.players[from]
	if !ok {
		return
	}

	r.broadcastExcept(0, &mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_Chat{Chat: &mmov1.ChatLine{
				Channel:      mmov1.ChatChannel_CHAT_CHANNEL_LOCAL,
				From:         p.entity.Name,
				Body:         body,
				ServerTimeMs: at,
			}},
		}},
	})
}
