/**
 * In-game prompts: a question, and optionally a line to type an answer on.
 *
 * Not window.confirm or window.prompt. A native modal blocks the event loop,
 * which stops rendering and stops the simulation stepping -- so a party
 * invitation arriving mid-fight would freeze the character in place until
 * somebody answered it, while the mobs kept hitting them on the server.
 *
 * It also takes the keyboard while it is open, for the same reason the chat
 * line does: everything typed into it is an answer, not movement.
 */

export interface PromptCallbacks {
  /** Called when the prompt takes or releases the keyboard. */
  onFocusChange(focused: boolean): void;
}

interface Pending {
  resolve(value: string | null): void;
}

export class Prompt {
  #root: HTMLElement;
  #cb: PromptCallbacks;
  #pending: Pending | null = null;

  constructor(root: HTMLElement, cb: PromptCallbacks) {
    this.#root = root;
    this.#cb = cb;
  }

  get isOpen(): boolean {
    return this.#pending !== null;
  }

  /** Asks a yes/no question. Resolves to true or false. */
  async confirm(question: string): Promise<boolean> {
    return (await this.#open(question, false)) !== null;
  }

  /** Asks for a line of text. Resolves to null if cancelled. */
  async ask(question: string): Promise<string | null> {
    return this.#open(question, true);
  }

  /** Closes without an answer, for a prompt that has stopped mattering. */
  cancel(): void {
    this.#finish(null);
  }

  #open(question: string, withInput: boolean): Promise<string | null> {
    // One at a time. A queue of questions nobody answered is worse than
    // losing the older one, and the older one is the stale one.
    this.#finish(null);

    this.#root.hidden = false;
    this.#root.innerHTML = `
      <div class="prompt-question">${esc(question)}</div>
      ${withInput ? `<input class="prompt-input" type="text" maxlength="500" />` : ""}
      <div class="prompt-actions">
        <button class="prompt-ok">OK</button>
        <button class="prompt-cancel">Cancel</button>
      </div>`;

    const input = this.#root.querySelector<HTMLInputElement>(".prompt-input");
    const answer = () => this.#finish(input ? input.value.trim() : "");

    this.#root.querySelector(".prompt-ok")!.addEventListener("click", answer);
    this.#root.querySelector(".prompt-cancel")!.addEventListener("click", () =>
      this.#finish(null),
    );

    this.#root.onkeydown = (e) => {
      // Swallowed, or the answer is also played into the world.
      e.stopPropagation();
      if (e.key === "Enter") answer();
      if (e.key === "Escape") this.#finish(null);
    };

    this.#cb.onFocusChange(true);
    (input ?? this.#root.querySelector<HTMLElement>(".prompt-ok"))?.focus();

    return new Promise((resolve) => {
      this.#pending = { resolve };
    });
  }

  #finish(value: string | null): void {
    const pending = this.#pending;
    this.#pending = null;

    this.#root.hidden = true;
    this.#root.innerHTML = "";
    this.#root.onkeydown = null;

    if (pending) {
      this.#cb.onFocusChange(false);
      pending.resolve(value);
    }
  }
}

function esc(s: string): string {
  return s.replace(
    /[&<>"']/g,
    (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!,
  );
}
