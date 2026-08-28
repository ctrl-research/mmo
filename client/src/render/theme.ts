/**
 * The M0 art direction is deliberately flat colour rather than sprites.
 *
 * Placeholder art that looks like placeholder art keeps attention on whether
 * movement feels right, which is what this milestone is actually for. Sprites
 * and the layered paperdoll arrive with equipment in M3.
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

  nameText: 0xd7dce6,
  ghost: 0x2ecc71,
} as const;

export const TILE = 32;
