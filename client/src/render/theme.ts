/**
 * Colours for everything that is not a sprite: the grid, health bars, damage
 * numbers, and the tints applied over generated art.
 *
 * The art itself carries its own palette (cmd/spritegen), because a sprite
 * sheet and a colour table in two places drift. What is left here is the
 * interface layer, which has no sprites and should not have any.
 */
export const theme = {
  background: 0x0b0d12,
  grid: 0x161a22,

  solid: 0x2a3140,
  solidEdge: 0x39435a,
  platform: 0x3d4a63,
  platformEdge: 0x5a6b8c,
  rope: 0x7a6a45,

  self: 0x4a83ff,
  selfEdge: 0x8fb4ff,
  other: 0x7f8899,
  otherEdge: 0xa8b1c2,

  // Other players wear the same mannequin, tinted, so two characters standing
  // together are obviously the same kind of thing and still tellable apart.
  otherTint: 0xb9c2d6,

  // Champions and rares are tinted rather than given their own art: the
  // modifiers are combinatorial, so there is no sprite to draw for "Brutal
  // Swift", and a colour reads at a distance where a name does not.
  champion: 0x7fc4ff,
  rare: 0xffd166,

  mob: 0xc25b5b,
  mobEdge: 0xe08585,
  mobHit: 0xffd0d0,
  mobDead: 0x4a3535,

  dropItem: 0x6fc28a,
  dropGold: 0xe0b44a,

  healthHigh: 0x5fbf6a,
  healthMid: 0xd8b243,
  healthLow: 0xd05353,

  damageDealt: 0xffe9a8,
  damageCrit: 0xffb347,
  damageTaken: 0xff7a7a,

  // What the effect vocabulary spawns. A bolt reads hot, ground effects read
  // as something you are standing in, and a telegraph reads as a warning --
  // which means it has to be the one thing on screen that cannot be mistaken
  // for scenery.
  projectile: 0xffca6b,
  projectileCore: 0xfff3d0,
  area: 0xff7a4d,
  areaEdge: 0xffb07a,
  telegraph: 0xff4d4d,
  telegraphFill: 0xff8a5c,

  // A shrine has to read as "this does something if you touch it" with no art
  // of its own, so it is a bright ring that breathes. Deliberately cool, where
  // a telegraph is hot: one is an invitation and the other is a warning.
  shrine: 0x9fe3d0,
  shrineCore: 0xe8fff8,

  // Resource nodes. Green for the living ones, because a tree and a herb patch
  // are the same idea; the level gate is drawn by dimming rather than by a
  // second colour, so "too high for me" reads without learning a legend.
  resource: 0x7fb069,
  resourceCore: 0xcfe8b4,

  nameText: 0xd7dce6,
  ghost: 0x2ecc71,
} as const;

export const TILE = 32;
