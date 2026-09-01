package world

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ctrl-research/mmo/internal/bus"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/content/contenttest"
	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/store"
	"github.com/ctrl-research/mmo/internal/store/storetest"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Two world nodes in one process, forced to talk over the bus.
//
// This is the test the whole scaling design rests on. Everything a room handoff
// touches -- the directory choosing a node, the fencing token, the checkpoint,
// the request and reply, the source being torn down only after the destination
// accepts -- runs here with a real database and two genuinely separate nodes
// that share nothing but the bus, the directory, and the in-process room
// registry that stands in for M9's remote handles.
//
// If this passes, M9 is configuration. If it were passing because of a local
// shortcut, M9 would be a rewrite (AGENTS.md invariant 2).

// cluster is two world nodes over one bus and one directory.
type cluster struct {
	t *testing.T

	a, b     *Node
	presence directory.Presence
	parties  directory.Parties
	dir      directory.Directory
	bus      bus.Bus
	store    *store.Store
	game     *content.Content
	cancel   context.CancelFunc
}

func newCluster(t *testing.T) *cluster {
	t.Helper()

	st := storetest.New(t)

	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load content: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())

	// Two registries would be more realistic still, but a room on another
	// *process* needs a bus-backed handle; sharing the registry is what stands
	// in for that, and it is the only thing these two nodes share beyond what a
	// real cluster would.
	leases := directory.NewMemoryLeases()
	registry := NewRegistry()

	// A directory per node, the same shape as the bus below. With
	// MMO_TEST_REDIS_ADDR set that is two Redis directories sharing a
	// namespace, each registering and heartbeating its own node -- so placement
	// crosses a network rather than a pointer. Without it they share one
	// in-memory directory, which is correct for a single process.
	dirA, dirB := testDirectories(t)
	dir := dirA

	// A bus per node, so "two nodes" means two independent connections rather
	// than two names for the same object. With MMO_TEST_NATS_URL set that is
	// two connections to a real server, and every cross-node test below --
	// portal transfer, global chat, a party invite -- runs over the transport
	// the cluster will actually use. Without it they share one in-process bus,
	// which is correct for a single process and is what CI runs when no server
	// is available.
	busA, busB := testBuses(t)
	presence := testPresence(t)
	parties := testParties(t, game.Balance.Party.MaxSize)

	node := func(id string, nodeBus bus.Bus, nodeDir directory.Directory) *Node {
		n, err := NewNode(Config{
			Directory:  nodeDir,
			Leases:     leases,
			Store:      st,
			Bus:        nodeBus,
			Rooms:      registry,
			Presence:   presence,
			Parties:    parties,
			NodeID:     id,
			Content:    game,
			DefaultMap: "test",
			Logger:     log.With("node", id),
			Seed:       0xC0FFEE,
			// Short, so the teardown path is reachable without waiting out the
			// content value. Still several ticks, so a room does not vanish
			// between a player leaving and another arriving.
			IdleTicks: 4,
		})
		if err != nil {
			cancel()
			t.Fatalf("new node %s: %v", id, err)
		}
		if err := n.Start(ctx); err != nil {
			cancel()
			t.Fatalf("start node %s: %v", id, err)
		}
		return n
	}

	c := &cluster{
		t: t, dir: dir, bus: busA, store: st, game: game,
		presence: presence, parties: parties, cancel: cancel,
	}
	c.a = node("node-a", busA, dirA)
	c.b = node("node-b", busB, dirB)

	t.Cleanup(func() {
		cancel()
		c.a.Stop()
		c.b.Stop()
	})
	return c
}

// testDirectories returns one directory per node.
//
// Over Redis when MMO_TEST_REDIS_ADDR is set, and one shared in-memory
// directory otherwise. Two directories rather than one is the point: two nodes
// agreeing about where a room lives is the property this milestone is about, and
// a shared object proves only that two names refer to the same map.
//
// The in-memory case returns the same directory twice, because Memory has no
// notion of a peer -- Node.Start registers each node on it through AddNode, so
// one object with two registered nodes is what "two nodes" means there.
func testDirectories(t *testing.T) (directory.Directory, directory.Directory) {
	t.Helper()

	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		shared := directory.NewMemory("node-a")
		t.Cleanup(func() { shared.Close() })
		return shared, shared
	}

	client := redis.NewClient(&redis.Options{Addr: addr})

	// A namespace per test: a directory is global by design, so two tests
	// sharing one would see each other's rooms.
	prefix := "mmoworld:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx := context.Background()

	open := func(node directory.NodeID) directory.Directory {
		d, err := directory.NewRedis(ctx, client, prefix, node)
		if err != nil {
			t.Fatalf("open redis directory for %s: %v", node, err)
		}
		t.Cleanup(func() { d.Close() })
		return d
	}

	a, b := open("node-a"), open("node-b")
	t.Cleanup(func() {
		if keys, err := client.Keys(ctx, prefix+"*").Result(); err == nil && len(keys) > 0 {
			client.Del(ctx, keys...)
		}
		client.Close()
	})
	return a, b
}

// testPresence returns the presence table both nodes share.
//
// One table rather than one per node, because that is what presence *is*: a
// single answer to "which node holds this character", which both nodes read and
// write. Over Redis when MMO_TEST_REDIS_ADDR is set, so a whisper crosses a
// network to find its recipient.
func testPresence(t *testing.T) directory.Presence {
	t.Helper()

	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		p := directory.NewMemoryPresence()
		t.Cleanup(func() { p.Close() })
		return p
	}

	client := redis.NewClient(&redis.Options{Addr: addr})
	prefix := "mmoworldpres:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx := context.Background()

	t.Cleanup(func() {
		if keys, err := client.Keys(ctx, prefix+"*").Result(); err == nil && len(keys) > 0 {
			client.Del(ctx, keys...)
		}
		client.Close()
	})
	return directory.NewRedisPresence(client, prefix)
}

// testParties returns one party table shared by every node in the harness.
//
// Redis when MMO_TEST_REDIS_ADDR is set, so the cross-node party tests run
// against the implementation that will actually be deployed rather than a map
// two nodes happen to share a pointer to.
func testParties(t *testing.T, maxSize int) directory.Parties {
	t.Helper()

	addr := os.Getenv("MMO_TEST_REDIS_ADDR")
	if addr == "" {
		p := directory.NewMemoryParties(maxSize)
		t.Cleanup(func() { p.Close() })
		return p
	}

	client := redis.NewClient(&redis.Options{Addr: addr})
	prefix := "mmoworldparty:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx := context.Background()

	t.Cleanup(func() {
		if keys, err := client.Keys(ctx, prefix+"*").Result(); err == nil && len(keys) > 0 {
			client.Del(ctx, keys...)
		}
		client.Close()
	})
	return directory.NewRedisParties(client, prefix, maxSize)
}

// testBuses returns one bus per node.
//
// Over NATS when MMO_TEST_NATS_URL is set, and one shared in-process bus
// otherwise. Two connections rather than one is the point: a message that
// crosses nodes has to cross a transport, and a shared object proves only that
// two names refer to the same map.
func testBuses(t *testing.T) (bus.Bus, bus.Bus) {
	t.Helper()

	url := os.Getenv("MMO_TEST_NATS_URL")
	if url == "" {
		shared := bus.NewInProc()
		t.Cleanup(func() { shared.Close() })
		return shared, shared
	}

	open := func(name string) bus.Bus {
		b, err := bus.Connect(url)
		if err != nil {
			t.Fatalf("connect %s to nats at %s: %v", name, url, err)
		}
		t.Cleanup(func() { b.Close() })
		return b
	}
	return open("node-a"), open("node-b")
}

// character creates an account and a character sitting in the test map.
func (c *cluster) character(name string) (uuid.UUID, uuid.UUID) {
	c.t.Helper()
	ctx := context.Background()

	account, _, err := c.store.UpsertIdentity(ctx, "test", name, name+"@example.test")
	if err != nil {
		c.t.Fatalf("create account: %v", err)
	}
	ch, err := c.store.CreateCharacter(ctx, account.ID, name, "warrior", "test")
	if err != nil {
		c.t.Fatalf("create character: %v", err)
	}
	return account.ID, ch.ID
}

// standingIn saves a character's position inside a rectangle, so entering the
// world puts them there.
//
// Walking them there would be a better test of movement and a worse test of
// transfers: at 20 Hz, crossing the map takes seconds of wall clock, and the
// walk could fail for reasons that have nothing to do with the handoff.
func (c *cluster) standingIn(mapID string, character uuid.UUID, at sim.Rect) {
	c.t.Helper()

	state, err := room.MarshalState(room.CharacterState{
		X:     at.X + at.W/2,
		Y:     at.Y + at.H - fixed.FromInt(1),
		HP:    100,
		MaxHP: 100,
		MP:    50,
		MaxMP: 50,
	})
	if err != nil {
		c.t.Fatalf("encode state: %v", err)
	}

	// Token zero is the lowest a lease can hand out, and the character has
	// never been leased, so this is the same predicate a real write passes.
	if err := c.store.Checkpoint(context.Background(), store.Character{
		ID:    character,
		Level: 1,
		MapID: mapID,
		State: state,
	}, 0); err != nil {
		c.t.Fatalf("place character: %v", err)
	}
}

// enter puts a character into the world through a node.
func (c *cluster) enter(n *Node, account, character uuid.UUID) (*Session, *captureSink) {
	c.t.Helper()

	sink := newCaptureSink()
	s, err := n.Enter(context.Background(), account, character, sink)
	if err != nil {
		c.t.Fatalf("enter: %v", err)
	}
	c.t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Close(closeCtx)
	})
	return s.(*Session), sink
}

// nodeHosting reports which node is running an instance.
func (c *cluster) nodeHosting(id directory.InstanceID) string {
	c.a.mu.Lock()
	_, onA := c.a.rooms[id]
	c.a.mu.Unlock()
	if onA {
		return "node-a"
	}

	c.b.mu.Lock()
	_, onB := c.b.rooms[id]
	c.b.mu.Unlock()
	if onB {
		return "node-b"
	}
	return ""
}

// captureSink records what a session sends its client.
type captureSink struct {
	mu       sync.Mutex
	messages []*mmov1.ServerMessage
	closed   bool
}

func newCaptureSink() *captureSink { return &captureSink{} }

func (s *captureSink) Send(msg *mmov1.ServerMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
}

func (s *captureSink) Close(uint32, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// welcomes returns every room the client has been told it is in.
//
// A second Welcome is how a client learns it has moved: same connection,
// different room, and everything it was tracking discarded.
func (s *captureSink) welcomes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string
	for _, m := range s.messages {
		if w := m.GetWelcome(); w != nil {
			out = append(out, w.GetMapId())
		}
	}
	return out
}

// chat returns every chat line the client was sent.
func (s *captureSink) chat() []*mmov1.ChatLine {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.ChatLine
	for _, m := range s.messages {
		if ev := m.GetEvent(); ev != nil && ev.GetChat() != nil {
			out = append(out, ev.GetChat())
		}
	}
	return out
}

// system returns every system notice the client was sent.
func (s *captureSink) system() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string
	for _, m := range s.messages {
		if ev := m.GetEvent(); ev != nil && ev.GetSystem() != nil {
			out = append(out, ev.GetSystem().GetBody())
		}
	}
	return out
}

// parties returns every party roster the client was sent, newest last.
func (s *captureSink) partyStates() []*mmov1.PartyState {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.PartyState
	for _, m := range s.messages {
		if ev := m.GetEvent(); ev != nil && ev.GetParty() != nil {
			out = append(out, ev.GetParty())
		}
	}
	return out
}

// guildStates returns every guild roster the client was sent, newest last.
func (s *captureSink) guildStates() []*mmov1.GuildState {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.GuildState
	for _, m := range s.messages {
		if ev := m.GetEvent(); ev != nil && ev.GetGuild() != nil {
			out = append(out, ev.GetGuild())
		}
	}
	return out
}

// guildInvites returns every guild invitation the client was shown.
func (s *captureSink) guildInvites() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string
	for _, m := range s.messages {
		if ev := m.GetEvent(); ev != nil && ev.GetGuildInvite() != nil {
			out = append(out, ev.GetGuildInvite().GetGuildName())
		}
	}
	return out
}

// friendLists returns every friends list the client was sent, newest last.
func (s *captureSink) friendLists() []*mmov1.FriendList {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.FriendList
	for _, m := range s.messages {
		if ev := m.GetEvent(); ev != nil && ev.GetFriends() != nil {
			out = append(out, ev.GetFriends())
		}
	}
	return out
}

// passiveStates returns every passive tree state the client was sent.
func (s *captureSink) passiveStates() []*mmov1.PassiveState {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.PassiveState
	for _, m := range s.messages {
		if ev := m.GetEvent(); ev != nil && ev.GetPassives() != nil {
			out = append(out, ev.GetPassives())
		}
	}
	return out
}

// invites returns every party invitation the client was shown.
func (s *captureSink) invites() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string
	for _, m := range s.messages {
		if ev := m.GetEvent(); ev != nil && ev.GetPartyInvite() != nil {
			out = append(out, ev.GetPartyInvite().GetFromName())
		}
	}
	return out
}

// heard reports whether a line with this body arrived on a channel.
func (s *captureSink) heard(channel mmov1.ChatChannel, body string) bool {
	for _, line := range s.chat() {
		if line.GetChannel() == channel && line.GetBody() == body {
			return true
		}
	}
	return false
}

// refusals returns every reason the player was told they did not travel.
func (s *captureSink) refusals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string
	for _, m := range s.messages {
		ev := m.GetEvent()
		if ev == nil {
			continue
		}
		if r := ev.GetPortalRefused(); r != nil {
			out = append(out, r.GetReason())
		}
	}
	return out
}

// eventually polls until cond holds, which beats a sleep long enough to be
// safe: a transfer is a database write and a bus round trip, and how long that
// takes is not something a test should assert.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- the milestone's exit criterion ------------------------------------------

// The map a character starts in and the map they walk into are hosted by
// different nodes, and getting between them uses nothing but the bus.
func TestPortalTransfersBetweenNodes(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Traveller")

	// Standing in the portal on the far side of the test map, so the first
	// tick after they enter takes them through it.
	c.standingIn("test", character, c.game.Maps["test"].Portals[0].Bounds)

	s, sink := c.enter(c.a, account, character)
	from := s.Instance()

	eventually(t, "the character to arrive in the annex", func() bool {
		return s.MapID() == "annex"
	})

	to := s.Instance()
	if to == from {
		t.Fatal("the character is in the same instance it started in")
	}

	// The source room outlives the transfer -- it stands empty for its idle
	// timeout before retiring -- so both are still resolvable here.
	hostFrom, hostTo := c.nodeHosting(from), c.nodeHosting(to)
	if hostFrom == "" || hostTo == "" {
		t.Fatalf("rooms are hosted by %q and %q; both should be running", hostFrom, hostTo)
	}
	if hostTo == hostFrom {
		t.Errorf("both rooms are on %s; the directory should have spread them "+
			"across the two nodes, which is what makes this test mean anything", hostTo)
	}

	// The client is told, or it is rendering the wrong map with a stale entity
	// id and every entity it was tracking gone.
	eventually(t, "the client to be welcomed into the annex", func() bool {
		got := sink.welcomes()
		return len(got) == 2 && got[0] == "test" && got[1] == "annex"
	})

	// The source slot is released, or capacity leaks on every portal taken.
	if inst, ok, _ := c.dir.Lookup(context.Background(), from); ok && inst.Players != 0 {
		t.Errorf("the source instance still holds %d players", inst.Players)
	}
}

// A character that has walked through a portal is checkpointed in the map they
// arrived in, so a crash mid-transfer cannot leave them recoverable only in a
// room they have already left.
func TestTransferCheckpointsTheDestinationMap(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Bookmark")

	c.standingIn("test", character, c.game.Maps["test"].Portals[0].Bounds)
	s, _ := c.enter(c.a, account, character)

	eventually(t, "the character to arrive", func() bool { return s.MapID() == "annex" })

	saved, err := c.store.LoadCharacter(context.Background(), account, character)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if saved.MapID != "annex" {
		t.Errorf("checkpointed map is %q, want annex", saved.MapID)
	}
}

// --- fast travel -------------------------------------------------------------

func TestWaypointTravelRequiresHavingBeenThere(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Tourist")

	s, sink := c.enter(c.a, account, character)

	// The annex waypoint has never been touched, so naming it is a client
	// asserting a fact rather than asking a question.
	if err := s.Travel(context.Background(), TravelRequest{WaypointID: "wp_annex"}); err != nil {
		t.Fatalf("queueing travel: %v", err)
	}

	eventually(t, "the refusal", func() bool { return len(sink.refusals()) > 0 })

	if got := s.MapID(); got != "test" {
		t.Errorf("character is in %q; a locked waypoint must not move anyone", got)
	}
	if got := sink.refusals()[0]; got != "you have not been there yet" {
		t.Errorf("refusal is %q, which does not say what went wrong", got)
	}
}

func TestUnlockedWaypointTravels(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Wanderer")

	// Unlocked by having stood on it, which is what the room records.
	if err := c.store.UnlockWaypoint(context.Background(), character, "wp_annex"); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	s, _ := c.enter(c.a, account, character)
	if err := s.Travel(context.Background(), TravelRequest{WaypointID: "wp_annex"}); err != nil {
		t.Fatalf("queueing travel: %v", err)
	}

	eventually(t, "the character to arrive", func() bool { return s.MapID() == "annex" })
}

// Standing on a waypoint unlocks it, which is what makes the world map a
// record of where someone has been rather than a list of everything there is.
func TestStandingOnAWaypointUnlocksIt(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Discoverer")

	c.standingIn("test", character, c.game.Maps["test"].Waypoints[0].Bounds)
	c.enter(c.a, account, character)

	eventually(t, "the unlock to be recorded", func() bool {
		got, err := c.store.CharacterWaypoints(context.Background(), character)
		if err != nil {
			t.Fatalf("read waypoints: %v", err)
		}
		return len(got) == 1 && got[0] == "wp_test"
	})
}

// --- channels ----------------------------------------------------------------

// Channel switching is the same handoff as a portal, to the same map, keeping
// the character where they stood.
func TestNewChannelMovesToADifferentInstance(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Hopper")

	s, _ := c.enter(c.a, account, character)
	before := s.Instance()

	if err := s.Travel(context.Background(), TravelRequest{NewChannel: true}); err != nil {
		t.Fatalf("queueing travel: %v", err)
	}

	eventually(t, "a different channel", func() bool { return s.Instance() != before })

	if got := s.MapID(); got != "test" {
		t.Errorf("a channel switch moved the character to %q", got)
	}

	// Both instances satisfy the same key, which is what makes them channels
	// of one zone rather than two unrelated rooms.
	channels, _ := c.dir.InstancesFor(context.Background(), directory.RoomKey{
		MapID: "test", Placement: directory.PlacementShared,
	})
	if len(channels) != 2 {
		t.Errorf("the map has %d channels, want 2", len(channels))
	}
}

// Naming an instance on another map would otherwise be a teleport to anywhere
// in the world, level gates included.
func TestChannelSwitchRefusesAnotherMapsInstance(t *testing.T) {
	c := newCluster(t)
	accountA, alice := c.character("Alice")
	accountB, bob := c.character("Bob")

	// Bob opens a room in the annex, so there is a real instance elsewhere for
	// Alice to name.
	c.standingIn("annex", bob, c.game.Maps["annex"].Portals[0].Bounds)
	bobSession, _ := c.enter(c.b, accountB, bob)
	elsewhere := bobSession.Instance()

	aliceSession, aliceSink := c.enter(c.a, accountA, alice)
	here := aliceSession.Instance()

	if err := aliceSession.Travel(context.Background(),
		TravelRequest{Channel: elsewhere}); err != nil {
		t.Fatalf("queueing travel: %v", err)
	}

	eventually(t, "the refusal", func() bool { return len(aliceSink.refusals()) > 0 })

	if got := aliceSession.Instance(); got != here {
		t.Errorf("Alice moved to instance %d by naming a room on another map", got)
	}
}

// --- placement ---------------------------------------------------------------

// A shared map spreads players across channels; a private one is one instance
// per owner, which is what gives a party its own dungeon in M5.
func TestPrivatePlacementIsOnePerOwner(t *testing.T) {
	c := newCluster(t)

	key := func(owner string) directory.RoomKey {
		return directory.RoomKey{
			MapID: "test", Placement: directory.PlacementPrivate, OwnerKey: owner,
		}
	}

	ctx := context.Background()
	first, err := c.dir.Join(ctx, key("owner-1"), 4)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	again, err := c.dir.Join(ctx, key("owner-1"), 4)
	if err != nil {
		t.Fatalf("second join: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("the same owner got instances %d and %d; a private room is one "+
			"per owner, or a party lands in two dungeons", first.ID, again.ID)
	}

	other, err := c.dir.Join(ctx, key("owner-2"), 4)
	if err != nil {
		t.Fatalf("other owner: %v", err)
	}
	if other.ID == first.ID {
		t.Error("two owners share one private instance")
	}
}

// --- fencing -----------------------------------------------------------------

// A replayed or delayed transfer carrying an old fencing token would resurrect
// a character in a room it has already left, which is how an item gets
// duplicated.
func TestStaleTransferTokensAreRefused(t *testing.T) {
	c := newCluster(t)
	_, character := c.character("Fenced")

	if !c.b.acceptToken(character, 7) {
		t.Fatal("the first token was refused")
	}
	if c.b.acceptToken(character, 6) {
		t.Error("a token older than one already accepted was let through")
	}
	if !c.b.acceptToken(character, 7) {
		t.Error("the same token was refused on retry; a transfer that times out " +
			"and is retried carries the same token")
	}
	if !c.b.acceptToken(character, 8) {
		t.Error("a newer token was refused")
	}
}

// --- room lifecycle ----------------------------------------------------------

// An empty room is a goroutine and twenty wakeups a second, and a world of
// many maps is mostly empty rooms.
func TestIdleRoomsAreTornDown(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Fleeting")

	sink := newCaptureSink()
	s, err := c.a.Enter(context.Background(), account, character, sink)
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	instance := s.(*Session).Instance()

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	s.Close(closeCtx)
	cancel()

	eventually(t, "the room to stop", func() bool {
		return c.nodeHosting(instance) == ""
	})

	if _, ok, _ := c.dir.Lookup(context.Background(), instance); ok {
		t.Error("the instance is still in the directory after its room stopped")
	}
}

// A character who walks through a portal must still be able to walk through
// the next one.
//
// Nothing a live session holds -- the connection, the channel the room reports
// portals and loot on -- can travel in the transfer request, because all of it
// is an in-process reference to the node the player is connected to. It has to
// be handed to the destination room separately, and when it is not, the
// character arrives able to move and unable to do anything else: no loot, no
// portals, no waypoints, and nothing in the logs to say so.
func TestASessionFollowsTheCharacterThroughAPortal(t *testing.T) {
	c := newCluster(t)
	account, character := c.character("Onward")

	// The annex spawn this portal lands on sits inside the annex's waypoint,
	// so arriving unlocks it -- which only happens if the destination room can
	// reach the session.
	c.standingIn("test", character, c.game.Maps["test"].Portals[0].Bounds)
	s, _ := c.enter(c.a, account, character)

	eventually(t, "the character to arrive", func() bool { return s.MapID() == "annex" })

	eventually(t, "the arrival waypoint to be recorded", func() bool {
		got, err := c.store.CharacterWaypoints(context.Background(), character)
		if err != nil {
			t.Fatalf("read waypoints: %v", err)
		}
		for _, id := range got {
			if id == "wp_annex" {
				return true
			}
		}
		return false
	})
}
