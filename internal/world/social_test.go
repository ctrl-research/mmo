package world

import (
	"context"
	"testing"
	"time"

	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/store"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/google/uuid"
)

// Chat and parties, across two nodes.
//
// Everything here is set up so the two characters are on different nodes,
// because that is the case the design is for: a whisper, a party invitation,
// and a roster change all have to find somebody whose session is somewhere
// else, and the only way to do that is over the bus. A single-node test would
// pass with a shared pointer.

func say(channel mmov1.ChatChannel, body string) *mmov1.ChatSend {
	return &mmov1.ChatSend{Channel: channel, Body: body}
}

func whisper(to, body string) *mmov1.ChatSend {
	return &mmov1.ChatSend{
		Channel: mmov1.ChatChannel_CHAT_CHANNEL_WHISPER,
		Target:  to,
		Body:    body,
	}
}

// --- chat --------------------------------------------------------------------

func TestGlobalChatReachesAnotherNode(t *testing.T) {
	c := newCluster(t)
	aliceAccount, alice := c.character("Alice")
	bobAccount, bob := c.character("Bob")

	aliceSession, aliceSink := c.enter(c.a, aliceAccount, alice)
	_, bobSink := c.enter(c.b, bobAccount, bob)

	if err := aliceSession.SendChat(context.Background(),
		say(mmov1.ChatChannel_CHAT_CHANNEL_GLOBAL, "hello world")); err != nil {
		t.Fatalf("send: %v", err)
	}

	eventually(t, "Bob to hear it", func() bool {
		return bobSink.heard(mmov1.ChatChannel_CHAT_CHANNEL_GLOBAL, "hello world")
	})

	// The speaker gets their own copy, produced locally rather than travelling
	// to every node and back. Without it, a client cannot tell a delivered
	// message from a dropped one.
	eventually(t, "Alice to see her own line", func() bool {
		return aliceSink.heard(mmov1.ChatChannel_CHAT_CHANNEL_GLOBAL, "hello world")
	})

	if n := len(aliceSink.chat()); n != 1 {
		t.Errorf("the speaker received %d copies of one message, want 1", n)
	}
}

// A whisper has one recipient. Broadcasting it so that one node keeps it would
// spread private messages across the cluster.
func TestWhisperReachesOnlyItsRecipient(t *testing.T) {
	c := newCluster(t)
	aliceAccount, alice := c.character("Alice")
	bobAccount, bob := c.character("Bob")
	eveAccount, eve := c.character("Eve")

	aliceSession, aliceSink := c.enter(c.a, aliceAccount, alice)
	_, bobSink := c.enter(c.b, bobAccount, bob)
	_, eveSink := c.enter(c.b, eveAccount, eve)

	if err := aliceSession.SendChat(context.Background(),
		whisper("bob", "just for you")); err != nil {
		t.Fatalf("send: %v", err)
	}

	eventually(t, "Bob to hear it", func() bool {
		return bobSink.heard(mmov1.ChatChannel_CHAT_CHANNEL_WHISPER, "just for you")
	})

	// Eve is on the same node as Bob, which is exactly where a routing bug
	// would deliver it.
	if eveSink.heard(mmov1.ChatChannel_CHAT_CHANNEL_WHISPER, "just for you") {
		t.Error("a whisper was delivered to somebody it was not addressed to")
	}

	// The sender's copy names the recipient, so the two halves of a
	// conversation read differently.
	var sent *mmov1.ChatLine
	for _, line := range aliceSink.chat() {
		if line.GetOutgoing() {
			sent = line
		}
	}
	if sent == nil {
		t.Fatal("the sender got no copy of their own whisper")
	}
	if sent.GetFrom() != "Bob" {
		t.Errorf("the sender's copy is from %q, want the recipient's name", sent.GetFrom())
	}
}

// Whispering a name nobody is using is a typo, and the usual answer to a typo
// is to say so.
func TestWhisperToSomebodyOfflineIsRefused(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Alice")
	s, sink := c.enter(c.a, account, character)

	if err := s.SendChat(context.Background(), whisper("Nobody", "hello")); err != nil {
		t.Fatalf("send: %v", err)
	}

	eventually(t, "the refusal", func() bool { return len(sink.system()) > 0 })
	if got := sink.system()[0]; got != "Nobody is not online" {
		t.Errorf("refusal is %q, which does not say what went wrong", got)
	}
}

// Local chat is the one channel that never touches the bus, so somebody in
// another room must not hear it however the routing is wired.
func TestLocalChatStaysInTheRoom(t *testing.T) {
	c := newCluster(t)
	aliceAccount, alice := c.character("Alice")
	bobAccount, bob := c.character("Bob")

	// Bob is in the annex; Alice is in the test map.
	c.standingIn("annex", bob, c.game.Maps["annex"].Portals[0].Bounds)
	aliceSession, aliceSink := c.enter(c.a, aliceAccount, alice)
	_, bobSink := c.enter(c.b, bobAccount, bob)

	if err := aliceSession.SendChat(context.Background(),
		say(mmov1.ChatChannel_CHAT_CHANNEL_LOCAL, "anyone here?")); err != nil {
		t.Fatalf("send: %v", err)
	}

	eventually(t, "Alice to hear herself", func() bool {
		return aliceSink.heard(mmov1.ChatChannel_CHAT_CHANNEL_LOCAL, "anyone here?")
	})
	if bobSink.heard(mmov1.ChatChannel_CHAT_CHANNEL_LOCAL, "anyone here?") {
		t.Error("local chat reached another room")
	}
}

// A muted player finds out they are muted. A message that vanishes reads as
// the game being broken.
func TestAMutedCharacterIsToldWhy(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Loud")

	if err := c.store.MuteCharacter(context.Background(), character, nil,
		"advertising", "test"); err != nil {
		t.Fatalf("mute: %v", err)
	}

	s, sink := c.enter(c.a, account, character)
	if err := s.SendChat(context.Background(),
		say(mmov1.ChatChannel_CHAT_CHANNEL_GLOBAL, "buy gold")); err != nil {
		t.Fatalf("send: %v", err)
	}

	eventually(t, "the refusal", func() bool { return len(sink.system()) > 0 })

	got := sink.system()[0]
	if got != "you cannot use chat (advertising)" {
		t.Errorf("refusal is %q; it should say both that they are muted and why", got)
	}
	if len(sink.chat()) != 0 {
		t.Error("a muted player's message was delivered anyway")
	}
}

// The whole point of a rate limit is that it stops somewhere.
func TestGlobalChatIsRateLimited(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Chatty")
	s, sink := c.enter(c.a, account, character)

	allowed := c.game.Balance.Chat.ChatLimit("global")
	for i := 0; i < allowed+5; i++ {
		if err := s.SendChat(context.Background(),
			say(mmov1.ChatChannel_CHAT_CHANNEL_GLOBAL, "spam")); err != nil {
			// The queue filling is itself a refusal, and an honest one.
			break
		}
	}

	eventually(t, "the limit to bite", func() bool {
		for _, msg := range sink.system() {
			if msg == "slow down" {
				return true
			}
		}
		return false
	})

	if n := len(sink.chat()); n > allowed {
		t.Errorf("%d messages got through on a budget of %d", n, allowed)
	}
}

// --- parties -----------------------------------------------------------------

// party puts two characters on different nodes into one party.
func (c *cluster) party(t *testing.T) (a, b *Session, aSink, bSink *captureSink) {
	t.Helper()

	aliceAccount, alice := c.character("Alice")
	bobAccount, bob := c.character("Bob")

	a, aSink = c.enter(c.a, aliceAccount, alice)
	b, bSink = c.enter(c.b, bobAccount, bob)

	ctx := context.Background()
	if err := a.Party(ctx, PartyRequest{Kind: PartyInvite, Target: "Bob"}); err != nil {
		t.Fatalf("invite: %v", err)
	}
	eventually(t, "Bob to be invited", func() bool { return len(bSink.invites()) > 0 })

	if err := b.Party(ctx, PartyRequest{Kind: PartyAccept}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	eventually(t, "both to be in the party", func() bool {
		return a.PartyID() != "" && a.PartyID() == b.PartyID()
	})
	return a, b, aSink, bSink
}

func TestPartyInviteCrossesNodes(t *testing.T) {
	c := newCluster(t)
	_, _, aliceSink, bobSink := c.party(t)

	if got := bobSink.invites(); len(got) == 0 || got[0] != "Alice" {
		t.Errorf("Bob's invitations are %v, want one from Alice", got)
	}

	// Both ends see the same roster: a party where the members disagree about
	// who is in it is worse than no party.
	for name, sink := range map[string]*captureSink{"Alice": aliceSink, "Bob": bobSink} {
		states := sink.partyStates()
		if len(states) == 0 {
			t.Fatalf("%s never received a roster", name)
		}
		last := states[len(states)-1]
		if len(last.GetMembers()) != 2 {
			t.Errorf("%s sees %d members, want 2", name, len(last.GetMembers()))
		}
	}
}

// The reason parties touch the simulation at all: members hunt one mob
// population instead of one each.
func TestPartyingMergesTheMobLayer(t *testing.T) {
	c := newCluster(t)
	alice, bob, _, _ := c.party(t)

	if alice.layerKey() != bob.layerKey() {
		t.Errorf("party members have layer keys %q and %q; partying up should "+
			"merge them", alice.layerKey(), bob.layerKey())
	}
	if alice.layerKey() != string(alice.PartyID()) {
		t.Errorf("the layer key is %q, want the party ID", alice.layerKey())
	}
}

func TestLeavingAPartySplitsTheLayerAgain(t *testing.T) {
	c := newCluster(t)
	alice, bob, _, _ := c.party(t)
	shared := alice.layerKey()

	if err := bob.Party(context.Background(), PartyRequest{Kind: PartyLeave}); err != nil {
		t.Fatalf("leave: %v", err)
	}

	eventually(t, "Bob to be out of the party", func() bool { return bob.PartyID() == "" })
	if bob.layerKey() == shared {
		t.Error("a character who left the party is still hunting in its layer")
	}
	if bob.layerKey() != bob.characterID.String() {
		t.Errorf("an unpartied character's layer key is %q, want their character ID",
			bob.layerKey())
	}

	// A party of one is not a party, so Alice is out of it too.
	eventually(t, "the party to disband", func() bool { return alice.PartyID() == "" })
}

func TestPartyChatReachesMembersOnOtherNodes(t *testing.T) {
	c := newCluster(t)
	alice, _, _, bobSink := c.party(t)

	if err := alice.SendChat(context.Background(),
		say(mmov1.ChatChannel_CHAT_CHANNEL_PARTY, "pulling now")); err != nil {
		t.Fatalf("send: %v", err)
	}

	eventually(t, "Bob to hear it", func() bool {
		return bobSink.heard(mmov1.ChatChannel_CHAT_CHANNEL_PARTY, "pulling now")
	})
}

func TestPartyChatWithNoPartyIsRefused(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Alone")
	s, sink := c.enter(c.a, account, character)

	if err := s.SendChat(context.Background(),
		say(mmov1.ChatChannel_CHAT_CHANNEL_PARTY, "anyone?")); err != nil {
		t.Fatalf("send: %v", err)
	}

	eventually(t, "the refusal", func() bool {
		for _, msg := range sink.system() {
			if msg == "you are not in a party" {
				return true
			}
		}
		return false
	})
}

// Member frames for somebody in another room have to come from somewhere.
func TestMemberFramesFillInFromVitals(t *testing.T) {
	c := newCluster(t)
	_, _, aliceSink, _ := c.party(t)

	eventually(t, "Bob's health to reach Alice's roster", func() bool {
		states := aliceSink.partyStates()
		if len(states) == 0 {
			return false
		}
		for _, m := range states[len(states)-1].GetMembers() {
			if m.GetName() == "Bob" && m.GetHpMax() > 0 && m.GetMapId() != "" {
				return true
			}
		}
		return false
	})
}

// Only the leader may kick, or a party is a group anybody can dissolve.
func TestOnlyTheLeaderCanKick(t *testing.T) {
	c := newCluster(t)
	alice, bob, _, bobSink := c.party(t)

	if err := bob.Party(context.Background(),
		PartyRequest{Kind: PartyKick, Target: "Alice"}); err != nil {
		t.Fatalf("kick: %v", err)
	}

	eventually(t, "the refusal", func() bool {
		for _, msg := range bobSink.system() {
			if msg == "only the party leader can do that" {
				return true
			}
		}
		return false
	})
	if alice.PartyID() == "" {
		t.Error("a non-leader kicked the leader out of the party")
	}
}

func TestLeaderCanKickAndPromote(t *testing.T) {
	c := newCluster(t)
	alice, bob, _, _ := c.party(t)
	ctx := context.Background()

	if err := alice.Party(ctx, PartyRequest{Kind: PartyPromote, Target: "Bob"}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	eventually(t, "Bob to be the leader", func() bool {
		party, ok := c.parties.Of(ctx, bob.characterID.String())
		return ok && party.Leader == bob.characterID.String()
	})

	// Now the new leader can remove the old one, which is what leadership is.
	if err := bob.Party(ctx, PartyRequest{Kind: PartyKick, Target: "Alice"}); err != nil {
		t.Fatalf("kick: %v", err)
	}
	eventually(t, "Alice to be out", func() bool { return alice.PartyID() == "" })
}

// An invitation is a question asked in the moment.
func TestExpiredInvitationsAreRefused(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Hopeful")
	s, sink := c.enter(c.a, account, character)

	if err := s.Party(context.Background(), PartyRequest{Kind: PartyAccept}); err != nil {
		t.Fatalf("accept: %v", err)
	}

	eventually(t, "the refusal", func() bool {
		for _, msg := range sink.system() {
			if msg == "that invitation has expired" {
				return true
			}
		}
		return false
	})
}

func TestLootRuleIsTheLeadersToSet(t *testing.T) {
	c := newCluster(t)
	alice, bob, _, bobSink := c.party(t)
	ctx := context.Background()

	if err := bob.Party(ctx, PartyRequest{
		Kind: PartySetLoot, Target: directory.LootRoundRobin,
	}); err != nil {
		t.Fatalf("set loot: %v", err)
	}
	eventually(t, "the refusal", func() bool {
		for _, msg := range bobSink.system() {
			if msg == "only the party leader can do that" {
				return true
			}
		}
		return false
	})

	if err := alice.Party(ctx, PartyRequest{
		Kind: PartySetLoot, Target: directory.LootRoundRobin,
	}); err != nil {
		t.Fatalf("set loot: %v", err)
	}
	eventually(t, "the rule to reach both members", func() bool {
		party, ok := c.parties.Of(ctx, alice.characterID.String())
		return ok && party.Loot == directory.LootRoundRobin && bob.lootRule() == 1
	})
}

// Party membership outlives a session: crashing and coming back should find
// the party you were in, not an empty roster.
func TestPartyMembershipSurvivesALogout(t *testing.T) {
	c := newCluster(t)
	alice, bob, _, _ := c.party(t)
	partyID := alice.PartyID()

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	bob.Close(closeCtx)
	cancel()

	back, sink := c.enter(c.b, bob.accountID, bob.characterID)

	eventually(t, "Bob to be back in the party", func() bool {
		return back.PartyID() == partyID
	})
	eventually(t, "Bob's client to be told", func() bool {
		states := sink.partyStates()
		return len(states) > 0 && len(states[len(states)-1].GetMembers()) == 2
	})
	if back.layerKey() != string(partyID) {
		t.Error("a returning member is hunting outside their party's layer")
	}
}

// --- guilds ------------------------------------------------------------------

// A guild is durable where a party is not, but the delivery shape is the same:
// a subject per guild, and every node holding a member subscribed to it.
func (c *cluster) guild(t *testing.T) (a, b *Session, aSink, bSink *captureSink) {
	t.Helper()

	aliceAccount, alice := c.character("Alice")
	bobAccount, bob := c.character("Bob")

	a, aSink = c.enter(c.a, aliceAccount, alice)
	b, bSink = c.enter(c.b, bobAccount, bob)

	ctx := context.Background()
	if err := a.Guild(ctx, GuildRequest{Kind: GuildCreate, Target: "Wardens"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	eventually(t, "the guild to exist", func() bool { return a.GuildID() != uuid.Nil })

	if err := a.Guild(ctx, GuildRequest{Kind: GuildInvite, Target: "Bob"}); err != nil {
		t.Fatalf("invite: %v", err)
	}
	eventually(t, "Bob to be invited", func() bool { return len(bSink.guildInvites()) > 0 })

	if err := b.Guild(ctx, GuildRequest{Kind: GuildAccept}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	eventually(t, "both to be in the guild", func() bool {
		return b.GuildID() != uuid.Nil && a.GuildID() == b.GuildID()
	})
	return a, b, aSink, bSink
}

func TestGuildInviteAndRosterCrossNodes(t *testing.T) {
	c := newCluster(t)
	_, _, aliceSink, bobSink := c.guild(t)

	// Both ends converge on the same roster. Waiting rather than reading once:
	// membership is set the moment the joiner's node hears, and the founder's
	// reload arrives a beat later.
	for name, sink := range map[string]*captureSink{"Alice": aliceSink, "Bob": bobSink} {
		eventually(t, name+" to see the whole roster", func() bool {
			states := sink.guildStates()
			if len(states) == 0 {
				return false
			}
			last := states[len(states)-1]
			return last.GetName() == "Wardens" && len(last.GetMembers()) == 2
		})
	}

	// The founder leads; whoever joins does not. Ranks are the whole reason
	// the permission checks below mean anything.
	last := aliceSink.guildStates()[len(aliceSink.guildStates())-1]
	if last.GetRank() != store.RankLeader {
		t.Errorf("the founder's rank is %d, want leader", last.GetRank())
	}
}

func TestGuildChatReachesMembersOnOtherNodes(t *testing.T) {
	c := newCluster(t)
	alice, _, _, bobSink := c.guild(t)

	if err := alice.SendChat(context.Background(),
		say(mmov1.ChatChannel_CHAT_CHANNEL_GUILD, "raid tonight")); err != nil {
		t.Fatalf("send: %v", err)
	}

	eventually(t, "Bob to hear it", func() bool {
		return bobSink.heard(mmov1.ChatChannel_CHAT_CHANNEL_GUILD, "raid tonight")
	})
}

// An officer able to make officers is an officer able to hand the guild to
// anybody.
func TestOnlyTheLeaderChangesRanks(t *testing.T) {
	c := newCluster(t)
	alice, bob, _, bobSink := c.guild(t)
	ctx := context.Background()

	if err := bob.Guild(ctx, GuildRequest{Kind: GuildPromote, Target: "Alice"}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	eventually(t, "the refusal", func() bool {
		for _, msg := range bobSink.system() {
			if msg == "your rank does not allow that" {
				return true
			}
		}
		return false
	})

	// The leader can, and a promoted officer can then invite.
	if err := alice.Guild(ctx, GuildRequest{Kind: GuildPromote, Target: "Bob"}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	eventually(t, "Bob to be an officer", func() bool {
		_, rank, err := c.store.GuildOf(ctx, bob.characterID)
		return err == nil && rank == store.RankOfficer
	})
}

func TestGuildMOTDNeedsRank(t *testing.T) {
	c := newCluster(t)
	alice, bob, _, bobSink := c.guild(t)
	ctx := context.Background()

	if err := bob.Guild(ctx, GuildRequest{
		Kind: GuildSetMOTD, Target: "everyone follow me",
	}); err != nil {
		t.Fatalf("motd: %v", err)
	}
	eventually(t, "the refusal", func() bool {
		for _, msg := range bobSink.system() {
			if msg == "your rank does not allow that" {
				return true
			}
		}
		return false
	})

	if err := alice.Guild(ctx, GuildRequest{
		Kind: GuildSetMOTD, Target: "raid at eight",
	}); err != nil {
		t.Fatalf("motd: %v", err)
	}
	eventually(t, "Bob to see the notice", func() bool {
		states := bobSink.guildStates()
		return len(states) > 0 && states[len(states)-1].GetMotd() == "raid at eight"
	})
}

// A guild outlives the session, the party, and being logged out for a month.
func TestGuildMembershipSurvivesALogout(t *testing.T) {
	c := newCluster(t)
	_, bob, _, _ := c.guild(t)
	guildID := bob.GuildID()

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	bob.Close(closeCtx)
	cancel()

	back, sink := c.enter(c.b, bob.accountID, bob.characterID)

	eventually(t, "Bob to be back in the guild", func() bool {
		return back.GuildID() == guildID
	})
	eventually(t, "Bob's client to be told", func() bool {
		states := sink.guildStates()
		return len(states) > 0 && states[len(states)-1].GetName() == "Wardens"
	})
}

// One guild per character, enforced by the database rather than assumed: two
// memberships would give one character two rosters and two guild chats.
func TestACharacterCanOnlyBeInOneGuild(t *testing.T) {
	c := newCluster(t)
	alice, _, aliceSink, _ := c.guild(t)

	if err := alice.Guild(context.Background(),
		GuildRequest{Kind: GuildCreate, Target: "Seconds"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	eventually(t, "the refusal", func() bool {
		for _, msg := range aliceSink.system() {
			if msg == "already in a guild" {
				return true
			}
		}
		return false
	})
}

func TestGuildNamesAreUnique(t *testing.T) {
	c := newCluster(t)
	c.guild(t)

	account, character := c.character("Rival")
	s, sink := c.enter(c.a, account, character)

	if err := s.Guild(context.Background(),
		GuildRequest{Kind: GuildCreate, Target: "wardens"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	eventually(t, "the refusal", func() bool {
		for _, msg := range sink.system() {
			if msg == "that guild name is taken" {
				return true
			}
		}
		return false
	})
}

// --- friends -----------------------------------------------------------------

// A friends list is durable; who is online comes from presence. Neither is
// much use without the other.
func TestFriendsListShowsWhoIsOnline(t *testing.T) {
	c := newCluster(t)
	aliceAccount, alice := c.character("Alice")
	bobAccount, bob := c.character("Bob")
	_, _ = c.character("Ghost") // never logs in

	aliceSession, aliceSink := c.enter(c.a, aliceAccount, alice)
	bobSession, _ := c.enter(c.b, bobAccount, bob)

	ctx := context.Background()
	for _, name := range []string{"Bob", "Ghost"} {
		if err := aliceSession.Social(ctx,
			SocialRequest{Kind: FriendAdd, Target: name}); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}

	eventually(t, "both friends to be listed", func() bool {
		lists := aliceSink.friendLists()
		return len(lists) > 0 && len(lists[len(lists)-1].GetFriends()) == 2
	})

	byName := map[string]*mmov1.FriendEntry{}
	lists := aliceSink.friendLists()
	for _, f := range lists[len(lists)-1].GetFriends() {
		byName[f.GetName()] = f
	}

	if !byName["Bob"].GetOnline() {
		t.Error("Bob is logged in and shows as offline")
	}
	if byName["Bob"].GetMapId() == "" {
		t.Error("an online friend has no map; a friends list should answer where they are")
	}
	if byName["Ghost"].GetOnline() {
		t.Error("a character who has never logged in shows as online")
	}

	// Logging out takes them off, without taking them off the list.
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	bobSession.Close(closeCtx)
	cancel()

	if err := aliceSession.Social(ctx, SocialRequest{Kind: FriendList}); err != nil {
		t.Fatalf("list: %v", err)
	}
	eventually(t, "Bob to show as offline", func() bool {
		lists := aliceSink.friendLists()
		for _, f := range lists[len(lists)-1].GetFriends() {
			if f.GetName() == "Bob" {
				return !f.GetOnline()
			}
		}
		return false
	})
}

func TestAddingSomebodyWhoDoesNotExistIsRefused(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Alice")
	s, sink := c.enter(c.a, account, character)

	if err := s.Social(context.Background(),
		SocialRequest{Kind: FriendAdd, Target: "Nobody"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	eventually(t, "the refusal", func() bool {
		for _, msg := range sink.system() {
			if msg == "there is no character called Nobody" {
				return true
			}
		}
		return false
	})
}

func TestRemovingAFriendTakesThemOffTheList(t *testing.T) {
	c := newCluster(t)
	aliceAccount, alice := c.character("Alice")
	_, _ = c.character("Bob")

	s, sink := c.enter(c.a, aliceAccount, alice)
	ctx := context.Background()

	if err := s.Social(ctx, SocialRequest{Kind: FriendAdd, Target: "Bob"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	eventually(t, "Bob to be listed", func() bool {
		lists := sink.friendLists()
		return len(lists) > 0 && len(lists[len(lists)-1].GetFriends()) == 1
	})

	if err := s.Social(ctx, SocialRequest{Kind: FriendRemove, Target: "Bob"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	eventually(t, "the list to empty", func() bool {
		lists := sink.friendLists()
		return len(lists[len(lists)-1].GetFriends()) == 0
	})
}

// The player's own frame is the one place a bug here shows as an empty health
// bar: every other member's is filled from the vitals they publish, and
// nobody publishes to themselves.
func TestYourOwnMemberFrameHasHealth(t *testing.T) {
	c := newCluster(t)
	_, _, aliceSink, _ := c.party(t)

	eventually(t, "Alice's own frame to fill in", func() bool {
		states := aliceSink.partyStates()
		if len(states) == 0 {
			return false
		}
		last := states[len(states)-1]
		for _, m := range last.GetMembers() {
			if m.GetCharacterId() == last.GetSelfCharacterId() {
				return m.GetHpMax() > 0 && m.GetHp() > 0
			}
		}
		return false
	})
}
