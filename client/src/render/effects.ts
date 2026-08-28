import { Container, Graphics, Text, TextStyle } from "pixi.js";
import { theme } from "./theme";

/**
 * Transient combat feedback: floating damage numbers and death fades.
 *
 * These exist because state alone does not communicate a fight. A health bar
 * that drops from 900 to 700 looks the same whether it was one heavy hit or
 * two light ones, and those feel completely different to play. The server
 * sends damage as discrete events precisely so the client can show them as
 * discrete events.
 */

/** How long a damage number stays on screen. */
const FLOAT_MS = 750;

/** How far it drifts upward over its life, in pixels. */
const FLOAT_RISE = 42;

/**
 * Cap on simultaneous numbers.
 *
 * A crowded fight can produce far more damage events than are readable, and
 * every one is a Text object. Beyond this the oldest are retired: the
 * information is already lost to the reader at that density, so spending
 * frames on it buys nothing.
 */
const MAX_FLOATERS = 48;

interface Floater {
  text: Text;
  bornAt: number;
  x: number;
  y: number;
  driftX: number;
}

export class Effects {
  readonly container = new Container();
  #floaters: Floater[] = [];

  /** Adds a damage number above a point. */
  damage(x: number, y: number, amount: number, opts: { critical?: boolean; toSelf?: boolean } = {}): void {
    if (this.#floaters.length >= MAX_FLOATERS) {
      this.#retire(0);
    }

    const critical = opts.critical ?? false;
    const toSelf = opts.toSelf ?? false;

    const text = new Text({
      text: critical ? `${amount}!` : String(amount),
      style: new TextStyle({
        fontFamily: "ui-monospace, Menlo, monospace",
        // Damage taken must be distinguishable from damage dealt at a glance,
        // in the middle of a fight, without reading the number.
        fontSize: critical ? 20 : toSelf ? 16 : 14,
        fontWeight: critical ? "700" : "600",
        fill: toSelf ? theme.damageTaken : critical ? theme.damageCrit : theme.damageDealt,
        stroke: { color: 0x0b0d12, width: 3 },
      }),
    });
    text.anchor.set(0.5, 1);
    text.position.set(x, y);

    this.container.addChild(text);
    this.#floaters.push({
      text,
      bornAt: performance.now(),
      x,
      y,
      // A slight sideways drift keeps rapid hits on one target from stacking
      // into an unreadable column.
      driftX: (Math.random() - 0.5) * 24,
    });
  }

  /** Advances every floater and retires the expired ones. */
  update(now: number): void {
    for (let i = this.#floaters.length - 1; i >= 0; i--) {
      const f = this.#floaters[i]!;
      const age = (now - f.bornAt) / FLOAT_MS;

      if (age >= 1) {
        this.#retire(i);
        continue;
      }

      // Ease out, so a number moves fastest when it first appears and settles
      // as it fades -- which is what makes it readable rather than a blur.
      const eased = 1 - (1 - age) * (1 - age);
      f.text.position.set(f.x + f.driftX * eased, f.y - FLOAT_RISE * eased);
      f.text.alpha = age < 0.7 ? 1 : 1 - (age - 0.7) / 0.3;
    }
  }

  clear(): void {
    for (const f of this.#floaters) f.text.destroy();
    this.#floaters.length = 0;
  }

  #retire(i: number): void {
    const f = this.#floaters[i];
    if (!f) return;
    this.container.removeChild(f.text);
    f.text.destroy();
    this.#floaters.splice(i, 1);
  }
}

/**
 * Draws a health bar above an entity.
 *
 * Only shown for damaged entities: a room full of full-health bars is visual
 * noise that hides the one thing the bars are for, which is seeing what is
 * hurt.
 */
export function drawHealthBar(
  g: Graphics,
  x: number,
  y: number,
  width: number,
  hp: number,
  hpMax: number,
): void {
  if (hpMax === 0 || hp >= hpMax) return;

  const w = Math.max(width, 24);
  const h = 4;
  const frac = Math.max(0, Math.min(1, hp / hpMax));

  g.rect(x, y, w, h).fill({ color: 0x000000, alpha: 0.6 });
  g.rect(x, y, w * frac, h).fill(
    frac > 0.5 ? theme.healthHigh : frac > 0.25 ? theme.healthMid : theme.healthLow,
  );
}
