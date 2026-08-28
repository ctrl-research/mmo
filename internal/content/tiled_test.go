package content

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ctrl-research/mmo/internal/fixed"
)

const validMap = `{
  "type": "map", "width": 20, "height": 10, "tilewidth": 32, "tileheight": 32,
  "properties": [
    {"name": "mapId", "type": "string", "value": "test-map"},
    {"name": "displayName", "type": "string", "value": "Test Map"},
    {"name": "placement", "type": "string", "value": "shared"},
    {"name": "capacity", "type": "int", "value": 12}
  ],
  "layers": [{
    "name": "collision", "type": "objectgroup",
    "objects": [
      {"id": 1, "name": "floor", "class": "solid", "x": 0, "y": 288, "width": 640, "height": 32},
      {"id": 2, "name": "ledge", "class": "platform", "x": 100, "y": 200, "width": 128, "height": 16},
      {"id": 3, "name": "rope", "class": "rope", "x": 300, "y": 100, "width": 16, "height": 188},
      {"id": 4, "name": "start", "class": "spawn_point", "x": 64, "y": 288, "width": 0, "height": 0,
       "properties": [{"name": "isDefault", "type": "bool", "value": true}]}
    ]
  }]
}`

func mapFS(body string) fstest.MapFS {
	return fstest.MapFS{"m.tmj": &fstest.MapFile{Data: []byte(body)}}
}

func TestLoadMap(t *testing.T) {
	m, err := LoadMap(mapFS(validMap), "m.tmj")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if m.ID != "test-map" {
		t.Errorf("id = %q, want test-map", m.ID)
	}
	if m.DisplayName != "Test Map" {
		t.Errorf("display name = %q, want Test Map", m.DisplayName)
	}
	if m.Capacity != 12 {
		t.Errorf("capacity = %d, want 12", m.Capacity)
	}
	if len(m.World.Solids) != 1 {
		t.Errorf("solids = %d, want 1", len(m.World.Solids))
	}
	if len(m.World.Platforms) != 1 {
		t.Errorf("platforms = %d, want 1", len(m.World.Platforms))
	}
	if len(m.World.Climbables) != 1 {
		t.Errorf("climbables = %d, want 1", len(m.World.Climbables))
	}

	// Bounds come from the tile grid, so the backstop clamp matches the map.
	if got := m.World.Bounds.W; got != fixed.FromInt(640) {
		t.Errorf("bounds width = %v, want 640", got)
	}
	if got := m.World.Bounds.H; got != fixed.FromInt(320) {
		t.Errorf("bounds height = %v, want 320", got)
	}
}

func TestCoordinatesConvertExactly(t *testing.T) {
	m, err := LoadMap(mapFS(validMap), "m.tmj")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	floor := m.World.Solids[0]
	if floor.X != fixed.FromInt(0) || floor.Y != fixed.FromInt(288) {
		t.Errorf("floor at (%v, %v), want (0, 288)", floor.X, floor.Y)
	}
	if floor.W != fixed.FromInt(640) || floor.H != fixed.FromInt(32) {
		t.Errorf("floor size (%v, %v), want (640, 32)", floor.W, floor.H)
	}
}

// Tiled writes floats, and a coordinate that round-trips as 511.99999 must
// still land exactly on 512 rather than one sub-unit short.
func TestNearIntegerCoordinatesRound(t *testing.T) {
	body := strings.Replace(validMap, `"x": 0, "y": 288`, `"x": 511.99999, "y": 288.00001`, 1)
	m, err := LoadMap(mapFS(body), "m.tmj")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := m.World.Solids[0].X; got != fixed.FromInt(512) {
		t.Errorf("x = %v, want exactly 512", got)
	}
	if got := m.World.Solids[0].Y; got != fixed.FromInt(288) {
		t.Errorf("y = %v, want exactly 288", got)
	}
}

func TestSpawnPointIsFeetPosition(t *testing.T) {
	m, err := LoadMap(mapFS(validMap), "m.tmj")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sp := m.DefaultSpawn()
	if sp.Name != "start" {
		t.Errorf("spawn name = %q, want start", sp.Name)
	}
	// Authored on the floor surface, which is where a designer places it.
	if sp.At.Y != fixed.FromInt(288) {
		t.Errorf("spawn y = %v, want 288 (the floor)", sp.At.Y)
	}
}

// Boot must fail on invalid content rather than starting with a broken world.
func TestInvalidMapsAreRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no spawn point",
			body: strings.Replace(validMap,
				`{"id": 4, "name": "start", "class": "spawn_point", "x": 64, "y": 288, "width": 0, "height": 0,
       "properties": [{"name": "isDefault", "type": "bool", "value": true}]}`, `{}`, 1),
			want: "class",
		},
		{
			name: "no solid geometry",
			body: strings.Replace(validMap, `"class": "solid"`, `"class": "platform"`, 1),
			want: "solid geometry",
		},
		{
			name: "unknown object class",
			body: strings.Replace(validMap, `"class": "platform"`, `"class": "platfrom"`, 1),
			want: "unknown class",
		},
		{
			name: "unknown placement",
			body: strings.Replace(validMap, `"value": "shared"`, `"value": "sometimes"`, 1),
			want: "unknown placement",
		},
		{
			name: "not a tiled map",
			body: strings.Replace(validMap, `"type": "map"`, `"type": "tileset"`, 1),
			want: "not a Tiled map",
		},
		{
			name: "malformed json",
			body: `{"type": "map"`,
			want: "parsing map",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadMap(mapFS(tt.body), "m.tmj")
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestMissingFileErrors(t *testing.T) {
	if _, err := LoadMap(mapFS(validMap), "nope.tmj"); err == nil {
		t.Error("expected an error loading a missing map")
	}
}

// Tiled before 1.9 spelled the class field "type"; maps authored in either
// version must load.
func TestLegacyTypeFieldSupported(t *testing.T) {
	body := strings.ReplaceAll(validMap, `"class":`, `"type":`)
	if _, err := LoadMap(mapFS(body), "m.tmj"); err != nil {
		t.Errorf("legacy Tiled map failed to load: %v", err)
	}
}

func TestMultipleDefaultSpawnsRejected(t *testing.T) {
	body := strings.Replace(validMap,
		`{"id": 4, "name": "start", "class": "spawn_point", "x": 64, "y": 288, "width": 0, "height": 0,
       "properties": [{"name": "isDefault", "type": "bool", "value": true}]}`,
		`{"id": 4, "name": "a", "class": "spawn_point", "x": 64, "y": 288,
       "properties": [{"name": "isDefault", "type": "bool", "value": true}]},
      {"id": 5, "name": "b", "class": "spawn_point", "x": 96, "y": 288,
       "properties": [{"name": "isDefault", "type": "bool", "value": true}]}`, 1)

	_, err := LoadMap(mapFS(body), "m.tmj")
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Errorf("expected a duplicate-default error, got %v", err)
	}
}
