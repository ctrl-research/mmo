/**
 * Decodes the collision geometry the server sends, matching MarshalWorld in
 * internal/world/sim/codec.go.
 *
 * The simulation inside WebAssembly decodes this itself; this second decode
 * exists only so the renderer can draw the same shapes. Both read the same
 * bytes from the same source, so there is no possibility of the drawn world
 * and the simulated world disagreeing.
 */

export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface World {
  bounds: Rect;
  solids: Rect[];
  platforms: Rect[];
  climbables: Rect[];
}

const RECT_SIZE = 16;
const HEADER_SIZE = 12 + RECT_SIZE;

export function decodeWorld(bytes: Uint8Array): World {
  if (bytes.byteLength < HEADER_SIZE) {
    throw new Error("collision: buffer too short to contain a header");
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);

  const nSolid = view.getUint32(0, true);
  const nPlatform = view.getUint32(4, true);
  const nClimbable = view.getUint32(8, true);

  const total = nSolid + nPlatform + nClimbable;
  const expected = HEADER_SIZE + RECT_SIZE * total;
  if (bytes.byteLength !== expected) {
    // A size disagreement means the client and server disagree about the
    // encoding. Guessing would mean rendering a different world from the one
    // being simulated.
    throw new Error(
      `collision: expected ${expected} bytes for ${total} rects, got ${bytes.byteLength}`,
    );
  }

  let off = HEADER_SIZE;
  const readRects = (n: number): Rect[] => {
    const out: Rect[] = [];
    for (let i = 0; i < n; i++) {
      out.push({
        x: view.getInt32(off, true),
        y: view.getInt32(off + 4, true),
        w: view.getInt32(off + 8, true),
        h: view.getInt32(off + 12, true),
      });
      off += RECT_SIZE;
    }
    return out;
  };

  return {
    bounds: {
      x: view.getInt32(12, true),
      y: view.getInt32(16, true),
      w: view.getInt32(20, true),
      h: view.getInt32(24, true),
    },
    solids: readRects(nSolid),
    platforms: readRects(nPlatform),
    climbables: readRects(nClimbable),
  };
}
