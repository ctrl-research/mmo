package world

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/store"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
)

// The session's half of crafting.
//
// Crafting spends, and spending is where a mistake destroys something. The
// whole of this file exists to keep two properties:
//
//   - one run is one transaction. Consuming three bars and then failing to
//     insert the sword would destroy the bars, and retrying the insert alone
//     would duplicate the sword. Both are handled in the store, in one
//     statement sequence under one set of locks.
//   - the room learns the answer. It cannot see the inventory, so it cannot
//     know a run failed for want of materials, and a run that failed silently
//     would tick forever producing nothing.
//
// Running out of materials is the ordinary end of a crafting run, not an error.
// It travels back as `made = false` with a reason, and the room stops the
// action and says so.

// OpenStation receives a request for a station's menu from the tick loop.
//
// Called mid-tick, so it must not block.
func (s *Session) OpenStation(req room.StationRequest) {
	select {
	case s.stations <- req:
	default:
		// The queue is full, which means this player is spamming a station.
		// Dropping is right: the menu is a question, and an unanswered question
		// costs nothing but a second press.
	}
}

// RunCraft receives one run from the tick loop.
func (s *Session) RunCraft(req room.CraftRequest) {
	select {
	case s.crafts <- req:
	default:
		// Backed up. The room is waiting on an answer and will wait forever
		// without one, so this has to reply rather than drop.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		handle, _ := s.Where()
		handle.ResolveCraft(ctx, req.Player, false, "the server is busy; try again")
		cancel()
	}
}

// handleStation answers what a station can make.
func (s *Session) handleStation(req room.StationRequest) {
	c := s.node.content
	recipes := c.RecipesAt(req.Station.ID)

	// One read of the bag for the whole menu. Counting per recipe would read it
	// once per row for an answer that cannot change in between.
	held := s.materialCounts()

	menu := &mmov1.StationMenu{
		EntityId:  uint32(req.Entity),
		StationId: req.Station.ID,
		Name:      req.Station.Name,
	}

	for _, rec := range recipes {
		opt := &mmov1.RecipeOption{
			RecipeId:   rec.ID,
			Name:       rec.Name,
			Skill:      rec.Skill,
			Level:      uint32(rec.Level),
			Exp:        uint64(rec.Exp),
			OutputQty:  uint32(rec.Qty),
			OutputName: itemName(c, rec.Output),
		}

		for _, in := range rec.Inputs {
			opt.Inputs = append(opt.Inputs, &mmov1.RecipeIngredient{
				ItemId: in.Item,
				Name:   itemName(c, in.Item),
				Qty:    uint32(in.Qty),
				Held:   uint32(held[in.Item]),
			})
		}

		// Level first, then materials. A recipe a character is too low for is
		// something to work towards; one they merely lack the bar for is
		// something to go and get, and reporting the second when the first is
		// also true would send them mining for a sword they could not forge.
		switch {
		case req.Levels[rec.Skill] < rec.Level:
			skill := rec.Skill
			if def, ok := c.Secondary[rec.Skill]; ok {
				skill = def.Name
			}
			opt.Blocked = "needs " + skill + " level " + strconv.Itoa(rec.Level)
		default:
			for _, in := range rec.Inputs {
				if held[in.Item] < in.Qty {
					opt.Blocked = "needs " + strconv.Itoa(in.Qty) + " " + itemName(c, in.Item)
					break
				}
			}
		}

		menu.Recipes = append(menu.Recipes, opt)
	}

	s.sink.Send(&mmov1.ServerMessage{Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
		Body: &mmov1.Event_Station{Station: menu},
	}}})
}

// handleCraft carries out one run and tells the room what happened.
func (s *Session) handleCraft(req room.CraftRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handle, _ := s.Where()
	rec := req.Recipe

	made, reason := s.craftOnce(ctx, rec, req.Tick)
	handle.ResolveCraft(ctx, req.Player, made, reason)

	if made {
		s.pushInventory(ctx)
		// Equipment changes nothing here, but a crafted tool does: a smith who
		// just made a better axe has different tool power, and the skills panel
		// shows what is in hand. Cheaper to refresh than to work out whether
		// this particular output mattered.
		s.refreshStats(ctx, s.characterLevel(ctx))
	}
}

// craftOnce spends the inputs and stores the output, and reports why not.
func (s *Session) craftOnce(ctx context.Context, rec *content.Recipe, tick uint64) (bool, string) {
	c := s.node.content

	base, ok := c.Items[rec.Output]
	if !ok {
		// Content is verified at load, so this is a loader bug rather than
		// something a player can cause.
		s.log.Error("recipe produces an unknown item", "recipe", rec.ID, "item", rec.Output)
		return false, "that cannot be made"
	}

	// Rolled like any other item, from this session's own stream, so a crafted
	// sword has implicits and is reproducible from the logs. Base rather than a
	// full roll: crafting produces a plain item, and affixes on a crafted
	// weapon are a decision for whenever there is a way to influence them.
	inst, err := s.inventory.Generator().RollBase(s.rolls, rec.Output, base.Level)
	if err != nil {
		s.log.Error("rolling a crafted item", "recipe", rec.ID, "err", err)
		return false, "that cannot be made"
	}
	inst.Stack = rec.Qty

	inputs := make([]store.CraftInput, 0, len(rec.Inputs))
	for _, in := range rec.Inputs {
		inputs = append(inputs, store.CraftInput{BaseID: in.Item, Qty: in.Qty})
	}

	mods, err := json.Marshal(inst)
	if err != nil {
		s.log.Error("encoding a crafted item", "recipe", rec.ID, "err", err)
		return false, "that cannot be made"
	}

	_, err = s.node.store.Craft(ctx, s.inventory.InventoryID(), inputs, store.ItemRow{
		BaseID:    inst.BaseID,
		Rarity:    string(inst.Rarity),
		ItemLevel: inst.ItemLevel,
		Mods:      mods,
		StackSize: rec.Qty,
	}, base.MaxStack, s.characterID, tick)

	switch {
	case errors.Is(err, store.ErrMissingInputs):
		// The ordinary end of a run. Named rather than generic, because "you
		// have run out" and "something went wrong" are different things and a
		// player can act on only one of them.
		return false, "you have run out of materials"
	case errors.Is(err, store.ErrContainerFull):
		return false, "your inventory is full"
	case err != nil:
		s.log.Error("crafting", "recipe", rec.ID, "err", err)
		return false, "that could not be made"
	}

	// The in-memory view is rebuilt rather than patched: a craft can delete
	// several rows and add one, and patching that by hand is how a view comes
	// to disagree with the database about where an item is.
	if err := s.inventory.Reload(ctx); err != nil {
		s.log.Error("reloading after a craft", "err", err)
	}
	return true, ""
}

// materialCounts totals every stackable the character is carrying.
//
// Carried only, not equipped: a bar cannot be worn, and an equipped tool being
// counted as a material would let a recipe consume the axe out of a
// woodcutter's hands.
func (s *Session) materialCounts() map[string]int {
	carried, _ := s.inventory.Snapshot()

	out := make(map[string]int, len(carried))
	for _, slot := range carried {
		if slot == nil || slot.Instance == nil {
			continue
		}
		qty := slot.Instance.Stack
		if qty < 1 {
			qty = 1
		}
		out[slot.Instance.BaseID] += qty
	}
	return out
}

// itemName is an item's display name, or its id when content has no such item.
//
// The id rather than an empty string: a menu row reading "material.ghost" is a
// bug report, and a blank one is a mystery.
func itemName(c *content.Content, id string) string {
	if item, ok := c.Items[id]; ok {
		return item.Name
	}
	return id
}
