package world

import (
	"testing"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/items"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// The conversions that put room.Handle on the wire.
//
// Round trips rather than field-by-field assertions where possible: a field
// added to one of these structs and forgotten in the encoder is the failure
// this is guarding against, and comparing whole values is what catches it.

func TestWireProgressRoundTrips(t *testing.T) {
	want := room.Progress{Level: 42, Exp: 123456, Gold: 789, MapID: "henesys"}
	if got := decodeProgress(encodeProgress(want)); got != want {
		t.Errorf("progress round-tripped to %+v, want %+v", got, want)
	}
}

func TestWireVecKeepsFixedPointUnits(t *testing.T) {
	// Deliberately not a whole number of pixels: converting through a float
	// would round, and a spawn a fraction of a unit off is a character
	// standing inside the floor.
	want := sim.Vec{X: fixed.F(12345), Y: fixed.F(-6789)}
	if got := decodeVec(encodeVec(want)); got != want {
		t.Errorf("vec round-tripped to %+v, want %+v", got, want)
	}
}

func TestWireStatsRoundTrip(t *testing.T) {
	b := stats.NewBlock()
	b.SetBase(stats.Strength, stats.FromInt(37))
	b.Add(stats.Modifier{Stat: stats.MaxLife, Kind: stats.Flat, Value: stats.FromInt(150)})
	b.Add(stats.Modifier{Stat: stats.MaxLife, Kind: stats.Increased, Value: stats.FromInt(20)})

	got, ok := decodeStats(encodeStats(b))
	if !ok {
		t.Fatal("a block encoded here did not decode")
	}
	for stat := stats.StatID(0); stat < stats.NumStats; stat++ {
		if got.Value(stat) != b.Value(stat) {
			t.Errorf("stat %d is %v, want %v", stat, got.Value(stat), b.Value(stat))
		}
	}
}

// A block of the wrong length is refused, not padded.
//
// The "more" layer is a product, so a padded zero is not a missing multiplier
// but a total one: every stat past the end would be multiplied by zero, and
// the character would have no life and do no damage. Refusing leaves the room
// with the block it already had, which is stale but playable.
func TestWireStatsRefusesAWrongLengthBlock(t *testing.T) {
	full := encodeStats(stats.NewBlock())

	short := &mmov1.WireStats{
		Base:      full.GetBase()[:2],
		Flat:      full.GetFlat(),
		Increased: full.GetIncreased(),
		More:      full.GetMore(),
	}
	if _, ok := decodeStats(short); ok {
		t.Error("a block built against a different stat list was accepted")
	}
	if _, ok := decodeStats(nil); ok {
		t.Error("a missing block was accepted")
	}
}

func TestWireDerivedRoundTrips(t *testing.T) {
	b := stats.NewBlock()
	b.SetBase(stats.Strength, stats.FromInt(11))

	want := room.Derived{
		Block: b, MaxHP: 1234,
		ToolPower: map[string]int{"mining": 7, "fishing": 3},
	}
	got, ok := decodeDerived(encodeDerived(want))
	if !ok {
		t.Fatal("derived stats did not decode")
	}
	if got.MaxHP != want.MaxHP {
		t.Errorf("max hp is %d, want %d", got.MaxHP, want.MaxHP)
	}
	if len(got.ToolPower) != 2 || got.ToolPower["mining"] != 7 || got.ToolPower["fishing"] != 3 {
		t.Errorf("tool power is %v", got.ToolPower)
	}
	if got.Block.Value(stats.Strength) != b.Value(stats.Strength) {
		t.Error("the stat block did not survive")
	}
}

func TestWireDerivedRefusesABadBlock(t *testing.T) {
	if _, ok := decodeDerived(&mmov1.WireDerived{MaxHp: 100}); ok {
		t.Error("derived stats with no block were accepted")
	}
}

func TestWireSnapshotRoundTrips(t *testing.T) {
	want := room.Snapshot{
		Progress:  room.Progress{Level: 9, Exp: 400, Gold: 50, MapID: "forest"},
		State:     room.CharacterState{HP: 77},
		Secondary: room.SecondaryProgress{"mining": 900},
	}

	got := decodeSnapshot(encodeSnapshot(want))
	if got.Progress != want.Progress {
		t.Errorf("progress is %+v, want %+v", got.Progress, want.Progress)
	}
	if got.State.HP != want.State.HP {
		t.Errorf("hp is %d, want %d", got.State.HP, want.State.HP)
	}
	if got.Secondary["mining"] != 900 {
		t.Errorf("secondary progress is %v", got.Secondary)
	}
}

func TestWireJoinSpecRoundTrips(t *testing.T) {
	want := room.JoinSpec{
		CharacterID:    "char-1",
		Name:           "Alice",
		Progress:       room.Progress{Level: 3, Exp: 12, Gold: 5, MapID: "test"},
		State:          room.CharacterState{HP: 42},
		Fresh:          true,
		Loadout:        []room.LoadoutSlot{{SkillID: "slash", Rank: 2, Supports: []string{"multi"}}},
		LayerKey:       "party-7",
		KnownWaypoints: []string{"town"},
		Secondary:      room.SecondaryProgress{"fishing": 10},
		Spawn:          sim.Vec{X: fixed.F(100), Y: fixed.F(200)},
		Arrived:        true,
	}

	got := decodeJoinSpec(encodeJoinSpec(want))
	if got.CharacterID != want.CharacterID || got.Name != want.Name {
		t.Errorf("identity is %q/%q", got.CharacterID, got.Name)
	}
	if got.Progress != want.Progress || got.Spawn != want.Spawn {
		t.Errorf("progress or spawn did not survive: %+v %+v", got.Progress, got.Spawn)
	}
	if !got.Fresh || !got.Arrived || got.LayerKey != "party-7" {
		t.Errorf("flags did not survive: %+v", got)
	}
	if len(got.Loadout) != 1 || got.Loadout[0].SkillID != "slash" ||
		got.Loadout[0].Rank != 2 || len(got.Loadout[0].Supports) != 1 {
		t.Errorf("loadout is %+v", got.Loadout)
	}
	if got.State.HP != 42 {
		t.Errorf("state is %+v", got.State)
	}
	if len(got.KnownWaypoints) != 1 || got.Secondary["fishing"] != 10 {
		t.Errorf("waypoints %v, secondary %v", got.KnownWaypoints, got.Secondary)
	}
}

// Nil and empty are different answers, and the wire has to keep them apart.
//
// Nil means "leave what the room already has", which is what a reconnect to a
// character still standing in the room wants. Empty means "set it to nothing".
func TestWireKeepsNilDistinctFromEmpty(t *testing.T) {
	if got := decodeSecondary(encodeSecondary(nil)); got != nil {
		t.Errorf("nil secondary progress round-tripped to %v", got)
	}
	if got := decodeSecondary(encodeSecondary(room.SecondaryProgress{})); got == nil {
		t.Error("empty secondary progress round-tripped to nil")
	}
	if got := decodeLoadout(encodeLoadout(nil)); got != nil {
		t.Errorf("a nil loadout round-tripped to %v", got)
	}
	if got := decodeLoadout(encodeLoadout([]room.LoadoutSlot{})); got == nil {
		t.Error("an empty loadout round-tripped to nil")
	}
}

func TestWireItemRoundTrips(t *testing.T) {
	want := &items.Instance{
		BaseID: "iron-sword", Rarity: items.Rare, ItemLevel: 12, Stack: 3,
	}
	got := decodeItem(encodeItem(want))
	if got == nil {
		t.Fatal("an item did not decode")
	}
	if got.BaseID != want.BaseID || got.Rarity != want.Rarity ||
		got.ItemLevel != want.ItemLevel || got.Stack != want.Stack {
		t.Errorf("item round-tripped to %+v, want %+v", got, want)
	}
	if decodeItem(nil) != nil {
		t.Error("nothing decoded into an item")
	}
}

// A portal travels as its position in its map, and is resolved from the
// receiving node's own content.
func TestWirePortalIsNamedByItsPlaceInTheMap(t *testing.T) {
	m := &content.Map{
		ID: "test",
		Portals: []content.Portal{
			{Name: "north", TargetMap: "forest", TargetSpawn: "south"},
			{Name: "south", TargetMap: "town", TargetSpawn: "north"},
		},
	}

	index, ok := portalIndex(m, m.Portals[1])
	if !ok {
		t.Fatal("a portal from this map was not found in it")
	}
	if index != 1 {
		t.Errorf("index is %d, want 1", index)
	}

	got, ok := portalAt(m, index)
	if !ok || got != m.Portals[1] {
		t.Errorf("resolved to %+v (%v), want %+v", got, ok, m.Portals[1])
	}
}

func TestWirePortalRefusesOneThatIsNotInTheMap(t *testing.T) {
	m := &content.Map{ID: "test", Portals: []content.Portal{{Name: "north"}}}

	if _, ok := portalIndex(m, content.Portal{Name: "nowhere"}); ok {
		t.Error("a portal that is not in the map was given an index")
	}
	// Out of range rather than a silent portal zero, which would send a player
	// somewhere real and wrong.
	if _, ok := portalAt(m, 5); ok {
		t.Error("an index past the end resolved to a portal")
	}
	if _, ok := portalAt(nil, 0); ok {
		t.Error("a nil map resolved to a portal")
	}
}
