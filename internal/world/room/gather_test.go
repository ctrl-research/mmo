package room

import (
	"io"
	"log/slog"
	"testing"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/content/contenttest"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Gathering.
//
// The rules these protect: an action resolves on the 600 ms beat and not on the
// 50 ms one; it stops the moment anything interrupts it; a node runs out and
// comes back; and every refusal says why. That last one is not politeness --
// "nothing happened" is the single most confusing thing a gathering skill can
// do, and it is the failure a player cannot debug.
//
// The fixture's nodes yield on every action tick, so these assert a yield
// rather than a probability. The probability itself is arithmetic and is tested
// as arithmetic, in TestGatherChanceRises* -- a loop that samples an RNG is a
// test that fails in CI once a month for no reason.

// newCopse returns a harness on the gathering map.
//
// Its own map, for the reason GladeTMJ exists: borrowing corners of the main
// test map was tried during the zone-events work and broke a neighbouring suite
// every time.
func newCopse(t *testing.T) *harness {
	t.Helper()

	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load test content: %v", err)
	}
	m := game.Maps["copse"]

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
		Seed:       0xC0F5E,
	})

	return &harness{t: t, room: r, game: game, sinks: make(map[EntityID]*recordSink)}
}

// nodeNamed returns the entity standing at a named placement.
func (h *harness) nodeNamed(name string) *Entity {
	h.t.Helper()
	for _, e := range h.room.entities {
		if e.Resource != nil && e.Resource.State.spot.Name == name {
			return e
		}
	}
	h.t.Fatalf("no resource node named %q is in the room", name)
	return nil
}

// nodeState returns the clocks behind a named placement, whether or not the
// node itself is currently standing.
func (h *harness) nodeState(name string) *resourceState {
	h.t.Helper()
	for _, state := range h.room.sharedResources {
		if state.spot.Name == name {
			return state
		}
	}
	for _, l := range h.room.layers {
		for _, state := range l.resources {
			if state.spot.Name == name {
				return state
			}
		}
	}
	h.t.Fatalf("the room has no resource placement named %q", name)
	return nil
}

// alignToActionTick advances to the beat, so a test's arithmetic is about the
// action tick rather than about where in the beat the join happened to land.
func (h *harness) alignToActionTick() {
	for h.room.tick%content.ActionTicks != 0 {
		h.tick(1)
	}
}

// gather asks to work a node.
func (h *harness) gather(id, node EntityID) {
	h.room.handle(interactCmd{id: id, target: node, kind: InteractGather})
}

// stopGathering asks to stop.
func (h *harness) stopGathering(id EntityID) {
	h.room.handle(interactCmd{id: id, target: 0, kind: InteractStop})
}

// giveTool pushes a tool in the way the session does, through the stat block.
func (h *harness) giveTool(id EntityID, skill string, power int) {
	h.t.Helper()
	h.room.handle(setStatsCmd{id: id, derived: Derived{
		ToolPower: map[string]int{skill: power},
	}})
}

// standAt puts a player at a node and gives them the tool for it.
func (h *harness) atNode(name string, tool int) (EntityID, *recordSink, *recordEvents, *Entity) {
	h.t.Helper()

	id, events := h.joinWithEvents("alice")
	sink := h.sinks[id]
	h.tick(1)

	node := h.nodeNamed(name)
	h.placeAt(id, node.Body.FeetCenter().X.Int(), node.Body.FeetCenter().Y.Int())
	if tool > 0 {
		h.giveTool(id, node.Resource.Skill, tool)
	}
	return id, sink, events, node
}

// gatherings returns every Gathering message this sink received.
func (s *recordSink) gatherings() []*mmov1.Gathering {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.Gathering
	for _, m := range s.msgs {
		if g := m.GetEvent().GetGathering(); g != nil {
			out = append(out, g)
		}
	}
	return out
}

// secondaryExp returns every SecondaryExp message this sink received.
func (s *recordSink) secondaryExp() []*mmov1.SecondaryExp {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.SecondaryExp
	for _, m := range s.msgs {
		if x := m.GetEvent().GetSecondaryExp(); x != nil {
			out = append(out, x)
		}
	}
	return out
}

// lastRefusal returns the reason on the most recent inactive Gathering.
func lastRefusal(msgs []*mmov1.Gathering) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !msgs[i].GetActive() {
			return msgs[i].GetReason()
		}
	}
	return ""
}

// --- the action tick --------------------------------------------------------

// The whole reason for a derived beat. A node that yielded on the simulation
// tick would produce twelve times as much, and the level requirements, the
// experience rates and the respawn timers would all be tuned against a number
// nobody meant.
func TestGatheringResolvesOnTheActionTickAndNotTheSimulationTick(t *testing.T) {
	h := newCopse(t)
	id, _, events, node := h.atNode("near-tree", 1)
	h.alignToActionTick()

	h.gather(id, node.ID)

	// Almost a full action tick: eleven simulation ticks must produce nothing.
	h.tick(content.ActionTicks - 1)
	if got := len(events.yields); got != 0 {
		t.Fatalf("got %d yields after %d simulation ticks, want 0; "+
			"gathering is resolving on the wrong clock",
			got, content.ActionTicks-1)
	}

	h.tick(1)
	if got := len(events.yields); got != 1 {
		t.Fatalf("got %d yields on the action tick, want 1", got)
	}
}

// One yield per action tick, not one per tick the key is held. Holding is how
// the client sends this, so a repeat has to be free.
func TestHoldingTheKeyDoesNotGatherFaster(t *testing.T) {
	h := newCopse(t)
	id, _, events, node := h.atNode("bush", 0)
	h.alignToActionTick()

	// Re-asked every single tick, which is what a held key looks like.
	for i := 0; i < content.ActionTicks*2; i++ {
		h.gather(id, node.ID)
		h.tick(1)
	}

	if got := len(events.yields); got != 2 {
		t.Fatalf("got %d yields over two action ticks of held input, want 2", got)
	}

	// And it announces itself once, not once per tick. The yield count alone
	// cannot see this -- the beat belongs to the room, so restarting the action
	// every tick would produce exactly the same two yields and a flood of
	// events behind them.
	var started int
	for _, g := range h.sinks[id].gatherings() {
		if g.GetActive() {
			started++
		}
	}
	if started != 1 {
		t.Errorf("holding the key announced the action %d times, want 1", started)
	}
}

// The beat belongs to the room, not to the player who started an action.
//
// That is what "derived from the simulation tick" means concretely, and it is
// the OSRS behaviour: two players who started half a beat apart still resolve
// together. Per-player timers would drift, and a party gathering would slowly
// desynchronise for no reason anyone could see.
func TestEveryoneGathersOnTheSameBeat(t *testing.T) {
	h := newCopse(t)
	alice, aliceEvents := h.joinWithEvents("alice")
	h.tick(1)

	bush := h.nodeNamed("bush")
	h.placeAt(alice, bush.Body.FeetCenter().X.Int(), bush.Body.FeetCenter().Y.Int())
	h.alignToActionTick()
	h.gather(alice, bush.ID)

	// Bob joins and starts partway through the beat. One tick for his layer's
	// own copy of the bush to appear -- an owner-layer node does not exist
	// until there is somebody to own it.
	h.tick(content.ActionTicks/2 - 1)
	bob, bobEvents := h.joinWithEvents("bob")
	h.tick(1)

	var bobBush *Entity
	for _, e := range h.room.entities {
		if e.Resource != nil && e.Resource.State.spot.Name == "bush" &&
			e.Layer == h.room.players[bob].layer {
			bobBush = e
		}
	}
	if bobBush == nil {
		t.Fatal("bob has no bush")
	}
	h.placeAt(bob, bobBush.Body.FeetCenter().X.Int(), bobBush.Body.FeetCenter().Y.Int())
	h.gather(bob, bobBush.ID)

	// To the next beat, and no further.
	for h.room.tick%content.ActionTicks != 0 {
		h.tick(1)
	}

	if len(aliceEvents.yields) != 1 || len(bobEvents.yields) != 1 {
		t.Errorf("alice got %d yields and bob %d on the shared beat, want one each",
			len(aliceEvents.yields), len(bobEvents.yields))
	}
}

// --- what a yield produces --------------------------------------------------

func TestAYieldGrantsAnItemAndExperience(t *testing.T) {
	h := newCopse(t)
	id, sink, events, node := h.atNode("near-tree", 1)

	h.gather(id, node.ID)
	h.tick(content.ActionTicks)

	if len(events.yields) != 1 {
		t.Fatalf("got %d yields, want 1", len(events.yields))
	}
	y := events.yields[0]
	if y.ItemID != "test.log" || y.Qty != 1 {
		t.Errorf("yield = %s x%d, want test.log x1", y.ItemID, y.Qty)
	}
	if y.Skill != "chopping" {
		t.Errorf("yield skill = %q, want chopping", y.Skill)
	}
	if y.CharacterID != "char-alice" {
		t.Errorf("yield character = %q, want char-alice; the session cannot store it otherwise", y.CharacterID)
	}

	gains := sink.secondaryExp()
	if len(gains) != 1 {
		t.Fatalf("got %d experience events, want 1", len(gains))
	}
	if gains[0].GetGained() != 100 || gains[0].GetTotal() != 100 {
		t.Errorf("gain = %d, total %d; want 100 and 100",
			gains[0].GetGained(), gains[0].GetTotal())
	}
	if got := h.entity(id).Player.Secondary["chopping"]; got != 100 {
		t.Errorf("the room holds %d chopping experience, want 100", got)
	}
}

// Experience accumulates and the level follows it. Cumulative rather than
// spent, which is what makes a secondary level unable to go backwards.
func TestSecondaryExperienceAccumulatesAndLevels(t *testing.T) {
	h := newCopse(t)
	id, sink, _, node := h.atNode("bush", 0)

	// Level 2 on the OSRS curve is 83 experience, so one 100-point yield
	// crosses it and the second does not reach level 3 (174).
	h.gather(id, node.ID)
	h.tick(content.ActionTicks)

	gains := sink.secondaryExp()
	if len(gains) != 1 {
		t.Fatalf("got %d gains, want 1", len(gains))
	}
	if gains[0].GetLevel() != 2 || !gains[0].GetLevelUp() {
		t.Errorf("first yield reached level %d (level_up=%v), want 2 and true",
			gains[0].GetLevel(), gains[0].GetLevelUp())
	}
	if gains[0].GetNextAt() != 174 {
		t.Errorf("next_at = %d, want 174 (level 3 on the OSRS curve)", gains[0].GetNextAt())
	}
	if gains[0].GetLevelAt() != 83 {
		t.Errorf("level_at = %d, want 83; without it the client cannot draw progress "+
			"*through* a level, only progress from zero", gains[0].GetLevelAt())
	}

	h.gather(id, node.ID)
	h.tick(content.ActionTicks)

	gains = sink.secondaryExp()
	if len(gains) != 2 {
		t.Fatalf("got %d gains, want 2", len(gains))
	}
	if gains[1].GetTotal() != 200 {
		t.Errorf("total = %d, want 200", gains[1].GetTotal())
	}
	if gains[1].GetLevel() != 3 {
		t.Errorf("level = %d, want 3", gains[1].GetLevel())
	}
}

// --- depletion and respawn --------------------------------------------------

// A node runs out. Without this a player stands at one tree forever, which is
// the failure mode OSRS avoids by making trees fall over.
func TestANodeRunsOutAfterItsYields(t *testing.T) {
	h := newCopse(t)
	id, _, events, node := h.atNode("near-tree", 1)
	nodeID := node.ID

	// The fixture tree yields twice.
	for i := 0; i < 3; i++ {
		h.gather(id, nodeID)
		h.tick(content.ActionTicks)
	}

	if got := len(events.yields); got != 2 {
		t.Fatalf("got %d yields from a two-yield node, want 2", got)
	}
	if h.room.entity(nodeID) != nil {
		t.Error("the node is still standing after it was used up")
	}
}

// And comes back. A node gone for good the first time anyone used it would make
// a map worse every time it was played.
func TestASpentNodeComesBack(t *testing.T) {
	h := newCopse(t)
	id, _, _, node := h.atNode("near-tree", 1)
	nodeID := node.ID

	for i := 0; i < 2; i++ {
		h.gather(id, nodeID)
		h.tick(content.ActionTicks)
	}
	if h.room.entity(nodeID) != nil {
		t.Fatal("the node did not deplete")
	}

	state := h.nodeState("near-tree")
	h.runUntil("the node to come back", func() bool { return state.entity != 0 })

	if got := state.remaining; got != state.def.Yields {
		t.Errorf("the node came back with %d yields, want %d; a node that returned "+
			"part-used would be worth less every time it respawned",
			got, state.def.Yields)
	}
}

// Depleting it stops the player, and not with an error. They succeeded.
func TestUsingUpANodeIsNotReportedAsAFailure(t *testing.T) {
	h := newCopse(t)
	id, sink, _, node := h.atNode("near-tree", 1)

	for i := 0; i < 2; i++ {
		h.gather(id, node.ID)
		h.tick(content.ActionTicks)
	}

	if got := lastRefusal(sink.gatherings()); got != "" {
		t.Errorf("using up a node reported %q; finishing a tree is a success", got)
	}
	if h.room.players[id].gather.node != 0 {
		t.Error("the player is still gathering a node that no longer exists")
	}
}

// --- interruptions ----------------------------------------------------------

// Walking away stops it, on the simulation tick rather than the action tick: a
// player who leaves should stop at once, not up to 600 ms later.
func TestWalkingAwayStopsGathering(t *testing.T) {
	h := newCopse(t)
	id, _, events, node := h.atNode("near-tree", 1)

	h.gather(id, node.ID)
	h.tick(2)

	// To the next tree along, which is far out of range of this one.
	far := h.nodeNamed("hardwood")
	h.placeAt(id, far.Body.FeetCenter().X.Int(), far.Body.FeetCenter().Y.Int())
	h.tick(1)

	if h.room.players[id].gather.node != 0 {
		t.Fatal("still gathering after walking away")
	}

	h.tick(content.ActionTicks * 2)
	if got := len(events.yields); got != 0 {
		t.Errorf("got %d yields after walking away, want 0", got)
	}
}

// Being hit stops it. Otherwise the safest place in the game to fight from
// would be behind a tree.
func TestTakingDamageStopsGathering(t *testing.T) {
	h := newCopse(t)
	id, sink, events, node := h.atNode("near-tree", 1)

	h.gather(id, node.ID)
	h.tick(2)

	// What a hit leaves behind, which is what the interruption actually reads.
	h.entity(id).Player.InCombatUntil = h.room.tick + uint64(TickRate)
	h.tick(1)

	if h.room.players[id].gather.node != 0 {
		t.Fatal("still gathering while under attack")
	}
	if got := lastRefusal(sink.gatherings()); got != "you are under attack" {
		t.Errorf("reason = %q, want %q", got, "you are under attack")
	}

	h.tick(content.ActionTicks * 2)
	if got := len(events.yields); got != 0 {
		t.Errorf("got %d yields while under attack, want 0", got)
	}
}

// Dying stops it. A corpse does not chop.
//
// The downed state is set directly rather than by taking a killing blow, and
// that is the point of the test rather than a shortcut. A killing blow is
// damage, so a real death always arrives alongside being in combat -- and a
// test that kills the character passes even with the downed check deleted,
// because the combat check catches it first. Isolating the state is the only
// way to assert the guard exists.
func TestADownedCharacterDoesNotGather(t *testing.T) {
	h := newCopse(t)
	id, _, events, node := h.atNode("near-tree", 1)

	h.gather(id, node.ID)
	h.tick(2)

	h.entity(id).Player.ReviveAt = h.room.tick + uint64(TickRate)
	h.tick(1)

	if h.room.players[id].gather.node != 0 {
		t.Fatal("a downed character is still gathering")
	}

	h.tick(content.ActionTicks * 2)
	if got := len(events.yields); got != 0 {
		t.Errorf("got %d yields while downed, want 0", got)
	}
}

// And a downed character cannot start one either. Their client still has the
// tree on screen and the key still works, so this is a real request to refuse.
func TestADownedCharacterCannotStartGathering(t *testing.T) {
	h := newCopse(t)
	id, sink, events, node := h.atNode("near-tree", 1)

	// Downed but not in combat, for the reason given above.
	h.entity(id).Player.ReviveAt = h.room.tick + uint64(TickRate)
	h.gather(id, node.ID)
	h.tick(content.ActionTicks * 2)

	if got := len(events.yields); got != 0 {
		t.Errorf("got %d yields, want 0", got)
	}

	// And it never even started. The yield count alone cannot see this -- the
	// interrupt would catch it on the next tick either way -- but a corpse
	// being told "you begin chopping" is a corpse being told it is chopping.
	for _, g := range sink.gatherings() {
		if g.GetActive() {
			t.Fatalf("a downed character was told they began gathering %q", g.GetSkill())
		}
	}
}

// A frozen player stops too. Their connection has dropped, so a yield would be
// an item written for a session that is not there to be told about it -- and a
// character left chopping through a network blip would come back to a full bag
// and no idea where it came from.
func TestAFrozenPlayerStopsGathering(t *testing.T) {
	h := newCopse(t)
	id, _, events, node := h.atNode("bush", 0)
	h.alignToActionTick()

	h.gather(id, node.ID)
	h.tick(2)

	h.room.handle(freezeCmd{id: id})
	h.tick(content.ActionTicks * 2)

	if got := len(events.yields); got != 0 {
		t.Errorf("got %d yields while frozen, want 0", got)
	}
	if h.room.players[id].gather.node != 0 {
		t.Error("a frozen player is still gathering")
	}
}

// A death by damage stops it too. The one above isolates the downed check; this
// one is the route a player actually takes, and asserts the two together do not
// leave a gap -- a corpse chopping for one tick before the interrupt catches up
// would be a corpse chopping.
func TestBeingKilledStopsGathering(t *testing.T) {
	h := newCopse(t)
	id, _, events, node := h.atNode("near-tree", 1)
	h.alignToActionTick()

	h.gather(id, node.ID)
	h.down(id)

	h.tick(content.ActionTicks * 2)
	if got := len(events.yields); got != 0 {
		t.Errorf("got %d yields after being killed, want 0", got)
	}
}

// Losing the tool mid-swing stops it, because the alternative is finishing the
// tree with your hands.
func TestUnequippingTheToolStopsGathering(t *testing.T) {
	h := newCopse(t)
	id, sink, events, node := h.atNode("near-tree", 1)

	h.gather(id, node.ID)
	h.tick(2)

	// An empty push is what the session sends after an unequip.
	h.room.handle(setStatsCmd{id: id, derived: Derived{}})
	h.tick(1)

	if h.room.players[id].gather.node != 0 {
		t.Fatal("still gathering with no tool in hand")
	}
	// "hatchet" rather than "axe": the fixture's tool name differs from its
	// class on purpose, so a refusal that printed the content class id -- which
	// is what "fishing_rod" looked like on screen -- fails here.
	if got := lastRefusal(sink.gatherings()); got != "you need a hatchet in hand" {
		t.Errorf("reason = %q, want %q", got, "you need a hatchet in hand")
	}

	h.tick(content.ActionTicks * 2)
	if got := len(events.yields); got != 0 {
		t.Errorf("got %d yields with no tool, want 0", got)
	}
}

// Asking to stop stops it, silently: the player knows, they asked.
func TestAskingToStopStopsGathering(t *testing.T) {
	h := newCopse(t)
	id, sink, events, node := h.atNode("bush", 0)

	h.gather(id, node.ID)
	h.tick(2)
	h.stopGathering(id)
	h.tick(content.ActionTicks * 2)

	if got := len(events.yields); got != 0 {
		t.Errorf("got %d yields after stopping, want 0", got)
	}
	if got := lastRefusal(sink.gatherings()); got != "" {
		t.Errorf("stopping on request reported %q; the player asked for it", got)
	}
}

// --- the gates --------------------------------------------------------------

// No tool, no chopping -- and it says so, because a player with a sword out
// standing at a tree has no other way to learn what is wrong.
func TestGatheringNeedsTheRightToolAndSaysSo(t *testing.T) {
	h := newCopse(t)
	id, sink, events, node := h.atNode("near-tree", 0)

	h.gather(id, node.ID)
	h.tick(content.ActionTicks * 2)

	if got := len(events.yields); got != 0 {
		t.Fatalf("got %d yields with no axe, want 0", got)
	}
	if got := lastRefusal(sink.gatherings()); got != "you need a hatchet in hand" {
		t.Errorf("reason = %q, want %q", got, "you need a hatchet in hand")
	}
}

// A skill with no tool class needs nothing in hand. Otherwise every herb in the
// game would require a licence.
func TestASkillWithNoToolNeedsNothingInHand(t *testing.T) {
	h := newCopse(t)
	id, _, events, node := h.atNode("bush", 0)

	h.gather(id, node.ID)
	h.tick(content.ActionTicks)

	if got := len(events.yields); got != 1 {
		t.Errorf("got %d yields from a bare-handed skill, want 1", got)
	}
}

// A tool that is too weak is refused, which is what makes the next one up worth
// buying rather than merely faster.
func TestAToolTooWeakForANodeIsRefused(t *testing.T) {
	h := newCopse(t)
	id, sink, events, node := h.atNode("hardwood", 1)

	h.gather(id, node.ID)
	h.tick(content.ActionTicks * 2)

	if got := len(events.yields); got != 0 {
		t.Fatalf("got %d yields with too weak an axe, want 0", got)
	}
	if got := lastRefusal(sink.gatherings()); got != "your hatchet is not good enough for that" {
		t.Errorf("reason = %q, want it to name the axe", got)
	}

	// The better one works, at the same node, with nothing else changed.
	h.giveTool(id, "chopping", 5)
	h.gather(id, node.ID)
	h.tick(content.ActionTicks)

	if got := len(events.yields); got != 1 {
		t.Errorf("got %d yields with a good enough axe, want 1", got)
	}
}

// A level requirement is refused by name, so "you need Chopping level 30" is
// something a player can act on.
func TestANodeAboveYourLevelIsRefusedByName(t *testing.T) {
	h := newCopse(t)
	id, sink, events, node := h.atNode("ancient", 1)

	h.gather(id, node.ID)
	h.tick(content.ActionTicks * 2)

	if got := len(events.yields); got != 0 {
		t.Fatalf("got %d yields below the level requirement, want 0", got)
	}
	if got := lastRefusal(sink.gatherings()); got != "you need Chopping level 30" {
		t.Errorf("reason = %q, want %q", got, "you need Chopping level 30")
	}
}

// Standing away from a node and asking anyway is refused, and told. A client
// can send any entity id, so range is checked here rather than trusted.
func TestGatheringOutOfRangeIsRefused(t *testing.T) {
	h := newCopse(t)
	id, sink, events, node := h.atNode("near-tree", 1)

	h.placeAt(id, 1200, 288)
	h.gather(id, node.ID)
	h.tick(content.ActionTicks * 2)

	if got := len(events.yields); got != 0 {
		t.Fatalf("got %d yields from across the map, want 0", got)
	}
	if got := lastRefusal(sink.gatherings()); got != "you are too far away" {
		t.Errorf("reason = %q, want %q", got, "you are too far away")
	}
}

// --- layering ---------------------------------------------------------------

// A per-player node is per player. This is the whole reason resource nodes are
// entities in a layer rather than a fixture on the map: two people chopping
// "the oak by the west platform" must not be chopping the same tree.
func TestAnOwnerLayerNodeIsOnePerPlayer(t *testing.T) {
	h := newCopse(t)
	alice, _ := h.joinWithEvents("alice")
	bob, _ := h.joinWithEvents("bob")
	h.tick(1)

	var mine, theirs int
	for _, e := range h.room.entities {
		if e.Resource == nil || e.Resource.State.spot.Name != "near-tree" {
			continue
		}
		switch e.Layer {
		case h.room.players[alice].layer:
			mine++
		case h.room.players[bob].layer:
			theirs++
		}
	}
	if mine != 1 || theirs != 1 {
		t.Fatalf("alice sees %d copies of the tree and bob %d, want one each", mine, theirs)
	}
}

// And one player cannot work another's. Layer visibility is the whole rule --
// the node was never sent to this client, so the request is stale or forged.
func TestAPlayerCannotGatherSomebodyElsesNode(t *testing.T) {
	h := newCopse(t)
	alice, _ := h.joinWithEvents("alice")
	bob, bobEvents := h.joinWithEvents("bob")
	h.tick(1)

	var aliceTree *Entity
	for _, e := range h.room.entities {
		if e.Resource != nil && e.Resource.State.spot.Name == "near-tree" &&
			e.Layer == h.room.players[alice].layer {
			aliceTree = e
		}
	}
	if aliceTree == nil {
		t.Fatal("alice has no tree")
	}

	// Bob stands on it and asks for it by id anyway.
	h.placeAt(bob, aliceTree.Body.FeetCenter().X.Int(), aliceTree.Body.FeetCenter().Y.Int())
	h.giveTool(bob, "chopping", 1)
	h.gather(bob, aliceTree.ID)
	h.tick(content.ActionTicks * 2)

	if got := len(bobEvents.yields); got != 0 {
		t.Errorf("bob got %d yields from alice's tree, want 0", got)
	}
}

// A shared node exists once for the whole room, which is how a contested
// resource is expressible at all.
func TestASharedNodeExistsOnceForTheRoom(t *testing.T) {
	h := newCopse(t)
	h.joinWithEvents("alice")
	h.joinWithEvents("bob")
	h.tick(1)

	n := 0
	for _, e := range h.room.entities {
		if e.Resource != nil && e.Resource.State.spot.Name == "commons" {
			n++
			if e.Layer != SharedLayer {
				t.Errorf("the shared node is in layer %d, want the shared layer", e.Layer)
			}
		}
	}
	if n != 1 {
		t.Errorf("the room holds %d copies of the shared node, want 1", n)
	}
}

// --- the client's view ------------------------------------------------------

// A node goes over the wire with what it needs, so the client can label it and
// grey out one the character is not good enough for without holding a copy of
// the content files.
func TestANodeTellsTheClientWhatItNeeds(t *testing.T) {
	h := newCopse(t)
	h.join("alice")
	h.tick(1)

	node := h.nodeNamed("ancient")
	state := node.state(false)

	if state.GetKind() != mmov1.EntityKind_ENTITY_KIND_RESOURCE {
		t.Errorf("kind = %v, want a resource", state.GetKind())
	}
	if state.GetNodeSkill() != "chopping" {
		t.Errorf("node_skill = %q, want chopping", state.GetNodeSkill())
	}
	if state.GetNodeLevel() != 30 {
		t.Errorf("node_level = %d, want 30", state.GetNodeLevel())
	}
	if state.GetName() != "Test Ancient Tree" {
		t.Errorf("name = %q, want the node's name", state.GetName())
	}
}

// --- reconnecting -----------------------------------------------------------

// A reconnecting player is not mid-swing: their client has no idea it was
// gathering, so an action left running would tick away invisibly.
func TestReconnectingDoesNotResumeGathering(t *testing.T) {
	h := newCopse(t)
	id, _, events, node := h.atNode("bush", 0)

	h.gather(id, node.ID)
	h.tick(2)

	h.room.handle(freezeCmd{id: id})
	h.room.handle(attachCmd{
		id:     id,
		attach: Attachment{Sink: newSink(), Events: events},
		result: make(chan bool, 1),
	})
	h.tick(content.ActionTicks * 2)

	if got := len(events.yields); got != 0 {
		t.Errorf("got %d yields after reconnecting, want 0", got)
	}
}

// The room's copy of the experience is the newer one, so a session pushing what
// it last read must not overwrite what has been gathered since.
func TestAttachingDoesNotRollBackExperience(t *testing.T) {
	h := newCopse(t)
	id, _, events, node := h.atNode("bush", 0)

	h.gather(id, node.ID)
	h.tick(content.ActionTicks)
	if got := h.entity(id).Player.Secondary["picking"]; got != 100 {
		t.Fatalf("the room holds %d picking experience, want 100", got)
	}

	// The session's copy is from before that yield, which is exactly what it is
	// between checkpoints.
	h.room.handle(freezeCmd{id: id})
	h.room.handle(attachCmd{
		id: id,
		attach: Attachment{
			Sink:      newSink(),
			Events:    events,
			Secondary: SecondaryProgress{"picking": 0},
		},
		result: make(chan bool, 1),
	})

	if got := h.entity(id).Player.Secondary["picking"]; got != 100 {
		t.Errorf("attaching rolled picking back to %d, want 100 kept", got)
	}
}

// --- the chance curve -------------------------------------------------------

// Tested as arithmetic rather than by sampling. A test that rolls an RNULL a
// thousand times and asserts a rate is a test that fails in CI eventually.
func TestGatherChanceRisesWithLevelAndTool(t *testing.T) {
	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load test content: %v", err)
	}

	node := &content.ResourceNode{
		Level:        1,
		ChancePPM:    100_000, // 10%
		MaxChancePPM: 500_000, // 50%
		MinToolPower: 1,
	}

	atOne := game.GatherChancePPM(node, 1, 1)
	if atOne != 100_000 {
		t.Errorf("at the node's own level the chance is %d, want the authored 100000", atOne)
	}

	atMax := game.GatherChancePPM(node, game.Curves.MaxSkillLevel, 1)
	if atMax != 500_000 {
		t.Errorf("at the maximum level the chance is %d, want the authored 500000", atMax)
	}

	mid := game.GatherChancePPM(node, (1+game.Curves.MaxSkillLevel)/2, 1)
	if mid <= atOne || mid >= atMax {
		t.Errorf("halfway up the chance is %d, want it between %d and %d",
			mid, atOne, atMax)
	}

	// Tool power above the requirement adds a percentage point each, so a
	// better axe is a speed-up and not only a key.
	better := game.GatherChancePPM(node, 1, 6)
	if better != atOne+5*10_000 {
		t.Errorf("five points of spare tool power gave %d, want %d",
			better, atOne+5*10_000)
	}

	// Below the requirement nothing is possible, whatever the tool.
	if got := game.GatherChancePPM(&content.ResourceNode{
		Level:     30,
		ChancePPM: 500_000,
	}, 10, 99); got != 0 {
		t.Errorf("below the level requirement the chance is %d, want 0", got)
	}
}

// A maxed skill has no next level, and says so with a zero rather than by
// repeating its own total -- which would make the client's progress bar a
// division by zero at the one point every player eventually reaches.
func TestAMaxedSkillHasNoNextLevel(t *testing.T) {
	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load test content: %v", err)
	}
	max := game.Curves.MaxSkillLevel

	if got := game.Curves.SecondaryNextAt(max); got != 0 {
		t.Errorf("SecondaryNextAt(%d) = %d, want 0", max, got)
	}
	if got := game.Curves.SecondaryNextAt(max - 1); got != game.Curves.SecondaryExpAt(max) {
		t.Errorf("SecondaryNextAt(%d) = %d, want the maximum level's own total %d",
			max-1, got, game.Curves.SecondaryExpAt(max))
	}
}

// A chance a designer wrote above certainty is certainty, not an out-of-range
// roll.
func TestGatherChanceIsClampedAtCertainty(t *testing.T) {
	game, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load test content: %v", err)
	}

	got := game.GatherChancePPM(&content.ResourceNode{
		Level:        1,
		ChancePPM:    900_000,
		MaxChancePPM: 900_000,
		MinToolPower: 1,
	}, 1, 99)
	if got != 1_000_000 {
		t.Errorf("chance = %d, want it clamped to 1000000", got)
	}
}

// --- the tick contract ------------------------------------------------------

// The action tick has to divide the simulation tick, or "every twelfth tick"
// is not 600 ms and the whole reason for deriving it rather than running a
// second loop is gone.
func TestTheActionTickIsSixHundredMilliseconds(t *testing.T) {
	if content.TickRate != TickRate {
		t.Fatalf("content says %d Hz and the room says %d", content.TickRate, TickRate)
	}
	if got := content.ActionTicks * int(TickPeriod.Milliseconds()); got != 600 {
		t.Errorf("%d action ticks of %v is %d ms, want 600",
			content.ActionTicks, TickPeriod, got)
	}
}
