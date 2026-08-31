package world

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/store"
	"github.com/ctrl-research/mmo/internal/world/items"
	"github.com/ctrl-research/mmo/internal/world/stats"
	"github.com/google/uuid"
)

// Inventory is a character's items, held by the session rather than the room.
//
// The room must never block, and item mutations must be durable the moment
// they happen -- a boss drop lost to a crash is exactly the kind of thing a
// player files a ticket over. Those two requirements are incompatible in the
// tick loop, so the session owns the inventory: it can do I/O, and it pushes
// only the *result* (a recomputed stat block) into the room.
//
// The room therefore holds no item state at all, which also means an item can
// never be duplicated by a room and a database disagreeing about it.
type Inventory struct {
	// InventorySlots is how many item slots a character carries.
	//
	// Generous by MapleStory standards, because inventory management is not
	// the interesting part of this game and running out mid-session is pure
	// friction.
	mu sync.Mutex

	characterID uuid.UUID
	store       *store.Store
	gen         *items.Generator
	content     *content.Content

	inventoryID uuid.UUID
	equipmentID uuid.UUID

	// slots and equipped mirror the database, so reads are instant and the
	// database is consulted only on change.
	slots    map[int]*Slot
	equipped map[content.EquipSlot]*Slot

	capacity int
}

// Slot is one item in a container.
type Slot struct {
	ItemID   uuid.UUID
	Slot     int
	Instance *items.Instance
}

// Inventory sizing.
const (
	InventorySlots = 24
	EquipmentSlots = 6
)

// Inventory errors.
var (
	ErrInventoryFull  = errors.New("world: inventory is full")
	ErrNotEquippable  = errors.New("world: item cannot be equipped")
	ErrLevelTooLow    = errors.New("world: character level is too low for this item")
	ErrNoSuchItem     = errors.New("world: no such item")
	ErrWrongCharacter = errors.New("world: item belongs to another character")
)

// LoadInventory reads a character's items.
func LoadInventory(ctx context.Context, st *store.Store, c *content.Content, characterID uuid.UUID) (*Inventory, error) {
	inv, equip, err := st.EnsureContainers(ctx, characterID, InventorySlots, EquipmentSlots)
	if err != nil {
		return nil, err
	}

	i := &Inventory{
		characterID: characterID,
		store:       st,
		gen:         items.NewGenerator(c),
		content:     c,
		inventoryID: inv.ID,
		equipmentID: equip.ID,
		slots:       make(map[int]*Slot),
		equipped:    make(map[content.EquipSlot]*Slot),
		capacity:    inv.Capacity,
	}

	if err := i.reload(ctx); err != nil {
		return nil, err
	}
	return i, nil
}

// reload rebuilds the in-memory view from the database.
func (i *Inventory) reload(ctx context.Context) error {
	invRows, err := i.store.LoadContainer(ctx, i.inventoryID)
	if err != nil {
		return err
	}
	equipRows, err := i.store.LoadContainer(ctx, i.equipmentID)
	if err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	clear(i.slots)
	clear(i.equipped)

	for _, row := range invRows {
		inst, err := decodeInstance(row)
		if err != nil {
			// A single unreadable item must not make a character unplayable.
			// Skipping it leaves it in the database to be investigated rather
			// than deleting it, which would be unrecoverable.
			continue
		}
		i.slots[row.Slot] = &Slot{ItemID: row.ID, Slot: row.Slot, Instance: inst}
	}

	for _, row := range equipRows {
		inst, err := decodeInstance(row)
		if err != nil {
			continue
		}
		if row.Slot >= 0 && row.Slot < len(content.EquipSlots) {
			i.equipped[content.EquipSlots[row.Slot]] = &Slot{
				ItemID: row.ID, Slot: row.Slot, Instance: inst,
			}
		}
	}
	return nil
}

// Grant adds a newly generated item, writing it through immediately.
//
// Write-through rather than waiting for the periodic checkpoint: this is a
// drop the player just earned, and losing it to a crash thirty seconds later
// is precisely the case the checkpoint window is not allowed to cover.
func (i *Inventory) Grant(ctx context.Context, inst *items.Instance, tick uint64) (uuid.UUID, error) {
	// Into an existing stack first, when the item is one that stacks.
	//
	// Without this a stackable material takes a slot per unit, which nobody
	// notices while loot is the only source -- a boar drops one hide -- and
	// which fills a bag in two minutes once gathering exists. A player is
	// meant to be able to chop for twenty minutes, and twenty minutes is
	// several hundred logs.
	if id, ok, err := i.stack(ctx, inst, tick); err != nil {
		return uuid.Nil, err
	} else if ok {
		return id, nil
	}

	slot, err := i.store.FreeSlot(ctx, i.inventoryID)
	if errors.Is(err, store.ErrContainerFull) {
		return uuid.Nil, ErrInventoryFull
	}
	if err != nil {
		return uuid.Nil, err
	}

	mods, err := json.Marshal(inst)
	if err != nil {
		return uuid.Nil, fmt.Errorf("world: encoding item: %w", err)
	}

	id, err := i.store.InsertItem(ctx, i.inventoryID, slot, store.ItemRow{
		BaseID:    inst.BaseID,
		Rarity:    string(inst.Rarity),
		ItemLevel: inst.ItemLevel,
		Mods:      mods,
		StackSize: inst.Stack,
	}, i.characterID, store.EventPickup, tick)
	if err != nil {
		return uuid.Nil, err
	}

	i.mu.Lock()
	i.slots[slot] = &Slot{ItemID: id, Slot: slot, Instance: inst}
	i.mu.Unlock()

	return id, nil
}

// stack tries to merge a grant into an existing stack, and reports whether it
// did.
//
// Only the base type has to match. Two stacks of the same material are
// interchangeable by definition -- a material has no rolled affixes, which is
// also why equipment is refused a stack at load (see the loader) rather than
// being filtered out here.
func (i *Inventory) stack(ctx context.Context, inst *items.Instance, tick uint64) (uuid.UUID, bool, error) {
	base, ok := i.content.Items[inst.BaseID]
	if !ok || !base.Stackable || base.MaxStack < 2 {
		return uuid.Nil, false, nil
	}

	qty := inst.Stack
	if qty < 1 {
		qty = 1
	}

	id, total, err := i.store.StackInto(
		ctx, i.inventoryID, inst.BaseID, qty, base.MaxStack, i.characterID, tick)
	if err != nil {
		return uuid.Nil, false, err
	}
	if id == uuid.Nil {
		return uuid.Nil, false, nil
	}

	// The in-memory view is patched rather than reloaded: a reload is two more
	// round trips for one number, on a path that runs every few seconds per
	// gatherer.
	i.mu.Lock()
	for _, slot := range i.slots {
		if slot != nil && slot.ItemID == id && slot.Instance != nil {
			slot.Instance.Stack = total
			break
		}
	}
	i.mu.Unlock()

	return id, true, nil
}

// Equip moves an item from the inventory into its slot.
//
// Whatever was equipped goes back to the inventory, in one atomic swap where
// possible -- an equip that half-succeeds would either lose the old item or
// duplicate the new one.
func (i *Inventory) Equip(ctx context.Context, itemID uuid.UUID, characterLevel int, tick uint64) error {
	i.mu.Lock()
	var found *Slot
	for _, s := range i.slots {
		if s.ItemID == itemID {
			found = s
			break
		}
	}
	i.mu.Unlock()

	if found == nil {
		return ErrNoSuchItem
	}

	base, ok := i.content.Items[found.Instance.BaseID]
	if !ok || !base.IsEquipment() {
		return ErrNotEquippable
	}
	if base.Level > characterLevel {
		return ErrLevelTooLow
	}

	slotIndex := equipSlotIndex(base.Slot)
	if slotIndex < 0 {
		return ErrNotEquippable
	}

	i.mu.Lock()
	current := i.equipped[base.Slot]
	i.mu.Unlock()

	if current != nil {
		// Both items change place at once. Doing it as two moves would leave a
		// window in which the old item is nowhere, and a failure there loses
		// it outright.
		if err := i.store.SwapItems(ctx, found.ItemID, current.ItemID, i.characterID, tick); err != nil {
			return err
		}
	} else {
		if err := i.store.MoveItem(ctx, found.ItemID, i.equipmentID, slotIndex,
			i.characterID, store.EventEquip, tick); err != nil {
			return err
		}
	}

	// Rebuilt from the database rather than patched in memory: the database is
	// the authority on where things are, and a divergence between the two is
	// how an item ends up appearing twice.
	return i.reload(ctx)
}

// Unequip moves an equipped item back to the inventory.
func (i *Inventory) Unequip(ctx context.Context, slot content.EquipSlot, tick uint64) error {
	i.mu.Lock()
	current := i.equipped[slot]
	i.mu.Unlock()

	if current == nil {
		return ErrNoSuchItem
	}

	free, err := i.store.FreeSlot(ctx, i.inventoryID)
	if errors.Is(err, store.ErrContainerFull) {
		return ErrInventoryFull
	}
	if err != nil {
		return err
	}

	if err := i.store.MoveItem(ctx, current.ItemID, i.inventoryID, free,
		i.characterID, store.EventUnequip, tick); err != nil {
		return err
	}
	return i.reload(ctx)
}

// Move relocates an item within the inventory, swapping if the target is
// occupied.
func (i *Inventory) Move(ctx context.Context, itemID uuid.UUID, toSlot int, tick uint64) error {
	if toSlot < 0 || toSlot >= i.capacity {
		return fmt.Errorf("world: slot %d is outside the inventory", toSlot)
	}

	i.mu.Lock()
	occupant := i.slots[toSlot]
	i.mu.Unlock()

	var err error
	if occupant != nil && occupant.ItemID != itemID {
		err = i.store.SwapItems(ctx, itemID, occupant.ItemID, i.characterID, tick)
	} else {
		err = i.store.MoveItem(ctx, itemID, i.inventoryID, toSlot,
			i.characterID, store.EventMove, tick)
	}
	if err != nil {
		return err
	}
	return i.reload(ctx)
}

// Destroy removes an item permanently.
func (i *Inventory) Destroy(ctx context.Context, itemID uuid.UUID, tick uint64) error {
	if err := i.store.DestroyItem(ctx, itemID, i.characterID, tick); err != nil {
		return err
	}
	return i.reload(ctx)
}

// StatBlock computes the character's derived statistics.
//
// Rebuilt from scratch on every change rather than patched, because removing a
// modifier from a running product is lossy and an incremental path that drifts
// produces stats that depend on the order things were equipped.
func (i *Inventory) StatBlock(level int) *stats.Block {
	b := stats.NewBlock()

	// Level scaling is the character's own contribution, before any item.
	b.SetBase(stats.Attack, stats.FromInt(5+level*2))
	b.SetBase(stats.Armour, stats.FromInt(level))
	b.SetBase(stats.MaxLife, stats.FromInt(100+(level-1)*20))
	b.SetBase(stats.MaxMana, stats.FromInt(50+(level-1)*10))
	b.SetBase(stats.CritChance, stats.FromPercent(5))
	b.SetBase(stats.CritMultiplier, stats.FromPercent(150))
	b.SetBase(stats.AttackSpeed, stats.FromInt(1))
	b.SetBase(stats.MovementSpeed, stats.FromInt(1))

	i.mu.Lock()
	defer i.mu.Unlock()

	// Only equipped items contribute. Carrying a sword should not make anyone
	// hit harder.
	for _, s := range i.equipped {
		if s != nil && s.Instance != nil {
			b.AddAll(s.Instance.Modifiers())
		}
	}
	return b
}

// Snapshot returns the current contents, for sending to a client.
func (i *Inventory) Snapshot() ([]*Slot, map[content.EquipSlot]*Slot) {
	i.mu.Lock()
	defer i.mu.Unlock()

	carried := make([]*Slot, 0, len(i.slots))
	for _, s := range i.slots {
		carried = append(carried, s)
	}
	// Sorted, so the client sees a stable order rather than one that depends
	// on map iteration.
	for a := 1; a < len(carried); a++ {
		for b := a; b > 0 && carried[b].Slot < carried[b-1].Slot; b-- {
			carried[b], carried[b-1] = carried[b-1], carried[b]
		}
	}

	worn := make(map[content.EquipSlot]*Slot, len(i.equipped))
	for k, v := range i.equipped {
		worn[k] = v
	}
	return carried, worn
}

// Capacity returns the number of inventory slots.
func (i *Inventory) Capacity() int { return i.capacity }

// Generator exposes the item generator, so callers can name and describe
// items without a second copy of the content.
func (i *Inventory) Generator() *items.Generator { return i.gen }

// equipSlotIndex maps a slot to its position in the equipment container.
func equipSlotIndex(slot content.EquipSlot) int {
	for i, s := range content.EquipSlots {
		if s == slot {
			return i
		}
	}
	return -1
}

// decodeInstance reads a stored item back.
func decodeInstance(row store.ItemRow) (*items.Instance, error) {
	var inst items.Instance
	if err := json.Unmarshal(row.Mods, &inst); err != nil {
		return nil, err
	}
	// The columns are authoritative for the fields they duplicate: the blob is
	// convenient, but a column is what queries and constraints can see.
	inst.BaseID = row.BaseID
	inst.Rarity = items.Rarity(row.Rarity)
	inst.ItemLevel = row.ItemLevel
	inst.Stack = row.StackSize
	return &inst, nil
}
