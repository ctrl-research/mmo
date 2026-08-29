import { GameLoop } from "@/game/loop";
import { Scene } from "@/render/scene";
import { Sim } from "@/sim/wasm";
import { Shell } from "@/ui/screens";
import { InventoryPanel } from "@/ui/inventory";
import { WorldMapPanel } from "@/ui/worldmap";
import { ChatPanel } from "@/ui/chat";
import { PartyFrames, SocialPanel } from "@/ui/social";
import { Prompt } from "@/ui/prompt";
import { SkillBarPanel, BuffBar } from "@/ui/skillbar";
import { PassivePanel } from "@/ui/passives";
import { BossFrame } from "@/ui/boss";
import { DeathScreen } from "@/ui/death";
import { PartyAction_Kind, GuildAction_Kind } from "@/net/connection";
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
const inventoryEl = document.getElementById("inventory") as HTMLDivElement;
const tooltipEl = document.getElementById("tooltip") as HTMLDivElement;
const worldMapEl = document.getElementById("worldmap") as HTMLDivElement;
const chatEl = document.getElementById("chat") as HTMLDivElement;
const partyEl = document.getElementById("party") as HTMLDivElement;
const socialEl = document.getElementById("social") as HTMLDivElement;
const promptEl = document.getElementById("prompt") as HTMLDivElement;
const skillBarEl = document.getElementById("skillbar") as HTMLDivElement;
const buffBarEl = document.getElementById("buffbar") as HTMLDivElement;
const passivesEl = document.getElementById("passives") as HTMLDivElement;
const bossEl = document.getElementById("boss") as HTMLDivElement;
const deathEl = document.getElementById("death") as HTMLDivElement;

let loop: GameLoop | null = null;
let scene: Scene | null = null;
let sim: Sim | null = null;
let panel: InventoryPanel | null = null;
let worldMap: WorldMapPanel | null = null;
let chat: ChatPanel | null = null;
let party: PartyFrames | null = null;
let social: SocialPanel | null = null;
let prompt: Prompt | null = null;
let skillBar: SkillBarPanel | null = null;
let buffBar: BuffBar | null = null;
let passives: PassivePanel | null = null;
let boss: BossFrame | null = null;
let death: DeathScreen | null = null;
let status = "";

const shell = new Shell(overlay, {
  onPlay: (ticket, character, contentHash) => void enterWorld(ticket, character, contentHash),
});

async function main(): Promise<void> {
  // G overlays the server's authoritative position. If it separates from the
  // drawn body, reconciliation is wrong, and seeing that live beats inferring
  // it from a log.
  window.addEventListener("keydown", (e) => {
    if (!loop) return;

    // Enter opens the chat line. While it holds the keyboard nothing else
    // does: every other shortcut here is a letter, and a player writing a
    // sentence would otherwise open four panels doing it.
    if (chat?.focused || prompt?.isOpen) return;

    if (e.code === "Enter") {
      e.preventDefault();
      chat?.focus();
      return;
    }

    if (e.code === "KeyG") {
      setStatus(loop.toggleGhost() ? "server ghost on" : "server ghost off");
    }
    if (e.code === "KeyI") {
      // Prevented, or the keypress also reaches the game and the character
      // acts on the same press that opened the panel.
      e.preventDefault();
      worldMap?.close();
      panel?.toggle();
    }
    if (e.code === "KeyM") {
      e.preventDefault();
      panel?.close();
      social?.close();
      worldMap?.toggle();
    }
    if (e.code === "KeyO") {
      e.preventDefault();
      panel?.close();
      worldMap?.close();
      passives?.close();
      social?.toggle();
    }
    if (e.code === "KeyP") {
      e.preventDefault();
      panel?.close();
      worldMap?.close();
      social?.close();
      void passives?.toggle();
    }
    if (e.code === "Escape") {
      panel?.close();
      worldMap?.close();
      social?.close();
      passives?.close();
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

    const game = new GameLoop(sim, scene, {
      onStatus: setStatus,
      onInventory: () => panel?.render(),
      onWorldMap: (m) => worldMap?.update(m),
      onChat: (line) => chat?.add(line),
      onSkillBar: (bar) => skillBar?.update(bar),
      onBuffs: (buffs) => buffBar?.update(buffs, performance.now()),
      onCast: (skillId) => skillBar?.cast(skillId, performance.now()),
      onPassives: (state) => passives?.update(state),
      onSystem: (msg) => chat?.addSystem(msg),
      onParty: (state) => party?.update(state),
      onDowned: (down) => death?.show(down, performance.now()),
      onBossPhase: (phase) => boss?.announce(phase, performance.now()),
      onBossHealth: (hp, hpMax) => boss?.track(hp, hpMax, performance.now()),
      bossEntityId: () => boss?.entityId ?? 0,
      onGuild: (state) => social?.updateGuild(state),
      onFriends: (list) => social?.updateFriends(list),

      // An invitation is a question asked in the moment, so it is asked in the
      // moment: a prompt that interrupts, not a badge to notice later.
      onPartyInvite: (invite) => {
        chat?.note(`${invite.fromName} invited you to their party`);
        void prompt?.confirm(`Join ${invite.fromName}'s party?`).then((yes) => {
          game.party(yes ? PartyAction_Kind.ACCEPT : PartyAction_Kind.DECLINE);
        });
      },
      onGuildInvite: (invite) => {
        chat?.note(`${invite.fromName} invited you to ${invite.guildName}`);
        void prompt?.confirm(`Join ${invite.guildName}?`).then((yes) => {
          game.guild(yes ? GuildAction_Kind.ACCEPT : GuildAction_Kind.DECLINE);
        });
      },
      onMapChanged: () => {
        // The boss you were fighting is in the room you left.
        boss?.hide();

        // The channel list and "you are here" both belong to the map that was
        // just left. Refetching beats showing a screen that is quietly wrong.
        if (worldMap?.isOpen) game.openWorldMap();
      },
      onDisconnect: (reason) => {
        loop = null;
        panel?.close();
        worldMap?.close();
        social?.close();
        prompt?.cancel();
        chatEl.hidden = true;
        skillBarEl.hidden = true;
        buffBarEl.hidden = true;
        boss?.hide();
        death?.hide();
        passives?.close();
        partyEl.hidden = true;
        void shell.resume(reason);
      },
    });
    loop = game;

    panel = new InventoryPanel(inventoryEl, tooltipEl, game.inventory, {
      onAction: (kind, item, slot, equipSlot) => {
        game.itemAction(kind, item?.itemId ?? "", slot ?? 0, equipSlot ?? "");
      },
    });

    worldMap = new WorldMapPanel(worldMapEl, {
      onRefresh: () => game.openWorldMap(),
      onTravel: (id) => game.travelTo(id),
      onSwitchChannel: (id) => game.switchChannel(id),
      onNewChannel: () => game.newChannel(),
    });

    skillBar = new SkillBarPanel(skillBarEl);
    passives = new PassivePanel(passivesEl, {
      onAllocate: (id) => game.allocatePassive(id),
      onRefund: (id) => game.refundPassive(id),
      onRespec: () => game.respecPassives(),
    });
    buffBar = new BuffBar(buffBarEl);
    boss = new BossFrame(bossEl);
    death = new DeathScreen(deathEl);

    prompt = new Prompt(promptEl, {
      onFocusChange: (focused) => game.setInputEnabled(!focused),
    });

    chat = new ChatPanel(chatEl, {
      onSend: (channel, body, target) => game.chat(channel, body, target),
      // The game stops reading the keyboard while the chat line has it.
      onFocusChange: (focused) => game.setInputEnabled(!focused),
    });
    chatEl.hidden = false;

    const socialCallbacks = {
      onParty: (kind: PartyAction_Kind, target = "") => game.party(kind, target),
      onGuild: (kind: GuildAction_Kind, target = "") => game.guild(kind, target),
      onFriends: (kind: Parameters<typeof game.friends>[0], target = "") =>
        game.friends(kind, target),
      prompt: (question: string) => prompt!.ask(question),
    };
    party = new PartyFrames(partyEl, socialCallbacks);
    party.render();
    social = new SocialPanel(socialEl, socialCallbacks);

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
      // The cooldown sweep and the buff countdown are the only parts of the
      // interface that change continuously, so they ride the frame loop
      // rather than a timer.
      const now = performance.now();
      skillBar?.tick(now);
      buffBar?.render(now);
      boss?.render(now);
      death?.render(now, loop.stats.hp);

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
        `\n[1-8] skills  [i] inventory  [p] passives  [m] map  [o] social  [enter] chat`;
      hud.hidden = false;
    } else {
      hud.hidden = true;
    }
    requestAnimationFrame(render);
  };
  requestAnimationFrame(render);
}

void main();
