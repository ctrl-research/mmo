package sim

import "github.com/ctrl-research/mmo/internal/fixed"

// groundProbeHeight is the depth of the box tested just below a body to decide
// whether it is standing on something. Small enough not to catch geometry the
// body is merely near, large enough to survive fixed-point rounding at the
// moment of contact.
const groundProbeHeight = fixed.One / 4

// Step advances one body by exactly one tick.
//
// It is the single entry point to the simulation, and it must stay
// deterministic: the client compiles this same code to WebAssembly and replays
// it to predict local movement. That imposes hard constraints on everything it
// touches — no floating point, no wall-clock reads, no map iteration, no
// goroutines, no allocation that depends on address values. See
// docs/architecture.md for why, and AGENTS.md invariant 3 for the rule.
//
// Given identical (Body, Input, World, Tuning), Step produces an identical
// Body on every platform, every time.
func Step(b *Body, in Input, w *World, t *Tuning) {
	moveX := in.clampMoveX()

	jumpPressed := in.Jump && !b.JumpHeld

	advanceTimers(b, t, jumpPressed)

	if b.Climbing {
		stepClimbing(b, in, moveX, w, t, jumpPressed)
	} else {
		stepAirborne(b, in, moveX, w, t, jumpPressed)
	}

	integrate(b, w, t)

	// Grounded is recomputed from geometry after moving rather than being set
	// during collision resolution. A body standing still has no downward
	// movement to collide with, so resolution alone would report it as
	// airborne every tick it does not move.
	b.Grounded = !b.Climbing && b.Vel.Y >= 0 && onGround(b, w)
	if b.Grounded {
		b.Coyote = t.CoyoteTicks
	}

	clampToBounds(b, w)

	b.JumpHeld = in.Jump
}

func advanceTimers(b *Body, t *Tuning, jumpPressed bool) {
	if b.Coyote > 0 {
		b.Coyote--
	}
	if b.DropThrough > 0 {
		b.DropThrough--
	}

	// A fresh press refills the buffer; otherwise it drains. This is what lets
	// a jump pressed just before landing still fire on touchdown.
	if jumpPressed {
		b.JumpBuffer = t.JumpBufTicks
	} else if b.JumpBuffer > 0 {
		b.JumpBuffer--
	}
}

// stepAirborne handles ordinary running, falling, and jumping. The name covers
// the grounded case too: the two differ only in their acceleration constants
// and whether a jump is available.
func stepAirborne(b *Body, in Input, moveX int32, w *World, t *Tuning, jumpPressed bool) {
	if c, ok := grabbableClimbable(b, in, w); ok {
		enterClimb(b, c)
		return
	}

	accel, fric := t.AirAccel, t.AirFric
	if b.Grounded {
		accel, fric = t.GroundAccel, t.GroundFric
	}

	if moveX != 0 {
		target := t.RunSpeed.MulRatio(int(moveX), 1000)
		b.Vel.X = b.Vel.X.Approach(target, accel)
		b.FacingLeft = moveX < 0
	} else {
		b.Vel.X = b.Vel.X.ApproachZero(fric)
	}

	// Down plus jump drops through a one-way platform. Requiring both, rather
	// than down alone, means crouching or looking down never drops you by
	// accident.
	if in.Down && jumpPressed && b.Grounded {
		b.DropThrough = t.DropThruTick
		b.JumpBuffer = 0
		b.Grounded = false
	} else if b.JumpBuffer > 0 && (b.Grounded || b.Coyote > 0) {
		b.Vel.Y = -t.JumpVel
		b.Grounded = false
		b.Coyote = 0
		b.JumpBuffer = 0
	}

	// Releasing jump while still rising cuts the remaining upward velocity,
	// giving a jump whose height depends on how long the button is held.
	//
	// This fires only on the release edge. Applying it every tick the button
	// is up compounds the cut geometrically and reduces a tapped jump to
	// almost nothing.
	if !in.Jump && b.JumpHeld && b.Vel.Y < 0 {
		b.Vel.Y = b.Vel.Y.MulRatio(t.JumpCutNum, t.JumpCutDen)
	}

	b.Vel.Y = min(b.Vel.Y+t.Gravity, t.TerminalVel)
}

// stepClimbing handles movement on a rope or ladder: no gravity, locked to the
// centre of the climbable, vertical speed straight from input.
func stepClimbing(b *Body, in Input, moveX int32, w *World, t *Tuning, jumpPressed bool) {
	c, ok := overlappingClimbable(b, w)
	if !ok {
		// Climbed off the end of the rope.
		b.Climbing = false
		return
	}

	// Jumping off pushes up and away, so a player can leave a rope mid-climb.
	if jumpPressed {
		b.Climbing = false
		b.Vel.Y = -t.ClimbOffVel
		if moveX != 0 {
			b.Vel.X = t.RunSpeed.MulRatio(int(moveX), 1000)
			b.FacingLeft = moveX < 0
		}
		return
	}

	b.Vel.X = 0
	b.Pos.X = c.CenterX() - b.W/2

	switch {
	case in.Up:
		b.Vel.Y = -t.ClimbSpeed
	case in.Down:
		b.Vel.Y = t.ClimbSpeed
	default:
		b.Vel.Y = 0
	}

	// Climbing down onto solid ground releases the rope, so the player does
	// not end up standing inside the floor still in the climbing state.
	if in.Down && onGround(b, w) {
		b.Climbing = false
		b.Vel.Y = 0
	}
}

func enterClimb(b *Body, c Rect) {
	b.Climbing = true
	b.Grounded = false
	b.Vel = Vec{}
	b.Pos.X = c.CenterX() - b.W/2
}

// grabbableClimbable reports the rope or ladder the body may grab this tick.
//
// Up always grabs. Down only grabs when the climbable actually continues below
// the body's feet, so pressing down at the top of a ladder starts a descent
// while pressing down at its base does nothing.
func grabbableClimbable(b *Body, in Input, w *World) (Rect, bool) {
	if !in.Up && !in.Down {
		return Rect{}, false
	}
	c, ok := overlappingClimbable(b, w)
	if !ok {
		return Rect{}, false
	}
	if in.Up {
		return c, true
	}
	if c.Bottom() > b.Pos.Y+b.H {
		return c, true
	}
	return Rect{}, false
}

func overlappingClimbable(b *Body, w *World) (Rect, bool) {
	box := b.Bounds()
	for i := range w.Climbables {
		if box.OverlapsInclusive(w.Climbables[i]) {
			return w.Climbables[i], true
		}
	}
	return Rect{}, false
}

// integrate applies velocity to position, resolving collisions as it goes.
//
// Movement is split into substeps no larger than Tuning.MaxSubstep. At
// terminal velocity a body covers 40 units in one tick, which is more than a
// 32-unit tile: resolved in a single pass it would pass straight through a
// floor without ever overlapping it.
//
// Each substep's delta is derived from the exact cumulative fraction of the
// total rather than by repeatedly adding a per-substep quotient, so integer
// division cannot accumulate drift across substeps.
func integrate(b *Body, w *World, t *Tuning) {
	if b.Vel.X == 0 && b.Vel.Y == 0 {
		return
	}

	steps := substepCount(b.Vel, t.MaxSubstep)

	var prevX, prevY fixed.F
	for i := 1; i <= steps; i++ {
		curX := b.Vel.X.MulRatio(i, steps)
		curY := b.Vel.Y.MulRatio(i, steps)

		moveAxisX(b, curX-prevX, w)
		moveAxisY(b, curY-prevY, w)

		prevX, prevY = curX, curY
	}
}

func substepCount(vel Vec, maxStep fixed.F) int {
	longest := max(vel.X.Abs(), vel.Y.Abs())
	if longest <= maxStep {
		return 1
	}
	// Ceiling division: enough substeps that none exceeds maxStep.
	return int((longest + maxStep - 1) / maxStep)
}

// moveAxisX moves horizontally and pushes out of solids. One-way platforms are
// deliberately not tested: they only ever block downward movement, so they
// must not stop a body running past them.
func moveAxisX(b *Body, dx fixed.F, w *World) {
	if dx == 0 {
		return
	}
	b.Pos.X += dx

	box := b.Bounds()
	for i := range w.Solids {
		s := w.Solids[i]
		if !box.Overlaps(s) {
			continue
		}
		if dx > 0 {
			b.Pos.X = s.Left() - b.W
		} else {
			b.Pos.X = s.Right()
		}
		b.Vel.X = 0
		box = b.Bounds()
	}
}

// moveAxisY moves vertically and resolves against solids and, when falling,
// one-way platforms.
func moveAxisY(b *Body, dy fixed.F, w *World) {
	if dy == 0 {
		return
	}
	prevBottom := b.Pos.Y + b.H
	b.Pos.Y += dy

	box := b.Bounds()
	for i := range w.Solids {
		s := w.Solids[i]
		if !box.Overlaps(s) {
			continue
		}
		if dy > 0 {
			b.Pos.Y = s.Top() - b.H
		} else {
			b.Pos.Y = s.Bottom()
		}
		b.Vel.Y = 0
		box = b.Bounds()
	}

	// A body on a rope passes through one-way platforms; ropes are authored
	// to cross them, and stopping on one would strand the climber.
	if dy <= 0 || b.DropThrough > 0 || b.Climbing {
		return
	}

	// A one-way platform catches the body only if it was above the platform's
	// surface before this substep. Testing the previous position rather than
	// the current one is what allows jumping up through a platform: on the way
	// up the body starts below the surface and is ignored.
	for i := range w.Platforms {
		p := w.Platforms[i]
		if !box.Overlaps(p) || prevBottom > p.Top() {
			continue
		}
		b.Pos.Y = p.Top() - b.H
		b.Vel.Y = 0
		box = b.Bounds()
	}
}

// onGround tests a thin box just beneath the body.
func onGround(b *Body, w *World) bool {
	probe := Rect{b.Pos.X, b.Pos.Y + b.H, b.W, groundProbeHeight}

	for i := range w.Solids {
		if probe.Overlaps(w.Solids[i]) {
			return true
		}
	}
	if b.DropThrough > 0 || b.Climbing {
		return false
	}
	for i := range w.Platforms {
		p := w.Platforms[i]
		// Only the platform's top surface is standable, and only if the body
		// is resting on it rather than overlapping it from below.
		if probe.Overlaps(p) && b.Pos.Y+b.H <= p.Top()+groundProbeHeight {
			return true
		}
	}
	return false
}

// clampToBounds keeps a body inside the map no matter what.
//
// This is a backstop, not a game mechanic: maps are expected to be sealed by
// solid geometry. If it ever fires, the map has a hole in it, but a player
// stuck at the edge of the world is far better than one falling forever.
func clampToBounds(b *Body, w *World) {
	bd := w.Bounds
	if bd.W == 0 && bd.H == 0 {
		return
	}
	if b.Pos.X < bd.Left() {
		b.Pos.X = bd.Left()
		b.Vel.X = 0
	}
	if b.Pos.X+b.W > bd.Right() {
		b.Pos.X = bd.Right() - b.W
		b.Vel.X = 0
	}
	if b.Pos.Y < bd.Top() {
		b.Pos.Y = bd.Top()
		b.Vel.Y = 0
	}
	if b.Pos.Y+b.H > bd.Bottom() {
		b.Pos.Y = bd.Bottom() - b.H
		b.Vel.Y = 0
		b.Grounded = true
	}
}

// Settle computes a freshly placed body's contact state without advancing time.
//
// Step derives Grounded at the end of a tick, so a body that has just been
// constructed reports itself airborne until it has been stepped once. That one
// tick of disagreement is enough to swallow a jump pressed on the same tick a
// player spawns or arrives through a portal. Call this after positioning a
// body and before its first Step.
func Settle(b *Body, w *World, t *Tuning) {
	b.Grounded = !b.Climbing && onGround(b, w)
	if b.Grounded {
		b.Coyote = t.CoyoteTicks
	}
}
