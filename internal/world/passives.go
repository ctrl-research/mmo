package world

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ctrl-research/mmo/internal/content"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// The passive tree, as a character holds it.
//
// Allocation is server-side and every rule is checked here, because a client
// that could allocate anything could take every keystone in the game. The
// rules are few and they are the whole of the tree's design:
//
//   - You start at your class's node, which is free.
//   - A node is available only next to one you already hold, so reaching a
//     distant keystone means paying for the path to it.
//   - You may not hold more than your level has paid for.
//   - Refunding may not strand anything: taking a node out of the middle of a
//     path would leave everything beyond it held and unreachable, which is a
//     build nobody could have made by allocating.

// PointsPerLevel is how many passive points a level grants.
//
// One. The tree is sized so that a character at the cap has spent a meaningful
// fraction of it rather than most of it -- a tree you can fill is a tree with
// no choices left in it by the end.
const PointsPerLevel = 1

// PassiveRequest is a player changing their tree.
type PassiveRequest struct {
	// Allocate is the node to take, if any.
	Allocate int

	// Refund is the node to give back, if any.
	Refund int

	// RespecAll clears the tree and returns every point.
	RespecAll bool
}

// Passive queues a change to the passive tree.
func (s *Session) Passive(_ context.Context, req PassiveRequest) error {
	select {
	case s.passives <- req:
		return nil
	case <-s.done:
		return errors.New("world: session has ended")
	default:
		return errors.New("world: too many passive changes")
	}
}

// handlePassive applies one change, on the session goroutine.
func (s *Session) handlePassive(req PassiveRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.changePassives(ctx, req); err != nil {
		s.notify(mmov1.ChatChannel_CHAT_CHANNEL_UNSPECIFIED, err.Error())
		return
	}

	// The stat block is rebuilt from scratch, because removing a modifier from
	// a running product is lossy and an incremental path that drifts produces
	// stats that depend on the order nodes happened to be allocated.
	s.refreshStats(ctx, s.characterLevel(ctx))
	s.sendPassives(ctx)
}

func (s *Session) changePassives(ctx context.Context, req PassiveRequest) error {
	tree := s.node.content.Passives
	if tree == nil {
		return errors.New("there is no passive tree")
	}

	start, ok := tree.ClassStarts[s.classID]
	if !ok {
		return errors.New("your class has no place on the tree")
	}

	held, err := s.heldPassives(ctx, start)
	if err != nil {
		return err
	}

	switch {
	case req.RespecAll:
		return s.node.store.RefundAllPassives(ctx, s.characterID)

	case req.Refund != 0:
		return s.refundPassive(ctx, tree, start, held, req.Refund)

	case req.Allocate != 0:
		return s.allocatePassive(ctx, tree, held, req.Allocate)
	}
	return nil
}

// heldPassives reads what a character holds, including the free class start.
//
// The start is added here rather than written to the database: it is not a
// decision, it costs nothing, and a row for it would be a row that has to be
// created for every character and never removed.
func (s *Session) heldPassives(ctx context.Context, start int) (map[int]bool, error) {
	allocated, err := s.node.store.AllocatedPassives(ctx, s.characterID)
	if err != nil {
		return nil, errors.New("could not read your passives")
	}

	held := make(map[int]bool, len(allocated)+1)
	held[start] = true
	for _, id := range allocated {
		held[id] = true
	}
	return held, nil
}

func (s *Session) allocatePassive(ctx context.Context, tree *content.PassiveTree, held map[int]bool, node int) error {
	if held[node] {
		return errors.New("you already have that")
	}
	if !tree.Allocatable(node, held) {
		// Named rather than generic: "not next to anything you have" is
		// actionable, and "cannot allocate" is not.
		return errors.New("that is not next to anything you have taken")
	}

	spent, total, err := s.passivePoints(ctx)
	if err != nil {
		return err
	}
	if spent >= total {
		return fmt.Errorf("no passive points left; you have spent all %d", total)
	}

	if _, err := s.node.store.AllocatePassive(ctx, s.characterID, node); err != nil {
		s.log.Error("allocating a passive", "err", err)
		return errors.New("could not save that")
	}
	return nil
}

func (s *Session) refundPassive(ctx context.Context, tree *content.PassiveTree, start int, held map[int]bool, node int) error {
	if node == start {
		return errors.New("your starting node is not a choice")
	}
	if !held[node] {
		return errors.New("you do not have that")
	}

	// What would remain, if this went. Checked before writing rather than
	// after, so a refund that would strand half the tree is refused rather
	// than undone.
	remaining := make(map[int]bool, len(held))
	for id := range held {
		if id != node {
			remaining[id] = true
		}
	}
	if !tree.Connected(start, remaining) {
		return errors.New("taking that out would strand everything past it")
	}

	if _, err := s.node.store.RefundPassives(ctx, s.characterID, []int{node}); err != nil {
		s.log.Error("refunding a passive", "err", err)
		return errors.New("could not save that")
	}
	return nil
}

// passivePoints returns how many are spent and how many the character has.
func (s *Session) passivePoints(ctx context.Context) (spent, total int, err error) {
	allocated, err := s.node.store.AllocatedPassives(ctx, s.characterID)
	if err != nil {
		return 0, 0, errors.New("could not read your passives")
	}

	// From level, so the tree fills as a character grows rather than being a
	// separate currency to track.
	level := s.characterLevel(ctx)
	return len(allocated), (level - 1) * PointsPerLevel, nil
}

// passiveMods returns the stat modifiers a character's tree contributes.
//
// Read on every stat rebuild, which happens when equipment, level, or the tree
// changes -- not in the tick, and not per hit.
func (s *Session) passiveMods(ctx context.Context) []stats.Modifier {
	tree := s.node.content.Passives
	if tree == nil {
		return nil
	}

	allocated, err := s.node.store.AllocatedPassives(ctx, s.characterID)
	if err != nil {
		s.log.Error("reading passives for stats", "err", err)
		return nil
	}

	// The class start counts too: it is free, but it is not nothing.
	ids := allocated
	if start, ok := tree.ClassStarts[s.classID]; ok {
		ids = append(ids, start)
	}

	var out []stats.Modifier
	for _, id := range ids {
		node := tree.Nodes[id]
		if node == nil {
			// Content moved under a saved character. Skipping beats refusing
			// them entry over a node that no longer exists.
			continue
		}
		for _, m := range node.Mods {
			stat, ok := stats.Parse(m.Stat)
			if !ok {
				continue
			}
			out = append(out,
				stats.Modifier{Stat: stat, Kind: stats.Flat, Value: stats.FromInt(m.Flat)},
				stats.Modifier{Stat: stat, Kind: stats.Increased, Value: stats.Value(m.Increased)},
				stats.Modifier{Stat: stat, Kind: stats.More, Value: stats.Value(m.More)},
			)
		}
	}
	return out
}

// sendPassives tells the client what is allocated and what is affordable.
func (s *Session) sendPassives(ctx context.Context) {
	tree := s.node.content.Passives
	if tree == nil {
		return
	}

	allocated, err := s.node.store.AllocatedPassives(ctx, s.characterID)
	if err != nil {
		s.log.Error("reading passives", "err", err)
		return
	}

	spent, total, err := s.passivePoints(ctx)
	if err != nil {
		return
	}

	state := &mmov1.PassiveState{
		Allocated:   make([]uint32, 0, len(allocated)),
		SpentPoints: uint32(spent),
		TotalPoints: uint32(total),
	}
	for _, id := range allocated {
		state.Allocated = append(state.Allocated, uint32(id))
	}
	if start, ok := tree.ClassStarts[s.classID]; ok {
		state.StartNode = uint32(start)
	}

	s.deliver(&mmov1.ServerMessage{
		Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
			Body: &mmov1.Event_Passives{Passives: state},
		}},
	})
}
