import { toPixels } from "./fixed";

/**
 * Body mirrors sim.Body, encoded exactly as internal/world/sim/codec.go writes
 * it: 28 bytes of little-endian integers.
 *
 * Every field is carried, including the three small counters. They look like
 * internal detail, but prediction works by snapping to the server's body and
 * replaying inputs through the identical code -- so any field left out here
 * makes the replay diverge, and the divergence shows up as rubber-banding
 * rather than as an obviously missing value.
 */
export const BODY_SIZE = 28;

export const FLAG_GROUNDED = 1 << 0;
export const FLAG_CLIMBING = 1 << 1;
export const FLAG_FACING_LEFT = 1 << 2;
export const FLAG_JUMP_HELD = 1 << 3;

export interface Body {
  x: number;
  y: number;
  vx: number;
  vy: number;
  w: number;
  h: number;
  flags: number;
  coyote: number;
  jumpBuffer: number;
  dropThrough: number;
}

export function newBody(): Body {
  return { x: 0, y: 0, vx: 0, vy: 0, w: 0, h: 0, flags: 0, coyote: 0, jumpBuffer: 0, dropThrough: 0 };
}

export function copyBody(dst: Body, src: Body): Body {
  dst.x = src.x;
  dst.y = src.y;
  dst.vx = src.vx;
  dst.vy = src.vy;
  dst.w = src.w;
  dst.h = src.h;
  dst.flags = src.flags;
  dst.coyote = src.coyote;
  dst.jumpBuffer = src.jumpBuffer;
  dst.dropThrough = src.dropThrough;
  return dst;
}

export function cloneBody(src: Body): Body {
  return copyBody(newBody(), src);
}

/** Writes a body into a 28-byte buffer for the WebAssembly call. */
export function encodeBody(view: DataView, b: Body): void {
  view.setInt32(0, b.x, true);
  view.setInt32(4, b.y, true);
  view.setInt32(8, b.vx, true);
  view.setInt32(12, b.vy, true);
  view.setInt32(16, b.w, true);
  view.setInt32(20, b.h, true);
  view.setUint8(24, b.flags);
  view.setUint8(25, b.coyote);
  view.setUint8(26, b.jumpBuffer);
  view.setUint8(27, b.dropThrough);
}

/** Reads a body back out of the 28-byte buffer. */
export function decodeBody(view: DataView, out: Body): Body {
  out.x = view.getInt32(0, true);
  out.y = view.getInt32(4, true);
  out.vx = view.getInt32(8, true);
  out.vy = view.getInt32(12, true);
  out.w = view.getInt32(16, true);
  out.h = view.getInt32(20, true);
  out.flags = view.getUint8(24);
  out.coyote = view.getUint8(25);
  out.jumpBuffer = view.getUint8(26);
  out.dropThrough = view.getUint8(27);
  return out;
}

export const isGrounded = (b: Body): boolean => (b.flags & FLAG_GROUNDED) !== 0;
export const isClimbing = (b: Body): boolean => (b.flags & FLAG_CLIMBING) !== 0;
export const isFacingLeft = (b: Body): boolean => (b.flags & FLAG_FACING_LEFT) !== 0;

/** Squared distance in pixels between two bodies, for the smoothing test. */
export function pixelDistanceSq(a: Body, b: Body): number {
  const dx = toPixels(a.x - b.x);
  const dy = toPixels(a.y - b.y);
  return dx * dx + dy * dy;
}
