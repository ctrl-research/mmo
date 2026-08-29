package room

import (
	"io"
	"log/slog"
	"testing"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/content/contenttest"
	"github.com/ctrl-research/mmo/internal/fixed"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// harness drives a room synchronously, without its goroutine or its ticker.
//
// Simulation tests need to control exactly how many ticks pass. Driving the
// real Run loop means sleeping on wall-clock time, which makes tests slow, and
// -- worse -- makes a failure depend on scheduler timing rather than on the
// logic under test. Stepping the room directly is both faster and exact.
type harness struct {
	t     *testing.T
	room  *Room
	game  *content.Content
	sinks map[EntityID]*recordSink
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load test content: %v", err)
	}
	m := game.Maps["test"]

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
		Seed:       0xB0B,
	})

	return &harness{t: t, room: r, game: game, sinks: make(map[EntityID]*recordSink)}
}

func (h *harness) join(name string) (EntityID, *recordSink) {
	h.t.Helper()

	sink := newSink()
	result := make(chan joinResult, 1)
	h.room.handle(joinCmd{
		spec: JoinSpec{
			CharacterID: "char-" + name,
			Name:        name,
			Fresh:       true,
			Sink:        sink,
		},
		result: result,
	})

	res := <-result
	if res.err != nil {
		h.t.Fatalf("join %s: %v", name, res.err)
	}
	h.sinks[res.id] = sink
	return res.id, sink
}

func (h *harness) leave(id EntityID) { h.room.handle(leaveCmd{id: id}) }

func (h *harness) tick(n int) {
	for i := 0; i < n; i++ {
		h.room.doTick()
	}
}

func (h *harness) input(id EntityID, seq uint32, in sim.Input) {
	h.room.handle(inputCmd{id: id, seq: seq, in: in})
}

func (h *harness) cast(id EntityID, skill string, facingLeft bool) {
	h.room.handle(castCmd{id: id, req: castRequest{skillID: skill, facingLeft: facingLeft}})
}

func (h *harness) loot(id, target EntityID) {
	h.room.handle(interactCmd{id: id, target: target, kind: InteractLoot})
}

func (h *harness) entity(id EntityID) *Entity { return h.room.entity(id) }

// mobs returns every live mob, optionally restricted to one layer.
func (h *harness) mobs(layer LayerID, anyLayer bool) []*Entity {
	var out []*Entity
	for _, e := range h.room.entities {
		if e.Mob == nil || e.Mob.State == aiDead {
			continue
		}
		if !anyLayer && e.Layer != layer {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (h *harness) drops() []*Entity {
	var out []*Entity
	for _, e := range h.room.entities {
		if e.Drop != nil {
			out = append(out, e)
		}
	}
	return out
}

// placeAt teleports a player, so a test can set up a fight without simulating
// the walk to it.
func (h *harness) placeAt(id EntityID, x, y int) {
	e := h.entity(id)
	e.Body.SetFeetCenter(sim.Vec{X: fixed.FromInt(x), Y: fixed.FromInt(y)})
	sim.Settle(&e.Body, h.room.cfg.World, &h.room.cfg.Tuning)
}

// --- spawning and layering --------------------------------------------------

func TestMobsSpawnOnFirstTick(t *testing.T) {
	h := newHarness(t)
	h.join("alice")
	h.tick(1)

	if len(h.mobs(0, true)) == 0 {
		t.Fatal("no mobs spawned; a room should populate immediately rather than " +
			"stay empty for one respawn interval")
	}
}

// The heart of the layering model: two players share a room and see each
// other, but hunt entirely separate mobs.
func TestEachPlayerGetsTheirOwnMobs(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	b, _ := h.join("bob")
	h.tick(3)

	layerA := h.room.players[a].layer
	layerB := h.room.players[b].layer
	if layerA == layerB {
		t.Fatal("two unpartied players were given the same layer")
	}

	mobsA := h.mobs(layerA, false)
	mobsB := h.mobs(layerB, false)

	if len(mobsA) == 0 || len(mobsB) == 0 {
		t.Fatalf("expected mobs in both layers, got %d and %d", len(mobsA), len(mobsB))
	}
	if len(mobsA) != len(mobsB) {
		t.Errorf("layers hold %d and %d mobs; each should get its own full population",
			len(mobsA), len(mobsB))
	}

	// No entity may appear in both layers.
	seen := make(map[EntityID]bool)
	for _, e := range mobsA {
		seen[e.ID] = true
	}
	for _, e := range mobsB {
		if seen[e.ID] {
			t.Errorf("mob %d appears in both players' layers", e.ID)
		}
	}
}

// The counterweight to per-player mobs: a shared-layer spawn everyone fights.
func TestSharedLayerMobIsCommonToEveryone(t *testing.T) {
	h := newHarness(t)
	h.join("alice")
	h.join("bob")
	h.tick(3)

	shared := h.mobs(SharedLayer, false)
	if len(shared) != 1 {
		t.Fatalf("found %d shared-layer mobs, want exactly 1", len(shared))
	}
	if shared[0].Mob.Def.ID != "test_statue" {
		t.Errorf("shared mob is %q, want test_statue", shared[0].Mob.Def.ID)
	}
}

func TestSpawnRespectsMaxAlive(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(200)

	layer := h.room.players[a].layer
	// The test map caps its owner spawn at two.
	if n := len(h.mobs(layer, false)); n > 2 {
		t.Errorf("layer holds %d mobs, above the spawn point's cap of 2", n)
	}
}

// A layer with no players left must take its mobs with it, or a long-lived
// room accumulates populations nobody can see.
func TestLeavingTearsDownTheLayer(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(3)

	layer := h.room.players[a].layer
	if len(h.mobs(layer, false)) == 0 {
		t.Fatal("setup: expected mobs in the layer")
	}

	h.leave(a)
	h.tick(1)

	if n := len(h.mobs(layer, false)); n != 0 {
		t.Errorf("%d mobs survived their layer's last player leaving", n)
	}
	if _, ok := h.room.layers[layer]; ok {
		t.Error("the layer itself was not released")
	}
}

// --- combat -----------------------------------------------------------------

// findMobNear returns a mob in a layer, moved adjacent to the player so a
// swing is guaranteed to reach it.
func (h *harness) mobInRange(playerID EntityID) *Entity {
	h.t.Helper()

	p := h.entity(playerID)
	layer := h.room.players[playerID].layer

	mobs := h.mobs(layer, false)
	if len(mobs) == 0 {
		h.t.Fatal("no mob in the player's layer")
	}
	mob := mobs[0]

	// Place the mob just in front of the player and face them at it.
	feet := p.Body.FeetCenter()
	mob.Body.SetFeetCenter(sim.Vec{X: feet.X + fixed.FromInt(30), Y: feet.Y})
	sim.Settle(&mob.Body, h.room.cfg.World, &h.room.cfg.Tuning)
	p.Body.FacingLeft = false
	return mob
}

func TestAttackDamagesMob(t *testing.T) {
	h := newHarness(t)
	a, sink := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	before := mob.HP

	h.cast(a, "slash", false)
	h.tick(1)

	if mob.HP >= before {
		t.Fatalf("mob HP unchanged: %d then %d", before, mob.HP)
	}

	// Damage is an event, not something a client infers from a falling HP
	// number: two hits of 100 and one of 200 look identical in state.
	var dealt *mmov1.DamageDealt
	for _, ev := range sink.events() {
		if d := ev.GetDamage(); d != nil {
			dealt = d
		}
	}
	if dealt == nil {
		t.Fatal("no DamageDealt event was sent")
	}
	if dealt.GetTargetId() != uint32(mob.ID) {
		t.Errorf("damage targeted %d, want %d", dealt.GetTargetId(), mob.ID)
	}
	if got, want := dealt.GetAmount(), before-mob.HP; got != want {
		t.Errorf("event reported %d damage but HP fell by %d", got, want)
	}
}

func TestAttackMissesOutOfRange(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	// Well beyond the skill's 72-unit reach.
	mob.Body.SetFeetCenter(sim.Vec{
		X: h.entity(a).Body.FeetCenter().X + fixed.FromInt(400),
		Y: h.entity(a).Body.FeetCenter().Y,
	})
	before := mob.HP

	h.cast(a, "slash", false)
	h.tick(1)

	if mob.HP != before {
		t.Errorf("a mob 400 units away took damage from a 72-unit swing")
	}
}

func TestAttackMissesBehind(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a) // placed to the player's right
	before := mob.HP

	// Swing left, away from the mob.
	h.cast(a, "slash", true)
	h.tick(1)

	if mob.HP != before {
		t.Error("swinging away from a mob still hit it")
	}
}

// A ground-level swing must not reach something standing on the platform
// above, or melee feels like it has a mind of its own.
func TestAttackRespectsVerticalBound(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	feet := h.entity(a).Body.FeetCenter()
	mob.Body.SetFeetCenter(sim.Vec{X: feet.X + fixed.FromInt(30), Y: feet.Y - fixed.FromInt(200)})
	before := mob.HP

	h.cast(a, "slash", false)
	h.tick(1)

	if mob.HP != before {
		t.Error("a swing reached a mob 200 units overhead")
	}
}

// Cross-layer damage is not merely forbidden, it is unreachable: the target
// resolution never considers another layer's entities.
func TestCannotDamageAnotherPlayersMob(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	b, _ := h.join("bob")
	h.tick(3)

	layerB := h.room.players[b].layer
	mobsB := h.mobs(layerB, false)
	if len(mobsB) == 0 {
		t.Fatal("setup: bob has no mobs")
	}
	victim := mobsB[0]

	// Put alice right on top of bob's mob and swing.
	feet := victim.Body.FeetCenter()
	h.placeAt(a, feet.X.Int()-20, feet.Y.Int())
	h.entity(a).Body.FacingLeft = false

	before := victim.HP
	h.cast(a, "slash", false)
	h.tick(1)

	if victim.HP != before {
		t.Error("alice damaged a mob in bob's layer")
	}
}

func TestCooldownPreventsSpam(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	before := mob.HP

	// The test skill has a 500ms cooldown, which is 10 ticks.
	h.cast(a, "slash", false)
	h.tick(1)
	afterFirst := mob.HP

	h.cast(a, "slash", false)
	h.tick(1)

	if mob.HP != afterFirst {
		t.Error("a second cast landed while the skill was on cooldown")
	}
	if afterFirst >= before {
		t.Fatal("setup: the first cast did not land")
	}

	// After the cooldown elapses it should work again.
	h.tick(12)
	h.cast(a, "slash", false)
	h.tick(1)

	if mob.HP >= afterFirst {
		t.Error("the skill did not become usable again after its cooldown")
	}
}

// A client may only cast what it has been granted, or it could swing with a
// mob's ability.
func TestCannotCastAnArbitrarySkill(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	before := mob.HP

	h.cast(a, "mob_bite", false)
	h.cast(a, "no_such_skill", false)
	h.tick(1)

	if mob.HP != before {
		t.Error("a player cast a skill they were never granted")
	}
}

// --- death, rewards, drops --------------------------------------------------

// killMob swings until the target dies, and reports how many casts it took.
func (h *harness) killMob(playerID EntityID, mob *Entity) {
	h.t.Helper()
	for i := 0; i < 200 && isAlive(mob); i++ {
		h.cast(playerID, "slash", false)
		h.tick(1)
		// Keep the mob adjacent: this test is about death and rewards, not
		// about chasing.
		feet := h.entity(playerID).Body.FeetCenter()
		mob.Body.SetFeetCenter(sim.Vec{X: feet.X + fixed.FromInt(30), Y: feet.Y})
		h.entity(playerID).Body.FacingLeft = false
	}
	if isAlive(mob) {
		h.t.Fatal("mob refused to die within 200 casts")
	}
}

func TestKillAwardsExperience(t *testing.T) {
	h := newHarness(t)
	a, sink := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	h.killMob(a, mob)
	h.tick(1)

	var gained *mmov1.ExpGained
	for _, ev := range sink.events() {
		if g := ev.GetExpGained(); g != nil {
			gained = g
		}
	}
	if gained == nil {
		t.Fatal("no ExpGained event after a kill")
	}
	if gained.GetAmount() != uint64(mob.Mob.Def.Exp) {
		t.Errorf("awarded %d exp, want %d", gained.GetAmount(), mob.Mob.Def.Exp)
	}
}

func TestExperienceLevelsTheCharacter(t *testing.T) {
	h := newHarness(t)
	a, sink := h.join("alice")
	h.tick(2)

	player := h.entity(a)
	startLevel := player.Player.Level

	// The dummy grants 50 exp and level 1 needs 16, so one kill is several
	// levels -- which is exactly the case a single-level implementation would
	// get wrong by silently discarding the remainder.
	mob := h.mobInRange(a)
	h.killMob(a, mob)
	h.tick(1)

	if player.Player.Level <= startLevel {
		t.Fatalf("level unchanged at %d after gaining %d exp", player.Player.Level, mob.Mob.Def.Exp)
	}

	levels := 0
	for _, ev := range sink.events() {
		if ev.GetLevelUp() != nil {
			levels++
		}
	}
	if levels != player.Player.Level-startLevel {
		t.Errorf("sent %d LevelUp events for %d levels gained",
			levels, player.Player.Level-startLevel)
	}

	// Levelling restores the character, which is what makes a level-up
	// mid-fight feel like a reward rather than a statistic.
	if player.HP != player.MaxHP {
		t.Errorf("HP is %d/%d after levelling; it should be restored",
			player.HP, player.MaxHP)
	}
	if player.MaxHP != MaxHPFor(player.Player.Level) {
		t.Errorf("max HP is %d, want %d for level %d",
			player.MaxHP, MaxHPFor(player.Player.Level), player.Player.Level)
	}
}

func TestKillProducesDropsInTheKillersLayer(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	layer := mob.Layer
	h.killMob(a, mob)
	h.tick(1)

	drops := h.drops()
	if len(drops) == 0 {
		t.Fatal("a kill produced no drops, though the test table always drops")
	}
	for _, d := range drops {
		if d.Layer != layer {
			t.Errorf("drop %d is in layer %d, want the mob's layer %d", d.ID, d.Layer, layer)
		}
	}
}

func TestLootPickup(t *testing.T) {
	h := newHarness(t)
	a, sink := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	h.killMob(a, mob)
	h.tick(1)

	drops := h.drops()
	if len(drops) == 0 {
		t.Fatal("no drops to loot")
	}

	// Stand on the loot.
	gold := drops[0]
	feet := gold.Body.FeetCenter()
	h.placeAt(a, feet.X.Int(), feet.Y.Int())

	before := h.entity(a).Player.Gold
	h.loot(a, gold.ID)
	h.tick(1)

	if h.entity(a).Drop != nil {
		t.Fatal("player entity was corrupted")
	}
	if h.room.entity(gold.ID) != nil {
		t.Error("the drop still exists after being looted")
	}

	var taken *mmov1.LootTaken
	for _, ev := range sink.events() {
		if l := ev.GetLootTaken(); l != nil {
			taken = l
		}
	}
	if taken == nil {
		t.Fatal("no LootTaken event")
	}
	if taken.GetGold() > 0 && h.entity(a).Player.Gold <= before {
		t.Error("gold was looted but the player's balance did not rise")
	}
}

func TestCannotLootOutOfRange(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	h.killMob(a, mob)
	h.tick(1)

	drops := h.drops()
	if len(drops) == 0 {
		t.Fatal("no drops")
	}
	target := drops[0]

	// Stand far away.
	h.placeAt(a, target.Body.FeetCenter().X.Int()+600, 288)
	h.loot(a, target.ID)
	h.tick(1)

	if h.room.entity(target.ID) == nil {
		t.Error("a drop 600 units away was looted")
	}
}

func TestCannotLootAnotherPlayersDrop(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	b, _ := h.join("bob")
	h.tick(3)

	mob := h.mobInRange(a)
	h.killMob(a, mob)
	h.tick(1)

	drops := h.drops()
	if len(drops) == 0 {
		t.Fatal("no drops")
	}
	target := drops[0]

	// Put bob directly on alice's loot. Layer visibility is the loot rule --
	// bob was never sent this entity, so the request is refused the same way a
	// forged one would be.
	feet := target.Body.FeetCenter()
	h.placeAt(b, feet.X.Int(), feet.Y.Int())
	h.loot(b, target.ID)
	h.tick(1)

	if h.room.entity(target.ID) == nil {
		t.Error("bob looted a drop from alice's layer")
	}
}

func TestDeadMobLeavesACorpseThenDisappears(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	h.killMob(a, mob)

	if h.room.entity(mob.ID) == nil {
		t.Fatal("the mob vanished immediately, leaving no time for a death animation")
	}
	if mob.Mob.State != aiDead {
		t.Errorf("state is %v after death, want dead", mob.Mob.State)
	}

	// corpse_ms is 500 in the test content, which is 10 ticks.
	h.tick(15)
	if h.room.entity(mob.ID) != nil {
		t.Error("the corpse was never cleared")
	}
}

// The spawn slot must free on death, not on corpse removal, or every respawn
// in the game is silently extended by the corpse duration.
func TestSpawnSlotFreesOnDeathNotOnCorpseRemoval(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	spawn := mob.Mob.Spawn
	aliveBefore := spawn.alive

	h.killMob(a, mob)

	if spawn.alive != aliveBefore-1 {
		t.Errorf("spawn holds %d alive after a death, want %d", spawn.alive, aliveBefore-1)
	}
}

func TestMobRespawnsAfterItsTimer(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	layer := h.room.players[a].layer
	mob := h.mobInRange(a)
	h.killMob(a, mob)

	countAfterKill := len(h.mobs(layer, false))

	// respawn_ms is 1000 in the test content, which is 20 ticks.
	h.tick(40)

	if len(h.mobs(layer, false)) <= countAfterKill {
		t.Errorf("population is %d after waiting out the respawn timer, was %d at the kill",
			len(h.mobs(layer, false)), countAfterKill)
	}
}

// --- AI ---------------------------------------------------------------------

func TestMobAggrosAndChases(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mobs := h.mobs(h.room.players[a].layer, false)
	if len(mobs) == 0 {
		t.Fatal("no mobs")
	}
	mob := mobs[0]

	// Stand well within aggro range but outside attack range.
	feet := mob.Body.FeetCenter()
	h.placeAt(a, feet.X.Int()-120, feet.Y.Int())

	startGap := horizontalGap(mob.Body.Pos, h.entity(a).Body.Pos)
	h.tick(20)

	if mob.Mob.State == aiIdle {
		t.Error("mob stayed idle with a player well inside aggro range")
	}
	if horizontalGap(mob.Body.Pos, h.entity(a).Body.Pos) >= startGap {
		t.Error("mob aggroed but never closed the distance")
	}
}

func TestMobIgnoresPlayersInOtherLayers(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	b, _ := h.join("bob")
	h.tick(3)

	mobsA := h.mobs(h.room.players[a].layer, false)
	if len(mobsA) == 0 {
		t.Fatal("no mobs in alice's layer")
	}
	mob := mobsA[0]

	// Move alice far away and stand bob right next to alice's mob.
	h.placeAt(a, 1200, 288)
	feet := mob.Body.FeetCenter()
	h.placeAt(b, feet.X.Int()-30, feet.Y.Int())

	h.tick(20)

	if mob.Mob.Target == b {
		t.Error("a mob targeted a player who cannot even see it")
	}
	if mob.Mob.State != aiIdle && mob.Mob.Target == 0 {
		t.Error("mob left idle without a target")
	}
}

func TestMobAttacksAndDealsDamage(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mobs := h.mobs(h.room.players[a].layer, false)
	mob := mobs[0]

	player := h.entity(a)
	feet := mob.Body.FeetCenter()
	h.placeAt(a, feet.X.Int()-20, feet.Y.Int())
	before := player.HP

	h.tick(40)

	if player.HP >= before {
		t.Errorf("player HP is %d after 40 ticks adjacent to a hostile mob, was %d",
			player.HP, before)
	}
}

func TestMobLeashesHomeWhenPulledTooFar(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mobs := h.mobs(h.room.players[a].layer, false)
	mob := mobs[0]
	home := mob.Mob.Home

	// Aggro it, then run beyond the leash range.
	feet := mob.Body.FeetCenter()
	h.placeAt(a, feet.X.Int()-100, feet.Y.Int())
	h.tick(10)

	h.placeAt(a, home.X.Int()+900, 288)
	h.tick(60)

	if mob.Mob.State == aiChase {
		t.Error("mob still chasing a player far beyond its leash range")
	}
	// It should be heading home, or already back and healed.
	if mob.Mob.State == aiLeash || mob.Mob.State == aiIdle {
		return
	}
	t.Errorf("mob is in state %v, want leash or idle", mob.Mob.State)
}

// Healing on leash is deliberate: without it a player can whittle a mob down
// across several pulls, turning every fight into attrition the mob cannot win.
func TestLeashingRestoresMobHealth(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	h.cast(a, "slash", false)
	h.tick(1)

	if mob.HP == mob.MaxHP {
		t.Fatal("setup: the mob took no damage")
	}

	// Force it home.
	mob.Mob.State = aiLeash
	mob.Body.SetFeetCenter(mob.Mob.Home)
	h.placeAt(a, 1250, 288)
	h.tick(5)

	if mob.HP != mob.MaxHP {
		t.Errorf("mob HP is %d/%d after leashing home, want full", mob.HP, mob.MaxHP)
	}
}

func TestPassiveMobNeverAttacks(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(3)

	statues := h.mobs(SharedLayer, false)
	if len(statues) == 0 {
		t.Fatal("no shared-layer mob")
	}
	statue := statues[0]

	player := h.entity(a)
	feet := statue.Body.FeetCenter()
	h.placeAt(a, feet.X.Int()-20, feet.Y.Int())
	before := player.HP

	h.tick(60)

	if player.HP != before {
		t.Errorf("a passive mob dealt damage: HP went from %d to %d", before, player.HP)
	}
	if statue.Mob.State != aiIdle {
		t.Errorf("passive mob entered state %v, want idle", statue.Mob.State)
	}
}

// Being hit pulls a mob's attention, so walking up and swinging starts a fight
// rather than leaving an indifferent target.
func TestHittingAnIdleMobAggrosIt(t *testing.T) {
	h := newHarness(t)
	a, _ := h.join("alice")
	h.tick(2)

	mob := h.mobInRange(a)
	mob.Mob.State = aiIdle
	mob.Mob.Target = 0

	h.cast(a, "slash", false)
	h.tick(1)

	if mob.Mob.Target != a {
		t.Errorf("mob target is %d after being hit, want the attacker %d", mob.Mob.Target, a)
	}
}

// --- determinism ------------------------------------------------------------

// The property replay rests on: the same seed and the same inputs produce the
// same world, tick for tick.
func TestRoomIsDeterministic(t *testing.T) {
	run := func() []string {
		h := newHarness(t)
		a, _ := h.join("alice")

		for tick := 0; tick < 120; tick++ {
			// A scripted, varied input sequence so the run is not trivially
			// identical.
			in := sim.Input{MoveX: 1000}
			if tick%7 == 0 {
				in = sim.Input{MoveX: -1000, Jump: true}
			}
			h.input(a, uint32(tick+1), in)
			if tick%10 == 0 {
				h.cast(a, "slash", tick%20 == 0)
			}
			h.tick(1)
		}

		var out []string
		for _, e := range h.room.entities {
			out = append(out, describeEntity(e))
		}
		return out
	}

	first := run()
	for i := 0; i < 5; i++ {
		got := run()
		if len(got) != len(first) {
			t.Fatalf("run %d produced %d entities, want %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d diverged at entity %d:\n got  %s\n want %s",
					i, j, got[j], first[j])
			}
		}
	}
}

func describeEntity(e *Entity) string {
	s := string(rune('A'+int(e.Kind))) +
		"|" + itoa(int(e.ID)) +
		"|" + itoa(int(e.Layer)) +
		"|" + itoa(int(e.Body.Pos.X)) + "," + itoa(int(e.Body.Pos.Y)) +
		"|hp" + itoa(int(e.HP))
	if e.Mob != nil {
		s += "|" + e.Mob.State.String() + "|t" + itoa(int(e.Mob.Target))
	}
	if e.Drop != nil {
		s += "|" + e.Drop.ItemID + "x" + itoa(int(e.Drop.Qty)) + "g" + itoa(int(e.Drop.Gold))
	}
	if e.Player != nil {
		s += "|lv" + itoa(e.Player.Level) + "|xp" + itoa(int(e.Player.Exp))
	}
	return s
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Layers draw from independent streams, so one player's luck never depends on
// how many others happen to share the room.
func TestLayerStreamsAreIndependent(t *testing.T) {
	solo := newHarness(t)
	a1, _ := solo.join("alice")
	solo.tick(3)
	soloLayer := solo.room.players[a1].layer

	crowded := newHarness(t)
	a2, _ := crowded.join("alice")
	for i := 0; i < 4; i++ {
		crowded.join("other")
	}
	crowded.tick(3)
	crowdedLayer := crowded.room.players[a2].layer

	// Alice is the first player in both rooms, so her layer key and therefore
	// her stream is the same regardless of who else is present.
	if soloLayer != crowdedLayer {
		t.Fatalf("alice got layer %d alone and %d in a crowd", soloLayer, crowdedLayer)
	}

	soloMobs := solo.mobs(soloLayer, false)
	crowdedMobs := crowded.mobs(crowdedLayer, false)
	if len(soloMobs) != len(crowdedMobs) {
		t.Errorf("alice has %d mobs alone and %d in a crowd; her layer should be unaffected",
			len(soloMobs), len(crowdedMobs))
	}
	for i := range soloMobs {
		if soloMobs[i].Body.Pos != crowdedMobs[i].Body.Pos {
			t.Errorf("mob %d spawned at %v alone and %v in a crowd",
				i, soloMobs[i].Body.Pos, crowdedMobs[i].Body.Pos)
		}
	}
}

// Content and the room must agree on the tick rate, or every authored duration
// in the game is silently wrong by the ratio between them.
func TestContentTickRateMatchesRoom(t *testing.T) {
	if content.TickRate != TickRate {
		t.Fatalf("content.TickRate is %d but room.TickRate is %d; every authored "+
			"cooldown and respawn timer would be scaled wrong",
			content.TickRate, TickRate)
	}
}
