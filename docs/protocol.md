# Network Protocol

Companion to `architecture.md`. This document specifies the wire format, the handshake, and — most importantly — the prediction and reconciliation model. Get this wrong and the game feels bad no matter how good everything else is.

## Transport

**WebSocket over TLS, binary frames, Protobuf-encoded.**

WebSocket is TCP: delivery is reliable and ordered, and there is no unreliable channel. That means no packet-loss handling and no input redundancy is required, but it does mean **head-of-line blocking** — one lost segment stalls everything behind it until retransmission.

For a 20 Hz 2D game this is an acceptable trade and is what essentially every browser MMO ships. If p99 latency under loss becomes a real complaint, **WebTransport** (QUIC, with unreliable datagrams for snapshots and a reliable stream for events) is the upgrade path. The message layer is designed to allow it: snapshots are already self-contained and idempotent, so they can be moved to an unreliable channel without redesign.

Protobuf over a hand-rolled bitpacked format is a deliberate early trade: schema evolution, codegen for both Go and TypeScript, and readable debugging matter more right now than the ~30% size saving. Snapshot bandwidth is nowhere near the constraint at this scale (§ Bandwidth budget).

## Handshake

Long-lived credentials must never appear in a WebSocket URL — URLs land in proxy logs, browser history, and referrer headers.

```
1. Browser        → GET /auth/login?provider=google
2. Auth (OIDC RP) → 302 to provider, Authorization Code + PKCE
3. Provider       → callback with code
4. Auth           → verify, check allowlist, upsert account
                  → session JWT (15 min) + refresh token (httpOnly, SameSite=Lax cookie)
5. Browser        → POST /api/ticket  (bearer: session JWT)
6. Auth           → single-use ticket, 30s TTL, bound to account + client IP
7. Browser        → WebSocket upgrade, ticket sent in the first Hello frame
8. Gateway        → redeem ticket (atomic delete-if-exists in Redis), bind connection
9. Gateway        → Welcome
```

The ticket is redeemed atomically, so a replayed ticket fails. Rejection at step 8 closes the socket with a typed close code — the client distinguishes "get a new ticket and retry" from "you are not allowed here".

### Version and content gating

`Hello` carries `protocol_version` and `content_hash`. The server rejects a mismatch with a typed error rather than letting a client run against content it disagrees about. A client that thinks a mob has 400 HP when the server says 900 produces bug reports that are almost impossible to read.

## Message envelope

Every frame is one `Envelope`. The server batches a tick's worth of messages into a single frame to avoid per-message WebSocket overhead.

```protobuf
message Envelope {
  repeated ServerMessage server = 1;  // server → client, batched per tick
  repeated ClientMessage client = 2;  // client → server
}
```

### Client → Server

```protobuf
message ClientMessage {
  oneof body {
    Hello        hello        = 1;
    Intent       intent       = 2;   // hot path, ~30 Hz
    CastRequest  cast         = 3;
    Interact     interact     = 4;   // loot, gather, portal, NPC
    ChatSend     chat         = 5;
    InventoryOp  inventory    = 6;
    Ping         ping         = 7;
    OpenWorldMap world_map    = 8;   // ask for the map screen's contents
    Travel       travel       = 9;   // waypoint, or a channel of this map
    PartyAction  party        = 10;
    GuildAction  guild        = 11;
    SocialAction social       = 12;  // the friends list
  }
}

message Intent {
  uint32 seq          = 1;  // strictly increasing, per connection
  uint32 ack_snapshot = 2;  // highest snapshot tick received (delta baseline)
  sint32 move_x       = 3;  // clamped to [-1000, 1000] = [-1.0, 1.0]
  bool   jump         = 4;
  bool   up           = 5;  // rope/ladder/portal
  bool   down         = 6;
}
```

`Intent` carries **no position**. The client does not tell the server where it is; it tells the server what buttons are held. This single rule removes the entire class of teleport and speed hacks.

### Server → Client

```protobuf
message ServerMessage {
  oneof body {
    Welcome  welcome  = 1;
    Snapshot snapshot = 2;   // hot path, 20 Hz
    Event    event    = 3;   // one-shot, not state
    Pong     pong     = 4;
    Kick     kick     = 5;
  }
}

message Snapshot {
  uint32 tick          = 1;
  uint32 baseline_tick = 2;  // 0 = full snapshot, else delta against this tick
  uint32 ack_seq       = 3;  // highest Intent.seq applied — drives reconciliation
  repeated EntityDelta entities = 4;
  repeated uint32      removed  = 5;
  SelfState self       = 6;   // always full, never delta'd
}

message EntityDelta {
  uint32 id         = 1;
  uint32 field_mask = 2;  // which optional fields below are present
  sint32 x = 3; sint32 y = 4;      // fixed-point, 1/256 world unit
  sint32 vx = 5; sint32 vy = 6;
  uint32 anim = 7;  bool facing_left = 8;
  uint32 hp = 9;    uint32 hp_max = 10;
  // ...
}
```

`SelfState` is always sent in full. It is one entity, it is the one the client reconciles against, and a delta bug there causes desync that is far more expensive than the handful of bytes saved.

## Prediction and reconciliation

The client runs **the same simulation code as the server** — `internal/world/sim` compiled to WASM via TinyGo — so predicted and authoritative results agree bit-for-bit given the same inputs.

### Client loop

```
seq     = 0
pending = ring buffer of {seq, input}     // unacknowledged inputs

every simulation tick (20 Hz, fixed accumulator):
    input = sampleInput()
    seq  += 1
    pending.push({seq, input})
    send Intent{seq, input, ack_snapshot: latestSnapshotTick}
    predicted = sim.Step(predicted, input)      // immediate local response

on Snapshot(s):
    predicted = s.self                          // snap to authority
    for p in pending where p.seq > s.ack_seq:   // replay unacked inputs
        predicted = sim.Step(predicted, p.input)
    pending.dropThrough(s.ack_seq)

    error = |predicted.pos - rendered.pos|
    if error < SMOOTH_THRESHOLD:                // 0.5 world units
        smooth rendered → predicted over ~100 ms
    else:
        rendered = predicted                    // hard correction, visible
```

The smoothing threshold matters. Correcting every sub-pixel disagreement instantly produces visible jitter; correcting nothing lets real desync accumulate. Small errors are eased out over ~100 ms, large ones snap — a rare hard snap reads as lag, constant micro-jitter reads as a broken game.

**The client simulates at the server's tick rate, not at the display rate.** One sampled input produces one predicted step and one `Intent`, so the server consumes exactly the sequence the client predicted. Rendering runs at the display refresh and interpolates between simulated ticks.

Driving the simulation from frame time instead would produce a different number of steps on a 60 Hz and a 144 Hz display, and the server would then apply a different number of inputs than the client predicted — turning the refresh rate into a gameplay variable.

### Why sequence numbers, not timestamps

`ack_seq` is the client's own counter, echoed back. It requires no clock synchronisation and cannot be manipulated to gain advantage — a client that lies about `seq` only corrupts its own reconciliation.

## Entity interpolation

The local character is predicted forward. **Every other entity is rendered in the past**, which is the only way to display smooth motion from 20 Hz updates without inventing state.

```
INTERP_DELAY = 100 ms   (2 ticks of buffer, absorbs one dropped/late snapshot)

renderTime = estimatedServerTime() - INTERP_DELAY
find snapshots a, b bracketing renderTime
t = (renderTime - a.time) / (b.time - a.time)
position  = lerp(a.pos, b.pos, t)
animation = a.anim        // discrete state never interpolates
```

If the buffer starves, extrapolate along last known velocity for at most 250 ms, then freeze the entity in place. Extrapolating further produces entities that visibly slide through walls and then teleport back.

**Consequence for combat:** the server resolves hits against *its* current state, not what the attacking client saw 100 ms ago. There is no lag compensation / backwards reconciliation, which is the right call for this game — melee and skill-shot hitboxes here are generous, and rewinding the world to validate hits creates "I was hit behind a wall" complaints that are worse than a slightly tight hitbox. Revisit only if it demonstrably hurts at higher latencies.

## Clock estimate

```
on Pong(clientSendTime, serverTick):
    rtt    = now - clientSendTime
    offset = serverTick + (rtt / 2 / TICK_MS) - localTickEstimate

estimatedServerTime() = localTime + smoothedOffset
```

Offset is smoothed with an EWMA and clamped to a slow drift rate, so a single delayed `Pong` cannot jerk the render clock. Ping every 2 s.

## Delta compression

The server tracks each connection's last acknowledged snapshot tick and deltas against it.

- `Intent.ack_snapshot` carries the acknowledgement, so it costs no extra messages.
- If the baseline is older than 1 second (20 ticks), send a full snapshot instead — chasing a stale baseline costs more than resending.
- Per entity, `field_mask` marks which fields changed. Unchanged entities are omitted entirely.
- `removed` lists entities that left view or died, so the client can retire them.

Positions are fixed-point (`1/256` world unit) rather than floats: smaller on the wire, and exactly reproducible across Go and WASM. **The simulation itself uses fixed-point integer math throughout** for the same reason — float determinism across platforms and compilers is not something to bet a prediction model on.

## Area of interest and layering

The AOI filter answers one question: **may this viewer see this entity?** It combines two independent tests.

```go
type AOI interface {
    Visible(viewer EntityID, e EntityID) bool
}
```

**Layer test (from M0).** An entity is visible if it is in the shared layer or in the viewer's own layer (`architecture.md` § Axis 2). Players are always shared-layer, so everyone in a room always sees everyone else; only hostile and lootable entities are layered. This test is not an optimisation — it is a correctness requirement, and a bug here leaks other players' mobs and loot into a client's view.

**Spatial test (deferred).** Initially "everything in the room": MapleStory-style maps are small and capped at 20–30 players, so a spatial filter would cost more than it saves. A grid-based filter drops in behind the same interface when large open zones arrive.

Because layering multiplies entity count by the number of active layers (`architecture.md` § What layering costs), the snapshot builder must iterate *the viewer's layer plus shared*, never the whole room filtered afterwards. Building a full entity list and discarding 95% of it per player is O(players x layers x mobs) and will dominate the tick at capacity.

```go
// Right: iterate only what this viewer can see.
for _, e := range room.layer[viewer.layer] { ... }
for _, e := range room.layer[SharedLayer]  { ... }

// Wrong: O(players x all entities) per tick.
for _, e := range room.allEntities { if aoi.Visible(viewer, e) { ... } }
```

Entity IDs are unique across the whole room regardless of layer, so nothing else in the protocol needs layer awareness — a client simply never receives IDs it cannot see.

## Events

Events are one-shot facts, not state: damage numbers, level-ups, skill VFX triggers, chat lines, loot notifications, system messages. They are never delta-compressed and never inferred by the client from state changes — a client that derives "took damage" from a falling HP number cannot distinguish 200 damage from two 100s.

```protobuf
message Event {
  oneof body {
    DamageDealt  damage    = 1;
    EntityDied   died      = 2;
    SkillCast    cast      = 3;
    ChatLine     chat      = 4;
    LootDropped  loot      = 5;
    LevelUp      level_up  = 6;
    BossPhase    boss_phase = 7;
    // ...
  }
}
```

`BossPhase` is the shape of the rule: a phase change is the fight telling a party to do something different, and a party that misses it does not. Left for the client to notice in a falling health bar, it would arrive as "the numbers got worse" rather than "the fight changed".

### Telegraphs are entities, not events

A boss's wind-up marker enters and leaves a client's view through the same path as a mob or a bolt — one entity kind among the others — rather than as a pair of "started" and "finished" messages the client has to correlate. There is no second visibility system to keep in step with the first, and a marker cannot outlive the layer it belongs to.

Its `hp` and `hp_max` carry the wind-up: ticks remaining when the marker appeared, out of the whole. Both are set once and never touched again, so the client fills the bar from its own clock and the entire wind-up costs one message rather than one per tick. Sending the *remainder* rather than the elapsed time is what makes a player who arrives mid-wind-up see the right amount of time left.

## Chat

Five channels — local, global, whisper, party, guild — that differ in who hears you, not in what you can say. One message with a channel rather than five messages, and the server decides the audience:

```protobuf
message ChatSend {
  ChatChannel channel = 1;
  string body = 2;
  string target = 3;   // a character name, for whispers only
}
```

There is deliberately no recipient list. A client that could name its own audience would be deciding what other players see, which is the one thing it never gets to do. `target` is the sole exception and it names one character, whom the server then resolves through presence — a name that is not online comes back as a refusal, not silence.

Every line comes back as a `ChatLine` carrying the speaker's name **as the server resolved it**. The sender gets their own copy too: a client that renders its message optimistically and then receives the real one shows it twice, and one that never receives it cannot tell delivery from a drop. A whisper's outgoing copy sets `outgoing` and names the *recipient*, so the two halves of a conversation read differently ("to Alice" against "Alice whispers").

Refusals arrive as `SystemMessage`, never as a missing `ChatLine`. A message that vanishes reads as the game being broken, and most of the reasons — a typo in a name, a rate limit, a mute — are things the player can act on.

### Limits

Per channel, from `content/balance.toml`, because they cost different amounts to send: a global line reaches everyone online and a local one reaches a room. Buckets start full, so greeting your party on arriving is not throttled. Messages are bounded in **characters, not bytes** — a byte limit quietly gives players of some languages less to say — and control characters are stripped, since a message that could forge a line break could impersonate the server's own notices.

Mutes are checked server-side per message and cached briefly. A muted player is told they are muted and, where there is one, why.

## Parties and guilds

Both are asked for and answered whole:

```protobuf
message PartyAction { Kind kind = 1; string target = 2; }
message PartyState {
  string party_id = 1;
  string leader_character_id = 2;
  repeated PartyMember members = 3;
  string self_character_id = 4;
  string loot = 5;
}
```

The roster is sent whole rather than as a delta. A party is at most six members, the delta would be most of the message anyway, and a client that misses one incremental update shows a roster that is quietly wrong until somebody leaves. An empty member list means "you are not in a party".

Member vitals are the one part that changes continuously, so they arrive on their own slower beat (once a second) rather than in the roster. A party member in another room does not appear in the snapshot at all — which is most of what a member frame is for.

Guild state is shaped the same way, with ranks (1 member, 2 officer, 3 leader) and the recipient's own rank included so a client can grey out the buttons they cannot use rather than offering them and being refused. Every action is still checked server-side: a client asking to promote somebody in a guild it does not lead is asking, not doing.

Invitations are questions asked in the moment. They arrive as their own event with a TTL and expire on their own, rather than sitting in a queue nobody answered.

## Moving between rooms

A character can end up somewhere else three ways: walking into a portal, fast-travelling to an unlocked waypoint, or switching channel. All three are the same room handoff (`architecture.md` § Room handoff), and all three end the same way on the wire — **a second `Welcome`**.

There is deliberately no separate "you moved" message. The `Welcome` the destination room sends already carries the new entity id, the new instance, the new map, and the full prediction state for the character; a smaller message alongside it would be a second source of the same truth, and eventually the wrong one.

A client receiving a `Welcome` while already playing must treat everything it holds as stale:

1. Stop predicting and sending intent — the character is frozen at both ends until the destination accepts.
2. Discard the pending input buffer. Those inputs describe movement in a room the character has left; replaying them against the new map pushes them through geometry that does not exist there.
3. Clear the smoothing offset, so the arrival is a hard snap rather than a visible glide across the new map from wherever they stood in the old one.
4. Drop every interpolated entity and the collision geometry, and fetch the new map's.
5. Seed the predictor from `Welcome.self` and resume.

Snapshots that arrive while the geometry is still loading are dropped rather than applied. The next one describes the same room completely.

`Travel` names exactly one destination: a waypoint id, an instance id, or `new_channel`. The server validates all three — the waypoint against the database rather than against what the room believes, the instance against the map the player is actually in — because a client that could name any destination could walk into a level-40 zone at level 3. A refusal comes back as `PortalRefused` with a reason: a travel request that silently does nothing reads as a broken button, and most of the reasons (a full channel, a waypoint not yet found) are things a player does by accident.

`OpenWorldMap` is requested rather than pushed. Its contents change only when a player unlocks a waypoint or a channel's occupancy shifts, neither of which is worth a per-tick broadcast to every client in the world. It lists every zone with its level range, only the waypoints the character has actually unlocked — the world map is a record of where they have been, and listing the rest would give away what is out there — and the channels of the map they are currently in.

## Bandwidth budget

Worst realistic case — 40 players and 60 mobs in one room, all moving:

| | |
|---|---|
| Entity delta (typical) | ~14 bytes |
| Entities changed per tick | ~100 |
| Snapshot | ~1.4 KB |
| Per player downstream | ~28 KB/s at 20 Hz |
| Per player upstream | ~0.5 KB/s |

At 1000 concurrent players spread across rooms, aggregate downstream is roughly 25 MB/s — comfortably inside a single gigabit link, and world nodes shard it further. Bandwidth is not the binding constraint; **CPU in the tick loop is**. Optimise the tick before optimising the wire.

## Input validation

Everything from the client is hostile until proven otherwise:

| Check | Response |
|---|---|
| `seq` not strictly increasing | Drop message |
| `move_x` outside `[-1000, 1000]` | Clamp |
| Intent rate > 60/s | Drop excess, log, kick on sustained abuse |
| Cast: cooldown not elapsed | Reject, do not surface as an error (normal race) |
| Cast: insufficient cost / out of range / no LoS | Reject with typed error |
| Interact: entity not in room or out of range | Reject |
| Inventory op: item not owned / slot occupied | Reject, log — likely a client bug or an exploit attempt |
| Chat rate > channel limit | Drop, apply cooldown |
| Frame > 8 KB | Close connection |

Rejections are typed so the client can distinguish a benign race (cooldown) from a real desync (item not owned) and resync instead of silently diverging.

## Close codes

| Code | Meaning | Client action |
|---|---|---|
| 4000 | Ticket invalid or expired | Fetch a new ticket, retry |
| 4001 | Protocol version mismatch | Force reload |
| 4002 | Content hash mismatch | Force reload |
| 4003 | Not on allowlist | Show message, do not retry |
| 4004 | Kicked / banned | Show message, do not retry |
| 4005 | Character lease lost | Retry once after 2 s |
| 4006 | Rate limit exceeded | Back off 30 s |
| 4007 | Server shutting down | Reconnect with backoff |

## Reconnection

The character stays live in its room for a **60-second grace period** after an unexpected disconnect, frozen and invulnerable. Reconnecting within that window resumes in place; after it, the character is checkpointed and unloaded.

This matters more than it looks: without a grace period, every transient network blip during a boss fight is a wipe.
