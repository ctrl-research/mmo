package world

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/store"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// Guilds.
//
// The same delivery shape as parties -- a subject per guild, and every node
// holding a member subscribed to it -- over durable state instead of ephemeral.
// That difference is the whole reason a guild is in Postgres and a party is
// not: losing a party costs a regroup, and losing a guild would cost months.
//
// The one place the shapes diverge is the roster. A party is at most six
// members and travels whole; a guild can have hundreds, so an update says what
// changed and each node reloads the roster it actually needs.

// GuildInviteTTL is how long a guild invitation stands.
//
// Longer than a party's: joining a guild is a decision rather than a reflex,
// and the invitation is worth reading before answering.
const GuildInviteTTL = 5 * time.Minute

// GuildActionKind is what a player wants done.
type GuildActionKind uint8

const (
	GuildCreate GuildActionKind = iota + 1
	GuildInvite
	GuildAccept
	GuildDecline
	GuildLeave
	GuildKick
	GuildPromote
	GuildDemote
	GuildSetMOTD
)

// GuildRequest is a queued guild action.
type GuildRequest struct {
	Kind GuildActionKind

	// Target is a guild name for create, a character name for the membership
	// actions, and the message itself for set-motd.
	Target string
}

// MaxMOTDLength bounds a message of the day. Generous, because it is a notice
// board rather than a chat line, and bounded because it is stored and shown to
// everybody who logs in.
const MaxMOTDLength = 500

func guildSubject(id uuid.UUID) string      { return "guild." + sanitiseSubject(id.String()) }
func guildChatSubject(id uuid.UUID) string  { return guildSubject(id) + ".chat" }
func guildStateSubject(id uuid.UUID) string { return guildSubject(id) + ".state" }
func guildPattern(id uuid.UUID) string      { return guildSubject(id) + ".*" }

// Guild queues a guild action.
func (s *Session) Guild(_ context.Context, req GuildRequest) error {
	select {
	case s.guildReqs <- req:
		return nil
	case <-s.done:
		return errors.New("world: session has ended")
	default:
		return errors.New("world: too many guild requests")
	}
}

// handleGuildRequest performs one action, on the session goroutine.
func (s *Session) handleGuildRequest(req GuildRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	notice, err := s.applyGuildAction(ctx, req)
	if err != nil {
		s.notify(mmov1.ChatChannel_CHAT_CHANNEL_GUILD, guildMessage(err))
		return
	}

	s.syncGuildMembership(ctx)
	if notice != "" {
		s.announceGuild(ctx, notice, req.Kind == GuildLeave)
	}
	s.sendGuildState(ctx)
}

// applyGuildAction performs one change and returns what to tell the guild.
func (s *Session) applyGuildAction(ctx context.Context, req GuildRequest) (string, error) {
	me := s.characterID

	switch req.Kind {
	case GuildCreate:
		name := strings.TrimSpace(req.Target)
		if err := store.ValidateName(name); err != nil {
			// Guild names follow the same rules as character names, so the two
			// cannot be confused in a chat line.
			return "", err
		}
		g, err := s.node.store.CreateGuild(ctx, me, name)
		if err != nil {
			return "", err
		}
		// Deliberately not recording the guild here. syncGuildMembership is
		// the only thing that sets it, because it is also the only thing that
		// takes out the subscription -- and setting the field first makes it
		// see no change and subscribe to nothing.
		return s.name + " founded " + g.Name, nil

	case GuildInvite:
		return "", s.inviteToGuild(ctx, req.Target)

	case GuildAccept:
		return s.acceptGuildInvite(ctx)

	case GuildDecline:
		s.clearGuildInvite()
		return "", nil

	case GuildLeave:
		g, _, err := s.node.store.GuildOf(ctx, me)
		if err != nil {
			return "", err
		}
		if _, err := s.node.store.RemoveGuildMember(ctx, g.ID, me); err != nil {
			return "", err
		}
		// Remembered so the departure can be announced to a subject this
		// session is about to unsubscribe from.
		s.mu.Lock()
		s.leftGuild = g.ID
		s.mu.Unlock()
		return s.name + " left the guild", nil

	case GuildKick:
		return s.kickFromGuild(ctx, req.Target)

	case GuildPromote:
		return s.setGuildRank(ctx, req.Target, store.RankOfficer)

	case GuildDemote:
		return s.setGuildRank(ctx, req.Target, store.RankMember)

	case GuildSetMOTD:
		g, rank, err := s.node.store.GuildOf(ctx, me)
		if err != nil {
			return "", err
		}
		if rank < store.RankOfficer {
			return "", store.ErrGuildRank
		}
		motd := strings.TrimSpace(req.Target)
		if len(motd) > MaxMOTDLength {
			motd = motd[:MaxMOTDLength]
		}
		if err := s.node.store.SetGuildMOTD(ctx, g.ID, motd); err != nil {
			return "", err
		}
		return s.name + " set the message of the day", nil
	}
	return "", errors.New("world: unknown guild action")
}

// inviteToGuild asks a named character to join.
func (s *Session) inviteToGuild(ctx context.Context, targetName string) error {
	g, rank, err := s.node.store.GuildOf(ctx, s.characterID)
	if err != nil {
		return err
	}
	if rank < store.RankOfficer {
		return store.ErrGuildRank
	}
	if s.node.presence == nil {
		return errors.New("world: presence is not available")
	}

	who, ok := s.node.presence.ByName(ctx, targetName)
	if !ok {
		return fmt.Errorf("%s is not online", strings.TrimSpace(targetName))
	}

	// Addressed to the node holding them: they are not a member yet, so their
	// node is not subscribed to this guild.
	return s.node.bus.Publish(ctx, chatSubject(string(who.Node)), &mmov1.ChatDelivery{
		Channel:         uint32(mmov1.ChatChannel_CHAT_CHANNEL_GUILD),
		FromCharacterId: s.characterID.String(),
		FromName:        s.name,
		ToCharacterId:   who.CharacterID,
		Body:            guildInvitePrefix + g.ID.String() + "\x00" + g.Name + "\x00" + s.name,
		ServerTimeMs:    time.Now().UnixMilli(),
	})
}

// guildInvitePrefix marks a chat delivery that is really an invitation.
const guildInvitePrefix = "\x00guild-invite\x00"

// acceptGuildInvite joins the guild this character was last invited to.
func (s *Session) acceptGuildInvite(ctx context.Context) (string, error) {
	s.mu.Lock()
	invite := s.guildInvite
	expires := s.guildInviteExpires
	s.mu.Unlock()

	if invite == uuid.Nil || time.Now().After(expires) {
		s.clearGuildInvite()
		return "", errors.New("that invitation has expired")
	}

	if err := s.node.store.AddGuildMember(ctx, invite, s.characterID); err != nil {
		return "", err
	}
	s.clearGuildInvite()
	return s.name + " joined the guild", nil
}

// kickFromGuild removes a member of lower rank.
func (s *Session) kickFromGuild(ctx context.Context, targetName string) (string, error) {
	g, rank, target, targetRank, err := s.guildTarget(ctx, targetName)
	if err != nil {
		return "", err
	}
	// Only above your own rank, so an officer cannot remove the leader or
	// another officer, and nobody can remove themselves this way.
	if rank < store.RankOfficer || targetRank >= rank {
		return "", store.ErrGuildRank
	}
	if _, err := s.node.store.RemoveGuildMember(ctx, g.ID, target.CharacterID); err != nil {
		return "", err
	}
	return target.Name + " was removed from the guild", nil
}

// setGuildRank promotes or demotes a member.
func (s *Session) setGuildRank(ctx context.Context, targetName string, to int) (string, error) {
	g, rank, target, targetRank, err := s.guildTarget(ctx, targetName)
	if err != nil {
		return "", err
	}
	// Only the leader changes ranks. An officer able to make officers is an
	// officer able to hand the guild to anybody.
	if rank < store.RankLeader || targetRank >= rank {
		return "", store.ErrGuildRank
	}
	if err := s.node.store.SetGuildRank(ctx, g.ID, target.CharacterID, to); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s is now a %s", target.Name, store.RankName(to)), nil
}

// guildTarget resolves a name against this character's own guild roster.
func (s *Session) guildTarget(ctx context.Context, name string) (
	store.Guild, int, store.GuildMember, int, error,
) {
	g, rank, err := s.node.store.GuildOf(ctx, s.characterID)
	if err != nil {
		return store.Guild{}, 0, store.GuildMember{}, 0, err
	}

	roster, err := s.node.store.GuildRoster(ctx, g.ID)
	if err != nil {
		return store.Guild{}, 0, store.GuildMember{}, 0, err
	}

	want := directory.NormaliseName(name)
	for _, m := range roster {
		if directory.NormaliseName(m.Name) == want {
			return g, rank, m, m.Rank, nil
		}
	}
	return store.Guild{}, 0, store.GuildMember{}, 0,
		fmt.Errorf("%s is not in your guild", strings.TrimSpace(name))
}

// GuildID returns the guild this character belongs to, if any.
func (s *Session) GuildID() uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.guildID
}

func (s *Session) clearGuildInvite() {
	s.mu.Lock()
	s.guildInvite = uuid.Nil
	s.mu.Unlock()
}

// syncGuildMembership reconciles the guild subscription with the database.
func (s *Session) syncGuildMembership(ctx context.Context) {
	var id uuid.UUID
	if g, _, err := s.node.store.GuildOf(ctx, s.characterID); err == nil {
		id = g.ID
	}

	s.mu.Lock()
	previous := s.guildID
	s.guildID = id
	s.mu.Unlock()

	if previous == id {
		return
	}
	if previous != uuid.Nil {
		s.node.unwatch(guildPattern(previous))
	}
	if id != uuid.Nil {
		if err := s.node.watch(guildPattern(id), s.node.onGuildMessage); err != nil {
			s.log.Error("subscribing to a guild", "guild", id, "err", err)
		}
	}
}

// announceGuild tells every node holding a member that something changed.
func (s *Session) announceGuild(ctx context.Context, notice string, afterLeaving bool) {
	// After leaving, this session is no longer subscribed, so the guild it is
	// telling is the one it just left.
	id := s.GuildID()
	if afterLeaving {
		s.mu.Lock()
		id = s.leftGuild
		s.mu.Unlock()
	}
	if id == uuid.Nil {
		return
	}

	err := s.node.bus.Publish(ctx, guildStateSubject(id), &mmov1.GuildUpdate{
		GuildId: id.String(),
		Notice:  notice,
	})
	if err != nil {
		s.log.Error("publishing a guild update", "err", err)
	}
}

// sendGuildState sends this player the whole guild.
func (s *Session) sendGuildState(ctx context.Context) {
	g, rank, err := s.node.store.GuildOf(ctx, s.characterID)
	if errors.Is(err, store.ErrNotInGuild) {
		// An empty guild id is how a client learns it is in no guild.
		s.deliver(&mmov1.ServerMessage{
			Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
				Body: &mmov1.Event_Guild{Guild: &mmov1.GuildState{}},
			}},
		})
		return
	}
	if err != nil {
		s.log.Error("reading guild", "err", err)
		return
	}

	roster, err := s.node.store.GuildRoster(ctx, g.ID)
	if err != nil {
		s.log.Error("reading guild roster", "err", err)
		return
	}

	state := &mmov1.GuildState{
		GuildId: g.ID.String(),
		Name:    g.Name,
		Motd:    g.MOTD,
		Rank:    uint32(rank),
	}
	for _, m := range roster {
		online := false
		if s.node.presence != nil {
			_, online = s.node.presence.ByID(ctx, m.CharacterID.String())
		}
		state.Members = append(state.Members, &mmov1.GuildMember{
			CharacterId: m.CharacterID.String(),
			Name:        m.Name,
			Rank:        uint32(m.Rank),
			Level:       uint32(m.Level),
			Online:      online,
		})
	}

	s.deliver(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_Guild{Guild: state},
		}},
	})
}

// onGuildMessage routes anything arriving on a guild's subjects.
func (n *Node) onGuildMessage(_ context.Context, subject string, payload []byte) {
	switch {
	case strings.HasSuffix(subject, ".chat"):
		var d mmov1.ChatDelivery
		if err := proto.Unmarshal(payload, &d); err != nil {
			n.log.Error("decoding guild chat", "err", err)
			return
		}
		n.fanOutChat(&d)

	case strings.HasSuffix(subject, ".state"):
		var u mmov1.GuildUpdate
		if err := proto.Unmarshal(payload, &u); err != nil {
			n.log.Error("decoding a guild update", "err", err)
			return
		}
		n.fanOutGuildUpdate(&u)
	}
}

// fanOutGuildUpdate tells the local members to reload.
//
// Reload rather than apply: unlike a party, a guild roster can be hundreds of
// rows, and shipping it on every join would be a lot of bytes for something
// each node can read from the database it is already talking to.
func (n *Node) fanOutGuildUpdate(u *mmov1.GuildUpdate) {
	id, err := uuid.Parse(u.GetGuildId())
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, s := range n.localSessions() {
		if s.GuildID() != id {
			continue
		}
		if u.GetNotice() != "" {
			s.notify(mmov1.ChatChannel_CHAT_CHANNEL_GUILD, u.GetNotice())
		}
		s.sendGuildState(ctx)
	}
}

// guildMessage turns a guild failure into something worth showing.
func guildMessage(err error) string {
	switch {
	case errors.Is(err, store.ErrGuildNameTaken):
		return "that guild name is taken"
	case errors.Is(err, store.ErrNotInGuild):
		return "you are not in a guild"
	case errors.Is(err, store.ErrAlreadyInGuild):
		return "already in a guild"
	case errors.Is(err, store.ErrGuildRank):
		return "your rank does not allow that"
	default:
		return err.Error()
	}
}
