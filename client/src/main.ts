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
import { DungeonFrame } from "@/ui/dungeon";
import { VitalsBar } from "@/ui/vitals";
import { SettingsPanel } from "@/ui/settings";
import { SkillsPanel } from "@/ui/skills";
import { GatheringLine } from "@/ui/gathering";
import { CraftingPanel } from "@/ui/crafting";
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
const dungeonEl = document.getElementById("dungeon") as HTMLDivElement;
const vitalsEl = document.getElementById("vitals") as HTMLDivElement;
const settingsEl = document.getElementById("settings") as HTMLDivElement;
const skillsEl = document.getElementById("skills") as HTMLDivElement;
const gatheringEl = document.getElementById("gathering") as HTMLDivElement;
const craftingEl = document.getElementById("crafting") as HTMLDivElement;

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
let dungeon: DungeonFrame | null = null;
let vitals: VitalsBar | null = null;
let settings: SettingsPanel | null = null;
let skills: SkillsPanel | null = null;
let gathering: GatheringLine | null = null;
let crafting: CraftingPanel | null = null;

// Secondary skill names, so the gathering line can say "Woodcutting" rather
// than "woodcutting". The server sends the display name once, with the full
// skills state, and the per-yield events carry only the id.
const skillNames = new Map<string, string>();
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

    if (e.code === "KeyK") {
      e.preventDefault();
      settings?.toggle();
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
      skills?.close();
      void passives?.toggle();
    }
    if (e.code === "KeyR") {
      e.preventDefault();
      if (crafting?.isOpen) {
        crafting.close();
      } else if (!loop?.openStation()) {
        // Nothing in reach. Said rather than silent, because a key that does
        // nothing is indistinguishable from a key that is broken.
        setStatus("there is nothing to work at here");
      }
    }
    if (e.code === "KeyJ") {
      e.preventDefault();
      panel?.close();
      worldMap?.close();
      social?.close();
      passives?.close();
      skills?.toggle();
    }
    if (e.code === "Escape") {
      panel?.close();
      worldMap?.close();
      social?.close();
      passives?.close();
      settings?.close();
      skills?.close();
      crafting?.close();
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

    // Whatever was playing before stops here, before anything new starts.
    //
    // Without this a character switch leaves the previous loop running: its
    // socket stays open, so the character it was playing stays in the world as
    // a second body standing on the spawn point, and two loops draw into one
    // scene -- both calling drawSelf, both moving the camera, and each
    // retiring sprites the other had just created. The visible symptoms are a
    // ghost of the character you just left and other players flickering in and
    // out.
    loop?.stop();
    loop = null;

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
      onDungeon: (state) => dungeon?.update(state, performance.now()),

      // Zone events go to chat rather than a frame of their own. They are
      // announcements -- one line, twice per event -- and a panel that was
      // empty except for ten seconds every three minutes would be worse than
      // the line it replaced.
      onZoneEvent: (zone) => {
        chat?.note(
          zone.active
            ? zone.message || `${zone.name} has begun`
            : `${zone.name} is over`,
        );
      },
      // Gathering. The refusal text is the server's, because every reason it
      // can refuse is a rule only the server knows.
      onGathering: (state) =>
        gathering?.update(
          state,
          skillNames.get(state.skill) ?? state.skill,
          performance.now(),
        ),

      onSecondaryExp: (exp) => {
        skills?.gained(exp, performance.now());
        if (exp.levelUp) {
          const name = skillNames.get(exp.skill) ?? exp.skill;
          chat?.note(`Your ${name} level is now ${exp.level}.`);
        }
      },

      // The whole set: on entering the world, and again whenever equipment
      // changes, because what is in hand is part of what the panel shows.
      onSecondarySkills: (state) => {
        const levels = new Map<string, number>();
        for (const s of state.skills) {
          skillNames.set(s.skill, s.name);
          levels.set(s.skill, s.level);
        }
        skills?.setAll(state.skills);
        // So the renderer can dim a node the character is not good enough for.
        scene?.setSkillLevels(levels);
      },

      // Crafting. Both halves go to the panel: it is the only place the state
      // makes sense, because "made 4" means nothing without the recipe it is
      // counting.
      onStationMenu: (menu) => crafting?.open(menu),
      onCrafting: (state) => {
        crafting?.update(state);
        // Re-ask after each completed run, so the ingredient counts follow the
        // materials being spent. A panel that kept showing 6/2 while the bag
        // emptied would be a panel that says a recipe is available right up to
        // the run that stops for want of it.
        if (state.active && state.produced) loop?.openStation();
      },

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
        // The boss you were fighting, and the run you were on, are both in
        // the room you left.
        boss?.hide();
        dungeon?.hide();

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
        dungeon?.hide();
        vitals?.hide();
        settings?.close();
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
    dungeon = new DungeonFrame(dungeonEl);
    vitals = new VitalsBar(vitalsEl);
    skills = new SkillsPanel(skillsEl);
    gathering = new GatheringLine(gatheringEl);
    crafting = new CraftingPanel(craftingEl, {
      onCraft: (entityId, recipeId) => game.craft(entityId, recipeId),
      onStop: (entityId) => game.stopCraft(entityId),
    });

    // Built here rather than at startup because it needs the scene, and the
    // scene does not exist until a character is in the world.
    settings = new SettingsPanel(settingsEl, {
      onZoom: (zoom) => scene!.setZoom(zoom),
      onAutoLoot: (on) => game.setAutoLoot(on),
    });
    settings.restore();

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
      dungeon?.render(now);
      skills?.render(now);
      gathering?.render(now);
      vitals?.update(loop.stats);

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
        `\n[1-8] skills  [i] inventory  [p] passives  [m] map  [o] social  [k] settings  [enter] chat`;
      hud.hidden = false;
    } else {
      hud.hidden = true;
    }
    requestAnimationFrame(render);
  };
  requestAnimationFrame(render);
}

void main();
