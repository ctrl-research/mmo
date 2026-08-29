package room

import (
	"testing"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// The effect vocabulary, in a running room.
//
// Content tests prove a support transforms an effect list. These prove the
// list does something when it reaches the simulation -- which is the half that
// would otherwise pass validation and produce a skill that casts, animates,
// and has no consequence.

func effect(kind content.EffectKind, set func(*content.Effect)) *content.Effect {
	e := &content.Effect{Kind: kind}
	if set != nil {
		set(e)
	}
	return e
}

// mobFor returns a live mob to act on, spawning ticks until one exists.
func (h *harness) aMob(id EntityID) *Entity {
	h.t.Helper()

	for i := 0; i < 40; i++ {
		if mobs := h.mobs(h.entity(id).HuntLayer, false); len(mobs) > 0 {
			return mobs[0]
		}
		h.tick(5)
	}
	h.t.Fatal("no mob spawned to act on")
	return nil
}

// --- restoration -------------------------------------------------------------

func TestHealIsCappedAtMaximum(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	e.HP = 10

	h.room.heal(e, 30)
	if e.HP != 40 {
		t.Errorf("healed to %d, want 40", e.HP)
	}

	h.room.heal(e, 10_000)
	if e.HP != e.MaxHP {
		t.Errorf("a large heal left %d of %d; it should cap", e.HP, e.MaxHP)
	}
}

func TestHealingTheDeadDoesNothing(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	e.HP = 0

	h.room.heal(e, 50)
	if e.HP != 0 {
		t.Errorf("a heal revived a dead entity to %d; resurrection is a skill, "+
			"not a side effect of healing", e.HP)
	}
}

// --- shields -----------------------------------------------------------------

// A shield sits in front of health rather than adding to it. That is what
// makes it different from a heal: it expires unspent.
func TestShieldAbsorbsBeforeHealth(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	before := e.HP

	h.room.applyShield(e, e, 30, 100)
	h.room.damage(e, e, 20, false, "physical")

	if e.HP != before {
		t.Errorf("health went from %d to %d while a shield was up", before, e.HP)
	}
	if e.Shield != 10 {
		t.Errorf("the shield has %d left of 30 after soaking 20", e.Shield)
	}

	// Overflow reaches health, or a small shield would make a character
	// immune to a large hit.
	h.room.damage(e, e, 25, false, "physical")
	if e.Shield != 0 {
		t.Errorf("the shield survived a hit larger than itself with %d left", e.Shield)
	}
	if e.HP != before-15 {
		t.Errorf("health is %d, want %d: 25 damage against 10 of shield",
			e.HP, before-15)
	}
}

func TestAnExpiredShieldAbsorbsNothing(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	h.room.applyShield(e, e, 50, 2)
	before := e.HP

	h.tick(5)
	h.room.damage(e, e, 20, false, "physical")

	if e.HP != before-20 {
		t.Errorf("health is %d, want %d; an expired shield still absorbed",
			e.HP, before-20)
	}
}

// --- buffs -------------------------------------------------------------------

func TestABuffsStatModifiersReachTheStatBlock(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	h.giveStats(alice, stats.Modifier{
		Stat: stats.Attack, Kind: stats.Flat, Value: stats.FromInt(40),
	})

	e := h.entity(alice)
	before := e.Player.Attack()

	might := h.game.Buffs["test_might"]
	if might == nil {
		t.Fatal("no test_might buff")
	}
	h.room.applyBuff(e, e, might, 1, 0)

	if after := e.Player.Attack(); after <= before {
		t.Errorf("attack went from %d to %d under a +25%% buff", before, after)
	}
}

// Stacks multiply the modifier, so three stacks of +8% is +24% rather than
// three separate entries nobody can read.
func TestStacksMultiplyAModifier(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	h.giveStats(alice, stats.Modifier{
		Stat: stats.Attack, Kind: stats.Flat, Value: stats.FromInt(40),
	})

	e := h.entity(alice)
	might := h.game.Buffs["test_might"]

	h.room.applyBuff(e, e, might, 1, 0)
	one := e.Player.Attack()

	h.room.applyBuff(e, e, might, 1, 0)
	two := e.Player.Attack()

	if two <= one {
		t.Errorf("a second stack changed attack from %d to %d", one, two)
	}
	if e.Buffs["test_might"].stacks != 2 {
		t.Errorf("holding %d stacks, want 2", e.Buffs["test_might"].stacks)
	}
}

func TestStacksAreCapped(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	might := h.game.Buffs["test_might"]

	for i := 0; i < 10; i++ {
		h.room.applyBuff(e, e, might, 1, 0)
	}
	if got := e.Buffs["test_might"].stacks; got != might.MaxStacks {
		t.Errorf("holding %d stacks against a cap of %d", got, might.MaxStacks)
	}
}

func TestBuffsExpireAndGiveBackTheirModifiers(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	h.giveStats(alice, stats.Modifier{
		Stat: stats.Attack, Kind: stats.Flat, Value: stats.FromInt(40),
	})

	e := h.entity(alice)
	before := e.Player.Attack()

	might := h.game.Buffs["test_might"]
	h.room.applyBuff(e, e, might, 1, 4)
	if e.Player.Attack() == before {
		t.Fatal("setup: the buff did nothing")
	}

	h.tick(6)

	if _, held := e.Buffs["test_might"]; held {
		t.Error("the buff outlived its duration")
	}
	if after := e.Player.Attack(); after != before {
		t.Errorf("attack is %d after the buff expired, want the original %d",
			after, before)
	}
}

// A damage-over-time is a buff whose effects fire on a beat, which is the same
// mechanism as a stat modifier and must not need a second one.
func TestDamageOverTimeTicksOnItsInterval(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(3)

	victim := h.aMob(alice)
	before := victim.HP

	burn := h.game.Buffs["test_burn"]
	h.room.applyBuff(h.entity(alice), victim, burn, 1, 0)

	// One interval is not enough to be sure; several is.
	h.tick(burn.TickInterval * 3)

	if victim.HP >= before {
		t.Errorf("HP went from %d to %d under a damage-over-time", before, victim.HP)
	}
}

func TestRemovingABuffTakesItOff(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	h.room.applyBuff(e, e, h.game.Buffs["test_might"], 1, 0)
	h.room.removeBuff(e, "test_might")

	if _, held := e.Buffs["test_might"]; held {
		t.Error("the buff is still there after being removed")
	}
}

// --- movement ----------------------------------------------------------------

func TestDashMovesTheCasterInTheirFacing(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(2)

	e := h.entity(alice)
	e.Body.FacingLeft = false
	h.room.dash(e, effect(content.EffectDash, func(x *content.Effect) {
		x.Speed = fixed.FromInt(20)
	}))
	if e.Body.Vel.X <= 0 {
		t.Errorf("dashing while facing right gave velocity %v", e.Body.Vel.X)
	}

	e.Body.FacingLeft = true
	h.room.dash(e, effect(content.EffectDash, func(x *content.Effect) {
		x.Speed = fixed.FromInt(20)
	}))
	if e.Body.Vel.X >= 0 {
		t.Errorf("dashing while facing left gave velocity %v", e.Body.Vel.X)
	}
}

// Away from the caster's actual position rather than their facing: a target
// that walked behind mid-swing should be pushed back the way it came.
func TestKnockbackPushesAwayFromTheCaster(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(3)

	caster := h.entity(alice)
	victim := h.aMob(alice)

	at := victim.Body.FeetCenter()
	h.placeAt(alice, at.X.Int()-40, at.Y.Int())

	push := effect(content.EffectKnockback, func(x *content.Effect) {
		x.Speed = fixed.FromInt(15)
	})
	h.room.knockback(caster, victim, push)

	if victim.Body.Vel.X <= 0 {
		t.Errorf("a target to the caster's right was pushed left (%v)", victim.Body.Vel.X)
	}

	h.placeAt(alice, at.X.Int()+40, at.Y.Int())
	h.room.knockback(caster, victim, push)

	if victim.Body.Vel.X >= 0 {
		t.Errorf("a target to the caster's left was pushed right (%v)", victim.Body.Vel.X)
	}
}

// --- projectiles -------------------------------------------------------------

func TestAProjectileTravelsAndHits(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(3)

	victim := h.aMob(alice)
	at := victim.Body.FeetCenter()

	// Stand well to the left, facing right, so the bolt has to travel.
	h.placeAt(alice, at.X.Int()-160, at.Y.Int())
	caster := h.entity(alice)
	caster.Body.FacingLeft = false

	before := victim.HP
	h.room.spawnProjectile(caster, effect(content.EffectProjectile, func(x *content.Effect) {
		x.Speed = fixed.FromInt(24)
		x.Distance = fixed.FromInt(400)
		x.Effects = []content.Effect{{
			Kind: content.EffectDamage, Element: "physical", BaseMin: 20, BaseMax: 20,
		}}
	}))

	if len(h.projectiles()) != 1 {
		t.Fatalf("spawned %d projectiles, want 1", len(h.projectiles()))
	}

	h.tick(20)

	if victim.HP >= before {
		t.Errorf("HP went from %d to %d; the bolt never landed", before, victim.HP)
	}
	if n := len(h.projectiles()); n != 0 {
		t.Errorf("%d projectiles still in flight after hitting", n)
	}
}

// A bolt that finds nothing has to stop existing, or a room's entity count
// grows with every miss.
func TestAProjectileThatMissesExpires(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(3)

	caster := h.entity(alice)
	caster.Body.FacingLeft = true
	h.placeAt(alice, 120, 288)

	h.room.spawnProjectile(caster, effect(content.EffectProjectile, func(x *content.Effect) {
		x.Speed = fixed.FromInt(24)
		x.Distance = fixed.FromInt(64)
		x.Effects = []content.Effect{{Kind: content.EffectDamage, BaseMin: 5, BaseMax: 5}}
	}))

	h.tick(40)

	if n := len(h.projectiles()); n != 0 {
		t.Errorf("%d projectiles are still in the room after their range", n)
	}
}

// --- ground areas ------------------------------------------------------------

func TestAnAreaKeepsApplyingAndThenExpires(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")
	h.tick(3)

	victim := h.aMob(alice)
	before := victim.HP

	h.room.spawnArea(h.entity(alice), victim, effect(content.EffectArea, func(x *content.Effect) {
		x.Radius = fixed.FromInt(64)
		x.DurationTicks = 20
		x.TickInterval = 4
		x.Effects = []content.Effect{{
			Kind: content.EffectDamage, Element: "fire", BaseMin: 3, BaseMax: 3,
		}}
	}))

	h.tick(12)
	during := victim.HP
	if during >= before {
		t.Errorf("HP went from %d to %d while standing in a fire", before, during)
	}

	h.tick(30)
	if n := len(h.areas()); n != 0 {
		t.Errorf("%d areas outlived their duration", n)
	}

	after := victim.HP
	h.tick(20)
	if victim.HP < after {
		t.Error("an expired area is still doing damage")
	}
}

// --- chain -------------------------------------------------------------------

func TestAChainReachesASecondTargetForLess(t *testing.T) {
	h := newHarness(t)
	alice, _ := h.join("alice")

	// The spawn point trickles mobs in one per respawn interval, so waiting
	// for a second is waiting a whole interval. A skipped test proves nothing,
	// so this waits rather than giving up.
	var mobs []*Entity
	for i := 0; i < 20 && len(mobs) < 2; i++ {
		h.tick(10)
		mobs = h.mobs(h.entity(alice).HuntLayer, false)
	}
	if len(mobs) < 2 {
		t.Fatal("the layer never held two mobs to chain between")
	}

	first, second := mobs[0], mobs[1]
	// Side by side, so the hop has somewhere to go.
	at := first.Body.FeetCenter()
	second.Body.SetFeetCenter(at)

	firstBefore, secondBefore := first.HP, second.HP

	h.room.chain(h.entity(alice), first, nil,
		effect(content.EffectChain, func(x *content.Effect) {
			x.Jumps = 1
			x.Radius = fixed.FromInt(200)
			x.Falloff = fixed.FromRatio(1, 2)
			x.Effects = []content.Effect{{
				Kind: content.EffectDamage, Element: "lightning", BaseMin: 40, BaseMax: 40,
			}}
		}), h.room.randFor(first.Layer))

	if second.HP >= secondBefore {
		t.Errorf("the second target went from %d to %d; the chain never hopped",
			secondBefore, second.HP)
	}
	if first.HP != firstBefore {
		t.Errorf("the chain hit the target it started from; it should skip it")
	}
}
