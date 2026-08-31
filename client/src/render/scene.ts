import {
  Application,
  Container,
  Graphics,
  Sprite,
  Text,
  TextStyle,
  Texture,
  TilingSprite,
} from "pixi.js";
import { toPixels } from "@/sim/fixed";
import { type Body, isFacingLeft } from "@/sim/body";
import {
  KIND_AREA,
  KIND_DROP,
  KIND_MOB,
  KIND_PROJECTILE,
  KIND_TELEGRAPH,
  type RenderedEntity,
} from "@/game/interpolator";
import { Effects, drawHealthBar } from "./effects";
import { theme, TILE } from "./theme";
import { Sprites } from "./sprites";
import { Animator } from "./animator";

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

  // Backdrop sits outside the world container: it scrolls at its own rate, so
  // it cannot be a child of the thing it is meant to lag behind.
  #backdrop = new Container();
  #hillsFar!: TilingSprite;
  #hillsNear!: TilingSprite;

  #world = new Container();
  #terrainLayer = new Container();
  #grid = new Graphics();
  #entityLayer = new Container();
  #selfLayer = new Container();

  #selfSprite: Sprite;
  #selfAnim = new Animator();
  #selfBars = new Graphics();
  #ghostGfx = new Graphics();
  #others = new Map<number, EntitySprite>();
  #effects = new Effects();

  #sprites: Sprites;
  #mapSize = { w: 0, h: 0 };
  #showGhost = false;

  // How much the world is magnified. The art is 32-pixel tiles drawn at
  // 1:1, which on a modern display is a very long way away -- the character
  // is 48 pixels tall on a 1440-pixel screen. Two is close enough to read
  // faces and far enough to see what is about to hit you.
  #zoom = DEFAULT_ZOOM;

  private constructor(app: Application, sprites: Sprites) {
    this.#app = app;
    this.#sprites = sprites;

    this.#selfSprite = new Sprite(sprites.player("idle", 0));
    // Anchored at the feet, centred: the simulation places a body by its
    // top-left corner but the sprite is wider than the body, and hanging it
    // from the feet is what keeps a swing from shifting the character.
    this.#selfSprite.anchor.set(0.5, 1);
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

    const sprites = await Sprites.load();

    const scene = new Scene(app, sprites);

    scene.#hillsFar = tiling(sprites.hillsFar);
    scene.#hillsNear = tiling(sprites.hillsNear);
    scene.#backdrop.addChild(scene.#hillsFar, scene.#hillsNear);

    app.stage.addChild(scene.#backdrop, scene.#world);
    scene.#world.addChild(
      scene.#grid,
      scene.#terrainLayer,
      scene.#entityLayer,
      scene.#selfLayer,
      // Effects sit above everything, since a damage number that renders
      // behind a mob is a damage number nobody reads.
      scene.#effects.container,
    );
    scene.#selfLayer.addChild(scene.#ghostGfx, scene.#selfSprite, scene.#selfBars);
    scene.#world.scale.set(scene.#zoom);
    scene.#ghostGfx.visible = false;
    return scene;
  }

  /** Draws the static map once. Geometry never changes during a session. */
  setMap(geo: MapGeometry): void {
    this.#mapSize = { w: geo.width, h: geo.height };

    this.#terrainLayer.removeChildren().forEach((c) => c.destroy());

    // The grid is a tuning aid, not scenery: it is the only reference for
    // speed and jump height while judging movement, and it is clutter over
    // finished art. It rides the same toggle as the server ghost, which is
    // the other thing that is only there to be measured against.
    const g = this.#grid;
    g.clear();
    for (let x = 0; x <= geo.width; x += TILE) g.moveTo(x, 0).lineTo(x, geo.height);
    for (let y = 0; y <= geo.height; y += TILE) g.moveTo(0, y).lineTo(geo.width, y);
    g.stroke({ width: 1, color: theme.grid, alpha: 0.5 });
    this.#grid.visible = this.#showGhost;

    for (const r of geo.solids) this.#tileSolid(r);
    for (const r of geo.platforms) this.#tilePlatform(r);
    for (const r of geo.climbables) this.#tileRope(r);
  }

  /**
   * Tiles a solid: a grass-capped top row, plain fill beneath, and lit or
   * shadowed edges down the sides.
   *
   * Tiled rather than stretched, because a stretched 32-pixel texture across a
   * 1280-wide floor is a smear. The cost is a sprite per tile, which for these
   * maps is a few hundred -- Pixi batches them into one draw call.
   */
  #tileSolid(r: Rect): void {
    const tile = this.#sprites.manifest.terrain.tile;

    for (let y = r.y; y < r.y + r.h; y += tile) {
      for (let x = r.x; x < r.x + r.w; x += tile) {
        const top = y === r.y;
        let which: Parameters<Sprites["terrain"]>[0] = top ? "groundTop" : "groundFill";

        // Edges only below the cap: a grass top already reads as an edge, and
        // shading it again makes the corner muddy.
        if (!top && x === r.x && r.w > tile) which = "groundLeft";
        else if (!top && x + tile >= r.x + r.w && r.w > tile) which = "groundRight";

        this.#placeTile(this.#sprites.terrain(which), x, y, tile, tile, r);
      }
    }
  }

  #tilePlatform(r: Rect): void {
    const tile = this.#sprites.manifest.terrain.tile;
    for (let x = r.x; x < r.x + r.w; x += tile) {
      // The plank is drawn into the top half of its tile, so it lands on the
      // platform's own top edge rather than hanging below it.
      this.#placeTile(this.#sprites.terrain("platform"), x, r.y, tile, tile, {
        ...r,
        h: tile,
      });
    }
  }

  #tileRope(r: Rect): void {
    const tile = this.#sprites.manifest.terrain.tile;
    for (let y = r.y; y < r.y + r.h; y += tile) {
      const sprite = new Sprite(this.#sprites.terrain("rope"));
      sprite.position.set(r.x + r.w / 2 - tile / 2, y);
      // Clipped at the bottom so a rope ends where the map says it ends.
      const overflow = y + tile - (r.y + r.h);
      if (overflow > 0) sprite.height = tile - overflow;
      this.#terrainLayer.addChild(sprite);
    }
  }

  /** Places one tile, clipped to the rectangle it belongs to. */
  #placeTile(texture: Texture, x: number, y: number, w: number, h: number, within: Rect): void {
    const sprite = new Sprite(texture);
    sprite.position.set(x, y);

    // A rectangle is rarely a whole number of tiles. Trimming the last one
    // keeps terrain inside its own collision box, which is what makes the art
    // and the physics agree.
    const right = Math.min(x + w, within.x + within.w);
    const bottom = Math.min(y + h, within.y + within.h);
    sprite.width = right - x;
    sprite.height = bottom - y;

    this.#terrainLayer.addChild(sprite);
  }

  /**
   * Plays a swing on whoever cast something.
   *
   * Driven by the server's event rather than by the client's own keypress: the
   * server decides whether a cast happened at all, and animating on the press
   * would show a swing that was refused.
   */
  playAttack(entityId: number, selfId: number): void {
    const now = performance.now();
    if (entityId === selfId) {
      this.#selfAnim.attack(now);
      return;
    }
    this.#others.get(entityId)?.attack(now);
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
    placeCharacter(this.#selfSprite, this.#selfAnim, this.#sprites, body, performance.now());
    this.#selfSprite.label = name;

    // The backdrop lags the camera. Two layers at different rates is the
    // cheapest thing that turns a flat background into distance.
    this.#parallax(toPixels(body.x), toPixels(body.y));

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
    const now = performance.now();

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
      sprite.update(e, this.#sprites, now);
    }
  }

  /**
   * Scrolls the backdrop at a fraction of the camera's speed.
   *
   * Fractions rather than one shared rate: layers that move together are one
   * layer, and the whole effect is the difference between them.
   */
  #parallax(cx: number, cy: number): void {
    const vw = this.#app.screen.width;
    const vh = this.#app.screen.height;

    this.#hillsFar.width = vw;
    this.#hillsNear.width = vw;

    // Anchored to the bottom of the view, so the hills sit on the horizon
    // wherever the window happens to end.
    this.#hillsFar.position.set(0, vh - this.#hillsFar.height - 40);
    this.#hillsNear.position.set(0, vh - this.#hillsNear.height);

    this.#hillsFar.tilePosition.x = -cx * 0.12;
    this.#hillsNear.tilePosition.x = -cx * 0.28;

    // A little vertical drift too, so climbing a map feels like climbing.
    this.#hillsFar.tilePosition.y = -cy * 0.04;
    this.#hillsNear.tilePosition.y = -cy * 0.08;
  }

  toggleGhost(): boolean {
    this.#showGhost = !this.#showGhost;
    this.#grid.visible = this.#showGhost;
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

  /** The current magnification. */
  get zoom(): number {
    return this.#zoom;
  }

  /**
   * Sets the magnification, clamped to what the map can actually fill.
   *
   * Returns what it settled on, which is not always what was asked for.
   */
  setZoom(zoom: number): number {
    this.#zoom = clamp(zoom, MIN_ZOOM, MAX_ZOOM);
    this.#world.scale.set(this.#zoom);
    return this.#zoom;
  }

  /**
   * Centres on a point, clamped so the view never runs past the edge of the
   * map into empty space.
   *
   * Everything here is in *world* units until the last step. The camera has to
   * reason about how much of the map fits on screen, and at a magnification of
   * two that is half the pixels -- so the viewport is divided by the zoom
   * before anything is compared to the map, and the result multiplied back at
   * the end. Comparing screen pixels to world units directly is what makes a
   * zoomed camera clamp to the wrong edge and jam against nothing.
   */
  #centreCamera(cx: number, cy: number): void {
    const vw = this.#app.screen.width / this.#zoom;
    const vh = this.#app.screen.height / this.#zoom;

    let x = vw / 2 - cx;
    let y = vh / 2 - cy;

    // When the map is smaller than the viewport, centre it instead of clamping
    // to a corner.
    x = this.#mapSize.w < vw ? (vw - this.#mapSize.w) / 2 : clamp(x, vw - this.#mapSize.w, 0);
    y = this.#mapSize.h < vh ? (vh - this.#mapSize.h) / 2 : clamp(y, vh - this.#mapSize.h, 0);

    // Whole *screen* pixels: a fractional container offset makes 1px strokes
    // shimmer, and the offset is applied after scaling.
    this.#world.position.set(
      Math.round(x * this.#zoom),
      Math.round(y * this.#zoom),
    );
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
  #sprite = new Sprite();
  #anim = new Animator();
  #bars = new Graphics();
  #label: Text | null = null;

  // When this entity first appeared on screen. A telegraph fills its bar from
  // the client's own clock rather than from a per-tick update, so the whole
  // wind-up costs one message; this is the clock it counts from.
  #bornAt = 0;

  constructor(e: RenderedEntity) {
    // Anchored at the feet for the same reason the player is: sprites are
    // wider than the bodies they stand in.
    this.#sprite.anchor.set(0.5, 1);
    this.#sprite.visible = false;

    this.container.addChild(this.#gfx, this.#sprite, this.#bars);

    // Only players and mobs are named. A label over every coin on the floor
    // would bury the map under text.
    const caption = labelFor(e);
    if (caption) {
      this.#label = new Text({
        text: caption,
        style: new TextStyle({
          fontFamily: "ui-monospace, Menlo, monospace",
          fontSize: e.kind === KIND_MOB ? 10 : 11,
          fontWeight: e.kind === KIND_TELEGRAPH ? "700" : "400",
          fill:
            e.kind === KIND_TELEGRAPH
              ? theme.telegraph
              : e.kind === KIND_MOB
                ? tierTint(e.tier) || theme.mobEdge
                : theme.nameText,
          // The one label that has to be readable over whatever it is drawn
          // on top of, because it is drawn on top of the floor of a fight.
          stroke: { color: 0x0b0d12, width: e.kind === KIND_TELEGRAPH ? 3 : 0 },
        }),
      });
      this.#label.anchor.set(0.5, 1);
      this.container.addChild(this.#label);
    }
  }

  /** Plays a swing, held for as long as the animator says. */
  attack(now: number): void {
    this.#anim.attack(now);
  }

  update(e: RenderedEntity, sprites: Sprites, now: number): void {
    if (this.#bornAt === 0) this.#bornAt = now;

    const px = toPixels(e.x);
    const py = toPixels(e.y);
    const pw = toPixels(e.w);
    const ph = toPixels(e.h);

    this.#gfx.clear();
    this.#bars.clear();
    this.#sprite.visible = false;
    this.#sprite.tint = 0xffffff;
    this.#sprite.alpha = 1;

    switch (e.kind) {
      case KIND_DROP: {
        // Loot bobs, so a coin on the floor catches the eye the way it should.
        this.#sprite.texture =
          e.dropGold > 0 ? sprites.coin(Math.floor(now / 120)) : sprites.item;
        this.#sprite.scale.set(1);
        this.#sprite.position.set(
          Math.round(px + pw / 2),
          Math.round(py + ph + Math.sin(now / 260 + e.id) * 2),
        );
        this.#sprite.visible = true;
        break;
      }

      case KIND_PROJECTILE: {
        // A bolt, drawn as a bolt. Before this it fell through to the branch
        // below and was rendered as a tinted person, which is funny exactly
        // once.
        const r = Math.max(3, Math.min(pw, ph) / 2);
        const cx = px + pw / 2;
        const cy = py + ph / 2;
        this.#gfx
          .circle(cx, cy, r * 1.9)
          .fill({ color: theme.projectile, alpha: 0.22 })
          .circle(cx, cy, r)
          .fill({ color: theme.projectile })
          .circle(cx, cy, r * 0.45)
          .fill({ color: theme.projectileCore });
        break;
      }

      case KIND_AREA: {
        // Ground effects breathe, so a patch of burning floor is obviously
        // still burning rather than a mark left behind.
        const pulse = 0.10 + 0.05 * Math.sin(now / 220 + e.id);
        const r = Math.min(pw, ph) / 2;
        this.#gfx
          .circle(px + pw / 2, py + ph / 2, r)
          .fill({ color: theme.area, alpha: pulse })
          .stroke({ width: 2, color: theme.areaEdge, alpha: 0.7 });
        break;
      }

      case KIND_TELEGRAPH: {
        this.#drawTelegraph(e, px, py, pw, ph, now);
        break;
      }

      case KIND_MOB: {
        const dead = e.hp === 0;
        const texture = sprites.mob(pw, ph, Math.floor(now / 320));

        if (texture) {
          this.#sprite.texture = texture;
          this.#sprite.scale.x = isFacingLeft(e as unknown as Body) ? -1 : 1;
          this.#sprite.scale.y = 1;
          this.#sprite.position.set(Math.round(px + pw / 2), Math.round(py + ph));
          this.#sprite.visible = true;

          // A champion or rare is the same creature wearing a different
          // colour. The modifiers are combinatorial -- there is no sprite to
          // draw for "Brutal Swift" -- and a tint reads across a room where a
          // name does not.
          const tint = tierTint(e.tier);
          if (tint !== 0 && !dead) {
            this.#sprite.tint = tint;
            this.#gfx
              .rect(px - 2, py - 2, pw + 4, ph + 4)
              .stroke({ width: 2, color: tint, alpha: 0.55 });
          }

          // Dead mobs fade rather than vanishing, so a kill has a beat. A
          // hit flash would go here too, but the snapshot carries no such
          // flag -- damage numbers already say a hit landed.
          if (dead) {
            this.#sprite.alpha = 0.45;
            this.#sprite.tint = theme.mobDead;
          }
        } else {
          // No art for this size. A box says "missing sprite"; drawing some
          // other creature would be a lie about what is attacking you.
          this.#gfx
            .rect(px, py, pw, ph)
            .fill({ color: dead ? theme.mobDead : theme.mob, alpha: dead ? 0.45 : 1 })
            .stroke({ width: 1.5, color: theme.mobEdge });
        }

        if (!dead) drawHealthBar(this.#bars, px, py - 8, pw, e.hp, e.hpMax);
        break;
      }

      default:
        // Another player. Same mannequin, tinted, so two characters standing
        // together are obviously the same kind of thing.
        placeCharacter(this.#sprite, this.#anim, sprites, e as unknown as Body, now);
        this.#sprite.tint = theme.otherTint;
        this.#sprite.visible = true;
        drawHealthBar(this.#bars, px, py - 8, pw, e.hp, e.hpMax);
    }

    if (this.#label) {
      this.#label.position.set(px + pw / 2, py - (e.kind === KIND_MOB ? 12 : 14));

      // A mob is named only while it is damaged, the same condition as its
      // health bar. Labelling every mob buries the map in overlapping text --
      // two slimes standing together render as "Green SlimGreen Slime" -- and
      // the name matters when you are fighting something, not when you are
      // walking past it.
      //
      // Champions are no exception, and that was tried: their names are long
      // enough that two adjacent ones ("Brutal Hulking Armoured Swift Green
      // Slime" twice over) are a solid bar of unreadable text. The colour is
      // the warning at a distance -- it reads across a room where a name does
      // not -- and the modifiers are the detail, wanted at the point of
      // actually fighting the thing.
      this.#label.visible = e.kind !== KIND_MOB || (e.hp > 0 && e.hp < e.hpMax);
    }
  }

  /**
   * Draws a wind-up: the area an attack is about to cover, filling as it
   * closes.
   *
   * The fill is the whole point. A static outline says "something is
   * happening here"; a bar that visibly runs out says how long you have, which
   * is the information a player is actually acting on. It is animated from the
   * client's clock because the server sends the duration once, on entry, and
   * then says nothing more until the attack lands.
   */
  #drawTelegraph(
    e: RenderedEntity,
    px: number,
    py: number,
    pw: number,
    ph: number,
    now: number,
  ): void {
    // hp is the ticks left when the marker appeared, hpMax the whole wind-up.
    const totalMs = (e.hpMax / TICKS_PER_SECOND) * 1000;
    const leftAtEntryMs = (e.hp / TICKS_PER_SECOND) * 1000;
    const elapsed = leftAtEntryMs > 0 ? totalMs - leftAtEntryMs + (now - this.#bornAt) : 0;
    const progress = totalMs > 0 ? clamp(elapsed / totalMs, 0, 1) : 1;

    // Brightening as it closes, so the last moments read as urgent even at the
    // edge of vision.
    const alpha = 0.16 + 0.20 * progress;

    this.#gfx
      .rect(px, py, pw, ph)
      .fill({ color: theme.telegraph, alpha })
      .rect(px, py, pw * progress, ph)
      .fill({ color: theme.telegraphFill, alpha: 0.34 })
      .rect(px, py, pw, ph)
      .stroke({ width: 2, color: theme.telegraph, alpha: 0.85 });
  }
}

/** The colour a tier is drawn in, or 0 for an ordinary mob. */
function tierTint(tier: string): number {
  switch (tier) {
    case "champion":
      return theme.champion;
    case "rare":
      return theme.rare;
    default:
      return 0;
  }
}

/** Ticks per second, matching the server's tick rate. */
const TICKS_PER_SECOND = 20;

function labelFor(e: RenderedEntity): string {
  // Loot, bolts, and ground effects are not named: a label over every coin and
  // every spark would bury the map under text.
  //
  // A telegraph is the exception, and it is why the name is carried at all.
  // Knowing which attack is winding up is the difference between reading a
  // fight and reacting to a red box -- and it is the whole of the counterplay
  // when two abilities want opposite things, one asking the party to stack and
  // the next to scatter.
  if (e.kind === KIND_DROP || e.kind === KIND_PROJECTILE || e.kind === KIND_AREA) return "";
  return e.name;
}

/**
 * Points a character sprite at the right frame and puts it where the body is.
 *
 * The sprite is wider than the body -- a swing leaves its footprint -- so it
 * hangs from the feet, centred. Anchoring anywhere else makes the character
 * shift sideways when they attack, which reads as the game nudging them.
 *
 * Facing is a horizontal flip rather than a second set of frames: half the
 * sheet, and the two directions cannot drift apart.
 */
function placeCharacter(
  sprite: Sprite,
  animator: Animator,
  sprites: Sprites,
  body: Body,
  now: number,
): void {
  const { anim, frame } = animator.frame(body, now);
  sprite.texture = sprites.player(anim, frame);

  const x = toPixels(body.x);
  const y = toPixels(body.y);
  const w = toPixels(body.w);
  const h = toPixels(body.h);

  // Whole pixels. A sprite on a half-pixel is a sprite with a seam down it,
  // and at this scale the seam is a quarter of a limb.
  sprite.position.set(Math.round(x + w / 2), Math.round(y + h));
  sprite.scale.x = isFacingLeft(body) ? -1 : 1;
}

/**
 * Magnification limits.
 *
 * Below 1 the art is being shrunk, which on 32-pixel tiles turns detail into
 * noise. Above 3 a character fills enough of the screen that a telegraph can
 * land from outside it, which is the one thing the camera must never do.
 */
export const MIN_ZOOM = 1;
export const MAX_ZOOM = 3;
export const DEFAULT_ZOOM = 2;

function clamp(v: number, lo: number, hi: number): number {
  return Math.min(Math.max(v, lo), hi);
}

/** A backdrop layer, repeated across whatever width the window happens to be. */
function tiling(texture: Texture): TilingSprite {
  const sprite = new TilingSprite({ texture, width: 1, height: texture.height });
  sprite.tileScale.set(1);
  return sprite;
}
