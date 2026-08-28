package room

import (
	"github.com/ctrl-research/mmo/internal/fixed"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
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
		spawn(DropState{ItemID: entry.Item, Qty: uint32(qty)})
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

	gap := horizontalGap(player.Body.FeetCenter(), drop.Body.FeetCenter())
	vgap := verticalGap(player.Body.FeetCenter(), drop.Body.FeetCenter())
	reach := r.content.Balance.Drops.PickupRange
	if gap > reach || vgap > reach {
		return
	}

	if drop.Drop.Gold > 0 {
		player.Player.Gold += int64(drop.Drop.Gold)
	}

	r.emitTo(player.ID, &mmov1.Event{Body: &mmov1.Event_LootTaken{LootTaken: &mmov1.LootTaken{
		EntityId: uint32(dropID),
		ItemId:   drop.Drop.ItemID,
		Qty:      drop.Drop.Qty,
		Gold:     drop.Drop.Gold,
	}}})

	// Inventory arrives in M3. Until then an item drop is consumed and
	// acknowledged so the pickup path is exercised end to end; only gold has
	// somewhere to go.
	r.removeEntity(dropID)
}
