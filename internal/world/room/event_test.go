package room

import (
	"io"
	"log/slog"
	"testing"

	"github.com/ctrl-research/mmo/internal/content/contenttest"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// newGlade returns a harness on the events map rather than the main test map.
//
// Its own map on purpose: borrowing corners of the shared one was tried, and
// every borrowed spawn point or shrine broke a neighbouring suite.
func newGlade(t *testing.T) *harness {
	t.Helper()

	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load test content: %v", err)
	}
	m := game.Maps["glade"]

	r := New(Config{
		InstanceID: 1,
		MapID:      m.ID,
		Capacity:   8,
		World:      m.World,
		Tuning:     sim.DefaultTuning(),
		Spawn:      m.DefaultSpawn().At,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Content:    game,
		Map:        m,
		Seed:       0x9EED,
	})

	return &harness{t: t, room: r, game: game, sinks: make(map[EntityID]*recordSink)}
}

// Zone events.
//
// The rules these protect: an event's mobs exist only while it runs, it starts
// the way its content says and no other way, and what it produced is gone when
// it is over.

// zoneEvents returns every ZoneEvent this sink received.
func (s *recordSink) zoneEvents() []*mmov1.ZoneEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.ZoneEvent
	for _, m := range s.msgs {
		if z := m.GetEvent().GetZone(); z != nil {
			out = append(out, z)
		}
	}
	return out
}

// eventNamed returns the room's clock for one event.
func (h *harness) eventNamed(id string) *eventState {
	h.t.Helper()
	for _, state := range h.room.events {
		if state.def.ID == id {
			return state
		}
	}
	h.t.Fatalf("the room is not running event %q", id)
	return nil
}

// runUntil ticks until a predicate holds, or fails.
//
// Tests drive off the room's own clocks rather than recomputing them: an event
// has a period, a duration and a cooldown, and a test that adds those up by
// hand is a test that breaks when the tuning changes and lies about why.
func (h *harness) runUntil(what string, cond func() bool) {
	h.t.Helper()
	for i := 0; i < 60*TickRate; i++ {
		if cond() {
			return
		}
		h.tick(1)
	}
	h.t.Fatalf("waited 60 seconds for %s", what)
}

// tideMobs counts the mobs the tide event has produced.
func (h *harness) tideMobs() int {
	n := 0
	for _, sp := range h.room.eventSpawns("test_tide") {
		for _, e := range h.room.entities {
			if e.Mob != nil && e.Mob.Spawn == sp {
				n++
			}
		}
	}
	return n
}

// --- what an event owns -----------------------------------------------------

// An event's spawn points produce nothing at any other time. That is the whole
// mechanism, and it is what makes the event the only reason those mobs exist.
func TestAnEventsMobsDoNotExistBeforeItStarts(t *testing.T) {
	h := newGlade(t)
	h.join("alice")
	h.tick(2)

	if h.eventNamed("test_tide").active {
		t.Fatal("a timed event was running on the first tick, before its period had passed")
	}
	if got := h.tideMobs(); got != 0 {
		t.Errorf("%d event mobs exist with no event running", got)
	}
}

func TestATimedEventStartsAndProducesMobs(t *testing.T) {
	h := newGlade(t)
	alice, sink := h.join("alice")
	h.placeAt(alice, 200, 288)

	state := h.eventNamed("test_tide")
	h.runUntil("the timed event to start", func() bool { return state.active })

	h.runUntil("the event to produce mobs", func() bool { return h.tideMobs() > 0 })
	if !state.active {
		t.Fatal("the event ended before it produced anything")
	}

	got := sink.zoneEvents()
	if len(got) == 0 || !got[0].GetActive() {
		t.Fatalf("the room was not told the event started: %v", got)
	}
	if got[0].GetMessage() == "" {
		t.Error("started with no announcement; an event nobody noticed is one " +
			"nobody takes part in")
	}
}

// A timed event in an empty room would burn its cooldown on nobody, and
// somebody walking in would find the aftermath rather than the event.
func TestATimedEventWaitsForSomebodyToBeThere(t *testing.T) {
	h := newGlade(t)

	state := h.eventNamed("test_tide")
	h.tick(state.def.EveryTicks * 3)

	if state.active {
		t.Error("an event ran in an empty room")
	}

	alice, _ := h.join("alice")
	_ = alice
	h.tick(2)

	if !state.active {
		t.Error("somebody arrived well past the period and the event did not start")
	}
}

// --- ending -----------------------------------------------------------------

// What is left over is cleared. Mobs from a finished event are
// indistinguishable from the zone getting harder every time one runs.
func TestEndingAnEventClearsWhatItProduced(t *testing.T) {
	h := newGlade(t)
	alice, sink := h.join("alice")
	h.placeAt(alice, 200, 288)

	state := h.eventNamed("test_tide")
	h.runUntil("mobs from the event", func() bool { return h.tideMobs() > 0 })

	h.runUntil("the event to end", func() bool { return !state.active })
	if got := h.tideMobs(); got != 0 {
		t.Errorf("%d event mobs survived the event that made them", got)
	}

	events := sink.zoneEvents()
	if last := events[len(events)-1]; last.GetActive() {
		t.Error("the room was never told the event ended")
	}
}

// The cooldown is what stops an event running back to back.
func TestAnEventWaitsOutItsCooldown(t *testing.T) {
	h := newGlade(t)
	alice, _ := h.join("alice")
	h.placeAt(alice, 200, 288)

	state := h.eventNamed("test_tide")
	h.runUntil("the first run to start", func() bool { return state.active })
	h.runUntil("the first run to end", func() bool { return !state.active })

	// Well inside the wait it just set for itself.
	until := state.readyAt
	for h.room.tick < until-2 {
		h.tick(1)
		if state.active {
			t.Fatalf("started again at tick %d, before its own wait ended at %d",
				h.room.tick, until)
		}
	}

	h.runUntil("the event to come round again", func() bool { return state.active })
}

// A second run produces a full wave rather than remembering that the first one
// already made its quota.
func TestASecondRunProducesAFullWave(t *testing.T) {
	h := newGlade(t)
	alice, _ := h.join("alice")
	h.placeAt(alice, 200, 288)

	state := h.eventNamed("test_tide")
	// Long enough for the whole wave, so "a full wave" means something.
	h.runUntil("the first run to fill up", func() bool { return h.tideMobs() >= 3 })
	first := h.tideMobs()

	h.runUntil("the first run to end", func() bool { return !state.active })
	h.runUntil("the second run to start", func() bool { return state.active })
	// Waiting for the same wave size rather than for the first mob: the point
	// is that a second run is not rationed by what the first one already
	// produced, and one mob would satisfy a weaker check either way.
	h.runUntil("the second run to fill up", func() bool { return h.tideMobs() >= first })

	if got := h.tideMobs(); got < first {
		t.Errorf("the second run produced %d against the first run's %d", got, first)
	}
}

// --- shrines ----------------------------------------------------------------

// A shrine-triggered event does not start on its own, ever. Walking past one
// being a decision is the entire difference between it and a timer.
func TestAShrineEventNeverStartsByItself(t *testing.T) {
	h := newGlade(t)
	h.join("alice")

	state := h.eventNamed("test_shrine")
	h.tick(20 * TickRate)

	if state.active {
		t.Error("a shrine event started with nobody touching the shrine")
	}
}

func TestTouchingAShrineStartsItsEvent(t *testing.T) {
	h := newGlade(t)
	alice, sink := h.join("alice")
	h.tick(2)

	state := h.eventNamed("test_shrine")
	shrine := h.room.mapDef.Shrines[0]
	h.placeAt(alice, shrine.Bounds.CenterX().Int(), shrine.Bounds.Bottom().Int())
	h.tick(2)

	if !state.active {
		t.Fatal("standing in the shrine did not start its event")
	}

	for _, z := range sink.zoneEvents() {
		if z.GetEventId() == "test_shrine" && z.GetActive() {
			return
		}
	}
	t.Error("the room was not told the shrine's event started")
}

// The shrine is visible, because a trigger nobody can see is one they step
// into by accident.
func TestAShrineIsAnEntityPlayersCanSee(t *testing.T) {
	h := newGlade(t)
	h.join("alice")
	h.tick(2)

	for _, e := range h.room.entities {
		if e.Kind == KindShrine {
			if e.Name == "" {
				t.Error("the shrine has no name, so nothing tells a player what it is")
			}
			return
		}
	}
	t.Error("the map has a shrine and the room spawned nothing for it")
}

// Touching it again while it runs, or during its cooldown, does nothing. A
// shrine without that is a button somebody stands next to and presses forever.
func TestAShrineCannotBeSpammed(t *testing.T) {
	h := newGlade(t)
	alice, sink := h.join("alice")
	h.tick(2)

	state := h.eventNamed("test_shrine")
	shrine := h.room.mapDef.Shrines[0]
	h.placeAt(alice, shrine.Bounds.CenterX().Int(), shrine.Bounds.Bottom().Int())

	// Standing on it for the whole run, and then well into the cooldown.
	h.runUntil("the shrine event to start", func() bool { return state.active })
	h.runUntil("it to end", func() bool { return !state.active })
	h.tick(state.def.CooldownTicks / 2)

	starts := 0
	for _, z := range sink.zoneEvents() {
		if z.GetEventId() == "test_shrine" && z.GetActive() {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("the event started %d times while somebody stood on the shrine", starts)
	}
}
