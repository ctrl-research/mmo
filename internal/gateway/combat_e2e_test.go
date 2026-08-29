package gateway

import (
	"testing"
	"time"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"google.golang.org/protobuf/proto"
)

// End-to-end combat over a real socket.
//
// The room's own tests drive the simulation directly, which is right for
// verifying behaviour. These verify the parts only a real connection
// exercises: that casts and loot requests survive the wire, that events reach
// the right client, and that one player's hunting never appears in another's
// stream.

func (c *client) cast(skillID string, facingLeft bool) {
	c.send(&mmov1.ClientMessage{Body: &mmov1.ClientMessage_Cast{
		Cast: &mmov1.Cast{SkillId: skillID, FacingLeft: facingLeft},
	}})
}

func (c *client) loot(entityID uint32) {
	c.send(&mmov1.ClientMessage{Body: &mmov1.ClientMessage_Interact{
		Interact: &mmov1.Interact{
			EntityId: entityID,
			Kind:     mmov1.InteractKind_INTERACT_KIND_LOOT,
		},
	}})
}

// collect drains messages for a while, returning every event and snapshot.
func (c *client) collect(d time.Duration) ([]*mmov1.Event, []*mmov1.Snapshot) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c.drain(300 * time.Millisecond)
	}

	// Everything read so far, including what other helpers left behind.
	var events []*mmov1.Event
	var snaps []*mmov1.Snapshot
	for _, m := range c.inbox {
		if e := m.GetEvent(); e != nil {
			events = append(events, e)
		}
		if s := m.GetSnapshot(); s != nil {
			snaps = append(snaps, s)
		}
	}
	c.inbox = c.inbox[:0]
	return events, snaps
}

// mobsIn returns the mob entities a client has been told about.
func mobsIn(snaps []*mmov1.Snapshot) map[uint32]*mmov1.EntityState {
	out := make(map[uint32]*mmov1.EntityState)
	for _, s := range snaps {
		for _, e := range s.GetEntered() {
			if e.GetKind() == mmov1.EntityKind_ENTITY_KIND_MOB {
				out[e.GetId()] = e
			}
		}
	}
	return out
}

func TestMobsReachTheClient(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)
	c.hello(ts.ticket(t, "alice"), ProtocolVersion)
	c.awaitWelcome()

	_, snaps := c.collect(1500 * time.Millisecond)
	mobs := mobsIn(snaps)

	if len(mobs) == 0 {
		t.Fatal("no mobs were ever sent to the client")
	}
	for id, m := range mobs {
		if m.GetMobId() == "" {
			t.Errorf("mob %d arrived with no content id, so the client cannot pick a sprite", id)
		}
		if m.GetHpMax() == 0 {
			t.Errorf("mob %d arrived with no max HP, so no health bar can be drawn", id)
		}
	}
}

// Two players in one room see each other but hunt entirely separate mobs.
// This is the M1 exit criterion, verified over the wire rather than in-process.
func TestTwoPlayersHuntSeparateMobs(t *testing.T) {
	ts := newTestServer(t)

	a := ts.dial(t)
	a.hello(ts.ticket(t, "alice"), ProtocolVersion)
	wa := a.awaitWelcome()

	b := ts.dial(t)
	b.hello(ts.ticket(t, "bob"), ProtocolVersion)
	wb := b.awaitWelcome()

	_, snapsA := a.collect(1500 * time.Millisecond)
	_, snapsB := b.collect(1500 * time.Millisecond)

	mobsA := mobsIn(snapsA)
	mobsB := mobsIn(snapsB)

	if len(mobsA) == 0 || len(mobsB) == 0 {
		t.Fatalf("expected mobs for both players, got %d and %d", len(mobsA), len(mobsB))
	}

	// The shared-layer statue is common to both; everything else must not be.
	sharedSeen := 0
	for id, m := range mobsA {
		if _, both := mobsB[id]; !both {
			continue
		}
		if m.GetMobId() == "test_statue" {
			sharedSeen++
			continue
		}
		t.Errorf("mob %d (%s) was sent to both players but is not shared-layer",
			id, m.GetMobId())
	}
	if sharedSeen == 0 {
		t.Error("the shared-layer mob reached neither player, so nothing common exists")
	}

	// And they must still see each other, which is the whole point of sharing
	// a room rather than each getting a private instance.
	if !sawPlayer(snapsA, wb.GetEntityId()) {
		t.Error("alice never saw bob")
	}
	if !sawPlayer(snapsB, wa.GetEntityId()) {
		t.Error("bob never saw alice")
	}
}

func sawPlayer(snaps []*mmov1.Snapshot, id uint32) bool {
	for _, s := range snaps {
		for _, e := range s.GetEntered() {
			if e.GetId() == id && e.GetKind() == mmov1.EntityKind_ENTITY_KIND_PLAYER {
				return true
			}
		}
	}
	return false
}

// fightTimeout is how long a driven fight is given to reach a kill.
//
// It takes a little over two seconds on an idle machine: walk three hundred
// units, then land two swings half a second apart. The generous multiple is
// for CI, where this package shares a runner with the whole suite under the
// race detector and the driver's own ticker is the first thing to starve --
// the character walks at whatever rate its intents actually get sent, while
// the room ticks on regardless.
//
// Twenty seconds was not enough for that, and failed as "dealt damage but
// never killed anything": far enough to reach a mob, not far enough to swing
// at it twice. A timeout this far above the real duration cannot fail for
// being slow, only for being broken -- and it costs nothing in the passing
// case, which is every case.
const fightTimeout = 90 * time.Second

// A full combat loop over the wire: walk to a mob, hit it until it dies, gain
// experience, and see loot appear.
func TestFullCombatLoopOverTheWire(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)
	c.hello(ts.ticket(t, "alice"), ProtocolVersion)
	c.awaitWelcome()

	defer c.driveIntoTheFight()()

	var snaps []*mmov1.Snapshot
	sawDamage, sawDeath, sawExp := false, false, false

	deadline := time.Now().Add(fightTimeout)
	for time.Now().Before(deadline) && !(sawDeath && sawExp) {
		ev, sn := c.collect(250 * time.Millisecond)
		snaps = append(snaps, sn...)

		for _, e := range ev {
			switch e.Body.(type) {
			case *mmov1.Event_Damage:
				sawDamage = true
			case *mmov1.Event_Died:
				sawDeath = true
			case *mmov1.Event_ExpGained:
				sawExp = true
			}
		}
	}

	if !sawDamage {
		t.Fatalf("never dealt damage after %s of running right and swinging", fightTimeout)
	}
	if !sawDeath {
		t.Fatal("dealt damage but never killed anything")
	}
	if !sawExp {
		t.Error("killed something but gained no experience")
	}

	// Loot should be on the ground, and it must be a drop entity so the client
	// can render and target it.
	var dropID uint32
	for _, s := range snaps {
		for _, e := range s.GetEntered() {
			if e.GetKind() != mmov1.EntityKind_ENTITY_KIND_DROP {
				continue
			}
			dropID = e.GetId()
			if e.GetDropGold() == 0 && e.GetDropItem() == "" {
				t.Errorf("drop %d carries neither gold nor an item", e.GetId())
			}
		}
	}
	if dropID == 0 {
		t.Fatal("a kill produced no ground loot, though the test table always drops")
	}

	// And looting it must be acknowledged. The player is standing where the
	// mob died, so range is not in question.
	sawLoot := false
	for i := 0; i < 60 && !sawLoot; i++ {
		c.loot(dropID)
		ev, _ := c.collect(150 * time.Millisecond)
		for _, e := range ev {
			if e.GetLootTaken() != nil {
				sawLoot = true
			}
		}
	}
	if !sawLoot {
		t.Error("looting a drop the player is standing on was never acknowledged")
	}
}

// Experience and loot are nobody else's business.
func TestProgressionEventsAreNotBroadcast(t *testing.T) {
	ts := newTestServer(t)

	a := ts.dial(t)
	a.hello(ts.ticket(t, "alice"), ProtocolVersion)
	a.awaitWelcome()

	b := ts.dial(t)
	b.hello(ts.ticket(t, "bob"), ProtocolVersion)
	b.awaitWelcome()

	// Alice fights; bob stands still.
	defer a.driveIntoTheFight()()

	aliceGained := false
	deadline := time.Now().Add(fightTimeout)
	for time.Now().Before(deadline) && !aliceGained {
		ev, _ := a.collect(250 * time.Millisecond)
		for _, e := range ev {
			if e.GetExpGained() != nil {
				aliceGained = true
			}
		}
	}
	if !aliceGained {
		t.Fatal("alice never reached a kill, so there is nothing to assert about bob")
	}

	bobEvents, _ := b.collect(700 * time.Millisecond)
	for _, e := range bobEvents {
		if e.GetExpGained() != nil {
			t.Error("bob received alice's experience event")
		}
		if e.GetLootTaken() != nil {
			t.Error("bob received alice's loot event")
		}
	}
}

// A client may only cast what it has been granted. Without this check a client
// could swing with a mob's ability, or with anything else in the content set.
func TestClientCannotCastArbitrarySkills(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)
	c.hello(ts.ticket(t, "alice"), ProtocolVersion)
	c.awaitWelcome()

	for i := 0; i < 30; i++ {
		c.cast("mob_bite", false)
		c.cast("definitely_not_a_skill", false)
	}

	events, _ := c.collect(1500 * time.Millisecond)
	for _, e := range events {
		if sc := e.GetSkillCast(); sc != nil && sc.GetSkillId() != "slash" {
			t.Errorf("server resolved a cast of %q, which the player was never granted",
				sc.GetSkillId())
		}
	}
}

// Casts share the input rate limit, so a client cannot use them to bypass it.
func TestCastFloodIsRateLimited(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)
	c.hello(ts.ticket(t, "alice"), ProtocolVersion)
	c.awaitWelcome()

	// Far above the legitimate rate of one per simulated tick.
	for i := 0; i < 400; i++ {
		c.cast("slash", false)
	}

	// The connection must survive rather than wedge, and the room must keep
	// ticking for everyone else in it.
	_, snaps := c.collect(1500 * time.Millisecond)
	if len(snaps) == 0 {
		t.Error("the room stopped sending snapshots after a cast flood")
	}
}

func TestSelfStateCarriesProgression(t *testing.T) {
	ts := newTestServer(t)
	c := ts.dial(t)
	c.hello(ts.ticket(t, "alice"), ProtocolVersion)
	c.awaitWelcome()

	_, snaps := c.collect(800 * time.Millisecond)
	if len(snaps) == 0 {
		t.Fatal("no snapshots")
	}
	self := snaps[len(snaps)-1].GetSelf()

	if self.GetLevel() == 0 {
		t.Error("self state carries no level, so the HUD cannot show progression")
	}
	if self.GetExpToNext() == 0 {
		t.Error("self state carries no exp requirement, so no progress bar is possible")
	}
	if self.GetHpMax() == 0 {
		t.Error("self state carries no max HP")
	}
}

// Field numbers are permanent. A client and server disagreeing about what
// field 7 means is a class of bug that is very hard to see, so the wire format
// is asserted rather than assumed.
func TestEventEncodingRoundTrips(t *testing.T) {
	original := &mmov1.Event{Body: &mmov1.Event_Damage{Damage: &mmov1.DamageDealt{
		SourceId: 7, TargetId: 42, Amount: 1234, Critical: true, Element: "fire",
	}}}

	blob, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got mmov1.Event
	if err := proto.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	d := got.GetDamage()
	if d == nil {
		t.Fatal("damage event did not survive the round trip")
	}
	if d.GetSourceId() != 7 || d.GetTargetId() != 42 || d.GetAmount() != 1234 ||
		!d.GetCritical() || d.GetElement() != "fire" {
		t.Errorf("round trip changed the event: %+v", d)
	}
}

// driveIntoTheFight runs right and swings at the simulation's own rate, and
// returns the function that stops it.
//
// At the simulation's rate rather than once per collect window: a client that
// sends five intents a second walks at a fifth speed and swings far below its
// cooldown, which under parallel test load is the difference between reaching
// a kill and timing out.
//
// It swings both ways. Running right forever pins the character against the
// far wall, where a mob that chased it is as likely to end up behind as in
// front, and a driver that only ever swung forward would be a coin flip on
// whether the fight happened there at all. It costs nothing to cover both.
//
// Every third attempt rather than every other, because attempts come twice as
// often as the cooldown allows: the server refuses one of each pair, and with
// two directions alternating the accepted half would be the same direction
// every time. Three against two has no such alias.
//
// Write errors are swallowed rather than failing the test. This runs on its
// own goroutine, where t.Fatal only stops that goroutine, and t.Cleanup closes
// the connection once the test returns -- so a driver still mid-write at that
// moment would fail a test that had already passed. Stopping waits for it to
// exit, which closes that window rather than narrowing it.
func (c *client) driveIntoTheFight() (stop func()) {
	done := make(chan struct{})
	quit := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(room.TickPeriod)
		defer ticker.Stop()

		seq := uint32(0)
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				seq++
				// The owner-layer spawn sits to the right of the player spawn,
				// so running right walks into the fight.
				if c.trySend(&mmov1.ClientMessage{Body: &mmov1.ClientMessage_Intent{
					Intent: &mmov1.Intent{Seq: seq, MoveX: 1000},
				}}) != nil {
					return
				}
				if seq%5 != 0 {
					continue
				}
				if c.trySend(&mmov1.ClientMessage{Body: &mmov1.ClientMessage_Cast{
					Cast: &mmov1.Cast{SkillId: "slash", FacingLeft: (seq/5)%3 == 0},
				}}) != nil {
					return
				}
			}
		}
	}()

	return func() {
		close(quit)
		<-done
	}
}
