package gateway

import (
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
