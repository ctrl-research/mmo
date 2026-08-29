import { GameLoop } from "@/game/loop";
import { Scene } from "@/render/scene";
import { Sim } from "@/sim/wasm";
import { Shell } from "@/ui/screens";
import type { Character } from "@/ui/api";
import { toPixels } from "@/sim/fixed";
import { isClimbing, isGrounded } from "@/sim/body";

/**
 * Entry point.
 *
 * The shell handles everything before the world -- signing in and choosing a
 * character -- and hands over a single-use ticket. Only then is the simulation
 * loaded and a socket opened.
 *
 * The HUD is not decoration. Correction size, replay depth, and round-trip
 * time are the only way to tell whether prediction is holding while actually
 * playing, which is what the milestone exit criteria turn on.
 */

const overlay = document.getElementById("overlay") as HTMLDivElement;
const hud = document.getElementById("hud") as HTMLDivElement;
const stage = document.getElementById("stage") as HTMLDivElement;

let loop: GameLoop | null = null;
let scene: Scene | null = null;
let sim: Sim | null = null;
let status = "";

const shell = new Shell(overlay, {
  onPlay: (ticket, character, contentHash) => void enterWorld(ticket, character, contentHash),
});

async function main(): Promise<void> {
  // G overlays the server's authoritative position. If it separates from the
  // drawn body, reconciliation is wrong, and seeing that live beats inferring
  // it from a log.
  window.addEventListener("keydown", (e) => {
    if (e.code === "KeyG" && loop) {
      setStatus(loop.toggleGhost() ? "server ghost on" : "server ghost off");
    }
  });

  await shell.start();
  startHud();
}

async function enterWorld(ticket: string, character: Character, contentHash: string): Promise<void> {
  try {
    setStatus("loading simulation...");

    // Loaded once and reused: the WebAssembly module is a couple of megabytes,
    // and re-instantiating it on every character switch would stall for no
    // reason.
    sim ??= await Sim.load();
    scene ??= await Scene.create(stage);

    scene.clearEntities();

    loop = new GameLoop(sim, scene, {
      onStatus: setStatus,
      onDisconnect: (reason) => {
        loop = null;
        void shell.resume(reason);
      },
    });

    setStatus(`connecting as ${character.name}...`);
    await loop.connect(character.name, ticket, contentHash);
    setStatus(`playing as ${character.name}`);
  } catch (err) {
    loop = null;
    void shell.resume(err instanceof Error ? err.message : String(err));
  }
}

function setStatus(text: string): void {
  status = text;
}

function startHud(): void {
  const render = () => {
    if (loop) {
      const s = loop.stats;
      const b = s.body;
      const state = isClimbing(b) ? "climbing" : isGrounded(b) ? "grounded" : "airborne";
      const expPct = s.expToNext > 0n ? Number((s.exp * 100n) / s.expToNext).toFixed(0) : "--";

      hud.innerHTML =
        `<b>${status}</b>\n` +
        `level    ${s.level}   exp ${s.exp}/${s.expToNext} (${expPct}%)\n` +
        `hp       ${s.hp}/${s.hpMax}   kills ${s.kills}\n` +
        `pos      ${toPixels(b.x).toFixed(1)}, ${toPixels(b.y).toFixed(1)}  (${state})\n` +
        `vel      ${toPixels(b.vx).toFixed(2)}, ${toPixels(b.vy).toFixed(2)}\n` +
        `rtt      ${s.rttMs} ms   fps ${s.fps.toFixed(0)}\n` +
        `tick     server ${s.serverTick}  local ${s.ticksSimulated}\n` +
        `predict  ${s.pending} pending, replayed ${s.replayDepth}\n` +
        `correct  ${s.lastCorrectionPx.toFixed(2)} px  (${s.hardCorrections} hard)\n` +
        `others   ${s.entities}\n` +
        `net      ${s.snapshotsReceived} snaps, ${(s.bytesReceived / 1024).toFixed(1)} KiB\n` +
        `\n[g] toggle server ghost`;
      hud.hidden = false;
    } else {
      hud.hidden = true;
    }
    requestAnimationFrame(render);
  };
  requestAnimationFrame(render);
}

void main();
