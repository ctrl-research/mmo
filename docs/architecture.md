# Architecture

## Goals

1. **Playable at hobby scale on one box.** `docker compose up` gives a full working game for <10 players. No Kubernetes, no message broker, no service mesh.
2. **Horizontally scalable to 1000+ concurrent players without a rewrite.** The path from one box to many nodes is a configuration change plus swapping two interface implementations — not a redesign.
3. **Server-authoritative.** The client renders state and sends intents. It never decides an outcome.

Goal 2 is the one that constrains day-to-day decisions. It is easy to write a single-process game server that cannot be split later; the constraints in this document exist specifically to prevent that.

## The unit of scale is the room instance

Players interact with the world almost entirely through the room they are standing in: they see each other, hit each other's mobs, and pick up each other's drops only within a room. Everything that crosses rooms — chat, party, guild, trade, mail, presence — is low-frequency and latency-tolerant.

That asymmetry is the whole scaling story:

- A **room instance** is one map, populated, simulated by a single goroutine running a fixed-rate tick loop. It owns every piece of mutable state inside it. It takes no locks, because nothing else touches its state.
- Room instances are **independent**. Two rooms never block on each other.
- A **world node** hosts many room instances. Adding capacity means adding world nodes and distributing rooms across them.
- Cross-room traffic goes over a **bus**, never a direct call.

Because rooms are independent, a room is also the natural unit of failure, migration, and replay.

### The rule that makes this work

> **A room may never read or write another room's state directly, even when both rooms live in the same process.**

There is no compiler that enforces this. It is the single most important convention in the codebase, and the one most likely to be violated by accident, because at hobby scale the shortcut always works. The day you run two world nodes, every such shortcut becomes a bug that only reproduces under load.

The same rule applies to player handoff between rooms. Moving a character from room A to room B follows the full transfer protocol (§ Room handoff) even when A and B are goroutines in one process. If the local path is special-cased, the distributed path is never actually exercised until the day it has to work.

## Process topology

One binary, `cmd/mmo`, with roles selected at runtime:

```
./mmo --roles=all                    # hobby: everything in one process
./mmo --roles=gateway                # scaled: separate deployments
./mmo --roles=world --shard=3
./mmo --roles=social
```

### Roles

| Role | Stateful? | Responsibility |
|---|---|---|
| `gateway` | Session only | TLS + WebSocket termination, ticket redemption, packet routing to world/social. Horizontally scalable, any client to any gateway. |
| `world` | **Yes** | Hosts room instances. Runs the tick loop. Owns live character state for players in its rooms. |
| `social` | No (Redis-backed) | Chat fanout, party, guild, friends, presence. |
| `auth` | No | OIDC relying party, allowlist enforcement, session and ticket issuance. HTTP only, not on the game path. |
| `directory` | No (Redis-backed) | Room registry (which node hosts which instance), character ownership leases, instance spawning decisions. |

### Hobby scale (today)

```
                    browser (PixiJS)
                          │ WebSocket
              ┌───────────▼────────────┐
              │   ./mmo --roles=all    │
              │                        │
              │  gateway ─┐            │
              │  world  ──┼─ in-proc   │
              │  social ──┤   channel  │
              │  auth   ──┤    bus     │
              │  directory┘            │
              └────┬──────────────┬────┘
                   │              │
              ┌────▼────┐   ┌─────▼────┐
              │ Postgres│   │  Redis   │
              └─────────┘   └──────────┘
```

### Scaled out (later, same code)

```
      browser        browser        browser
         └──────────────┼──────────────┘
                   ┌────▼────┐
                   │   LB    │
              ┌────┴────┬────┴────┐
          gateway-1  gateway-2  gateway-3     (stateless)
              └────┬────┴────┬────┘
                   │  NATS   │                (bus)
         ┌─────────┼─────────┼─────────┐
      world-1   world-2   world-3   social-1  (world: stateful)
         └─────────┴────┬────┴─────────┘
                  ┌─────┴─────┐
              Postgres      Redis
                            (directory, leases, presence)
```

Nothing in the game logic changes between these two diagrams. Only the bus and directory implementations differ.

## The two swappable seams

Everything above rests on exactly two interfaces. Keeping the seam count at two is deliberate — each additional abstraction is a tax paid every day for a scaling event that may never come.

### 1. Bus — all cross-room, cross-node messaging

```go
type Bus interface {
    Publish(ctx context.Context, subject string, msg proto.Message) error
    Subscribe(ctx context.Context, subject string, fn Handler) (Subscription, error)
    Request(ctx context.Context, subject string, msg proto.Message, out proto.Message) error
}
```

- **`inproc`** — Go channels with a subject-tree router. Zero dependencies, used at hobby scale.
- **`nats`** — NATS, used when roles are split across nodes.

Subjects are hierarchical and encode routing: `room.{instanceID}.input`, `chat.guild.{guildID}`, `party.{partyID}.event`, `char.{charID}.transfer`.

If a message is not addressable by a subject, it is a design smell — it usually means two components are sharing state they should not.

### 2. Directory — who owns what, and where does it live

```go
type Directory interface {
    // Room placement
    LookupRoom(ctx context.Context, key RoomKey) (NodeID, InstanceID, error)
    PlaceRoom(ctx context.Context, key RoomKey) (NodeID, InstanceID, error)
    ReleaseRoom(ctx context.Context, id InstanceID) error

    // Character ownership: exactly one node may mutate a character at a time
    AcquireLease(ctx context.Context, charID CharID, node NodeID) (Lease, error)
    RenewLease(ctx context.Context, l Lease) (Lease, error)
    ReleaseLease(ctx context.Context, l Lease) error
}
```

- **`memory`** — maps plus a mutex. Hobby scale.
- **`redis`** — Redis keys with TTLs. Scaled.

The character lease is not a scaling nicety; it is the primary defence against item duplication. See `data-model.md`.

## Room instances: shared vs. private

Public hunting zones and instanced dungeons are the **same primitive** with different placement policies. This answers the "instanced vs public?" question directly: build one thing, configure it two ways.

```go
type InstancePolicy string

const (
    // Public hunting zone. Auto-scaled "channels", MapleStory-style.
    // Join the least-full instance under capacity; spawn a new one when all are full.
    // Independent respawn timer per spawn point, shared by everyone inside.
    PolicyShared InstancePolicy = "shared"

    // Dungeon or boss room. Keyed to a party (or a character, for solo).
    // Fresh spawn state on creation, torn down after a TTL once empty.
    PolicyPrivate InstancePolicy = "private"
)
```

`RoomKey` is `(mapID, policy, ownerKey)` where `ownerKey` is empty for shared rooms and the party/character ID for private ones. The directory maps a `RoomKey` to a live instance, spawning one if needed. Capacity, instance TTL, and respawn behaviour are per-map content data, not code.

Per-spawn-point independent timers (rather than wave-based respawns) come free from this model and match both MapleStory and OSRS behaviour: each spawn point holds its own `nextSpawnAt`, ticked in the room's spawn phase.

## The tick loop

Each room instance runs at **20 Hz (50 ms)**. Fixed rate, never variable — variable timesteps make the simulation non-deterministic and prediction impossible to match.

Why 20 Hz: MapleStory-style action combat needs sub-100ms responsiveness, which 20 Hz plus client prediction delivers comfortably. OSRS's 600 ms cadence would feel wrong for platformer combat.

OSRS-style secondary skills (woodcutting, fishing, mining) run on a derived **action tick** every 12 sim ticks = 600 ms, deliberately matching OSRS. Gathering resolves on that slower beat while movement and combat stay at 20 Hz.

### Phases, in order

```
tick(n):
  1. ingest    drain per-player input queues; validate sequence numbers
  2. movement  apply intents; platformer physics (gravity, AABB, ropes, ladders)
  3. abilities validate casts (cooldown, cost, range, LoS); schedule effects
  4. ai        mob behaviour state machines, aggro, pathing
  5. effects   resolve damage/heal/buff; ALL RNG rolled here, server-side
  6. rewards   deaths, drop rolls, exp award, drop/corpse lifetimes
  7. spawns    per-spawn-point respawn timers, zone event triggers
  8. snapshot  build AOI-filtered delta per player; enqueue to gateway
  9. persist   mark dirty; flush on the checkpoint interval
```

Phase ordering is part of the game's observable behaviour (it decides whether a mob that dies on tick *n* can still land the hit it queued on tick *n*). Changing the order changes the game. Treat it as a spec, not an implementation detail.

### Tick budget

The 50 ms budget is a hard SLO. Instrument `room_tick_duration_seconds` as a histogram per map and alert on p99 > 25 ms (50% of budget). A room that consistently overruns must be split or have its capacity lowered — it cannot be fixed by adding nodes.

### Determinism and replay

Each room owns a seeded PRNG advanced only inside the tick loop. Combined with the recorded input log, this makes a room **fully replayable**: `(seed, input log) → identical tick-by-tick outcome`.

This is cheap to build early and disproportionately valuable — it turns "a boss desynced last Tuesday" and "did this player cheat" from unanswerable questions into a replay. It also gives the simulation a regression-test harness for free.

Consequences: no wall-clock reads inside the sim (tick number is the only clock), no map iteration without sorted keys, no goroutines inside a tick.

## Room handoff

Moving a character between rooms is the trickiest correctness-critical path, because it is where the single-writer invariant is most easily broken. The protocol is identical whether the target room is local or on another node:

```
1. source room   → freeze character, stop applying its input
2. source room   → serialize character state, persist checkpoint to Postgres
3. source node   → directory.LookupRoom / PlaceRoom for the destination
4. source node   → publish char.{id}.transfer with state + fencing token
5. target node   → acquire lease (fencing token must be > last seen)
6. target room   → deserialize, insert into room, resume input
7. target node   → ack; source releases lease and drops character
8. gateway       → repoint the player's packet routing to the new instance
```

If step 5 or 6 fails, the source retains the lease and returns the player to a safe point. The character is never live in two rooms; a crash mid-transfer leaves it recoverable from the step-2 checkpoint, at worst losing the transfer.

## Client/server split

The client is deliberately thin:

- **Sends intents only** — "I am holding right", "I cast skill 7 aimed left", "I want to pick up drop 42". Never "I dealt 300 damage" or "I moved to x=412".
- **Renders authoritative snapshots**, interpolating other entities ~100 ms in the past.
- **Predicts its own movement locally** and reconciles against the server.

Prediction requires the movement math to run identically in both places. Rather than maintaining two implementations, `internal/world/sim` is written as a **dependency-free, allocation-light, goroutine-free Go package** and compiled to WebAssembly via TinyGo for the browser. The client runs literally the same code as the server.

That constraint is why `sim` must stay pure: no goroutines, no reflection, no stdlib beyond the basics, no I/O, no clock. It is worth the discipline — physics drift between client and server manifests as rubber-banding that is miserable to diagnose.

A **golden-vector test corpus** (`(initial state, input sequence) → expected positions`, generated from the Go implementation, replayed in CI) backstops this and would also catch drift if the WASM path is ever abandoned for a hand-port.

Protocol details, snapshot format, and reconciliation are specified in `protocol.md`.

## Content is data, not code

Every item, mob, skill, passive node, drop table, map, and exp curve is a declarative, schema-validated file loaded at boot. No content requires a Go change or a redeploy of logic.

This is not tidiness — it is the difference between shipping 10 skills and shipping 300. Skills in particular are defined through a composable **effect DSL** rather than Go functions, which is what makes PoE-style support-gem interactions tractable. See `content-pipeline.md`.

Server and client validate a shared content hash at handshake and refuse to connect on mismatch.

## Storage

- **Postgres** — durable truth for accounts, characters, items, inventories, skills, guilds, and the allowlist. It is a *checkpoint store*, not the live simulation state.
- **Redis** — presence, session cache, character leases, room directory, cross-node pub/sub at scale. Everything in Redis is reconstructible; losing it costs a disconnect, not data.

Live character state resides in memory on the owning world node and is checkpointed to Postgres on an interval, on room handoff, and on logout. See `data-model.md` for the single-writer invariant and anti-duplication rules.

## Security posture

- The client is untrusted. Every action is validated server-side against cooldowns, costs, ranges, line of sight, and inventory contents.
- Client asset and code exposure is irrelevant to game integrity by design — knowing the damage formula does not let you deal more damage.
- OIDC Authorization Code + PKCE; the game server is a relying party, not an identity provider. Allowlist gates both account creation and login.
- The WebSocket handshake redeems a **single-use, 30-second ticket** obtained over authenticated HTTP, so long-lived credentials never appear in a WebSocket URL or query string.
- Per-connection and per-channel rate limits on input and chat.

## Observability

Non-negotiable from M0, because a simulation you cannot see inside is one you cannot tune:

- **Prometheus**: tick duration histogram (per map), players per instance, instances per node, snapshot bytes/sec, input packets/sec, DB checkpoint latency, lease acquisition failures, bus publish latency.
- **Grafana**: dashboards checked into `deploy/grafana/`.
- **Structured logs** via `log/slog`, with instance ID and tick number on every simulation log line.
- **pprof** exposed on the admin port.
- Room replay (§ Determinism) as the deep-debugging tool of last resort.

## Repository layout

```
cmd/mmo/                  single binary, --roles selects behaviour
internal/
  gateway/                WS termination, session, packet routing
  world/
    room/                 instance lifecycle, tick loop
    sim/                  PURE deterministic simulation (→ TinyGo WASM)
  social/                 chat, party, guild, presence
  auth/                   OIDC RP, allowlist, sessions, tickets
  directory/              room registry + character leases (memory | redis)
  bus/                    Bus interface (inproc | nats)
  store/                  Postgres repositories + migrations
  content/                loaders, schema validation, content hashing
proto/                    protobuf wire definitions
client/                   TypeScript + PixiJS + Vite
  src/net/                transport, snapshot buffer, reconciliation
  src/sim/                WASM bridge to internal/world/sim
  src/render/             Pixi scene, tilemap, sprites, paperdoll
  src/ui/                 DOM/React UI (inventory, skill tree, chat, ...)
content/                  game data (items, mobs, skills, maps, tables)
deploy/                   docker-compose, grafana dashboards, k8s (later)
docs/
```

## Decisions deliberately deferred

Recorded so they are not re-litigated, and so the cost of changing course later is known:

| Decision | Deferred until | Why it is safe to defer |
|---|---|---|
| NATS bus | M9 | `Bus` interface makes it a swap. |
| Redis directory | M9 | `Directory` interface makes it a swap. |
| Kubernetes | M9 | Roles already split by flag; compose works until then. |
| Grid-based AOI | Large open maps | Snapshot builder takes an AOI filter; whole-room is the trivial filter. |
| Sharded Postgres | >>1000 CCU | Rooms are the hot path; DB is checkpoint-rate, not tick-rate. |
| Native client wrapper | Post-launch | Tauri/Electron wrap of the same web client. |
