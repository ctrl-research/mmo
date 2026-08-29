package main

// Mobs and drops.
//
// Sized from content: a sprite that does not match its hitbox makes every
// "that should have hit" argument unanswerable, so the sizes here are the
// numbers in content/mobs, not numbers that looked right.

// drawSlime draws a blob that squashes as it moves.
//
// The squash is the animation: a slime with a walk cycle would need legs, and
// the appeal of a slime is that it has none.
func drawSlime(w, h int, squash int, r ramp) *canvas {
	c := newCanvas(w, h)

	bodyH := h - squash
	top := h - bodyH

	// A dome rather than an ellipse: flat on the floor, round on top.
	c.ellipse(0, top, w, bodyH*2, r.body)
	c.rect(0, top+bodyH, w, h-top-bodyH, transparent)
	c.rect(0, h-1, w, 1, r.shadow)

	// A highlight where the light hits, and a darker underside. Two bands is
	// enough to read as glossy at this size.
	c.ellipse(w/5, top+2, w/3, bodyH/2, r.light)
	c.rect(1, h-2, w-2, 1, r.shadow)

	// Eyes on the facing side.
	eyeY := top + bodyH/2
	c.rect(w/2+2, eyeY, 2, 2, outline)
	c.rect(w-6, eyeY, 2, 2, outline)

	c.outlineSprite(outline)
	return c
}

// drawBoar draws a four-legged charger.
func drawBoar(w, h int, stride int, r ramp) *canvas {
	c := newCanvas(w, h)

	bodyH := h * 3 / 5
	bodyY := h - bodyH - 6

	c.ellipse(2, bodyY, w-8, bodyH, r.body)
	c.ellipse(4, bodyY+1, w-14, bodyH/2, r.light)

	// Legs, opposed so a stride reads as movement.
	legTop := bodyY + bodyH - 2
	legH := h - legTop
	c.rect(5+stride, legTop, 4, legH, r.shadow)
	c.rect(w-14-stride, legTop, 4, legH, r.shadow)
	c.rect(8-stride, legTop, 4, legH, r.body)
	c.rect(w-11+stride, legTop, 4, legH, r.body)

	// Head and snout on the facing side, which is where the danger is.
	headH := bodyH * 3 / 4
	c.ellipse(w-14, bodyY+2, 12, headH, r.body)
	c.rect(w-4, bodyY+headH/2, 4, 4, r.shadow)

	// A tusk, so a charging boar looks like it means it.
	c.rect(w-5, bodyY+headH/2-2, 4, 2, metal.light)
	c.set(w-2, bodyY+headH/2-3, metal.light)

	c.rect(w-9, bodyY+4, 2, 2, outline)

	// Bristles along the spine.
	c.noise(4, bodyY, w-12, 3, r.shadow, 0x5eed, 40)

	c.outlineSprite(outline)
	return c
}

// drawSlimeKing draws the shared-layer field boss: a slime with a crown.
//
// Visibly the same creature as the small ones, because a boss that shares a
// silhouette with its minions tells a player what it is before they read the
// name.
func drawSlimeKing(w, h int, squash int, r ramp) *canvas {
	c := drawSlime(w, h, squash, r)

	crownW := w / 3
	crownX := w/2 - crownW/2
	crownY := squash + 1

	c.rect(crownX, crownY+3, crownW, 3, gold.body)
	c.rect(crownX, crownY+3, crownW, 1, gold.light)
	for i := 0; i < 3; i++ {
		x := crownX + i*(crownW-2)/2
		c.rect(x, crownY, 2, 4, gold.body)
		c.set(x, crownY, gold.light)
	}

	c.outlineSprite(outline)
	return c
}

// drawCoin draws ground gold.
func drawCoin(size int, frame int) *canvas {
	c := newCanvas(size, size)

	// The frame narrows the coin, so a pile of gold shimmers rather than
	// sitting there. Never to nothing: a coin that vanishes reads as a bug.
	w := size - frame*2
	if w < 3 {
		w = 3
	}
	x := (size - w) / 2

	c.ellipse(x, 1, w, size-2, gold.body)
	c.ellipse(x, 1, w, (size-2)/2, gold.light)
	c.rect(x, size-2, w, 1, gold.shadow)

	c.outlineSprite(outline)
	return c
}

// drawItemDrop draws a ground item as a small pouch.
func drawItemDrop(size int) *canvas {
	c := newCanvas(size, size)

	c.ellipse(1, 3, size-2, size-4, gem.body)
	c.ellipse(3, 5, (size-2)/2, (size-4)/3, gem.light)
	// A tie at the neck, so it reads as a pouch rather than a ball.
	c.rect(size/2-3, 2, 6, 2, wood.body)
	c.rect(size/2-3, 2, 6, 1, wood.light)

	c.outlineSprite(outline)
	return c
}
