package world

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/store"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
)

// The skill bar.
//
// What a character can cast, and what is linked to it. The session owns this
// for the same reason it owns the inventory: it lives in the database, and a
// room that read it would be a room that knows about persistence.
//
// Every rule about what may go where is enforced here rather than in the
// client, because a client that could put any skill in any slot with any
// support attached could cast a mob ability with every support in the game
// linked to it.

// LoadoutRequest is a player rearranging their bar.
type LoadoutRequest struct {
	Slot int

	// SkillID empty clears the slot.
	SkillID string

	// Supports are the modifiers to link, in the order they apply.
	Supports []string
}

// SetBarSlot queues a change to the skill bar.
func (s *Session) SetBarSlot(_ context.Context, req LoadoutRequest) error {
	select {
	case s.loadouts <- req:
		return nil
	case <-s.done:
		return errors.New("world: session has ended")
	default:
		return errors.New("world: too many loadout changes")
	}
}

// handleLoadout validates and applies one bar change, on the session goroutine.
func (s *Session) handleLoadout(req LoadoutRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.validateSlot(ctx, req); err != nil {
		s.notify(mmov1.ChatChannel_CHAT_CHANNEL_UNSPECIFIED, err.Error())
		return
	}

	if err := s.node.store.SetBarSlot(ctx, s.characterID, req.Slot, req.SkillID, req.Supports); err != nil {
		s.log.Error("saving the skill bar", "err", err)
		s.notify(mmov1.ChatChannel_CHAT_CHANNEL_UNSPECIFIED, "could not save that")
		return
	}

	s.pushLoadout(ctx)
}

// validateSlot checks one bar change against what the character may actually
// do.
func (s *Session) validateSlot(ctx context.Context, req LoadoutRequest) error {
	if req.Slot < 0 || req.Slot >= store.MaxSkillSlots {
		return fmt.Errorf("there is no slot %d", req.Slot)
	}
	if req.SkillID == "" {
		return nil
	}

	skill, ok := s.node.content.Skills[req.SkillID]
	if !ok {
		return errors.New("no such skill")
	}

	known, err := s.node.store.LearnedSkills(ctx, s.characterID)
	if err != nil {
		return errors.New("could not read your skills")
	}
	if !knows(known, req.SkillID) {
		return fmt.Errorf("you have not learned %s", skill.Name)
	}

	if len(req.Supports) > MaxSupportsPerSkill {
		return fmt.Errorf("a skill takes at most %d supports", MaxSupportsPerSkill)
	}

	seen := make(map[string]bool, len(req.Supports))
	for _, id := range req.Supports {
		support, ok := s.node.content.Supports[id]
		if !ok {
			return errors.New("no such support")
		}
		if seen[id] {
			// The same support twice would multiply its effect while costing
			// one link, which is the cheapest possible exploit.
			return fmt.Errorf("%s is already linked", support.Name)
		}
		seen[id] = true

		// The tags are the rule that makes a support a choice. Refusing here
		// rather than silently dropping it at cast time means a player finds
		// out when they try, not when the damage looks wrong.
		if !support.Attaches(skill) {
			return fmt.Errorf("%s does not fit %s", support.Name, skill.Name)
		}
	}
	return nil
}

// MaxSupportsPerSkill bounds how many modifiers one skill may carry.
//
// Bounded because supports multiply: a skill with unlimited links is a skill
// whose damage is unbounded, and because resolving them is work done whenever
// a bar changes.
const MaxSupportsPerSkill = 4

// loadLoadout reads a character's bar, seeding it on first login.
func (s *Session) loadLoadout(ctx context.Context, classID string) ([]room.LoadoutSlot, error) {
	known, err := s.node.store.LearnedSkills(ctx, s.characterID)
	if err != nil {
		return nil, err
	}

	// A character who has never played knows nothing, which would leave them
	// unable to act. Their class says what they start with.
	if len(known) == 0 {
		known, err = s.grantStartingSkills(ctx, classID)
		if err != nil {
			return nil, err
		}
	}

	bar, err := s.node.store.SkillBar(ctx, s.characterID)
	if err != nil {
		return nil, err
	}
	if len(bar) == 0 {
		bar, err = s.seedBar(ctx, known)
		if err != nil {
			return nil, err
		}
	}

	ranks := make(map[string]int, len(known))
	for _, k := range known {
		ranks[k.SkillID] = k.Rank
	}

	out := make([]room.LoadoutSlot, 0, len(bar))
	for _, slot := range bar {
		out = append(out, room.LoadoutSlot{
			SkillID:  slot.SkillID,
			Rank:     ranks[slot.SkillID],
			Supports: slot.Supports,
		})
	}
	return out, nil
}

// grantStartingSkills gives a new character what their class begins with.
func (s *Session) grantStartingSkills(ctx context.Context, classID string) ([]store.LearnedSkill, error) {
	class, ok := s.node.content.Classes[classID]
	if !ok {
		// An unknown class is a character created before the class existed, or
		// content that has moved on. Falling back beats refusing them entry.
		s.log.Warn("character has an unknown class", "class", classID)
		class = anyClass(s.node.content)
		if class == nil {
			return nil, nil
		}
	}

	if err := s.node.store.LearnSkills(ctx, s.characterID, class.StartingSkills); err != nil {
		return nil, err
	}
	return s.node.store.LearnedSkills(ctx, s.characterID)
}

// seedBar puts a character's skills on the bar in the order their class lists
// them, so a new player has something under their fingers.
func (s *Session) seedBar(ctx context.Context, known []store.LearnedSkill) ([]store.BarSlot, error) {
	ids := make([]string, 0, len(known))
	for _, k := range known {
		ids = append(ids, k.SkillID)
	}

	if err := s.node.store.SeedSkillBar(ctx, s.characterID, ids); err != nil {
		return nil, err
	}
	return s.node.store.SkillBar(ctx, s.characterID)
}

// pushLoadout sends the bar to the room and to the client.
func (s *Session) pushLoadout(ctx context.Context) {
	slots, err := s.loadLoadout(ctx, s.classID)
	if err != nil {
		s.log.Error("reading the skill bar", "err", err)
		return
	}

	handle, entityID := s.Where()
	if handle != nil {
		handle.SetLoadout(ctx, entityID, slots)
	}
	s.sendLoadout(ctx, slots)
}

// sendLoadout tells the client what is on the bar and what it costs.
func (s *Session) sendLoadout(ctx context.Context, slots []room.LoadoutSlot) {
	known, err := s.node.store.LearnedSkills(ctx, s.characterID)
	if err != nil {
		s.log.Error("reading skills", "err", err)
		return
	}

	bar := &mmov1.SkillBar{}
	for _, slot := range slots {
		skill, ok := s.node.content.Skills[slot.SkillID]
		if !ok {
			continue
		}
		bar.Slots = append(bar.Slots, &mmov1.SkillSlot{
			SkillId:    slot.SkillID,
			Name:       skill.Name,
			Rank:       uint32(slot.Rank),
			CooldownMs: uint32(skill.Cooldown) * uint32(room.TickPeriod.Milliseconds()),
			// The cost after supports, because that is what will actually be
			// charged and a bar showing the base cost is a bar that lies.
			CostMp:   uint32(s.linkedCost(skill, slot.Supports)),
			Supports: slot.Supports,
		})
	}

	for _, k := range known {
		skill, ok := s.node.content.Skills[k.SkillID]
		if !ok {
			continue
		}
		bar.Known = append(bar.Known, &mmov1.KnownSkill{
			SkillId: k.SkillID,
			Name:    skill.Name,
			Rank:    uint32(k.Rank),
			MaxRank: uint32(skill.MaxRank),
			Tags:    skill.Tags,
		})
	}

	for id, support := range s.node.content.Supports {
		bar.Supports = append(bar.Supports, &mmov1.SupportInfo{
			SupportId: id,
			Name:      support.Name,
			Tags:      support.Tags,
		})
	}

	s.deliver(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_SkillBar{SkillBar: bar},
		}},
	})
}

// linkedCost is what a skill costs with its supports attached.
func (s *Session) linkedCost(skill *content.Skill, supportIDs []string) int {
	cost := skill.CostMP
	for _, id := range supportIDs {
		support, ok := s.node.content.Supports[id]
		if !ok || !support.Attaches(skill) {
			continue
		}
		cost = fixed.FromInt(cost).Mul(support.ManaMult).Int()
	}
	return cost
}

func knows(known []store.LearnedSkill, skillID string) bool {
	for _, k := range known {
		if k.SkillID == skillID {
			return true
		}
	}
	return false
}

// anyClass returns some class, deterministically, for a character whose own no
// longer exists.
func anyClass(c *content.Content) *content.Class {
	var best *content.Class
	for _, class := range c.Classes {
		if best == nil || class.ID < best.ID {
			best = class
		}
	}
	return best
}
