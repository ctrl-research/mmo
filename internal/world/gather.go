package world

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
)

// The session's half of gathering.
//
// The room rolls, grants the experience, and reports the yield; everything here
// is the work the tick loop must not do. Two different durabilities, on
// purpose:
//
//   - the *item* is written the moment it is gathered, because an item has to
//     exist somewhere and "in a tick loop's memory" is not somewhere;
//   - the *experience* rides the ordinary checkpoint, because a yield lands
//     every few seconds for as long as somebody keeps at it, and one write per
//     log would make woodcutting the busiest table in the database.
//
// The room is authoritative for the whole session either way. Nothing here
// decides anything about gathering; it stores what already happened.

// GrantGather receives a yield from the tick loop.
//
// Called mid-tick, so it must not block: it hands the yield to this session's
// own goroutine and returns immediately.
func (s *Session) GrantGather(yield room.GatherYield) {
	select {
	case s.gathers <- yield:
	default:
		// The queue is full, which means persistence is badly backed up. The
		// experience is already granted and cannot be taken back, so this
		// costs the player the item and says so -- the alternative is stalling
		// the room for everyone in it.
		s.log.Warn("dropping a gathered item; persistence is backed up",
			"skill", yield.Skill, "item", yield.ItemID)
		s.noteGather(yield, "you could not carry that")
	}
}

// handleGather stores one gathered item.
func (s *Session) handleGather(yield room.GatherYield) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	base, ok := s.node.content.Items[yield.ItemID]
	if !ok {
		// Content is verified at load, so this is a loader bug rather than
		// something a player can cause.
		s.log.Error("gathered an unknown item", "item", yield.ItemID)
		return
	}

	inst, err := s.inventory.Generator().RollBase(s.rolls, yield.ItemID, base.Level)
	if err != nil {
		s.log.Error("rolling a gathered item", "item", yield.ItemID, "err", err)
		return
	}
	if yield.Qty > 1 {
		inst.Stack = yield.Qty
	}

	if _, err := s.inventory.Grant(ctx, inst, yield.Tick); err != nil {
		switch {
		case errors.Is(err, ErrInventoryFull):
			// Said out loud, because a bag that silently swallows every log is
			// the worst possible version of this. The experience stands: the
			// swing landed and only storage failed, and taking the experience
			// back would make a full bag read as "my woodcutting stopped going
			// up" three hours later.
			s.noteGather(yield, "your inventory is full")
		default:
			s.log.Error("storing a gathered item", "item", yield.ItemID, "err", err)
			s.noteGather(yield, "you could not carry that")
		}
		return
	}

	s.pushInventory(ctx)
}

// noteGather tells the client that a yield could not be kept.
func (s *Session) noteGather(yield room.GatherYield, reason string) {
	s.sink.Send(&mmov1.ServerMessage{Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
		Body: &mmov1.Event_Gathering{Gathering: &mmov1.Gathering{
			Skill:  yield.Skill,
			Active: false,
			Reason: reason,
		}},
	}}})
}

// toolPower reads what the character has in hand, per secondary skill.
//
// Computed here rather than in the room for the same reason the stat block is:
// it is derived from equipment, and equipment lives where it can be written
// durably. Pushed with the block because it changes on exactly the same event.
//
// Only equipped items count. Carrying a spare axe should no more speed up
// woodcutting than carrying a spare sword makes anyone hit harder.
func (s *Session) toolPower() map[string]int {
	_, worn := s.inventory.Snapshot()

	var out map[string]int
	for _, slot := range worn {
		if slot == nil || slot.Instance == nil {
			continue
		}
		base, ok := s.node.content.Items[slot.Instance.BaseID]
		if !ok || base.Tool == nil {
			continue
		}
		if out == nil {
			out = make(map[string]int, 2)
		}
		// The best of them, in case two slots ever hold tools for one skill.
		// Summing would make two mediocre axes beat one good one, which is not
		// what "the tool in your hand" means.
		if base.Tool.Power > out[base.Tool.Skill] {
			out[base.Tool.Skill] = base.Tool.Power
		}
	}
	return out
}

// secondarySkills builds the full state for the skills panel.
//
// Every skill in content appears, including the ones at level 1: a panel with
// holes in it looks broken, and "you have never chopped a tree" and "this skill
// does not exist" are different things.
func (s *Session) secondarySkills(exp map[string]int64) *mmov1.SecondarySkills {
	c := s.node.content
	tools := s.toolPower()

	msg := &mmov1.SecondarySkills{}
	for _, id := range c.SecondaryOrder() {
		skill := c.Secondary[id]
		total := exp[id]
		level := c.Curves.SecondaryLevelFor(total)

		msg.Skills = append(msg.Skills, &mmov1.SecondarySkill{
			Skill:     id,
			Name:      skill.Name,
			Total:     uint64(total),
			Level:     uint32(level),
			LevelAt:   uint64(c.Curves.SecondaryExpAt(level)),
			NextAt:    uint64(c.Curves.SecondaryNextAt(level)),
			ToolName:  skill.ToolName,
			ToolPower: uint32(tools[id]),
		})
	}
	return msg
}

// characterSeed derives a stable roll stream from a character's identity.
//
// The same mixing the node uses for room seeds: the low bits of a UUID are
// well distributed but two characters created a moment apart share most of the
// high ones, and multiplying by the golden-ratio constant spreads them.
func characterSeed(id uuid.UUID) uint64 {
	var seed uint64
	for _, b := range id[:8] {
		seed = seed<<8 | uint64(b)
	}
	return seed * 0x9E3779B97F4A7C15
}

// pushSecondary sends the whole set of secondary skills to the client.
//
// A full state rather than a stream of gains, because a client that has just
// connected has no way to reconstruct totals from deltas it never saw. Gains
// after this arrive one at a time as SecondaryExp.
func (s *Session) pushSecondary(exp map[string]int64) {
	s.sink.Send(&mmov1.ServerMessage{Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
		Body: &mmov1.Event_Secondary{Secondary: s.secondarySkills(exp)},
	}}})
}
