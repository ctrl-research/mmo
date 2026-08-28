/**
 * Q24.8 fixed-point, matching internal/fixed exactly.
 *
 * The simulation avoids floating point so that the server and this client --
 * which runs the server's own code compiled to WebAssembly -- cannot disagree
 * about a position. Values arrive from the wire as raw Q24.8 integers and stay
 * that way until the moment they are handed to the renderer.
 */

export const FRAC_BITS = 8;
export const ONE = 1 << FRAC_BITS;

/** Converts fixed-point to pixels. Rendering is the only place this belongs. */
export function toPixels(v: number): number {
  return v / ONE;
}

/** Converts whole world units to fixed-point. */
export function fromInt(v: number): number {
  return v << FRAC_BITS;
}
