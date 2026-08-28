// Package content loads game data from files.
//
// Everything the game is made of -- maps now, and items, mobs, skills, drop
// tables, and the passive tree in later milestones -- is declarative data in
// git rather than Go code. That is the difference between shipping ten skills
// and shipping three hundred (see docs/content-pipeline.md).
//
// Two rules apply to every loader here:
//
//   - Boot fails on invalid content. Never start with a partial world and
//     never fall back to defaults; a server that silently starts with a broken
//     map produces bug reports weeks later that trace to a startup warning
//     nobody read.
//   - Loaded content is immutable and shared. Rooms read it concurrently with
//     no locking, which is part of what keeps the tick loop cheap.
package content

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"path"
	"strings"

	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// Object classes recognised in a map's object layers. They are Tiled's "class"
// field, so maps stay editable in the real editor rather than in a bespoke
// format we would have to build and maintain ourselves.
const (
	classSolid      = "solid"
	classPlatform   = "platform"
	classRope       = "rope"
	classLadder     = "ladder"
	classSpawnPoint = "spawn_point"
)

// tmj mirrors the subset of Tiled's JSON export that the game reads. Fields
// Tiled writes but the game does not use are simply absent.
type tmj struct {
	Width      int        `json:"width"`
	Height     int        `json:"height"`
	TileWidth  int        `json:"tilewidth"`
	TileHeight int        `json:"tileheight"`
	Layers     []tmjLayer `json:"layers"`
	Properties []tmjProp  `json:"properties"`
	Type       string     `json:"type"`
}

type tmjLayer struct {
	Name    string      `json:"name"`
	Type    string      `json:"type"`
	Objects []tmjObject `json:"objects"`
}

type tmjObject struct {
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Class  string  `json:"class"`
	Type   string  `json:"type"` // Tiled before 1.9 called it "type"
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`

	Properties []tmjProp `json:"properties"`
}

// class returns the object's class, tolerating both Tiled spellings.
func (o tmjObject) class() string {
	if o.Class != "" {
		return o.Class
	}
	return o.Type
}

type tmjProp struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

func props(list []tmjProp) map[string]any {
	m := make(map[string]any, len(list))
	for _, p := range list {
		m[p.Name] = p.Value
	}
	return m
}

// SpawnPoint is a place a player can enter the map.
type SpawnPoint struct {
	Name string

	// At is the feet position, matching how spawn points are authored: a
	// designer places a marker on the floor, not at the centre of a body.
	At sim.Vec

	Default bool
}

// Map is one loaded, validated, immutable map.
type Map struct {
	ID          string
	DisplayName string
	Placement   string
	Capacity    int

	// World is the collision geometry the simulation runs against. It is
	// derived from the same file the client renders, so the two cannot drift.
	World *sim.World

	Spawns []SpawnPoint

	// Source is the original Tiled file, retained so the client renders the
	// very same document the collision geometry was derived from.
	Source []byte
}

// DefaultSpawn returns the spawn point marked default, or the first one.
func (m *Map) DefaultSpawn() SpawnPoint {
	for _, s := range m.Spawns {
		if s.Default {
			return s
		}
	}
	return m.Spawns[0]
}

// LoadMap reads and validates one Tiled map from fsys.
func LoadMap(fsys fs.FS, name string) (*Map, error) {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, fmt.Errorf("content: reading map %s: %w", name, err)
	}

	var doc tmj
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("content: parsing map %s: %w", name, err)
	}
	if doc.Type != "map" {
		return nil, fmt.Errorf("content: %s is not a Tiled map (type %q)", name, doc.Type)
	}

	m := &Map{
		ID:        strings.TrimSuffix(path.Base(name), path.Ext(name)),
		Placement: "shared",
		Capacity:  30,
		World:     &sim.World{},
		Source:    raw,
	}

	mp := props(doc.Properties)
	if v, ok := mp["mapId"].(string); ok && v != "" {
		m.ID = v
	}
	if v, ok := mp["displayName"].(string); ok {
		m.DisplayName = v
	}
	if v, ok := mp["placement"].(string); ok && v != "" {
		m.Placement = v
	}
	if v, ok := mp["capacity"].(float64); ok && v > 0 {
		m.Capacity = int(v)
	}

	for _, layer := range doc.Layers {
		if layer.Type != "objectgroup" {
			continue
		}
		for _, obj := range layer.Objects {
			if err := m.addObject(name, obj); err != nil {
				return nil, err
			}
		}
	}

	m.World.Bounds = sim.RectFromInts(0, 0, doc.Width*doc.TileWidth, doc.Height*doc.TileHeight)

	if err := m.validate(name); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Map) addObject(mapName string, obj tmjObject) error {
	switch obj.class() {
	case classSolid:
		m.World.Solids = append(m.World.Solids, rect(obj))
	case classPlatform:
		m.World.Platforms = append(m.World.Platforms, rect(obj))
	case classRope, classLadder:
		m.World.Climbables = append(m.World.Climbables, rect(obj))
	case classSpawnPoint:
		sp := SpawnPoint{
			Name: obj.Name,
			At:   sim.Vec{X: toFixed(obj.X), Y: toFixed(obj.Y)},
		}
		if v, ok := props(obj.Properties)["isDefault"].(bool); ok {
			sp.Default = v
		}
		m.Spawns = append(m.Spawns, sp)
	case "":
		return fmt.Errorf("content: %s: object %d (%q) has no class",
			mapName, obj.ID, obj.Name)
	default:
		// An unrecognised class is almost always a typo in the map, and
		// ignoring it silently means a designer's spawn point simply never
		// appears with no indication why.
		return fmt.Errorf("content: %s: object %d (%q) has unknown class %q",
			mapName, obj.ID, obj.Name, obj.class())
	}
	return nil
}

func (m *Map) validate(name string) error {
	if len(m.Spawns) == 0 {
		return fmt.Errorf("content: %s has no spawn_point; players would have nowhere to enter", name)
	}
	if len(m.World.Solids) == 0 {
		return fmt.Errorf("content: %s has no solid geometry; players would fall out of the world", name)
	}
	switch m.Placement {
	case "shared", "private":
	default:
		return fmt.Errorf("content: %s has unknown placement %q, want shared or private", name, m.Placement)
	}

	defaults := 0
	for _, s := range m.Spawns {
		if s.Default {
			defaults++
		}
	}
	if defaults > 1 {
		return fmt.Errorf("content: %s marks %d spawn points as default, want at most 1", name, defaults)
	}
	return nil
}

func rect(o tmjObject) sim.Rect {
	return sim.Rect{
		X: toFixed(o.X),
		Y: toFixed(o.Y),
		W: toFixed(o.Width),
		H: toFixed(o.Height),
	}
}

// toFixed converts a Tiled coordinate to the simulation's fixed-point form.
//
// This is the only float-to-fixed conversion in the codebase, and it happens
// once at load time rather than anywhere near the tick loop. Tiled writes whole
// pixel values in practice; rounding rather than truncating means a coordinate
// that round-trips through JSON as 511.99999 still lands on 512.
//
// math.Round rounds half away from zero, so negative coordinates -- which
// Tiled does allow for objects placed above or left of the origin -- round
// symmetrically instead of drifting toward zero.
func toFixed(v float64) fixed.F {
	return fixed.F(int32(math.Round(v * float64(fixed.One))))
}
