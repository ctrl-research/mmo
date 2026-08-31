package content

import (
	"strings"
	"testing"
	"testing/fstest"
)

// The events loader has two jobs beyond parsing: refusing a trigger that has
// been given two answers to "when does this start again", and refusing a
// reference that would load cleanly and then do nothing in play. Both are
// authoring mistakes that produce no error at runtime -- an event naming a
// renamed spawn point announces itself to the whole room and then produces
// nothing -- so the load is the only place they can be caught.

const eventMapTMJ = `{
  "type": "map", "width": 40, "height": 10, "tilewidth": 32, "tileheight": 32,
  "properties": [
    {"name": "mapId", "type": "string", "value": "glade"},
    {"name": "placement", "type": "string", "value": "shared"},
    {"name": "capacity", "type": "int", "value": 12}
  ],
  "layers": [{
    "name": "collision", "type": "objectgroup",
    "objects": [
      {"id": 1, "class": "solid", "x": 0, "y": 288, "width": 1280, "height": 32},
      {"id": 2, "class": "spawn_point", "name": "start", "x": 64, "y": 288,
       "properties": [{"name": "isDefault", "type": "bool", "value": true}]},
      {"id": 3, "class": "mob_spawn", "name": "tide", "x": 300, "y": 288,
       "properties": [
         {"name": "mob_id", "type": "string", "value": "test_mob"},
         {"name": "layer", "type": "string", "value": "shared"},
         {"name": "respawn_ms", "type": "int", "value": 1000},
         {"name": "max_alive", "type": "int", "value": 4}
       ]},
      {"id": 4, "class": "shrine", "name": "test-shrine", "x": 700, "y": 240,
       "width": 48, "height": 48}
    ]
  }]
}`

const eventsTOML = `
[event.tide]
name = "Slime Tide"
map = "glade"
trigger = "timer"
announce = "the undergrowth churns"
every_ms = 60000
duration_ms = 30000
spawns = ["tide"]

[event.summons]
name = "Warden's Call"
map = "glade"
trigger = "shrine"
shrine = "test-shrine"
announce = "something answers"
cooldown_ms = 60000
duration_ms = 30000
spawns = ["tide"]
`

func eventFS() fstest.MapFS {
	f := minimalFS()
	f["maps/glade.tmj"] = file(eventMapTMJ)
	f["events/test.toml"] = file(eventsTOML)
	return f
}

func TestEventContentLoads(t *testing.T) {
	c, err := Load(eventFS())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	events := c.EventsForMap("glade")
	if len(events) != 2 {
		t.Fatalf("glade has %d events, want 2", len(events))
	}
	if got := c.EventForShrine("glade", "test-shrine"); got == nil || got.ID != "summons" {
		t.Errorf("EventForShrine = %v, want the summons event", got)
	}
	if got := c.EventForSpawn("glade", "tide"); got == nil {
		t.Error("EventForSpawn(tide) = nil, want an event: the spawn belongs to both")
	}
	// A spawn point on a map with no events at all must not be claimed by one,
	// or every ordinary spawn point in the game would be gated shut.
	if got := c.EventForSpawn("test", "mobs"); got != nil {
		t.Errorf("EventForSpawn(test/mobs) = %v, want nil", got)
	}

	// Milliseconds are ticks by the time a room sees them.
	tide := events[1]
	if tide.ID != "tide" {
		tide = events[0]
	}
	if tide.EveryTicks != 60000/(1000/TickRate) {
		t.Errorf("EveryTicks = %d, want %d", tide.EveryTicks, 60000/(1000/TickRate))
	}
}

func TestBrokenEventsAreRejected(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(fstest.MapFS)
		wantErr string
	}{
		{
			name: "an event names a spawn point the map does not have",
			mutate: func(f fstest.MapFS) {
				f["events/test.toml"] = file(strings.Replace(eventsTOML,
					`spawns = ["tide"]`, `spawns = ["no_such_spawn"]`, 1))
			},
			wantErr: `which map "glade" does not have`,
		},
		{
			name: "an event names a shrine the map does not have",
			mutate: func(f fstest.MapFS) {
				f["events/test.toml"] = file(strings.Replace(eventsTOML,
					`shrine = "test-shrine"`, `shrine = "no_such_shrine"`, 1))
			},
			wantErr: `needs shrine "no_such_shrine"`,
		},
		{
			name: "an event happens on a map that does not exist",
			mutate: func(f fstest.MapFS) {
				f["events/test.toml"] = file(strings.Replace(eventsTOML,
					`map = "glade"`, `map = "no_such_map"`, 1))
			},
			wantErr: `unknown map "no_such_map"`,
		},
		{
			name: "a shrine no event listens to",
			mutate: func(f fstest.MapFS) {
				// Deleting the event that listens leaves the shrine standing:
				// a thing a player can walk into that does nothing.
				f["events/test.toml"] = file(strings.Split(eventsTOML, "[event.summons]")[0])
			},
			wantErr: `no event is triggered by it`,
		},
		{
			name: "a timed event also sets a cooldown",
			mutate: func(f fstest.MapFS) {
				f["events/test.toml"] = file(strings.Replace(eventsTOML,
					"every_ms = 60000", "every_ms = 60000\ncooldown_ms = 5000", 1))
			},
			wantErr: "two answers to when it starts again",
		},
		{
			name: "a shrine event also sets a period",
			mutate: func(f fstest.MapFS) {
				f["events/test.toml"] = file(strings.Replace(eventsTOML,
					"cooldown_ms = 60000", "cooldown_ms = 60000\nevery_ms = 5000", 1))
			},
			wantErr: "starts when somebody starts it",
		},
		{
			name: "a timed event with no period",
			mutate: func(f fstest.MapFS) {
				f["events/test.toml"] = file(strings.Replace(eventsTOML,
					"every_ms = 60000\n", "", 1))
			},
			wantErr: "would never start",
		},
		{
			name: "a shrine event with no cooldown",
			mutate: func(f fstest.MapFS) {
				f["events/test.toml"] = file(strings.Replace(eventsTOML,
					"cooldown_ms = 60000\n", "", 1))
			},
			wantErr: "presses forever",
		},
		{
			name: "an event with no duration",
			mutate: func(f fstest.MapFS) {
				f["events/test.toml"] = file(strings.Replace(eventsTOML,
					"duration_ms = 30000", "duration_ms = 0", 1))
			},
			wantErr: "has no duration",
		},
		{
			name: "an event with no spawn points",
			mutate: func(f fstest.MapFS) {
				f["events/test.toml"] = file(strings.Replace(eventsTOML,
					`spawns = ["tide"]`, `spawns = []`, 1))
			},
			wantErr: "nothing would happen when it started",
		},
		{
			name: "an unknown trigger",
			mutate: func(f fstest.MapFS) {
				f["events/test.toml"] = file(strings.Replace(eventsTOML,
					`trigger = "timer"`, `trigger = "vibes"`, 1))
			},
			wantErr: "want timer or shrine",
		},
		{
			name: "a shrine with no name",
			mutate: func(f fstest.MapFS) {
				f["maps/glade.tmj"] = file(strings.Replace(eventMapTMJ,
					`"class": "shrine", "name": "test-shrine"`, `"class": "shrine", "name": ""`, 1))
			},
			wantErr: "so no event could name it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := eventFS()
			tt.mutate(f)
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
