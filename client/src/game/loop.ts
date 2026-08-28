import { Connection, describeClose } from "@/net/connection";
import type { Event, Snapshot, Welcome } from "@/net/connection";
import { Interpolator } from "./interpolator";
import { Predictor } from "./predictor";
import { InputSource } from "./input";
import { Sim } from "@/sim/wasm";
import { isFacingLeft } from "@/sim/body";
import { toPixels } from "@/sim/fixed";
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

/**
 * How far to look for a drop when the loot key is pressed, in fixed-point.
 *
 * Deliberately wider than the server's pickup range: the client is only
 * choosing which drop the player probably meant, and the server still checks
 * range and ownership. A generous search means one press loots the obvious
 * thing rather than requiring precise positioning.
 */
const LOOT_SEARCH_RADIUS = 120 * 256;

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

  // Progression, carried in the self state each snapshot.
  #level = 1;
  #exp = 0n;
  #expToNext = 0n;
  #hp = 0;
  #hpMax = 0;

  // Bumped whenever the server confirms a kill this player made, so the HUD
  // can show progress without the client inferring it from state.
  #kills = 0;

  constructor(sim: Sim, scene: Scene, cb: LoopCallbacks) {
    this.#sim = sim;
    this.#scene = scene;
    this.#cb = cb;
    this.#predictor = new Predictor(sim);

    this.#conn = new Connection({
      onWelcome: (w) => void this.#onWelcome(w),
      onSnapshot: (s) => this.#onSnapshot(s),
      onEvent: (e) => this.#onEvent(e),
      onPong: () => {},
      onClosed: (code, reason) => this.#onClosed(code, reason),
    });
  }

  async connect(name: string): Promise<void> {
    this.#name = name;

    // Health first, for two reasons.
    //
    // It supplies the content hash for the handshake. And it confirms that
    // whatever is answering on this origin is actually the game server: in
    // development the requests go through the Vite proxy, and if its target
    // port is wrong or occupied by something else, that something else
    // happily answers with its own page. Checking here turns "unexpected
    // token '<'" into a message that names the problem.
    const info = await this.#checkServer();

    const ticket = await this.#requestTicket(name);
    await this.#conn.connect(ticket, info.content ?? "");
  }

  /** Confirms the origin is really the game server, and returns its info. */
  async #checkServer(): Promise<{ protocol?: number; content?: string }> {
    let res: Response;
    try {
      res = await fetch("/healthz");
    } catch {
      throw new Error(
        "Could not reach a server on this address. Is the game server running?",
      );
    }

    if (!res.ok) {
      throw new Error(`Server health check failed: ${res.status}`);
    }

    const body = await res.text();
    let info: { protocol?: number; content?: string };
    try {
      info = JSON.parse(body) as { protocol?: number; content?: string };
    } catch {
      throw new Error(wrongServerMessage(body));
    }

    // Our /healthz always reports a protocol version. Something else serving
    // JSON on this port would not.
    if (typeof info.protocol !== "number") {
      throw new Error(wrongServerMessage(body));
    }
    return info;
  }

  async #requestTicket(name: string): Promise<string> {
    const res = await fetch("/api/dev/ticket", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });

    if (res.status === 404 || res.status === 405) {
      throw new Error(
        "The server is running without development authentication. " +
          "Restart it with --dev-auth.",
      );
    }
    if (!res.ok) {
      throw new Error(`Could not get a ticket: ${res.status} ${await res.text()}`);
    }

    const body = await res.text();
    try {
      const parsed = JSON.parse(body) as { ticket?: string };
      if (!parsed.ticket) throw new Error("no ticket in response");
      return parsed.ticket;
    } catch {
      throw new Error(wrongServerMessage(body));
    }
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
      level: this.#level,
      exp: this.#exp,
      expToNext: this.#expToNext,
      hp: this.#hp,
      hpMax: this.#hpMax,
      kills: this.#kills,
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

      this.#level = snap.self.level || this.#level;
      this.#exp = snap.self.exp;
      this.#expToNext = snap.self.expToNext;
      this.#hp = snap.self.hp;
      this.#hpMax = snap.self.hpMax;
    }
    this.#interp.apply(snap, this.#selfId, performance.now());
  }

  /**
   * Turns server events into feedback.
   *
   * Damage arrives as an event rather than being inferred from a falling
   * health bar, because two hits of 100 and one of 200 are indistinguishable
   * in state and completely different to play.
   */
  #onEvent(e: Event): void {
    switch (e.body.case) {
      case "damage": {
        const d = e.body.value;
        const toSelf = d.targetId === this.#selfId;
        const pos = this.#positionOf(d.targetId);
        if (!pos) break;

        this.#scene.effects.damage(pos.x, pos.y, d.amount, {
          critical: d.critical,
          toSelf,
        });
        break;
      }

      case "died": {
        const d = e.body.value;
        if (d.killerId === this.#selfId && d.entityId !== this.#selfId) {
          this.#kills++;
        }
        break;
      }

      case "levelUp":
        this.#level = e.body.value.level;
        this.#expToNext = e.body.value.expToNext;
        this.#cb.onStatus(`level ${this.#level}`);
        break;

      case "lootTaken": {
        const l = e.body.value;
        this.#cb.onStatus(l.gold > 0 ? `+${l.gold} gold` : `picked up ${l.itemId}`);
        break;
      }
    }
  }

  /**
   * Where to anchor an effect for an entity, in pixels.
   *
   * Self is predicted and everything else is interpolated, so the two come
   * from different places -- using the interpolator for self would put a
   * damage number where the player was 100ms ago.
   */
  #positionOf(entityId: number): { x: number; y: number } | null {
    if (entityId === this.#selfId) {
      const b = this.#predictor.body;
      return { x: toPixels(b.x) + toPixels(b.w) / 2, y: toPixels(b.y) };
    }
    const p = this.#interp.positionOf(entityId);
    return p ? { x: toPixels(p.x) + 16, y: toPixels(p.y) } : null;
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

    this.#scene.drawSelf(
      this.#predictor.body,
      this.#name,
      this.#hp,
      this.#hpMax,
      this.#predictor.authoritative,
    );
    this.#scene.drawEntities(this.#interp.sample(now), this.#interp.drainRemoved());
    this.#scene.effects.update(now);
  }

  /** One simulation tick: sample input, predict, send. */
  #tick(): void {
    const input = this.#input.sample();
    const seq = this.#predictor.predict(input);
    this.#conn.sendIntent(seq, input.moveX, input.jump, input.up, input.down);

    // Attacks are not predicted. The server decides what a swing reached and
    // for how much, and guessing locally would mean showing damage numbers
    // that the server then contradicts -- worse than showing them a frame
    // later.
    if (this.#input.takeAttack()) {
      this.#conn.sendCast(seq, "slash", isFacingLeft(this.#predictor.body));
    }

    if (this.#input.takeLoot()) {
      const b = this.#predictor.body;
      const target = this.#interp.nearestDrop(b.x, b.y, LOOT_SEARCH_RADIUS);
      if (target !== 0) this.#conn.sendLoot(target);
    }

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

/**
 * Explains a response that came from something other than the game server.
 *
 * The common cause in development is the Vite proxy: its target defaults to
 * port 8080, which is one of the most commonly occupied ports on a developer
 * machine. Whatever else is listening there answers with its own page, and the
 * only symptom is a JSON parse error mentioning "<!doctype", which says
 * nothing about the actual problem.
 */
function wrongServerMessage(body: string): string {
  const looksLikeHTML = body.trimStart().startsWith("<");
  return (
    "This address is not the game server" +
    (looksLikeHTML ? " -- it returned an HTML page" : "") +
    ". If you are using the Vite dev server, check that the game server is " +
    "running and that MMO_SERVER points at its port, for example: " +
    "MMO_SERVER=http://localhost:8088 npm run dev"
  );
}
