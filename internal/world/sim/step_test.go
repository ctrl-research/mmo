package sim

import (
	"testing"

	"github.com/ctrl-research/mmo/internal/fixed"
)

// testWorld is a 640x480 room with a floor, a left wall, a one-way platform,
// and a rope hanging from above down to the floor.
//
// The platform sits 70 units above the floor so a full jump (~96) clears it,
// and its x range stops short of the rope so the two never interact.
//
//	                        | rope   x 300..316, y 200..400
//	wall  ______            |        platform x 120..280, y 330
//	|___________________________________________ floor y=400
func testWorld() *World {
	return &World{
		Solids: []Rect{
			RectFromInts(0, 400, 640, 80), // floor
			RectFromInts(0, 0, 16, 400),   // left wall
		},
		Platforms: []Rect{
			RectFromInts(120, platformTop, 160, 8),
		},
		Climbables: []Rect{
			RectFromInts(300, 200, 16, 200),
		},
		Bounds: RectFromInts(0, 0, 640, 480),
	}
}

func newTestBody(feetX, feetY int) Body {
	b := NewBody(
		Vec{fixed.FromInt(feetX), fixed.FromInt(feetY)},
		PlayerSize.W, PlayerSize.H,
	)
	w, t := testWorld(), DefaultTuning()
	Settle(&b, w, &t)
	return b
}

// run advances n ticks with a constant input and returns the world/tuning used.
func run(b *Body, in Input, n int) {
	w, t := testWorld(), DefaultTuning()
	for i := 0; i < n; i++ {
		Step(b, in, w, &t)
	}
}

const (
	floorTop    = 400
	platformTop = 330
)

func TestFallsAndLandsOnFloor(t *testing.T) {
	b := newTestBody(100, 100)
	run(&b, Input{}, 60)

	if !b.Grounded {
		t.Fatal("body should be grounded after falling to the floor")
	}
	if got := b.FeetCenter().Y.Int(); got != floorTop {
		t.Errorf("feet at y=%d, want %d", got, floorTop)
	}
	if b.Vel.Y != 0 {
		t.Errorf("vertical velocity %v after landing, want 0", b.Vel.Y)
	}
}

func TestTerminalVelocityIsRespected(t *testing.T) {
	w := &World{Bounds: RectFromInts(0, 0, 640, 100000)}
	tn := DefaultTuning()
	b := newTestBody(100, 100)

	for i := 0; i < 200; i++ {
		Step(&b, Input{}, w, &tn)
		if b.Vel.Y > tn.TerminalVel {
			t.Fatalf("tick %d: fall speed %v exceeds terminal %v", i, b.Vel.Y, tn.TerminalVel)
		}
	}
	if b.Vel.Y != tn.TerminalVel {
		t.Errorf("fall speed %v after 200 ticks, want terminal %v", b.Vel.Y, tn.TerminalVel)
	}
}

func TestRunReachesMaxSpeedThenStops(t *testing.T) {
	tn := DefaultTuning()
	b := newTestBody(100, floorTop)

	run(&b, Input{MoveX: 1000}, 30)
	if b.Vel.X != tn.RunSpeed {
		t.Errorf("run speed %v, want %v", b.Vel.X, tn.RunSpeed)
	}
	if b.FacingLeft {
		t.Error("should be facing right while running right")
	}

	run(&b, Input{}, 30)
	if b.Vel.X != 0 {
		t.Errorf("horizontal velocity %v after releasing input, want 0", b.Vel.X)
	}
}

func TestPartialMoveInputGivesPartialSpeed(t *testing.T) {
	tn := DefaultTuning()
	b := newTestBody(100, floorTop)
	run(&b, Input{MoveX: 500}, 30)

	want := tn.RunSpeed.MulRatio(500, 1000)
	if b.Vel.X != want {
		t.Errorf("half-input speed %v, want %v", b.Vel.X, want)
	}
}

func TestMoveInputIsClampedForHostileClients(t *testing.T) {
	tn := DefaultTuning()
	b := newTestBody(100, floorTop)
	run(&b, Input{MoveX: 999999}, 30)

	if b.Vel.X != tn.RunSpeed {
		t.Errorf("speed %v with out-of-range input, want clamp to %v", b.Vel.X, tn.RunSpeed)
	}
}

func TestJumpRisesAndReturnsToGround(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(100, floorTop)

	Step(&b, Input{Jump: true}, w, &tn)
	if b.Grounded {
		t.Fatal("should leave the ground on the jump tick")
	}

	apex := b.Pos.Y
	for i := 0; i < 60; i++ {
		Step(&b, Input{Jump: true}, w, &tn)
		apex = min(apex, b.Pos.Y)
		if b.Grounded {
			break
		}
	}

	// Checked against the closed form rather than a hand-written range. A
	// range is what let the jump sit at 84 units for five milestones under a
	// comment claiming 96, while every map was built on 96 gaps.
	height := fixed.FromInt(floorTop) - PlayerSize.H - apex
	if want := MaxJumpHeight(&tn); height != want {
		t.Errorf("jump reaches %v, MaxJumpHeight says %v", height, want)
	}
	if height < fixed.FromInt(StandardGap) {
		t.Errorf("jump reaches %v, below the %d-unit gap the maps are built on",
			height, StandardGap)
	}
	if !b.Grounded {
		t.Error("should be back on the ground within 60 ticks")
	}
}

func TestReleasingJumpEarlyGivesALowerJump(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()

	apexFor := func(holdTicks int) fixed.F {
		b := newTestBody(100, floorTop)
		apex := b.Pos.Y
		for i := 0; i < 60; i++ {
			Step(&b, Input{Jump: i < holdTicks}, w, &tn)
			apex = min(apex, b.Pos.Y)
		}
		return apex
	}

	full, tapped := apexFor(60), apexFor(2)
	if tapped <= full {
		t.Errorf("tapped jump apex %v should be lower (larger y) than held apex %v", tapped, full)
	}
}

func TestCoyoteTimeAllowsJumpJustAfterLeavingLedge(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()

	// Stand on the one-way platform, then walk off its right edge.
	b := newTestBody(270, platformTop)
	Step(&b, Input{}, w, &tn)
	if !b.Grounded {
		t.Fatal("expected to start grounded on the platform")
	}

	for i := 0; i < 20 && b.Grounded; i++ {
		Step(&b, Input{MoveX: 1000}, w, &tn)
	}
	if b.Grounded {
		t.Fatal("expected to walk off the platform edge")
	}

	before := b.Vel.Y
	Step(&b, Input{MoveX: 1000, Jump: true}, w, &tn)
	if b.Vel.Y >= before {
		t.Error("jump within coyote window should produce upward velocity")
	}
}

func TestCoyoteTimeExpires(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(100, 100) // start in mid-air, far from anything

	for i := 0; i < 10; i++ {
		Step(&b, Input{}, w, &tn)
	}
	before := b.Vel.Y
	Step(&b, Input{Jump: true}, w, &tn)

	if b.Vel.Y < before {
		t.Error("jump long after leaving ground should not work")
	}
}

func TestJumpBufferFiresOnLanding(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()

	// How long the fall takes with no input at all.
	probe := newTestBody(100, 300)
	fall := 0
	for ; fall < 60 && !probe.Grounded; fall++ {
		Step(&probe, Input{}, w, &tn)
	}

	// Press jump one tick before touchdown: too early to fire, but inside the
	// buffer window, so it must fire on the tick after landing.
	b := newTestBody(100, 300)
	for i := 0; i < fall; i++ {
		Step(&b, Input{Jump: i == fall-1}, w, &tn)
	}
	if b.Vel.Y < 0 {
		t.Fatal("setup: jump fired before landing")
	}

	fired := false
	for i := 0; i < int(tn.JumpBufTicks); i++ {
		Step(&b, Input{}, w, &tn)
		if b.Vel.Y < 0 {
			fired = true
			break
		}
	}
	if !fired {
		t.Error("a jump pressed just before landing should fire on touchdown")
	}
}

// Holding jump through a fall must not auto-hop on landing. Buffering exists
// to forgive a press that was slightly early, not to turn a held button into
// repeated jumps.
func TestHeldJumpDoesNotAutoHopOnLanding(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(100, 200)

	for i := 0; i < 60 && !b.Grounded; i++ {
		Step(&b, Input{Jump: true}, w, &tn)
	}
	if !b.Grounded {
		t.Fatal("setup: expected to land")
	}
	for i := 0; i < 10; i++ {
		Step(&b, Input{Jump: true}, w, &tn)
		if b.Vel.Y < 0 {
			t.Fatal("holding jump auto-hopped on landing")
		}
	}
}

func TestCannotDoubleJump(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(100, floorTop)

	Step(&b, Input{Jump: true}, w, &tn)
	for i := 0; i < 4; i++ {
		Step(&b, Input{}, w, &tn) // release, drain coyote
	}

	before := b.Vel.Y
	Step(&b, Input{Jump: true}, w, &tn) // fresh press, mid-air
	if b.Vel.Y < before {
		t.Error("a second jump in mid-air should not be possible")
	}
}

func TestWallBlocksHorizontalMovement(t *testing.T) {
	b := newTestBody(100, floorTop)
	run(&b, Input{MoveX: -1000}, 120)

	// The wall occupies x 0..16; the body is 24 wide.
	if got := b.Pos.X.Int(); got != 16 {
		t.Errorf("body stopped at x=%d, want 16 (against the wall)", got)
	}
	if b.Vel.X != 0 {
		t.Errorf("horizontal velocity %v against a wall, want 0", b.Vel.X)
	}
}

func TestLandsOnOneWayPlatformFromAbove(t *testing.T) {
	b := newTestBody(200, 250) // above the platform
	run(&b, Input{}, 30)

	if !b.Grounded {
		t.Fatal("should land on the one-way platform")
	}
	if got := b.FeetCenter().Y.Int(); got != platformTop {
		t.Errorf("feet at y=%d, want %d (platform top)", got, platformTop)
	}
}

func TestJumpsUpThroughOneWayPlatform(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(200, floorTop) // on the floor, platform is 70 above

	passed := false
	for i := 0; i < 40; i++ {
		Step(&b, Input{Jump: true}, w, &tn)
		if b.Pos.Y+b.H < fixed.FromInt(platformTop) {
			passed = true
			break
		}
	}
	if !passed {
		t.Fatal("should be able to jump up through a one-way platform")
	}

	// ...and then land on it on the way back down.
	for i := 0; i < 40 && !b.Grounded; i++ {
		Step(&b, Input{}, w, &tn)
	}
	if got := b.FeetCenter().Y.Int(); got != platformTop {
		t.Errorf("feet at y=%d after descending, want %d (landed on platform)", got, platformTop)
	}
}

func TestDropThroughPlatformWithDownAndJump(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(200, 250)

	for i := 0; i < 30 && !b.Grounded; i++ {
		Step(&b, Input{}, w, &tn)
	}
	if b.FeetCenter().Y.Int() != platformTop {
		t.Fatal("setup: expected to be standing on the platform")
	}

	Step(&b, Input{Down: true, Jump: true}, w, &tn)
	for i := 0; i < 60 && !b.Grounded; i++ {
		Step(&b, Input{Down: true}, w, &tn)
	}

	if got := b.FeetCenter().Y.Int(); got != floorTop {
		t.Errorf("feet at y=%d after dropping through, want %d (the floor)", got, floorTop)
	}
}

func TestDownAloneDoesNotDropThrough(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(200, 250)

	for i := 0; i < 30 && !b.Grounded; i++ {
		Step(&b, Input{}, w, &tn)
	}
	for i := 0; i < 30; i++ {
		Step(&b, Input{Down: true}, w, &tn)
	}

	if got := b.FeetCenter().Y.Int(); got != platformTop {
		t.Errorf("feet at y=%d holding down, want %d - down alone must not drop through", got, platformTop)
	}
}

// A body at terminal velocity covers more than a tile per tick. Without
// substepping it would pass straight through a thin floor.
func TestNoTunnelingThroughThinFloorAtTerminalVelocity(t *testing.T) {
	w := &World{
		Solids: []Rect{RectFromInts(0, 1000, 640, 8)}, // 8 units thin
		Bounds: RectFromInts(0, 0, 640, 2000),
	}
	tn := DefaultTuning()
	b := newTestBody(100, 0)

	for i := 0; i < 200; i++ {
		Step(&b, Input{}, w, &tn)
		if b.Grounded {
			break
		}
	}

	if !b.Grounded {
		t.Fatal("body tunnelled through the thin floor")
	}
	if got := b.FeetCenter().Y.Int(); got != 1000 {
		t.Errorf("feet at y=%d, want 1000 (thin floor top)", got)
	}
}

func TestGrabsRopeWithUp(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(308, floorTop) // standing at the rope's base

	Step(&b, Input{Up: true}, w, &tn)
	if !b.Climbing {
		t.Fatal("pressing up while overlapping a rope should start climbing")
	}

	// Locked to the rope's centre: rope spans x 300..316, centre 308.
	if got := (b.Pos.X + b.W/2).Int(); got != 308 {
		t.Errorf("body centre x=%d while climbing, want 308 (rope centre)", got)
	}
}

func TestClimbsUpAndDownRope(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(308, floorTop)

	Step(&b, Input{Up: true}, w, &tn)
	startY := b.Pos.Y
	for i := 0; i < 10; i++ {
		Step(&b, Input{Up: true}, w, &tn)
	}
	if b.Pos.Y >= startY {
		t.Error("holding up on a rope should move the body upward")
	}
	if !b.Climbing {
		t.Error("should still be climbing")
	}

	upY := b.Pos.Y
	for i := 0; i < 5; i++ {
		Step(&b, Input{Down: true}, w, &tn)
	}
	if b.Pos.Y <= upY {
		t.Error("holding down on a rope should move the body downward")
	}
}

func TestNoGravityWhileClimbing(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(308, floorTop)

	Step(&b, Input{Up: true}, w, &tn)
	for i := 0; i < 5; i++ {
		Step(&b, Input{Up: true}, w, &tn)
	}
	held := b.Pos.Y
	for i := 0; i < 20; i++ {
		Step(&b, Input{}, w, &tn) // no input at all
	}

	if b.Pos.Y != held {
		t.Errorf("body drifted from %v to %v while idle on a rope", held, b.Pos.Y)
	}
}

func TestJumpOffRope(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(308, floorTop)

	Step(&b, Input{Up: true}, w, &tn)
	for i := 0; i < 5; i++ {
		Step(&b, Input{Up: true}, w, &tn)
	}

	Step(&b, Input{Jump: true}, w, &tn)
	if b.Climbing {
		t.Error("jumping should release the rope")
	}
	if b.Vel.Y >= 0 {
		t.Errorf("jumping off a rope should push upward, got vy=%v", b.Vel.Y)
	}
}

func TestClimbingDownToGroundReleasesRope(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(308, 250)

	Step(&b, Input{Up: true}, w, &tn)
	if !b.Climbing {
		t.Fatal("setup: expected to grab the rope")
	}
	for i := 0; i < 60; i++ {
		Step(&b, Input{Down: true}, w, &tn)
		if !b.Climbing {
			break
		}
	}

	if b.Climbing {
		t.Error("climbing down to the floor should release the rope")
	}
	if got := b.FeetCenter().Y.Int(); got != floorTop {
		t.Errorf("feet at y=%d, want %d", got, floorTop)
	}
}

func TestBoundsClampIsABackstop(t *testing.T) {
	w := &World{Bounds: RectFromInts(0, 0, 640, 480)} // no geometry at all
	tn := DefaultTuning()
	b := newTestBody(100, 100)

	for i := 0; i < 500; i++ {
		Step(&b, Input{MoveX: 1000}, w, &tn)
	}

	if b.Pos.X+b.W > fixed.FromInt(640) {
		t.Errorf("body escaped bounds to the right: x=%v", b.Pos.X)
	}
	if b.Pos.Y+b.H > fixed.FromInt(480) {
		t.Errorf("body escaped bounds downward: y=%v", b.Pos.Y)
	}
}

// The determinism guarantee the whole prediction model rests on.
func TestStepIsDeterministic(t *testing.T) {
	script := []Input{
		{MoveX: 1000}, {MoveX: 1000, Jump: true}, {MoveX: 1000, Jump: true},
		{MoveX: 500}, {}, {MoveX: -1000}, {MoveX: -1000, Jump: true},
		{Up: true}, {Up: true}, {Down: true}, {Down: true, Jump: true},
	}

	runScript := func() Body {
		w, tn := testWorld(), DefaultTuning()
		b := newTestBody(308, floorTop)
		for i := 0; i < 40; i++ {
			Step(&b, script[i%len(script)], w, &tn)
		}
		return b
	}

	first := runScript()
	for i := 0; i < 50; i++ {
		if got := runScript(); got != first {
			t.Fatalf("run %d diverged:\n got %+v\nwant %+v", i, got, first)
		}
	}
}

// Replaying from an intermediate state must match running straight through.
// This is exactly what client reconciliation does: snap to an authoritative
// Body, replay the unacknowledged inputs, and expect to arrive where the
// server will.
func TestReplayFromIntermediateStateMatches(t *testing.T) {
	script := []Input{
		{MoveX: 1000}, {MoveX: 1000, Jump: true}, {MoveX: 1000},
		{MoveX: 1000}, {}, {MoveX: -1000}, {MoveX: -1000},
		{MoveX: -1000, Jump: true}, {}, {},
	}
	w, tn := testWorld(), DefaultTuning()

	straight := newTestBody(200, floorTop)
	for _, in := range script {
		Step(&straight, in, w, &tn)
	}

	const split = 4
	resumed := newTestBody(200, floorTop)
	for _, in := range script[:split] {
		Step(&resumed, in, w, &tn)
	}
	// Serialising and restoring the body must lose nothing.
	snapshot := resumed
	replay := snapshot
	for _, in := range script[split:] {
		Step(&replay, in, w, &tn)
	}

	if replay != straight {
		t.Errorf("replay diverged from continuous run:\n replay %+v\n direct %+v", replay, straight)
	}
}

// --- codec ------------------------------------------------------------------

func TestBodyCodecRoundTrips(t *testing.T) {
	w, tn := testWorld(), DefaultTuning()
	b := newTestBody(308, floorTop)

	// Exercise a body in a non-trivial state rather than a fresh one: the
	// fields most likely to be dropped by a codec bug are the small counters.
	script := []Input{{Up: true}, {Up: true}, {MoveX: -1000, Jump: true}, {}, {Down: true}}
	for i := 0; i < 12; i++ {
		Step(&b, script[i%len(script)], w, &tn)
	}

	buf := make([]byte, BodySize)
	MarshalBody(buf, &b)

	var got Body
	UnmarshalBody(buf, &got)

	if got != b {
		t.Errorf("body did not survive the round trip:\n got  %+v\n want %+v", got, b)
	}
}

func TestBodyCodecCoversEveryField(t *testing.T) {
	b := Body{
		Pos: Vec{fixed.FromInt(-1234), fixed.FromInt(5678)},
		Vel: Vec{fixed.FromRatio(-7, 3), fixed.FromRatio(11, 5)},
		W:   fixed.FromInt(24), H: fixed.FromInt(48),
		Grounded: true, Climbing: true, FacingLeft: true, JumpHeld: true,
		Coyote: 3, JumpBuffer: 4, DropThrough: 5,
	}

	buf := make([]byte, BodySize)
	MarshalBody(buf, &b)
	var got Body
	UnmarshalBody(buf, &got)

	if got != b {
		t.Errorf("round trip lost data:\n got  %+v\n want %+v", got, b)
	}
}

func TestWorldCodecRoundTrips(t *testing.T) {
	want := testWorld()

	got, ok := UnmarshalWorld(MarshalWorld(want))
	if !ok {
		t.Fatal("UnmarshalWorld rejected its own output")
	}

	if len(got.Solids) != len(want.Solids) ||
		len(got.Platforms) != len(want.Platforms) ||
		len(got.Climbables) != len(want.Climbables) {
		t.Fatalf("section counts differ: %d/%d/%d, want %d/%d/%d",
			len(got.Solids), len(got.Platforms), len(got.Climbables),
			len(want.Solids), len(want.Platforms), len(want.Climbables))
	}
	if got.Bounds != want.Bounds {
		t.Errorf("bounds = %+v, want %+v", got.Bounds, want.Bounds)
	}
	for i := range want.Solids {
		if got.Solids[i] != want.Solids[i] {
			t.Errorf("solid %d = %+v, want %+v", i, got.Solids[i], want.Solids[i])
		}
	}
	for i := range want.Climbables {
		if got.Climbables[i] != want.Climbables[i] {
			t.Errorf("climbable %d = %+v, want %+v", i, got.Climbables[i], want.Climbables[i])
		}
	}
}

// A decoded world must produce identical simulation results to the original,
// which is the property the client actually depends on.
func TestDecodedWorldSimulatesIdentically(t *testing.T) {
	orig := testWorld()
	decoded, ok := UnmarshalWorld(MarshalWorld(orig))
	if !ok {
		t.Fatal("UnmarshalWorld failed")
	}
	tn := DefaultTuning()

	script := []Input{
		{MoveX: 1000}, {MoveX: 1000, Jump: true}, {}, {MoveX: -1000},
		{Up: true}, {Up: true}, {Down: true, Jump: true},
	}

	a := NewBody(Vec{fixed.FromInt(308), fixed.FromInt(floorTop)}, PlayerSize.W, PlayerSize.H)
	b := a
	Settle(&a, orig, &tn)
	Settle(&b, decoded, &tn)

	for i := 0; i < 120; i++ {
		in := script[i%len(script)]
		Step(&a, in, orig, &tn)
		Step(&b, in, decoded, &tn)
		if a != b {
			t.Fatalf("tick %d diverged between original and decoded world:\n orig %+v\n dec  %+v", i, a, b)
		}
	}
}

// A malformed buffer means the two sides disagree about the layout. That must
// fail rather than be guessed at, or the client silently simulates a different
// world from the server.
func TestUnmarshalWorldRejectsMalformedInput(t *testing.T) {
	good := MarshalWorld(testWorld())

	cases := map[string][]byte{
		"empty":          {},
		"short header":   good[:4],
		"truncated body": good[:len(good)-1],
		"trailing bytes": append(append([]byte{}, good...), 0),
	}
	for name, buf := range cases {
		if _, ok := UnmarshalWorld(buf); ok {
			t.Errorf("%s: accepted a malformed world buffer", name)
		}
	}
}

func TestInputCodecRoundTrips(t *testing.T) {
	inputs := []Input{
		{},
		{MoveX: 1000, Jump: true, Up: true, Down: true},
		{MoveX: -1000},
		{MoveX: 0, Jump: true},
		{MoveX: 437, Up: true},
		{MoveX: -1, Down: true},
	}
	for _, want := range inputs {
		if got := DecodeInput(EncodeInput(want)); got != want {
			t.Errorf("round trip of %+v gave %+v", want, got)
		}
	}
}

func TestInputCodecClampsHostileValues(t *testing.T) {
	got := DecodeInput(EncodeInput(Input{MoveX: 30000}))
	if got.MoveX != 1000 {
		t.Errorf("MoveX = %d after encoding an out-of-range value, want 1000", got.MoveX)
	}
	got = DecodeInput(EncodeInput(Input{MoveX: -30000}))
	if got.MoveX != -1000 {
		t.Errorf("MoveX = %d, want -1000", got.MoveX)
	}
}
