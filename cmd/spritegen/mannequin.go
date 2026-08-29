package main

// The pixel mannequin.
//
// A stand-in, not a character: it has to read as a person at 24 pixels wide,
// show which way it is facing, and make its state obvious in motion. Anything
// more is art direction, and this is not the milestone for that.
//
// Poses come from limb offsets rather than hand-drawn frames. Six numbers
// describe a pose, so a walk cycle is a table rather than a drawing, and
// changing the proportions changes every frame at once instead of eight.

// The body matches sim.PlayerSize exactly: a sprite that does not match its
// hitbox makes every "that should have hit" argument unanswerable.
//
// The frame is wider, because a swung sword and a thrown-out arm leave the
// body's footprint and would otherwise be clipped. The renderer draws the
// frame offset by the padding, so the body still lands on the hitbox.
const (
	bodyW = 24
	bodyH = 48

	// Wide enough for the sword at full extension: the swing frame reaches
	// further from the body than anything else the mannequin does.
	framePad = 16
	manW     = bodyW + framePad*2
	manH     = bodyH
)

// pose is one frame's worth of limb placement, in pixels from the neutral
// stand. Positive lift raises, positive swing moves in the facing direction.
type pose struct {
	bodyLift  int
	armFront  int // swing of the arm on the camera side
	armBack   int
	legFront  int
	legBack   int
	armRaised bool // arm held out rather than down, for climbing and attacks
}

// The walk cycle: contact, pass, contact, pass, with the arms opposing the
// legs. Four frames is enough to read as walking and few enough that each one
// can be looked at.
//
// The amplitudes are small because the figure is 24 pixels wide. A stride that
// looks right on paper reads as splits at this size.
var runCycle = []pose{
	{legFront: 3, legBack: -3, armFront: -2, armBack: 2},
	{bodyLift: 2, legFront: -1, legBack: 1, armFront: 0, armBack: 0},
	{legFront: -3, legBack: 3, armFront: 2, armBack: -2},
	{bodyLift: 2, legFront: 1, legBack: -1, armFront: 0, armBack: 0},
}

// Idle breathes rather than standing perfectly still. A motionless character
// reads as a frozen game.
var idleCycle = []pose{
	{},
	{bodyLift: 1},
}

var (
	jumpPose   = pose{bodyLift: 4, legFront: 2, legBack: -3, armFront: -3, armBack: -3, armRaised: true}
	fallPose   = pose{bodyLift: 1, legFront: -2, legBack: 2, armFront: -4, armBack: -4, armRaised: true}
	climbPoses = []pose{
		{bodyLift: 2, legFront: 2, legBack: -2, armFront: -4, armBack: 2, armRaised: true},
		{bodyLift: 2, legFront: -2, legBack: 2, armFront: 2, armBack: -4, armRaised: true},
	}
	// Wind up, then swing through. Two frames, because the swing is what the
	// server has already decided -- the animation reports it, it does not
	// negotiate it.
	attackPoses = []pose{
		{bodyLift: 1, armFront: -5, armBack: 2, legFront: 1, legBack: -2, armRaised: true},
		{armFront: 6, armBack: -2, legFront: 3, legBack: -3},
	}
)

// drawMannequin draws one frame facing right.
//
// Facing right only: the renderer mirrors for left, which halves the sheet and
// guarantees the two directions cannot drift apart.
func drawMannequin(p pose, weapon bool) *canvas {
	c := newCanvas(manW, manH)

	// Proportions: a head about a fifth of the height, which is stylised
	// enough to read at this size without looking like a child.
	const (
		headW, headH = 10, 9
		torsoW       = 10
		torsoH       = 15
		legH         = 13
		footH        = 2
	)

	cx := framePad + bodyW/2

	// The feet stay planted and the body rises off them: a lift applied to the
	// whole figure floats it, which turns an idle breath into a hover.
	groundY := manH - footH
	hipY := groundY - legH + p.bodyLift
	torsoY := hipY - torsoH
	headY := torsoY - headH

	// Legs first, so the torso overlaps them at the hip rather than the other
	// way round.
	drawLeg(c, cx-3, hipY, groundY-hipY, footH, p.legBack, cloth, metal, true)
	drawLeg(c, cx+1, hipY, groundY-hipY, footH, p.legFront, cloth, metal, false)

	// Torso.
	c.shaded(cx-torsoW/2, torsoY, torsoW, torsoH, cloth)
	// A belt, which is most of what separates "person" from "blue rectangle".
	c.rect(cx-torsoW/2, torsoY+torsoH-4, torsoW, 2, metal.shadow)
	c.rect(cx-1, torsoY+torsoH-4, 2, 2, metal.light)

	// Arms. The far one first and a shade darker, so the near one reads as in
	// front of the torso rather than beside it.
	drawArm(c, cx-torsoW/2-2, torsoY+2, p.armBack, p.armRaised,
		ramp{skin.shadow, skin.shadow, skin.body}, ramp{cloth.shadow, cloth.shadow, cloth.body})
	drawArm(c, cx+torsoW/2-1, torsoY+2, p.armFront, p.armRaised, skin, cloth)

	// Head, with the face on the facing side so direction is readable even
	// when standing still.
	c.shaded(cx-headW/2, headY, headW, headH, skin)
	// Hair as a cap, darker than the skin so the silhouette has a top.
	c.rect(cx-headW/2, headY, headW, 3, wood.shadow)
	c.rect(cx-headW/2, headY, headW, 1, wood.body)
	// One eye: two would need a nose between them to not read as a face-on
	// stare, and there is no room for a nose.
	c.set(cx+2, headY+5, outline)
	c.set(cx+3, headY+5, outline)

	if weapon {
		// From the hand, so the sword goes where the arm went rather than
		// where the arm usually is.
		drawSword(c, cx+torsoW/2-1+p.armFront, torsoY+2+armDrop(p.armRaised))
	}

	c.outlineSprite(outline)
	return c
}

// armDrop is how far down the shoulder an arm hangs, which is the difference
// between a raised arm and a resting one.
func armDrop(raised bool) int {
	if raised {
		return -2
	}
	return 0
}

// drawLeg draws a leg in two segments, hinged at the knee.
//
// The hip stays where the body is and the foot swings, which is what a leg
// does. Sliding the whole leg sideways -- the obvious thing, and what this
// did first -- reads as the character doing the splits with stiff legs.
func drawLeg(c *canvas, x, hipY, legH, footH, swing int, trouser, boot ramp, back bool) {
	shade, bootShade := trouser, boot
	if back {
		// The far leg is a shade darker, which is the cheapest possible depth
		// cue and the one that makes a walk cycle legible.
		shade = ramp{trouser.shadow, trouser.shadow, trouser.body}
		bootShade = ramp{boot.shadow, boot.shadow, boot.body}
	}

	thighH := legH / 2
	shinH := legH - thighH

	// The hip stays under the torso for the same reason the shoulder stays on
	// it: a leg that starts away from the body is a leg that fell off.
	c.shaded(x, hipY, 3, 3, shade)

	// The thigh leans half as far as the foot travels; the shin makes up the
	// rest. Two segments is enough of a joint at this size.
	c.shaded(x+swing/2, hipY+1, 3, thighH, shade)
	c.shaded(x+swing, hipY+thighH, 3, shinH, shade)

	// A forward foot points forward; a trailing one lifts at the heel.
	footX := x + swing - 1
	c.rect(footX, hipY+legH, 5, footH, bootShade.body)
	c.rect(footX, hipY+legH, 5, 1, bootShade.light)
}

// drawArm draws an arm in two segments, hinged at the elbow.
func drawArm(c *canvas, x, shoulderY, swing int, raised bool, hand, sleeve ramp) {
	const length = 11
	y := shoulderY + armDrop(raised)

	upper := length / 2
	lower := length - upper

	// The shoulder stays on the torso whatever the arm is doing. Without it a
	// swung arm floats beside the body with daylight between them, which is
	// the single most obvious way assembled pixel art looks assembled.
	c.shaded(x, y, 3, 3, sleeve)

	c.shaded(x+swing/2, y+1, 3, upper, sleeve)
	c.shaded(x+swing, y+upper, 3, lower-2, sleeve)

	c.rect(x+swing, y+length-3, 3, 3, hand.body)
	c.rect(x+swing, y+length-3, 3, 1, hand.light)
}

func drawSword(c *canvas, x, y int) {
	// Held forward from the hand, so an attack frame reads as a swing rather
	// than a lunge. The guard sits behind the hand and the blade ahead of it.
	hand := y + 8

	c.rect(x-1, hand-1, 2, 5, wood.body)
	c.rect(x-1, hand-1, 2, 1, wood.light)

	c.rect(x+1, hand, 13, 3, metal.body)
	c.rect(x+1, hand, 13, 1, metal.light)
	c.rect(x+1, hand+2, 13, 1, metal.shadow)
	// A point rather than a blunt end.
	c.rect(x+14, hand+1, 2, 1, metal.light)
}
