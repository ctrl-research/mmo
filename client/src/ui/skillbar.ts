import type { BuffState, SkillBar } from "@/net/connection";

/**
 * The skill bar and the buff bar.
 *
 * Both are permanently on screen and both answer questions that only matter
 * in the moment: is this off cooldown, and how long is that debuff. A panel
 * behind a key would answer them too late.
 *
 * The cooldown sweep is client-side and the server is the authority. That is
 * the right way round: a client that predicts a cooldown and is wrong shows a
 * key greying out a fraction early, which is invisible, while a client that
 * waited for the server to say so would show a bar that lags every press.
 */

export class SkillBarPanel {
  #root: HTMLElement;
  #bar: SkillBar | null = null;

  // When each skill was last cast, so the sweep can be drawn between updates.
  #castAt = new Map<string, number>();

  constructor(root: HTMLElement) {
    this.#root = root;
  }

  update(bar: SkillBar): void {
    this.#bar = bar;
    this.render();
  }

  /** Called when the server confirms this player cast something. */
  cast(skillId: string, now: number): void {
    this.#castAt.set(skillId, now);
  }

  render(): void {
    const bar = this.#bar;
    if (!bar || bar.slots.length === 0) {
      this.#root.hidden = true;
      return;
    }

    this.#root.hidden = false;
    this.#root.innerHTML = bar.slots
      .map(
        (s, i) => `<div class="slot" data-skill="${esc(s.skillId)}">
          <span class="slot-key">${i + 1}</span>
          <span class="slot-name">${esc(s.name)}</span>
          <span class="slot-cost">${s.costMp > 0 ? s.costMp : ""}</span>
          ${
            s.supports.length > 0
              ? `<span class="slot-links">${s.supports.length}</span>`
              : ""
          }
          <div class="slot-sweep"></div>
        </div>`,
      )
      .join("");
  }

  /**
   * Advances the cooldown sweeps.
   *
   * Called every frame rather than on a timer, because the sweep is the only
   * part of this that changes continuously and re-rendering the markup for it
   * would rebuild eight elements sixty times a second.
   */
  tick(now: number): void {
    const bar = this.#bar;
    if (!bar) return;

    const sweeps = this.#root.querySelectorAll<HTMLElement>(".slot-sweep");
    bar.slots.forEach((slot, i) => {
      const sweep = sweeps[i];
      if (!sweep) return;

      const since = now - (this.#castAt.get(slot.skillId) ?? -Infinity);
      const left = slot.cooldownMs - since;

      if (left <= 0) {
        sweep.style.height = "0%";
        return;
      }
      sweep.style.height = `${Math.min(100, (left / slot.cooldownMs) * 100)}%`;
    });
  }
}

export class BuffBar {
  #root: HTMLElement;
  #state: BuffState | null = null;

  // When the state arrived, so the remaining time can be counted down between
  // updates rather than stepping once a second.
  #at = 0;

  constructor(root: HTMLElement) {
    this.#root = root;
  }

  update(state: BuffState, now: number): void {
    this.#state = state;
    this.#at = now;
    this.render(now);
  }

  render(now: number): void {
    const state = this.#state;
    if (!state || state.buffs.length === 0) {
      this.#root.hidden = true;
      this.#root.innerHTML = "";
      return;
    }

    const elapsed = now - this.#at;

    // Helpful first, harmful after: a player scanning the bar for "what is
    // killing me" should find it in the same place every time.
    const sorted = [...state.buffs].sort(
      (a, b) => Number(a.harmful) - Number(b.harmful),
    );

    this.#root.hidden = false;
    this.#root.innerHTML = sorted
      .map((b) => {
        const left = b.remainingMs === 0 ? 0 : Math.max(0, b.remainingMs - elapsed);
        return `<div class="buff${b.harmful ? " harmful" : ""}">
          <span class="buff-name">${esc(b.name)}</span>
          ${b.stacks > 1 ? `<span class="buff-stacks">${b.stacks}</span>` : ""}
          <span class="buff-time">${
            b.remainingMs === 0 ? "" : `${Math.ceil(left / 1000)}s`
          }</span>
        </div>`;
      })
      .join("");
  }
}

function esc(s: string): string {
  return s.replace(
    /[&<>"']/g,
    (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!,
  );
}
