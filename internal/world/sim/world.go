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

// DefaultTuning is the M0 movement feel: a roughly 96-unit jump reaching its
// apex in 8 ticks (0.4 s), and a run speed of 8 units per tick (160/s).
//
// With 32-unit tiles that is a three-tile jump and five tiles per second,
// which lands close to MapleStory's pacing.
func DefaultTuning() Tuning {
	return Tuning{
		Gravity:     fixed.FromInt(3),
		TerminalVel: fixed.FromInt(40),

		RunSpeed:    fixed.FromInt(8),
		GroundAccel: fixed.FromInt(2),
		GroundFric:  fixed.FromRatio(5, 2),
		AirAccel:    fixed.FromInt(1),
		AirFric:     fixed.FromRatio(3, 10),

		JumpVel:    fixed.FromInt(24),
		JumpCutNum: 1,
		JumpCutDen: 2,

		ClimbSpeed:  fixed.FromInt(5),
		ClimbOffVel: fixed.FromInt(16),

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
