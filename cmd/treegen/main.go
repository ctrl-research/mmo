// Command treegen generates the passive tree.
//
// The tree is the one piece of content too large to hand-author: hand-editing
// a several-hundred-node graph is not a plan, and a graphical editor is a
// project rather than a tool. What is here instead is a generator whose input
// is a readable description of what each part of the tree is *about*
// (themes.go) and whose output is the graph.
//
// That split is the point. The shape -- three clusters, branches radiating
// out, notables at intervals, keystones at the ends, bridges between
// neighbours -- is mechanical and belongs in code. What the nodes do is design
// and belongs in a file somebody reads.
//
// Run `make tree`, look at the diff, commit both. Same arrangement as the
// golden movement fixtures and the sprites.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "content/passives/tree.json", "where to write the tree")
	flag.Parse()

	tree := generate()

	if err := write(*out, tree); err != nil {
		fmt.Fprintln(os.Stderr, "treegen:", err)
		os.Exit(1)
	}

	fmt.Printf("%d nodes, %d edges, %d class starts\n",
		len(tree.Nodes), len(tree.Edges), len(tree.ClassStarts))
	fmt.Printf("%d notables, %d keystones\n", count(tree, "notable"), count(tree, "keystone"))
}

// The tree's shape.
//
// Sized so that a character at the level cap has spent a meaningful fraction
// of it rather than most of it: a tree you can fill is a tree with no choices
// left in it by the end.
const (
	// branchesPerCluster radiate out from each class start.
	branchesPerCluster = 3

	// nodesPerBranch is how far a branch runs before its keystone.
	nodesPerBranch = 9

	// notableEvery places a notable this far along a branch, so there is
	// always something worth reaching a few points away.
	notableEvery = 4

	// clusterRadius is how far a class start sits from the centre, and
	// nodeSpacing how far apart nodes are along a branch. Both are layout
	// only -- the client draws what it is given.
	clusterRadius = 320
	nodeSpacing   = 84

	// bridgeNodes join neighbouring clusters. Few and plain, so travelling to
	// another class's keystone is a commitment rather than a detour.
	bridgeNodes = 5
)

type treeJSON struct {
	Nodes       []nodeJSON     `json:"nodes"`
	Edges       [][2]int       `json:"edges"`
	ClassStarts map[string]int `json:"class_starts"`
}

type nodeJSON struct {
	ID    int    `json:"id"`
	Kind  string `json:"kind"`
	Name  string `json:"name,omitempty"`
	Pos   [2]int `json:"pos"`
	Stats []mod  `json:"stats,omitempty"`
}

// builder accumulates the graph while it is being laid out.
type builder struct {
	nodes  []nodeJSON
	edges  [][2]int
	starts map[string]int
	nextID int
}

func (b *builder) add(kind, name string, x, y int, mods []mod) int {
	b.nextID++
	b.nodes = append(b.nodes, nodeJSON{
		ID: b.nextID, Kind: kind, Name: name, Pos: [2]int{x, y}, Stats: mods,
	})
	return b.nextID
}

func (b *builder) link(a, c int) {
	b.edges = append(b.edges, [2]int{a, c})
}

func generate() treeJSON {
	b := &builder{starts: map[string]int{}}

	// One cluster per class, spaced evenly around a centre. Evenly, because
	// the class you picked should not decide how far you are from everything
	// else.
	clusterAngle := 2 * math.Pi / float64(len(themes))
	startIDs := make([]int, len(themes))

	for i, t := range themes {
		angle := clusterAngle * float64(i)
		cx := int(math.Round(math.Cos(angle) * clusterRadius))
		cy := int(math.Round(math.Sin(angle) * clusterRadius))

		start := b.add(string(kindStart), t.name+" Start", cx, cy, nil)
		b.starts[t.class] = start
		startIDs[i] = start

		b.growCluster(t, start, angle, cx, cy)
	}

	// Bridges between neighbouring clusters, so a build can travel. Without
	// them each class is on its own island and the tree is three trees.
	for i := range startIDs {
		b.bridge(startIDs[i], startIDs[(i+1)%len(startIDs)])
	}

	return treeJSON{Nodes: b.nodes, Edges: b.edges, ClassStarts: b.starts}
}

// kind names, kept as constants so a typo fails to compile rather than failing
// content validation.
type kindName string

const (
	kindStart    kindName = "start"
	kindSmall    kindName = "small"
	kindNotable  kindName = "notable"
	kindKeystone kindName = "keystone"
)

// growCluster grows one class's branches outward from its start.
func (b *builder) growCluster(t theme, start int, baseAngle float64, cx, cy int) {
	// Branches fan outward, away from the centre, so a cluster reads as
	// belonging to one class rather than sprawling across its neighbours.
	spread := math.Pi / 2.2

	for branch := 0; branch < branchesPerCluster; branch++ {
		offset := spread * (float64(branch)/float64(branchesPerCluster-1) - 0.5)
		angle := baseAngle + offset

		previous := start
		notableIndex := branch

		for step := 1; step <= nodesPerBranch; step++ {
			x := cx + int(math.Round(math.Cos(angle)*float64(step*nodeSpacing)))
			y := cy + int(math.Round(math.Sin(angle)*float64(step*nodeSpacing)))

			var id int
			switch {
			case step == nodesPerBranch:
				// The end of a branch is a keystone: the thing the whole
				// route was for.
				k := t.keystones[branch%len(t.keystones)]
				id = b.add(string(kindKeystone), k.name, x, y, k.mods)

			case step%notableEvery == 0:
				n := t.notables[notableIndex%len(t.notables)]
				notableIndex += branchesPerCluster
				id = b.add(string(kindNotable), n.name, x, y, n.mods)

			default:
				// Small nodes cycle through the theme, so a branch reads as
				// "this way is strength" rather than as a list.
				m := t.smalls[(step+branch)%len(t.smalls)]
				id = b.add(string(kindSmall), "", x, y, []mod{m})
			}

			b.link(previous, id)
			previous = id
		}
	}
}

// bridge joins two class starts with a plain path.
func (b *builder) bridge(from, to int) {
	fromNode := b.nodes[from-1]
	toNode := b.nodes[to-1]

	previous := from
	for i := 1; i <= bridgeNodes; i++ {
		// A straight line between the two starts, bowed toward the centre so
		// the bridges do not cross the clusters they pass.
		f := float64(i) / float64(bridgeNodes+1)
		x := int(math.Round(float64(fromNode.Pos[0])*(1-f) + float64(toNode.Pos[0])*f))
		y := int(math.Round(float64(fromNode.Pos[1])*(1-f) + float64(toNode.Pos[1])*f))

		// Pulled toward the origin, which is empty, so bridges arc through the
		// middle rather than through somebody's branches.
		x = int(float64(x) * 0.55)
		y = int(float64(y) * 0.55)

		id := b.add(string(kindSmall), "", x, y, []mod{bridge[i%len(bridge)]})
		b.link(previous, id)
		previous = id
	}
	b.link(previous, to)
}

func count(t treeJSON, kind string) int {
	n := 0
	for _, node := range t.Nodes {
		if node.Kind == kind {
			n++
		}
	}
	return n
}

func write(path string, tree treeJSON) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
