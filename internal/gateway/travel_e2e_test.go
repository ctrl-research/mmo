package gateway

import (
	"testing"
	"time"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// The world map and fast travel, over a real socket.
//
// The unit tests cover what a transfer does; these cover that a client can ask
// for one at all -- the client message reaching the session, and the answer
// reaching the client -- which is the part that silently breaks when a oneof
// gains a field and a switch does not.

func (c *client) openWorldMap() {
	c.send(&mmov1.ClientMessage{
		Body: &mmov1.ClientMessage_OpenWorldMap{OpenWorldMap: &mmov1.OpenWorldMap{}},
	})
}

func (c *client) newChannel() {
	c.send(&mmov1.ClientMessage{Body: &mmov1.ClientMessage_Travel{
		Travel: &mmov1.Travel{Destination: &mmov1.Travel_NewChannel{NewChannel: true}},
	}})
}

// awaitEvent reads until an event matching the predicate arrives.
func (c *client) awaitEvent(within time.Duration, match func(*mmov1.Event) bool) *mmov1.Event {
	c.t.Helper()

	m := c.findInInbox(within, func(m *mmov1.ServerMessage) bool {
		ev := m.GetEvent()
		return ev != nil && match(ev)
	})
	if m == nil {
		return nil
	}
	return m.GetEvent()
}

func TestWorldMapListsZonesAndTheCurrentChannel(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)

	c.hello(ts.ticket(t, "cartographer"), ProtocolVersion)
	welcome := c.awaitWelcome()

	c.openWorldMap()
	ev := c.awaitEvent(5*time.Second, func(e *mmov1.Event) bool { return e.GetWorldMap() != nil })
	if ev == nil {
		t.Fatal("no world map came back")
	}
	m := ev.GetWorldMap()

	if m.GetCurrentMapId() != "test" {
		t.Errorf("current map is %q, want test", m.GetCurrentMapId())
	}
	if m.GetCurrentInstanceId() != welcome.GetInstanceId() {
		t.Errorf("world map says instance %d, the Welcome said %d",
			m.GetCurrentInstanceId(), welcome.GetInstanceId())
	}
	if len(m.GetMaps()) < 2 {
		t.Errorf("world map lists %d zones, want the whole world", len(m.GetMaps()))
	}

	// A brand new character has been nowhere, and listing waypoints they have
	// not found would give away what is out there.
	if len(m.GetWaypoints()) != 0 {
		t.Errorf("a new character was shown %d waypoints", len(m.GetWaypoints()))
	}

	// Exactly one channel, and the player is in it.
	channels := m.GetChannels()
	if len(channels) != 1 || !channels[0].GetCurrent() {
		t.Fatalf("channels are %v, want exactly one marked current", channels)
	}
	if channels[0].GetPlayers() != 1 {
		t.Errorf("the channel reports %d players, want 1", channels[0].GetPlayers())
	}
}

// The button that gets a player out of the instance they are in. At hobby
// scale there is usually only one channel, so it has to be able to make one.
func TestNewChannelPutsThePlayerSomewhereElse(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)

	c.hello(ts.ticket(t, "hopper"), ProtocolVersion)
	first := c.awaitWelcome()

	c.newChannel()

	// A second Welcome is how a client learns it has moved.
	deadline := time.Now().Add(10 * time.Second)
	var second *mmov1.Welcome
	for time.Now().Before(deadline) {
		c.drain(200 * time.Millisecond)
		for _, m := range c.inbox {
			w := m.GetWelcome()
			if w != nil && w.GetInstanceId() != first.GetInstanceId() {
				second = w
			}
		}
		if second != nil {
			break
		}
	}
	if second == nil {
		t.Fatal("the client was never welcomed into another channel")
	}
	if second.GetMapId() != first.GetMapId() {
		t.Errorf("a channel switch moved the player to %q, want %q",
			second.GetMapId(), first.GetMapId())
	}

	// And the world map now shows both, with the player in the new one.
	c.openWorldMap()
	ev := c.awaitEvent(5*time.Second, func(e *mmov1.Event) bool {
		wm := e.GetWorldMap()
		return wm != nil && wm.GetCurrentInstanceId() == second.GetInstanceId()
	})
	if ev == nil {
		t.Fatal("the world map never caught up with the channel switch")
	}
	if n := len(ev.GetWorldMap().GetChannels()); n != 2 {
		t.Errorf("the world map lists %d channels, want 2", n)
	}
}

// A client naming a waypoint it has never found must be refused, and told why:
// a button that does nothing reads as broken.
func TestTravelToAnUnknownWaypointIsRefused(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)

	c.hello(ts.ticket(t, "chancer"), ProtocolVersion)
	c.awaitWelcome()

	c.send(&mmov1.ClientMessage{Body: &mmov1.ClientMessage_Travel{
		Travel: &mmov1.Travel{
			Destination: &mmov1.Travel_WaypointId{WaypointId: "wp_annex"},
		},
	}})

	ev := c.awaitEvent(5*time.Second, func(e *mmov1.Event) bool {
		return e.GetPortalRefused() != nil
	})
	if ev == nil {
		t.Fatal("the client was never told why it did not travel")
	}
	if reason := ev.GetPortalRefused().GetReason(); reason == "" {
		t.Error("the refusal has no reason, which reads as the game ignoring the request")
	}
}
