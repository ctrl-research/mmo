package room

import (
	"github.com/ctrl-research/mmo/internal/fixed"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/items"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// DropState is an item or coin pile lying on the ground.
//
// A drop is a full entity rather than a special list, so visibility, layering,
// and removal all work without a separate code path. That is also what makes
// cross-layer looting unrepresentable: a drop inherits the layer of the mob
// that produced it, and an entity in another layer is not merely forbidden to
// the client -- it is never sent.
type DropState struct {
	ItemID string
	Qty    uint32
	Gold   uint32

	// Instance is the rolled item, generated at the moment of the kill so its
	// modifiers come from the room's seeded stream and a replay reproduces the
	// exact drop.
	//
	// Deliberately not persisted while it lies on the ground: most drops are
	// never picked up, and writing every one would be a great many writes for
	// nothing. The cost is that un-looted loot does not survive a crash.
	Instance *items.Instance

	// Claimed marks a drop a player has asked for and whose persistence is in
	// flight. It stays in the world, invisible and unlootable, until the write
	// succeeds -- so a database error returns the item rather than destroying
	// it.
	Claimed bool

	// Owner may loot immediately; anyone else in the same layer must wait for
	// the lock to expire. Within one player's layer this never matters, but it
	// is what makes party loot rules possible in M5 without redesigning drops.
	Owner     EntityID
	UnlockAt  uint64
	ExpiresAt uint64
}

// rollDrops generates loot for a kill.
func (r *Room) rollDrops(killer, victim *Entity) {
	if victim.Mob == nil || victim.Mob.Def.DropTable == "" {
		return
	}
	table, ok := r.content.Drops[victim.Mob.Def.DropTable]
	if !ok {
		return
	}

	source := r.randFor(victim.Layer)
	bal := r.content.Balance.Drops

	spawn := func(d DropState) {
		d.Owner = killer.ID
		d.UnlockAt = r.tick
		d.ExpiresAt = r.tick + uint64(bal.GroundTicks)

		at := victim.Body.FeetCenter()
		// Scatter so a multi-item kill is several things to pick up rather
		// than one pile of overlapping boxes.
		if bal.ScatterRange > 0 {
			spread := bal.ScatterRange.Int()
			at.X += fixed.FromInt(source.Range(-spread, spread))
		}

		body := sim.NewBody(at, fixed.FromInt(20), fixed.FromInt(20))
		sim.Settle(&body, r.cfg.World, &r.cfg.Tuning)

		r.spawnEntity(&Entity{
			Kind:  KindDrop,
			Layer: victim.Layer,
			Body:  body,
			Drop:  &d,
		})
	}

	// Rolled in a fixed order -- gold, then each entry in file order -- because
	// the order of rolls is part of what a seed reproduces. Reordering these
	// would change every drop in the game from the same seed.
	if table.GoldChance > 0 && source.PPM(table.GoldChance) {
		amount := source.Range(table.GoldMin, table.GoldMax)
		if amount > 0 {
			spawn(DropState{Gold: uint32(amount)})
		}
	}

	for i := range table.Entries {
		entry := &table.Entries[i]
		if !source.PPM(entry.Chance) {
			continue
		}
		qty := source.Range(entry.QtyMin, entry.QtyMax)
		if qty <= 0 {
			continue
		}

		// Rolled here rather than at pickup, so the item exists the moment it
		// drops and two players examining the same corpse could not see
		// different loot.
		inst, err := r.items.Roll(source, entry.Item, victim.Mob.Def.Level, r.rarityWeights())
		if err != nil {
			r.log.Error("rolling a drop", "item", entry.Item, "err", err)
			continue
		}
		inst.Stack = qty

		spawn(DropState{ItemID: entry.Item, Qty: uint32(qty), Instance: inst})
	}
}

// phaseDrops expires ground loot that nobody collected.
func (r *Room) phaseDrops() {
	for _, e := range r.entities {
		if e.Drop == nil {
			continue
		}
		if r.tick >= e.Drop.ExpiresAt {
			r.removeEntity(e.ID)
		}
		// Drops fall to the ground rather than hovering where the mob died,
		// which matters when something is killed in mid-air.
		if !e.Body.Grounded {
			sim.Step(&e.Body, sim.Input{}, r.cfg.World, &r.cfg.Tuning)
		}
	}
}

// tryLoot handles a player's request to pick something up.
func (r *Room) tryLoot(player *Entity, dropID EntityID) {
	drop := r.entity(dropID)
	if drop == nil || drop.Drop == nil || player.Player == nil {
		return
	}

	// Layer visibility is the loot rule. Nothing else is needed: a drop in
	// another player's layer was never sent to this client, so a request for
	// it is either a stale ID or a forged one, and both are refused the same
	// way.
	if !canInteract(player, drop) {
		return
	}

	if drop.Drop.Owner != player.ID && r.tick < drop.Drop.UnlockAt {
		return
	}
	// Already asked for, with the write in flight. Silently ignoring a second
	// request beats granting the item twice.
	if drop.Drop.Claimed {
		return
	}

	gap := horizontalGap(player.Body.FeetCenter(), drop.Body.FeetCenter())
	vgap := verticalGap(player.Body.FeetCenter(), drop.Body.FeetCenter())
	reach := r.content.Balance.Drops.PickupRange
	if gap > reach || vgap > reach {
		return
	}

	// Gold has no inventory slot and cannot fail to be stored, so it is
	// granted immediately.
	if drop.Drop.Gold > 0 {
		player.Player.Gold += int64(drop.Drop.Gold)

		r.emitTo(player.ID, &mmov1.Event{Body: &mmov1.Event_LootTaken{LootTaken: &mmov1.LootTaken{
			EntityId: uint32(dropID),
			Gold:     drop.Drop.Gold,
		}}})
		r.removeEntity(dropID)
		return
	}

	p := r.players[player.ID]
	if drop.Drop.Instance == nil || p == nil || p.items == nil {
		// Nothing to store, or nowhere to store it. Leaving the drop rather
		// than silently consuming it means the loss is visible.
		return
	}

	// Claimed rather than removed. The item stays in the world until the
	// database confirms it, so a transient failure returns it instead of
	// destroying a drop the player just earned.
	drop.Drop.Claimed = true

	p.items.ClaimLoot(LootClaim{
		Player:      player.ID,
		CharacterID: p.characterID,
		DropID:      dropID,
		Instance:    drop.Drop.Instance,
		Tick:        r.tick,
	})
}

// resolveLoot completes a claim once persistence has finished.
func (r *Room) resolveLoot(dropID EntityID, playerID EntityID, granted bool, reason string) {
	drop := r.entity(dropID)
	if drop == nil || drop.Drop == nil {
		return
	}

	if !granted {
		// Returned to the ground, so the player can try again once whatever
		// blocked it clears.
		drop.Drop.Claimed = false
		if reason != "" {
			r.emitTo(playerID, &mmov1.Event{Body: &mmov1.Event_LootTaken{LootTaken: &mmov1.LootTaken{
				EntityId: uint32(dropID),
				Failed:   true,
				Reason:   reason,
			}}})
		}
		return
	}

	r.emitTo(playerID, &mmov1.Event{Body: &mmov1.Event_LootTaken{LootTaken: &mmov1.LootTaken{
		EntityId: uint32(dropID),
		ItemId:   drop.Drop.ItemID,
		Qty:      drop.Drop.Qty,
	}}})
	r.removeEntity(dropID)
}
