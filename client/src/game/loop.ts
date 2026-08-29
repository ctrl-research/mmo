import { Connection, describeClose } from "@/net/connection";
import type {
  ChatLine,
  Event,
  FriendList,
  GuildInvite,
  GuildState,
  Inventory,
  PartyInvite,
  PartyState,
  Snapshot,
  SkillBar,
  BuffState,
  SystemMessage,
  Welcome,
  WorldMap,
} from "@/net/connection";
import {
  ChatChannel,
  GuildAction_Kind,
  PartyAction_Kind,
  SocialAction_Kind,
} from "@/net/connection";
import { Interpolator } from "./interpolator";
import { InventoryState } from "./inventory";
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

  /** Called when the server sends a new inventory, so the UI can redraw. */
  onInventory?(): void;

  /** Called with the world map the server sent, in answer to a request. */
  onWorldMap?(map: WorldMap): void;

  /** Called when the character arrives somewhere else. */
  onMapChanged?(mapId: string): void;

  onChat?(line: ChatLine): void;
  onSkillBar?(bar: SkillBar): void;
  onBuffs?(buffs: BuffState): void;

  /** Called when the server confirms this player cast something. */
  onCast?(skillId: string): void;
  onSystem?(msg: SystemMessage): void;
  onParty?(state: PartyState): void;
  onPartyInvite?(invite: PartyInvite): void;
  onGuild?(state: GuildState): void;
  onGuildInvite?(invite: GuildInvite): void;
  onFriends?(list: FriendList): void;
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
  #mapId = "";
  #running = false;

  /**
   * Set while a map change is being loaded.
   *
   * Snapshots that arrive in the meantime describe the new room using geometry
   * the client has not fetched yet, so they are dropped rather than applied.
   * The next one after the load is a full state anyway.
   */
  #changingMap = false;

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

  /** The server's view of the inventory, cached so the UI can be drawn. */
  readonly inventory = new InventoryState();

  /**
   * The skill bar as the server last sent it.
   *
   * The client never decides what is on it: the bar is persisted, validated
   * against what the character has learned, and pushed down. Pressing a key
   * looks up an id here and asks.
   */
  #bar: SkillBar | null = null;

  constructor(sim: Sim, scene: Scene, cb: LoopCallbacks) {
    this.#sim = sim;
    this.#scene = scene;
    this.#cb = cb;
    this.#predictor = new Predictor(sim);

    this.#conn = new Connection({
      onWelcome: (w) => void this.#onWelcome(w),
      onSnapshot: (s) => this.#onSnapshot(s),
      onEvent: (e) => this.#onEvent(e),
      onInventory: (i) => this.#onInventory(i),
      onPong: () => {},
      onClosed: (code, reason) => this.#onClosed(code, reason),
    });
  }

  /**
   * Connects with a ticket already obtained by the shell.
   *
   * Identity and character selection are settled over authenticated HTTP
   * before this point, so the socket carries only a single-use ticket and the
   * game protocol never has to know who anyone is.
   */
  async connect(name: string, ticket: string, contentHash: string): Promise<void> {
    this.#name = name;
    await this.#conn.connect(ticket, contentHash);
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

  /**
   * Enters a room.
   *
   * A second Welcome means the character has moved: the same connection, a
   * different room. Everything the client was tracking is now wrong -- the
   * geometry, its own entity id, and every entity it had interpolating -- so
   * rather than reconcile any of it, all of it is discarded and rebuilt from
   * this message and the snapshots that follow.
   */
  async #onWelcome(w: Welcome): Promise<void> {
    const moved = this.#running;

    this.#changingMap = true;
    this.#selfId = w.entityId;
    this.#serverTick = w.tick;
    this.#interp.clear();
    this.#scene.clearEntities();
    this.#predictor.suspend();

    try {
      await this.#loadMap(w.mapId);
    } finally {
      this.#changingMap = false;
    }

    if (w.self) this.#predictor.reset(w.self);

    if (moved) {
      this.#cb.onMapChanged?.(w.mapId);
      this.#cb.onStatus(`arrived in ${w.mapId}`);
      return;
    }

    this.#input.attach();
    this.#running = true;
    this.#lastFrame = performance.now();
    this.#scene.ticker.add(() => this.#frame());

    this.#cb.onStatus(`connected as ${this.#name}`);
  }

  /**
   * Fetches a map's collision geometry and hands it to both the simulation and
   * the renderer.
   *
   * Geometry comes from the server, encoded by the same function the
   * simulation uses, so the client cannot be predicting against different
   * geometry than the server is enforcing.
   */
  async #loadMap(mapId: string): Promise<void> {
    const collision = await fetch(`/api/map/${mapId}/collision`);
    if (!collision.ok) throw new Error(`could not load collision for map ${mapId}`);
    const bytes = new Uint8Array(await collision.arrayBuffer());

    this.#sim.setWorld(bytes);
    this.#scene.setMap(toGeometry(decodeWorld(bytes)));
    this.#mapId = mapId;
  }

  #onSnapshot(snap: Snapshot): void {
    // Arriving mid-load: this snapshot describes a room whose geometry is not
    // in hand yet, and the next one will describe the same thing completely.
    if (this.#changingMap) return;

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
  #onInventory(inv: Inventory): void {
    this.inventory.apply(inv);
    this.#cb.onInventory?.();
  }

  /** Asks the server to act on an item. */
  itemAction(
    kind: "move" | "equip" | "unequip" | "destroy",
    itemId: string,
    slot = 0,
    equipSlot = "",
  ): void {
    this.#conn.sendItemAction(kind, itemId, slot, equipSlot);
  }

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

      case "skillBar":
        this.#bar = e.body.value;
        this.#cb.onSkillBar?.(e.body.value);
        break;

      case "buffs":
        this.#cb.onBuffs?.(e.body.value);
        break;

      case "skillCast": {
        const cast = e.body.value;
        this.#scene.playAttack(cast.casterId, this.#selfId);

        // The bar's cooldown sweep starts when the server confirms the cast,
        // not when the key was pressed: a cast the server refused should not
        // grey the key out.
        if (cast.casterId === this.#selfId) {
          this.#cb.onCast?.(cast.skillId);
        }
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

      case "worldMap":
        this.#cb.onWorldMap?.(e.body.value);
        break;

      case "waypointFound":
        this.#cb.onStatus(`waypoint discovered: ${e.body.value.name}`);
        break;

      case "portalRefused": {
        const r = e.body.value;
        this.#cb.onStatus(
          r.reason || `you need level ${r.requiredLevel} to enter ${r.targetMap}`,
        );
        break;
      }

      case "chat":
        this.#cb.onChat?.(e.body.value);
        break;

      case "system":
        this.#cb.onSystem?.(e.body.value);
        break;

      case "party":
        this.#cb.onParty?.(e.body.value);
        break;

      case "partyInvite":
        this.#cb.onPartyInvite?.(e.body.value);
        break;

      case "guild":
        this.#cb.onGuild?.(e.body.value);
        break;

      case "guildInvite":
        this.#cb.onGuildInvite?.(e.body.value);
        break;

      case "friends":
        this.#cb.onFriends?.(e.body.value);
        break;

      case "lootTaken": {
        const l = e.body.value;
        if (l.failed) {
          // The drop is still there. Saying why beats the keypress appearing
          // to do nothing.
          this.#cb.onStatus(l.reason || "could not pick that up");
        } else if (l.gold > 0) {
          this.#cb.onStatus(`+${l.gold} gold`);
        } else {
          this.#cb.onStatus("picked up an item");
        }
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
    // Nothing predicted or sent while the character is between rooms: the
    // source room has already frozen them, and the destination has not
    // accepted them yet.
    if (this.#changingMap) return;

    const input = this.#input.sample();
    const seq = this.#predictor.predict(input);
    this.#conn.sendIntent(seq, input.moveX, input.jump, input.up, input.down);

    // Attacks are not predicted. The server decides what a swing reached and
    // for how much, and guessing locally would mean showing damage numbers
    // that the server then contradicts -- worse than showing them a frame
    // later.
    // The bar decides what a key casts. The client sends a skill id and the
    // server checks it against the character's own loadout, so pressing 3 is
    // a request rather than an instruction.
    const facing = isFacingLeft(this.#predictor.body);

    const slot = this.#input.takeSlot();
    if (slot >= 0) {
      const skill = this.#bar?.slots[slot]?.skillId;
      if (skill) this.#conn.sendCast(seq, skill, facing);
    }

    // The attack key is slot one, so the game is playable without learning
    // the bar first.
    if (this.#input.takeAttack()) {
      const skill = this.#bar?.slots[0]?.skillId;
      if (skill) this.#conn.sendCast(seq, skill, facing);
    }

    if (this.#input.takeLoot()) {
      const b = this.#predictor.body;
      const target = this.#interp.nearestDrop(b.x, b.y, LOOT_SEARCH_RADIUS);
      if (target !== 0) this.#conn.sendLoot(target);
    }

    this.#ticksSimulated++;
  }

  /** Says something. The server decides who hears it. */
  chat(channel: ChatChannel, body: string, target = ""): void {
    this.#conn.sendChat(channel, body, target);
  }

  /** Asks to change party membership. */
  party(kind: PartyAction_Kind, target = ""): void {
    this.#conn.sendParty(kind, target);
  }

  /** Asks to change a guild. */
  guild(kind: GuildAction_Kind, target = ""): void {
    this.#conn.sendGuild(kind, target);
  }

  /** Asks to change the friends list. */
  friends(kind: SocialAction_Kind, target = ""): void {
    this.#conn.sendSocial(kind, target);
  }

  /** Asks to put a skill and its supports in a bar slot. */
  setSkillSlot(slot: number, skillId: string, supports: string[]): void {
    this.#conn.sendSkillSlot(slot, skillId, supports);
  }

  /** The bar as the server last sent it, for the UI to draw. */
  get skillBar(): SkillBar | null {
    return this.#bar;
  }

  /**
   * Stops sampling the keyboard, while a text field has it.
   *
   * Without this the character runs off across the map while its player writes
   * a sentence -- and worse, the movement keys never come back up, because the
   * keyup lands in the input.
   */
  setInputEnabled(enabled: boolean): void {
    if (enabled) {
      this.#input.attach();
    } else {
      this.#input.detach();
    }
  }

  /** Asks the server for the world map. */
  openWorldMap(): void {
    this.#conn.sendOpenWorldMap();
  }

  /** Asks to fast-travel to an unlocked waypoint. */
  travelTo(waypointId: string): void {
    this.#conn.sendTravel({ case: "waypointId", value: waypointId });
  }

  /** Asks to switch to a particular channel of the current map. */
  switchChannel(instanceId: bigint): void {
    this.#conn.sendTravel({ case: "channelInstanceId", value: instanceId });
  }

  /** Asks for any channel but this one, creating it if necessary. */
  newChannel(): void {
    this.#conn.sendTravel({ case: "newChannel", value: true });
  }

  get mapId(): string {
    return this.#mapId;
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
