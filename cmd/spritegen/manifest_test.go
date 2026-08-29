package main

import (
	"testing"

	gamedata "github.com/ctrl-research/mmo/content"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/world/sim"
)

// The art has to match the simulation, or it lies about the game.
//
// A sprite drawn at a different size from its hitbox makes every "that should
// have hit" argument unanswerable, and the client picks a mob's sprite by its
// dimensions -- so a mismatch here does not draw the wrong size, it draws
// nothing at all.

func TestMobSpritesMatchContent(t *testing.T) {
	c, err := content.Load(gamedata.FS)
	if err != nil {
		t.Fatalf("load content: %v", err)
	}

	drawn := map[string]mobFrame{}
	for _, m := range mobLayout {
		drawn[m.id] = m
	}

	for id, mob := range c.Mobs {
		sprite, ok := drawn[id]
		if !ok {
			t.Errorf("mob %q has no sprite; the client will draw it as a box "+
				"labelled with its name", id)
			continue
		}
		if got, want := sprite.w, mob.Width.Int(); got != want {
			t.Errorf("mob %q is %d units wide and its sprite is %d", id, want, got)
		}
		if got, want := sprite.h, mob.Height.Int(); got != want {
			t.Errorf("mob %q is %d units tall and its sprite is %d", id, want, got)
		}
	}

	for id := range drawn {
		if _, ok := c.Mobs[id]; !ok {
			t.Errorf("sprite %q draws a mob that no longer exists", id)
		}
	}
}

// Two mobs the same size would collide in the manifest, which is keyed by
// size -- and the collision would silently give one of them the other's art.
func TestNoTwoMobsShareASize(t *testing.T) {
	seen := map[string]string{}
	for _, m := range mobLayout {
		key := sizeKey(m.w, m.h)
		if other, dup := seen[key]; dup {
			t.Errorf("%q and %q are both %s; the client picks a sprite by size, "+
				"so one of them would be drawn as the other", m.id, other, key)
		}
		seen[key] = m.id
	}
}

func TestPlayerSpriteMatchesTheSimulationBody(t *testing.T) {
	if got, want := bodyW, sim.PlayerSize.W.Int(); got != want {
		t.Errorf("the mannequin is %d wide and a player body is %d", got, want)
	}
	if got, want := bodyH, sim.PlayerSize.H.Int(); got != want {
		t.Errorf("the mannequin is %d tall and a player body is %d", got, want)
	}
}

// The generator is run by hand and its output committed, so it has to produce
// the same bytes every time -- otherwise every run is a diff and nobody can
// see the one that matters.
func TestGenerationIsDeterministic(t *testing.T) {
	for name, draw := range map[string]func() *canvas{
		"player":  drawPlayerSheet,
		"mobs":    drawMobSheet,
		"terrain": drawTerrainSheet,
		"drops":   drawDropSheet,
	} {
		first, second := draw(), draw()

		if first.w != second.w || first.h != second.h {
			t.Fatalf("%s changed size between runs", name)
		}
		for y := 0; y < first.h; y++ {
			for x := 0; x < first.w; x++ {
				if first.at(x, y) != second.at(x, y) {
					t.Fatalf("%s differs at %d,%d between two runs", name, x, y)
				}
			}
		}
	}
}
