package main

// Terrain.
//
// Tiles rather than one stretched image, because the maps are built on a
// 32-unit grid and terrain is drawn by repeating. The set is small on purpose:
// a fill, the four edges a fill needs to not look cut out, and the two things
// that are not fills -- a plank platform and a rope.

const tile = 32

// tileset lays the terrain tiles out in one row, in the order the renderer
// indexes them.
type tileIndex int

const (
	tileGroundTop tileIndex = iota
	tileGroundFill
	tileGroundLeft
	tileGroundRight
	tilePlatform
	tileRope
	tileCount
)

func drawTerrainSheet() *canvas {
	sheet := newCanvas(tile*int(tileCount), tile)

	for i := tileIndex(0); i < tileCount; i++ {
		sheet.blit(drawTile(i), int(i)*tile, 0)
	}
	return sheet
}

func drawTile(which tileIndex) *canvas {
	c := newCanvas(tile, tile)

	switch which {
	case tileGroundTop:
		fillStone(c)
		// A grass cap, thicker on the left so the light direction is
		// consistent with everything else.
		c.rect(0, 0, tile, 5, grass.body)
		c.rect(0, 0, tile, 2, grass.light)
		// Blades hanging into the stone, so the join is not a straight line.
		for _, x := range []int{2, 7, 13, 18, 24, 29} {
			c.rect(x, 5, 2, 2, grass.body)
			c.set(x, 6, grass.shadow)
		}

	case tileGroundFill:
		fillStone(c)

	case tileGroundLeft:
		fillStone(c)
		c.rect(0, 0, 2, tile, stone.light)

	case tileGroundRight:
		fillStone(c)
		c.rect(tile-2, 0, 2, tile, stone.shadow)

	case tilePlatform:
		// A plank, 16 tall to match the map geometry, drawn into the top half
		// of the tile so the renderer can place it by its top edge.
		c.rect(0, 0, tile, 16, wood.body)
		c.rect(0, 0, tile, 2, wood.light)
		c.rect(0, 14, tile, 2, wood.shadow)
		// Grain, and a nail at each end.
		c.noise(0, 3, tile, 10, wood.shadow, 0x91a3, 12)
		c.rect(2, 4, 1, 8, wood.shadow)
		c.rect(tile-3, 4, 1, 8, wood.shadow)
		c.set(4, 3, metal.light)
		c.set(tile-5, 3, metal.light)

	case tileRope:
		// A rope is 16 wide in the maps, drawn centred so it tiles vertically.
		x := tile/2 - 3
		c.rect(x, 0, 6, tile, rope.body)
		c.rect(x, 0, 2, tile, rope.light)
		c.rect(x+4, 0, 2, tile, rope.shadow)
		// Twist marks every few pixels, which is what makes a rope read as a
		// rope rather than a stick.
		for y := 1; y < tile; y += 5 {
			c.rect(x, y, 6, 1, rope.shadow)
			c.set(x+1, y, rope.light)
		}
	}

	return c
}

func fillStone(c *canvas) {
	c.rect(0, 0, tile, tile, stone.body)
	// Blocks, offset per row so the wall does not read as graph paper.
	for y := 0; y < tile; y += 8 {
		c.rect(0, y, tile, 1, stone.shadow)
		offset := 0
		if (y/8)%2 == 1 {
			offset = 8
		}
		for x := offset; x < tile; x += 16 {
			c.rect(x, y+1, 1, 7, stone.shadow)
		}
	}
	c.noise(0, 0, tile, tile, stone.light, 0x2f81, 4)
}

// drawBackdrop draws one parallax layer: a silhouette skyline.
//
// Two layers at different depths is the cheapest thing that turns a flat
// background into somewhere, and it costs one image each.
func drawBackdrop(w, h int, r ramp, seed uint32, hills int) *canvas {
	// Drawn on a transparent field: the renderer tints and layers these, and
	// a baked-in sky would fight whatever background the theme picks.
	c := newCanvas(w, h)

	state := seed | 1
	next := func(n int) int {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		return int(state % uint32(n))
	}

	// Wide and shallow. Domes as tall as they are wide read as spikes, and a
	// backdrop of spikes reads as a hazard rather than as distance.
	step := w / hills
	for i := 0; i <= hills; i++ {
		cx := i * step
		height := h/2 + next(h/3)
		width := step*2 + next(step)

		// Twice the height, so only the top half of the ellipse shows and the
		// hill meets the bottom edge rather than floating.
		c.ellipse(cx-width/2, h-height, width, height*2, r.body)
	}

	// A lighter rim along the top of the silhouette, so the layers separate.
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			if c.at(x, y).A != 0 {
				c.rect(x, y, 1, 2, r.light)
				break
			}
		}
	}
	return c
}
