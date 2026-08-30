/**
 * The vitals bar: health, mana, and experience, along the bottom of the
 * screen.
 *
 * These three numbers were only in the diagnostic readout in the corner,
 * mixed in with tick counts and packet sizes. That panel is for working on
 * the game; this one is for playing it. A player watching their health should
 * not be reading the same block of text as somebody debugging reconciliation.
 *
 * Bars rather than numbers, because the question in a fight is "how much is
 * left", which is a proportion. The numbers are there too, small, for when the
 * question is "how much exactly".
 */

export interface Vitals {
  level: number;
  hp: number;
  hpMax: number;
  mp: number;
  mpMax: number;
  exp: bigint;
  expToNext: bigint;
}

export class VitalsBar {
  #root: HTMLElement;
  #level: HTMLElement;
  #hpFill: HTMLElement;
  #hpText: HTMLElement;
  #mpFill: HTMLElement;
  #mpText: HTMLElement;
  #expFill: HTMLElement;
  #expText: HTMLElement;

  constructor(root: HTMLElement) {
    this.#root = root;
    root.innerHTML =
      '<div class="vitals-level"></div>' +
      '<div class="vitals-bars">' +
      '<div class="vital hp"><div class="vital-fill"></div><span class="vital-text"></span></div>' +
      '<div class="vital mp"><div class="vital-fill"></div><span class="vital-text"></span></div>' +
      '<div class="vital exp"><div class="vital-fill"></div><span class="vital-text"></span></div>' +
      "</div>";

    const fill = (sel: string) => root.querySelector(`${sel} .vital-fill`) as HTMLElement;
    const text = (sel: string) => root.querySelector(`${sel} .vital-text`) as HTMLElement;

    this.#level = root.querySelector(".vitals-level") as HTMLElement;
    this.#hpFill = fill(".hp");
    this.#hpText = text(".hp");
    this.#mpFill = fill(".mp");
    this.#mpText = text(".mp");
    this.#expFill = fill(".exp");
    this.#expText = text(".exp");

    root.hidden = true;
  }

  update(v: Vitals): void {
    this.#root.hidden = false;
    this.#level.textContent = `Lv ${v.level}`;

    this.#set(this.#hpFill, this.#hpText, v.hp, v.hpMax, `${v.hp} / ${v.hpMax}`);
    this.#set(this.#mpFill, this.#mpText, v.mp, v.mpMax, `${v.mp} / ${v.mpMax}`);

    // Experience is the one bar where the proportion is the whole story and
    // the raw number is nearly unreadable, so the percentage is what is shown.
    const need = v.expToNext;
    const pct = need > 0n ? Number((v.exp * 1000n) / need) / 10 : 0;
    this.#set(this.#expFill, this.#expText, pct, 100, need > 0n ? `${pct.toFixed(1)}%` : "max");
  }

  hide(): void {
    this.#root.hidden = true;
  }

  #set(fill: HTMLElement, text: HTMLElement, value: number, max: number, label: string): void {
    const frac = max > 0 ? Math.max(0, Math.min(1, value / max)) : 0;
    fill.style.width = `${(frac * 100).toFixed(2)}%`;
    text.textContent = label;
  }
}
