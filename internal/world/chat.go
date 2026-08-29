package world

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// Chat.
//
// Five channels that differ in who hears you, not in what you can say. Exactly
// one of them -- local -- never leaves the room, because everyone who can hear
// it is already there. The rest cross node boundaries and so go over the bus,
// even when the recipient turns out to be sitting on this same node: the
// shortcut works today and means the distributed path is never exercised
// (AGENTS.md invariant 1).
//
// Routing is the interesting part, and it is deliberately two shapes rather
// than one:
//
//   - Broadcast channels have a subject and every interested node subscribes:
//     chat.global for everyone, party.{id} for a party. The publisher does not
//     know or care who is listening, which is what makes the NATS
//     implementation a direct mapping.
//   - A whisper names one character, so it takes a presence lookup to find the
//     node holding them and is addressed to that node.
//
// The difference is not an inconsistency. A whisper genuinely has one
// recipient, and broadcasting it to every node so that one of them keeps it
// would leak private messages across the cluster.

const (
	// subjectGlobalChat reaches every node.
	subjectGlobalChat = "chat.global"
)

// chatSubject is where a node listens for chat addressed to its characters.
func chatSubject(nodeID string) string {
	return "chat.node." + sanitiseSubject(nodeID)
}

// partySubject is one party's channel: chat, roster changes, and vitals.
func partySubject(id directory.PartyID) string {
	return "party." + sanitiseSubject(string(id))
}

// serveChat subscribes to the channels this node must always hear.
func (n *Node) serveChat(ctx context.Context) error {
	global, err := n.bus.Subscribe(ctx, subjectGlobalChat, n.onChatDelivery)
	if err != nil {
		return fmt.Errorf("world: subscribing to global chat: %w", err)
	}

	direct, err := n.bus.Subscribe(ctx, chatSubject(n.nodeID), n.onChatDelivery)
	if err != nil {
		global.Close()
		return fmt.Errorf("world: subscribing to directed chat: %w", err)
	}

	n.chatSubs = append(n.chatSubs, global, direct)
	return nil
}

// onChatDelivery hands an incoming line to whichever local sessions should
// hear it.
func (n *Node) onChatDelivery(_ context.Context, _ string, payload []byte) {
	var d mmov1.ChatDelivery
	if err := proto.Unmarshal(payload, &d); err != nil {
		n.log.Error("decoding a chat delivery", "err", err)
		return
	}
	n.fanOutChat(&d)
}

// fanOutChat delivers a line to the local sessions it is addressed to.
func (n *Node) fanOutChat(d *mmov1.ChatDelivery) {
	// A party invitation travels the same directed path a whisper does -- one
	// character, on another node -- so it arrives here and is unwrapped before
	// anything can mistake it for something said.
	if from, ok := strings.CutPrefix(d.GetBody(), invitePrefix); ok {
		n.fanOutInvite(d.GetToCharacterId(), from)
		return
	}
	if rest, ok := strings.CutPrefix(d.GetBody(), guildInvitePrefix); ok {
		n.fanOutGuildInvite(d.GetToCharacterId(), rest)
		return
	}

	line := &mmov1.ChatLine{
		Channel:      mmov1.ChatChannel(d.GetChannel()),
		From:         d.GetFromName(),
		Body:         d.GetBody(),
		ServerTimeMs: d.GetServerTimeMs(),
	}

	for _, s := range n.localSessions() {
		id := s.characterID.String()

		// A whisper reaches exactly one character. The sender's own copy is
		// produced locally, so this is only ever the recipient.
		if to := d.GetToCharacterId(); to != "" && to != id {
			continue
		}
		// Nobody needs their own broadcast twice: the sender already has it.
		if d.GetToCharacterId() == "" && d.GetFromCharacterId() == id {
			continue
		}
		s.deliver(&mmov1.ServerMessage{
			Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
				Body: &mmov1.Event_Chat{Chat: line},
			}},
		})
	}
}

// SendChat queues a chat message. It returns as soon as the request is
// accepted, not when it is delivered.
//
// Queued for the same reason travel is: the work involves a database read for
// the mute check, a presence lookup, and a publish, and the goroutine calling
// this is the one reading the socket.
func (s *Session) SendChat(_ context.Context, req *mmov1.ChatSend) error {
	select {
	case s.chats <- req:
		return nil
	case <-s.done:
		return fmt.Errorf("world: session has ended")
	default:
		// The queue is full, which at these rate limits means a client that is
		// not respecting them. Dropping is the right answer; so is saying so.
		return errChatFlood
	}
}

var errChatFlood = fmt.Errorf("world: too much chat")

// handleChat validates and routes one message, on the session goroutine.
func (s *Session) handleChat(req *mmov1.ChatSend) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	body, err := s.validateChat(ctx, req)
	if err != nil {
		s.notify(req.GetChannel(), err.Error())
		return
	}

	at := time.Now().UnixMilli()

	switch req.GetChannel() {
	case mmov1.ChatChannel_CHAT_CHANNEL_LOCAL:
		// The one channel that never touches the bus. The room delivers it to
		// everyone present, including the speaker.
		handle, entityID := s.Where()
		handle.Say(ctx, entityID, body, at)

	case mmov1.ChatChannel_CHAT_CHANNEL_GLOBAL:
		s.publishChat(ctx, subjectGlobalChat, req.GetChannel(), "", body, at)
		// The publisher skips itself when fanning out, so the sender's copy is
		// produced here rather than travelling to every node and back.
		s.echo(req.GetChannel(), s.name, body, at, false)

	case mmov1.ChatChannel_CHAT_CHANNEL_WHISPER:
		s.whisper(ctx, req.GetTarget(), body, at)

	case mmov1.ChatChannel_CHAT_CHANNEL_GUILD:
		id := s.GuildID()
		if id == uuid.Nil {
			s.notify(req.GetChannel(), "you are not in a guild")
			return
		}
		s.publishChat(ctx, guildChatSubject(id), req.GetChannel(), "", body, at)
		s.echo(req.GetChannel(), s.name, body, at, false)

	case mmov1.ChatChannel_CHAT_CHANNEL_PARTY:
		party, ok := s.node.parties.Of(ctx, s.characterID.String())
		if !ok {
			s.notify(req.GetChannel(), "you are not in a party")
			return
		}
		s.publishChat(ctx, partyChatSubject(party.ID), req.GetChannel(), "", body, at)
		s.echo(req.GetChannel(), s.name, body, at, false)

	default:
		s.notify(req.GetChannel(), "that channel is not available yet")
	}
}

// validateChat checks a message against length, mutes, and the rate limit.
//
// In that order on purpose: length is free, the mute check is a database read,
// and the rate limit consumes a token that a rejected message should not.
func (s *Session) validateChat(ctx context.Context, req *mmov1.ChatSend) (string, error) {
	body := strings.TrimSpace(req.GetBody())
	if body == "" {
		return "", fmt.Errorf("say something first")
	}

	limits := s.node.content.Balance.Chat
	if utf8.RuneCountInString(body) > limits.MaxLength {
		return "", fmt.Errorf("that is longer than %d characters", limits.MaxLength)
	}

	// Control characters would let a message forge line breaks and impersonate
	// the server's own notices in a client that renders text verbatim.
	body = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, body)

	if mute, muted, err := s.mute(ctx); err != nil {
		s.log.Error("checking mute", "err", err)
	} else if muted {
		return "", fmt.Errorf("you cannot use chat%s", muteSuffix(mute))
	}

	if !s.chatAllowed(req.GetChannel()) {
		return "", fmt.Errorf("slow down")
	}
	return body, nil
}

// muteSuffix turns a mute into the tail of a sentence explaining it.
func muteSuffix(mute chatMute) string {
	var b strings.Builder
	if !mute.until.IsZero() {
		b.WriteString(" until ")
		b.WriteString(mute.until.UTC().Format("15:04 on 2 Jan"))
	}
	if mute.reason != "" {
		b.WriteString(" (")
		b.WriteString(mute.reason)
		b.WriteString(")")
	}
	return b.String()
}

// whisper delivers a message to one named character.
func (s *Session) whisper(ctx context.Context, target, body string, at int64) {
	if s.node.presence == nil {
		s.notify(mmov1.ChatChannel_CHAT_CHANNEL_WHISPER, "whispers are not available")
		return
	}
	if directory.NormaliseName(target) == directory.NormaliseName(s.name) {
		s.notify(mmov1.ChatChannel_CHAT_CHANNEL_WHISPER, "you cannot whisper yourself")
		return
	}

	who, ok := s.node.presence.ByName(ctx, target)
	if ok && who.Away {
		// Delivering to a session whose socket has gone would drop the message
		// silently, and a whisper that vanishes is worse than one refused.
		s.notify(mmov1.ChatChannel_CHAT_CHANNEL_WHISPER,
			fmt.Sprintf("%s is away", who.Name))
		return
	}
	if !ok {
		// Naming who was not found, since the usual cause is a typo. This does
		// leak whether a character is online, which is what a friends list
		// shows anyway.
		s.notify(mmov1.ChatChannel_CHAT_CHANNEL_WHISPER,
			fmt.Sprintf("%s is not online", strings.TrimSpace(target)))
		return
	}

	// Addressed to the node holding them rather than broadcast: a whisper has
	// one recipient, and publishing it everywhere so that one node keeps it
	// would spread private messages across the cluster.
	s.publishChat(ctx, chatSubject(string(who.Node)),
		mmov1.ChatChannel_CHAT_CHANNEL_WHISPER, who.CharacterID, body, at)

	// The sender's copy names the recipient rather than the speaker, so the
	// two halves of a conversation read differently.
	s.echo(mmov1.ChatChannel_CHAT_CHANNEL_WHISPER, who.Name, body, at, true)
}

// publishChat puts a line on the bus.
func (s *Session) publishChat(ctx context.Context, subject string,
	channel mmov1.ChatChannel, toCharacterID, body string, at int64,
) {
	err := s.node.bus.Publish(ctx, subject, &mmov1.ChatDelivery{
		Channel:         uint32(channel),
		FromCharacterId: s.characterID.String(),
		FromName:        s.name,
		Body:            body,
		ServerTimeMs:    at,
		ToCharacterId:   toCharacterID,
	})
	if err != nil {
		s.log.Error("publishing chat", "subject", subject, "err", err)
		s.notify(channel, "that did not get through")
	}
}

// echo sends the speaker their own copy of what they said.
func (s *Session) echo(channel mmov1.ChatChannel, from, body string, at int64, outgoing bool) {
	s.deliver(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_Chat{Chat: &mmov1.ChatLine{
				Channel:      channel,
				From:         from,
				Body:         body,
				Outgoing:     outgoing,
				ServerTimeMs: at,
			}},
		}},
	})
}

// notify tells one player something the server decided.
//
// Every refusal gets one. A chat message that vanishes without explanation
// reads as the game being broken, and most of the reasons -- a typo in a name,
// a rate limit, a mute -- are things the player can act on.
func (s *Session) notify(channel mmov1.ChatChannel, body string) {
	s.deliver(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_System{System: &mmov1.SystemMessage{
				Body: body, Channel: channel,
			}},
		}},
	})
}

// deliver writes to the player's connection, if they still have one.
func (s *Session) deliver(msg *mmov1.ServerMessage) {
	s.mu.Lock()
	sink := s.sink
	s.mu.Unlock()

	if sink != nil {
		sink.Send(msg)
	}
}

// fanOutInvite delivers a party invitation to its recipient.
func (n *Node) fanOutInvite(toCharacterID, fromName string) {
	for _, s := range n.localSessions() {
		if s.characterID.String() != toCharacterID {
			continue
		}
		s.deliver(&mmov1.ServerMessage{
			Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
				Body: &mmov1.Event_PartyInvite{PartyInvite: &mmov1.PartyInvite{
					FromName:    fromName,
					ExpiresInMs: directory.InviteTTL.Milliseconds(),
				}},
			}},
		})
	}
}

// fanOutGuildInvite delivers a guild invitation to its recipient.
//
// The payload is the guild id, its name, and the inviter, packed into the body
// of a directed delivery. Reusing that path rather than adding a second
// directed-message type: an invitation is one character on another node, which
// is exactly what it already does.
func (n *Node) fanOutGuildInvite(toCharacterID, packed string) {
	parts := strings.SplitN(packed, "\x00", 3)
	if len(parts) != 3 {
		return
	}
	id, err := uuid.Parse(parts[0])
	if err != nil {
		return
	}

	for _, s := range n.localSessions() {
		if s.characterID.String() != toCharacterID {
			continue
		}

		s.mu.Lock()
		s.guildInvite = id
		s.guildInviteExpires = time.Now().Add(GuildInviteTTL)
		s.mu.Unlock()

		s.deliver(&mmov1.ServerMessage{
			Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
				Body: &mmov1.Event_GuildInvite{GuildInvite: &mmov1.GuildInvite{
					GuildName:   parts[1],
					FromName:    parts[2],
					ExpiresInMs: GuildInviteTTL.Milliseconds(),
				}},
			}},
		})
	}
}
