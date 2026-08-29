import type { Downed } from "@/net/connection";

/**
 * The death screen: what killed the run, what it cost, and how long until you
 * are back.
 *
 * It exists because the alternative is a character who stops responding for
 * eight seconds with no explanation. A player who cannot tell "I died" from
 * "the game froze" will conclude the second one every time.
 *
 * It is deliberately not a dialog and has no button. There is nothing to
 * decide -- the server brings the character back on its own clock -- so
 * offering a choice would only invite the player to press something and find
 * that it did nothing.
 */
export class DeathScreen {
  #root: HTMLElement;
  #countdown: HTMLElement;
  #cost: HTMLElement;

  /** When the countdown reaches zero, in page time. */
  #until = 0;

  constructor(root: HTMLElement) {
    this.#root = root;
    root.innerHTML =
      '<div class="death-title">You died</div>' +
      '<div class="death-countdown"></div>' +
      '<div class="death-cost"></div>';

    this.#countdown = root.querySelector(".death-countdown") as HTMLElement;
    this.#cost = root.querySelector(".death-cost") as HTMLElement;
    root.hidden = true;
  }

  /** Shows the screen for a death that has just happened. */
  show(down: Downed, now: number): void {
    this.#until = now + down.reviveInMs;

    const lost = Number(down.expLost);
    this.#cost.textContent = lost > 0 ? `-${lost.toLocaleString()} experience` : "";
    this.#cost.hidden = lost === 0;

    this.#root.hidden = false;
    this.render(now, 0);
  }

  /**
   * Called every frame with the countdown and the character's own health.
   *
   * Health is what closes it. The server sends no "you are back" message,
   * because coming back is state the client already reconciles exactly -- and
   * a second message saying what the snapshot says is the one that goes wrong
   * eventually. The countdown is presentation only; if it reaches zero while
   * health is still zero, the screen waits rather than lying.
   */
  render(now: number, hp: number): void {
    if (this.#root.hidden) return;

    if (hp > 0) {
      this.hide();
      return;
    }

    const left = Math.max(0, this.#until - now);
    this.#countdown.textContent =
      left > 0 ? `back in ${(left / 1000).toFixed(1)}s` : "getting up...";
  }

  hide(): void {
    this.#root.hidden = true;
    this.#until = 0;
  }
}
