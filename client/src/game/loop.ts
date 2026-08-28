import { Connection, describeClose } from "@/net/connection";
import type { Event, Snapshot, Welcome } from "@/net/connection";
import { Interpolator } from "./interpolator";
import { Predictor } from "./predictor";
import { InputSource } from "./input";
import { Sim } from "@/sim/wasm";
import type { Scene, MapGeometry } from "@/render/scene";
import { decodeWorld } from "@/net/collision";

/**
 * The game loop.
 *
 * Simulation runs on a fixed 20 Hz step, matching the server exactly, while
 * rendering runs at the display refresh rate. Keeping those separate is what
 * makes prediction sound: one predicted step corresponds to one input, which
 * corresponds to one server tick. A simulation driven by frame time would
 * produce a different number of steps on a 60 Hz and a 144 Hz display, and the
 * server would consume a different number of inputs than the client predicted.
 */

/** Bound on catch-up steps after the tab is backgrounded and resumes. */
const MAX_CATCHUP_TICKS = 5;

export interface LoopCallbacks {
  onStatus(text: string): void;
  onDisconnect(reason: string): void;
}

export class GameLoop {
  #sim: Sim;
  #scene: Scene;
  #conn: Connection;
  #input = new InputSource();
  #predictor: Predictor;
  #interp = new Interpolator();
  #cb: LoopCallbacks;

  #selfId = 0;
  #name = "";
  #running = false;

  #accumulator = 0;
  #lastFrame = 0;

  #serverTick = 0n;
  #ticksSimulated = 0;
  #fps = 0;

  constructor(sim: Sim, scene: Scene, cb: LoopCallbacks) {
    this.#sim = sim;
    this.#scene = scene;
    this.#cb = cb;
    this.#predictor = new Predictor(sim);

    this.#conn = new Connection({
      onWelcome: (w) => this.#onWelcome(w),
      onSnapshot: (s) => this.#onSnapshot(s),
      onEvent: (e) => this.#onEvent(e),
      onPong: () => {},
      onClosed: (code, reason) => this.#onClosed(code, reason),
    });
  }

  async connect(name: string): Promise<void> {
    this.#name = name;

    const res = await fetch("/api/dev/ticket", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    if (!res.ok) {
      throw new Error(`could not get a ticket: ${res.status} ${await res.text()}`);
    }
    const { ticket } = (await res.json()) as { ticket: string };

    await this.#conn.connect(ticket);
  }

  stop(): void {
    this.#running = false;
    this.#input.detach();
    this.#conn.close();
    this.#interp.clear();
    this.#scene.clearEntities();
  }

  get stats() {
    return {
      ...this.#conn.stats,
      ...this.#predictor.stats,
      serverTick: this.#serverTick,
      ticksSimulated: this.#ticksSimulated,
      entities: this.#interp.size,
      fps: this.#fps,
      body: this.#predictor.body,
    };
  }

  async #onWelcome(w: Welcome): Promise<void> {
    this.#selfId = w.entityId;
    this.#serverTick = w.tick;

    // Collision geometry comes from the server, encoded by the same function
    // the simulation uses, so the client cannot be predicting against
    // different geometry than the server is enforcing.
    const collision = await fetch(`/api/map/${w.mapId}/collision`);
    if (!collision.ok) throw new Error(`could not load collision for map ${w.mapId}`);
    const bytes = new Uint8Array(await collision.arrayBuffer());

    this.#sim.setWorld(bytes);
    this.#scene.setMap(toGeometry(decodeWorld(bytes)));

    if (w.self) this.#predictor.reset(w.self);

    this.#input.attach();
    this.#running = true;
    this.#lastFrame = performance.now();
    this.#scene.ticker.add(() => this.#frame());

    this.#cb.onStatus(`connected as ${this.#name}`);
  }

  #onSnapshot(snap: Snapshot): void {
    this.#serverTick = snap.tick;

    if (snap.self) {
      this.#predictor.reconcile(snap.self, snap.ackSeq);
    }
    this.#interp.apply(snap, this.#selfId, performance.now());
  }

  #onEvent(_e: Event): void {
    // M0 carries only join and left events, and both are already visible
    // through entities entering and being removed. Chat and combat events
    // arrive with the systems that produce them.
  }

  #onClosed(code: number, reason: string): void {
    this.#running = false;
    this.#input.detach();
    this.#cb.onDisconnect(reason || describeClose(code));
  }

  /**
   * One rendered frame: advance the simulation by whole ticks, then draw.
   */
  #frame(): void {
    if (!this.#running) return;

    const now = performance.now();
    const elapsed = now - this.#lastFrame;
    this.#lastFrame = now;

    this.#fps = this.#fps * 0.9 + (1000 / Math.max(elapsed, 1)) * 0.1;

    this.#accumulator += elapsed;

    // A backgrounded tab wakes with a huge elapsed time. Simulating every
    // missed tick would freeze the frame and flood the server with intent, so
    // catch-up is bounded and the rest of the gap is simply forfeited -- the
    // next snapshot corrects the position anyway.
    let steps = 0;
    while (this.#accumulator >= this.#sim.tickMs && steps < MAX_CATCHUP_TICKS) {
      this.#accumulator -= this.#sim.tickMs;
      this.#tick();
      steps++;
    }
    if (steps === MAX_CATCHUP_TICKS) this.#accumulator = 0;

    this.#scene.drawSelf(this.#predictor.body, this.#name, this.#predictor.authoritative);
    this.#scene.drawEntities(this.#interp.sample(now), this.#interp.drainRemoved());
  }

  /** One simulation tick: sample input, predict, send. */
  #tick(): void {
    const input = this.#input.sample();
    const seq = this.#predictor.predict(input);
    this.#conn.sendIntent(seq, input.moveX, input.jump, input.up, input.down);
    this.#ticksSimulated++;
  }

  toggleGhost(): boolean {
    return this.#scene.toggleGhost();
  }
}

function toGeometry(w: ReturnType<typeof decodeWorld>): MapGeometry {
  const px = (v: number) => v / 256;
  const rects = (list: typeof w.solids) =>
    list.map((r) => ({ x: px(r.x), y: px(r.y), w: px(r.w), h: px(r.h) }));

  return {
    width: px(w.bounds.w),
    height: px(w.bounds.h),
    solids: rects(w.solids),
    platforms: rects(w.platforms),
    climbables: rects(w.climbables),
  };
}
