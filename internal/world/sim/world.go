package sim

import "github.com/ctrl-research/mmo/internal/fixed"

// World is the static collision geometry of one map.
//
// It is built once at load time from the map file and is immutable and shared:
// every room instance on a node, and every body inside them, reads the same
// World concurrently without locking. Nothing in this package writes to it.
type World struct {
	// Solids block movement from every direction.
	Solids []Rect

	// Platforms block movement downward only. A body may jump up through one
	// and land on top of it, which is the MapleStory and OSRS-platformer
	// convention and the basis of vertical map layout.
	Platforms []Rect

	// Climbables are ropes and ladders. They do not block movement; they
	// enable the climbing state while a body overlaps one.
	Climbables []Rect

	// Bounds is the playable extent. Bodies are clamped inside it, so a
	// physics bug can never put an entity outside the map.
	Bounds Rect
}

// Tuning holds every movement constant.
//
// These are parameters rather than package constants because balance values
// belong in content (see docs/content-pipeline.md). Until the content loader
// lands they come from DefaultTuning, but the simulation itself never reaches
// for a global.
//
// All rates are per tick, not per second, so the simulation never divides by a
// tick rate at runtime. At 20 Hz one tick is 50 ms.
type Tuning struct {
	Gravity      fixed.F // downward acceleration per tick
	TerminalVel  fixed.F // maximum fall speed
	RunSpeed     fixed.F // maximum horizontal speed
	GroundAccel  fixed.F // horizontal acceleration while grounded
	GroundFric   fixed.F // horizontal deceleration while grounded, no input
	AirAccel     fixed.F // horizontal acceleration while airborne
	AirFric      fixed.F // horizontal deceleration while airborne, no input
	JumpVel      fixed.F // upward speed applied on jump
	JumpCutNum   int     // numerator of the velocity kept when jump is released
	JumpCutDen   int     // denominator of the same
	ClimbSpeed   fixed.F // vertical speed on a rope or ladder
	ClimbOffVel  fixed.F // upward speed when jumping off a rope
	CoyoteTicks  uint8   // ticks after leaving ground during which a jump still works
	JumpBufTicks uint8   // ticks a jump press is remembered before landing
	DropThruTick uint8   // ticks platforms are ignored after pressing down
	MaxSubstep   fixed.F // largest movement resolved in one collision pass
}

// DefaultTuning is the movement feel: a 144-unit jump reaching its apex in 9
// ticks (0.45 s), and a run speed of 8 units per tick (160/s).
//
// With 32-unit tiles that is a four-and-a-half-tile jump and five tiles per
// second, close to MapleStory's pacing.
//
// The jump is sized against the maps rather than chosen and hoped for. Every
// platform in every map sits 96 units -- three tiles -- above the surface
// below it, and the interesting jumps are diagonal: 96 up and 96 across.
// Clearing that needs both height and air time, which is why gravity is not
// simply raised alongside the jump to keep the arc snappy: higher gravity
// gives a shorter hang and less horizontal reach at height, and the diagonal
// is what runs out first. MaxJumpHeight is the number to check a map against.
func DefaultTuning() Tuning {
	return Tuning{
		Gravity:     fixed.FromInt(4),
		TerminalVel: fixed.FromInt(40),

		RunSpeed:    fixed.FromInt(8),
		GroundAccel: fixed.FromInt(2),
		GroundFric:  fixed.FromRatio(5, 2),
		AirAccel:    fixed.FromInt(1),
		AirFric:     fixed.FromRatio(3, 10),

		JumpVel:    fixed.FromInt(36),
		JumpCutNum: 1,
		JumpCutDen: 2,

		ClimbSpeed:  fixed.FromInt(5),
		ClimbOffVel: fixed.FromInt(20),

		// Coyote time and jump buffering do not change what is possible, only
		// how forgiving the controls feel. Both are worth having from the
		// start: without them movement reads as unresponsive, and that is the
		// hardest kind of feedback to act on later.
		CoyoteTicks:  4,
		JumpBufTicks: 4,
		DropThruTick: 6,

		// Half a 32-unit tile. Terminal velocity is 40 units per tick, so a
		// single-pass move could otherwise skip straight through a floor.
		MaxSubstep: fixed.FromInt(16),
	}
}

// MaxJumpHeight is how far a held jump lifts a body, in world units.
//
// Closed form rather than simulated, because it is exact and because the
// callers are tools and tests that have no world to step against: content
// validation checks that no platform sits further above the ground beneath it
// than this, which is the property that makes a map traversable at all.
//
// The body rises while velocity is upward, gaining v0-g, then v0-2g, and so on
// until the step where velocity would turn downward. That is n = floor(v0/g)
// steps, summing to n*v0 - g*n*(n+1)/2.
func MaxJumpHeight(t *Tuning) fixed.F {
	if t.Gravity <= 0 || t.JumpVel <= 0 {
		return 0
	}

	// Both are fixed-point, so dividing the raw values gives the step count
	// directly. Multiplying a fixed value by a plain count is a raw multiply:
	// going through Mul would scale it twice.
	n := int64(t.JumpVel) / int64(t.Gravity)
	triangular := n * (n + 1) / 2

	return fixed.F(int64(t.JumpVel)*n - int64(t.Gravity)*triangular)
}

// JumpReach is how far a running jump travels horizontally while at least
// `height` above where it launched.
//
// This is the number that decides whether a diagonal ledge is reachable, and
// it is not the same as the jump's total distance: near the apex a body is
// barely moving upward but is still moving sideways, and near the ground it
// has the whole arc left. A map with 96-unit steps and 96-unit horizontal
// spacing needs reach at height, not reach at ground level.
//
// The arc is integrated rather than solved: it has to match Step's ordering
// exactly, and the ordering is the part that would drift. No world is needed,
// because this is the arc through open air -- which is the case a map wants
// checked.
func JumpReach(t *Tuning, height fixed.F) fixed.F {
	if t.Gravity <= 0 || t.JumpVel <= 0 || t.RunSpeed <= 0 {
		return 0
	}

	// Step applies gravity, then moves. Matching that here is what keeps this
	// honest against the simulation rather than approximately like it.
	vel := -t.JumpVel

	var risen fixed.F
	var lastAbove int64

	// Counted from launch, not from when the height is first reached: landing
	// on a ledge means being above it *and* far enough across at the same
	// moment, so what matters is the displacement at the last tick still above
	// it -- which includes everything covered on the way up.
	for i := int64(1); i < 1000; i++ {
		vel += t.Gravity
		risen -= vel

		if risen >= height {
			lastAbove = i
		}
		if risen < 0 {
			break
		}
	}

	// A tick count is a plain scalar, so this is a raw multiply. Mul would
	// treat the count as a fraction and give a reach of nearly nothing.
	return fixed.F(int64(t.RunSpeed) * lastAbove)
}
