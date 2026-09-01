package world

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ctrl-research/mmo/internal/bus"
	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"github.com/google/uuid"
)

// A gateway that runs no simulation.
//
// The cluster suite covers world nodes talking to each other. This covers the
// other seam: a process that terminates a socket, owns no rooms, holds no
// leases, and plays a character entirely through another process.
//
// The gateway here gets its own bus connection and is given no Node at all, so
// there is nothing in the process it could be reaching by accident -- which is
// the only way to be sure the split is real.

// gatewayBus returns a third connection, standing in for a gateway process.
func gatewayBus(t *testing.T, shared bus.Bus) bus.Bus {
	t.Helper()

	url := os.Getenv("MMO_TEST_NATS_URL")
	if url == "" {
		// One process, one in-process bus: the nodes share it too. Not as
		// strong as a separate connection, which is what MMO_TEST_NATS_URL
		// gives, but the gateway still holds no Node.
		return shared
	}

	b, err := bus.Connect(url)
	if err != nil {
		t.Fatalf("connect a gateway to nats at %s: %v", url, err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// remoteGateway is a RemoteWorld with no world node of its own.
func (c *cluster) remoteGateway(t *testing.T) *RemoteWorld {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRemoteWorld(context.Background(), gatewayBus(t, c.bus), c.dir, "gateway-1", log)
}

// gatewayEnter logs a character in through a gateway that runs no simulation.
func gatewayEnter(t *testing.T, c *cluster, name string) (PlayerSession, *recordingSink) {
	t.Helper()

	account, character := c.character(name)
	sink := &recordingSink{}

	play, err := c.remoteGateway(t).Enter(context.Background(), account, character, sink)
	if err != nil {
		t.Fatalf("entering the world through a gateway: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		play.Close(ctx)
	})
	return play, sink
}

func TestAGatewayWithNoWorldNodePlacesACharacter(t *testing.T) {
	c := newCluster(t)
	play, _ := gatewayEnter(t, c, "Alice")

	if play.Name() != "Alice" {
		t.Errorf("the session is named %q, want Alice", play.Name())
	}
	if play.EntityID() == 0 {
		t.Error("the character was placed with no entity")
	}

	// The gateway holds no room, and is not supposed to: a room handle is an
	// in-process reference and this process is not running the room.
	if handle := play.Handle(); handle != nil {
		t.Errorf("a gateway was given a room handle (%T)", handle)
	}
	if !play.InRoom() {
		t.Error("the character is not in a room")
	}
}

// Everything bound for the player's screen has to cross back.
func TestAGatewayReceivesWhatTheWorldSends(t *testing.T) {
	c := newCluster(t)
	_, sink := gatewayEnter(t, c, "Alice")

	waitFor(t, "the world to send the gateway something", func() bool {
		return sink.count() > 0
	})
}

// Input reaches the simulation through two processes.
func TestAGatewaysInputMovesTheCharacter(t *testing.T) {
	c := newCluster(t)
	play, sink := gatewayEnter(t, c, "Alice")
	ctx := context.Background()

	// A snapshot, specifically: the first things a session sends are the
	// welcome, the loadout and the passives, none of which carry a position.
	waitFor(t, "the first snapshot", func() bool {
		_, ok := sink.selfX()
		return ok
	})

	start, _ := sink.selfX()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		play.Input(ctx, 1, simRight())

		if x, ok := sink.selfX(); ok && x != start {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the character never moved, so input is not crossing to the world node")
}

// A refusal the player is meant to read comes back as one.
//
// The gateway prints these straight to the player, so a refusal that arrived
// as a bus error would show somebody "context deadline exceeded" when what
// happened is that they tried to equip something they do not own.
func TestAGatewaySeesARefusal(t *testing.T) {
	c := newCluster(t)
	play, _ := gatewayEnter(t, c, "Alice")

	err := play.ApplyItemAction(context.Background(), ItemAction{
		Kind:   ItemEquip,
		ItemID: uuid.New(), // an item nobody has
	})
	if err == nil {
		t.Fatal("equipping an item that does not exist was accepted")
	}
	if err.Error() == "" {
		t.Error("the refusal arrived with nothing to show the player")
	}
}

// A character the world node is not holding is reported as gone, so the
// gateway can close the connection rather than talking to nobody.
func TestAGatewayIsToldWhenTheCharacterHasGone(t *testing.T) {
	c := newCluster(t)
	play, _ := gatewayEnter(t, c, "Alice")
	ctx := context.Background()

	play.Close(ctx)

	// Checked before anything else: a later command would learn the character
	// has gone from the reply, which would make this pass whether or not
	// closing had said so itself.
	if play.InRoom() {
		t.Error("a character the gateway has closed is still reported as in a room")
	}

	if err := play.ApplyItemAction(ctx, ItemAction{
		Kind: ItemEquip, ItemID: uuid.New(),
	}); err == nil {
		t.Error("a command for a character that has left was accepted")
	}
}

// The gateway's own actions reach the rest of the cluster.
func TestAGatewaysChatReachesAWorldNode(t *testing.T) {
	c := newCluster(t)
	play, _ := gatewayEnter(t, c, "Alice")

	err := play.SendChat(context.Background(), &mmov1.ChatSend{
		Channel: mmov1.ChatChannel_CHAT_CHANNEL_GLOBAL,
		Body:    "hello from a gateway",
	})
	if err != nil {
		t.Fatalf("sending chat through a gateway: %v", err)
	}
}

// A gateway that cannot find a world node says so rather than hanging.
func TestAGatewayWithNoWorldNodeRefusesToPlace(t *testing.T) {
	c := newCluster(t)

	empty := NewRemoteWorld(context.Background(), gatewayBus(t, c.bus), emptyDirectory{}, "gateway-1",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := empty.Enter(context.Background(), uuid.New(), uuid.New(), &recordingSink{})
	if err == nil {
		t.Fatal("a gateway placed a character with no world node running")
	}
}

// simRight is full-speed movement to the right.
func simRight() sim.Input { return sim.Input{MoveX: 1000} }

// emptyDirectory knows about no world nodes at all.
type emptyDirectory struct{ directory.Directory }

func (emptyDirectory) LiveNodes(context.Context) ([]directory.NodeID, error) {
	return nil, nil
}

// A character already in play is refused, not hunted for.
//
// The lease is held cluster-wide, so every other world node would say the same
// thing. Trying them all turns one refusal into one per node, and the player
// waits out every timeout to be told what the first node already knew.
func TestAGatewayDoesNotShopAroundForABusyCharacter(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Alice")
	ctx := context.Background()

	first, err := c.remoteGateway(t).Enter(ctx, account, character, &recordingSink{})
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	t.Cleanup(func() { first.Close(context.Background()) })

	// A second gateway, as a second process would be.
	_, err = c.remoteGateway(t).Enter(ctx, account, character, &recordingSink{})
	if !errors.Is(err, ErrCharacterBusy) {
		t.Errorf("a second login gave %v, want ErrCharacterBusy", err)
	}
}

// "Nothing is running" and "everything refused" are different problems.
//
// The first is an empty cluster and the second is a character that cannot be
// placed. An operator reading the log needs to know which.
func TestAGatewaySaysWhenThereIsNoWorldNode(t *testing.T) {
	c := newCluster(t)

	empty := NewRemoteWorld(context.Background(), gatewayBus(t, c.bus), emptyDirectory{}, "gateway-1",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := empty.Enter(context.Background(), uuid.New(), uuid.New(), &recordingSink{})
	if err == nil {
		t.Fatal("a gateway placed a character with no world node running")
	}
	if !strings.Contains(err.Error(), "no world node is running") {
		t.Errorf("the error is %q, which does not say the cluster is empty", err)
	}
}

// A gateway that does not say where to send the player's messages is refused.
//
// Accepting it would take the character's lease, place them in a room, and
// then publish every message they are owed to an empty subject -- a player
// logged in, holding their character hostage, seeing nothing.
func TestAWorldNodeRefusesAGatewayWithNoReturnAddress(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Alice")

	reply := c.a.acceptEnter(context.Background(), &mmov1.EnterRequest{
		AccountId:   account.String(),
		CharacterId: character.String(),
	})
	if reply.GetError() == "" {
		t.Fatal("a gateway with no return address was accepted")
	}

	// And the character is still free to log in properly.
	if _, ok := c.a.held(character); ok {
		t.Error("the refused login took the character anyway")
	}
}

// A player keeps receiving after the handshake that logged them in is over.
//
// The gateway calls Enter with the handshake's context, which is cancelled as
// soon as the handshake finishes -- so a subscription opened against it is
// closed before the player has finished loading. The world node then publishes
// into a subject nobody is listening on, and every test that passed
// context.Background() sees nothing wrong.
//
// The symptom in a browser was a character standing in a rendered world with
// "0 snaps" on the debug overlay and no health, receiving nothing at all.
func TestAGatewayKeepsReceivingAfterTheHandshakeContextEnds(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Alice")
	sink := &recordingSink{}

	// Exactly the shape of the real caller: a short-lived context that is
	// cancelled the moment Enter returns.
	handshake, cancel := context.WithTimeout(context.Background(), 15*time.Second)

	play, err := c.remoteGateway(t).Enter(handshake, account, character, sink)
	if err != nil {
		cancel()
		t.Fatalf("entering the world: %v", err)
	}
	cancel()
	t.Cleanup(func() { play.Close(context.Background()) })

	waitFor(t, "snapshots to keep arriving after the handshake ended", func() bool {
		_, ok := sink.selfX()
		return ok
	})

	// And they keep coming: one that arrived in the race before cancel would
	// pass the check above.
	before := sink.count()
	waitFor(t, "more messages after that", func() bool { return sink.count() > before })
}
