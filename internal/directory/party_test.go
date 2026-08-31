package directory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The Parties contract, run against every implementation.
//
// Parties had no tests in this package before this: they were exercised only
// through the cluster suite, which meant a rule about who may kick whom was
// asserted through six layers of session plumbing or not at all. Every rule in
// the interface doc is asserted here, against both implementations.

const testMaxParty = 4

type partiesUnderTest struct {
	name string
	open func(t *testing.T, ttl time.Duration) Parties
}

func partyImplementations() []partiesUnderTest {
	impls := []partiesUnderTest{{
		name: "memory",
		open: func(t *testing.T, ttl time.Duration) Parties {
			t.Helper()
			p := NewMemoryParties(testMaxParty)
			p.inviteTTL = ttl
			t.Cleanup(func() { p.Close() })
			return p
		},
	}}

	if addr := os.Getenv("MMO_TEST_REDIS_ADDR"); addr != "" {
		impls = append(impls, partiesUnderTest{
			name: "redis",
			open: func(t *testing.T, ttl time.Duration) Parties {
				return openRedisParties(t, addr, ttl)
			},
		})
	}
	return impls
}

// openRedisParties returns a party table with a namespace of its own.
func openRedisParties(t *testing.T, addr string, ttl time.Duration) *RedisParties {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: addr})
	prefix := "mmoparty:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx := context.Background()

	t.Cleanup(func() {
		if keys, err := client.Keys(ctx, prefix+"*").Result(); err == nil && len(keys) > 0 {
			client.Del(ctx, keys...)
		}
		client.Close()
	})

	p := NewRedisParties(client, prefix, testMaxParty)
	p.inviteTTL = ttl
	return p
}

func eachParties(t *testing.T, fn func(t *testing.T, p Parties)) {
	t.Helper()
	for _, impl := range partyImplementations() {
		t.Run(impl.name, func(t *testing.T) { fn(t, impl.open(t, InviteTTL)) })
	}
}

// eachPartiesWithTTL is for the two tests that need an invitation to expire
// while they watch.
func eachPartiesWithTTL(t *testing.T, ttl time.Duration, fn func(t *testing.T, p Parties)) {
	t.Helper()
	for _, impl := range partyImplementations() {
		t.Run(impl.name, func(t *testing.T) { fn(t, impl.open(t, ttl)) })
	}
}

func member(id, name string) Member { return Member{CharacterID: id, Name: name} }

// --- helpers that fail rather than returning ---------------------------------

func mustCreate(t *testing.T, p Parties, ctx context.Context, m Member) Party {
	t.Helper()
	party, err := p.Create(ctx, m)
	if err != nil {
		t.Fatalf("creating a party for %s: %v", m.CharacterID, err)
	}
	return party
}

func mustInvite(t *testing.T, p Parties, ctx context.Context, from Member, to string) Party {
	t.Helper()
	party, err := p.Invite(ctx, from, to)
	if err != nil {
		t.Fatalf("inviting %s: %v", to, err)
	}
	return party
}

func mustAccept(t *testing.T, p Parties, ctx context.Context, who Member) Party {
	t.Helper()
	party, err := p.Accept(ctx, who)
	if err != nil {
		t.Fatalf("%s accepting: %v", who.CharacterID, err)
	}
	return party
}

func mustOf(t *testing.T, p Parties, ctx context.Context, id string) (Party, bool) {
	t.Helper()
	party, ok, err := p.Of(ctx, id)
	if err != nil {
		t.Fatalf("reading %s's party: %v", id, err)
	}
	return party, ok
}

// join builds a party of the given size, the first member leading.
func join(t *testing.T, p Parties, ctx context.Context, ids ...string) Party {
	t.Helper()
	leader := member(ids[0], "name-"+ids[0])
	var party Party
	for _, id := range ids[1:] {
		mustInvite(t, p, ctx, leader, id)
		party = mustAccept(t, p, ctx, member(id, "name-"+id))
	}
	return party
}

func memberIDs(p Party) []string {
	out := make([]string, 0, len(p.Members))
	for _, m := range p.Members {
		out = append(out, m.CharacterID)
	}
	return out
}

func sameIDs(got Party, want ...string) bool {
	ids := memberIDs(got)
	if len(ids) != len(want) {
		return false
	}
	for i := range ids {
		if ids[i] != want[i] {
			return false
		}
	}
	return true
}

// --- creating and inviting ---------------------------------------------------

func TestPartyCreateMakesTheFounderLeader(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		party := mustCreate(t, p, ctx, member("alice", "Alice"))
		if party.ID == "" {
			t.Error("a party with no ID")
		}
		if party.Leader != "alice" {
			t.Errorf("leader is %q, want alice", party.Leader)
		}
		if !sameIDs(party, "alice") {
			t.Errorf("members are %v, want [alice]", memberIDs(party))
		}
		if party.Loot != LootFreeForAll {
			t.Errorf("loot rule is %q, want the default %q", party.Loot, LootFreeForAll)
		}
	})
}

func TestPartyCreateRefusesSomebodyAlreadyInOne(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		mustCreate(t, p, ctx, member("alice", "Alice"))
		if _, err := p.Create(ctx, member("alice", "Alice")); !errors.Is(err, ErrAlreadyInParty) {
			t.Errorf("second create gave %v, want ErrAlreadyInParty", err)
		}
	})
}

// Inviting founds a party, because the only reason to make one is to ask
// somebody into it.
func TestPartyInviteFoundsAPartyForTheInviter(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		party := mustInvite(t, p, ctx, member("alice", "Alice"), "bob")
		if party.Leader != "alice" || !sameIDs(party, "alice") {
			t.Errorf("inviting gave %+v, want a party of alice alone", party)
		}

		// The invitee is not a member until they answer.
		if _, ok := mustOf(t, p, ctx, "bob"); ok {
			t.Error("bob is in the party before accepting")
		}
	})
}

// The founder's name has to be carried, or the party has a leader nobody can
// name -- which surfaces later as being unable to kick or promote them.
func TestPartyInviteFoundsWithTheInvitersName(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		party := mustInvite(t, p, ctx, member("alice", "Alice"), "bob")
		if len(party.Members) != 1 || party.Members[0].Name != "Alice" {
			t.Errorf("founder is %+v, want a member named Alice", party.Members)
		}
	})
}

func TestPartyInviteYourselfIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		if _, err := p.Invite(ctx, member("alice", "Alice"), "alice"); err == nil {
			t.Error("alice invited herself")
		}
		if _, ok := mustOf(t, p, ctx, "alice"); ok {
			t.Error("a refused self-invite still founded a party")
		}
	})
}

func TestPartyInviteSomebodyAlreadyInAPartyIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		mustCreate(t, p, ctx, member("bob", "Bob"))
		_, err := p.Invite(ctx, member("alice", "Alice"), "bob")
		if !errors.Is(err, ErrAlreadyInParty) {
			t.Errorf("inviting bob gave %v, want ErrAlreadyInParty", err)
		}
	})
}

// Anyone may invite. Only the leader may kick.
func TestPartyAnyMemberMayInvite(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob")
		mustInvite(t, p, ctx, member("bob", "Bob"), "carol")
		party := mustAccept(t, p, ctx, member("carol", "Carol"))

		if !sameIDs(party, "alice", "bob", "carol") {
			t.Errorf("members are %v, want [alice bob carol]", memberIDs(party))
		}
		if party.Leader != "alice" {
			t.Errorf("bob inviting made the leader %q, want alice", party.Leader)
		}
	})
}

func TestPartyInviteToAFullPartyIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob", "carol", "dave") // testMaxParty
		_, err := p.Invite(ctx, member("alice", "Alice"), "erin")
		if !errors.Is(err, ErrPartyFull) {
			t.Errorf("inviting a fifth gave %v, want ErrPartyFull", err)
		}
	})
}

// --- answering ---------------------------------------------------------------

func TestPartyAcceptAddsInJoinOrder(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		party := join(t, p, ctx, "alice", "bob", "carol")
		if !sameIDs(party, "alice", "bob", "carol") {
			t.Errorf("members are %v, want them in join order", memberIDs(party))
		}
	})
}

func TestPartyAcceptWithNoInvitationIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		_, err := p.Accept(ctx, member("bob", "Bob"))
		if !errors.Is(err, ErrNoInvite) {
			t.Errorf("accepting nothing gave %v, want ErrNoInvite", err)
		}
	})
}

// An invitation is spent by answering it, whichever way.
func TestPartyAnInvitationIsSpentByAccepting(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		mustInvite(t, p, ctx, member("alice", "Alice"), "bob")
		mustAccept(t, p, ctx, member("bob", "Bob"))

		if _, err := p.Leave(ctx, "bob"); err != nil {
			t.Fatalf("bob leaving: %v", err)
		}
		if _, err := p.Accept(ctx, member("bob", "Bob")); !errors.Is(err, ErrNoInvite) {
			t.Errorf("the same invitation worked twice: %v", err)
		}
	})
}

func TestPartyDeclineDiscardsTheInvitation(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		mustInvite(t, p, ctx, member("alice", "Alice"), "bob")
		if err := p.Decline(ctx, "bob"); err != nil {
			t.Fatalf("declining: %v", err)
		}
		if _, err := p.Accept(ctx, member("bob", "Bob")); !errors.Is(err, ErrNoInvite) {
			t.Errorf("a declined invitation was still accepted: %v", err)
		}
	})
}

func TestPartyDecliningNothingIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		if err := p.Decline(ctx, "bob"); !errors.Is(err, ErrNoInvite) {
			t.Errorf("declining nothing gave %v, want ErrNoInvite", err)
		}
	})
}

// An invitation is a question asked in the moment. One that lingers gets
// accepted long after everybody has moved on.
func TestPartyAnExpiredInvitationIsRefused(t *testing.T) {
	eachPartiesWithTTL(t, 50*time.Millisecond, func(t *testing.T, p Parties) {
		ctx := context.Background()

		mustInvite(t, p, ctx, member("alice", "Alice"), "bob")
		time.Sleep(120 * time.Millisecond)

		if _, err := p.Accept(ctx, member("bob", "Bob")); !errors.Is(err, ErrNoInvite) {
			t.Errorf("an invitation outlived its TTL: %v", err)
		}
		if _, ok := mustOf(t, p, ctx, "bob"); ok {
			t.Error("bob joined on an expired invitation")
		}
	})
}

func TestPartyAnInvitationStandsUntilItExpires(t *testing.T) {
	eachPartiesWithTTL(t, 2*time.Second, func(t *testing.T, p Parties) {
		ctx := context.Background()

		mustInvite(t, p, ctx, member("alice", "Alice"), "bob")
		time.Sleep(50 * time.Millisecond)

		// The twin of the test above: without this, a TTL of zero would pass
		// it and break every invitation in the game.
		if _, err := p.Accept(ctx, member("bob", "Bob")); err != nil {
			t.Errorf("an invitation expired early: %v", err)
		}
	})
}

// The party can disband between the asking and the answering.
func TestPartyAcceptingIntoADisbandedPartyIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		mustInvite(t, p, ctx, member("alice", "Alice"), "bob")
		if _, err := p.Leave(ctx, "alice"); err != nil {
			t.Fatalf("alice leaving: %v", err)
		}

		if _, err := p.Accept(ctx, member("bob", "Bob")); !errors.Is(err, ErrNoParty) {
			t.Errorf("accepting into a disbanded party gave %v, want ErrNoParty", err)
		}
		if _, ok := mustOf(t, p, ctx, "bob"); ok {
			t.Error("bob joined a party that no longer exists")
		}
	})
}

// --- leaving -----------------------------------------------------------------

func TestPartyLeaveRemovesTheMember(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob", "carol")
		party, err := p.Leave(ctx, "bob")
		if err != nil {
			t.Fatalf("bob leaving: %v", err)
		}
		if !sameIDs(party, "alice", "carol") {
			t.Errorf("members are %v, want [alice carol]", memberIDs(party))
		}
		if _, ok := mustOf(t, p, ctx, "bob"); ok {
			t.Error("bob is still in a party after leaving")
		}
	})
}

// One person logging out must not scatter the other five.
func TestPartyTheLeaderLeavingPassesLeadership(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob", "carol")
		party, err := p.Leave(ctx, "alice")
		if err != nil {
			t.Fatalf("alice leaving: %v", err)
		}
		if party.Leader != "bob" {
			t.Errorf("leadership went to %q, want bob -- the next member", party.Leader)
		}
		if !sameIDs(party, "bob", "carol") {
			t.Errorf("members are %v, want [bob carol]", memberIDs(party))
		}
	})
}

// A party of one is not a party.
func TestPartyDisbandsWhenOneMemberIsLeft(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob")
		party, err := p.Leave(ctx, "bob")
		if err != nil {
			t.Fatalf("bob leaving: %v", err)
		}
		if len(party.Members) != 0 {
			t.Errorf("a disbanded party came back with %v, want no members", memberIDs(party))
		}

		// Both of them, not just the one who left: alice was not asked, but
		// she is not in a party either.
		for _, who := range []string{"alice", "bob"} {
			if _, ok := mustOf(t, p, ctx, who); ok {
				t.Errorf("%s is still in a party that disbanded", who)
			}
		}
	})
}

// Disbanding must free everyone to join something else.
func TestPartyDisbandingLetsMembersRegroup(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob")
		if _, err := p.Leave(ctx, "bob"); err != nil {
			t.Fatalf("bob leaving: %v", err)
		}

		// If the membership index had been left behind, this is ErrAlreadyInParty.
		if _, err := p.Create(ctx, member("alice", "Alice")); err != nil {
			t.Errorf("alice could not start a new party: %v", err)
		}
	})
}

func TestPartyLeavingNothingIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		if _, err := p.Leave(ctx, "alice"); !errors.Is(err, ErrNotInParty) {
			t.Errorf("leaving no party gave %v, want ErrNotInParty", err)
		}
	})
}

// --- the leader's powers -----------------------------------------------------

func TestPartyKickRemovesTheTarget(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob", "carol")
		party, err := p.Kick(ctx, "alice", "bob")
		if err != nil {
			t.Fatalf("kicking bob: %v", err)
		}
		if !sameIDs(party, "alice", "carol") {
			t.Errorf("members are %v, want [alice carol]", memberIDs(party))
		}
		if _, ok := mustOf(t, p, ctx, "bob"); ok {
			t.Error("bob is still in the party after being kicked")
		}
	})
}

func TestPartyOnlyTheLeaderMayKick(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob", "carol")
		if _, err := p.Kick(ctx, "bob", "carol"); !errors.Is(err, ErrNotLeader) {
			t.Errorf("bob kicking gave %v, want ErrNotLeader", err)
		}
		if party, _ := mustOf(t, p, ctx, "carol"); !sameIDs(party, "alice", "bob", "carol") {
			t.Errorf("a refused kick changed the roster to %v", memberIDs(party))
		}
	})
}

func TestPartyKickingSomebodyElsesMemberIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob")
		join(t, p, ctx, "carol", "dave")

		if _, err := p.Kick(ctx, "alice", "dave"); !errors.Is(err, ErrNotInParty) {
			t.Errorf("kicking another party's member gave %v, want ErrNotInParty", err)
		}
		if party, _ := mustOf(t, p, ctx, "dave"); !sameIDs(party, "carol", "dave") {
			t.Errorf("dave's party became %v", memberIDs(party))
		}
	})
}

// Kicking yourself would disband by the back door, skipping the handover.
func TestPartyTheLeaderMayNotKickThemselves(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob", "carol")
		if _, err := p.Kick(ctx, "alice", "alice"); !errors.Is(err, ErrNotInParty) {
			t.Errorf("alice kicked herself: %v", err)
		}
		if party, _ := mustOf(t, p, ctx, "alice"); party.Leader != "alice" {
			t.Errorf("leader is now %q", party.Leader)
		}
	})
}

func TestPartyKickingDownToOneDisbands(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob")
		party, err := p.Kick(ctx, "alice", "bob")
		if err != nil {
			t.Fatalf("kicking bob: %v", err)
		}
		if len(party.Members) != 0 {
			t.Errorf("kicking the last other member left %v", memberIDs(party))
		}
		if _, ok := mustOf(t, p, ctx, "alice"); ok {
			t.Error("alice is in a party by herself")
		}
	})
}

func TestPartyPromoteTransfersLeadership(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob", "carol")
		party, err := p.Promote(ctx, "alice", "carol")
		if err != nil {
			t.Fatalf("promoting carol: %v", err)
		}
		if party.Leader != "carol" {
			t.Errorf("leader is %q, want carol", party.Leader)
		}
		// The roster is untouched: promotion is not a reordering.
		if !sameIDs(party, "alice", "bob", "carol") {
			t.Errorf("promotion reordered the roster to %v", memberIDs(party))
		}
	})
}

// The old leader loses the powers along with the title.
func TestPartyPromotingHandsOverThePowers(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob", "carol")
		if _, err := p.Promote(ctx, "alice", "bob"); err != nil {
			t.Fatalf("promoting bob: %v", err)
		}

		if _, err := p.Kick(ctx, "alice", "carol"); !errors.Is(err, ErrNotLeader) {
			t.Errorf("the demoted leader kicked somebody: %v", err)
		}
		if _, err := p.Kick(ctx, "bob", "carol"); err != nil {
			t.Errorf("the new leader could not kick: %v", err)
		}
	})
}

func TestPartyOnlyTheLeaderMayPromote(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob", "carol")
		if _, err := p.Promote(ctx, "bob", "bob"); !errors.Is(err, ErrNotLeader) {
			t.Errorf("bob promoted himself: %v", err)
		}
		if party, _ := mustOf(t, p, ctx, "bob"); party.Leader != "alice" {
			t.Errorf("leader is %q, want alice", party.Leader)
		}
	})
}

func TestPartyPromotingANonMemberIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob")
		if _, err := p.Promote(ctx, "alice", "carol"); !errors.Is(err, ErrNotInParty) {
			t.Errorf("promoting an outsider gave %v, want ErrNotInParty", err)
		}
		if party, _ := mustOf(t, p, ctx, "alice"); party.Leader != "alice" {
			t.Errorf("a refused promotion moved leadership to %q", party.Leader)
		}
	})
}

func TestPartySetLootChangesTheRule(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob")
		party, err := p.SetLoot(ctx, "alice", LootRoundRobin)
		if err != nil {
			t.Fatalf("setting the loot rule: %v", err)
		}
		if party.Loot != LootRoundRobin {
			t.Errorf("rule is %q, want %q", party.Loot, LootRoundRobin)
		}

		// And every member sees it, since it decides who takes the next drop.
		if seen, _ := mustOf(t, p, ctx, "bob"); seen.Loot != LootRoundRobin {
			t.Errorf("bob still sees %q", seen.Loot)
		}
	})
}

func TestPartyOnlyTheLeaderMaySetLoot(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob")
		if _, err := p.SetLoot(ctx, "bob", LootRoundRobin); !errors.Is(err, ErrNotLeader) {
			t.Errorf("bob set the loot rule: %v", err)
		}
		if party, _ := mustOf(t, p, ctx, "alice"); party.Loot != LootFreeForAll {
			t.Errorf("the rule changed to %q anyway", party.Loot)
		}
	})
}

// An unknown rule is refused rather than stored: a rule the room does not
// recognise is loot nobody can pick up.
func TestPartyAnUnknownLootRuleIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob")
		if _, err := p.SetLoot(ctx, "alice", "whoever-shouts-loudest"); err == nil {
			t.Error("an unknown loot rule was accepted")
		}
		if party, _ := mustOf(t, p, ctx, "alice"); party.Loot != LootFreeForAll {
			t.Errorf("the rule became %q", party.Loot)
		}
	})
}

func TestPartyLeaderActionsNeedAParty(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		for name, call := range map[string]func() error{
			"kick":    func() error { _, err := p.Kick(ctx, "alice", "bob"); return err },
			"promote": func() error { _, err := p.Promote(ctx, "alice", "bob"); return err },
			"loot":    func() error { _, err := p.SetLoot(ctx, "alice", LootRoundRobin); return err },
		} {
			if err := call(); !errors.Is(err, ErrNotInParty) {
				t.Errorf("%s with no party gave %v, want ErrNotInParty", name, err)
			}
		}
	})
}

// --- reading and renaming ----------------------------------------------------

func TestPartyOfFindsEveryMember(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		want := join(t, p, ctx, "alice", "bob", "carol")
		for _, who := range []string{"alice", "bob", "carol"} {
			got, ok := mustOf(t, p, ctx, who)
			if !ok {
				t.Fatalf("%s is in no party", who)
			}
			if got.ID != want.ID {
				t.Errorf("%s is in party %q, want %q", who, got.ID, want.ID)
			}
			if !sameIDs(got, "alice", "bob", "carol") {
				t.Errorf("%s sees members %v", who, memberIDs(got))
			}
		}
	})
}

func TestPartyOfSomebodyInNoPartyIsNotAnError(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		party, ok, err := p.Of(ctx, "nobody")
		if err != nil {
			t.Fatalf("looking up a stranger: %v", err)
		}
		if ok {
			t.Errorf("a stranger is in %+v", party)
		}
	})
}

// A party can hold a member it has never seen online.
func TestPartyRenameUpdatesTheRoster(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		join(t, p, ctx, "alice", "bob")
		if err := p.Rename(ctx, "bob", "Bobbie"); err != nil {
			t.Fatalf("renaming bob: %v", err)
		}

		party, _ := mustOf(t, p, ctx, "alice")
		for _, m := range party.Members {
			if m.CharacterID == "bob" && m.Name != "Bobbie" {
				t.Errorf("bob is named %q on alice's roster, want Bobbie", m.Name)
			}
		}
	})
}

func TestPartyRenamingSomebodyInNoPartyIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		if err := p.Rename(ctx, "alice", "Alice"); !errors.Is(err, ErrNotInParty) {
			t.Errorf("renaming outside a party gave %v, want ErrNotInParty", err)
		}
	})
}

func TestPartyNamesReadsTheRoster(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		party := join(t, p, ctx, "alice", "bob")
		names := party.Names()
		if len(names) != 2 || names[0] != "name-alice" || names[1] != "name-bob" {
			t.Errorf("names are %v", names)
		}
	})
}

// --- racing ------------------------------------------------------------------

// Two simultaneous accepts must not both take the last slot.
//
// This is the reason every method is one script. A read of the roster followed
// by a write of it leaves exactly enough room between them for a party of
// seven.
func TestPartyConcurrentAcceptsNeverExceedCapacity(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		mustCreate(t, p, ctx, member("alice", "Alice"))

		const hopefuls = 20
		for i := range hopefuls {
			mustInvite(t, p, ctx, member("alice", "Alice"), fmt.Sprintf("hopeful-%d", i))
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		var joined int

		for i := range hopefuls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("hopeful-%d", i)
				if _, err := p.Accept(ctx, member(id, id)); err == nil {
					mu.Lock()
					joined++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if want := testMaxParty - 1; joined != want {
			t.Errorf("%d of %d accepts succeeded, want exactly %d", joined, hopefuls, want)
		}

		party, ok := mustOf(t, p, ctx, "alice")
		if !ok {
			t.Fatal("alice's party is gone")
		}
		if len(party.Members) != testMaxParty {
			t.Errorf("the party holds %d members, over the cap of %d",
				len(party.Members), testMaxParty)
		}
	})
}

// Everybody leaving at once must not leave a roster behind.
func TestPartyConcurrentLeavesSettleCleanly(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		ids := []string{"alice", "bob", "carol", "dave"}
		join(t, p, ctx, ids...)

		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.Leave(ctx, id)
			}()
		}
		wg.Wait()

		for _, id := range ids {
			if party, ok := mustOf(t, p, ctx, id); ok {
				t.Errorf("%s is still in %+v after everybody left", id, party)
			}
		}
	})
}

// --- Redis only --------------------------------------------------------------

// Two nodes are two clients against one table.
func TestRedisPartiesAreSharedBetweenNodes(t *testing.T) {
	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set MMO_TEST_REDIS_ADDR to run the Redis party tests")
	}

	ctx := context.Background()
	nodeA := openRedisParties(t, addr, InviteTTL)

	// A second client on the same namespace: a different node, same table.
	nodeB := NewRedisParties(nodeA.client, nodeA.prefix, testMaxParty)

	if _, err := nodeA.Invite(ctx, member("alice", "Alice"), "bob"); err != nil {
		t.Fatalf("alice inviting on node A: %v", err)
	}
	// Answered on the other node, which is the normal case: a party is what
	// two people on two nodes have in common.
	party, err := nodeB.Accept(ctx, member("bob", "Bob"))
	if err != nil {
		t.Fatalf("bob accepting on node B: %v", err)
	}
	if !sameIDs(party, "alice", "bob") {
		t.Errorf("node B sees %v", memberIDs(party))
	}

	if seen, ok, err := nodeA.Of(ctx, "bob"); err != nil || !ok || seen.ID != party.ID {
		t.Errorf("node A sees bob in %+v (%v, %v)", seen, ok, err)
	}

	if _, err := nodeB.Promote(ctx, "alice", "bob"); err != nil {
		t.Fatalf("promoting across nodes: %v", err)
	}
	if seen, _, _ := nodeA.Of(ctx, "alice"); seen.Leader != "bob" {
		t.Errorf("node A still thinks the leader is %q", seen.Leader)
	}
}

// A disbanded party must leave nothing behind.
//
// Redis-only because it is not observable through the interface: Of resolves
// the roster through the membership index, so an orphaned roster reads exactly
// like no party at all. The leak is silent and grows with every party the
// server has ever formed.
func TestRedisPartiesLeaveNoBookkeepingBehind(t *testing.T) {
	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set MMO_TEST_REDIS_ADDR to run the Redis party tests")
	}

	ctx := context.Background()
	p := openRedisParties(t, addr, InviteTTL)

	count := func(key string) int64 {
		t.Helper()
		n, err := p.client.HLen(ctx, key).Result()
		if err != nil {
			t.Fatalf("counting %s: %v", key, err)
		}
		return n
	}

	join(t, p, ctx, "alice", "bob", "carol")
	if _, err := p.Leave(ctx, "carol"); err != nil {
		t.Fatalf("carol leaving: %v", err)
	}
	// Down to two: still a party, and carol is out of the index.
	if got := count(p.memberOfKey()); got != 2 {
		t.Errorf("the membership index holds %d after one left, want 2", got)
	}

	if _, err := p.Leave(ctx, "bob"); err != nil {
		t.Fatalf("bob leaving: %v", err)
	}
	if got := count(p.partiesKey()); got != 0 {
		t.Errorf("%d rosters survive a disband", got)
	}
	if got := count(p.memberOfKey()); got != 0 {
		t.Errorf("%d membership entries survive a disband", got)
	}
}

// An invitation nobody answers must not sit in Redis forever.
func TestRedisPartiesExpireUnansweredInvitations(t *testing.T) {
	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set MMO_TEST_REDIS_ADDR to run the Redis party tests")
	}

	ctx := context.Background()
	p := openRedisParties(t, addr, 50*time.Millisecond)

	mustInvite(t, p, ctx, member("alice", "Alice"), "bob")
	if n, _ := p.client.Exists(ctx, p.inviteKey("bob")).Result(); n != 1 {
		t.Fatal("the invitation was not written")
	}

	time.Sleep(120 * time.Millisecond)

	// Redis does the expiring, so there is nothing to sweep and no deadline
	// crossing the wire.
	if n, _ := p.client.Exists(ctx, p.inviteKey("bob")).Result(); n != 0 {
		t.Error("an unanswered invitation outlived its TTL in Redis")
	}
}

// An invitation can be answered after joining somewhere else.
//
// Inviting somebody already in a party is refused, so this looks unreachable.
// It is not: the invitation is written first and answered later, and starting
// a party of your own in between touches no invitation at all.
//
// Without the check bob ends up in two parties, and since the membership index
// holds only one, he is on a roster that every read disagrees with.
func TestPartyAcceptingWhileAlreadyPartiedIsRefused(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		mustInvite(t, p, ctx, member("alice", "Alice"), "bob")
		// Bob does not answer. He starts his own instead.
		own := mustCreate(t, p, ctx, member("bob", "Bob"))

		if _, err := p.Accept(ctx, member("bob", "Bob")); !errors.Is(err, ErrAlreadyInParty) {
			t.Errorf("bob joined a second party: %v", err)
		}

		party, ok := mustOf(t, p, ctx, "bob")
		if !ok || party.ID != own.ID {
			t.Errorf("bob is in %+v, want his own party %q", party, own.ID)
		}
		if seen, _ := mustOf(t, p, ctx, "alice"); seen.Has("bob") {
			t.Error("bob is on alice's roster as well")
		}
	})
}

// A character holds one invitation at a time: the second replaces the first.
//
// Keyed by invitee rather than by party and invitee, which is what makes this
// so. It is the right shape for a client that shows one prompt -- but it means
// being asked by somebody else quietly withdraws the first offer, so it is
// asserted rather than left as an accident of the key layout.
func TestPartyASecondInvitationReplacesTheFirst(t *testing.T) {
	eachParties(t, func(t *testing.T, p Parties) {
		ctx := context.Background()

		mustInvite(t, p, ctx, member("alice", "Alice"), "bob")
		mustInvite(t, p, ctx, member("carol", "Carol"), "bob")

		party := mustAccept(t, p, ctx, member("bob", "Bob"))
		if party.Leader != "carol" {
			t.Errorf("bob joined %q's party, want carol -- who asked last", party.Leader)
		}
		if seen, _ := mustOf(t, p, ctx, "alice"); seen.Has("bob") {
			t.Error("bob is on alice's roster too")
		}
	})
}

// A character whose party vanished must not be stranded.
//
// Redis-only because it cannot happen in memory: one map under one mutex has no
// way to lose half a party. Redis does -- this is ephemeral data in a store
// that may be running with an eviction policy, and the whole argument for
// putting parties there is that losing them is survivable.
//
// Survivable means the character can regroup. Without the cleanup they are
// listed as a member of a party that no longer exists, and since starting one
// checks that index, they can never party again for as long as the entry sits
// there.
func TestRedisPartiesRecoverFromAVanishedRoster(t *testing.T) {
	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set MMO_TEST_REDIS_ADDR to run the Redis party tests")
	}

	ctx := context.Background()
	p := openRedisParties(t, addr, InviteTTL)

	join(t, p, ctx, "alice", "bob")

	// The roster is evicted; the membership index survives.
	if err := p.client.Del(ctx, p.partiesKey()).Err(); err != nil {
		t.Fatalf("dropping the roster: %v", err)
	}

	if party, ok, err := p.Of(ctx, "alice"); err != nil || ok {
		t.Errorf("alice is in %+v (%v, %v), want no party", party, ok, err)
	}
	if _, err := p.Create(ctx, member("alice", "Alice")); err != nil {
		t.Errorf("alice is stranded and cannot start a new party: %v", err)
	}
	if party, ok := mustOf(t, p, ctx, "alice"); !ok || party.Leader != "alice" {
		t.Errorf("alice's new party is %+v", party)
	}
}
