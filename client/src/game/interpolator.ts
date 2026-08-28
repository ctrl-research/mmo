import type { EntityState, EntityDelta, Snapshot } from "@/gen/mmo/v1/game_pb";

/**
 * Renders every entity except the local player.
 *
 * Their positions arrive 20 times a second, so drawing the latest one each
 * frame would be visibly steppy. Instead everything else is drawn slightly in
 * the past, interpolating between the two snapshots that bracket the render
 * time. The cost is that other players are seen ~100 ms behind where the
 * server has them, which is invisible in play and much better than inventing
 * motion that never happened.
 */

/**
 * How far in the past to render. Two ticks: enough buffer to absorb one late
 * or dropped snapshot without the buffer starving.
 */
export const INTERP_DELAY_MS = 100;

/**
 * How far past the last known sample to extrapolate before giving up.
 *
 * Beyond this, entities visibly slide through walls and then teleport back
 * when real data arrives. Freezing is the less wrong answer.
 */
const MAX_EXTRAPOLATE_MS = 250;

/** How long a sample history to keep per entity. */
const HISTORY_MS = 1000;

interface Sample {
  t: number;
  x: number;
  y: number;
  vx: number;
  vy: number;
  flags: number;
  anim: number;
}

export interface RenderedEntity {
  id: number;
  kind: number;
  name: string;
  w: number;
  h: number;
  x: number;
  y: number;
  flags: number;
  anim: number;
  hp: number;
  hpMax: number;

  /** Content id of a mob, so the renderer can choose a sprite. */
  mobId: string;

  /** Ground loot. Exactly one of item or gold is set. */
  dropItem: string;
  dropQty: number;
  dropGold: number;
}

/** Entity kinds, mirroring mmov1.EntityKind. */
export const KIND_PLAYER = 1;
export const KIND_MOB = 2;
export const KIND_DROP = 3;

// Field-mask bits, mirroring mmov1.EntityField.
const FIELD_POS = 1 << 0;
const FIELD_VEL = 1 << 1;
const FIELD_FLAGS = 1 << 2;
const FIELD_ANIM = 1 << 3;
const FIELD_HP = 1 << 4;

interface Tracked {
  id: number;
  kind: number;
  name: string;
  w: number;
  h: number;
  hp: number;
  hpMax: number;
  mobId: string;
  dropItem: string;
  dropQty: number;
  dropGold: number;
  latest: Sample;
  history: Sample[];
}

export class Interpolator {
  #entities = new Map<number, Tracked>();

  /** Entity IDs removed since the last drain, for the renderer to clean up. */
  #removed: number[] = [];

  get size(): number {
    return this.#entities.size;
  }

  /** Applies one snapshot. selfId is skipped: the local player is predicted. */
  apply(snap: Snapshot, selfId: number, now: number): void {
    for (const state of snap.entered) {
      if (state.id === selfId) continue;
      this.#enter(state, now);
    }
    for (const delta of snap.entities) {
      if (delta.id === selfId) continue;
      this.#update(delta, now);
    }
    for (const id of snap.removed) {
      if (this.#entities.delete(id)) this.#removed.push(id);
    }
  }

  #enter(state: EntityState, now: number): void {
    const sample: Sample = {
      t: now,
      x: state.x,
      y: state.y,
      vx: state.vx,
      vy: state.vy,
      flags: state.flags,
      anim: state.anim,
    };
    this.#entities.set(state.id, {
      id: state.id,
      kind: state.kind,
      name: state.name,
      w: state.w,
      h: state.h,
      hp: state.hp,
      hpMax: state.hpMax,
      mobId: state.mobId,
      dropItem: state.dropItem,
      dropQty: state.dropQty,
      dropGold: state.dropGold,
      latest: sample,
      history: [sample],
    });
  }

  #update(delta: EntityDelta, now: number): void {
    const e = this.#entities.get(delta.id);
    // A delta for something never announced means the client missed the
    // entity's introduction. Dropping it is right: a delta without a baseline
    // has nothing to apply to.
    if (!e) return;

    const prev = e.latest;
    const next: Sample = { ...prev, t: now };

    if (delta.fieldMask & FIELD_POS) {
      next.x = delta.x;
      next.y = delta.y;
    }
    if (delta.fieldMask & FIELD_VEL) {
      next.vx = delta.vx;
      next.vy = delta.vy;
    }
    if (delta.fieldMask & FIELD_FLAGS) next.flags = delta.flags;
    if (delta.fieldMask & FIELD_ANIM) next.anim = delta.anim;
    if (delta.fieldMask & FIELD_HP) {
      e.hp = delta.hp;
      e.hpMax = delta.hpMax;
    }

    e.latest = next;
    e.history.push(next);

    const cutoff = now - HISTORY_MS;
    while (e.history.length > 2 && e.history[0]!.t < cutoff) e.history.shift();
  }

  /** Returns every entity positioned for the given render time. */
  sample(now: number): RenderedEntity[] {
    const renderTime = now - INTERP_DELAY_MS;
    const out: RenderedEntity[] = [];

    for (const e of this.#entities.values()) {
      const { x, y, flags, anim } = interpolate(e, renderTime);
      out.push({
        id: e.id,
        kind: e.kind,
        name: e.name,
        w: e.w,
        h: e.h,
        hp: e.hp,
        hpMax: e.hpMax,
        mobId: e.mobId,
        dropItem: e.dropItem,
        dropQty: e.dropQty,
        dropGold: e.dropGold,
        x,
        y,
        flags,
        anim,
      });
    }
    return out;
  }

  /** Returns and clears the IDs removed since the last call. */
  drainRemoved(): number[] {
    const out = this.#removed;
    this.#removed = [];
    return out;
  }

  clear(): void {
    for (const id of this.#entities.keys()) this.#removed.push(id);
    this.#entities.clear();
  }

  /** Returns one tracked entity's latest known position, or null. */
  positionOf(id: number): { x: number; y: number } | null {
    const e = this.#entities.get(id);
    return e ? { x: e.latest.x, y: e.latest.y } : null;
  }

  /**
   * Returns the nearest lootable drop to a point, within a radius.
   *
   * The client picks the target so a single key press loots the obvious thing;
   * the server still validates range and ownership, so a client that picks
   * badly -- or lies -- simply gets nothing.
   */
  nearestDrop(x: number, y: number, radius: number): number {
    let best = 0;
    let bestDistSq = radius * radius;

    for (const e of this.#entities.values()) {
      if (e.kind !== KIND_DROP) continue;
      const dx = e.latest.x - x;
      const dy = e.latest.y - y;
      const d = dx * dx + dy * dy;
      if (d <= bestDistSq) {
        bestDistSq = d;
        best = e.id;
      }
    }
    return best;
  }
}

function interpolate(e: Tracked, renderTime: number) {
  const h = e.history;
  const newest = h[h.length - 1]!;

  // Ahead of everything we have: extrapolate briefly along last known
  // velocity, then hold position rather than slide somewhere impossible.
  if (renderTime >= newest.t) {
    const ahead = Math.min(renderTime - newest.t, MAX_EXTRAPOLATE_MS);
    const ticks = ahead / 50;
    return {
      x: newest.x + newest.vx * ticks,
      y: newest.y + newest.vy * ticks,
      flags: newest.flags,
      anim: newest.anim,
    };
  }

  // Behind everything we have: the entity just appeared. Hold the oldest.
  const oldest = h[0]!;
  if (renderTime <= oldest.t) {
    return { x: oldest.x, y: oldest.y, flags: oldest.flags, anim: oldest.anim };
  }

  for (let i = h.length - 1; i > 0; i--) {
    const b = h[i]!;
    const a = h[i - 1]!;
    if (renderTime >= a.t && renderTime <= b.t) {
      const span = b.t - a.t;
      const t = span > 0 ? (renderTime - a.t) / span : 0;
      return {
        x: Math.round(a.x + (b.x - a.x) * t),
        y: Math.round(a.y + (b.y - a.y) * t),
        // Discrete state never interpolates: half of "grounded" is nonsense,
        // and a blended animation index would pick an unrelated frame.
        flags: a.flags,
        anim: a.anim,
      };
    }
  }

  return { x: newest.x, y: newest.y, flags: newest.flags, anim: newest.anim };
}
