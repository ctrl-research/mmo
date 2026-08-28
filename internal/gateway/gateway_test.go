package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ctrl-research/mmo/internal/content/contenttest"
	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/metrics"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
)

// testServer wires the full stack -- node, directory, gateway -- behind a real
// HTTP server, so these tests exercise the actual protocol over a real socket
// rather than a mock of it.
type testServer struct {
	url  string
	gw   *Gateway
	node *world.Node
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load content: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mx := metrics.New(prometheus.NewRegistry())
	dir := directory.NewMemory("test-node")

	node, err := world.NewNode(world.Config{
		Directory:  dir,
		Content:    game,
		DefaultMap: "test",
		Logger:     log,
		Observer:   mx,
		// A fixed seed so these tests never depend on the roll of the day.
		Seed: 0xC0FFEE,
	})
	if err != nil {
		t.Fatalf("new node: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	node.Start(ctx)

	gw, err := New(Config{
		Rooms:   node,
		Maps:    game.Maps,
		Tickets: NewTicketStore(),
		Metrics: mx,
		Logger:  log,
		DevAuth: true,
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	srv := httptest.NewServer(gw.Routes())
	t.Cleanup(func() {
		srv.Close()
		cancel()
		node.Stop()
	})

	return &testServer{url: srv.URL, gw: gw, node: node}
}

func (ts *testServer) ticket(t *testing.T, name string) string {
	t.Helper()
	resp, err := http.Post(ts.url+"/api/dev/ticket", "application/json",
		strings.NewReader(`{"name":"`+name+`"}`))
	if err != nil {
		t.Fatalf("ticket request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ticket status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode ticket: %v", err)
	}
	return body.Ticket
}

// client is a minimal protocol client for tests.
type client struct {
	conn *websocket.Conn
	t    *testing.T
}

func (ts *testServer) dial(t *testing.T) *client {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(ts.url, "http") + "/ws"
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return &client{conn: conn, t: t}
}

func (c *client) send(msgs ...*mmov1.ClientMessage) {
	c.t.Helper()
	payload, err := proto.Marshal(&mmov1.Envelope{Client: msgs})
	if err != nil {
		c.t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageBinary, payload); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *client) hello(ticket string, version uint32) {
	c.send(&mmov1.ClientMessage{Body: &mmov1.ClientMessage_Hello{
		Hello: &mmov1.Hello{Ticket: ticket, ProtocolVersion: version},
	}})
}

func (c *client) intent(seq uint32, moveX int32, jump bool) {
	c.send(&mmov1.ClientMessage{Body: &mmov1.ClientMessage_Intent{
		Intent: &mmov1.Intent{Seq: seq, MoveX: moveX, Jump: jump},
	}})
}

// recv reads one envelope and returns the server messages it batched.
func (c *client) recv(timeout time.Duration) ([]*mmov1.ServerMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, data, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var env mmov1.Envelope
	if err := proto.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	return env.GetServer(), nil
}

// awaitWelcome reads until the Welcome arrives.
func (c *client) awaitWelcome() *mmov1.Welcome {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := c.recv(2 * time.Second)
		if err != nil {
			c.t.Fatalf("awaiting welcome: %v", err)
		}
		for _, m := range msgs {
			if w := m.GetWelcome(); w != nil {
				return w
			}
		}
	}
	c.t.Fatal("no Welcome received")
	return nil
}

// awaitSnapshots collects at least n snapshots.
func (c *client) awaitSnapshots(n int) []*mmov1.Snapshot {
	c.t.Helper()
	var out []*mmov1.Snapshot
	deadline := time.Now().Add(10 * time.Second)
	for len(out) < n && time.Now().Before(deadline) {
		msgs, err := c.recv(3 * time.Second)
		if err != nil {
			c.t.Fatalf("awaiting snapshots: %v", err)
		}
		for _, m := range msgs {
			if s := m.GetSnapshot(); s != nil {
				out = append(out, s)
			}
		}
	}
	if len(out) < n {
		c.t.Fatalf("got %d snapshots, want %d", len(out), n)
	}
	return out
}

func TestFullHandshakeAndSnapshots(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)

	c.hello(ts.ticket(t, "alice"), ProtocolVersion)

	w := c.awaitWelcome()
	if w.GetEntityId() == 0 {
		t.Error("welcome carried no entity id")
	}
	if w.GetMapId() != "test" {
		t.Errorf("map = %q, want test", w.GetMapId())
	}
	if w.GetTickMs() != uint32(room.TickPeriod.Milliseconds()) {
		t.Errorf("tick_ms = %d, want %d", w.GetTickMs(), room.TickPeriod.Milliseconds())
	}

	snaps := c.awaitSnapshots(5)
	for i, s := range snaps {
		if s.GetSelf() == nil {
			t.Fatalf("snapshot %d carried no self state", i)
		}
		if i > 0 && s.GetTick() <= snaps[i-1].GetTick() {
			t.Errorf("snapshot ticks did not advance: %d then %d",
				snaps[i-1].GetTick(), s.GetTick())
		}
	}
}

// The whole point of the input path: buttons in, authoritative movement out.
func TestIntentMovesPlayerAndIsAcknowledged(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)
	c.hello(ts.ticket(t, "alice"), ProtocolVersion)
	c.awaitWelcome()

	first := c.awaitSnapshots(2)
	startX := first[len(first)-1].GetSelf().GetX()

	for seq := uint32(1); seq <= 25; seq++ {
		c.intent(seq, 1000, false)
		time.Sleep(room.TickPeriod / 2)
	}

	snaps := c.awaitSnapshots(10)
	last := snaps[len(snaps)-1]

	if last.GetSelf().GetX() <= startX {
		t.Errorf("player did not move right: x %d then %d", startX, last.GetSelf().GetX())
	}
	if last.GetAckSeq() == 0 {
		t.Error("server never acknowledged an input sequence")
	}
}

func TestTwoClientsSeeEachOther(t *testing.T) {
	ts := newTestServer(t)

	a := ts.dial(t)
	a.hello(ts.ticket(t, "alice"), ProtocolVersion)
	wa := a.awaitWelcome()

	b := ts.dial(t)
	b.hello(ts.ticket(t, "bob"), ProtocolVersion)
	wb := b.awaitWelcome()

	if wa.GetEntityId() == wb.GetEntityId() {
		t.Fatal("two players were given the same entity id")
	}

	// Alice must learn about Bob in full, since she has no baseline for him.
	found := false
	for _, s := range a.awaitSnapshots(15) {
		for _, e := range s.GetEntered() {
			if e.GetId() == wb.GetEntityId() && e.GetName() == "bob" {
				found = true
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Error("alice was never told about bob")
	}
}

func TestTicketIsSingleUse(t *testing.T) {
	ts := newTestServer(t)
	ticket := ts.ticket(t, "alice")

	a := ts.dial(t)
	a.hello(ticket, ProtocolVersion)
	a.awaitWelcome()

	// Replaying a redeemed ticket must fail.
	b := ts.dial(t)
	b.hello(ticket, ProtocolVersion)

	if _, err := b.recv(3 * time.Second); err == nil {
		t.Error("a replayed ticket was accepted")
	} else if code := websocket.CloseStatus(err); code != room.CloseTicketInvalid {
		t.Errorf("close code = %d, want %d", code, room.CloseTicketInvalid)
	}
}

func TestUnknownTicketRejected(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)
	c.hello("not-a-real-ticket", ProtocolVersion)

	_, err := c.recv(3 * time.Second)
	if err == nil {
		t.Fatal("an invalid ticket was accepted")
	}
	if code := websocket.CloseStatus(err); code != room.CloseTicketInvalid {
		t.Errorf("close code = %d, want %d", code, room.CloseTicketInvalid)
	}
}

// A version mismatch must be refused at the handshake. Letting it through
// produces bugs that read as physics problems.
func TestProtocolVersionMismatchRejected(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)
	c.hello(ts.ticket(t, "alice"), ProtocolVersion+99)

	_, err := c.recv(3 * time.Second)
	if err == nil {
		t.Fatal("a mismatched protocol version was accepted")
	}
	if code := websocket.CloseStatus(err); code != room.CloseProtocolVersion {
		t.Errorf("close code = %d, want %d", code, room.CloseProtocolVersion)
	}
}

func TestFirstMessageMustBeHello(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)

	c.intent(1, 1000, false) // skipping the handshake entirely

	if _, err := c.recv(3 * time.Second); err == nil {
		t.Error("input before the handshake was accepted")
	}
}

// A full shared room opens another channel rather than turning players away.
// That is the MapleStory model and the reason shared rooms have a placement
// policy at all: capacity limits tick cost, it does not limit who may play.
func TestFullSharedRoomOpensANewChannel(t *testing.T) {
	ts := newTestServer(t)

	// Read the capacity from content rather than hardcoding it, so tuning the
	// test map cannot silently turn this into a test of nothing.
	capacity := ts.node.Content().Maps["test"].Capacity

	var firstInstance uint64
	for i := 0; i < capacity; i++ {
		c := ts.dial(t)
		c.hello(ts.ticket(t, "player"), ProtocolVersion)
		w := c.awaitWelcome()

		if i == 0 {
			firstInstance = w.GetInstanceId()
		} else if w.GetInstanceId() != firstInstance {
			t.Fatalf("player %d landed in instance %d, want %d while capacity remained",
				i, w.GetInstanceId(), firstInstance)
		}
	}

	overflow := ts.dial(t)
	overflow.hello(ts.ticket(t, "overflow"), ProtocolVersion)
	w := overflow.awaitWelcome()

	if w.GetInstanceId() == firstInstance {
		t.Errorf("the overflow player joined instance %d, which was already at capacity", firstInstance)
	}
	if ts.node.Rooms() != 2 {
		t.Errorf("node hosts %d rooms, want 2", ts.node.Rooms())
	}

	// Being in another channel means not sharing a world with the others. Mobs
	// are filtered out because the fresh channel spawns its own.
	for _, snap := range overflow.awaitSnapshots(5) {
		for _, e := range snap.GetEntered() {
			if e.GetKind() == mmov1.EntityKind_ENTITY_KIND_PLAYER {
				t.Errorf("a player in a fresh channel saw player %d from the full one", e.GetId())
			}
		}
	}
}

func TestPingIsAnswered(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)
	c.hello(ts.ticket(t, "alice"), ProtocolVersion)
	c.awaitWelcome()

	c.send(&mmov1.ClientMessage{Body: &mmov1.ClientMessage_Ping{
		Ping: &mmov1.Ping{ClientTimeMs: 123456},
	}})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := c.recv(2 * time.Second)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		for _, m := range msgs {
			if p := m.GetPong(); p != nil {
				if p.GetClientTimeMs() != 123456 {
					t.Errorf("pong echoed %d, want 123456", p.GetClientTimeMs())
				}
				return
			}
		}
	}
	t.Fatal("no Pong received")
}

func TestDisconnectRemovesPlayerFromRoom(t *testing.T) {
	ts := newTestServer(t)

	a := ts.dial(t)
	a.hello(ts.ticket(t, "alice"), ProtocolVersion)
	a.awaitWelcome()

	b := ts.dial(t)
	b.hello(ts.ticket(t, "bob"), ProtocolVersion)
	wb := b.awaitWelcome()
	a.awaitSnapshots(5)

	b.conn.Close(websocket.StatusNormalClosure, "leaving")

	// Alice must eventually be told Bob's entity is gone.
	removed := false
	deadline := time.Now().Add(5 * time.Second)
	for !removed && time.Now().Before(deadline) {
		msgs, err := a.recv(2 * time.Second)
		if err != nil {
			break
		}
		for _, m := range msgs {
			if s := m.GetSnapshot(); s != nil {
				for _, id := range s.GetRemoved() {
					if id == wb.GetEntityId() {
						removed = true
					}
				}
			}
		}
	}
	if !removed {
		t.Error("bob's entity was never removed after he disconnected")
	}
}

func TestMalformedFrameClosesConnection(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageBinary, []byte{0xff, 0xff, 0xff, 0xff}); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := c.recv(3 * time.Second); err == nil {
		t.Error("a malformed frame did not close the connection")
	}
}

// The protocol is binary; a text frame is either a confused client or a probe.
func TestTextFrameRejected(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, []byte("hello?")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := c.recv(3 * time.Second); err == nil {
		t.Error("a text frame was accepted")
	}
}

func TestHealthEndpoint(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.url + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want ok", body["status"])
	}
}

func TestDevTicketRejectsEmptyName(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Post(ts.url+"/api/dev/ticket", "application/json",
		strings.NewReader(`{"name":"   "}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// The dev ticket endpoint must not exist unless it was explicitly enabled.
func TestDevAuthDisabledHidesTicketEndpoint(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := New(Config{
		Rooms:   stubProvider{},
		Metrics: metrics.New(prometheus.NewRegistry()),
		Logger:  log,
		DevAuth: false,
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	srv := httptest.NewServer(gw.Routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/dev/ticket", "application/json",
		strings.NewReader(`{"name":"mallory"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Error("the dev ticket endpoint served a request with dev auth disabled")
	}
}

type stubProvider struct{}

func (stubProvider) Handle(context.Context) (room.Handle, error) { return nil, nil }
