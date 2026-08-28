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

every frame (60 Hz):
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

Inputs are sent at 30 Hz while prediction runs at 60 Hz: two sampled frames are coalesced per `Intent`. The server applies inputs on its own 20 Hz tick, consuming whatever has arrived.

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
    // ...
  }
}
```

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
