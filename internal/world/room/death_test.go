package room

import (
	"testing"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Death, and coming back from it.
//
// What these are testing is that dying costs something and is over: that a
// character stays down for the clock, that the fight goes on without them,
// and that they come back whole rather than back where they died.

// kill puts a character down by dealing exactly their remaining health.
func (h *harness) down(id EntityID) *Entity {
	h.t.Helper()

	e := h.entity(id)
	// Through the shield as well as the health: a killing blow that the
	// shield eats is not a killing blow.
	h.room.damage(e, e, int(e.HP+e.Shield), false, "physical")
	// One tick, so the events queued by the death reach the sink -- they are
	// batched and flushed with the snapshot, like every other event.
	h.tick(1)
	if !isDowned(e) {
		h.t.Fatalf("%s took a killing blow and is still up at %d health", e.Name, e.HP)
	}
	return e
}

// damageEvents returns every DamageDealt event this sink received.
func (s *recordSink) damageEvents() []*mmov1.DamageDealt {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.DamageDealt
	for _, m := range s.msgs {
		if d := m.GetEvent().GetDamage(); d != nil {
			out = append(out, d)
		}
	}
	return out
}

// downedEvents returns every Downed event this sink received.
func (s *recordSink) downedEvents() []*mmov1.Downed {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.Downed
	for _, m := range s.msgs {
		if d := m.GetEvent().GetDowned(); d != nil {
			out = append(out, d)
		}
	}
	return out
}

func TestDeathPutsACharacterDownRatherThanBackOnTheirFeet(t *testing.T) {
	h := newHarness(t)
	alice, sink := h.join("alice")
	h.tick(2)

	e := h.down(alice)

	if e.HP != 0 {
		t.Errorf("health is %d after a killing blow; a death that heals is not a death", e.HP)
	}

	got := sink.downedEvents()
	if len(got) != 1 {
		t.Fatalf("sent %d downed events, want exactly one", len(got))
	}
	if got[0].GetReviveInMs() == 0 {
		t.Error("the countdown is zero, so the client has nothing to show")
	}
}

// The clock is the whole design: a fight you can rejoin the moment you lose it
// is a fight with no stakes.
func TestADownedCharacterStaysDownForTheClock(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	// Spent, so the restore has something to restore. A character who died at
	// full mana would pass this whether or not reviving refilled anything.
	h.entity(alice).Player.MP = 0

	e := h.down(alice)
	until := e.Player.ReviveAt

	for h.room.tick < until {
		h.tick(1)
		if !isDowned(e) && h.room.tick < until {
			t.Fatalf("back up at tick %d, %d short of the clock", h.room.tick, until-h.room.tick)
		}
	}

	h.tick(1)
	if isDowned(e) {
		t.Error("the clock ran out and the character is still down")
	}
	if e.HP != e.MaxHP {
		t.Errorf("came back at %d of %d health; a revive that leaves you nearly "+
			"dead just puts you back where you were", e.HP, e.MaxHP)
	}
	if e.Player.MP != e.Player.MaxMP {
		t.Errorf("came back with %d of %d mana; a caster revived empty cannot "+
			"do the thing they came back to do", e.Player.MP, e.Player.MaxMP)
	}
}

// Back at the spawn point, not where the body fell. Reviving in place is what
// turns a lost fight into a war of attrition the player cannot lose.
func TestRevivingReturnsToTheSpawnPoint(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	h.placeAt(alice, 600, 288)
	where := h.entity(alice).Body.Pos

	e := h.down(alice)
	h.tick(h.room.content.Balance.Combat.DownedTicks + 2)

	if e.Body.Pos == where {
		t.Errorf("came back at %v, where the body fell", where)
	}
	if got := e.Body.FeetCenter(); (got.X - h.room.cfg.Spawn.X).Abs() > sim.PlayerSize.W {
		t.Errorf("came back at %v, want the spawn point at %v", got, h.room.cfg.Spawn)
	}
}

// --- coming back next to what killed you ------------------------------------

// A spawn point is a fixed place and something is often standing on it.
// Without a moment's grace, coming back means dying again before you can move,
// and paying the penalty each time.
func TestComingBackIsBrieflySafe(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.down(alice)
	h.tick(h.room.content.Balance.Combat.DownedTicks + 2)
	if isDowned(e) {
		t.Fatal("still down, so there is no grace to test")
	}

	full := e.HP
	h.room.damage(e, e, 50, false, "physical")
	if e.HP != full {
		t.Errorf("took %d damage inside the revive grace", full-e.HP)
	}

	h.tick(h.room.content.Balance.Combat.ReviveGraceTicks + 2)
	h.room.damage(e, e, 50, false, "physical")
	if e.HP == full {
		t.Error("still untouchable after the grace ran out; a character who " +
			"cannot be hit is not playing the game")
	}
}

// Grace that survived an attack would be a free opening rather than a chance
// to get clear: walk into a fight untouchable, swing first, then become a
// target.
func TestAttackingEndsTheReviveGrace(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.down(alice)
	h.tick(h.room.content.Balance.Combat.DownedTicks + 2)

	if !h.room.isProtected(e) {
		t.Fatal("no grace to give up")
	}

	h.cast(alice, "slash", false)
	h.tick(1)

	if h.room.isProtected(e) {
		t.Fatal("still protected after swinging")
	}

	full := e.HP
	h.room.damage(e, e, 50, false, "physical")
	if e.HP == full {
		t.Error("took no damage after attacking, so the grace outlived the swing")
	}
}

// --- what death costs -------------------------------------------------------

func TestDeathCostsProgressTowardTheCurrentLevel(t *testing.T) {
	h := newHarness(t)
	alice, sink := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	e.Player.Exp = 1000
	e.Player.Level = 5

	h.down(alice)

	// The test content charges a tenth.
	if e.Player.Exp != 900 {
		t.Errorf("left with %d experience of 1000, want 900", e.Player.Exp)
	}
	if e.Player.Level != 5 {
		t.Errorf("death took a level: %d, want 5", e.Player.Level)
	}
	if got := sink.downedEvents(); len(got) != 1 || got[0].GetExpLost() != 100 {
		t.Errorf("reported %v lost; an unexplained drop in a number reads as a bug", got)
	}
}

// A penalty that rounds to nothing still takes something, so the rule reads
// the same at every level rather than quietly switching off for anyone who has
// only just levelled.
func TestASmallAmountOfProgressStillCosts(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	// A tenth of five rounds to zero.
	e.Player.Exp = 5

	h.down(alice)

	if e.Player.Exp != 4 {
		t.Errorf("left with %d of 5; a tenth of five rounds to nothing, and "+
			"nothing is not what a death costs", e.Player.Exp)
	}
}

// A character who just levelled has nothing to lose, and must not go negative
// into the level below.
func TestDeathWithNoProgressCostsNothing(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	e.Player.Exp = 0
	e.Player.Level = 3

	h.down(alice)

	if e.Player.Exp != 0 {
		t.Errorf("experience went to %d; a death must never reach into the level below", e.Player.Exp)
	}
	if e.Player.Level != 3 {
		t.Errorf("level went to %d, want 3", e.Player.Level)
	}
}

// --- being out of the fight -------------------------------------------------

// Buffs belong to the fight they were part of. Keeping them would mean coming
// back stronger for having died, or coming back on fire.
func TestDeathClearsBuffsAndShields(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	h.room.applyBuff(e, e, h.game.Buffs["test_might"], 1, 0)
	h.room.applyShield(e, e, 500, 0)

	if len(e.Buffs) == 0 || e.Shield == 0 {
		t.Fatal("nothing to lose: the buff or the shield never applied")
	}

	h.down(alice)

	if len(e.Buffs) != 0 {
		t.Errorf("still holding %d buffs", len(e.Buffs))
	}
	if e.Shield != 0 {
		t.Errorf("still holding a %d shield", e.Shield)
	}
}

// A hit that lands on a body does nothing at all.
//
// This is what makes one death one death: the penalty is charged where the
// character goes down, and nothing can reach them to charge it again. Without
// it, dying in front of a mob would cost a tenth of the level per swing until
// the clock ran out.
func TestHittingADownedCharacterDoesNothing(t *testing.T) {
	h := newHarness(t)
	alice, sink := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	e.Player.Exp = 1000

	h.down(alice)
	first := e.Player.ReviveAt

	h.tick(2)
	before := len(sink.damageEvents())
	for i := 0; i < 5; i++ {
		h.room.damage(e, e, 50, false, "physical")
	}
	h.tick(2)

	// No damage numbers either. A body that soaks visible hits reads as a
	// character still in the fight and losing it.
	if got := len(sink.damageEvents()) - before; got != 0 {
		t.Errorf("%d hits landed on a body", got)
	}
	if e.Player.ReviveAt != first {
		t.Errorf("the clock restarted at %d from %d", e.Player.ReviveAt, first)
	}
	if e.Player.Exp != 900 {
		t.Errorf("paid the penalty again: %d experience left of 1000", e.Player.Exp)
	}
	if got := sink.downedEvents(); len(got) != 1 {
		t.Errorf("sent %d downed events for one death", len(got))
	}
}

// Downing someone who is already down is not a second death.
//
// Nothing reaches a body through combat -- damage refuses the dead -- so this
// is the function's own contract rather than a path a fight can take today.
// It is worth holding: the next caller is a dungeon wipe, which downs a whole
// party at once regardless of who was already lying there.
func TestDowningACharacterTwiceIsOneDeath(t *testing.T) {
	h := newHarness(t)
	alice, sink := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	e.Player.Exp = 1000

	h.room.downPlayer(e)
	first := e.Player.ReviveAt

	h.tick(2)
	h.room.downPlayer(e)
	h.tick(1)

	if e.Player.ReviveAt != first {
		t.Errorf("the clock restarted at %d from %d", e.Player.ReviveAt, first)
	}
	if e.Player.Exp != 900 {
		t.Errorf("charged twice: %d experience left of 1000", e.Player.Exp)
	}
	if got := sink.downedEvents(); len(got) != 1 {
		t.Errorf("sent %d downed events for one death", len(got))
	}
}

// recordEvents captures what the room asks a session to do.
//
// The harness normally joins players with no session at all, which is fine for
// anything that stays inside the tick. A portal does not: it hands off to the
// session, so a test about portals with no session behind it would pass
// whatever the room did.
type recordEvents struct {
	portals  []PortalRequest
	runEnds  []RunResult
	yields   []GatherYield
	stations []StationRequest
	crafts   []CraftRequest
}

func (e *recordEvents) ClaimLoot(LootClaim)                       {}
func (e *recordEvents) GrantGather(y GatherYield)                 { e.yields = append(e.yields, y) }
func (e *recordEvents) OpenStation(r StationRequest)              { e.stations = append(e.stations, r) }
func (e *recordEvents) RunCraft(r CraftRequest)                   { e.crafts = append(e.crafts, r) }
func (e *recordEvents) EnterPortal(req PortalRequest)             { e.portals = append(e.portals, req) }
func (e *recordEvents) DiscoverWaypoint(EntityID, string, string) {}
func (e *recordEvents) EndRun(res RunResult)                      { e.runEnds = append(e.runEnds, res) }

// standingInAPortal joins a character and puts them in the map's first portal.
func standingInAPortal(t *testing.T, h *harness, name string) (EntityID, *recordEvents) {
	t.Helper()

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
		t.Fatalf("join %s: %v", name, res.err)
	}
	h.sinks[res.id] = sink
	h.tick(2)

	portal := h.room.mapDef.Portals[0]
	h.placeAt(res.id, portal.Bounds.CenterX().Int(), portal.Bounds.Bottom().Int())
	return res.id, events
}

// Dying on top of a portal must not send the body through it. A character who
// came back somewhere they never walked to would have no idea how they got
// there.
func TestADownedCharacterDoesNotTakeAPortal(t *testing.T) {
	h := newHarness(t)
	alice, events := standingInAPortal(t, h, "alice")

	// Standing in it alive takes it, which is what makes the rest meaningful.
	h.tick(2)
	if len(events.portals) == 0 {
		t.Fatal("a living character standing in a portal did not take it, so " +
			"this test could not tell a refusal from a portal that never fires")
	}

	// Released, moved off the portal, and left long enough for the portal's
	// own cooldown to lapse -- otherwise the cooldown would be what refused
	// the second attempt and this would prove nothing about being downed.
	h.room.abortTransfer(alice, "")
	h.placeAt(alice, 100, 288)
	h.tick(portalCooldownTicks + 2)

	portal := h.room.mapDef.Portals[0]
	h.placeAt(alice, portal.Bounds.CenterX().Int(), portal.Bounds.Bottom().Int())
	before := len(events.portals)

	h.down(alice)
	h.tick(10)

	if got := len(events.portals) - before; got != 0 {
		t.Errorf("a body was sent through a portal %d times", got)
	}
	if !isDowned(h.entity(alice)) {
		t.Fatal("the character revived mid-test, so this proved nothing")
	}
}

// A downed character is not a target. Mobs that kept swinging at a body would
// make dying next to a spawn point unrecoverable.
func TestMobsIgnoreADownedCharacter(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(alice)
	e := h.down(alice)

	// Well inside the revive clock, or the character comes back up and is
	// legitimately a target again.
	h.tick(10)

	if mob.Mob.Target == e.ID {
		t.Error("a mob is still hunting a body")
	}
	if isDowned(e) != true {
		t.Fatal("the character revived mid-test, so this proved nothing")
	}
}

// Input is dropped rather than queued. A character who came back and then
// walked a second's worth of held keys would arrive somewhere they never went.
func TestADownedCharacterDoesNotWalk(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.down(alice)
	at := e.Body.Pos

	for seq := uint32(1); seq <= 20; seq++ {
		h.input(alice, seq, sim.Input{MoveX: 1000})
	}
	h.tick(20)

	if !isDowned(e) {
		t.Fatal("the character revived mid-test, so this proved nothing")
	}
	if e.Body.Pos.X != at.X {
		t.Errorf("the body walked from %v to %v", at.X, e.Body.Pos.X)
	}

	// Dropped, but still acknowledged. The client reconciles by replaying
	// everything the server has not acknowledged, so a server that stopped
	// acking would leave it replaying a queue that only ever grew -- and the
	// body would stroll away from where it died, at full speed, on the
	// client alone. It looks exactly like the server moving a corpse.
	if got := h.room.players[alice].ackSeq; got != 20 {
		t.Errorf("acknowledged up to %d of 20 inputs; unacknowledged input is "+
			"what the client replays, and a dead character replaying a growing "+
			"queue walks away from their own body", got)
	}
	if queued := len(h.room.players[alice].queue); queued != 0 {
		t.Errorf("%d inputs left queued, which the character would replay on revive", queued)
	}
}

// The dead do not loot. The client still has the drop on screen and the key
// still works, so this is a real request to refuse.
func TestADownedCharacterCannotLoot(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(alice)
	h.killMob(alice, mob)

	drops := h.drops()
	if len(drops) == 0 {
		t.Skip("the kill dropped nothing to try to take")
	}

	h.down(alice)
	h.loot(alice, drops[0].ID)
	h.tick(2)

	if len(h.drops()) != len(drops) {
		t.Error("a downed character picked something up")
	}
}
