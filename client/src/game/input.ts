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

  // Edge-triggered actions, consumed by the simulation tick rather than
  // sampled, so one press is exactly one action.
  #attackPressed = false;
  #lootPressed = false;

  // The skill slot pressed since the last tick, or -1. One per tick rather
  // than a queue: a player who mashes two keys in 50ms meant one of them, and
  // the server would refuse the second for cooldown anyway.
  #slotPressed = -1;

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

    if (ATTACK_KEYS.has(e.code)) this.#attackPressed = true;
    if (LOOT_KEYS.has(e.code)) this.#lootPressed = true;

    const slot = SKILL_KEYS.indexOf(e.code);
    if (slot >= 0) this.#slotPressed = slot;
  };

  #onUp = (e: KeyboardEvent) => {
    this.#held.delete(e.code);
  };

  #onBlur = () => {
    this.#held.clear();
    this.#attackPressed = false;
    this.#lootPressed = false;
    this.#slotPressed = -1;
  };

  /** Consumes the skill slot pressed since the last tick, or -1. */
  takeSlot(): number {
    const slot = this.#slotPressed;
    this.#slotPressed = -1;
    return slot;
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
   * Attack and loot are edge-triggered rather than held.
   *
   * Movement is a state the simulation samples every tick; an attack is an
   * event. Sampling a held attack key would fire once per tick and be
   * throttled by the cooldown anyway, but it would also mean a player who
   * rests a finger on the key attacks forever -- which reads as the game
   * playing itself.
   */
  takeAttack(): boolean {
    const v = this.#attackPressed;
    this.#attackPressed = false;
    return v;
  }

  takeLoot(): boolean {
    const v = this.#lootPressed;
    this.#lootPressed = false;
    return v;
  }

  #has(...codes: string[]): boolean {
    return codes.some((c) => this.#held.has(c));
  }
}

const ATTACK_KEYS = new Set(["KeyX", "ControlLeft", "ControlRight"]);

// The skill bar. Number keys, in order, which is what every game of this shape
// uses and therefore what a player's hands already expect.
const SKILL_KEYS = [
  "Digit1", "Digit2", "Digit3", "Digit4",
  "Digit5", "Digit6", "Digit7", "Digit8",
];
const LOOT_KEYS = new Set(["KeyZ", "KeyC"]);

const GAME_KEYS = new Set([
  "ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown",
  "KeyA", "KeyD", "KeyW", "KeyS",
  "Space",
  ...ATTACK_KEYS,
  ...SKILL_KEYS,
  ...LOOT_KEYS,
]);

function isGameKey(code: string): boolean {
  return GAME_KEYS.has(code);
}
