package world

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ctrl-research/mmo/internal/bus"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/content/contenttest"
	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"github.com/ctrl-research/mmo/internal/world/stats"
	"google.golang.org/protobuf/proto"
)

// Driving a room in another process, directly.
//
// The cluster suite covers this through transfers, which is the path that
// matters most -- but it only reaches the commands a transfer happens to use.
// These drive the handle itself, so the calls a player makes every tick are
// covered by something that fails when they stop arriving rather than by
// inference from a test about portals.
//
// No database: hosting a room needs a directory, content and a bus, and
// nothing here logs anybody in.

const testInstance = directory.InstanceID(4242)

type remotePair struct {
	host   *Node // hosts the room
	caller *Node // drives it from "another process"
	handle *remoteRoom
}

// newRemotePair returns two nodes with separate registries, and a handle from
// one to a room on the other.
func newRemotePair(t *testing.T) *remotePair {
	t.Helper()

	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load content: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dir := directory.NewMemory("host")
	t.Cleanup(func() { dir.Close() })
	busA, busB := testBuses(t)

	newNode := func(id string, b bus.Bus) *Node {
		n, err := NewNode(Config{
			Directory: dir,
			Bus:       b,
			// A registry each. Sharing one is what used to let these reach
			// each other by pointer, which is exactly what must not happen
			// here.
			Rooms:      NewRegistry(),
			NodeID:     id,
			Content:    game,
			DefaultMap: "test",
			Logger:     log.With("node", id),
		})
		if err != nil {
			t.Fatalf("new node %s: %v", id, err)
		}
		if err := n.Start(ctx); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		t.Cleanup(n.Stop)
		return n
	}

	host := newNode("host", busA)
	caller := newNode("caller", busB)

	// The room exists on the host only.
	m := game.Maps["test"]
	if _, err := host.ensureRoom(directory.Instance{
		ID: testInstance, Node: "host", Capacity: m.Capacity,
	}, m); err != nil {
		t.Fatalf("hosting the room: %v", err)
	}

	handle, err := caller.resolve(testInstance, "host", "char-1")
	if err != nil {
		t.Fatalf("resolving a room on another node: %v", err)
	}
	remote, ok := handle.(*remoteRoom)
	if !ok {
		t.Fatalf("resolve returned %T, want a bus-backed handle", handle)
	}
	return &remotePair{host: host, caller: caller, handle: remote}
}

// recordingSink collects everything a room sends to one connection.
type recordingSink struct {
	mu       sync.Mutex
	messages []*mmov1.ServerMessage
	closed   bool
	code     uint32
	reason   string
}

func (s *recordingSink) Send(msg *mmov1.ServerMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
}

func (s *recordingSink) Close(code uint32, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed, s.code, s.reason = true, code, reason
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

func (s *recordingSink) closeReason() (bool, uint32, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed, s.code, s.reason
}

func joinRemote(t *testing.T, p *remotePair) room.EntityID {
	t.Helper()

	id, err := p.handle.Join(context.Background(), room.JoinSpec{
		CharacterID: "char-1",
		Name:        "Alice",
		Progress:    room.Progress{Level: 1, MapID: "test"},
		Fresh:       true,
	})
	if err != nil {
		t.Fatalf("joining a room on another node: %v", err)
	}
	return id
}

func TestRemoteRoomJoinAndCapture(t *testing.T) {
	p := newRemotePair(t)
	id := joinRemote(t, p)

	if id == 0 {
		t.Fatal("joined with no entity id")
	}

	snap, ok := p.handle.Capture(context.Background(), id)
	if !ok {
		t.Fatal("the character is not in the room it just joined")
	}
	if snap.Progress.MapID != "test" {
		t.Errorf("captured map %q, want test", snap.Progress.MapID)
	}
	if snap.State.MaxHP == 0 {
		t.Error("captured no maximum health, so the state did not cross")
	}
}

// Input is the highest-frequency call in the game and the one that is
// published rather than waited for, so nothing else would notice it silently
// going nowhere.
func TestRemoteRoomAppliesInput(t *testing.T) {
	p := newRemotePair(t)
	id := joinRemote(t, p)
	ctx := context.Background()

	before, ok := p.handle.Capture(ctx, id)
	if !ok {
		t.Fatal("no character to move")
	}

	// Held for long enough to be applied over several ticks; the room replays
	// the last input when its queue starves, so one is enough to keep moving.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.handle.Input(ctx, id, 1, sim.Input{MoveX: 1000})

		after, ok := p.handle.Capture(ctx, id)
		if ok && after.State.X != before.State.X {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the character never moved, so input is not reaching the room")
}

func TestRemoteRoomLeaveRemovesTheCharacter(t *testing.T) {
	p := newRemotePair(t)
	id := joinRemote(t, p)
	ctx := context.Background()

	p.handle.Leave(ctx, id)

	// Published rather than awaited, so the room may not have applied it yet.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := p.handle.Capture(ctx, id); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the character is still in the room after leaving; a ghost nobody can remove")
}

// A room that is not hosted reports itself closed rather than failing.
//
// The join path retries placement on a closed room, and treating this as an
// error instead would turn "the room retired while you were being placed" --
// which happens whenever an idle room times out -- into a failed login.
func TestRemoteRoomReportsAMissingRoomAsClosed(t *testing.T) {
	p := newRemotePair(t)

	gone, err := p.caller.resolve(directory.InstanceID(999999), "host", "char-1")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}

	_, err = gone.Join(context.Background(), room.JoinSpec{
		CharacterID: "char-1", Name: "Alice",
		Progress: room.Progress{Level: 1, MapID: "test"}, Fresh: true,
	})
	if !errors.Is(err, room.ErrRoomClosed) {
		t.Errorf("joining a room that is not hosted gave %v, want ErrRoomClosed", err)
	}
}

// A capture that could not be made is "not there", never an empty snapshot.
//
// The caller checkpoints what it gets back, so an invented snapshot writes
// zeroes over a character's position and health.
func TestRemoteRoomCaptureFailureIsNotAnEmptySnapshot(t *testing.T) {
	p := newRemotePair(t)
	joinRemote(t, p)

	// A node that is not there at all: the request goes nowhere and times out.
	nowhere, err := p.caller.resolve(testInstance, "no-such-node", "char-1")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}

	if _, ok := nowhere.Capture(context.Background(), 1); ok {
		t.Error("a capture that never reached a room reported success")
	}
}

// Everything the room sends back reaches the node holding the connection.
func TestRemoteRoomCallbacksReachTheCaller(t *testing.T) {
	p := newRemotePair(t)
	ctx := context.Background()

	sink := &recordingSink{}
	received := make(chan struct{}, 1)

	// The entity every callback claims to be about. Collected so the test can
	// assert the room learned it: a callback that says nothing about which
	// body it concerns cannot be told apart from one sent by a room the
	// character has already left.
	var mu sync.Mutex
	entities := map[uint32]int{}

	sub, err := p.caller.bus.Subscribe(ctx, p.handle.callback,
		func(_ context.Context, _ string, payload []byte) {
			var cb mmov1.RoomCallback
			if err := proto.Unmarshal(payload, &cb); err != nil {
				return
			}
			mu.Lock()
			entities[cb.GetEntityId()]++
			mu.Unlock()

			switch body := cb.GetBody().(type) {
			case *mmov1.RoomCallback_Send:
				var msg mmov1.ServerMessage
				if err := proto.Unmarshal(body.Send, &msg); err != nil {
					return
				}
				sink.Send(&msg)
				select {
				case received <- struct{}{}:
				default:
				}
			case *mmov1.RoomCallback_Close:
				sink.Close(body.Close.GetCode(), body.Close.GetReason())
			}
		})
	if err != nil {
		t.Fatalf("subscribing to callbacks: %v", err)
	}
	defer sub.Close()

	id := joinRemote(t, p)

	// Attaching is what gives the room somewhere to send: the join above
	// carried the subject, and a room that is ticking produces snapshots.
	if !p.handle.Attach(ctx, id, room.Attachment{}) {
		t.Fatal("attaching to a room on another node was refused")
	}

	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing the room sent reached the node holding the connection")
	}

	if sink.count() == 0 {
		t.Error("no server messages arrived")
	}

	// Once attached, callbacks say which body they are about. The first few
	// can legitimately say zero: the room is given its sink during the join and
	// is told the entity id only when the join returns.
	waitFor(t, "a callback tagged with the character's entity", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return entities[uint32(id)] > 0
	})
}

// A stat block that did not survive the wire must not be applied.
//
// The "more" layer is a product, so an empty block is not a neutral one: it
// multiplies every stat by zero. Applying it would leave the character with no
// life and no damage, which is worse than leaving them with the block they
// already had.
func TestRemoteRoomRefusesABadStatBlock(t *testing.T) {
	p := newRemotePair(t)
	id := joinRemote(t, p)
	ctx := context.Background()

	good := stats.NewBlock()
	good.SetBase(stats.MaxLife, stats.FromInt(4000))
	p.handle.SetStats(ctx, id, room.Derived{Block: good, MaxHP: 4000})

	waitFor(t, "the good stat block to arrive", func() bool {
		snap, ok := p.handle.Capture(ctx, id)
		return ok && snap.State.MaxHP == 4000
	})

	// A block built against a different stat list: too short to rebuild.
	p.host.applyRoomCommand(ctx, &mmov1.RoomCommand{
		InstanceId: uint64(testInstance),
		Body: &mmov1.RoomCommand_SetStats{SetStats: &mmov1.SetStatsCommand{
			EntityId: uint32(id),
			Derived: &mmov1.WireDerived{
				MaxHp: 1,
				Block: &mmov1.WireStats{Base: []int64{1}, Flat: []int64{1}},
			},
		}},
	})

	// Nothing to wait for -- a refusal is the absence of a change -- so this
	// gives the room a few ticks to have applied it if it were going to.
	time.Sleep(200 * time.Millisecond)
	snap, ok := p.handle.Capture(ctx, id)
	if !ok {
		t.Fatal("the character vanished")
	}
	if snap.State.MaxHP != 4000 {
		t.Errorf("maximum health is %d, want the 4000 it had before the bad block",
			snap.State.MaxHP)
	}
}

// A command for a room this node does not host is answered, not ignored.
func TestRemoteRoomServerAnswersForARoomItDoesNotHost(t *testing.T) {
	p := newRemotePair(t)

	reply := p.host.applyRoomCommand(context.Background(), &mmov1.RoomCommand{
		InstanceId: 999999,
		Body:       &mmov1.RoomCommand_Leave{Leave: &mmov1.LeaveCommand{EntityId: 1}},
	})
	if !reply.GetClosed() {
		t.Error("a command for an unknown room was not reported as closed")
	}
}

// Absent and empty are different instructions, and only the sender knows which
// it meant.
func TestAttachmentKeepsAbsentDistinctFromEmpty(t *testing.T) {
	absent := attachmentFrom(&mmov1.AttachCommand{})
	if absent.KnownWaypoints != nil {
		t.Errorf("an attachment that said nothing about waypoints got %v; "+
			"the room would forget the ones it knows",
			absent.KnownWaypoints)
	}
	if absent.Secondary != nil {
		t.Errorf("an attachment that said nothing about secondary progress got %v",
			absent.Secondary)
	}

	empty := attachmentFrom(&mmov1.AttachCommand{
		HasWaypoints: true, HasSecondary: true,
	})
	if empty.KnownWaypoints == nil {
		t.Error("an attachment that said 'no waypoints' was read as 'leave them alone'")
	}
	if empty.Secondary == nil {
		t.Error("an attachment that said 'no secondary progress' was read as 'leave it alone'")
	}
}

// The portal index is what decides where a player is sent.
//
// Every map in the test content has one portal, so a version that ignored the
// index would pass every other test in this package.
func TestResolvePortalEventUsesTheIndex(t *testing.T) {
	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load content: %v", err)
	}

	// A map with two portals, so picking the wrong one is visible.
	game.Maps["twoway"] = &content.Map{
		ID: "twoway",
		Portals: []content.Portal{
			{Name: "north", TargetMap: "annex", TargetSpawn: "from_test"},
			{Name: "south", TargetMap: "test", TargetSpawn: "start"},
		},
	}

	req, ok := resolvePortalEvent(game, &mmov1.PortalEvent{
		Player: 7, CharacterId: "char-1", MapId: "twoway", PortalIndex: 1,
	})
	if !ok {
		t.Fatal("a portal that exists did not resolve")
	}
	if req.Portal.Name != "south" {
		t.Errorf("resolved to portal %q, want south -- the one at index 1", req.Portal.Name)
	}
	if req.Player != 7 || req.CharacterID != "char-1" {
		t.Errorf("resolved to %+v", req)
	}

	if _, ok := resolvePortalEvent(game, &mmov1.PortalEvent{
		MapId: "twoway", PortalIndex: 9,
	}); ok {
		t.Error("an index past the end of the map resolved")
	}
	if _, ok := resolvePortalEvent(game, &mmov1.PortalEvent{MapId: "nowhere"}); ok {
		t.Error("a map this node does not have resolved")
	}
}

// waitFor polls until a condition holds, or fails.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A message about a body the character no longer has is dropped.
//
// The callback subject is per character, and a character outlives the rooms it
// passes through: a snapshot published by the room they just left can arrive
// after they have arrived somewhere else, and delivering it renders the player
// briefly back where they came from.
//
// Driven through applyRoomCallback rather than a real transfer because the
// race is a matter of milliseconds and cannot be arranged on purpose -- which
// is exactly why the guard is worth having and worth pinning down.
func TestStaleRoomCallbacksAreDropped(t *testing.T) {
	sink := &recordingSink{}
	s := &Session{entityID: 5, sink: sink}

	send := func(entity uint32) *mmov1.RoomCallback {
		return &mmov1.RoomCallback{
			EntityId: entity,
			Body:     &mmov1.RoomCallback_Send{Send: mustMarshal(t, &mmov1.ServerMessage{})},
		}
	}

	// The room the character has left still had something to say.
	s.applyRoomCallback(send(9))
	if got := sink.count(); got != 0 {
		t.Errorf("%d messages about an old body reached the client", got)
	}

	// The room they are in now.
	s.applyRoomCallback(send(5))
	if got := sink.count(); got != 1 {
		t.Errorf("the current room's message did not arrive: %d delivered", got)
	}

	// Zero is the window between a remote join being applied and its reply
	// coming back, when the room has not been told the entity id yet. There is
	// nothing else the character could be at that point, so it is delivered.
	s.applyRoomCallback(send(0))
	if got := sink.count(); got != 2 {
		t.Errorf("a message sent before the entity id was known was dropped: %d delivered", got)
	}
}

func mustMarshal(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// A character who joins a remote room without ever attaching still has its
// callbacks tagged.
//
// That is the login path when the directory places the first room on another
// node: the sink is built from the callback subject during the join, before
// there is an entity id to put on it, and is told afterwards. Without that the
// character's callbacks say "entity nobody" for as long as they play, and the
// guard against messages from a room they have left never engages for them.
func TestRemoteRoomJoinTagsItsCallbacks(t *testing.T) {
	p := newRemotePair(t)
	ctx := context.Background()

	var mu sync.Mutex
	entities := map[uint32]int{}

	sub, err := p.caller.bus.Subscribe(ctx, p.handle.callback,
		func(_ context.Context, _ string, payload []byte) {
			var cb mmov1.RoomCallback
			if err := proto.Unmarshal(payload, &cb); err != nil {
				return
			}
			mu.Lock()
			entities[cb.GetEntityId()]++
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("subscribing to callbacks: %v", err)
	}
	defer sub.Close()

	// Joined and never attached, unlike the test above.
	id := joinRemote(t, p)

	waitFor(t, "a callback tagged with the joined entity", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return entities[uint32(id)] > 0
	})
}
