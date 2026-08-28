import { BODY_SIZE, type Body, decodeBody, encodeBody } from "./body";

/**
 * Bridge to the simulation compiled to WebAssembly.
 *
 * The client does not reimplement the physics. It runs the server's own
 * compiled code, so a predicted position and the authoritative one agree bit
 * for bit given the same inputs. Two hand-maintained implementations would
 * drift, and that drift is what rubber-banding is made of.
 */

interface SimExports {
  bodySize: number;
  tickRate: number;
  fracBits: number;
  setWorld(bytes: Uint8Array): boolean;
  step(body: Uint8Array, encodedInput: number): boolean;
  settle(body: Uint8Array): boolean;
  tuning(): { runSpeed: number; jumpVel: number; gravity: number; terminalVel: number };
  encodeInput(moveX: number, jump: boolean, up: boolean, down: boolean): number;
}

declare global {
  // Installed by wasm_exec.js and by the Go module respectively.
  // eslint-disable-next-line no-var
  var Go: new () => { importObject: WebAssembly.Imports; run(i: WebAssembly.Instance): void };
  // eslint-disable-next-line no-var
  var __sim: SimExports | undefined;
  // eslint-disable-next-line no-var
  var __simReady: (() => void) | undefined;
}

export interface Input {
  moveX: number;
  jump: boolean;
  up: boolean;
  down: boolean;
}

export const NO_INPUT: Input = { moveX: 0, jump: false, up: false, down: false };

export class Sim {
  readonly tickRate: number;
  readonly tickMs: number;

  #api: SimExports;
  #buffer: Uint8Array;
  #view: DataView;

  private constructor(api: SimExports) {
    this.#api = api;
    this.tickRate = api.tickRate;
    this.tickMs = 1000 / api.tickRate;

    // One reusable buffer. Step runs many times per frame during
    // reconciliation, so allocating per call would create real GC pressure on
    // the hot path.
    this.#buffer = new Uint8Array(BODY_SIZE);
    this.#view = new DataView(this.#buffer.buffer);
  }

  /** Loads and starts the WebAssembly module. */
  static async load(wasmUrl = "/sim.wasm"): Promise<Sim> {
    await loadScript("/wasm_exec.js");

    const ready = new Promise<void>((resolve) => {
      globalThis.__simReady = resolve;
    });

    const go = new globalThis.Go();
    const { instance } = await WebAssembly.instantiateStreaming(fetch(wasmUrl), go.importObject);

    // go.run resolves only when the module exits, and the module blocks
    // forever so its exports stay callable. Awaiting it would hang.
    void go.run(instance);
    await ready;

    const api = globalThis.__sim;
    if (!api) throw new Error("sim: WebAssembly module did not install its exports");

    // A size mismatch means the Go and TypeScript encoders disagree about the
    // byte layout. Failing here beats silently reading misaligned fields and
    // wondering why prediction drifts.
    if (api.bodySize !== BODY_SIZE) {
      throw new Error(
        `sim: body layout mismatch (wasm says ${api.bodySize} bytes, client expects ${BODY_SIZE}); ` +
          `rebuild the client and the wasm module together`,
      );
    }

    return new Sim(api);
  }

  /**
   * Uploads collision geometry, which the server encodes with the same
   * function the simulation uses. The client never derives collision from the
   * map file itself, so there is no second implementation to drift.
   */
  setWorld(bytes: Uint8Array): void {
    if (!this.#api.setWorld(bytes)) {
      throw new Error("sim: server sent collision geometry this client cannot decode");
    }
  }

  /** Advances a body by one tick, in place. */
  step(body: Body, input: Input): void {
    encodeBody(this.#view, body);
    const encoded = this.#api.encodeInput(input.moveX, input.jump, input.up, input.down);
    this.#api.step(this.#buffer, encoded);
    decodeBody(this.#view, body);
  }

  /**
   * Computes contact state for a freshly placed body without advancing time,
   * so a jump pressed on the first tick after spawning is not swallowed.
   */
  settle(body: Body): void {
    encodeBody(this.#view, body);
    this.#api.settle(this.#buffer);
    decodeBody(this.#view, body);
  }

  tuning() {
    return this.#api.tuning();
  }
}

function loadScript(src: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const el = document.createElement("script");
    el.src = src;
    el.onload = () => resolve();
    el.onerror = () => reject(new Error(`failed to load ${src}`));
    document.head.appendChild(el);
  });
}
