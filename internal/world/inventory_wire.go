package world

import (
	"context"

	"github.com/ctrl-research/mmo/internal/content"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/items"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// Sending the inventory to a client.
//
// Sent whole rather than delta-compressed. Inventories change rarely compared
// to positions, they are small, and a delta bug here means an item that
// appears to exist twice -- which is the one class of bug this whole system is
// built to make impossible. The bandwidth saving is not worth that risk.

// statsShown are the statistics a client is told about.
//
// Not every stat: the client needs what a tooltip and a character sheet show,
// and sending the rest is bytes nobody reads.
var statsShown = []stats.StatID{
	stats.Attack,
	stats.Armour,
	stats.MaxLife,
	stats.MaxMana,
	stats.CritChance,
	stats.CritMultiplier,
	stats.AttackSpeed,
	stats.MovementSpeed,
	stats.Strength,
	stats.Dexterity,
	stats.Intelligence,
	stats.FireResistance,
	stats.ColdResistance,
	stats.LightningResistance,
}

// pushInventory sends the current inventory with freshly computed stats.
func (s *Session) pushInventory(ctx context.Context) {
	s.pushInventoryWithStats(ctx, s.inventory.StatBlock(s.characterLevel(ctx)))
}

// pushInventoryWithStats sends the inventory alongside an already-computed
// block, so a refresh does not compute it twice.
func (s *Session) pushInventoryWithStats(_ context.Context, block *stats.Block) {
	// Read under the lock: a reconnect replaces the sink from another
	// goroutine, and sending to the old one means the returning player never
	// sees their inventory.
	s.mu.Lock()
	sink := s.sink
	s.mu.Unlock()

	if sink == nil {
		return
	}

	carried, worn := s.inventory.Snapshot()
	gen := s.inventory.Generator()

	msg := &mmov1.Inventory{
		Capacity: uint32(s.inventory.Capacity()),
	}

	for _, slot := range carried {
		msg.Carried = append(msg.Carried, s.itemStack(gen, slot, ""))
	}
	for _, equipSlot := range content.EquipSlots {
		if slot := worn[equipSlot]; slot != nil {
			msg.Equipped = append(msg.Equipped, s.itemStack(gen, slot, equipSlot))
		}
	}

	for _, id := range statsShown {
		msg.Stats = append(msg.Stats, &mmov1.StatValue{
			Stat:  id.String(),
			Value: int64(block.Value(id)),
		})
	}

	sink.Send(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Inventory{Inventory: msg},
	})
}

// itemStack renders one item for the wire.
//
// The modifiers are sent with their rolled values and tiers, so the client can
// draw a complete tooltip without holding a copy of the affix tables -- and so
// what it shows is exactly the number the server computed with, rather than
// its own re-derivation.
func (s *Session) itemStack(gen *items.Generator, slot *Slot, equipSlot content.EquipSlot) *mmov1.ItemStack {
	inst := slot.Instance

	stack := &mmov1.ItemStack{
		ItemId:    slot.ItemID.String(),
		BaseId:    inst.BaseID,
		Name:      gen.DisplayName(inst),
		Rarity:    string(inst.Rarity),
		Slot:      uint32(slot.Slot),
		Stack:     uint32(inst.Stack),
		ItemLevel: uint32(inst.ItemLevel),
		EquipSlot: string(equipSlot),
	}

	if base, ok := gen.Base(inst); ok {
		stack.RequiredLevel = uint32(base.Level)
		if equipSlot == "" && base.IsEquipment() {
			// Carried equipment still reports where it would go, so the client
			// can show a slot hint without looking the base type up.
			stack.EquipSlot = string(base.Slot)
		}
	}

	for _, m := range inst.Implicits {
		stack.Mods = append(stack.Mods, &mmov1.ItemMod{
			Stat: m.Stat.String(), Kind: m.Kind.String(),
			Value: int64(m.Value), Implicit: true,
		})
	}
	for _, m := range inst.Affixes {
		stack.Mods = append(stack.Mods, &mmov1.ItemMod{
			Stat: m.Stat.String(), Kind: m.Kind.String(),
			Value: int64(m.Value), Tier: uint32(m.Tier),
		})
	}
	return stack
}
