package room

import (
	"testing"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Boss encounters, in a running room.
//
// What these are testing is that the fight is a fight: that it changes shape as
// health falls, that an attack is announced before it lands, that the wind-up
// is long enough to move out of, and that the one ability meant to need a party
// actually needs one.

// summonBoss places a boss in the shared layer and returns it.
//
// Directly rather than through a spawn point, so a boss appears only in the
// tests that ask for one and every other room test keeps the population it was
// written against.
func (h *harness) summonBoss(x, y int) *Entity {
	h.t.Helper()

	def := h.game.Mobs["test_boss"]
	if def == nil {
		h.t.Fatal("no test_boss in the test content set")
	}

	at := sim.Vec{X: fixed.FromInt(x), Y: fixed.FromInt(y)}
	body := sim.NewBody(at, def.Width, def.Height)
	tuning := h.room.tuningFor(def)
	sim.Settle(&body, h.room.cfg.World, &tuning)

	return h.room.spawnEntity(&Entity{
		Kind:  KindMob,
		Layer: SharedLayer,
		Body:  body,
		HP:    uint32(def.HP),
		MaxHP: uint32(def.HP),
		Name:  def.Name,
		Mob:   &MobState{Def: def, Home: at, State: aiIdle},
	})
}

// telegraphs returns every marker on the ground.
func (h *harness) telegraphs() []*Entity {
	var out []*Entity
	for _, e := range h.room.entities {
		if e.Telegraph != nil {
			out = append(out, e)
		}
	}
	return out
}

// bossPhases returns the phase names announced so far.
func (s *recordSink) bossPhases() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string
	for _, m := range s.msgs {
		if b := m.GetEvent().GetBossPhase(); b != nil {
			out = append(out, b.Phase)
		}
	}
	return out
}

// --- phases -----------------------------------------------------------------

func TestBossEntersItsFirstPhaseImmediately(t *testing.T) {
	h := newHarness(t)
	_, sink := h.join("alice")
	h.tick(2)

	boss := h.summonBoss(300, 288)
	h.tick(1)

	if boss.Mob.Boss == nil {
		t.Fatal("a boss ticked without an encounter; the profile did nothing")
	}
	if got := boss.Mob.Boss.Phase; got != 0 {
		t.Errorf("started in phase %d, want 0", got)
	}
	if got := sink.bossPhases(); len(got) != 1 || got[0] != "opening" {
		t.Errorf("announced %v, want the opening phase once", got)
	}
}

// Phases are entered on health and never left. A boss that went back on being
// healed would be a boss whose fight resets on a mistake.
func TestBossAdvancesPhasesAsHealthFallsAndNeverReturns(t *testing.T) {
	h := newHarness(t)
	_, sink := h.join("alice")
	h.tick(2)

	boss := h.summonBoss(300, 288)
	h.tick(1)

	// 60% of 1000 is the second phase's threshold, and 25% the third's.
	boss.HP = 600
	h.tick(1)
	if got := boss.Mob.Boss.Phase; got != 1 {
		t.Fatalf("at 60%% health the boss is in phase %d, want 1", got)
	}

	boss.HP = 250
	h.tick(1)
	if got := boss.Mob.Boss.Phase; got != 2 {
		t.Fatalf("at 25%% health the boss is in phase %d, want 2", got)
	}

	boss.HP = boss.MaxHP
	h.tick(5)
	if got := boss.Mob.Boss.Phase; got != 2 {
		t.Errorf("healing the boss to full put it back in phase %d; phases only "+
			"ever advance, or a fight resets every time someone misplays", got)
	}

	if got := sink.bossPhases(); len(got) != 3 {
		t.Errorf("announced %v, want one line per phase entered", got)
	}
}

// A single hit that crosses two thresholds should land in the phase its health
// says, not one phase per tick: an encounter that walks down through phases it
// has already passed gives a party several free openings.
func TestBossSkipsPhasesItsHealthHasPassed(t *testing.T) {
	h := newHarness(t)
	_, sink := h.join("alice")
	h.tick(2)

	boss := h.summonBoss(300, 288)
	h.tick(1)

	boss.HP = 100
	h.tick(1)

	if got := boss.Mob.Boss.Phase; got != 2 {
		t.Errorf("a hit to 10%% health left the boss in phase %d, want 2", got)
	}
	if got := sink.bossPhases(); len(got) != 3 {
		t.Errorf("announced %v; every phase entered is announced, including "+
			"the ones passed through", got)
	}
}

// The phase's on_enter fires once, when the phase begins.
func TestBossCastsItsPhaseOpener(t *testing.T) {
	h := newHarness(t)
	h.join("alice")
	h.tick(2)

	boss := h.summonBoss(300, 288)
	h.tick(1)

	if _, held := boss.Buffs["test_hardened"]; held {
		t.Fatal("the second phase's opener fired during the first")
	}

	boss.HP = 600
	h.tick(1)

	if _, held := boss.Buffs["test_hardened"]; !held {
		t.Error("entering the phase did not cast its on_enter skill")
	}
}

// --- the enrage clock --------------------------------------------------------

func TestBossEnragesWhenItsPhaseClockRunsOut(t *testing.T) {
	h := newHarness(t)
	_, sink := h.join("alice")
	h.tick(2)

	boss := h.summonBoss(300, 288)
	h.tick(1)

	// Straight into the last phase, whose clock is one second.
	boss.HP = 200
	h.tick(1)
	if _, enraged := boss.Buffs["test_enrage"]; enraged {
		t.Fatal("enraged on entering the phase; the clock is the mechanic")
	}

	h.tick(TickRate + 2)

	if _, enraged := boss.Buffs["test_enrage"]; !enraged {
		t.Error("the clock ran out and nothing happened; a fight with no hard " +
			"timer can be won by outlasting it")
	}
	if got := sink.bossPhases(); len(got) == 0 || got[len(got)-1] != "enraged" {
		t.Errorf("announced %v, want the enrage announced last", got)
	}
}

// Earlier phases have no clock, and a phase without one must never enrage.
func TestBossWithoutAClockNeverEnrages(t *testing.T) {
	h := newHarness(t)
	h.join("alice")
	h.tick(2)

	boss := h.summonBoss(300, 288)
	h.tick(5 * TickRate)

	if boss.Mob.Boss.Enraged {
		t.Error("a phase with no enrage timer enraged anyway")
	}
	if len(boss.Buffs) != 0 {
		t.Errorf("holding %v; a phase with no enrage buff should apply none", boss.Buffs)
	}
}

// The clock is per phase, not per fight: a boss that carried an expired clock
// into its next phase would enrage the instant it got there.
func TestTheEnrageClockRestartsWithEachPhase(t *testing.T) {
	h := newHarness(t)
	h.join("alice")
	h.tick(2)

	boss := h.summonBoss(300, 288)
	h.tick(3 * TickRate)

	boss.HP = 200
	h.tick(1)

	if _, enraged := boss.Buffs["test_enrage"]; enraged {
		t.Error("three seconds spent in an earlier phase enraged the last one " +
			"on arrival; the clock belongs to the phase, not the fight")
	}
}

// A boss does not settle for standing near a target it cannot reach.
//
// Horizontal distance alone would have it plant itself a few units short of a
// player on a ledge and stop there, out of reach and out of ideas.
func TestABossKeepsClosingOnATargetItCannotReach(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	boss := h.summonBoss(300, 288)
	def := boss.Mob.Def

	// Inside the boss's horizontal attack range but outside the width it will
	// settle at, so a stop decided on horizontal distance and one decided on
	// reach put the boss in visibly different places. And far enough above
	// that nothing it has could possibly land.
	gap := (def.AI.AttackRange + def.Width) / 2
	h.placeAt(alice, 300, 288)
	h.entity(alice).Body.Pos.X = boss.Body.Pos.X + gap

	above := h.entity(alice).Body.Pos.Y - fixed.FromInt(200)
	before := boss.Body.Pos.X
	for i := 0; i < 40; i++ {
		h.entity(alice).Body.Pos.Y = above
		h.tick(1)
	}

	if boss.Body.Pos.X <= before {
		t.Errorf("the boss sat at %v and never moved from %v; a target it "+
			"cannot reach is one to walk towards, not one to stand near",
			boss.Body.Pos.X, before)
	}
	if got := horizontalGap(boss.Body.Pos, h.entity(alice).Body.Pos); got > def.Width {
		t.Errorf("the boss closed to %v of a target it cannot reach; it should "+
			"come right under them", got)
	}
}

// --- telegraphs --------------------------------------------------------------

func TestABossAnnouncesAnAttackBeforeItLands(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	h.placeAt(alice, 320, 288)
	h.summonBoss(300, 288)

	// Find the wind-up. It starts on the first tick the boss is in range with
	// its ability off cooldown.
	var marker *Entity
	for i := 0; i < 40 && marker == nil; i++ {
		h.tick(1)
		if got := h.telegraphs(); len(got) > 0 {
			marker = got[0]
		}
	}
	if marker == nil {
		t.Fatal("the boss never wound up; a telegraphed ability never fired")
	}

	before := h.entity(alice).HP
	if marker.HP != marker.MaxHP {
		t.Errorf("marker entered at %d of %d; the client is told the whole "+
			"wind-up once and animates it locally", marker.HP, marker.MaxHP)
	}

	// The wind-up is half a second, and nothing may land during it.
	h.tick(int(marker.MaxHP) - 1)
	if h.entity(alice).HP != before {
		t.Fatal("the attack landed during its own wind-up, which makes the " +
			"marker decorative")
	}

	h.tick(2)
	if h.entity(alice).HP >= before {
		t.Error("the wind-up finished and the attack never landed")
	}
	if got := h.telegraphs(); len(got) != 0 {
		t.Errorf("%d markers left after the attack landed", len(got))
	}
}

// The marker is the hitbox. If it were computed separately the two would drift
// apart the first time either was tuned.
func TestTheMarkerIsExactlyWhatTheAttackWillHit(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	h.placeAt(alice, 320, 288)
	boss := h.summonBoss(300, 288)

	var marker *Entity
	for i := 0; i < 40 && marker == nil; i++ {
		h.tick(1)
		if got := h.telegraphs(); len(got) > 0 {
			marker = got[0]
		}
	}
	if marker == nil {
		t.Fatal("no wind-up to inspect")
	}

	want := hitbox(boss, h.game.Skills["boss_swing"])
	if got := marker.Body.Bounds(); got != want {
		t.Errorf("marker covers %v, the swing covers %v", got, want)
	}
}

// Moving out of a marker is the counterplay. If the boss turned to follow, the
// marker would be a lie and the attack undodgeable.
func TestWalkingOutOfAMarkerAvoidsTheAttack(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	h.placeAt(alice, 340, 288)
	h.summonBoss(300, 288)

	var marker *Entity
	for i := 0; i < 40 && marker == nil; i++ {
		h.tick(1)
		if got := h.telegraphs(); len(got) > 0 {
			marker = got[0]
		}
	}
	if marker == nil {
		t.Fatal("no wind-up to dodge")
	}

	before := h.entity(alice).HP

	// Behind the boss, which the marker does not cover.
	h.placeAt(alice, 180, 288)
	h.tick(int(marker.MaxHP) + 2)

	if h.entity(alice).HP != before {
		t.Errorf("took %d damage after leaving the marked area; a boss that "+
			"turns during its wind-up makes the marker meaningless",
			before-h.entity(alice).HP)
	}
}

// A boss does not wind up an attack that cannot reach anyone.
//
// This is the difference between a boss and a machine: without it, one whose
// target is standing on a ledge above it roots itself in place and telegraphs
// forever at a player in no danger at all.
func TestABossDoesNotWindUpAnAttackThatCannotReach(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	// Within aggro range horizontally, and far out of reach vertically. Held
	// there each tick, because the test map is a flat floor with nothing to
	// stand on and gravity would otherwise drop the player back into range
	// before the boss had a chance to decide anything.
	h.placeAt(alice, 320, 288)
	boss := h.summonBoss(300, 288)

	above := h.entity(alice).Body.Pos.Y - fixed.FromInt(400)
	for i := 0; i < 40; i++ {
		h.entity(alice).Body.Pos.Y = above
		h.tick(1)

		// Checked every tick, not at the end. A wind-up is over in half a
		// second, so looking once afterwards would pass whether or not the
		// boss had spent the whole time swinging at nothing.
		if got := h.telegraphs(); len(got) != 0 {
			t.Fatalf("tick %d: wound up an attack at a player it cannot hit", i)
		}
		if boss.Mob.Boss.CastAt != 0 {
			t.Fatalf("tick %d: committed to a cast that could not land", i)
		}
	}

	// Back in reach, and it commits at once.
	h.placeAt(alice, 320, 288)
	for i := 0; i < 40 && len(h.telegraphs()) == 0; i++ {
		h.tick(1)
	}
	if len(h.telegraphs()) == 0 {
		t.Error("the boss never attacked a player standing right next to it; " +
			"the reach check refuses everything")
	}
}

// A boss killed mid-wind-up leaves a marker for an attack that is never coming.
func TestAMarkerDoesNotOutliveItsBoss(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	h.placeAt(alice, 320, 288)
	boss := h.summonBoss(300, 288)

	for i := 0; i < 40 && len(h.telegraphs()) == 0; i++ {
		h.tick(1)
	}
	if len(h.telegraphs()) == 0 {
		t.Fatal("no wind-up in flight")
	}

	h.room.damage(h.entity(alice), boss, int(boss.HP), false, "physical")
	h.tick(1)

	if got := h.telegraphs(); len(got) != 0 {
		t.Errorf("%d markers survived the boss that made them", len(got))
	}
}

// --- split damage ------------------------------------------------------------

func TestSplitDamageIsDividedAmongEveryoneItHits(t *testing.T) {
	names := []string{"alice", "bob", "carol", "dan"}

	// One player takes the whole hit; four take a quarter each. Run the same
	// cast at both sizes and compare, so the assertion is about the division
	// rather than about an exact number the balance file can change.
	solo := splitDamageTaken(t, names[:1])
	group := splitDamageTaken(t, names)

	if solo <= 0 || group <= 0 {
		t.Fatalf("nobody was hit: solo %d, group %d", solo, group)
	}
	if group*4 != solo {
		t.Errorf("one player took %d and each of four took %d; four shares "+
			"should add up to the solo hit", solo, group)
	}
}

// splitDamageTaken puts everyone in range of one split hit and returns what the
// first of them lost.
func splitDamageTaken(t *testing.T, names []string) uint32 {
	t.Helper()

	h := newHarness(t)

	var ids []EntityID
	for _, name := range names {
		id, _ := h.join(name)
		ids = append(ids, id)
	}
	h.tick(2)

	boss := h.summonBoss(300, 288)
	// Straight to the phase whose ability splits.
	boss.HP = 600
	h.tick(1)

	// Everyone stacked on the boss, and given enough health to survive the
	// whole hit so the measurement is a subtraction rather than a death.
	for _, id := range ids {
		e := h.entity(id)
		e.MaxHP, e.HP = 5000, 5000
		h.placeAt(id, 310, 288)
	}

	before := h.entity(ids[0]).HP
	for i := 0; i < 60 && h.entity(ids[0]).HP == before; i++ {
		h.tick(1)
	}
	return before - h.entity(ids[0]).HP
}

// The division happens per cast, so an ability with no split effect is left
// alone entirely -- including the slice, which every cast in the game passes
// through.
func TestSplittingLeavesOrdinaryEffectsAlone(t *testing.T) {
	effects := []content.Effect{
		{Kind: content.EffectDamage, BaseMin: 100, BaseMax: 100},
		{Kind: content.EffectKnockback, Speed: fixed.FromInt(4)},
	}

	got := splitAmongTargets(effects, 4)
	if &got[0] != &effects[0] {
		t.Error("a cast with nothing to split copied its effect list anyway")
	}
}

func TestSplittingRewritesOnlyTheSplitEffect(t *testing.T) {
	effects := []content.Effect{
		{Kind: content.EffectDamage, BaseMin: 100, BaseMax: 100},
		{Kind: content.EffectSplitDamage, BaseMin: 800, BaseMax: 800},
		{Kind: content.EffectKnockback, Speed: fixed.FromInt(4)},
	}

	got := splitAmongTargets(effects, 4)

	if len(got) != len(effects) {
		t.Fatalf("split produced %d effects from %d", len(got), len(effects))
	}
	if got[0].BaseMin != 100 || got[2].Speed != fixed.FromInt(4) {
		t.Error("splitting changed an effect that was not a split")
	}
	if got[1].Kind != content.EffectDamage {
		t.Errorf("the share is a %q; it should resolve as ordinary damage so it "+
			"is mitigated and rolled like any other hit", got[1].Kind)
	}
	if got[1].BaseMin != 200 || got[1].BaseMax != 200 {
		t.Errorf("share rolls %d-%d of 800 among four, want 200-200",
			got[1].BaseMin, got[1].BaseMax)
	}
	if effects[1].BaseMin != 800 {
		t.Error("splitting mutated the content it was given; the effect list " +
			"belongs to the loaded skill and is reused by every later cast")
	}
}

// One target takes the whole thing. That is the mechanic working: a hit meant
// for a party is supposed to be lethal to whoever takes it alone.
func TestASingleTargetTakesTheWholeSplit(t *testing.T) {
	effects := []content.Effect{{Kind: content.EffectSplitDamage, BaseMin: 800, BaseMax: 800}}

	got := splitAmongTargets(effects, 1)
	if got[0].BaseMin != 800 {
		t.Errorf("a lone target took %d of 800", got[0].BaseMin)
	}
}
