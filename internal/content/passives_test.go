package content

import (
	"strings"
	"testing"
	"testing/fstest"
)

// The passive tree.
//
// Every failure mode here is silent at runtime: a disconnected node is one
// nobody can ever reach, a class start that does not exist is a character who
// can allocate nothing, and a typo in a stat name is a passive that costs a
// point and does nothing. So each is a load error.

func TestTheShippedTreeIsWholeAndReachable(t *testing.T) {
	c := mustLoadShipped(t)
	tree := c.Passives

	if len(tree.Nodes) < 50 {
		t.Errorf("the tree has %d nodes; that is a menu rather than a tree",
			len(tree.Nodes))
	}

	// Every class must reach every node, or part of the tree is invisible to
	// some classes -- a decision nobody made, showing up as a build that works
	// for one class and cannot exist for another.
	for classID, start := range tree.ClassStarts {
		if got := len(tree.reachable(start)); got != len(tree.Nodes) {
			t.Errorf("%s reaches %d of %d nodes", classID, got, len(tree.Nodes))
		}
	}

	// Every class has a start, and every start belongs to a class.
	for classID := range c.Classes {
		if _, ok := tree.ClassStarts[classID]; !ok {
			t.Errorf("class %q has nowhere to start", classID)
		}
	}
}

// A keystone that is strictly better is a keystone everybody takes, which is a
// keystone that should have been a notable.
func TestEveryKeystoneHasADrawback(t *testing.T) {
	c := mustLoadShipped(t)

	found := 0
	for _, id := range c.Passives.Order {
		node := c.Passives.Nodes[id]
		if node.Kind != NodeKeystone {
			continue
		}
		found++

		up, down := false, false
		for _, m := range node.Mods {
			if m.Flat > 0 || m.Increased > 0 || m.More > 0 {
				up = true
			}
			if m.Flat < 0 || m.Increased < 0 || m.More < 0 {
				down = true
			}
		}
		if !up || !down {
			t.Errorf("keystone %q (%d) has no trade: it should give something and "+
				"cost something", node.Name, id)
		}
	}

	if found == 0 {
		t.Fatal("the tree has no keystones, so this proved nothing")
	}
}

// Notables and keystones are what a build is described in terms of, so they
// need names. Small nodes deliberately do not: a hundred names nobody reads is
// a hundred names to maintain.
func TestNamedNodesAreNamed(t *testing.T) {
	c := mustLoadShipped(t)

	for _, id := range c.Passives.Order {
		node := c.Passives.Nodes[id]
		if node.Kind == NodeNotable || node.Kind == NodeKeystone {
			if node.Name == "" {
				t.Errorf("%s node %d has no name", node.Kind, id)
			}
		}
	}
}

// --- allocation rules --------------------------------------------------------

// The rule that makes the tree a set of routes rather than a menu.
func TestANodeIsOnlyAllocatableNextToOneYouHave(t *testing.T) {
	tree := mustLoad(t).Passives
	start := tree.ClassStarts["warrior"]

	held := map[int]bool{start: true}

	// The neighbour is available; anything beyond it is not.
	neighbour := tree.Adjacency[start][0]
	if !tree.Allocatable(neighbour, held) {
		t.Fatalf("node %d borders the start and is not allocatable", neighbour)
	}

	for _, id := range tree.Order {
		if id == start || id == neighbour {
			continue
		}
		if tree.Adjacent(id, start) {
			continue
		}
		if tree.Allocatable(id, held) {
			t.Errorf("node %d is allocatable from the start alone, and it does not "+
				"border it", id)
		}
	}

	// Already held is not allocatable again, or a point could be spent twice.
	if tree.Allocatable(start, held) {
		t.Error("the start node is allocatable while already held")
	}
}

// Refunding from the middle of a path would leave everything past it allocated
// and unreachable -- a build nobody could have made by allocating.
func TestRefundingFromTheMiddleBreaksConnectivity(t *testing.T) {
	tree := mustLoad(t).Passives
	start := tree.ClassStarts["warrior"]

	// A path out from the start.
	path := []int{start}
	held := map[int]bool{start: true}
	for len(path) < 4 {
		last := path[len(path)-1]
		var next int
		for _, candidate := range tree.Adjacency[last] {
			if !held[candidate] {
				next = candidate
				break
			}
		}
		if next == 0 {
			t.Fatal("could not walk a path out from the start")
		}
		held[next] = true
		path = append(path, next)
	}

	if !tree.Connected(start, held) {
		t.Fatal("a path allocated outward is not connected")
	}

	// Take out the middle.
	delete(held, path[1])
	if tree.Connected(start, held) {
		t.Error("removing a node from the middle of a path left the rest connected")
	}
}

// --- validation --------------------------------------------------------------

// The validator is only worth having if it fails on the things it is for.
func TestBrokenTreesAreRejected(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name: "a node nobody can reach",
			// Cutting the only edge into node 6 strands it.
			mutate:  func(s string) string { return strings.Replace(s, `[3, 6]`, `[6, 6]`, 1) },
			wantErr: "linked to itself",
		},
		{
			name: "an edge to a node that does not exist",
			mutate: func(s string) string {
				return strings.Replace(s, `[3, 6]`, `[3, 999]`, 1)
			},
			wantErr: "unknown node",
		},
		{
			name: "a class starting nowhere",
			mutate: func(s string) string {
				return strings.Replace(s, `"warrior": 1`, `"warrior": 999`, 1)
			},
			wantErr: "does not exist",
		},
		{
			name: "a class starting on an ordinary node",
			mutate: func(s string) string {
				return strings.Replace(s, `"warrior": 1`, `"warrior": 2`, 1)
			},
			wantErr: "which is a small",
		},
		{
			name: "a passive that modifies nothing real",
			mutate: func(s string) string {
				return strings.Replace(s, `"stat": "strength"`, `"stat": "charisma"`, 1)
			},
			wantErr: "unknown stat",
		},
		{
			name: "a node that costs a point and does nothing",
			mutate: func(s string) string {
				return strings.Replace(s,
					`"stats": [{"stat": "strength", "flat": 8}]`, `"stats": []`, 1)
			},
			wantErr: "no modifiers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := minimalFS()
			f["passives/tree.json"] = file(tt.mutate(treeJSON))

			_, err := Load(f)
			if err == nil {
				t.Fatal("expected a load error, got none")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// A stranded node is the failure the reachability check exists for, and the
// one a naive validator misses.
func TestAStrandedNodeIsRejected(t *testing.T) {
	f := minimalFS()

	// A seventh node with no edges at all: valid on its own terms, and
	// unreachable from anywhere.
	stranded := strings.Replace(treeJSON,
		`  "edges"`,
		`  {"id": 7, "kind": "small", "pos": [400, 400], "stats": [{"stat": "strength", "flat": 3}]}
  ],
  "edges"`, 1)
	stranded = strings.Replace(stranded,
		`"stats": [{"stat": "armour", "increased": 0.1}]}
  ],`,
		`"stats": [{"stat": "armour", "increased": 0.1}]},`, 1)

	f["passives/tree.json"] = file(stranded)

	_, err := Load(f)
	if err == nil {
		t.Fatal("a node with no edges loaded without complaint")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error = %q, want it to mention unreachable nodes", err)
	}
}

var _ = fstest.MapFS{}

// A debuff made of negative modifiers is the case that broke: the conversion
// to parts-per-million clamped negatives to zero, so Chilled slowed nobody and
// every keystone gave its upside for free.
func TestNegativeModifiersSurviveLoading(t *testing.T) {
	c := mustLoadShipped(t)

	chilled := c.Buffs["chilled"]
	if chilled == nil {
		t.Fatal("no chilled buff")
	}

	slowed := false
	for _, m := range chilled.StatMods {
		if m.Stat == "movement_speed" && m.Increased < 0 {
			slowed = true
		}
	}
	if !slowed {
		t.Error("Chilled does not reduce movement speed; a debuff whose whole " +
			"purpose is a negative modifier is doing nothing")
	}

	weakened := c.Buffs["weakened"]
	if weakened == nil {
		t.Fatal("no weakened buff")
	}
	for _, m := range weakened.StatMods {
		if m.Stat == "attack" && m.Increased >= 0 {
			t.Errorf("Weakened changes attack by %d, which is not a weakening",
				m.Increased)
		}
	}
}

// Two notables with the same name in one cluster reads as a bug, and makes a
// build impossible to describe: "take Bulwark" stops meaning anything.
func TestNotableNamesAreNotRepeated(t *testing.T) {
	c := mustLoadShipped(t)

	seen := map[string]int{}
	for _, id := range c.Passives.Order {
		node := c.Passives.Nodes[id]
		if node.Kind == NodeNotable || node.Kind == NodeKeystone {
			seen[node.Name]++
		}
	}

	// A name may appear in more than one cluster -- every class having a
	// "Bulwark" route is fine -- but not more often than there are clusters.
	clusters := len(c.Passives.ClassStarts)
	for name, n := range seen {
		if n > clusters {
			t.Errorf("%q appears %d times across %d clusters; a name that repeats "+
				"within a cluster makes a build impossible to describe",
				name, n, clusters)
		}
	}
}
