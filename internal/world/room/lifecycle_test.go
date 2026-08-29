package room

import (
	"testing"

	"github.com/ctrl-research/mmo/internal/fixed"
)

// --- proximity spawning -----------------------------------------------------

// mobsNear counts live mobs whose feet are within reach of an x coordinate.
func (h *harness) mobsNear(x int) int {
	n := 0
	for _, e := range h.mobs(0, true) {
		if (e.Body.FeetCenter().X - fixed.FromInt(x)).Abs() <= fixed.FromInt(64) {
			n++
		}
	}
	return n
}

// The test map has a spawn point at x=1250, well beyond the activation range
// from the spawn at x=100. Under layering a room holds roughly layers x mobs
// entities, and simulating the ones nobody is anywhere near is pure cost.
func TestDistantSpawnPointsStayEmpty(t *testing.T) {
	h := newHarness(t)
	h.join("alice")
	h.tick(60)

	if n := h.mobsNear(1250); n != 0 {
		t.Errorf("%d mobs spawned 1150 units from the only player; "+
			"spawn points beyond the activation range should stay empty", n)
	}

	// The near spawn point is unaffected, or the gate is simply broken.
	if n := h.mobsNear(400); n == 0 {
		t.Error("no mobs at the spawn point next to the player")
	}
}

func TestApproachingASpawnPointPopulatesIt(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(60)

	h.placeAt(alice, 1200, 288)
	h.tick(3)

	if n := h.mobsNear(1250); n == 0 {
		t.Fatal("walking up to a spawn point left it empty; " +
			"a player should never see the gate that populates the map")
	}
}

// Without this the saving only ever applies to ground a player has not walked
// yet: cross a map once and every spawn point on it is populated for as long
// as the room lives.
func TestWalkingAwayCullsMobs(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")

	h.placeAt(alice, 1200, 288)
	h.tick(3)
	if h.mobsNear(1250) == 0 {
		t.Fatal("setup: the far spawn point never populated")
	}

	h.placeAt(alice, 100, 288)
	h.tick(cullInterval + 1)

	if n := h.mobsNear(1250); n != 0 {
		t.Errorf("%d mobs still simulated 1100 units from the only player", n)
	}
}

// A culled population must not leave its spawn point on a respawn timer: the
// player did not kill anything, so returning should find the area as they left
// it rather than empty for a respawn interval.
func TestReturningRepopulatesImmediately(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")

	h.placeAt(alice, 1200, 288)
	h.tick(3)
	h.placeAt(alice, 100, 288)
	h.tick(cullInterval + 1)

	h.placeAt(alice, 1200, 288)
	h.tick(2)

	if n := h.mobsNear(1250); n == 0 {
		t.Error("returning to a culled area found it empty; the spawn timer " +
			"should not be running for mobs the engine removed")
	}
}

// A mob chasing a player must not evaporate because it has strayed from the
// spawn point that made it.
func TestChasingMobsAreNeverCulled(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(3)

	mobs := h.mobs(0, true)
	if len(mobs) == 0 {
		t.Fatal("setup: no mobs")
	}
	target := mobs[0]

	// Drag both far from every spawn point, with the mob locked onto the
	// player, which is exactly the state a cull must not touch.
	h.placeAt(alice, 1250, 288)
	target.Body.SetFeetCenter(h.entity(alice).Body.FeetCenter())
	target.Mob.State = aiChase
	target.Mob.Target = alice

	h.tick(cullInterval + 1)

	if h.room.entity(target.ID) == nil {
		t.Error("a mob chasing the player was culled out from under them")
	}
}

// --- idle teardown ----------------------------------------------------------

func TestEmptyRoomRetiresAfterTheIdleTimeout(t *testing.T) {
	h := newHarness(t)
	asked := 0
	h.room.cfg.IdleTicks = 10
	h.room.cfg.Retire = func() bool { asked++; return true }

	alice, _ := h.join("alice")
	h.tick(30)
	if asked != 0 || h.room.retiring {
		t.Fatalf("a room with a player in it asked to retire (%d times)", asked)
	}

	h.leave(alice)
	h.tick(11)

	if asked != 1 {
		t.Errorf("Retire called %d times, want exactly 1", asked)
	}
	if !h.room.retiring {
		t.Error("the room did not retire after standing empty past its timeout")
	}
}

// The node refuses when somebody has just been placed here and their join is
// still in flight. Retiring anyway would strand them in a room that stops
// ticking a moment later.
func TestARefusedRetirementRestartsTheClock(t *testing.T) {
	h := newHarness(t)
	asked := 0
	h.room.cfg.IdleTicks = 10
	h.room.cfg.Retire = func() bool { asked++; return false }

	h.tick(30)

	if asked == 0 {
		t.Fatal("Retire was never called")
	}
	if h.room.retiring {
		t.Error("the room retired even though the node refused")
	}
	if asked > 2 {
		t.Errorf("Retire asked %d times in 30 ticks; a refusal should restart "+
			"the idle clock rather than retry every tick", asked)
	}
}

func TestARoomWithNoRetireCallbackRunsForever(t *testing.T) {
	h := newHarness(t)
	h.room.cfg.IdleTicks = 10

	h.tick(100)

	if h.room.retiring {
		t.Error("a room with no Retire callback stopped itself")
	}
}
