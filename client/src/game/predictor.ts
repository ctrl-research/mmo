import { type Body, cloneBody, copyBody, newBody, pixelDistanceSq } from "@/sim/body";
import type { Input, Sim } from "@/sim/wasm";
import type { EntityState } from "@/gen/mmo/v1/game_pb";

/**
 * Client-side prediction and server reconciliation for the local player.
 *
 * The problem: the server is authoritative, so a naive client would wait a
 * round trip before showing your own movement, which feels terrible at any
 * latency. The fix is to simulate locally the instant a key is pressed, then
 * correct against the server when its answer arrives.
 *
 * The correction is what makes this subtle. On each snapshot we snap to the
 * server's body and replay every input it has not yet acknowledged. Because
 * the replay runs the server's own code (compiled to WebAssembly), the result
 * matches what the server will compute for those same inputs -- so in the
 * common case the correction is exactly zero and nothing visibly moves.
 */

interface PendingInput {
  seq: number;
  input: Input;
}

/**
 * Corrections smaller than this are eased out rather than applied instantly.
 *
 * Both extremes are bad: snapping every sub-pixel disagreement produces
 * constant visible jitter, and easing everything lets real desync accumulate
 * unseen. A rare hard snap reads as lag, which players accept; permanent
 * micro-jitter reads as a broken game.
 */
const SMOOTH_THRESHOLD_PX = 24;

/** How quickly a smoothed correction decays, per tick. */
const SMOOTH_DECAY = 0.72;

export class Predictor {
  #sim: Sim;

  /** The predicted body, ahead of the server by the unacknowledged inputs. */
  #predicted: Body = newBody();

  /** What is actually drawn: predicted plus a decaying correction offset. */
  #rendered: Body = newBody();

  #pending: PendingInput[] = [];
  #seq = 0;

  /** Visual offset in fixed-point, eased toward zero after a correction. */
  #offsetX = 0;
  #offsetY = 0;

  /** Diagnostics, surfaced in the HUD because they explain how it feels. */
  #lastCorrectionPx = 0;
  #hardCorrections = 0;
  #replayDepth = 0;

  constructor(sim: Sim) {
    this.#sim = sim;
  }

  get body(): Readonly<Body> {
    return this.#rendered;
  }

  get authoritative(): Readonly<Body> {
    return this.#predicted;
  }

  get stats() {
    return {
      pending: this.#pending.length,
      lastCorrectionPx: this.#lastCorrectionPx,
      hardCorrections: this.#hardCorrections,
      replayDepth: this.#replayDepth,
    };
  }

  /** Seeds state from the Welcome message. */
  reset(state: EntityState): void {
    applyState(this.#predicted, state);
    this.#sim.settle(this.#predicted);
    copyBody(this.#rendered, this.#predicted);
    this.#pending.length = 0;
    this.#seq = 0;
    this.#offsetX = 0;
    this.#offsetY = 0;
  }

  /**
   * Discards prediction state ahead of a map change.
   *
   * The pending inputs describe movement in a room the character has left, and
   * replaying them against the new map would push them through geometry that
   * does not exist there. Clearing the smoothing offset too means the arrival
   * is a hard snap rather than a visible glide across the new map from
   * wherever they stood in the old one.
   */
  suspend(): void {
    this.#pending.length = 0;
    this.#offsetX = 0;
    this.#offsetY = 0;
    this.#replayDepth = 0;
  }

  /**
   * Simulates one tick locally and returns the sequence number to send.
   *
   * The client ticks at the server's rate rather than at the display rate:
   * one input in, one step out, so the server consumes exactly what was
   * predicted. Rendering interpolates between ticks separately.
   */
  predict(input: Input): number {
    this.#seq++;
    this.#pending.push({ seq: this.#seq, input: { ...input } });

    // Bound the buffer. If it grows this far the connection is gone in all but
    // name, and replaying a huge backlog on the next snapshot would stall a
    // frame badly.
    if (this.#pending.length > 120) this.#pending.shift();

    this.#sim.step(this.#predicted, input);
    this.#decayOffset();
    this.#compose();

    return this.#seq;
  }

  /**
   * Reconciles against authoritative state: snap, then replay what the server
   * has not seen yet.
   */
  reconcile(state: EntityState, ackSeq: number): void {
    const before = cloneBody(this.#predicted);

    applyState(this.#predicted, state);

    // Drop what the server has already applied; replay the rest.
    let i = 0;
    while (i < this.#pending.length && this.#pending[i]!.seq <= ackSeq) i++;
    if (i > 0) this.#pending.splice(0, i);

    this.#replayDepth = this.#pending.length;
    for (const p of this.#pending) {
      this.#sim.step(this.#predicted, p.input);
    }

    // How far the correction moved us. Zero whenever prediction was right,
    // which should be the overwhelmingly common case.
    const errorSq = pixelDistanceSq(before, this.#predicted);
    this.#lastCorrectionPx = Math.sqrt(errorSq);

    if (errorSq === 0) {
      this.#compose();
      return;
    }

    if (this.#lastCorrectionPx < SMOOTH_THRESHOLD_PX) {
      // Carry the discrepancy as a visual offset and ease it out, so a small
      // correction is felt as a slight drift rather than seen as a jump.
      this.#offsetX += before.x - this.#predicted.x;
      this.#offsetY += before.y - this.#predicted.y;
    } else {
      // Too far to hide. Snap, and count it: a rising count means prediction
      // is genuinely diverging and something is wrong.
      this.#offsetX = 0;
      this.#offsetY = 0;
      this.#hardCorrections++;
    }
    this.#compose();
  }

  #decayOffset(): void {
    this.#offsetX = Math.abs(this.#offsetX) < 8 ? 0 : Math.round(this.#offsetX * SMOOTH_DECAY);
    this.#offsetY = Math.abs(this.#offsetY) < 8 ? 0 : Math.round(this.#offsetY * SMOOTH_DECAY);
  }

  #compose(): void {
    copyBody(this.#rendered, this.#predicted);
    this.#rendered.x += this.#offsetX;
    this.#rendered.y += this.#offsetY;
  }
}

/** Copies authoritative wire state into a body. */
export function applyState(b: Body, s: EntityState): void {
  b.x = s.x;
  b.y = s.y;
  b.vx = s.vx;
  b.vy = s.vy;
  b.w = s.w;
  b.h = s.h;
  b.flags = s.flags;
  b.coyote = s.coyote;
  b.jumpBuffer = s.jumpBuffer;
  b.dropThrough = s.dropThrough;
}
