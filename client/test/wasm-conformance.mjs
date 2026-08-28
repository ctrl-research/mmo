/**
 * Conformance test: the WebAssembly build must match the Go build exactly.
 *
 * This is the check that makes client-side prediction trustworthy. The client
 * predicts by running the simulation compiled to WASM; the server runs the same
 * source compiled for the host. If those two ever diverge -- a compiler
 * difference, an accidental float, a codec mismatch -- prediction drifts and
 * players see rubber-banding whose cause is extremely hard to trace.
 *
 * The golden corpus in internal/world/sim/testdata carries the world, the
 * starting body, the input script, and the expected frame-by-frame output. Here
 * we replay each vector through the real .wasm and assert the frames match tick
 * for tick. A single differing bit fails the build.
 *
 * Run: node test/wasm-conformance.mjs
 */

import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { createRequire } from "node:module";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..");
const fixtureDir = join(repoRoot, "internal", "world", "sim", "testdata");
const wasmPath = join(here, "..", "public", "sim.wasm");
const wasmExecPath = join(here, "..", "public", "wasm_exec.js");

const BODY_SIZE = 28;
const RECT_SIZE = 16;
const WORLD_HEADER = 12 + RECT_SIZE;

const FLAG_GROUNDED = 1 << 0;
const FLAG_CLIMBING = 1 << 1;
const FLAG_FACING_LEFT = 1 << 2;
const FLAG_JUMP_HELD = 1 << 3;

/** Encodes a world exactly as MarshalWorld does in codec.go. */
function encodeWorld(w) {
  const total = w.solids.length + w.platforms.length + w.climbables.length;
  const buf = new Uint8Array(WORLD_HEADER + RECT_SIZE * total);
  const view = new DataView(buf.buffer);

  view.setUint32(0, w.solids.length, true);
  view.setUint32(4, w.platforms.length, true);
  view.setUint32(8, w.climbables.length, true);

  const putRect = (off, r) => {
    view.setInt32(off, r[0], true);
    view.setInt32(off + 4, r[1], true);
    view.setInt32(off + 8, r[2], true);
    view.setInt32(off + 12, r[3], true);
  };

  putRect(12, w.bounds);

  let off = WORLD_HEADER;
  for (const group of [w.solids, w.platforms, w.climbables]) {
    for (const r of group) {
      putRect(off, r);
      off += RECT_SIZE;
    }
  }
  return buf;
}

async function loadSim() {
  // wasm_exec.js is a classic script that expects to install globals. Running
  // it through require gives it a CommonJS context where that works.
  const require = createRequire(import.meta.url);
  const src = readFileSync(wasmExecPath, "utf8");
  const mod = { exports: {} };
  new Function("require", "module", "exports", "process", "globalThis", src)(
    require, mod, mod.exports, process, globalThis,
  );

  const ready = new Promise((resolve) => {
    globalThis.__simReady = resolve;
  });

  const go = new globalThis.Go();
  const { instance } = await WebAssembly.instantiate(readFileSync(wasmPath), go.importObject);
  void go.run(instance);
  await ready;

  const sim = globalThis.__sim;
  if (!sim) throw new Error("the wasm module did not install __sim");
  if (sim.bodySize !== BODY_SIZE) {
    throw new Error(`body layout mismatch: wasm says ${sim.bodySize}, test expects ${BODY_SIZE}`);
  }
  return sim;
}

function runVector(sim, fx) {
  if (!sim.setWorld(encodeWorld(fx.world))) {
    throw new Error("setWorld rejected the fixture's geometry");
  }

  const buf = new Uint8Array(BODY_SIZE);
  const view = new DataView(buf.buffer);

  // Fixtures record the body already settled, matching how a room places a
  // player before its first tick.
  view.setInt32(0, fx.start.x, true);
  view.setInt32(4, fx.start.y, true);
  view.setInt32(16, fx.start.w, true);
  view.setInt32(20, fx.start.h, true);
  sim.settle(buf);

  const out = [];
  for (let i = 0; i < fx.ticks; i++) {
    const [moveX, jump, up, down] = fx.script[i % fx.script.length];
    sim.step(buf, sim.encodeInput(moveX, jump === 1, up === 1, down === 1));

    out.push({
      tick: i,
      x: view.getInt32(0, true),
      y: view.getInt32(4, true),
      vx: view.getInt32(8, true),
      vy: view.getInt32(12, true),
      flags: view.getUint8(24),
    });
  }
  return out;
}

function describeFlags(f) {
  const parts = [];
  if (f & FLAG_GROUNDED) parts.push("grounded");
  if (f & FLAG_CLIMBING) parts.push("climbing");
  if (f & FLAG_FACING_LEFT) parts.push("facingLeft");
  if (f & FLAG_JUMP_HELD) parts.push("jumpHeld");
  return parts.length ? parts.join("|") : "none";
}

const sim = await loadSim();

const files = readdirSync(fixtureDir).filter((f) => f.endsWith(".json")).sort();
if (files.length === 0) {
  console.error(`no fixtures found in ${fixtureDir}`);
  process.exit(1);
}

let failed = 0;
let framesChecked = 0;

for (const file of files) {
  const fx = JSON.parse(readFileSync(join(fixtureDir, file), "utf8"));
  const got = runVector(sim, fx);
  const want = fx.frames;

  let mismatch = null;
  if (got.length !== want.length) {
    mismatch = `produced ${got.length} frames, fixture has ${want.length}`;
  } else {
    for (let i = 0; i < got.length; i++) {
      const g = got[i];
      const w = want[i];
      if (g.x !== w.x || g.y !== w.y || g.vx !== w.vx || g.vy !== w.vy || g.flags !== w.flags) {
        mismatch =
          `tick ${i}\n` +
          `    wasm: x=${g.x} y=${g.y} vx=${g.vx} vy=${g.vy} flags=${describeFlags(g.flags)}\n` +
          `    go:   x=${w.x} y=${w.y} vx=${w.vx} vy=${w.vy} flags=${describeFlags(w.flags)}`;
        break;
      }
      framesChecked++;
    }
  }

  if (mismatch) {
    failed++;
    console.error(`FAIL  ${fx.name}\n  ${mismatch}`);
  } else {
    console.log(`ok    ${fx.name}  (${got.length} ticks)`);
  }
}

console.log();
if (failed > 0) {
  console.error(
    `${failed} of ${files.length} vectors diverged between the Go and WebAssembly builds.\n` +
      `Client prediction cannot be trusted while this fails.`,
  );
  process.exit(1);
}
console.log(`all ${files.length} vectors match the Go build exactly (${framesChecked} frames verified)`);
