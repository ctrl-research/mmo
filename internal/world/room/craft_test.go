package room

import (
	"testing"

	"github.com/ctrl-research/mmo/internal/content"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// Crafting.
//
// The rules these protect are mostly about the two-step: the room asks, the
// session answers, and nothing is granted in between. A room that paid a smith
// on asking would pay for bars they never made, and a room that asked twice
// while one answer was in flight would spend the same materials twice.
//
// The other half is that a station is somewhere you *stand*: walking away ends
// a run, and being hit deliberately does not.

// stationNamed returns the station entity placed under a given name.
func (h *harness) stationNamed(name string) *Entity {
	h.t.Helper()
	for _, e := range h.room.entities {
		if e.Station != nil && e.Name == h.stationDisplayName(name) {
			return e
		}
	}
	h.t.Fatalf("no station %q is in the room", name)
	return nil
}

func (h *harness) stationDisplayName(id string) string {
	h.t.Helper()
	def, ok := h.game.Stations[id]
	if !ok {
		h.t.Fatalf("the fixture has no station %q", id)
	}
	return def.Name
}

// craft asks to make something at a station.
func (h *harness) craft(id, station EntityID, recipe string) {
	h.room.handle(craftCmd{id: id, station: station, recipe: recipe})
}

// answer delivers the session's reply to the run in flight.
func (h *harness) answer(id EntityID, made bool, reason string) {
	h.room.handle(resolveCraftCmd{player: id, made: made, reason: reason})
}

// atStation joins a player and stands them at a station.
func (h *harness) atStation(id string) (EntityID, *recordSink, *recordEvents, *Entity) {
	h.t.Helper()

	player, events := h.joinWithEvents("alice")
	sink := h.sinks[player]
	h.tick(1)

	station := h.stationNamed(id)
	at := station.Body.Bounds()
	h.placeAt(player, at.CenterX().Int(), at.Bottom().Int())
	return player, sink, events, station
}

// craftings returns every Crafting message this sink received.
func (s *recordSink) craftings() []*mmov1.Crafting {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*mmov1.Crafting
	for _, m := range s.msgs {
		if c := m.GetEvent().GetCrafting(); c != nil {
			out = append(out, c)
		}
	}
	return out
}

// lastCraftReason returns the reason on the most recent inactive Crafting.
func lastCraftReason(msgs []*mmov1.Crafting) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !msgs[i].GetActive() {
			return msgs[i].GetReason()
		}
	}
	return ""
}

// produced counts the runs the room reported as completed.
func produced(msgs []*mmov1.Crafting) int {
	n := 0
	for _, m := range msgs {
		if m.GetProduced() {
			n++
		}
	}
	return n
}

// --- the two-step -----------------------------------------------------------

// The room asks; it does not decide. It cannot see the inventory, so a run
// reaches the session as a request and nothing is granted until the answer.
func TestARunAsksTheSessionAndGrantsNothingYet(t *testing.T) {
	h := newCopse(t)
	player, sink, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	h.tick(content.ActionTicks)

	if len(events.crafts) != 1 {
		t.Fatalf("the session received %d run requests, want 1", len(events.crafts))
	}
	if got := events.crafts[0].Recipe.ID; got != "test_plank" {
		t.Errorf("the request names recipe %q, want test_plank", got)
	}

	// Nothing granted: the materials may not have been there.
	if got := h.entity(player).Player.Secondary["forging"]; got != 0 {
		t.Errorf("the room granted %d forging experience before hearing back, want 0", got)
	}
	if got := produced(sink.craftings()); got != 0 {
		t.Errorf("the room reported %d completed runs before hearing back, want 0", got)
	}
}

// And grants it when the answer comes back.
func TestAnAnsweredRunGrantsExperience(t *testing.T) {
	h := newCopse(t)
	player, sink, _, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	h.tick(content.ActionTicks)
	h.answer(player, true, "")
	h.tick(1)

	if got := h.entity(player).Player.Secondary["forging"]; got != 100 {
		t.Errorf("forging experience = %d, want 100", got)
	}
	if got := produced(sink.craftings()); got != 1 {
		t.Errorf("the room reported %d completed runs, want 1", got)
	}
}

// A second run must not be asked for while the first is unanswered. Both would
// ask for the same materials, and a database that had already spent them once
// would have to say yes twice or no once.
func TestOnlyOneRunIsInFlightAtATime(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	// Several beats without answering.
	h.tick(content.ActionTicks * 4)

	if len(events.crafts) != 1 {
		t.Errorf("the session received %d requests with none answered, want 1", len(events.crafts))
	}
}

// Running out is the ordinary end of a run, and it says so rather than looking
// like something went wrong.
func TestRunningOutOfMaterialsEndsTheRun(t *testing.T) {
	h := newCopse(t)
	player, sink, _, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	h.tick(content.ActionTicks)
	h.answer(player, false, "you have run out of materials")
	h.tick(1)

	if h.room.players[player].craft.station != 0 {
		t.Fatal("still crafting after running out")
	}
	if got := lastCraftReason(sink.craftings()); got != "you have run out of materials" {
		t.Errorf("reason = %q, want it to say they ran out", got)
	}
	if got := h.entity(player).Player.Secondary["forging"]; got != 0 {
		t.Errorf("a failed run granted %d experience, want 0", got)
	}
}

// A completed run rolls into the next one without another key press. A run is a
// commitment several seconds long; the decision was "make these", not "make
// one".
func TestACompletedRunStartsTheNext(t *testing.T) {
	h := newCopse(t)
	player, sink, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	for i := 0; i < 3; i++ {
		h.tick(content.ActionTicks)
		h.answer(player, true, "")
		h.tick(1)
	}

	if len(events.crafts) != 3 {
		t.Errorf("the session received %d requests over three answered beats, want 3", len(events.crafts))
	}
	if got := produced(sink.craftings()); got != 3 {
		t.Errorf("the room reported %d completed runs, want 3", got)
	}
	if got := h.entity(player).Player.Secondary["forging"]; got != 300 {
		t.Errorf("forging experience = %d, want 300", got)
	}
}

// And the *next* run takes as long as the first.
//
// A one-beat recipe cannot see this: the clock has already run long enough for
// the next run whatever happens to the start time. With a three-beat recipe,
// forgetting to restart the clock makes every run after the first instant.
func TestTheSecondRunTakesAsLongAsTheFirst(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_slow")
	h.tick(content.ActionTicks * 3)
	if len(events.crafts) != 1 {
		t.Fatalf("the first run took %d requests to ask, want 1", len(events.crafts))
	}
	h.answer(player, true, "")
	h.tick(1)

	// Two of the second run's three beats.
	h.tick(content.ActionTicks * 2)
	if len(events.crafts) != 1 {
		t.Fatalf("the second run asked after two of its three beats (%d requests); "+
			"the clock did not restart", len(events.crafts))
	}

	h.tick(content.ActionTicks)
	if len(events.crafts) != 2 {
		t.Errorf("the second run made %d requests after three beats, want 2 in total",
			len(events.crafts))
	}
}

// --- timing -----------------------------------------------------------------

// A longer recipe takes longer. Without this a three-beat recipe is a one-beat
// recipe with a bigger number written on it.
func TestALongerRecipeTakesLonger(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_slow")

	// Two of its three beats.
	h.tick(content.ActionTicks * 2)
	if len(events.crafts) != 0 {
		t.Fatalf("a three-beat recipe asked after two beats (%d requests)", len(events.crafts))
	}

	h.tick(content.ActionTicks)
	if len(events.crafts) != 1 {
		t.Errorf("a three-beat recipe made %d requests after three beats, want 1", len(events.crafts))
	}
}

// Crafting shares gathering's beat. Two clocks would drift, and the action tick
// is a division of the simulation tick precisely so there is only one.
func TestCraftingRunsOnTheActionTick(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")

	h.tick(content.ActionTicks - 1)
	if len(events.crafts) != 0 {
		t.Fatalf("crafting resolved after %d simulation ticks; it is on the wrong clock",
			content.ActionTicks-1)
	}
	h.tick(1)
	if len(events.crafts) != 1 {
		t.Errorf("crafting made %d requests on the beat, want 1", len(events.crafts))
	}
}

// --- interruptions ----------------------------------------------------------

// Walking away ends it: that is the player deciding.
func TestWalkingAwayStopsCrafting(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	h.tick(2)

	h.placeAt(player, 900, 288)
	h.tick(1)

	if h.room.players[player].craft.station != 0 {
		t.Fatal("still crafting after walking away")
	}
	h.tick(content.ActionTicks * 2)
	if len(events.crafts) != 0 {
		t.Errorf("%d runs were asked for after walking away, want 0", len(events.crafts))
	}
}

// Being hit does *not*. A station is somewhere a player has chosen to stand
// still, usually in a camp, and a mob wandering past should not cost them the
// bar. Deliberately different from gathering, which a hit does interrupt.
func TestTakingDamageDoesNotStopCrafting(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	h.entity(player).Player.InCombatUntil = h.room.tick + uint64(TickRate)
	h.tick(content.ActionTicks)

	if len(events.crafts) != 1 {
		t.Errorf("being hit stopped a craft (%d requests, want 1); a station is not a fight",
			len(events.crafts))
	}
}

// Dying does.
func TestADownedCharacterDoesNotCraft(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	// Downed without the in-combat flag, because being hit deliberately does
	// not stop a craft and would prove nothing here.
	h.entity(player).Player.ReviveAt = h.room.tick + uint64(TickRate)
	h.tick(1)

	if h.room.players[player].craft.station != 0 {
		t.Fatal("a downed character is still crafting")
	}
	h.tick(content.ActionTicks * 2)
	if len(events.crafts) != 0 {
		t.Errorf("%d runs were asked for while downed, want 0", len(events.crafts))
	}
}

// And cannot start one either.
//
// The request count alone cannot see this -- the interrupt catches it next tick
// either way -- but a corpse being told "you begin smithing" is a corpse being
// told it is smithing.
func TestADownedCharacterCannotStartCrafting(t *testing.T) {
	h := newCopse(t)
	player, sink, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.entity(player).Player.ReviveAt = h.room.tick + uint64(TickRate)
	h.craft(player, station.ID, "test_plank")
	h.tick(content.ActionTicks * 2)

	if len(events.crafts) != 0 {
		t.Errorf("%d runs were asked for, want 0", len(events.crafts))
	}
	for _, c := range sink.craftings() {
		if c.GetActive() {
			t.Fatalf("a downed character was told they began making %q", c.GetName())
		}
	}
}

// A frozen player stops: their connection is gone, and a craft left running
// would keep spending materials nobody is watching.
func TestAFrozenPlayerStopsCrafting(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	h.room.handle(freezeCmd{id: player})
	h.tick(content.ActionTicks * 2)

	if len(events.crafts) != 0 {
		t.Errorf("%d runs were asked for while frozen, want 0", len(events.crafts))
	}
}

// Asking to stop stops it, silently.
func TestAskingToStopStopsCrafting(t *testing.T) {
	h := newCopse(t)
	player, sink, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	h.craft(player, station.ID, "")
	h.tick(content.ActionTicks * 2)

	if len(events.crafts) != 0 {
		t.Errorf("%d runs were asked for after stopping, want 0", len(events.crafts))
	}
	if got := lastCraftReason(sink.craftings()); got != "" {
		t.Errorf("stopping on request reported %q; the player asked for it", got)
	}
}

// --- one action at a time ---------------------------------------------------

// Gathering and crafting share the "what is this character doing" slot. Two at
// once would be two runs against one bag, and the bag would lose.
func TestStartingACraftStopsGathering(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")

	// A tree within reach of the forge, so both are legitimately startable.
	node := h.nodeNamed("near-tree")
	at := station.Body.Bounds()
	h.placeAt(player, at.CenterX().Int(), at.Bottom().Int())
	h.giveTool(player, "chopping", 1)
	h.gather(player, node.ID)

	h.craft(player, station.ID, "test_plank")
	if h.room.players[player].gather.node != 0 {
		t.Error("starting a craft left the gather running")
	}

	h.alignToActionTick()
	h.tick(content.ActionTicks)
	if len(events.yields) != 0 {
		t.Errorf("%d gather yields landed after starting a craft, want 0", len(events.yields))
	}
}

func TestStartingAGatherStopsCrafting(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")

	h.craft(player, station.ID, "test_plank")

	node := h.nodeNamed("near-tree")
	h.placeAt(player, node.Body.FeetCenter().X.Int(), node.Body.FeetCenter().Y.Int())
	h.giveTool(player, "chopping", 1)
	h.gather(player, node.ID)

	if h.room.players[player].craft.station != 0 {
		t.Error("starting a gather left the craft running")
	}
	h.alignToActionTick()
	h.tick(content.ActionTicks)
	if len(events.crafts) != 0 {
		t.Errorf("%d runs were asked for after starting a gather, want 0", len(events.crafts))
	}
}

// --- the gates --------------------------------------------------------------

// A recipe above the character's level is refused by name, so it is something
// to work towards rather than a mystery.
func TestARecipeAboveYourLevelIsRefused(t *testing.T) {
	h := newCopse(t)
	player, sink, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_masterwork")
	h.tick(content.ActionTicks * 2)

	if len(events.crafts) != 0 {
		t.Fatalf("%d runs were asked for below the level requirement, want 0", len(events.crafts))
	}
	if got := lastCraftReason(sink.craftings()); got != "you need Forging level 30" {
		t.Errorf("reason = %q, want %q", got, "you need Forging level 30")
	}
}

// A recipe asked for at the wrong station is refused, and says which station it
// needs. A client can send any pair, so this is a real request to refuse.
func TestARecipeAtTheWrongStationIsRefused(t *testing.T) {
	h := newCopse(t)
	player, sink, events, station := h.atStation("forge")
	h.alignToActionTick()

	// test_stool is made at the bench.
	h.craft(player, station.ID, "test_stool")
	h.tick(content.ActionTicks * 2)

	if len(events.crafts) != 0 {
		t.Fatalf("%d runs were asked for at the wrong station, want 0", len(events.crafts))
	}
	if got := lastCraftReason(sink.craftings()); got != "that is made at a Test Bench" {
		t.Errorf("reason = %q, want it to name the bench", got)
	}
}

// Standing away from a station and asking anyway is refused.
func TestCraftingOutOfRangeIsRefused(t *testing.T) {
	h := newCopse(t)
	player, sink, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.placeAt(player, 900, 288)
	h.craft(player, station.ID, "test_plank")
	h.tick(content.ActionTicks * 2)

	if len(events.crafts) != 0 {
		t.Fatalf("%d runs were asked for from across the map, want 0", len(events.crafts))
	}
	if got := lastCraftReason(sink.craftings()); got != "you are too far away" {
		t.Errorf("reason = %q, want %q", got, "you are too far away")
	}
}

// --- the station itself -----------------------------------------------------

// A station is an entity, shared by everyone, with its content id on the wire.
func TestAStationIsASharedEntityEveryoneCanSee(t *testing.T) {
	h := newCopse(t)
	h.joinWithEvents("alice")
	h.joinWithEvents("bob")
	h.tick(1)

	n := 0
	for _, e := range h.room.entities {
		if e.Station == nil {
			continue
		}
		n++
		if e.Layer != SharedLayer {
			t.Errorf("station %q is in layer %d, want the shared layer", e.Name, e.Layer)
		}
	}
	if n != 2 {
		t.Errorf("the room holds %d stations, want 2 (one each, shared)", n)
	}

	state := h.stationNamed("forge").state(false)
	if state.GetKind() != mmov1.EntityKind_ENTITY_KIND_STATION {
		t.Errorf("kind = %v, want a station", state.GetKind())
	}
	if state.GetStationId() != "forge" {
		t.Errorf("station_id = %q, want forge", state.GetStationId())
	}
}

// Asking a station what it makes reaches the session, because only the session
// can see what is in the bag.
func TestAskingAStationWhatItMakesReachesTheSession(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")

	h.room.handle(interactCmd{id: player, target: station.ID, kind: InteractStation})

	if len(events.stations) != 1 {
		t.Fatalf("the session received %d menu requests, want 1", len(events.stations))
	}
	req := events.stations[0]
	if req.Station.ID != "forge" {
		t.Errorf("the request names station %q, want forge", req.Station.ID)
	}
	// The levels ride along, because the room holds them and the session would
	// otherwise have to ask back for what the room already knows.
	if _, ok := req.Levels["forging"]; !ok {
		t.Errorf("the request carries levels %v, want forging among them", req.Levels)
	}
}

// And asking from across the map is refused rather than answered.
func TestAskingAStationFromTooFarAwayIsRefused(t *testing.T) {
	h := newCopse(t)
	player, sink, events, station := h.atStation("forge")

	h.placeAt(player, 900, 288)
	h.room.handle(interactCmd{id: player, target: station.ID, kind: InteractStation})
	// One tick, so the refusal reaches the sink: events are batched and flushed
	// with the snapshot, like every other one.
	h.tick(1)

	if len(events.stations) != 0 {
		t.Errorf("%d menu requests were sent from across the map, want 0", len(events.stations))
	}
	if got := lastCraftReason(sink.craftings()); got != "you are too far away" {
		t.Errorf("reason = %q, want %q", got, "you are too far away")
	}
}

// --- reconnecting -----------------------------------------------------------

// A reconnecting player is not mid-craft. Their client has no idea it was
// running, and a craft left running would keep spending materials invisibly.
func TestReconnectingDoesNotResumeCrafting(t *testing.T) {
	h := newCopse(t)
	player, _, events, station := h.atStation("forge")
	h.alignToActionTick()

	h.craft(player, station.ID, "test_plank")
	h.room.handle(freezeCmd{id: player})
	h.room.handle(attachCmd{
		id:     player,
		attach: Attachment{Sink: newSink(), Events: events},
		result: make(chan bool, 1),
	})
	h.tick(content.ActionTicks * 2)

	if len(events.crafts) != 0 {
		t.Errorf("%d runs were asked for after reconnecting, want 0", len(events.crafts))
	}
}
