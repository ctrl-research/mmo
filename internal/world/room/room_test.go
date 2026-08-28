package room

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ctrl-research/mmo/internal/content/contenttest"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// recordSink captures everything the room sends to one player.
type recordSink struct {
	mu     sync.Mutex
	msgs   []*mmov1.ServerMessage
	closed bool
	code   uint32
}

func newSink() *recordSink { return &recordSink{} }

func (s *recordSink) Send(m *mmov1.ServerMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
}

func (s *recordSink) Close(code uint32, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed, s.code = true, code
}

func (s *recordSink) snapshots() []*mmov1.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*mmov1.Snapshot
	for _, m := range s.msgs {
		if snap := m.GetSnapshot(); snap != nil {
			out = append(out, snap)
		}
	}
	return out
}

func (s *recordSink) welcome() *mmov1.Welcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		if w := m.GetWelcome(); w != nil {
			return w
		}
	}
	return nil
}

func (s *recordSink) events() []*mmov1.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*mmov1.Event
	for _, m := range s.msgs {
		if e := m.GetEvent(); e != nil {
			out = append(out, e)
		}
	}
	return out
}

func (s *recordSink) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func testRoom(t *testing.T, capacity int) (*Room, Handle, context.CancelFunc) {
	t.Helper()

	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load test content: %v", err)
	}
	m := game.Maps["test"]

	r := New(Config{
		InstanceID: 1,
		MapID:      m.ID,
		Capacity:   capacity,
		World:      m.World,
		Tuning:     sim.DefaultTuning(),
		Spawn:      m.DefaultSpawn().At,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Content:    game,
		Map:        m,
		// Fixed, so a failure is reproducible rather than a roll of the day.
		Seed: 0xA11CE,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	t.Cleanup(cancel)

	return r, NewHandle(r), cancel
}

func TestJoinSendsWelcome(t *testing.T) {
	_, h, _ := testRoom(t, 10)
	ctx := context.Background()

	sink := newSink()
	id, err := h.Join(ctx, "alice", sink)
	if err != nil {
		t.Fatalf("join: %v", err)
	}

	w := sink.welcome()
	if w == nil {
		t.Fatal("no Welcome sent on join")
	}
	if w.GetEntityId() != uint32(id) {
		t.Errorf("welcome entity id = %d, want %d", w.GetEntityId(), id)
	}
	if w.GetMapId() != "test" {
		t.Errorf("welcome map = %q, want test", w.GetMapId())
	}
	if w.GetTickMs() != uint32(TickPeriod.Milliseconds()) {
		t.Errorf("welcome tick_ms = %d, want %d", w.GetTickMs(), TickPeriod.Milliseconds())
	}
	// Self must carry the prediction-only fields, or the client's replay
	// diverges from the server on its very first reconciliation.
	if w.GetSelf() == nil {
		t.Fatal("welcome carried no self state")
	}
}

func TestJoinRejectedWhenFull(t *testing.T) {
	_, h, _ := testRoom(t, 2)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := h.Join(ctx, "player", newSink()); err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
	}
	if _, err := h.Join(ctx, "overflow", newSink()); err != ErrRoomFull {
		t.Errorf("join beyond capacity = %v, want ErrRoomFull", err)
	}
}

func TestPlayersSeeEachOther(t *testing.T) {
	_, h, _ := testRoom(t, 10)
	ctx := context.Background()

	a, b := newSink(), newSink()
	idA, _ := h.Join(ctx, "alice", a)
	idB, _ := h.Join(ctx, "bob", b)

	waitForSnapshots(t, a, 3)

	// Alice must be told Bob exists, as a full EntityState rather than a delta
	// against a baseline she does not have.
	found := false
	for _, snap := range a.snapshots() {
		for _, e := range snap.GetEntered() {
			if e.GetId() == uint32(idB) && e.GetName() == "bob" {
				found = true
			}
		}
	}
	if !found {
		t.Error("alice was never told about bob")
	}

	// And Alice must receive a join event for Bob.
	joined := false
	for _, ev := range a.events() {
		if pj := ev.GetPlayerJoined(); pj != nil && pj.GetEntityId() == uint32(idB) {
			joined = true
		}
	}
	if !joined {
		t.Error("alice received no PlayerJoined event for bob")
	}

	// Bob joined second, so he learns about Alice the same way.
	foundA := false
	for _, snap := range b.snapshots() {
		for _, e := range snap.GetEntered() {
			if e.GetId() == uint32(idA) {
				foundA = true
			}
		}
	}
	if !foundA {
		t.Error("bob was never told about alice")
	}
}

func TestLeaveNotifiesOthersAndRemovesEntity(t *testing.T) {
	_, h, _ := testRoom(t, 10)
	ctx := context.Background()

	a, b := newSink(), newSink()
	h.Join(ctx, "alice", a)
	idB, _ := h.Join(ctx, "bob", b)

	waitForSnapshots(t, a, 3)
	h.Leave(ctx, idB)
	waitForSnapshots(t, a, 6)

	left := false
	for _, ev := range a.events() {
		if pl := ev.GetPlayerLeft(); pl != nil && pl.GetEntityId() == uint32(idB) {
			left = true
		}
	}
	if !left {
		t.Error("alice received no PlayerLeft event")
	}

	removed := false
	for _, snap := range a.snapshots() {
		for _, id := range snap.GetRemoved() {
			if id == uint32(idB) {
				removed = true
			}
		}
	}
	if !removed {
		t.Error("bob's entity was never marked removed in a snapshot")
	}
}

func TestLeaveOfUnknownPlayerIsSafe(t *testing.T) {
	_, h, _ := testRoom(t, 10)
	// A disconnect racing a kick makes this happen for real; it must not panic
	// or wedge the room.
	h.Leave(context.Background(), EntityID(9999))

	sink := newSink()
	if _, err := h.Join(context.Background(), "alice", sink); err != nil {
		t.Fatalf("room broken after unknown leave: %v", err)
	}
}

func TestInputMovesThePlayer(t *testing.T) {
	_, h, _ := testRoom(t, 10)
	ctx := context.Background()

	sink := newSink()
	id, _ := h.Join(ctx, "alice", sink)

	startX := selfX(t, sink)

	for seq := uint32(1); seq <= 20; seq++ {
		h.Input(ctx, id, seq, sim.Input{MoveX: 1000})
		time.Sleep(TickPeriod / 2)
	}
	waitForSnapshots(t, sink, 20)

	if endX := selfX(t, sink); endX <= startX {
		t.Errorf("player did not move right: x went from %d to %d", startX, endX)
	}
}

// The client sends intent, never position. There is no message by which it can
// claim to be somewhere, and this test exists to make that regression loud if
// someone ever adds one.
func TestSnapshotSelfIsServerAuthoritative(t *testing.T) {
	_, h, _ := testRoom(t, 10)
	ctx := context.Background()

	sink := newSink()
	id, _ := h.Join(ctx, "alice", sink)

	// Nothing but held-button intent is accepted.
	h.Input(ctx, id, 1, sim.Input{MoveX: 999999}) // out of range: clamped
	waitForSnapshots(t, sink, 5)

	snaps := sink.snapshots()
	last := snaps[len(snaps)-1]
	// Clamped to run speed, so one tick of movement cannot exceed it.
	if last.GetSelf().GetVx() > int32(sim.DefaultTuning().RunSpeed) {
		t.Errorf("velocity %d exceeds run speed: input was not clamped", last.GetSelf().GetVx())
	}
}

func TestDuplicateAndStaleInputIgnored(t *testing.T) {
	r, h, _ := testRoom(t, 10)
	ctx := context.Background()

	sink := newSink()
	id, _ := h.Join(ctx, "alice", sink)

	h.Input(ctx, id, 5, sim.Input{MoveX: 1000})
	h.Input(ctx, id, 5, sim.Input{MoveX: 1000}) // duplicate
	h.Input(ctx, id, 3, sim.Input{MoveX: 1000}) // reordered, stale

	time.Sleep(5 * TickPeriod)

	done := make(chan int, 1)
	r.cmds <- inspectCmd{fn: func(rr *Room) {
		done <- len(rr.players[id].queue)
	}}
	select {
	case n := <-done:
		if n > 1 {
			t.Errorf("queue holds %d inputs; duplicates and stale sequences should be dropped", n)
		}
	case <-time.After(time.Second):
		t.Fatal("inspect timed out")
	}
}

func TestInputQueueIsBounded(t *testing.T) {
	r, h, _ := testRoom(t, 10)
	ctx := context.Background()

	sink := newSink()
	id, _ := h.Join(ctx, "alice", sink)

	// A client running far ahead must not be able to grow the queue without
	// limit; stale intent is worth less than fresh.
	for seq := uint32(1); seq <= 200; seq++ {
		h.Input(ctx, id, seq, sim.Input{MoveX: 1000})
	}

	done := make(chan int, 1)
	r.cmds <- inspectCmd{fn: func(rr *Room) { done <- len(rr.players[id].queue) }}
	select {
	case n := <-done:
		if n > maxInputQueue {
			t.Errorf("queue depth %d exceeds cap %d", n, maxInputQueue)
		}
	case <-time.After(time.Second):
		t.Fatal("inspect timed out")
	}
}

func TestSnapshotOmitsUnchangedEntities(t *testing.T) {
	_, h, _ := testRoom(t, 10)
	ctx := context.Background()

	a, b := newSink(), newSink()
	h.Join(ctx, "alice", a)
	h.Join(ctx, "bob", b)

	// Let both settle on the floor with no input at all.
	waitForSnapshots(t, a, 25)

	// Once everything is at rest, later snapshots should carry no entity
	// deltas: delta compression exists so an idle room costs almost nothing.
	snaps := a.snapshots()
	tail := snaps[len(snaps)-5:]
	for i, snap := range tail {
		if len(snap.GetEntities()) != 0 {
			t.Errorf("idle snapshot %d still carried %d entity deltas",
				i, len(snap.GetEntities()))
		}
	}
}

func TestSnapshotSendsDeltaAfterFirstFullState(t *testing.T) {
	_, h, _ := testRoom(t, 10)
	ctx := context.Background()

	a, b := newSink(), newSink()
	h.Join(ctx, "alice", a)
	idB, _ := h.Join(ctx, "bob", b)

	waitForSnapshots(t, a, 5)
	for seq := uint32(1); seq <= 15; seq++ {
		h.Input(ctx, idB, seq, sim.Input{MoveX: 1000})
		time.Sleep(TickPeriod / 2)
	}
	waitForSnapshots(t, a, 25)

	// Bob is announced once in full, then only as deltas.
	fullCount := 0
	deltaCount := 0
	for _, snap := range a.snapshots() {
		for _, e := range snap.GetEntered() {
			if e.GetId() == uint32(idB) {
				fullCount++
			}
		}
		for _, d := range snap.GetEntities() {
			if d.GetId() == uint32(idB) {
				deltaCount++
			}
		}
	}
	if fullCount != 1 {
		t.Errorf("bob sent in full %d times, want exactly 1", fullCount)
	}
	if deltaCount == 0 {
		t.Error("bob moved but no deltas were sent for him")
	}
}

// Only the entity's owner receives the prediction-only fields; nobody else
// replays that body's inputs, so for them they would be dead weight every tick.
func TestPredictionFieldsSentOnlyToOwner(t *testing.T) {
	_, h, _ := testRoom(t, 10)
	ctx := context.Background()

	a, b := newSink(), newSink()
	h.Join(ctx, "alice", a)
	idB, _ := h.Join(ctx, "bob", b)

	waitForSnapshots(t, a, 5)

	for _, snap := range a.snapshots() {
		for _, e := range snap.GetEntered() {
			if e.GetId() != uint32(idB) {
				continue
			}
			if e.GetCoyote() != 0 || e.GetJumpBuffer() != 0 || e.GetDropThrough() != 0 {
				t.Error("another player's entity carried prediction-only fields")
			}
		}
	}
}

func TestShutdownClosesConnections(t *testing.T) {
	_, h, cancel := testRoom(t, 10)
	ctx := context.Background()

	sink := newSink()
	h.Join(ctx, "alice", sink)
	waitForSnapshots(t, sink, 2)

	cancel()

	deadline := time.After(2 * time.Second)
	for !sink.isClosed() {
		select {
		case <-deadline:
			t.Fatal("connection was not closed on shutdown")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if sink.code != CloseServerShutdown {
		t.Errorf("close code = %d, want %d", sink.code, CloseServerShutdown)
	}
}

func TestConcurrentJoinsAndLeaves(t *testing.T) {
	_, h, _ := testRoom(t, 50)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				id, err := h.Join(ctx, "churn", newSink())
				if err == nil {
					h.Input(ctx, id, uint32(j+1), sim.Input{MoveX: 500})
					h.Leave(ctx, id)
				}
			}
		}()
	}
	wg.Wait()

	// The room must still be alive and accepting players.
	if _, err := h.Join(ctx, "final", newSink()); err != nil {
		t.Fatalf("room unhealthy after churn: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

// inspectCmd runs a function on the room goroutine, so tests can read room
// state without racing the tick loop.
type inspectCmd struct{ fn func(*Room) }

func (inspectCmd) isCommand() {}

func waitForSnapshots(t *testing.T, s *recordSink, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if len(s.snapshots()) >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d snapshots, have %d", n, len(s.snapshots()))
		case <-time.After(TickPeriod / 2):
		}
	}
}

func selfX(t *testing.T, s *recordSink) int32 {
	t.Helper()
	snaps := s.snapshots()
	if len(snaps) == 0 {
		w := s.welcome()
		if w == nil {
			t.Fatal("no state received yet")
		}
		return w.GetSelf().GetX()
	}
	return snaps[len(snaps)-1].GetSelf().GetX()
}

func (c inspectCmd) run(r *Room) { c.fn(r) }
