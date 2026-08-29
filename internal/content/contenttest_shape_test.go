package content_test

import (
	"testing"

	"github.com/ctrl-research/mmo/internal/content/contenttest"
)

// The test content set is used by every package that needs a world, so a
// change to it that quietly drops a portal or a waypoint would weaken tests
// elsewhere without failing anything.
func TestTestContentHasTwoConnectedMaps(t *testing.T) {
	c, err := contenttest.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	for _, want := range []struct {
		id        string
		portals   int
		waypoints int
	}{
		{"test", 1, 1},
		{"annex", 1, 1},
	} {
		m, ok := c.Maps[want.id]
		if !ok {
			t.Errorf("map %q is missing", want.id)
			continue
		}
		if len(m.Portals) != want.portals {
			t.Errorf("map %q has %d portals, want %d", want.id, len(m.Portals), want.portals)
		}
		if len(m.Waypoints) != want.waypoints {
			t.Errorf("map %q has %d waypoints, want %d", want.id, len(m.Waypoints), want.waypoints)
		}
	}

	for _, id := range []string{"wp_test", "wp_annex"} {
		if _, ok := c.Waypoints[id]; !ok {
			t.Errorf("waypoint %q is not indexed", id)
		}
	}
	if c.Maps["annex"] != nil && c.Maps["annex"].SpawnNamed("from_test").Name != "from_test" {
		t.Error("annex has no from_test spawn point for the portal to land on")
	}
}
