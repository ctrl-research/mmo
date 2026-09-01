package gateway

import (
	"fmt"
	"testing"
	"time"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// A full party, formed over the wire.
//
// Every party test until now has been at the world level, calling into the
// session directly. That covers the rules -- who may kick, what a full party
// refuses, which instance a party routes to -- and skips the part a player
// actually does: send a message, wait to be told, answer. The gateway's own
// test server did not even have a party table wired into it, so a party
// message over a socket had nowhere to go and no test noticed.
//
// Six because that is the cap, and because M7's exit is about six players. What
// this does not do is take them into a dungeon; see the note at the end.

const partySize = 6

func delverName(i int) string { return fmt.Sprintf("delver%d", i) }

// waitFor reads until a message matches, keeping everything else.
func waitFor(t *testing.T, c *client, what string, match func(*mmov1.ServerMessage) bool) *mmov1.ServerMessage {
	t.Helper()

	m := c.findInInbox(20*time.Second, match)
	if m == nil {
		t.Fatalf("timed out waiting for %s: %s", what, c.why())
	}
	return m
}

// Six players form a party by asking and answering, the way a player does.
func TestSixPlayersFormAPartyOverTheWire(t *testing.T) {
	ts := newTestServer(t)

	clients := make([]*client, 0, partySize)
	for i := range partySize {
		name := delverName(i)
		c := ts.dial(t)
		c.hello(ts.ticket(t, name), ProtocolVersion)
		c.awaitWelcome()
		clients = append(clients, c)

		if i == 0 {
			continue
		}

		// Invited by name. Resolving that name is a presence lookup, and
		// presence is announced when the character is placed -- which is why
		// this works immediately after a Welcome rather than a moment later.
		clients[0].send(&mmov1.ClientMessage{
			Body: &mmov1.ClientMessage_Party{Party: &mmov1.PartyAction{
				Kind: mmov1.PartyAction_KIND_INVITE, Target: name,
			}},
		})
		waitFor(t, c, name+" to be invited", func(m *mmov1.ServerMessage) bool {
			return m.GetEvent().GetPartyInvite() != nil
		})
		c.send(&mmov1.ClientMessage{
			Body: &mmov1.ClientMessage_Party{Party: &mmov1.PartyAction{
				Kind: mmov1.PartyAction_KIND_ACCEPT,
			}},
		})
	}

	// Everyone ends up seeing the same six. A roster the members disagree
	// about is worse than no roster: it decides who shares a dungeon.
	for i, c := range clients {
		state := waitFor(t, c, delverName(i)+" to see a full party",
			func(m *mmov1.ServerMessage) bool {
				p := m.GetEvent().GetParty()
				return p != nil && len(p.GetMembers()) == partySize
			}).GetEvent().GetParty()

		if state.GetLeaderCharacterId() == "" {
			t.Errorf("%s sees a party with no leader", delverName(i))
		}
		seen := make(map[string]bool, partySize)
		for _, m := range state.GetMembers() {
			seen[m.GetName()] = true
		}
		for j := range partySize {
			if !seen[delverName(j)] {
				t.Errorf("%s does not see %s in the party", delverName(i), delverName(j))
			}
		}
	}
}

// A seventh player is refused, over the wire.
func TestAFullPartyRefusesOverTheWire(t *testing.T) {
	ts := newTestServer(t)

	leader := ts.dial(t)
	leader.hello(ts.ticket(t, "leader"), ProtocolVersion)
	leader.awaitWelcome()

	for i := 1; i < partySize; i++ {
		name := delverName(i)
		c := ts.dial(t)
		c.hello(ts.ticket(t, name), ProtocolVersion)
		c.awaitWelcome()

		leader.send(&mmov1.ClientMessage{
			Body: &mmov1.ClientMessage_Party{Party: &mmov1.PartyAction{
				Kind: mmov1.PartyAction_KIND_INVITE, Target: name,
			}},
		})
		waitFor(t, c, name+" to be invited", func(m *mmov1.ServerMessage) bool {
			return m.GetEvent().GetPartyInvite() != nil
		})
		c.send(&mmov1.ClientMessage{
			Body: &mmov1.ClientMessage_Party{Party: &mmov1.PartyAction{
				Kind: mmov1.PartyAction_KIND_ACCEPT,
			}},
		})
	}

	waitFor(t, leader, "the party to fill", func(m *mmov1.ServerMessage) bool {
		p := m.GetEvent().GetParty()
		return p != nil && len(p.GetMembers()) == partySize
	})

	spare := ts.dial(t)
	spare.hello(ts.ticket(t, "spare"), ProtocolVersion)
	spare.awaitWelcome()

	leader.send(&mmov1.ClientMessage{
		Body: &mmov1.ClientMessage_Party{Party: &mmov1.PartyAction{
			Kind: mmov1.PartyAction_KIND_INVITE, Target: "spare",
		}},
	})

	// The refusal reaches the person who asked, in words. A cap enforced
	// silently is a player clicking invite and wondering.
	waitFor(t, leader, "the party to say it is full", func(m *mmov1.ServerMessage) bool {
		s := m.GetEvent().GetSystem()
		return s != nil && s.GetBody() != "" && s.GetBody() != "invited spare"
	})
}
