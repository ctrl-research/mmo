package main

import "image/color"

// The palette.
//
// One small palette for everything, because a coherent set of colours is most
// of what makes generated art read as art rather than as debug output. Every
// material is a ramp of three: a shadow, a body, and a lit edge, so the same
// top-left light source can be applied everywhere without picking colours per
// sprite.

type ramp struct{ shadow, body, light color.RGBA }

func rgb(hex uint32) color.RGBA {
	return color.RGBA{
		R: uint8(hex >> 16),
		G: uint8(hex >> 8),
		B: uint8(hex),
		A: 0xff,
	}
}

var (
	transparent = color.RGBA{}

	// Outline, not black: a pure black outline against a dark background
	// disappears, and against a light one it reads as a hole.
	outline = rgb(0x14161d)

	stone = ramp{rgb(0x232a38), rgb(0x333c4f), rgb(0x4a5670)}
	grass = ramp{rgb(0x2f6b3f), rgb(0x469a55), rgb(0x63c471)}
	wood  = ramp{rgb(0x4a3a20), rgb(0x6b5433), rgb(0x8f7346)}
	rope  = ramp{rgb(0x6b5a34), rgb(0x8f7a45), rgb(0xb39a5c)}

	// The mannequin: cloth, skin, and metal. Deliberately plain -- it is a
	// stand-in for a character, and reading as a person at 24 pixels wide is
	// the whole job.
	cloth = ramp{rgb(0x2c4a86), rgb(0x3f68b8), rgb(0x5d8ae0)}
	skin  = ramp{rgb(0xa87049), rgb(0xd39a6a), rgb(0xf0bd8e)}
	metal = ramp{rgb(0x5a6272), rgb(0x828b9d), rgb(0xb3bccd)}

	slime = ramp{rgb(0x2f7a4f), rgb(0x45b070), rgb(0x74e09b)}
	boar  = ramp{rgb(0x6b3a2c), rgb(0x96543d), rgb(0xc07a5c)}
	royal = ramp{rgb(0x6b3a7a), rgb(0x9c55b0), rgb(0xc77fdb)}

	gold = ramp{rgb(0x8f6b1c), rgb(0xd8a72f), rgb(0xf5d76a)}
	gem  = ramp{rgb(0x2c6b6b), rgb(0x3f9c9c), rgb(0x66d4d4)}
)
