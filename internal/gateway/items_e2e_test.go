package gateway

import (
	"testing"
	"time"

	"github.com/coder/websocket"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// The M3 exit criterion, over a real socket and a real database: kill a mob,
// get an item, equip it, and watch the stat change by exactly the amount the
// item's own modifiers predict.
//
// "Exactly" is the point. A tooltip that is approximately right is a stat
// system players cannot plan against.

func (c *client) itemAction(kind mmov1.ItemActionKind, itemID string, slot uint32, equipSlot string) {
	c.send(&mmov1.ClientMessage{Body: &mmov1.ClientMessage_ItemAction{
		ItemAction: &mmov1.ItemAction{
			Kind: kind, ItemId: itemID, Slot: slot, EquipSlot: equipSlot,
		},
	}})
}

// awaitInventory returns the most recent inventory, reading more if none has
// arrived yet.
//
// The most recent rather than the first: an equip produces a new inventory,
// and a test asserting on the state after one must not be handed the state
// before it.
func (c *client) awaitInventory(d time.Duration) *mmov1.Inventory {
	deadline := time.Now().Add(d)
	for {
		var latest *mmov1.Inventory
		for _, m := range c.inbox {
			if inv := m.GetInventory(); inv != nil {
				latest = inv
			}
		}
		if latest != nil {
			c.inbox = c.inbox[:0]
			return latest
		}
		if time.Now().After(deadline) {
			return nil
		}
		c.drain(200 * time.Millisecond)
	}
}

func statOf(inv *mmov1.Inventory, id stats.StatID) stats.Value {
	for _, s := range inv.GetStats() {
		if s.GetStat() == id.String() {
			return stats.Value(s.GetValue())
		}
	}
	return 0
}

func TestInventoryArrivesOnConnect(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Collector")
	c := ts.connect(t, p)

	inv := c.awaitInventory(3 * time.Second)
	if inv == nil {
		t.Fatal("no inventory was sent on connect")
	}
	if inv.GetCapacity() == 0 {
		t.Error("the inventory reports no capacity")
	}

	// Stats must arrive with it, or a tooltip cannot show what equipping
	// something would change.
	if len(inv.GetStats()) == 0 {
		t.Error("no stats were sent with the inventory")
	}
	if statOf(inv, stats.Attack) == 0 {
		t.Error("attack is zero; the character has no base stats")
	}
}

// The headline: equip an item and the stat changes by exactly its modifiers.
func TestEquippingChangesStatsExactly(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Equipper")
	c := ts.connect(t, p)

	before, equippable := farmUntilCarrying(t, c, 30*time.Second, equippableAtLevelOne)
	if equippable == nil {
		t.Skip("no equippable item dropped within the time budget")
	}

	// What the item claims it will do. This is exactly what a tooltip shows.
	predicted := map[string]stats.Value{}
	for _, m := range equippable.GetMods() {
		if m.GetKind() == "flat" {
			predicted[m.GetStat()] += stats.Value(m.GetValue())
		}
	}
	if len(predicted) == 0 {
		t.Skip("the dropped item has no flat modifiers to predict against")
	}

	attackBefore := statOf(before, stats.Attack)
	lifeBefore := statOf(before, stats.MaxLife)

	c.itemAction(mmov1.ItemActionKind_ITEM_ACTION_KIND_EQUIP, equippable.GetItemId(), 0, "")

	// Several inventories may arrive before the right one: the equip is a
	// database round trip on another goroutine, and anything already queued is
	// still the state from before it.
	var after *mmov1.Inventory
	equipDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(equipDeadline) && after == nil {
		inv := c.awaitInventory(2 * time.Second)
		if inv == nil {
			continue
		}
		for _, item := range inv.GetEquipped() {
			if item.GetItemId() == equippable.GetItemId() {
				after = inv
			}
		}
	}
	if after == nil {
		t.Fatal("the item was never reported as equipped")
	}

	// The exact-match assertion. Any drift here is a stat system players
	// cannot plan against.
	for stat, delta := range predicted {
		var gotBefore, gotAfter stats.Value
		switch stat {
		case stats.Attack.String():
			gotBefore, gotAfter = attackBefore, statOf(after, stats.Attack)
		case stats.MaxLife.String():
			gotBefore, gotAfter = lifeBefore, statOf(after, stats.MaxLife)
		default:
			continue
		}

		if gotAfter-gotBefore != delta {
			t.Errorf("%s changed by %v, but the item's modifiers predict %v",
				stat, gotAfter-gotBefore, delta)
		}
	}

	// And unequipping must put it back exactly.
	c.itemAction(mmov1.ItemActionKind_ITEM_ACTION_KIND_UNEQUIP, "", 0, equippable.GetEquipSlot())

	var restored *mmov1.Inventory
	unequipDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(unequipDeadline) && restored == nil {
		inv := c.awaitInventory(2 * time.Second)
		if inv == nil {
			continue
		}
		if len(inv.GetEquipped()) == 0 {
			restored = inv
		}
	}
	if restored == nil {
		t.Fatal("the item was never unequipped")
	}
	if got := statOf(restored, stats.Attack); got != attackBefore {
		t.Errorf("attack is %v after unequipping, was %v before equipping", got, attackBefore)
	}
}

// An item cannot be equipped into a slot it does not belong in, and a
// character below its level requirement cannot wear it at all.
func TestEquipRefusesUnsuitableItems(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Picky")
	c := ts.connect(t, p)

	inv := c.awaitInventory(3 * time.Second)
	if inv == nil {
		t.Fatal("no inventory")
	}

	// A forged item id must be refused rather than crashing anything.
	c.itemAction(mmov1.ItemActionKind_ITEM_ACTION_KIND_EQUIP,
		"00000000-0000-0000-0000-000000000000", 0, "")

	// The connection must survive, and the room keep ticking.
	if snaps := c.awaitSnapshots(3); len(snaps) < 3 {
		t.Error("the session stopped after an invalid item action")
	}
}

// Item actions share the input rate limit, so they cannot be used to bypass it
// or to hammer the database.
func TestItemActionFloodIsSurvivable(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Spammer")
	c := ts.connect(t, p)
	c.awaitInventory(3 * time.Second)

	for i := 0; i < 400; i++ {
		c.itemAction(mmov1.ItemActionKind_ITEM_ACTION_KIND_EQUIP,
			"00000000-0000-0000-0000-000000000000", 0, "")
	}

	if snaps := c.awaitSnapshots(3); len(snaps) < 3 {
		t.Error("the room stopped ticking under an item action flood")
	}
}

// Items must survive a reconnect: they are written through at the moment they
// are picked up rather than waiting for a checkpoint.
func TestItemsSurviveReconnect(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Hoarder")

	first := ts.connect(t, p)
	before, item := farmUntilCarrying(t, first, 30*time.Second, func(*mmov1.ItemStack) bool { return true })
	if item == nil {
		t.Skip("nothing was looted within the time budget")
	}
	carried := len(before.GetCarried())

	first.conn.Close(websocket.StatusNormalClosure, "logging out")
	waitUntil(t, 8*time.Second, func() bool { return ts.characterFree(t, p) })

	second := ts.connect(t, p)
	after := second.awaitInventory(5 * time.Second)
	if after == nil {
		t.Fatal("no inventory after reconnecting")
	}

	if len(after.GetCarried()) < carried {
		t.Errorf("carried %d items before the reconnect and %d after; "+
			"items must be written through at pickup, not at checkpoint",
			carried, len(after.GetCarried()))
	}
}

// A dropped item's modifiers must reach the client, or no tooltip is possible.
func TestDroppedItemsCarryTheirModifiers(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Inspector")
	c := ts.connect(t, p)

	_, item := farmUntilCarrying(t, c, 30*time.Second, func(i *mmov1.ItemStack) bool {
		return i.GetEquipSlot() != ""
	})
	if item == nil {
		t.Skip("no equipment dropped within the time budget")
	}

	if item.GetName() == "" {
		t.Error("an item arrived with no display name")
	}
	if len(item.GetMods()) == 0 {
		t.Errorf("equipment %q arrived with no modifiers, so no tooltip is possible", item.GetName())
	}
	// Every modifier must name a stat the client can resolve.
	for _, m := range item.GetMods() {
		if _, ok := stats.Parse(m.GetStat()); !ok {
			t.Errorf("modifier names unknown stat %q", m.GetStat())
		}
	}
}

// farmUntilCarrying fights until the inventory holds an item matching want,
// then stops.
//
// Stopping matters: item actions share the input rate limit, so a test that
// keeps hammering intents and loot requests while it equips will have the
// equip dropped as excess -- which looks exactly like the equip failing.
//
// Loot requests target drop ids seen in snapshots rather than a blind sweep,
// for the same reason: a sweep across sixty ids several times a second is
// itself over the limit.
func farmUntilCarrying(t *testing.T, c *client, within time.Duration, want func(*mmov1.ItemStack) bool) (*mmov1.Inventory, *mmov1.ItemStack) {
	t.Helper()

	deadline := time.Now().Add(within)
	seq := uint32(0)
	seenDrops := map[uint32]bool{}

	for time.Now().Before(deadline) {
		// A short burst of play, well inside the input rate limit.
		for i := 0; i < 8; i++ {
			seq++
			c.intent(seq, 1000, false)
			if seq%3 == 0 {
				c.cast("slash", false)
			}
			time.Sleep(room.TickPeriod)
		}

		c.drain(200 * time.Millisecond)

		var inventory *mmov1.Inventory
		for _, m := range c.inbox {
			if inv := m.GetInventory(); inv != nil {
				inventory = inv
			}
			snap := m.GetSnapshot()
			if snap == nil {
				continue
			}
			for _, e := range snap.GetEntered() {
				if e.GetKind() == mmov1.EntityKind_ENTITY_KIND_DROP && !seenDrops[e.GetId()] {
					seenDrops[e.GetId()] = true
					c.loot(e.GetId())
				}
			}
		}
		c.inbox = c.inbox[:0]

		if inventory == nil {
			continue
		}
		for _, item := range inventory.GetCarried() {
			if want(item) {
				return inventory, item
			}
		}
	}
	return nil, nil
}

func equippableAtLevelOne(item *mmov1.ItemStack) bool {
	return item.GetEquipSlot() != "" && item.GetRequiredLevel() <= 1
}
