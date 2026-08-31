import type { Gathering } from "@/net/connection";

/**
 * The gathering line: what the character is doing, or why they are not.
 *
 * Both halves matter equally. An action that resolves on a 600 ms beat with
 * nothing on screen looks like nothing is happening, and a refusal with nothing
 * on screen looks like a bug — "I pressed E at a tree and nothing happened" is
 * the failure a player cannot debug, and it is the one this element exists to
 * make impossible.
 *
 * The refusal text is written by the server, because every reason it can refuse
 * is a rule only the server knows: the level, the tool in hand, the range, the
 * layer. A client composing its own would be a client guessing.
 */

/** How long a refusal stays up before clearing itself. */
const REFUSAL_MS = 3_500;

export class GatheringLine {
  #root: HTMLElement;

  /** When the current refusal should clear, in page time; 0 when active. */
  #clearAt = 0;

  constructor(root: HTMLElement) {
    this.#root = root;
    root.hidden = true;
  }

  update(state: Gathering, skillName: string, now: number): void {
    if (state.active) {
      this.#clearAt = 0;
      this.#root.hidden = false;
      this.#root.classList.remove("refused");
      // Present tense and no ellipsis animation: the beat itself is the
      // feedback, arriving as items and experience every second or so.
      this.#root.textContent = skillName ? `${skillName}…` : "working…";
      return;
    }

    if (!state.reason) {
      // An ending the player does not need explained: they walked away, they
      // let go of the key, or they finished the tree.
      this.hide();
      return;
    }

    this.#root.hidden = false;
    this.#root.classList.add("refused");
    this.#root.textContent = state.reason;
    this.#clearAt = now + REFUSAL_MS;
  }

  /** Called every frame: clears a refusal once it has been read. */
  render(now: number): void {
    if (this.#clearAt !== 0 && now >= this.#clearAt) this.hide();
  }

  hide(): void {
    this.#root.hidden = true;
    this.#root.classList.remove("refused");
    this.#clearAt = 0;
  }
}
