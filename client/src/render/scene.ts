import { Application, Container, Graphics, Text, TextStyle } from "pixi.js";
import { toPixels } from "@/sim/fixed";
import { type Body, isClimbing, isFacingLeft, isGrounded } from "@/sim/body";
import { KIND_DROP, KIND_MOB, type RenderedEntity } from "@/game/interpolator";
import { Effects, drawHealthBar } from "./effects";
import { theme, TILE } from "./theme";

/**
 * PixiJS scene: map geometry, entities, camera.
 *
 * The renderer is deliberately dumb. It draws what it is handed and owns no
 * game state, so every question about where something is has exactly one
 * answer, in the simulation. It is also the only place fixed-point becomes
 * pixels.
 */

export interface MapGeometry {
  width: number;
  height: number;
  solids: Rect[];
  platforms: Rect[];
  climbables: Rect[];
}

export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

export class Scene {
  #app: Application;
  #world = new Container();
  #terrain = new Graphics();
  #entityLayer = new Container();
  #selfLayer = new Container();

  #selfGfx = new Graphics();
  #selfBars = new Graphics();
  #ghostGfx = new Graphics();
  #others = new Map<number, EntitySprite>();
  #effects = new Effects();

  #mapSize = { w: 0, h: 0 };
  #showGhost = false;

  private constructor(app: Application) {
    this.#app = app;
  }

  static async create(mount: HTMLElement): Promise<Scene> {
    const app = new Application();
    await app.init({
      background: theme.background,
      resizeTo: window,
      antialias: true,
      // The renderer is drawing flat shapes at 20 Hz of real state change;
      // matching the display refresh keeps interpolation smooth.
      autoDensity: true,
      resolution: window.devicePixelRatio || 1,
    });
    mount.appendChild(app.canvas);

    const scene = new Scene(app);
    app.stage.addChild(scene.#world);
    scene.#world.addChild(
      scene.#terrain,
      scene.#entityLayer,
      scene.#selfLayer,
      // Effects sit above everything, since a damage number that renders
      // behind a mob is a damage number nobody reads.
      scene.#effects.container,
    );
    scene.#selfLayer.addChild(scene.#ghostGfx, scene.#selfGfx, scene.#selfBars);
    scene.#ghostGfx.visible = false;
    return scene;
  }

  /** Draws the static map once. Geometry never changes during a session. */
  setMap(geo: MapGeometry): void {
    this.#mapSize = { w: geo.width, h: geo.height };

    const g = this.#terrain;
    g.clear();

    // A faint grid gives the eye a reference for speed and jump height, which
    // is exactly what needs judging in this milestone.
    for (let x = 0; x <= geo.width; x += TILE) {
      g.moveTo(x, 0).lineTo(x, geo.height);
    }
    for (let y = 0; y <= geo.height; y += TILE) {
      g.moveTo(0, y).lineTo(geo.width, y);
    }
    g.stroke({ width: 1, color: theme.grid, alpha: 0.55 });

    for (const r of geo.solids) {
      g.rect(r.x, r.y, r.w, r.h).fill(theme.solid).stroke({ width: 1, color: theme.solidEdge });
    }
    // Platforms are drawn as a thin lip rather than a slab, so that "you can
    // jump up through this" is legible at a glance.
    for (const r of geo.platforms) {
      g.rect(r.x, r.y, r.w, Math.max(r.h, 4))
        .fill(theme.platform)
        .stroke({ width: 1, color: theme.platformEdge });
    }
    for (const r of geo.climbables) {
      g.rect(r.x + r.w / 2 - 1.5, r.y, 3, r.h).fill(theme.rope);
      for (let y = r.y + 8; y < r.y + r.h; y += 16) {
        g.rect(r.x + r.w / 2 - 5, y, 10, 2).fill(theme.rope);
      }
    }
  }

  /** Exposes the effects layer so the game loop can spawn damage numbers. */
  get effects(): Effects {
    return this.#effects;
  }

  /** Converts a fixed-point world position to pixels, for effect placement. */
  static toPixels(v: number): number {
    return toPixels(v);
  }

  /** Draws the local player and points the camera at them. */
  drawSelf(body: Body, name: string, hp: number, hpMax: number, authoritative?: Body): void {
    drawCharacter(this.#selfGfx, body, theme.self, theme.selfEdge);
    this.#selfGfx.label = name;

    this.#selfBars.clear();
    drawHealthBar(
      this.#selfBars,
      toPixels(body.x),
      toPixels(body.y) - 8,
      toPixels(body.w),
      hp,
      hpMax,
    );

    // The ghost shows unsmoothed authoritative state. Invaluable while tuning
    // prediction: if the ghost and the body separate, reconciliation is wrong.
    if (this.#showGhost && authoritative) {
      this.#ghostGfx.visible = true;
      this.#ghostGfx.clear();
      this.#ghostGfx
        .rect(toPixels(authoritative.x), toPixels(authoritative.y),
              toPixels(authoritative.w), toPixels(authoritative.h))
        .stroke({ width: 1, color: theme.ghost, alpha: 0.9 });
    } else {
      this.#ghostGfx.visible = false;
    }

    this.#centreCamera(toPixels(body.x) + toPixels(body.w) / 2,
                       toPixels(body.y) + toPixels(body.h) / 2);
  }

  /** Draws everyone else, creating and retiring sprites as they come and go. */
  drawEntities(entities: RenderedEntity[], removed: number[]): void {
    for (const id of removed) {
      const sprite = this.#others.get(id);
      if (sprite) {
        this.#entityLayer.removeChild(sprite.container);
        sprite.container.destroy({ children: true });
        this.#others.delete(id);
      }
    }

    for (const e of entities) {
      let sprite = this.#others.get(e.id);
      if (!sprite) {
        sprite = new EntitySprite(e);
        this.#others.set(e.id, sprite);
        this.#entityLayer.addChild(sprite.container);
      }
      sprite.update(e);
    }
  }

  toggleGhost(): boolean {
    this.#showGhost = !this.#showGhost;
    return this.#showGhost;
  }

  clearEntities(): void {
    for (const sprite of this.#others.values()) {
      this.#entityLayer.removeChild(sprite.container);
      sprite.container.destroy({ children: true });
    }
    this.#others.clear();
    this.#effects.clear();
  }

  /**
   * Centres on a point, clamped so the view never runs past the edge of the
   * map into empty space.
   */
  #centreCamera(cx: number, cy: number): void {
    const vw = this.#app.screen.width;
    const vh = this.#app.screen.height;

    let x = vw / 2 - cx;
    let y = vh / 2 - cy;

    // When the map is smaller than the viewport, centre it instead of clamping
    // to a corner.
    x = this.#mapSize.w < vw ? (vw - this.#mapSize.w) / 2 : clamp(x, vw - this.#mapSize.w, 0);
    y = this.#mapSize.h < vh ? (vh - this.#mapSize.h) / 2 : clamp(y, vh - this.#mapSize.h, 0);

    // Whole pixels: a fractional container offset makes 1px strokes shimmer.
    this.#world.position.set(Math.round(x), Math.round(y));
  }

  get ticker() {
    return this.#app.ticker;
  }

  destroy(): void {
    this.#app.destroy(true, { children: true });
  }
}

/** One remote entity: its body, its label, and its health bar. */
class EntitySprite {
  readonly container = new Container();
  #gfx = new Graphics();
  #bars = new Graphics();
  #label: Text | null = null;

  constructor(e: RenderedEntity) {
    this.container.addChild(this.#gfx, this.#bars);

    // Only players and mobs are named. A label over every coin on the floor
    // would bury the map under text.
    const caption = labelFor(e);
    if (caption) {
      this.#label = new Text({
        text: caption,
        style: new TextStyle({
          fontFamily: "ui-monospace, Menlo, monospace",
          fontSize: e.kind === KIND_MOB ? 10 : 11,
          fill: e.kind === KIND_MOB ? theme.mobEdge : theme.nameText,
        }),
      });
      this.#label.anchor.set(0.5, 1);
      this.container.addChild(this.#label);
    }
  }

  update(e: RenderedEntity): void {
    const px = toPixels(e.x);
    const py = toPixels(e.y);
    const pw = toPixels(e.w);
    const ph = toPixels(e.h);

    this.#gfx.clear();
    this.#bars.clear();

    switch (e.kind) {
      case KIND_DROP:
        drawDrop(this.#gfx, px, py, pw, ph, e);
        break;

      case KIND_MOB: {
        const dead = e.hp === 0;
        this.#gfx
          .rect(px, py, pw, ph)
          .fill({ color: dead ? theme.mobDead : theme.mob, alpha: dead ? 0.45 : 1 })
          .stroke({ width: 1.5, color: dead ? theme.mobDead : theme.mobEdge });

        // Facing marker, so it is obvious which way a mob is about to swing.
        const eyeX = isFacingLeft(e as unknown as Body) ? px + pw * 0.3 : px + pw * 0.7;
        this.#gfx.circle(eyeX, py + ph * 0.3, 2).fill(0x0b0d12);

        if (!dead) drawHealthBar(this.#bars, px, py - 8, pw, e.hp, e.hpMax);
        break;
      }

      default:
        drawCharacter(this.#gfx, e, theme.other, theme.otherEdge);
        drawHealthBar(this.#bars, px, py - 8, pw, e.hp, e.hpMax);
    }

    if (this.#label) {
      this.#label.position.set(px + pw / 2, py - (e.kind === KIND_MOB ? 12 : 14));

      // A mob is named only while it is damaged, the same condition as its
      // health bar. Labelling every mob buries the map in overlapping text --
      // two slimes standing together render as "Green SlimGreen Slime" -- and
      // the name matters when you are fighting something, not when you are
      // walking past it.
      this.#label.visible = e.kind !== KIND_MOB || (e.hp > 0 && e.hp < e.hpMax);
    }
  }
}

function labelFor(e: RenderedEntity): string {
  if (e.kind === KIND_DROP) return "";
  return e.name;
}

/**
 * Draws ground loot as a small diamond.
 *
 * Gold and items are coloured differently because deciding whether something
 * is worth walking back for should not require reading a tooltip.
 */
function drawDrop(
  g: Graphics,
  px: number,
  py: number,
  pw: number,
  ph: number,
  e: RenderedEntity,
): void {
  const cx = px + pw / 2;
  const cy = py + ph / 2;
  const r = Math.max(pw, ph) / 2;
  const colour = e.dropGold > 0 ? theme.dropGold : theme.dropItem;

  g.moveTo(cx, cy - r)
    .lineTo(cx + r, cy)
    .lineTo(cx, cy + r)
    .lineTo(cx - r, cy)
    .closePath()
    .fill(colour)
    .stroke({ width: 1, color: 0x0b0d12 });
}

/**
 * Draws a character box with a facing marker and a state tint.
 *
 * Flat shapes, but the state has to be visible: without a facing marker and a
 * grounded/climbing tint there is no way to tell whether the simulation is
 * doing the right thing, which is what this milestone is for.
 */
function drawCharacter(
  g: Graphics,
  b: { x: number; y: number; w: number; h: number; flags: number },
  fill: number,
  edge: number,
): void {
  const x = toPixels(b.x);
  const y = toPixels(b.y);
  const w = toPixels(b.w);
  const h = toPixels(b.h);

  g.clear();
  g.rect(x, y, w, h).fill({ color: fill, alpha: isGrounded(b as Body) ? 1 : 0.82 });
  g.rect(x, y, w, h).stroke({ width: 1.5, color: isClimbing(b as Body) ? theme.rope : edge });

  // Eye marker, so facing is readable at a glance.
  const eyeX = isFacingLeft(b as Body) ? x + w * 0.28 : x + w * 0.72;
  g.circle(eyeX, y + h * 0.22, 2.5).fill(0x0b0d12);
}

function clamp(v: number, lo: number, hi: number): number {
  return Math.min(Math.max(v, lo), hi);
}
