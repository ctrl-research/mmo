package content

import (
	"testing"

	gamedata "github.com/ctrl-research/mmo/content"
	"github.com/ctrl-research/mmo/internal/fixed"
)

// The property the whole system exists for.
//
// A support is a transformation of an effect list, so it works on skills it
// was never written for. If any of these tests needed a support to know a
// skill's name, the design would have failed and the combinatorial appeal --
// which is the only reason to build it this way -- would be gone.

func TestASupportAttachesToSkillsItWasNeverWrittenFor(t *testing.T) {
	c := mustLoadShipped(t)

	swiftness := c.Supports["swiftness"]
	if swiftness == nil {
		t.Fatal("no swiftness support")
	}

	// Every skill that both applies a buff and carries the tag. The support
	// names none of them, and it must lengthen all of them.
	lengthened := 0
	for id, skill := range c.Skills {
		if !swiftness.Attaches(skill) {
			continue
		}

		before := buffDurations(skill.Effects)
		if len(before) == 0 {
			continue
		}

		after := buffDurations(swiftness.Apply(skill.Effects))
		if len(after) != len(before) {
			t.Errorf("%s: swiftness changed how many buffs the skill applies", id)
			continue
		}
		for i := range before {
			if after[i] <= before[i] {
				t.Errorf("%s buff %d: duration %d became %d; swiftness should lengthen it",
					id, i, before[i], after[i])
			}
		}
		lengthened++
	}

	if lengthened == 0 {
		t.Fatal("swiftness attached to nothing, so this test proved nothing")
	}
}

// Tags are all-or-nothing. Matching any of them would attach a melee support
// to a fireball.
func TestSupportsNeedEveryTag(t *testing.T) {
	c := mustLoadShipped(t)

	multistrike := c.Supports["multistrike"]
	if multistrike == nil {
		t.Fatal("no multistrike support")
	}

	melee := &Skill{Tags: []string{"melee", "attack", "physical"}}
	spell := &Skill{Tags: []string{"spell", "attack", "fire"}}
	halfway := &Skill{Tags: []string{"melee"}}

	if !multistrike.Attaches(melee) {
		t.Error("a melee attack skill was refused a melee attack support")
	}
	if multistrike.Attaches(spell) {
		t.Error("a melee support attached to a spell")
	}
	if multistrike.Attaches(halfway) {
		t.Error("a support attached to a skill carrying only one of its tags")
	}
}

// Less damage, more times. The trade is the point: a support that only added
// repeats would be strictly better than not taking it.
func TestMultistrikeTradesDamageForRepeats(t *testing.T) {
	c := mustLoadShipped(t)
	multistrike := c.Supports["multistrike"]

	before := []Effect{{Kind: EffectDamage, Element: "physical", BaseMin: 10, BaseMax: 20}}
	after := multistrike.Apply(before)

	if len(after) != 3 {
		t.Fatalf("multistrike produced %d effects, want 3", len(after))
	}
	for i, e := range after {
		if e.BaseMax >= before[0].BaseMax {
			t.Errorf("hit %d does %d damage, no less than the original %d",
				i, e.BaseMax, before[0].BaseMax)
		}
	}

	// The trade has to be worth considering in both directions: three hits at
	// 55% is more total damage, which is what makes the mana cost the reason
	// to think about it.
	total := 0
	for _, e := range after {
		total += e.BaseMax
	}
	if total <= before[0].BaseMax {
		t.Errorf("three reduced hits total %d against the original %d; nobody "+
			"would ever take this", total, before[0].BaseMax)
	}
	if multistrike.ManaMult <= fixed.One {
		t.Error("multistrike is free, so there is no reason not to use it")
	}
}

// A support reaches a projectile's payload. A fire support should work on a
// fireball whether the fire is applied directly or carried by a bolt.
func TestSupportsReachNestedEffects(t *testing.T) {
	c := mustLoadShipped(t)
	conflagrate := c.Supports["conflagrate"]

	before := []Effect{{
		Kind:  EffectProjectile,
		Speed: fixed.FromInt(20),
		Effects: []Effect{
			{Kind: EffectDamage, Element: "fire", BaseMin: 10, BaseMax: 20},
		},
	}}

	after := conflagrate.Apply(before)
	if len(after) != 1 || len(after[0].Effects) != 1 {
		t.Fatalf("conflagrate reshaped the projectile: %+v", after)
	}
	if after[0].Effects[0].BaseMax <= before[0].Effects[0].BaseMax {
		t.Errorf("the bolt's fire damage went from %d to %d; a fire support has "+
			"to reach the payload or it does nothing on any projectile skill",
			before[0].Effects[0].BaseMax, after[0].Effects[0].BaseMax)
	}
}

// An element filter is what makes "increased fire damage" a support rather
// than a stat: it must leave everything else alone.
func TestElementFiltersLeaveOtherDamageAlone(t *testing.T) {
	c := mustLoadShipped(t)
	brutality := c.Supports["brutality"]

	before := []Effect{
		{Kind: EffectDamage, Element: "physical", BaseMin: 10, BaseMax: 10},
		{Kind: EffectDamage, Element: "fire", BaseMin: 10, BaseMax: 10},
	}
	after := brutality.Apply(before)

	if after[0].BaseMax <= 10 {
		t.Error("brutality did not increase physical damage")
	}
	if after[1].BaseMax != 10 {
		t.Errorf("brutality changed fire damage to %d; it is a physical support",
			after[1].BaseMax)
	}
}

// Applying a support must not edit the skill, which is shared by every room on
// the node and read without locking.
func TestApplyingASupportDoesNotEditTheSkill(t *testing.T) {
	c := mustLoadShipped(t)

	skill := c.Skills["slash"]
	if skill == nil {
		t.Fatal("no slash skill")
	}
	before := skill.Effects[0].BaseMax

	for _, support := range c.Supports {
		if support.Attaches(skill) {
			support.Apply(skill.Effects)
		}
	}

	if skill.Effects[0].BaseMax != before {
		t.Errorf("a support edited the skill definition: base damage went from "+
			"%d to %d, and every room on this node shares that struct",
			before, skill.Effects[0].BaseMax)
	}
}

func buffDurations(effects []Effect) []int {
	var out []int
	for _, e := range effects {
		if e.Kind == EffectApplyBuff && e.DurationTicks > 0 {
			out = append(out, e.DurationTicks)
		}
		out = append(out, buffDurations(e.Effects)...)
	}
	return out
}

func mustLoadShipped(t *testing.T) *Content {
	t.Helper()

	c, err := Load(gamedata.FS)
	if err != nil {
		t.Fatalf("load shipped content: %v", err)
	}
	return c
}
