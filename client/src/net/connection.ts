import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import {
  EnvelopeSchema,
  type ClientMessage,
  ClientMessageSchema,
  HelloSchema,
  IntentSchema,
  PingSchema,
  CastSchema,
  InteractSchema,
  InteractKind,
  ItemActionSchema,
  ItemActionKind,
  OpenWorldMapSchema,
  TravelSchema,
  ChatSendSchema,
  ChatChannel,
  PartyActionSchema,
  PartyAction_Kind,
  GuildActionSchema,
  GuildAction_Kind,
  SocialActionSchema,
  SocialAction_Kind,
  SetSkillSlotSchema,
  PassiveActionSchema,
  type Inventory,
  type Snapshot,
  type Welcome,
  type Event,
  type EntityState,
  type Travel,
  type WorldMap,
  type ChatLine,
  type SystemMessage,
  type PartyState,
  type PartyInvite,
  type GuildState,
  type GuildInvite,
  type FriendList,
  type SkillBar,
  type BuffState,
  type PassiveState,
  type PassiveAction,
  type BossPhase,
  type Downed,
  type DungeonState,
  type ZoneEvent,
} from "@/gen/mmo/v1/game_pb";

/** Bumped on any incompatible wire change; must match gateway.ProtocolVersion. */
export const PROTOCOL_VERSION = 1;

/**
 * Close codes, mirroring docs/protocol.md.
 *
 * They exist so the client can tell a transient blip from a permanent refusal.
 * Without them, "your ticket expired, get another" and "you are banned" are
 * indistinguishable, and the client either retries forever or gives up too
 * early.
 */
export const Close = {
  TicketInvalid: 4000,
  ProtocolVersion: 4001,
  ContentHash: 4002,
  NotAllowed: 4003,
  Kicked: 4004,
  LeaseLost: 4005,
  RateLimited: 4006,
  ServerShutdown: 4007,
} as const;

export interface ConnectionHandlers {
  onWelcome(w: Welcome): void;
  onSnapshot(s: Snapshot): void;
  onEvent(e: Event): void;
  onInventory(i: Inventory): void;
  onPong(clientTimeMs: number): void;
  onClosed(code: number, reason: string): void;
}

export interface ConnectionStats {
  rttMs: number;
  snapshotsReceived: number;
  bytesReceived: number;
}

/**
 * Connection owns the WebSocket and speaks the protocol.
 *
 * It deliberately knows nothing about prediction or rendering: it decodes
 * frames and hands them upward. That separation is what lets the reconciliation
 * logic be reasoned about, and tested, without a socket.
 */
export class Connection {
  #ws: WebSocket | null = null;
  #handlers: ConnectionHandlers;
  #pingTimer: number | undefined;

  #stats: ConnectionStats = { rttMs: 0, snapshotsReceived: 0, bytesReceived: 0 };

  /** Highest snapshot tick received, echoed back so the server can ack. */
  #lastSnapshotTick = 0n;

  constructor(handlers: ConnectionHandlers) {
    this.#handlers = handlers;
  }

  get stats(): Readonly<ConnectionStats> {
    return this.#stats;
  }

  get connected(): boolean {
    return this.#ws?.readyState === WebSocket.OPEN;
  }

  /**
   * Opens a connection and completes the handshake.
   *
   * The ticket travels in the first frame rather than in the URL. URLs end up
   * in proxy logs, browser history, and referrer headers; a credential must
   * not.
   */
  async connect(ticket: string, contentHash = ""): Promise<void> {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(`${proto}//${location.host}/ws`);
    ws.binaryType = "arraybuffer";
    this.#ws = ws;

    await new Promise<void>((resolve, reject) => {
      ws.onopen = () => resolve();
      ws.onerror = () => reject(new Error("could not reach the server"));
    });

    ws.onmessage = (ev) => this.#onMessage(ev);
    ws.onclose = (ev) => this.#onClose(ev);

    this.send({
      case: "hello",
      value: create(HelloSchema, { ticket, protocolVersion: PROTOCOL_VERSION, contentHash }),
    });

    this.#pingTimer = window.setInterval(() => this.#ping(), 2000);
  }

  /** Sends one tick of intent. */
  sendIntent(seq: number, moveX: number, jump: boolean, up: boolean, down: boolean): void {
    this.send({
      case: "intent",
      value: create(IntentSchema, {
        seq,
        ackSnapshot: this.#lastSnapshotTick,
        moveX,
        jump,
        up,
        down,
      }),
    });
  }

  /**
   * Requests a skill. The client asks; the server decides what it reached and
   * for how much, so there is deliberately no target or damage to send.
   */
  sendCast(seq: number, skillId: string, facingLeft: boolean): void {
    this.send({
      case: "cast",
      value: create(CastSchema, { seq, skillId, facingLeft }),
    });
  }

  /** Requests to loot a nearby drop. */
  sendLoot(entityId: number): void {
    this.send({
      case: "interact",
      value: create(InteractSchema, {
        entityId,
        kind: InteractKind.LOOT,
      }),
    });
  }

  /**
   * Asks the server to move, equip, or destroy an item.
   *
   * The client says what it wants done and never where the item ends up, nor
   * what the resulting stats are. The server decides both, and answers with a
   * whole new inventory.
   */
  sendItemAction(
    kind: "move" | "equip" | "unequip" | "destroy",
    itemId: string,
    slot = 0,
    equipSlot = "",
  ): void {
    const kinds = {
      move: ItemActionKind.MOVE,
      equip: ItemActionKind.EQUIP,
      unequip: ItemActionKind.UNEQUIP,
      destroy: ItemActionKind.DESTROY,
    } as const;

    this.send({
      case: "itemAction",
      value: create(ItemActionSchema, { kind: kinds[kind], itemId, slot, equipSlot }),
    });
  }

  /** Asks for the world map: where the player can go, and where they are. */
  sendOpenWorldMap(): void {
    this.send({ case: "openWorldMap", value: create(OpenWorldMapSchema, {}) });
  }

  /**
   * Asks to move without walking.
   *
   * The server validates every one of these -- that the waypoint is unlocked,
   * that the channel exists and is on this map -- because a client that could
   * name any destination could walk into a level-40 zone at level 3.
   */
  sendTravel(dest: Travel["destination"]): void {
    this.send({ case: "travel", value: create(TravelSchema, { destination: dest }) });
  }

  /**
   * Says something on a channel.
   *
   * The client never names its own audience. It says which channel and, for a
   * whisper, who -- and the server decides who hears it, because a client that
   * could list recipients would be deciding what other players see.
   */
  sendChat(channel: ChatChannel, body: string, target = ""): void {
    this.send({ case: "chat", value: create(ChatSendSchema, { channel, body, target }) });
  }

  /** Asks to change party membership. */
  sendParty(kind: PartyAction_Kind, target = ""): void {
    this.send({ case: "party", value: create(PartyActionSchema, { kind, target }) });
  }

  /** Asks to change a guild. */
  sendGuild(kind: GuildAction_Kind, target = ""): void {
    this.send({ case: "guild", value: create(GuildActionSchema, { kind, target }) });
  }

  /** Asks to change the friends list. */
  sendSocial(kind: SocialAction_Kind, target = ""): void {
    this.send({ case: "social", value: create(SocialActionSchema, { kind, target }) });
  }

  /** Asks to put a skill and its supports in a bar slot. */
  sendSkillSlot(slot: number, skillId: string, supports: string[]): void {
    this.send({
      case: "skillSlot",
      value: create(SetSkillSlotSchema, { slot, skillId, supports }),
    });
  }

  /**
   * Asks to change the passive tree.
   *
   * Exactly one action per message: allocating and refunding together would
   * need an order between them, and the order would decide whether it was
   * legal.
   */
  sendPassive(action: PassiveAction["action"]): void {
    this.send({ case: "passive", value: create(PassiveActionSchema, { action }) });
  }

  send(body: ClientMessage["body"]): void {
    if (!this.connected) return;
    const msg = create(ClientMessageSchema, { body });
    const env = create(EnvelopeSchema, { client: [msg] });
    this.#ws!.send(toBinary(EnvelopeSchema, env));
  }

  close(): void {
    if (this.#pingTimer !== undefined) clearInterval(this.#pingTimer);
    this.#ws?.close(1000, "client closing");
    this.#ws = null;
  }

  #ping(): void {
    this.send({ case: "ping", value: create(PingSchema, { clientTimeMs: BigInt(Date.now()) }) });
  }

  #onMessage(ev: MessageEvent): void {
    const bytes = new Uint8Array(ev.data as ArrayBuffer);
    this.#stats.bytesReceived += bytes.byteLength;

    const env = fromBinary(EnvelopeSchema, bytes);

    // The server batches a whole tick into one frame, so a frame usually holds
    // a snapshot plus whatever events fired alongside it.
    for (const msg of env.server) {
      switch (msg.body.case) {
        case "welcome":
          this.#handlers.onWelcome(msg.body.value);
          break;
        case "snapshot": {
          const snap = msg.body.value;
          if (snap.tick > this.#lastSnapshotTick) this.#lastSnapshotTick = snap.tick;
          this.#stats.snapshotsReceived++;
          this.#handlers.onSnapshot(snap);
          break;
        }
        case "event":
          this.#handlers.onEvent(msg.body.value);
          break;
        case "inventory":
          this.#handlers.onInventory(msg.body.value);
          break;
        case "pong": {
          const sent = Number(msg.body.value.clientTimeMs);
          this.#stats.rttMs = Date.now() - sent;
          this.#handlers.onPong(sent);
          break;
        }
        case "kick":
          this.#handlers.onClosed(msg.body.value.code, msg.body.value.reason);
          break;
      }
    }
  }

  #onClose(ev: CloseEvent): void {
    if (this.#pingTimer !== undefined) clearInterval(this.#pingTimer);
    this.#handlers.onClosed(ev.code, ev.reason || describeClose(ev.code));
  }
}

/** Turns a close code into something worth showing a player. */
export function describeClose(code: number): string {
  switch (code) {
    case Close.TicketInvalid:
      return "Your session expired. Connect again.";
    case Close.ProtocolVersion:
      return "This client is out of date. Reload the page.";
    case Close.ContentHash:
      return "Game content changed. Reload the page.";
    case Close.NotAllowed:
      return "You are not allowed to join.";
    case Close.Kicked:
      return "You were disconnected.";
    case Close.LeaseLost:
      return "Your character was claimed elsewhere.";
    case Close.RateLimited:
      return "Connection could not keep up.";
    case Close.ServerShutdown:
      return "The server is restarting.";
    case 1000:
      return "Disconnected.";
    default:
      return `Connection lost (${code}).`;
  }
}

/** Whether reconnecting is worth attempting for this close code. */
export function isRetryable(code: number): boolean {
  return (
    code === Close.ServerShutdown ||
    code === Close.LeaseLost ||
    code === 1001 ||
    code === 1006
  );
}

export type {
  Snapshot,
  Welcome,
  Event,
  EntityState,
  Inventory,
  WorldMap,
  ChatLine,
  SystemMessage,
  PartyState,
  PartyInvite,
  GuildState,
  GuildInvite,
  FriendList,
  SkillBar,
  BuffState,
  PassiveState,
  BossPhase,
  Downed,
  DungeonState,
  ZoneEvent,
};
export { ChatChannel, PartyAction_Kind, GuildAction_Kind, SocialAction_Kind };
