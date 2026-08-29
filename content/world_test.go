package content_test

import (
	"sort"
	"testing"

	gamedata "github.com/ctrl-research/mmo/content"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/world/sim"
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

// --- traversability ----------------------------------------------------------

// Can a player actually get from where they spawn to where the map sends them?
//
// This is the check that was missing, and the bug it would have caught was not
// subtle: the jump cleared 84 units, every map was built on 96-unit steps, and
// the tutorial's only route to its exit portal was a 128-unit pillar. The
// starting zone had no way out and had not since the map was written. Nothing
// failed, because nothing compared the jump to the maps.
//
// The model is deliberately coarse -- surfaces as nodes, jumps and ropes as
// edges -- and deliberately conservative: it uses the horizontal reach the
// simulation actually has at each height, so an edge it allows is one a player
// can make. A dead end it reports is a real one.
func TestSpawnCanReachEveryPortalAndWaypoint(t *testing.T) {
	c := load(t)
	tuning := sim.DefaultTuning()

	for id, m := range c.Maps {
		reachable := reachableSurfaces(m, &tuning)

		check := func(what string, at sim.Rect) {
			if standingOn(m, &tuning, at, reachable) {
				return
			}
			t.Errorf("map %s: %s at x=%v y=%v cannot be reached from the spawn point",
				id, what, at.X, at.Y)
		}

		for _, p := range m.Portals {
			check("the portal to "+p.TargetMap, p.Bounds)
		}
		for _, w := range m.Waypoints {
			check("the waypoint "+w.Name, w.Bounds)
		}
		for _, sp := range m.Spawns {
			check("spawn point "+sp.Name, sim.Rect{X: sp.At.X, Y: sp.At.Y})
		}
	}
}

// reachableSurfaces floods out from the surface under the spawn point.
func reachableSurfaces(m *content.Map, tuning *sim.Tuning) map[int]bool {
	all := standable(m.World, tuning)

	from := m.DefaultSpawn().At
	start := surfaceUnder(all, from)
	if start < 0 {
		return nil
	}

	seen := map[int]bool{start: true}
	queue := []int{start}

	for len(queue) > 0 {
		here := all[queue[0]]
		queue = queue[1:]

		for i, there := range all {
			if seen[i] || !connected(m.World, tuning, here, there) {
				continue
			}
			seen[i] = true
			queue = append(queue, i)
		}
	}
	return seen
}

// connected reports whether a player standing on `from` can get onto `to`.
func connected(w *sim.World, tuning *sim.Tuning, from, to sim.Rect) bool {
	rise := from.Y - to.Y

	// Whatever stands between them has to be climbable, or there is no path
	// however short the gap looks. This is the pillar case: two stretches of
	// the same floor, 96 units apart, with a wall between them.
	if wallBetween(w, tuning, from, to) {
		return false
	}

	// Downward or level: you can always drop onto something below, as long as
	// it is not off in another part of the map. Falling covers ground while it
	// descends, so this is generous on purpose -- the interesting failures are
	// upward.
	if rise <= 0 {
		return horizontalGap(from, to) <= sim.JumpReach(tuning, 0)
	}

	if rise <= sim.MaxJumpHeight(tuning) &&
		horizontalGap(from, to) <= sim.JumpReach(tuning, rise) {
		return true
	}

	// A rope reaches anything it passes, from anything it passes.
	for _, rope := range w.Climbables {
		if overlapsColumn(rope, from) && overlapsColumn(rope, to) &&
			rope.Y <= to.Y && rope.Bottom() >= from.Y {
			return true
		}
	}
	return false
}

// wallBetween reports whether a solid stands in the space between two surfaces
// and rises too far above both to be climbed over.
func wallBetween(w *sim.World, tuning *sim.Tuning, from, to sim.Rect) bool {
	left, right := from, to
	if right.X < left.X {
		left, right = right, left
	}
	if left.Right() >= right.X {
		// They overlap horizontally; nothing is between them.
		return false
	}

	climb := sim.MaxJumpHeight(tuning)
	higher := from.Y
	if to.Y < higher {
		higher = to.Y
	}

	for _, solid := range w.Solids {
		if solid.Right() <= left.Right() || solid.X >= right.X {
			continue
		}
		// Above the higher of the two surfaces by more than a jump, and
		// reaching down to at least it: a wall, not a step. The comparison is
		// inclusive because a pillar standing on a floor has its bottom edge
		// exactly at the floor's top.
		if higher-solid.Y > climb && solid.Bottom() >= higher {
			return true
		}
	}
	return false
}

// horizontalGap is the distance between two surfaces' spans, zero if they
// overlap.
func horizontalGap(a, b sim.Rect) fixed.F {
	if a.Right() < b.X {
		return b.X - a.Right()
	}
	if b.Right() < a.X {
		return a.X - b.Right()
	}
	return 0
}

// overlapsColumn reports whether a rope is close enough to a surface to be
// stepped onto from it.
func overlapsColumn(rope, surface sim.Rect) bool {
	return horizontalGap(rope, surface) <= fixed.FromInt(48)
}

// standable returns every stretch of surface a player can stand on and walk
// along without being stopped.
//
// The splitting is the part that matters. Treating each surface as one node
// says the floor is one place, so walking from one end to the other is free --
// and that is exactly the assumption that hid the bug this test exists for.
// The tutorial's floor ran the width of the map with a pillar standing on it
// too tall to climb, which made the far end, and the portal on it, a different
// place entirely.
//
// Only obstacles too tall to jump onto split a surface. A knee-high block is
// scenery; one above the jump is a wall.
func standable(w *sim.World, tuning *sim.Tuning) []sim.Rect {
	tops := make([]sim.Rect, 0, len(w.Solids)+len(w.Platforms))
	for _, r := range append(append([]sim.Rect{}, w.Solids...), w.Platforms...) {
		// Wider than they are tall: a wall's "top" is the top of the map,
		// which nobody stands on and which would otherwise look like an
		// unreachable ledge.
		if r.W > r.H {
			tops = append(tops, r)
		}
	}

	climb := sim.MaxJumpHeight(tuning)

	var out []sim.Rect
	for _, top := range tops {
		segments := []sim.Rect{top}

		for _, blocker := range w.Solids {
			// Standing on this surface, and too tall to get over.
			if blocker.Bottom() < top.Y || blocker.Y >= top.Y {
				continue
			}
			if top.Y-blocker.Y <= climb {
				continue
			}

			var next []sim.Rect
			for _, seg := range segments {
				next = append(next, cutOut(seg, blocker.X, blocker.Right())...)
			}
			segments = next
		}
		out = append(out, segments...)
	}
	return out
}

// cutOut removes a horizontal range from a surface, leaving what is still
// walkable on either side.
func cutOut(seg sim.Rect, from, to fixed.F) []sim.Rect {
	if to <= seg.X || from >= seg.Right() {
		return []sim.Rect{seg}
	}

	var out []sim.Rect
	if from > seg.X {
		left := seg
		left.W = from - seg.X
		out = append(out, left)
	}
	if to < seg.Right() {
		right := seg
		right.X = to
		right.W = seg.Right() - to
		out = append(out, right)
	}
	return out
}

// surfaceUnder finds the surface a point is standing on, or the nearest below.
func surfaceUnder(surfaces []sim.Rect, at sim.Vec) int {
	best := -1
	for i, s := range surfaces {
		if at.X < s.X || at.X > s.Right() || s.Y < at.Y-fixed.FromInt(8) {
			continue
		}
		if best < 0 || s.Y < surfaces[best].Y {
			best = i
		}
	}
	return best
}

// standingOn reports whether something sits on a reachable surface.
//
// A portal or waypoint is a volume rather than a point, so it counts as
// reached if any reachable surface passes through or just below it.
func standingOn(m *content.Map, tuning *sim.Tuning, what sim.Rect, reachable map[int]bool) bool {
	all := standable(m.World, tuning)
	width := what.W
	if width <= 0 {
		width = fixed.FromInt(1)
	}

	for i, s := range all {
		if !reachable[i] {
			continue
		}
		if horizontalGap(s, sim.Rect{X: what.X, Y: what.Y, W: width, H: what.H}) > 0 {
			continue
		}
		// The surface has to be at the thing's feet: level with it, or within
		// a body's height below its bottom edge.
		bottom := what.Y + what.H
		if s.Y >= what.Y-fixed.FromInt(8) && s.Y <= bottom+sim.PlayerSize.H {
			return true
		}
	}
	return false
}
