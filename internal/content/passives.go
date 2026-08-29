package content

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"

	"github.com/ctrl-research/mmo/internal/world/stats"
)

// The passive tree.
//
// The one piece of content too large to hand-author, and the one that decides
// whether two characters of the same class play differently. It is generated
// by cmd/treegen and checked in, the same arrangement as the golden movement
// fixtures and the sprites: the generator is the source, the JSON is the
// output, and the diff is reviewed.
//
// Everything here is validated at load, because every failure mode is silent
// at runtime: a disconnected node is one nobody can ever reach, a class start
// that does not exist is a character who can allocate nothing, and a stat name
// with a typo is a passive that does nothing at all.

// NodeKind is what a passive node is worth.
type NodeKind string

const (
	// NodeSmall is one modest modifier. Most of the tree, and most of what
	// travelling across it costs.
	NodeSmall NodeKind = "small"

	// NodeNotable is a named cluster of modifiers worth going out of the way
	// for. These are what a build is described in terms of.
	NodeNotable NodeKind = "notable"

	// NodeKeystone changes how a character works, with a real drawback. The
	// drawback is what makes it a choice rather than a strict upgrade -- a
	// keystone everybody takes is a keystone that should have been a notable.
	NodeKeystone NodeKind = "keystone"

	// NodeStart is where a class begins. Allocated for free and never
	// refunded, because it is not a decision.
	NodeStart NodeKind = "start"
)

// PassiveNode is one node on the tree.
type PassiveNode struct {
	ID   int
	Kind NodeKind

	// Name is shown for notables and keystones. Small nodes are described by
	// their modifiers, because a hundred names nobody reads is a hundred
	// names to maintain.
	Name string

	// X and Y place it on the tree screen. Layout is content rather than
	// something the client computes, so every player sees the same tree and a
	// build can be described by pointing at it.
	X, Y int

	Mods []StatMod
}

// PassiveTree is the whole graph.
type PassiveTree struct {
	Nodes map[int]*PassiveNode

	// Adjacency is the edge list as a lookup, both ways. Allocation asks "is
	// this next to something I have" on every click, and that question should
	// not be a scan.
	Adjacency map[int][]int

	// ClassStarts maps a class to the node it begins at.
	ClassStarts map[string]int

	// Order is every node id, sorted, so anything that iterates the tree does
	// so identically every time.
	Order []int
}

type treeFile struct {
	Nodes []struct {
		ID   int    `json:"id"`
		Kind string `json:"kind"`
		Name string `json:"name"`
		Pos  [2]int `json:"pos"`

		Stats []struct {
			Stat      string  `json:"stat"`
			Flat      int     `json:"flat"`
			Increased float64 `json:"increased"`
			More      float64 `json:"more"`
		} `json:"stats"`
	} `json:"nodes"`

	Edges       [][2]int       `json:"edges"`
	ClassStarts map[string]int `json:"class_starts"`
}

func (c *Content) loadPassives(fsys fs.FS, rec *hashRecorder) error {
	data, err := rec.readAndRecord("passives/tree.json")
	if err != nil {
		return err
	}

	var f treeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("passives/tree.json: %w", err)
	}

	tree := &PassiveTree{
		Nodes:       make(map[int]*PassiveNode, len(f.Nodes)),
		Adjacency:   make(map[int][]int, len(f.Nodes)),
		ClassStarts: f.ClassStarts,
	}

	for _, raw := range f.Nodes {
		if _, dup := tree.Nodes[raw.ID]; dup {
			return fmt.Errorf("passives: node %d appears twice", raw.ID)
		}

		kind := NodeKind(raw.Kind)
		switch kind {
		case NodeSmall, NodeNotable, NodeKeystone, NodeStart:
		default:
			return fmt.Errorf("passives: node %d has unknown kind %q", raw.ID, raw.Kind)
		}
		if kind != NodeStart && len(raw.Stats) == 0 {
			return fmt.Errorf("passives: node %d has no modifiers, so allocating it "+
				"would cost a point and do nothing", raw.ID)
		}

		node := &PassiveNode{
			ID:   raw.ID,
			Kind: kind,
			Name: raw.Name,
			X:    raw.Pos[0],
			Y:    raw.Pos[1],
		}

		for _, m := range raw.Stats {
			if _, ok := stats.Parse(m.Stat); !ok {
				return fmt.Errorf("passives: node %d modifies unknown stat %q", raw.ID, m.Stat)
			}
			node.Mods = append(node.Mods, StatMod{
				Stat:      m.Stat,
				Flat:      m.Flat,
				Increased: modifierToPPM(m.Increased),
				More:      modifierToPPM(m.More),
			})
		}

		tree.Nodes[raw.ID] = node
		tree.Order = append(tree.Order, raw.ID)
	}

	sort.Ints(tree.Order)

	for _, edge := range f.Edges {
		a, b := edge[0], edge[1]
		if _, ok := tree.Nodes[a]; !ok {
			return fmt.Errorf("passives: edge references unknown node %d", a)
		}
		if _, ok := tree.Nodes[b]; !ok {
			return fmt.Errorf("passives: edge references unknown node %d", b)
		}
		if a == b {
			return fmt.Errorf("passives: node %d is linked to itself", a)
		}
		tree.Adjacency[a] = append(tree.Adjacency[a], b)
		tree.Adjacency[b] = append(tree.Adjacency[b], a)
	}

	// Sorted, so allocation and rendering walk the graph in the same order
	// every time. Go randomises nothing here, but an edge list that arrives in
	// file order would make the tree depend on how it was generated.
	for id := range tree.Adjacency {
		sort.Ints(tree.Adjacency[id])
	}

	c.Passives = tree
	return nil
}

// validatePassives checks the properties that are silent at runtime.
func (c *Content) validatePassives() error {
	tree := c.Passives
	if tree == nil || len(tree.Nodes) == 0 {
		return fmt.Errorf("passives: the tree is empty")
	}

	for classID := range c.Classes {
		start, ok := tree.ClassStarts[classID]
		if !ok {
			return fmt.Errorf("passives: class %q has no start node", classID)
		}
		node, ok := tree.Nodes[start]
		if !ok {
			return fmt.Errorf("passives: class %q starts at node %d, which does not exist",
				classID, start)
		}
		if node.Kind != NodeStart {
			return fmt.Errorf("passives: class %q starts at node %d, which is a %s",
				classID, start, node.Kind)
		}
	}

	for classID, start := range tree.ClassStarts {
		if _, ok := c.Classes[classID]; !ok {
			return fmt.Errorf("passives: tree has a start for unknown class %q", classID)
		}

		// Every node must be reachable from every start, or part of the tree
		// is invisible to some classes -- which is a design decision nobody
		// made, and which shows up as a build that works for one class and
		// silently cannot exist for another.
		reached := tree.reachable(start)
		if len(reached) != len(tree.Nodes) {
			var stranded []int
			for _, id := range tree.Order {
				if !reached[id] {
					stranded = append(stranded, id)
				}
			}
			if len(stranded) > 8 {
				stranded = stranded[:8]
			}
			return fmt.Errorf("passives: %d of %d nodes are unreachable from the %s "+
				"start, including %v", len(tree.Nodes)-len(reached), len(tree.Nodes),
				classID, stranded)
		}
	}

	return nil
}

// reachable returns every node connected to a starting point.
func (t *PassiveTree) reachable(from int) map[int]bool {
	seen := map[int]bool{from: true}
	queue := []int{from}

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		for _, next := range t.Adjacency[id] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return seen
}

// Adjacent reports whether two nodes are linked.
func (t *PassiveTree) Adjacent(a, b int) bool {
	for _, next := range t.Adjacency[a] {
		if next == b {
			return true
		}
	}
	return false
}

// Allocatable reports whether a node may be taken given what is already held.
//
// The rule is the whole of the tree's shape: a node is available only next to
// one you already have, so reaching a distant keystone means paying for the
// path to it. That is what makes the tree a set of routes rather than a menu.
func (t *PassiveTree) Allocatable(node int, held map[int]bool) bool {
	if held[node] {
		return false
	}
	if _, ok := t.Nodes[node]; !ok {
		return false
	}

	for _, next := range t.Adjacency[node] {
		if held[next] {
			return true
		}
	}
	return false
}

// Connected reports whether a set of allocated nodes still hangs together from
// a start.
//
// Used when refunding: taking a node out of the middle of a path would leave
// everything beyond it allocated but unreachable, which is a build nobody could
// have made by allocating.
func (t *PassiveTree) Connected(start int, held map[int]bool) bool {
	if !held[start] {
		return false
	}

	seen := map[int]bool{start: true}
	queue := []int{start}

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		for _, next := range t.Adjacency[id] {
			if held[next] && !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return len(seen) == len(held)
}
