package world

import (
	"context"
	"testing"

	"time"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/stats"
	"github.com/google/uuid"
)

// The passive tree, against a real character.
//
// Every rule here is enforced server-side, because a client that could
// allocate anything could take every keystone in the game. These check that
// each refusal actually refuses, and that the one thing the tree is for --
// changing how a character plays -- reaches the stat block.

// atLevel returns a session for a character at a given level.
//
// The level has to go through a logout: it is the *room* that holds a live
// character's level, loaded at join, so writing it to the database under a
// running session changes nothing until they come back.
func (c *cluster) atLevel(t *testing.T, level int, name string) (*Session, *captureSink, uuid.UUID) {
	t.Helper()

	account, character := c.character(name)
	ctx := context.Background()

	// Enter and leave, so the checkpoint below is not overwritten by the one
	// Close writes.
	first, _ := c.enter(c.a, account, character)
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	first.Close(closeCtx)
	cancel()

	saved, err := c.store.LoadCharacter(ctx, account, character)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	saved.Level = level
	if err := c.store.Checkpoint(ctx, saved, first.lease.Token); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	s, sink := c.enter(c.a, account, character)
	if got := s.characterLevel(ctx); got != level {
		t.Fatalf("character came back at level %d, want %d", got, level)
	}
	return s, sink, character
}

// tree is the loaded passive tree.
func (c *cluster) tree() (start int, neighbour int, distant int) {
	t := c.game.Passives
	start = t.ClassStarts["warrior"]
	neighbour = t.Adjacency[start][0]

	for _, id := range t.Order {
		if id != start && id != neighbour && !t.Adjacent(id, start) {
			distant = id
			break
		}
	}
	return start, neighbour, distant
}

func TestAllocatingNeedsANeighbour(t *testing.T) {
	c := newCluster(t)
	s, sink, character := c.atLevel(t, 10, "Tree")

	_, neighbour, distant := c.tree()
	ctx := context.Background()

	// Not next to anything held: refused, and told why.
	if err := s.Passive(ctx, PassiveRequest{Allocate: distant}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	eventually(t, "the refusal", func() bool {
		for _, msg := range sink.system() {
			if msg == "that is not next to anything you have taken" {
				return true
			}
		}
		return false
	})

	// Next to the start: allowed.
	if err := s.Passive(ctx, PassiveRequest{Allocate: neighbour}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	eventually(t, "the node to be allocated", func() bool {
		held, _ := c.store.AllocatedPassives(ctx, character)
		return len(held) == 1 && held[0] == neighbour
	})
}

// Points come from levels, and a character cannot hold more than they have
// paid for.
func TestAllocationIsBoundedByLevel(t *testing.T) {
	c := newCluster(t)
	// Level one: no points at all, which is the bound being tested.
	s, sink, character := c.atLevel(t, 1, "Broke")

	ctx := context.Background()
	start := c.game.Passives.ClassStarts["warrior"]

	if err := s.Passive(ctx, PassiveRequest{
		Allocate: c.game.Passives.Adjacency[start][0],
	}); err != nil {
		t.Fatalf("queue: %v", err)
	}

	eventually(t, "the refusal", func() bool {
		for _, msg := range sink.system() {
			if msg == "no passive points left; you have spent all 0" {
				return true
			}
		}
		return false
	})

	held, _ := c.store.AllocatedPassives(ctx, character)
	if len(held) != 0 {
		t.Errorf("a level-one character allocated %d nodes", len(held))
	}
}

// Taking a node out of the middle of a path would leave everything beyond it
// held and unreachable -- a build nobody could have made by allocating.
func TestRefundingCannotStrandNodes(t *testing.T) {
	c := newCluster(t)
	s, sink, character := c.atLevel(t, 20, "Pruner")

	ctx := context.Background()
	tree := c.game.Passives
	start := tree.ClassStarts["warrior"]

	// Walk three nodes out from the start.
	path := []int{}
	held := map[int]bool{start: true}
	previous := start
	for len(path) < 3 {
		var next int
		for _, candidate := range tree.Adjacency[previous] {
			if !held[candidate] {
				next = candidate
				break
			}
		}
		if next == 0 {
			t.Fatal("could not walk out from the start")
		}

		if err := s.Passive(ctx, PassiveRequest{Allocate: next}); err != nil {
			t.Fatalf("queue: %v", err)
		}
		want := len(path) + 1
		eventually(t, "the node to be allocated", func() bool {
			got, _ := c.store.AllocatedPassives(ctx, character)
			return len(got) == want
		})

		held[next] = true
		path = append(path, next)
		previous = next
	}

	// Refunding the middle is refused.
	if err := s.Passive(ctx, PassiveRequest{Refund: path[0]}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	eventually(t, "the refusal", func() bool {
		for _, msg := range sink.system() {
			if msg == "taking that out would strand everything past it" {
				return true
			}
		}
		return false
	})

	if got, _ := c.store.AllocatedPassives(ctx, character); len(got) != 3 {
		t.Errorf("holding %d nodes after a refused refund, want 3", len(got))
	}

	// Refunding the end is allowed.
	if err := s.Passive(ctx, PassiveRequest{Refund: path[2]}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	eventually(t, "the leaf to be refunded", func() bool {
		got, _ := c.store.AllocatedPassives(ctx, character)
		return len(got) == 2
	})
}

func TestTheStartNodeCannotBeRefunded(t *testing.T) {
	c := newCluster(t)
	s, sink, _ := c.atLevel(t, 5, "Rooted")

	start := c.game.Passives.ClassStarts["warrior"]
	if err := s.Passive(context.Background(), PassiveRequest{Refund: start}); err != nil {
		t.Fatalf("queue: %v", err)
	}

	eventually(t, "the refusal", func() bool {
		for _, msg := range sink.system() {
			if msg == "your starting node is not a choice" {
				return true
			}
		}
		return false
	})
}

func TestRespecReturnsEveryPoint(t *testing.T) {
	c := newCluster(t)
	s, _, character := c.atLevel(t, 20, "Rethink")

	ctx := context.Background()
	tree := c.game.Passives
	start := tree.ClassStarts["warrior"]

	if err := s.Passive(ctx, PassiveRequest{Allocate: tree.Adjacency[start][0]}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	eventually(t, "a node to be allocated", func() bool {
		got, _ := c.store.AllocatedPassives(ctx, character)
		return len(got) == 1
	})

	if err := s.Passive(ctx, PassiveRequest{RespecAll: true}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	eventually(t, "the tree to be cleared", func() bool {
		got, _ := c.store.AllocatedPassives(ctx, character)
		return len(got) == 0
	})
}

// The whole point of the tree: allocating changes how a character plays.
func TestAllocatingChangesTheStatBlock(t *testing.T) {
	c := newCluster(t)
	s, sink, character := c.atLevel(t, 20, "Stronger")

	ctx := context.Background()
	tree := c.game.Passives
	start := tree.ClassStarts["warrior"]

	// A node that gives strength, so the change is visible in one stat.
	var target int
	for _, id := range tree.Adjacency[start] {
		for _, m := range tree.Nodes[id].Mods {
			if m.Stat == "strength" && m.Flat > 0 {
				target = id
			}
		}
	}
	if target == 0 {
		t.Skip("no strength node borders the warrior start in this tree")
	}

	before := statFromInventory(s, stats.Strength)

	if err := s.Passive(ctx, PassiveRequest{Allocate: target}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	eventually(t, "the node to be allocated", func() bool {
		got, _ := c.store.AllocatedPassives(ctx, character)
		return len(got) == 1
	})

	after := statFromInventory(s, stats.Strength)
	if after <= before {
		t.Errorf("strength went from %v to %v after allocating a strength node; "+
			"the tree is bookkeeping if it does not reach the stat block",
			before, after)
	}

	// And the client is told what it holds and what it can afford.
	eventually(t, "the client to be told", func() bool {
		for _, p := range sink.passiveStates() {
			if len(p.GetAllocated()) == 1 && p.GetTotalPoints() > 0 {
				return true
			}
		}
		return false
	})
}

// statFromInventory computes one stat the way refreshStats does.
func statFromInventory(s *Session, stat stats.StatID) stats.Value {
	ctx := context.Background()

	block := s.inventory.StatBlock(s.characterLevel(ctx))
	block.AddAll(s.passiveMods(ctx))
	return block.Value(stat)
}

var _ = mmov1.PassiveState{}
