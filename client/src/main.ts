import { GameLoop } from "@/game/loop";
import { Scene } from "@/render/scene";
import { Sim } from "@/sim/wasm";
import { toPixels } from "@/sim/fixed";
import { isClimbing, isGrounded } from "@/sim/body";

/**
 * Entry point: load the simulation, build the scene, connect, run.
 *
 * The HUD is not decoration. This milestone exists to answer "does movement
 * feel right and does prediction hold", and neither question can be answered
 * without seeing correction size, replay depth, and round-trip time while
 * playing.
 */

const overlay = document.getElementById("overlay") as HTMLDivElement;
const nameInput = document.getElementById("name") as HTMLInputElement;
const connectBtn = document.getElementById("connect") as HTMLButtonElement;
const errorEl = document.getElementById("error") as HTMLDivElement;
const hud = document.getElementById("hud") as HTMLDivElement;
const stage = document.getElementById("stage") as HTMLDivElement;

let loop: GameLoop | null = null;

async function main() {
  nameInput.value = localStorage.getItem("mmo.name") ?? "";
  nameInput.focus();

  connectBtn.addEventListener("click", () => void start());
  nameInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter") void start();
  });

  // G toggles the authoritative-position ghost. If it separates from the drawn
  // body, reconciliation is wrong, and seeing that live beats inferring it
  // from a log.
  window.addEventListener("keydown", (e) => {
    if (e.code === "KeyG" && loop) {
      const on = loop.toggleGhost();
      setStatus(on ? "server ghost on" : "server ghost off");
    }
  });
}

async function start() {
  const name = nameInput.value.trim();
  if (!name) {
    errorEl.textContent = "Enter a name.";
    return;
  }

  connectBtn.disabled = true;
  errorEl.textContent = "";
  localStorage.setItem("mmo.name", name);

  try {
    setStatus("loading simulation...");
    const sim = await Sim.load();

    setStatus("building scene...");
    const scene = await Scene.create(stage);

    loop = new GameLoop(sim, scene, {
      onStatus: setStatus,
      onDisconnect: (reason) => {
        overlay.hidden = false;
        connectBtn.disabled = false;
        errorEl.textContent = reason;
        loop = null;
      },
    });

    setStatus("connecting...");
    await loop.connect(name);

    overlay.hidden = true;
    startHud();
  } catch (err) {
    errorEl.textContent = err instanceof Error ? err.message : String(err);
    connectBtn.disabled = false;
    loop = null;
  }
}

let status = "";
function setStatus(text: string) {
  status = text;
}

function startHud() {
  const render = () => {
    if (loop) {
      const s = loop.stats;
      const b = s.body;
      const state = isClimbing(b) ? "climbing" : isGrounded(b) ? "grounded" : "airborne";

      hud.innerHTML =
        `<b>${status}</b>\n` +
        `pos      ${toPixels(b.x).toFixed(1)}, ${toPixels(b.y).toFixed(1)}  (${state})\n` +
        `vel      ${toPixels(b.vx).toFixed(2)}, ${toPixels(b.vy).toFixed(2)}\n` +
        `rtt      ${s.rttMs} ms\n` +
        `fps      ${s.fps.toFixed(0)}\n` +
        `tick     server ${s.serverTick}  local ${s.ticksSimulated}\n` +
        `predict  ${s.pending} pending, replayed ${s.replayDepth}\n` +
        `correct  ${s.lastCorrectionPx.toFixed(2)} px  (${s.hardCorrections} hard)\n` +
        `others   ${s.entities}\n` +
        `net      ${s.snapshotsReceived} snaps, ${(s.bytesReceived / 1024).toFixed(1)} KiB\n` +
        `\n[g] toggle server ghost`;
    }
    requestAnimationFrame(render);
  };
  requestAnimationFrame(render);
}

void main();
