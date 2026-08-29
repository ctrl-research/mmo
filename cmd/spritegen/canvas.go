package main

import (
	"image"
	"image/color"
)

// A tiny drawing surface for pixel art.
//
// Rectangles and spans rather than curves, because that is what pixel art is:
// at 24 pixels wide an antialiased circle is a smudge, and every edge here has
// to land on a whole pixel. Nothing in this file knows what it is drawing.

type canvas struct {
	img *image.RGBA
	w   int
	h   int
}

func newCanvas(w, h int) *canvas {
	return &canvas{img: image.NewRGBA(image.Rect(0, 0, w, h)), w: w, h: h}
}

func (c *canvas) set(x, y int, col color.RGBA) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h || col.A == 0 {
		return
	}
	c.img.SetRGBA(x, y, col)
}

func (c *canvas) at(x, y int) color.RGBA {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return transparent
	}
	return c.img.RGBAAt(x, y)
}

// rect fills a rectangle, inclusive of its origin and exclusive of its far
// edge, the way every other rectangle in this codebase works.
func (c *canvas) rect(x, y, w, h int, col color.RGBA) {
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			c.set(x+dx, y+dy, col)
		}
	}
}

// shaded fills a rectangle with a light top edge and a shadowed bottom, which
// is the whole of the lighting model: one source, above and to the left.
func (c *canvas) shaded(x, y, w, h int, r ramp) {
	c.rect(x, y, w, h, r.body)
	c.rect(x, y, w, 1, r.light)
	if h > 2 {
		c.rect(x, y+h-1, w, 1, r.shadow)
	}
	if w > 2 {
		c.rect(x, y+1, 1, h-2, r.light)
		c.rect(x+w-1, y+1, 1, h-2, r.shadow)
	}
}

// ellipse fills an axis-aligned ellipse inscribed in a box.
//
// Used for anything organic -- a slime, a boar's body -- where a rectangle
// reads as a crate.
func (c *canvas) ellipse(x, y, w, h int, col color.RGBA) {
	rx, ry := float64(w)/2, float64(h)/2
	cx, cy := float64(x)+rx, float64(y)+ry

	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			px, py := float64(x+dx)+0.5, float64(y+dy)+0.5
			nx, ny := (px-cx)/rx, (py-cy)/ry
			if nx*nx+ny*ny <= 1 {
				c.set(x+dx, y+dy, col)
			}
		}
	}
}

// outlineSprite draws a one-pixel border around everything opaque.
//
// Applied last, over the whole sprite, because an outline drawn per shape
// leaves seams where two shapes meet -- and the seams are exactly what makes
// assembled pixel art look assembled.
func (c *canvas) outlineSprite(col color.RGBA) {
	// Read from a copy: outlining in place would outline the outline.
	before := make([]color.RGBA, c.w*c.h)
	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			before[y*c.w+x] = c.at(x, y)
		}
	}
	opaque := func(x, y int) bool {
		if x < 0 || y < 0 || x >= c.w || y >= c.h {
			return false
		}
		return before[y*c.w+x].A != 0
	}

	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			if opaque(x, y) {
				continue
			}
			if opaque(x-1, y) || opaque(x+1, y) || opaque(x, y-1) || opaque(x, y+1) {
				c.set(x, y, col)
			}
		}
	}
}

// blit copies a sprite into a sheet at a frame position.
func (c *canvas) blit(src *canvas, atX, atY int) {
	for y := 0; y < src.h; y++ {
		for x := 0; x < src.w; x++ {
			if col := src.at(x, y); col.A != 0 {
				c.set(atX+x, atY+y, col)
			}
		}
	}
}

// noise scatters a colour deterministically, for texture that is not a
// pattern. Seeded per call so a regenerated sheet is byte-identical.
func (c *canvas) noise(x, y, w, h int, col color.RGBA, seed uint32, density int) {
	state := seed | 1
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			// xorshift: small, deterministic, and good enough for freckles.
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			if int(state%100) < density {
				c.set(x+dx, y+dy, col)
			}
		}
	}
}
