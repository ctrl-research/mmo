import type { AnimName } from "./sprites";
import { isClimbing, isGrounded, type Body } from "@/sim/body";

/**
 * Which animation a body is in, and which frame of it.
 *
 * Derived from simulation state rather than tracked separately: the body
 * already knows whether it is grounded, climbing, and how fast it is moving,
 * and a second source of truth for "is this character running" is a second
 * thing that can be wrong. The only state kept here is the frame clock, which
 * is presentation and nothing else.
 *
 * The one exception is attacking, which the simulation does not model as a
 * body state -- a cast is resolved and gone within a tick. The client is told
 * a cast happened and holds the animation for a fixed time, because an attack
 * that renders for one frame is an attack nobody sees.
 */

/** How long a swing is held, in milliseconds. Two frames over 240ms. */
const ATTACK_MS = 240;

/** Milliseconds per frame, per animation. */
const FRAME_MS: Record<AnimName, number> = {
  idle: 500,
  // A four-frame cycle at 110ms is roughly two steps a second, which is what
  // the run speed works out to. A cycle that does not match the speed reads as
  // skating, and it is the thing people notice without knowing why.
  run: 110,
  jump: 1000,
  fall: 1000,
  climb: 180,
  attack: ATTACK_MS / 2,
};

export class Animator {
  #attackUntil = 0;

  /** Called when the server confirms this body cast something. */
  attack(now: number): void {
    this.#attackUntil = now + ATTACK_MS;
  }

  /**
   * Picks the animation and frame for a body.
   *
   * The order is a priority list: attacking beats climbing beats airborne
   * beats moving. A character who swings while falling should read as
   * swinging, because that is the part with a consequence.
   */
  frame(body: Body, now: number): { anim: AnimName; frame: number } {
    const anim = this.#animFor(body, now);

    // The clock drives the frame rather than a counter, so an animation runs
    // at the same speed whatever the display refresh is -- and so a character
    // that stops and starts does not resume mid-stride.
    const frame = Math.floor(now / FRAME_MS[anim]);
    return { anim, frame };
  }

  #animFor(body: Body, now: number): AnimName {
    if (now < this.#attackUntil) return "attack";
    if (isClimbing(body)) return "climb";

    if (!isGrounded(body)) {
      return body.vy < 0 ? "jump" : "fall";
    }
    // A threshold rather than "not zero": friction leaves a body drifting for
    // a few ticks after the key is released, and a run cycle that plays during
    // the slide reads as the controls not having registered.
    return Math.abs(body.vx) > 64 ? "run" : "idle";
  }
}
