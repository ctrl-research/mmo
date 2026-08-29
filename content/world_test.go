package content_test

import (
	"sort"
	"testing"

	gamedata "github.com/ctrl-research/mmo/content"
	"github.com/ctrl-research/mmo/internal/content"
)

// The shipped world, not a fixture.
//
// Content loading is validated in internal/content; what this checks is that
// the maps actually shipped form a world a player can move through: every
// portal leads somewhere, every zone but the first is reachable, and there is
// a waypoint to find. A map with no way out loads perfectly and is a dead end.

func load(t *testing.T) *content.Content {
	t.Helper()

	c, err := content.Load(gamedata.FS)
	if err != nil {
		t.Fatalf("load shipped content: %v", err)
	}
	return c
}

func TestEveryZoneIsReachableFromTheStart(t *testing.T) {
	c := load(t)

	const start = "tutorial"
	if _, ok := c.Maps[start]; !ok {
		t.Fatalf("there is no %q map to start in", start)
	}

	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		for _, p := range c.Maps[id].Portals {
			if !seen[p.TargetMap] {
				seen[p.TargetMap] = true
				queue = append(queue, p.TargetMap)
			}
		}
	}

	var stranded []string
	for id := range c.Maps {
		if !seen[id] {
			stranded = append(stranded, id)
		}
	}
	sort.Strings(stranded)
	if len(stranded) > 0 {
		t.Errorf("no route from %s to %v; a zone with no way in loads perfectly "+
			"and is a dead end", start, stranded)
	}
}

// A zone a player can walk into but not out of is a trap: there is no
// disconnect-and-log-back-in escape, because the character's map is saved.
func TestEveryZoneHasAWayOut(t *testing.T) {
	c := load(t)

	for id, m := range c.Maps {
		if len(m.Portals) == 0 {
			t.Errorf("map %q has no portals; a character who walks in is stuck there", id)
		}
	}
}

// Fast travel is only worth having if there is somewhere to travel to, and a
// waypoint is only reachable if the zone holding it is.
func TestThereAreWaypointsToFind(t *testing.T) {
	c := load(t)

	if len(c.Waypoints) < 2 {
		t.Errorf("the world has %d waypoints; fast travel needs somewhere to go",
			len(c.Waypoints))
	}
	for id, w := range c.Waypoints {
		if _, ok := c.Maps[w.MapID]; !ok {
			t.Errorf("waypoint %q claims to be on map %q, which does not exist", id, w.MapID)
		}
	}
}

// Level gates are the reason a portal can refuse, and a gate that is never set
// means the level ranges on the world map are decoration.
func TestGatedZonesMatchTheirLevelRange(t *testing.T) {
	c := load(t)

	gated := 0
	for id, m := range c.Maps {
		for _, p := range m.Portals {
			if p.RequiredLevel == 0 {
				continue
			}
			gated++

			target := c.Maps[p.TargetMap]
			if p.RequiredLevel > target.MinLevel {
				t.Errorf("map %q gates %q at level %d, but the zone is advertised "+
					"from level %d; the world map would be telling a player to go "+
					"somewhere they cannot enter",
					id, p.TargetMap, p.RequiredLevel, target.MinLevel)
			}
		}
	}
	if gated == 0 {
		t.Error("no portal in the world has a level requirement")
	}
}
