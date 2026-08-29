import { Assets, Rectangle, Texture } from "pixi.js";

/**
 * The sprite sheets, and the manifest that says where everything is in them.
 *
 * Nothing here hard-codes a frame position. The generator that draws the art
 * writes the manifest alongside it, so a sheet and the code reading it cannot
 * drift -- and drift, in a sprite sheet, shows up as a character sliced down
 * the middle rather than as anything that fails.
 */

export interface SpriteManifest {
  player: {
    frameW: number;
    frameH: number;
    bodyW: number;
    bodyH: number;
    /** Blank space each side of the body, for swings that leave its footprint. */
    pad: number;
    anim: Record<AnimName, number>;
    lengths: Record<AnimName, number>;
  };
  terrain: {
    tile: number;
    index: Record<TerrainTile, number>;
  };
  /**
   * Keyed by "WxH". A snapshot carries a mob's display name and its size, not
   * a content id, and size is the stable half of that -- the manifest is
   * generated from the same content the server simulates, so the two agree by
   * construction rather than by convention.
   */
  mobs: Record<string, { x: number; w: number; h: number; frames: number }>;
  drops: { size: number; coinFrames: number; itemFrame: number };
}

export type AnimName = "idle" | "run" | "jump" | "fall" | "climb" | "attack";
export type TerrainTile =
  | "groundTop"
  | "groundFill"
  | "groundLeft"
  | "groundRight"
  | "platform"
  | "rope";

export class Sprites {
  readonly manifest: SpriteManifest;

  #player: Texture[] = [];
  #terrain: Texture[] = [];
  #mobs = new Map<string, Texture[]>();
  #coins: Texture[] = [];
  #item!: Texture;

  readonly hillsFar: Texture;
  readonly hillsNear: Texture;

  private constructor(
    manifest: SpriteManifest,
    sheets: Record<string, Texture>,
  ) {
    this.manifest = manifest;
    this.hillsFar = sheets.hillsFar!;
    this.hillsNear = sheets.hillsNear!;

    const p = manifest.player;
    this.#player = sliceRow(sheets.player!, p.frameW, p.frameH);

    const t = manifest.terrain;
    this.#terrain = sliceRow(sheets.terrain!, t.tile, t.tile);

    for (const [key, mob] of Object.entries(manifest.mobs)) {
      const frames: Texture[] = [];
      for (let i = 0; i < mob.frames; i++) {
        // Mobs are bottom-aligned on their sheet, because they stand on the
        // ground and a top-aligned sheet would sink the short ones into it.
        frames.push(
          sub(sheets.mobs!, mob.x + i * mob.w, sheets.mobs!.height - mob.h, mob.w, mob.h),
        );
      }
      this.#mobs.set(key, frames);
    }

    const d = manifest.drops;
    for (let i = 0; i < d.coinFrames; i++) {
      this.#coins.push(sub(sheets.drops!, i * d.size, 0, d.size, d.size));
    }
    this.#item = sub(sheets.drops!, d.itemFrame * d.size, 0, d.size, d.size);
  }

  static async load(base = "/sprites"): Promise<Sprites> {
    const manifest: SpriteManifest = await fetch(`${base}/sprites.json`).then((r) => {
      if (!r.ok) throw new Error(`could not load the sprite manifest: ${r.status}`);
      return r.json();
    });

    const names = {
      player: "player.png",
      terrain: "terrain.png",
      mobs: "mobs.png",
      drops: "drops.png",
      hillsFar: "hills-far.png",
      hillsNear: "hills-near.png",
    };

    const sheets: Record<string, Texture> = {};
    await Promise.all(
      Object.entries(names).map(async ([key, file]) => {
        const texture = await Assets.load<Texture>(`${base}/${file}`);
        // Nearest-neighbour, or every sprite is a blur. This is the single
        // most important line in the file: pixel art scaled with linear
        // filtering stops being pixel art.
        texture.source.scaleMode = "nearest";
        sheets[key] = texture;
      }),
    );

    return new Sprites(manifest, sheets);
  }

  /** One frame of a player animation, wrapping the index into the cycle. */
  player(anim: AnimName, frame: number): Texture {
    const start = this.manifest.player.anim[anim];
    const length = this.manifest.player.lengths[anim];
    return this.#player[start + (((frame % length) + length) % length)]!;
  }

  terrain(tile: TerrainTile): Texture {
    return this.#terrain[this.manifest.terrain.index[tile]]!;
  }

  /**
   * The frames for a mob of a given size, or null if nothing matches.
   *
   * Null rather than a substitute: a mob drawn as the wrong creature is worse
   * than one drawn as a coloured box, because the box is obviously missing art
   * and the wrong creature is a lie about what is attacking you.
   */
  mob(w: number, h: number, frame: number): Texture | null {
    const frames = this.#mobs.get(`${Math.round(w)}x${Math.round(h)}`);
    if (!frames) return null;
    return frames[frame % frames.length]!;
  }

  coin(frame: number): Texture {
    return this.#coins[frame % this.#coins.length]!;
  }

  get item(): Texture {
    return this.#item;
  }
}

function sliceRow(sheet: Texture, w: number, h: number): Texture[] {
  const out: Texture[] = [];
  for (let x = 0; x + w <= sheet.width; x += w) {
    out.push(sub(sheet, x, 0, w, h));
  }
  return out;
}

function sub(sheet: Texture, x: number, y: number, w: number, h: number): Texture {
  return new Texture({ source: sheet.source, frame: new Rectangle(x, y, w, h) });
}
