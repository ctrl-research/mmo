import type { BossPhase } from "@/net/connection";

/**
 * The boss frame: who you are fighting, what it is doing, and how much of it
 * is left.
 *
 * A boss bar is not a nicer version of the health bar over its head. That one
 * answers "how much longer"; this one answers "what is happening now", which
 * is the question a phase change asks and the one a party has to answer
 * together. Phases and enrages arrive as their own events for exactly that
 * reason, and this is where they land.
 */

/**
 * How long the frame stays after the boss stops updating.
 *
 * A fight has pauses -- the party runs, the boss walks back -- and a frame that
 * vanished during them would flicker. Long enough to cover a wipe recovery,
 * short enough that leaving the room clears it.
 */
const LINGER_MS = 30_000;

/** How long a phase announcement stays lit. */
const ANNOUNCE_MS = 4_000;

export class BossFrame {
  #root: HTMLElement;
  #name: HTMLElement;
  #phase: HTMLElement;
  #fill: HTMLElement;
  #text: HTMLElement;

  #entityId = 0;
  #lastSeen = 0;
  #announcedAt = 0;

  constructor(root: HTMLElement) {
    this.#root = root;
    root.innerHTML =
      '<div class="boss-head">' +
      '<span class="boss-name"></span><span class="boss-phase"></span>' +
      "</div>" +
      '<div class="boss-bar"><div class="boss-fill"></div>' +
      '<span class="boss-text"></span></div>';

    this.#name = root.querySelector(".boss-name") as HTMLElement;
    this.#phase = root.querySelector(".boss-phase") as HTMLElement;
    this.#fill = root.querySelector(".boss-fill") as HTMLElement;
    this.#text = root.querySelector(".boss-text") as HTMLElement;
    root.hidden = true;
  }

  /** The entity the frame is currently following, or 0. */
  get entityId(): number {
    return this.#entityId;
  }

  /** A phase change, an enrage, or the first sight of an encounter. */
  announce(ev: BossPhase, now: number): void {
    this.#entityId = ev.entityId;
    this.#lastSeen = now;
    this.#announcedAt = now;

    this.#name.textContent = ev.name;
    this.#phase.textContent = ev.phase;
    this.#root.classList.toggle("enraged", ev.phase === "enraged");
    this.#root.hidden = false;

    this.#setHealth(ev.hp, ev.hpMax);
  }

  /**
   * Health, from the snapshot rather than from the event.
   *
   * The bar has to move continuously, and phase events arrive three times in a
   * fight. Following the entity is what makes the frame a health bar rather
   * than a notification that happens to have a number in it.
   */
  track(hp: number, hpMax: number, now: number): void {
    if (this.#root.hidden) return;

    // Dead, or gone from view. Either way the fight is over and the frame
    // should go with it rather than sitting there at zero.
    if (hp === 0 || hpMax === 0) {
      this.hide();
      return;
    }

    this.#lastSeen = now;
    this.#setHealth(hp, hpMax);
  }

  /** Called every frame: fades the phase text and retires a stale frame. */
  render(now: number): void {
    if (this.#root.hidden) return;

    if (now - this.#lastSeen > LINGER_MS) {
      this.hide();
      return;
    }
    this.#phase.classList.toggle("fresh", now - this.#announcedAt < ANNOUNCE_MS);
  }

  /** The boss died, or the character left the room it was in. */
  hide(): void {
    this.#root.hidden = true;
    this.#root.classList.remove("enraged");
    this.#entityId = 0;
  }

  #setHealth(hp: number, hpMax: number): void {
    const frac = hpMax > 0 ? Math.max(0, Math.min(1, hp / hpMax)) : 0;
    this.#fill.style.width = `${(frac * 100).toFixed(1)}%`;
    this.#text.textContent = `${hp.toLocaleString()} / ${hpMax.toLocaleString()}`;
  }
}
