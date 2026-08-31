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

  // The skill slot most recently pressed and still held, or -1.
  //
  // Most recently pressed rather than lowest: with two keys down the one the
  // player reached for last is the one they meant, and holding a new key
  // should take over from the old rather than be ignored until it is released.
  #slotHeld = -1;

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
    if (!isGameKey(e.code)) return;

    // Space and the arrows scroll the page otherwise, which is jarring during
    // play.
    e.preventDefault();
    this.#held.add(e.code);

    const slot = SKILL_KEYS.indexOf(e.code);
    if (slot >= 0) this.#slotHeld = slot;
  };

  #onUp = (e: KeyboardEvent) => {
    this.#held.delete(e.code);

    // Releasing the key that was driving the bar hands it back to whichever
    // other skill key is still down, if any. Without this, letting go of the
    // newer of two held keys would stop casting entirely while a key was
    // still pressed.
    if (SKILL_KEYS.indexOf(e.code) === this.#slotHeld) {
      this.#slotHeld = -1;
      for (let i = SKILL_KEYS.length - 1; i >= 0; i--) {
        if (this.#held.has(SKILL_KEYS[i]!)) {
          this.#slotHeld = i;
          break;
        }
      }
    }
  };

  #onBlur = () => {
    this.#held.clear();
    this.#slotHeld = -1;
  };

  /** The skill slot currently held, or -1. */
  slotHeld(): number {
    return this.#slotHeld;
  }

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
      jump: this.#has("Space"),
      up: this.#has("ArrowUp", "KeyW"),
      down: this.#has("ArrowDown", "KeyS"),
    };
  }

  /**
   * Attack and loot are held, not edge-triggered.
   *
   * They were events at first, on the reasoning that a player resting a finger
   * on the key would attack forever and the game would be playing itself. In
   * practice the opposite is true: a fight is dozens of swings, and requiring
   * a press for each one is not a decision, it is typing. Holding a key is
   * still the player saying "keep going", and the repeat is paced by the
   * cooldown rather than by how fast they can tap.
   */
  attackHeld(): boolean {
    return this.#has(...ATTACK_KEYS);
  }

  lootHeld(): boolean {
    return this.#has(...LOOT_KEYS);
  }

  /**
   * Gathering is held for the same reason attacking is: an evening of
   * woodcutting is hundreds of swings, and requiring a press for each one is not
   * a decision, it is typing. Releasing the key is what stops the action, which
   * is what makes it read as a commitment rather than a toggle.
   */
  gatherHeld(): boolean {
    return this.#has(...GATHER_KEYS);
  }

  #has(...codes: string[]): boolean {
    return codes.some((c) => this.#held.has(c));
  }
}

const ATTACK_KEYS = ["KeyX", "ControlLeft", "ControlRight"];

// The skill bar. Number keys, in order, which is what every game of this shape
// uses and therefore what a player's hands already expect.
const SKILL_KEYS = [
  "Digit1", "Digit2", "Digit3", "Digit4",
  "Digit5", "Digit6", "Digit7", "Digit8",
];
const LOOT_KEYS = ["KeyZ", "KeyC"];

// Gathering. E for "interact", which is where a hand already goes, and F for
// anyone whose left hand is on WASD with a thumb to spare.
const GATHER_KEYS = ["KeyE", "KeyF"];

const GAME_KEYS = new Set([
  "ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown",
  "KeyA", "KeyD", "KeyW", "KeyS",
  "Space",
  ...ATTACK_KEYS,
  ...SKILL_KEYS,
  ...LOOT_KEYS,
  ...GATHER_KEYS,
]);

function isGameKey(code: string): boolean {
  return GAME_KEYS.has(code);
}
