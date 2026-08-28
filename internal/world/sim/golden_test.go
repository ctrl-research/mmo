package sim

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctrl-research/mmo/internal/fixed"
)

// The golden corpus is the contract between the Go server build and the
// WebAssembly client build of this package.
//
// Client-side prediction only works if both produce bit-identical results from
// the same inputs. These fixtures record the exact tick-by-tick output of
// scripted input sequences so that any change to the simulation — intended or
// not — shows up as a diff rather than as rubber-banding in production.
//
// Regenerate deliberately, never reflexively:
//
//	go test ./internal/world/sim -update
//
// A diff in these files means movement changed. If that was the intent, commit
// the regenerated fixtures with the change that caused it. If it was not, the
// test just caught a real bug.

var update = flag.Bool("update", false, "rewrite the golden fixtures in testdata")

// frame is one tick of recorded output. Values are raw Q24.8 integers rather
// than decimal strings so the comparison is exact and has no formatting step
// that could hide a one-ulp difference.
type frame struct {
	Tick int   `json:"tick"`
	X    int32 `json:"x"`
	Y    int32 `json:"y"`
	VX   int32 `json:"vx"`
	VY   int32 `json:"vy"`
	Flag uint8 `json:"flags"`
}

const (
	flagGrounded = 1 << iota
	flagClimbing
	flagFacingLeft
	flagJumpHeld
)

func capture(tick int, b *Body) frame {
	var f uint8
	if b.Grounded {
		f |= flagGrounded
	}
	if b.Climbing {
		f |= flagClimbing
	}
	if b.FacingLeft {
		f |= flagFacingLeft
	}
	if b.JumpHeld {
		f |= flagJumpHeld
	}
	return frame{tick, int32(b.Pos.X), int32(b.Pos.Y), int32(b.Vel.X), int32(b.Vel.Y), f}
}

// vector is one scripted scenario: a starting position and a per-tick input
// script that repeats until the tick budget is spent.
type vector struct {
	name   string
	feetX  int
	feetY  int
	ticks  int
	script []Input
}

// fixture is the on-disk form of a vector: the scenario AND its expected
// output, so the corpus is self-describing.
//
// Carrying the world and the input script rather than just the frames is what
// makes this corpus portable. Any implementation of the simulation can replay
// it without reading Go test code -- which is exactly what the WebAssembly
// conformance test in the client does, and it is the mechanism that catches
// the two builds drifting apart.
type fixture struct {
	Name   string    `json:"name"`
	Ticks  int       `json:"ticks"`
	World  worldJSON `json:"world"`
	Start  startJSON `json:"start"`
	Script [][4]int  `json:"script"`
	Frames []frame   `json:"frames"`
}

type worldJSON struct {
	Solids     [][4]int32 `json:"solids"`
	Platforms  [][4]int32 `json:"platforms"`
	Climbables [][4]int32 `json:"climbables"`
	Bounds     [4]int32   `json:"bounds"`
}

type startJSON struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
	W int32 `json:"w"`
	H int32 `json:"h"`
}

func encodeRects(rs []Rect) [][4]int32 {
	out := make([][4]int32, 0, len(rs))
	for _, r := range rs {
		out = append(out, [4]int32{int32(r.X), int32(r.Y), int32(r.W), int32(r.H)})
	}
	return out
}

func encodeWorld(w *World) worldJSON {
	return worldJSON{
		Solids:     encodeRects(w.Solids),
		Platforms:  encodeRects(w.Platforms),
		Climbables: encodeRects(w.Climbables),
		Bounds: [4]int32{
			int32(w.Bounds.X), int32(w.Bounds.Y),
			int32(w.Bounds.W), int32(w.Bounds.H),
		},
	}
}

func encodeScript(script []Input) [][4]int {
	out := make([][4]int, 0, len(script))
	for _, in := range script {
		out = append(out, [4]int{
			int(in.MoveX), boolToInt(in.Jump), boolToInt(in.Up), boolToInt(in.Down),
		})
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// build assembles the complete fixture for a vector.
func (v vector) build() fixture {
	w := testWorld()
	b := NewBody(Vec{fixed.FromInt(v.feetX), fixed.FromInt(v.feetY)}, PlayerSize.W, PlayerSize.H)
	t := DefaultTuning()
	Settle(&b, w, &t)

	return fixture{
		Name:   v.name,
		Ticks:  v.ticks,
		World:  encodeWorld(w),
		Start:  startJSON{X: int32(b.Pos.X), Y: int32(b.Pos.Y), W: int32(b.W), H: int32(b.H)},
		Script: encodeScript(v.script),
		Frames: v.record(),
	}
}

func goldenVectors() []vector {
	return []vector{
		{
			name: "idle_fall_and_land", feetX: 100, feetY: 100, ticks: 40,
			script: []Input{{}},
		},
		{
			name: "run_right_into_open", feetX: 100, feetY: floorTop, ticks: 40,
			script: []Input{{MoveX: 1000}},
		},
		{
			name: "run_left_into_wall", feetX: 200, feetY: floorTop, ticks: 60,
			script: []Input{{MoveX: -1000}},
		},
		{
			name: "full_jump_held", feetX: 100, feetY: floorTop, ticks: 40,
			script: []Input{{Jump: true}},
		},
		{
			name: "tapped_jump", feetX: 100, feetY: floorTop, ticks: 40,
			script: []Input{{Jump: true}, {Jump: true}, {}},
		},
		{
			name: "running_jump_onto_platform", feetX: 60, feetY: floorTop, ticks: 60,
			script: []Input{
				{MoveX: 1000}, {MoveX: 1000}, {MoveX: 1000}, {MoveX: 1000},
				{MoveX: 1000, Jump: true}, {MoveX: 1000, Jump: true},
				{MoveX: 1000, Jump: true}, {MoveX: 1000},
			},
		},
		{
			name: "drop_through_platform", feetX: 200, feetY: 250, ticks: 60,
			script: []Input{
				{}, {}, {}, {}, {}, {}, {}, {}, {}, {},
				{Down: true, Jump: true}, {Down: true},
			},
		},
		{
			name: "climb_rope_up_then_down", feetX: 308, feetY: floorTop, ticks: 80,
			script: []Input{
				{Up: true}, {Up: true}, {Up: true}, {Up: true}, {Up: true},
				{Up: true}, {Up: true}, {Up: true}, {Up: true}, {Up: true},
				{}, {}, {},
				{Down: true}, {Down: true}, {Down: true}, {Down: true},
			},
		},
		{
			name: "jump_off_rope", feetX: 308, feetY: floorTop, ticks: 50,
			script: []Input{
				{Up: true}, {Up: true}, {Up: true}, {Up: true}, {Up: true},
				{Jump: true}, {MoveX: -1000}, {MoveX: -1000},
			},
		},
		{
			name: "terminal_velocity_long_fall", feetX: 400, feetY: 0, ticks: 60,
			script: []Input{{}},
		},
		{
			name: "coyote_jump_off_ledge", feetX: 270, feetY: platformTop, ticks: 50,
			script: []Input{
				{MoveX: 1000}, {MoveX: 1000}, {MoveX: 1000}, {MoveX: 1000},
				{MoveX: 1000}, {MoveX: 1000}, {MoveX: 1000, Jump: true},
				{MoveX: 1000, Jump: true},
			},
		},
		{
			name: "direction_reversal", feetX: 320, feetY: floorTop, ticks: 60,
			script: []Input{
				{MoveX: 1000}, {MoveX: 1000}, {MoveX: 1000}, {MoveX: 1000},
				{MoveX: -1000}, {MoveX: -1000}, {MoveX: -1000}, {MoveX: -1000},
			},
		},
	}
}

func (v vector) record() []frame {
	w, t := testWorld(), DefaultTuning()
	b := NewBody(Vec{fixed.FromInt(v.feetX), fixed.FromInt(v.feetY)}, PlayerSize.W, PlayerSize.H)
	Settle(&b, w, &t)

	out := make([]frame, 0, v.ticks)
	for i := 0; i < v.ticks; i++ {
		Step(&b, v.script[i%len(v.script)], w, &t)
		out = append(out, capture(i, &b))
	}
	return out
}

func goldenPath(name string) string {
	return filepath.Join("testdata", name+".json")
}

func TestGoldenVectors(t *testing.T) {
	for _, v := range goldenVectors() {
		t.Run(v.name, func(t *testing.T) {
			got := v.record()

			if *update {
				blob, err := json.MarshalIndent(v.build(), "", "  ")
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if err := os.WriteFile(goldenPath(v.name), append(blob, '\n'), 0o644); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
				t.Logf("updated %s", goldenPath(v.name))
				return
			}

			blob, err := os.ReadFile(goldenPath(v.name))
			if err != nil {
				t.Fatalf("read fixture (run with -update to create it): %v", err)
			}
			var fx fixture
			if err := json.Unmarshal(blob, &fx); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			want := fx.Frames

			if len(got) != len(want) {
				t.Fatalf("recorded %d frames, fixture has %d", len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("tick %d diverged from fixture:\n got  %+v\n want %+v\n"+
						"Movement changed. If that was intended, rerun with -update and "+
						"commit the fixtures alongside the change.", i, got[i], want[i])
				}
			}
		})
	}
}

// Every vector must actually exercise the simulation. A script that leaves the
// body motionless would pass the golden comparison forever while testing
// nothing, which is the classic way a golden corpus quietly rots.
func TestGoldenVectorsAreNonTrivial(t *testing.T) {
	for _, v := range goldenVectors() {
		frames := v.record()
		first := frames[0]
		moved := false
		for _, f := range frames {
			if f.X != first.X || f.Y != first.Y {
				moved = true
				break
			}
		}
		if !moved {
			t.Errorf("vector %q never moves the body and so tests nothing", v.name)
		}
	}
}
