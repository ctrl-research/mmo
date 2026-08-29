package world

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"google.golang.org/protobuf/proto"
)

// Parties.
//
// A party spans rooms and nodes, which is most of what makes one worth having,
// so none of its state lives in a room. Membership is the directory's; the
// session's job is to ask for changes, tell the members what happened, and
// apply the one consequence a room does care about -- the layer key.
//
// That last part is the whole reason parties touch the simulation at all.
// Hostile entities belong to a layer keyed by party ID, or by character ID
// when unpartied, so partying up merges the members' mob populations and
// leaving splits them again. It is the same code path with a different key.

// PartyVitalsInterval is how often a member's health is pushed to the party.
//
// Once a second: a member frame for somebody in another room has to come from
// somewhere, and it is the only part of a party that changes continuously.
// Folding it into the roster broadcast would mean a full roster every second;
// putting it in the snapshot would not work at all, because a member in
// another room is not in the snapshot.
const PartyVitalsInterval = time.Second

// PartyActionKind is what a player wants done.
type PartyActionKind uint8

const (
	PartyInvite PartyActionKind = iota + 1
	PartyAccept
	PartyDecline
	PartyLeave
	PartyKick
	PartyPromote
	PartySetLoot
)

// PartyRequest is a queued party action.
type PartyRequest struct {
	Kind PartyActionKind

	// Target is a character name, for invite, kick, and promote.
	Target string
}

// partySubjects are the three things a party talks about.
func partyChatSubject(id directory.PartyID) string   { return partySubject(id) + ".chat" }
func partyStateSubject(id directory.PartyID) string  { return partySubject(id) + ".state" }
func partyVitalsSubject(id directory.PartyID) string { return partySubject(id) + ".vitals" }

// partyPattern is the subscription one node takes out for one party.
func partyPattern(id directory.PartyID) string { return partySubject(id) + ".*" }

// Party queues a party action.
func (s *Session) Party(_ context.Context, req PartyRequest) error {
	if s.node.parties == nil {
		return errors.New("world: parties are not available")
	}

	select {
	case s.partyReqs <- req:
		return nil
	case <-s.done:
		return errors.New("world: session has ended")
	default:
		return errors.New("world: too many party requests")
	}
}

// handlePartyRequest performs one action, on the session goroutine.
func (s *Session) handlePartyRequest(req PartyRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	me := s.characterID.String()
	parties := s.node.parties

	var (
		party  directory.Party
		notice string
		err    error
	)

	switch req.Kind {
	case PartyInvite:
		party, err = s.invite(ctx, req.Target)
		if err == nil {
			// No notice on the party subject: the invitation itself is what
			// the target hears, and the existing members do not need telling
			// that somebody was asked until they answer.
			s.notify(mmov1.ChatChannel_CHAT_CHANNEL_PARTY,
				fmt.Sprintf("invited %s", req.Target))
		}

	case PartyAccept:
		party, err = parties.Accept(ctx, directory.Member{CharacterID: me, Name: s.name})
		notice = s.name + " joined the party"

	case PartyDecline:
		if err = parties.Decline(ctx, me); err == nil {
			s.notify(mmov1.ChatChannel_CHAT_CHANNEL_PARTY, "invitation declined")
		}

	case PartyLeave:
		party, err = parties.Leave(ctx, me)
		notice = s.name + " left the party"

	case PartyKick:
		party, err = s.kick(ctx, req.Target)
		notice = req.Target + " was removed from the party"

	case PartyPromote:
		party, err = s.promote(ctx, req.Target)
		notice = req.Target + " is now the party leader"

	case PartySetLoot:
		party, err = parties.SetLoot(ctx, me, req.Target)
		notice = "loot is now " + req.Target

	default:
		return
	}

	if err != nil {
		s.notify(mmov1.ChatChannel_CHAT_CHANNEL_PARTY, partyMessage(err))
		return
	}
	if req.Kind == PartyInvite || req.Kind == PartyDecline {
		return
	}

	// Reconcile this session first, then announce. The other order publishes
	// the roster to a subject this node has not subscribed to yet, so the
	// character who just joined never hears about the party they are in.
	s.syncPartyMembership(ctx)
	s.announceParty(ctx, party, notice)
}

// invite asks a named character to join.
func (s *Session) invite(ctx context.Context, targetName string) (directory.Party, error) {
	if s.node.presence == nil {
		return directory.Party{}, errors.New("world: presence is not available")
	}

	who, ok := s.node.presence.ByName(ctx, targetName)
	if !ok {
		return directory.Party{}, fmt.Errorf("%s is not online", strings.TrimSpace(targetName))
	}

	party, err := s.node.parties.Invite(ctx,
		directory.Member{CharacterID: s.characterID.String(), Name: s.name},
		who.CharacterID)
	if err != nil {
		return directory.Party{}, err
	}

	// The inviter's node may not have been in this party before -- inviting
	// creates one -- so it has to be listening before the answer arrives.
	s.syncPartyMembership(ctx)

	// Addressed to the node holding the target rather than to the party
	// subject: they are not a member yet, so their node is not subscribed.
	err = s.node.bus.Publish(ctx, chatSubject(string(who.Node)), &mmov1.ChatDelivery{
		Channel:         uint32(mmov1.ChatChannel_CHAT_CHANNEL_PARTY),
		FromCharacterId: s.characterID.String(),
		FromName:        s.name,
		ToCharacterId:   who.CharacterID,
		Body:            invitePrefix + s.name,
		ServerTimeMs:    time.Now().UnixMilli(),
	})
	if err != nil {
		return directory.Party{}, err
	}
	return party, nil
}

// invitePrefix marks a chat delivery that is really an invitation.
//
// Reusing the chat delivery rather than adding a second directed-message type:
// an invitation is addressed to one character on another node, which is
// exactly what that path already does. The prefix is stripped before anything
// is shown, so it can never appear in a message.
const invitePrefix = "\x00party-invite\x00"

// kick and promote resolve a name to the member it refers to.
func (s *Session) kick(ctx context.Context, targetName string) (directory.Party, error) {
	target, err := s.memberNamed(ctx, targetName)
	if err != nil {
		return directory.Party{}, err
	}
	return s.node.parties.Kick(ctx, s.characterID.String(), target)
}

func (s *Session) promote(ctx context.Context, targetName string) (directory.Party, error) {
	target, err := s.memberNamed(ctx, targetName)
	if err != nil {
		return directory.Party{}, err
	}
	return s.node.parties.Promote(ctx, s.characterID.String(), target)
}

// memberNamed resolves a name against the party's own roster.
//
// Against the roster rather than presence, because kicking somebody who has
// logged out is exactly when you most want to: presence would say they are not
// there and refuse.
func (s *Session) memberNamed(ctx context.Context, name string) (string, error) {
	party, ok := s.node.parties.Of(ctx, s.characterID.String())
	if !ok {
		return "", directory.ErrNotInParty
	}

	want := directory.NormaliseName(name)
	for _, m := range party.Members {
		if directory.NormaliseName(m.Name) == want {
			return m.CharacterID, nil
		}
	}
	return "", fmt.Errorf("%s is not in your party", strings.TrimSpace(name))
}

// announceParty tells every member's node that the party changed.
func (s *Session) announceParty(ctx context.Context, party directory.Party, notice string) {
	if party.ID == "" {
		return
	}

	update := &mmov1.PartyUpdate{
		PartyId:           string(party.ID),
		LeaderCharacterId: party.Leader,
		Loot:              party.Loot,
		Disbanded:         len(party.Members) == 0,
		Notice:            notice,
	}
	for _, m := range party.Members {
		update.Members = append(update.Members, &mmov1.PartyMemberInfo{
			CharacterId: m.CharacterID, Name: m.Name,
		})
	}

	if err := s.node.bus.Publish(ctx, partyStateSubject(party.ID), update); err != nil {
		s.log.Error("publishing a party update", "err", err)
	}

	// A member who has just left is no longer on the subject, so their own
	// node has to be told directly that they are now in no party.
	if !party.Has(s.characterID.String()) {
		s.sendPartyState(directory.Party{})
	}
}

// syncPartyMembership reconciles this session with whatever the directory now
// says, taking out or dropping the party subscription and re-keying the layer.
//
// Called after every change rather than each caller doing its own bookkeeping:
// there are six actions and three consequences, and pairing them by hand is
// how a session ends up subscribed to a party it left.
func (s *Session) syncPartyMembership(ctx context.Context) {
	var id directory.PartyID
	if party, ok := s.node.parties.Of(ctx, s.characterID.String()); ok {
		id = party.ID
	}

	loot := directory.LootFreeForAll
	if party, ok := s.node.parties.Of(ctx, s.characterID.String()); ok {
		loot = party.Loot
	}

	s.mu.Lock()
	previous, previousLoot := s.partyID, s.partyLoot
	s.partyID, s.partyLoot = id, loot
	s.mu.Unlock()

	if previous == id && previousLoot == loot {
		// The rule can change without the party doing, which still has to
		// reach the room -- it decides who may pick up the next drop.
		return
	}
	if previous == id {
		s.applyLayer(ctx)
		return
	}

	if previous != "" {
		s.node.unwatch(partyPattern(previous))
	}
	if id != "" {
		if err := s.node.watch(partyPattern(id), s.node.onPartyMessage); err != nil {
			s.log.Error("subscribing to a party", "party", id, "err", err)
		}
	}

	// The layer key is the party while partied and the character otherwise,
	// which is what makes partying up merge the members' mobs.
	s.applyLayer(ctx)
}

// applyLayer pushes the current layer key and loot rule into the room.
func (s *Session) applyLayer(ctx context.Context) {
	handle, entityID := s.Where()
	if handle == nil {
		return
	}
	handle.SetLayer(ctx, entityID, s.layerKey(), s.lootRule())
}

// lootRule is the party's rule, or free-for-all when unpartied -- where it
// means nothing, since a layer of one has nobody to take turns with.
func (s *Session) lootRule() room.LootRule {
	s.mu.Lock()
	loot := s.partyLoot
	s.mu.Unlock()

	if loot == directory.LootRoundRobin {
		return room.LootRoundRobin
	}
	return room.LootFreeForAll
}

// layerKey is the party ID while partied, and the character ID otherwise.
func (s *Session) layerKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.partyID != "" {
		return string(s.partyID)
	}
	return s.characterID.String()
}

// PartyID returns the party this character is in, if any.
func (s *Session) PartyID() directory.PartyID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.partyID
}

// onPartyMessage routes anything arriving on a party's subjects.
//
// One subscription per party with a wildcard, dispatched on the last token,
// rather than three subscriptions: a party is one thing to listen to, and
// three subscriptions is three chances to get the lifecycle wrong.
func (n *Node) onPartyMessage(_ context.Context, subject string, payload []byte) {
	switch {
	case strings.HasSuffix(subject, ".chat"):
		var d mmov1.ChatDelivery
		if err := proto.Unmarshal(payload, &d); err != nil {
			n.log.Error("decoding party chat", "err", err)
			return
		}
		n.fanOutChat(&d)

	case strings.HasSuffix(subject, ".state"):
		var u mmov1.PartyUpdate
		if err := proto.Unmarshal(payload, &u); err != nil {
			n.log.Error("decoding a party update", "err", err)
			return
		}
		n.fanOutPartyUpdate(&u)

	case strings.HasSuffix(subject, ".vitals"):
		var v mmov1.PartyVitals
		if err := proto.Unmarshal(payload, &v); err != nil {
			n.log.Error("decoding party vitals", "err", err)
			return
		}
		n.fanOutPartyVitals(subject, &v)
	}
}

// fanOutPartyUpdate hands a roster change to the local members.
func (n *Node) fanOutPartyUpdate(u *mmov1.PartyUpdate) {
	party := directory.Party{
		ID:     directory.PartyID(u.GetPartyId()),
		Leader: u.GetLeaderCharacterId(),
		Loot:   u.GetLoot(),
	}
	for _, m := range u.GetMembers() {
		party.Members = append(party.Members,
			directory.Member{CharacterID: m.GetCharacterId(), Name: m.GetName()})
	}

	for _, s := range n.localSessions() {
		if s.PartyID() != party.ID {
			continue
		}
		s.sendPartyState(party)

		// A member whose leader changed the rule has to hear about it in the
		// room, not just on the roster: it decides who may take the next drop.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.syncPartyMembership(ctx)
		cancel()

		if u.GetNotice() != "" {
			s.notify(mmov1.ChatChannel_CHAT_CHANNEL_PARTY, u.GetNotice())
		}
	}
}

// fanOutPartyVitals updates one member frame.
func (n *Node) fanOutPartyVitals(subject string, v *mmov1.PartyVitals) {
	id := directory.PartyID(strings.TrimSuffix(strings.TrimPrefix(subject, "party."), ".vitals"))

	for _, s := range n.localSessions() {
		if s.PartyID() != id || s.characterID.String() == v.GetCharacterId() {
			continue
		}
		s.recordVitals(v)
	}
}

// partyMessage turns a party failure into something worth showing.
func partyMessage(err error) string {
	switch {
	case errors.Is(err, directory.ErrPartyFull):
		return "that party is full"
	case errors.Is(err, directory.ErrAlreadyInParty):
		return "they are already in a party"
	case errors.Is(err, directory.ErrNotInParty):
		return "you are not in a party"
	case errors.Is(err, directory.ErrNotLeader):
		return "only the party leader can do that"
	case errors.Is(err, directory.ErrNoInvite):
		return "that invitation has expired"
	case errors.Is(err, directory.ErrNoParty):
		return "that party no longer exists"
	default:
		return err.Error()
	}
}

// sendPartyState sends the whole roster to this player's client.
//
// Whole rather than incremental: a party is at most six members, the delta
// would be most of the message anyway, and a client that misses one
// incremental update shows a roster that is quietly wrong until somebody
// leaves. An empty member list means "you are not in a party".
func (s *Session) sendPartyState(party directory.Party) {
	state := &mmov1.PartyState{
		PartyId:           string(party.ID),
		LeaderCharacterId: party.Leader,
		SelfCharacterId:   s.characterID.String(),
		Loot:              party.Loot,
	}

	s.mu.Lock()
	vitals := s.partyVitals
	s.mu.Unlock()

	for _, m := range party.Members {
		frame := &mmov1.PartyMember{
			CharacterId: m.CharacterID,
			Name:        m.Name,
			Online:      true,
		}
		// Somebody in another room has no entry in this player's snapshot, so
		// their frame is filled from the vitals they publish on a slow beat.
		// A member who has just joined has not published one yet, which reads
		// as a frame that fills in a moment later rather than a blank one.
		if v, ok := vitals[m.CharacterID]; ok {
			frame.Level = v.GetLevel()
			frame.Hp = v.GetHp()
			frame.HpMax = v.GetHpMax()
			frame.MapId = v.GetMapId()
			frame.Online = v.GetOnline()
		}
		if m.CharacterID == s.characterID.String() {
			// The player's own frame: their map is known here directly, and
			// they are by definition looking at this.
			frame.MapId = s.MapID()
			frame.Online = true
		}
		state.Members = append(state.Members, frame)
	}

	s.deliver(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_Party{Party: state},
		}},
	})
}

// recordVitals stores a member's health and resends the roster.
//
// Resending the whole roster on every vitals push is a message a second per
// member, which for six members is nothing, and it means there is exactly one
// path that produces a member frame rather than two that can disagree.
func (s *Session) recordVitals(v *mmov1.PartyVitals) {
	s.mu.Lock()
	if s.partyVitals == nil {
		s.partyVitals = make(map[string]*mmov1.PartyVitals)
	}
	s.partyVitals[v.GetCharacterId()] = v
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if party, ok := s.node.parties.Of(ctx, s.characterID.String()); ok {
		s.sendPartyState(party)
	}
}

// publishVitals pushes this character's health to their party.
func (s *Session) publishVitals(ctx context.Context) {
	id := s.PartyID()
	if id == "" {
		return
	}

	handle, entityID := s.Where()
	if handle == nil {
		return
	}
	snap, ok := handle.Capture(ctx, entityID)
	if !ok {
		return
	}

	s.mu.Lock()
	online := s.sink != nil && !s.disconnected
	s.mu.Unlock()

	mine := &mmov1.PartyVitals{
		CharacterId: s.characterID.String(),
		Level:       uint32(snap.Progress.Level),
		Hp:          snap.State.HP,
		HpMax:       snap.State.MaxHP,
		MapId:       s.MapID(),
		Online:      online,
	}

	// Kept as well as sent. Every other member's frame is filled from the
	// vitals they publish, and without this the player's own frame is the one
	// place on the panel with an empty health bar.
	s.mu.Lock()
	if s.partyVitals == nil {
		s.partyVitals = make(map[string]*mmov1.PartyVitals)
	}
	s.partyVitals[mine.GetCharacterId()] = mine
	s.mu.Unlock()

	if err := s.node.bus.Publish(ctx, partyVitalsSubject(id), mine); err != nil {
		s.log.Error("publishing party vitals", "err", err)
	}
}

// announcePresence records this character as online, or away.
//
// Called again after a transfer, because the map changed and the node may
// have too, and again on a disconnect so a whisper is refused rather than
// delivered to a socket that has gone.
func (s *Session) announcePresence(ctx context.Context, away bool) {
	if s.node.presence == nil {
		return
	}

	err := s.node.presence.Announce(ctx, directory.Online{
		CharacterID: s.characterID.String(),
		Name:        s.name,
		Node:        directory.NodeID(s.node.nodeID),
		MapID:       s.MapID(),
		Away:        away,
	})
	if err != nil {
		s.log.Error("announcing presence", "err", err)
	}
}

// restoreParty picks a character's party back up on login.
//
// Party membership outlives a session: somebody who crashes and comes back is
// still in the party they left, so the subscription and the layer key have to
// be restored rather than assumed absent. The name is refreshed at the same
// time, since a party can hold a member it has never seen online.
func (s *Session) restoreParty(ctx context.Context) {
	if s.node.parties == nil {
		return
	}

	party, ok := s.node.parties.Of(ctx, s.characterID.String())
	if !ok {
		return
	}
	if err := s.node.parties.Rename(ctx, s.characterID.String(), s.name); err != nil {
		s.log.Error("refreshing a party name", "err", err)
	}

	s.syncPartyMembership(ctx)
	s.announceParty(ctx, party, s.name+" came online")
	s.sendPartyState(party)
}

// leaveParty drops this session's claim on its party subscription.
//
// Deliberately not leaving the party itself: logging out does not remove
// somebody from their group, any more than walking out of the room does. The
// membership stays, the subscription goes, and the remaining members see them
// as offline once their vitals stop arriving.
func (s *Session) leaveParty(ctx context.Context) {
	id := s.PartyID()
	if id == "" {
		return
	}

	// One last vitals push, so the members' frames show them offline now
	// rather than showing stale health until somebody else changes.
	s.mu.Lock()
	s.disconnected = true
	s.mu.Unlock()
	s.publishVitals(ctx)

	s.mu.Lock()
	s.partyID = ""
	s.mu.Unlock()

	s.node.unwatch(partyPattern(id))
}
