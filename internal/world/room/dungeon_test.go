package room

import (
	"io"
	"log/slog"
	"testing"

	"github.com/ctrl-research/mmo/internal/content/contenttest"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Running a dungeon.
//
// What these are testing is that an instance has a shape: that it opens one
// stage at a time, that it does not open the next until the last one is
// actually finished, and that both ways a run can end actually end it.

// newDungeon returns a harness on the test crypt rather than the test map.
func newDungeon(t *testing.T) *harness {
	t.Helper()

	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load test content: %v", err)
	}
	m := game.Maps["crypt"]

	r := New(Config{
		InstanceID: 1,
		MapID:      m.ID,
		Capacity:   6,
		World:      m.World,
		Tuning:     sim.DefaultTuning(),
		Spawn:      m.DefaultSpawn().At,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Content:    game,
		Map:        m,
		Seed:       0xD1CE,
	})

	return &harness{t: t, room: r, game: game, sinks: make(map[EntityID]*recordSink)}
}

// joinWithEvents joins a character whose session records what the room asks of
// it, which is the only way to observe a run ending.
func (h *harness) joinWithEvents(name string) (EntityID, *recordEvents) {
	h.t.Helper()

	events := &recordEvents{}
	sink := newSink()
	result := make(chan joinResult, 1)
	h.room.handle(joinCmd{
		spec: JoinSpec{
			CharacterID: "char-" + name,
			Name:        name,
			Fresh:       true,
			Sink:        sink,
			Events:      events,
			Loadout:     []LoadoutSlot{{SkillID: "slash", Rank: 1}},
		},
		result: result,
	})
	res := <-result
	if res.err != nil {
		h.t.Fatalf("join %s: %v", name, res.err)
	}
	h.sinks[res.id] = sink
	return res.id, events
}

// dungeonStates returns every DungeonState this sink received.
func (s *recordSink) dungeonStates() []*mmov1.DungeonState {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.DungeonState
	for _, m := range s.msgs {
		if d := m.GetEvent().GetDungeon(); d != nil {
			out = append(out, d)
		}
	}
	return out
}

// clearStage kills everything currently alive, repeatedly, until the stage
// stops producing more.
func (h *harness) clearStage(by EntityID) {
	h.t.Helper()

	for i := 0; i < 200; i++ {
		h.tick(1)
		for _, e := range h.mobs(SharedLayer, true) {
			h.room.damage(h.entity(by), e, int(e.HP), false, "physical")
		}
		if h.room.stageIsClear(h.room.dungeon.stage) {
			return
		}
	}
	h.t.Fatal("the stage never cleared")
}

// --- progression ------------------------------------------------------------

func TestADungeonOpensOneStageAtATime(t *testing.T) {
	h := newDungeon(t)
	alice, _ := h.join("alice")
	h.tick(5)

	if h.room.dungeon == nil {
		t.Fatal("a room on a dungeon's map is not running one")
	}

	// The first stage's mobs exist and the second's do not. This is the whole
	// of the progression: no doors, just nothing there yet.
	h.placeAt(alice, 400, 288)
	h.tick(10)

	var outer, inner int
	for _, e := range h.mobs(SharedLayer, true) {
		switch e.Mob.Def.Name {
		case "Test Boss":
			inner++
		default:
			outer++
		}
	}
	if outer == 0 {
		t.Error("the first stage spawned nothing")
	}
	if inner != 0 {
		t.Errorf("the second stage spawned %d before the first was cleared", inner)
	}
}

func TestClearingAStageOpensTheNext(t *testing.T) {
	h := newDungeon(t)
	alice, _ := h.join("alice")
	h.placeAt(alice, 400, 288)
	h.tick(5)

	if got := h.room.dungeon.stage; got != 0 {
		t.Fatalf("started on stage %d", got)
	}

	h.clearStage(alice)
	h.tick(2)

	if got := h.room.dungeon.stage; got != 1 {
		t.Fatalf("clearing the first stage left the run on stage %d", got)
	}

	// And the boss is now real.
	h.tick(20)
	found := false
	for _, e := range h.mobs(SharedLayer, true) {
		if e.Mob.Def.Name == "Test Boss" {
			found = true
		}
	}
	if !found {
		t.Error("the second stage opened but spawned nothing")
	}
}

// A stage that cleared before its mobs had spawned would clear on the tick it
// opened, and the whole dungeon would finish instantly.
func TestAStageIsNotClearBeforeItHasSpawned(t *testing.T) {
	h := newDungeon(t)
	h.join("alice")

	// Before the first tick, nothing has spawned and nothing is alive.
	if h.room.stageIsClear(0) {
		t.Error("the first stage was clear before anything spawned in it")
	}
}

// A stage still holding something alive is not a stage that has been cleared.
// Counting only what had spawned would clear a stage while the party was in
// the middle of fighting it.
func TestAStageWithMobsAliveIsNotClear(t *testing.T) {
	h := newDungeon(t)
	alice, _ := h.join("alice")
	h.placeAt(alice, 400, 288)

	// Long enough for the whole first stage to have appeared, and nothing
	// killed.
	h.tick(60)

	live := len(h.mobs(SharedLayer, true))
	if live == 0 {
		t.Fatal("nothing spawned, so there is nothing to be un-clear about")
	}
	if h.room.stageIsClear(0) {
		t.Errorf("the stage reported clear with %d mobs still alive", live)
	}
}

// Inside a dungeon a spawn point produces its population once. Respawning
// would mean a stage that can never be cleared: the party would be fighting
// the same two dummies until the server gave out.
func TestADungeonStageDoesNotRespawn(t *testing.T) {
	h := newDungeon(t)
	alice, _ := h.join("alice")
	h.placeAt(alice, 400, 288)
	h.tick(5)

	h.clearStage(alice)

	// Well past the spawn point's respawn interval.
	h.tick(80)

	for _, e := range h.mobs(SharedLayer, true) {
		if e.Mob.Def.Name != "Test Boss" {
			t.Fatalf("a cleared stage put %s back; the stage could never finish", e.Mob.Def.Name)
		}
	}
}

// --- how a run ends ---------------------------------------------------------

func TestClearingTheLastStageEndsTheRun(t *testing.T) {
	h := newDungeon(t)
	alice, events := h.joinWithEvents("alice")
	h.placeAt(alice, 400, 288)
	h.tick(5)

	h.clearStage(alice)
	h.tick(2)
	h.clearStage(alice)
	h.tick(2)

	if got := h.room.dungeon.state; got != RunCleared {
		t.Fatalf("the run is %s after the last stage was cleared", got)
	}

	// Not immediately: being teleported out the instant a boss dies denies the
	// party the moment they earned, and the loot on the floor.
	if len(events.runEnds) != 0 {
		t.Error("sent home the instant the boss died")
	}

	h.tick(runLingerTicks + 2)
	if len(events.runEnds) != 1 {
		t.Fatalf("asked to send the party home %d times", len(events.runEnds))
	}
	if !events.runEnds[0].Cleared {
		t.Error("a cleared run was reported as something else, so no lockout would be written")
	}
}

// A party all down at once is a wipe. It is the only thing that ends a run
// short, and it is why death had to be real first.
func TestAPartyAllDownAtOnceWipes(t *testing.T) {
	h := newDungeon(t)
	alice, events := h.joinWithEvents("alice")
	bob, _ := h.joinWithEvents("bob")
	h.tick(5)

	h.room.downPlayer(h.entity(alice))
	h.tick(2)
	if h.room.dungeon.state != RunActive {
		t.Fatal("one player down wiped the run; the other was still fighting")
	}

	h.room.downPlayer(h.entity(bob))
	h.tick(2)

	if got := h.room.dungeon.state; got != RunWiped {
		t.Fatalf("both players down and the run is %s", got)
	}

	h.tick(runLingerTicks + 2)
	if len(events.runEnds) != 1 {
		t.Fatalf("asked to send alice home %d times", len(events.runEnds))
	}
	if events.runEnds[0].Cleared {
		t.Error("a wipe was reported as a clear, which would write a lockout " +
			"for a dungeon the party never beat")
	}
}

// A dropped connection is not a death. A frozen character is invulnerable and
// their party may still be fighting, so counting them among the fallen would
// wipe a run because somebody's wifi went.
func TestADisconnectedPlayerDoesNotWipeTheRun(t *testing.T) {
	h := newDungeon(t)
	alice, _ := h.joinWithEvents("alice")
	bob, _ := h.joinWithEvents("bob")
	h.tick(5)

	h.room.handle(freezeCmd{id: bob})
	h.room.downPlayer(h.entity(alice))
	h.tick(5)

	if got := h.room.dungeon.state; got != RunWiped {
		t.Fatalf("the only player still connected went down and the run is %s", got)
	}
}

// The other half: a frozen character must not hold a wiped run open either.
func TestADisconnectedPlayerDoesNotHoldARunOpen(t *testing.T) {
	h := newDungeon(t)
	alice, _ := h.joinWithEvents("alice")
	bob, _ := h.joinWithEvents("bob")
	h.tick(5)

	// Bob drops without ever going down. Alice falls. If a frozen character
	// counted as standing, the run would never end.
	h.room.handle(freezeCmd{id: bob})
	h.room.downPlayer(h.entity(alice))
	h.tick(5)

	if got := h.room.dungeon.state; got != RunWiped {
		t.Errorf("a disconnected character held the run open at %s", got)
	}
}

// A run that ended does not un-end because somebody stood back up.
func TestAWipedRunStaysWiped(t *testing.T) {
	h := newDungeon(t)
	alice, _ := h.joinWithEvents("alice")
	h.tick(5)

	h.room.downPlayer(h.entity(alice))
	h.tick(2)
	if h.room.dungeon.state != RunWiped {
		t.Fatal("a lone player down did not wipe the run")
	}

	h.tick(h.room.content.Balance.Combat.DownedTicks + 2)

	if got := h.room.dungeon.state; got != RunWiped {
		t.Errorf("the run went back to %s when the character revived", got)
	}
}

// An empty instance is not a wipe. Everyone left, which the idle timeout
// already handles, and calling it a wipe would end a run nobody was in.
func TestAnEmptyInstanceIsNotAWipe(t *testing.T) {
	h := newDungeon(t)
	alice, _ := h.join("alice")
	h.tick(5)

	h.leave(alice)
	h.tick(10)

	if got := h.room.dungeon.state; got != RunActive {
		t.Errorf("an empty instance reported %s", got)
	}
}

// --- what the party is told -------------------------------------------------

func TestTheInstanceIsToldWhereTheRunStands(t *testing.T) {
	h := newDungeon(t)
	alice, _ := h.join("alice")
	sink := h.sinks[alice]
	h.placeAt(alice, 400, 288)
	h.tick(5)

	got := sink.dungeonStates()
	if len(got) == 0 {
		t.Fatal("nobody was told the run had started")
	}
	first := got[0]
	if first.GetStage() != 1 || first.GetStages() != 2 {
		t.Errorf("opened on stage %d of %d, want 1 of 2", first.GetStage(), first.GetStages())
	}
	if first.GetStageName() != "Outer" {
		t.Errorf("opened on %q, want Outer", first.GetStageName())
	}

	h.clearStage(alice)
	h.tick(2)

	got = sink.dungeonStates()
	last := got[len(got)-1]
	if last.GetStage() != 2 || last.GetStageName() != "Inner" {
		t.Errorf("after clearing the first stage the party was told stage %d %q",
			last.GetStage(), last.GetStageName())
	}
}
