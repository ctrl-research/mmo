import type { Input } from "@/sim/wasm";

/**
 * Keyboard state, sampled once per simulation tick.
 *
 * Held state is read rather than key events queued, because the simulation
 * consumes "what is held this tick" and the server expects exactly one intent
 * per tick. Queuing events would let a fast typist send more input than ticks.
 */
export class InputSource {
  #held = new Set<string>();
  #attached = false;

  attach(): void {
    if (this.#attached) return;
    this.#attached = true;

    window.addEventListener("keydown", this.#onDown);
    window.addEventListener("keyup", this.#onUp);
    // Held keys are stuck down forever if the tab loses focus mid-press, which
    // reads as the character running into a wall on their own.
    window.addEventListener("blur", this.#onBlur);
  }

  detach(): void {
    if (!this.#attached) return;
    this.#attached = false;
    window.removeEventListener("keydown", this.#onDown);
    window.removeEventListener("keyup", this.#onUp);
    window.removeEventListener("blur", this.#onBlur);
    this.#held.clear();
  }

  #onDown = (e: KeyboardEvent) => {
    if (e.repeat) return;
    if (isGameKey(e.code)) {
      // Space and the arrows scroll the page otherwise, which is jarring
      // during play.
      e.preventDefault();
      this.#held.add(e.code);
    }
  };

  #onUp = (e: KeyboardEvent) => {
    this.#held.delete(e.code);
  };

  #onBlur = () => this.#held.clear();

  /** Samples the current intent. */
  sample(): Input {
    const left = this.#has("ArrowLeft", "KeyA");
    const right = this.#has("ArrowRight", "KeyD");

    // Both directions held cancel out, rather than one arbitrarily winning.
    let moveX = 0;
    if (left && !right) moveX = -1000;
    else if (right && !left) moveX = 1000;

    return {
      moveX,
      jump: this.#has("Space", "KeyZ"),
      up: this.#has("ArrowUp", "KeyW"),
      down: this.#has("ArrowDown", "KeyS"),
    };
  }

  #has(...codes: string[]): boolean {
    return codes.some((c) => this.#held.has(c));
  }
}

const GAME_KEYS = new Set([
  "ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown",
  "KeyA", "KeyD", "KeyW", "KeyS",
  "Space", "KeyZ",
]);

function isGameKey(code: string): boolean {
  return GAME_KEYS.has(code);
}
