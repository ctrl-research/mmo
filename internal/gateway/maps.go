package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Map endpoints serve what the client needs to render and to predict.
//
// The collision endpoint matters more than it looks. The client's prediction
// must run against byte-identical geometry, so rather than have the client
// re-derive collision from the map file, the server encodes it with the same
// function the simulation uses and hands it over. There is no second
// implementation to drift.

func (g *Gateway) handleMapCollision(w http.ResponseWriter, r *http.Request) {
	m, ok := g.lookupMap(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	// Collision geometry changes only when content does, and the content hash
	// is part of the handshake, so it is safe to cache hard.
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Write(sim.MarshalWorld(m.World))
}

// handleMapSource serves the original Tiled file for rendering.
func (g *Gateway) handleMapSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, ok := g.lookupMap(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if m.Source == nil {
		http.Error(w, "map source unavailable", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Write(m.Source)
}

func (g *Gateway) lookupMap(id string) (*content.Map, bool) {
	// Path values are untrusted. Only exact matches against loaded map IDs are
	// served, so no path traversal is possible regardless of what is sent.
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return nil, false
	}
	m, ok := g.maps[id]
	return m, ok
}

// handlePassiveTree serves the tree the client draws.
//
// Over HTTP rather than the socket, and once rather than per allocation: the
// tree is content, identical for everybody, and several hundred nodes with
// positions and modifiers is a lot to resend every time somebody spends a
// point. What travels on the socket is which nodes a character holds.
//
// Served from the loaded content rather than the file, so it cannot disagree
// with what the server is validating against.
func (g *Gateway) handlePassiveTree(w http.ResponseWriter, r *http.Request) {
	tree := g.passiveTree
	if len(tree) == 0 {
		http.Error(w, "no passive tree", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Cached hard: the tree changes only when content does, and a content
	// change already forces every client to reconnect on the hash check.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(tree)
}

// renderPassiveTree turns the loaded tree into what the client draws.
//
// A shape of its own rather than the content type: the client needs positions,
// names, and modifiers as text it can show, and it has no business
// reconstructing the server's stat identifiers to do it.
func renderPassiveTree(tree *content.PassiveTree) []byte {
	if tree == nil {
		return nil
	}

	type node struct {
		ID    int      `json:"id"`
		Kind  string   `json:"kind"`
		Name  string   `json:"name,omitempty"`
		X     int      `json:"x"`
		Y     int      `json:"y"`
		Lines []string `json:"lines"`
	}
	type payload struct {
		Nodes       []node         `json:"nodes"`
		Edges       [][2]int       `json:"edges"`
		ClassStarts map[string]int `json:"classStarts"`
	}

	out := payload{ClassStarts: tree.ClassStarts}

	for _, id := range tree.Order {
		n := tree.Nodes[id]
		out.Nodes = append(out.Nodes, node{
			ID: n.ID, Kind: string(n.Kind), Name: n.Name,
			X: n.X, Y: n.Y, Lines: describeMods(n.Mods),
		})

		// Each edge once. Adjacency holds both directions, so taking only the
		// ascending half halves what is sent and drawn.
		for _, other := range tree.Adjacency[id] {
			if other > id {
				out.Edges = append(out.Edges, [2]int{id, other})
			}
		}
	}

	data, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return data
}

// describeMods turns modifiers into the lines shown on a node.
//
// Phrased the way a player reads them -- "+8 Strength", "24% increased Armour"
// -- because a tooltip that says `armour increased 240000` is a tooltip nobody
// can use.
func describeMods(mods []content.StatMod) []string {
	out := make([]string, 0, len(mods))

	for _, m := range mods {
		label := statLabel(m.Stat)
		switch {
		case m.Flat != 0:
			out = append(out, fmt.Sprintf("%+d %s", m.Flat, label))
		case m.Increased != 0:
			out = append(out, fmt.Sprintf("%s increased %s", percent(m.Increased), label))
		case m.More != 0:
			out = append(out, fmt.Sprintf("%s more %s", percent(m.More), label))
		}
	}
	return out
}

// percent renders parts-per-million as a percentage, signed.
func percent(ppm int) string {
	return fmt.Sprintf("%+.0f%%", float64(ppm)/10_000)
}

// statLabel turns a stat identifier into a readable name.
func statLabel(id string) string {
	words := strings.Split(id, "_")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
