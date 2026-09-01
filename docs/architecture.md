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

## Everything that leaves a room

Chat, parties, guilds, and presence are the first things that genuinely cross room and node boundaries in normal play, and they settle the shape everything after them follows. Two shapes, chosen per case rather than one applied everywhere:

**A subject, and every interested node subscribed to it.** `chat.global` for everyone; `party.{id}.*` for one party; `guild.{id}.*` for one guild. The publisher does not know or care who is listening, which is what makes the NATS implementation a direct mapping rather than a translation. Subscriptions are refcounted per node and come and go as members log in, log out, and move between nodes.

**An address, resolved through presence.** A whisper names one character, so it takes a lookup to find the node holding them and is published to that node alone. Broadcasting it everywhere so that one node keeps it would spread private messages across the cluster.

The difference is not an inconsistency: a whisper genuinely has one recipient, and a party message genuinely has an audience that changes without the sender knowing.

Local chat is the exception that proves the rule. It never touches the bus at all, because everyone who can hear it is already in the room — sending it anywhere else would be routing a message out of the process and back in.

### What travels, and what does not

Parties carry their whole roster in every update; guilds carry only a notice and let each node reload. That is a size decision, not an inconsistency: a party is at most six members and the delta would be most of the message anyway, while a guild can be hundreds of rows that every node already has a database connection to read.

Carrying the whole roster is also what keeps the party's hot path off the coordination store. Vitals arrive once a second per member, and each one re-renders the roster; a session renders from the update it was last given rather than re-reading the party, because the update already carried everything the render needs. For a full party that is the difference between thirty-six reads a second and none.

Nothing a *live session* holds can travel at all — the socket, and the channel a room reports portals and loot on, are in-process references to the node the player is connected to. That is why a room handoff hands them over separately (§ Room handoff) rather than packing them into the transfer.

### Presence is the third seam

`Presence` answers "who is online, and which node holds them". It is deliberately ephemeral: losing it costs everyone their friends list for a moment and nothing else, because the characters are still in their rooms, still leased, still checkpointing. That is what makes Redis its right home and Postgres its wrong one — the same argument that puts parties in one place and guilds in the other.

It now has that home: `RedisPresence` keeps two indexes — characters by ID, IDs by normalised name — updated together by Lua, because two writes from the client leave a window in which a renamed character is reachable under both names or neither. Records carry the node holding the session, and a node clears its own on startup rather than every character heartbeating a TTL; a node that dies and never returns leaves its characters listed until something sweeps them, which is a chaos-testing concern rather than a presence one.

The normalised name is *stored in the record*, not derived in Lua. `string.lower` is ASCII-only and does not trim, so a script computing the lookup key would disagree with `NormaliseName` for any name with an accent — and a disagreement leaves a name pointing at a character forever. One definition of the key, in Go.

Presence reads report errors, for the reason the directory's do: an unreachable Redis answering "nobody is online by that name" is not a degraded answer but a wrong one, and a caller acts on it by refusing a whisper to somebody who is standing right there.

### Parties are the fourth

`Parties` owns membership, which spans rooms and nodes by definition — that is most of what makes a party worth having, and it is why the state was never going to survive on one node's heap. Same argument as presence, same conclusion: ephemeral, losable, Redis rather than Postgres.

What is different is that every method has a rule attached, and every rule is a check followed by a write. `RedisParties` makes each one a single Lua script, because a read of the roster followed by a write of it leaves exactly enough room between them for two people to take the last slot, or for somebody demoted in between to kick anyway. Atomicity here is not an optimisation; it is the only thing that makes the guards mean anything.

Invitations are keys with a TTL rather than fields with a stored deadline, so Redis does the expiring and nothing lingers for somebody who was asked once and never came back. One invitation per character: a second replaces the first.

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

- **`inproc`** — Go channels with a subject-tree router. Zero dependencies, and the *correct* implementation when every role runs in one process rather than a stand-in for one.
- **`nats`** — NATS, used when roles are split across nodes. Selected with `--nats-url`; unset keeps the bus in-process.

Both are held to one contract by a conformance suite that runs every behaviour a
caller can rely on against both implementations. Two suites would drift, and the
drift would be invisible until the day roles are split and a subject that worked
in one process stopped working.

The one place they genuinely differ is request/reply. `inproc` has no notion of a
reply, so it builds one from publish/subscribe — a private inbox subject and a
correlation id in the envelope. NATS has request/reply natively, including a
server-side answer to "is anybody listening", so it uses that. Observable
behaviour is identical, which is what the suite asserts.

**A NATS subscription is not live until it is flushed**, and that is worth
knowing because it is not obvious: a subscribe is a protocol message like any
other, so a caller that subscribes and then publishes can miss its own message.
`Subscribe` flushes, so the postcondition every caller already assumes is
actually true. `Publish` deliberately does not — a flush is a round trip, and one
per message would turn the bus into a bottleneck at the rate rooms publish.

Subjects are hierarchical and encode routing: `room.{instanceID}.input`, `chat.guild.{guildID}`, `party.{partyID}.event`, `char.{charID}.transfer`.

If a message is not addressable by a subject, it is a design smell — it usually means two components are sharing state they should not.

### 2. Directory — who owns what, and where does it live

Two implementations, held to one contract by a conformance suite: `Memory`, which
is the *correct* implementation when every role runs in one process, and `Redis`,
selected with `--redis-addr`. Unlike the bus, this one is not optional at scale:
two processes with in-memory directories do not disagree about placement, they
are unaware of each other's rooms entirely.

Placement and reservation are one atomic step, which in Redis means a Lua
script. Choosing an instance and then incrementing its count separately lets two
simultaneous joins take the same last slot, and lets a private key end up with
two instances — two members of one party, two dungeons.

Nodes register with a **TTL heartbeat**: one that stops beating stops receiving
new rooms. The registration is kept when the liveness lapses, because its score
is what keeps placement order stable across a restart.

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

## Rooms have two independent axes

Two questions that look like one question, and conflating them is a design error:

1. **Who may enter this room?** — placement policy
2. **Which entities inside it can a given player see and hit?** — layering

A hunting zone wants a crowd of visible players but no competition for mobs. A dungeon wants a private group all fighting the same boss. Those are different answers on different axes.

### Axis 1: placement policy — who enters

```go
type Placement string

const (
    // Public zone. Auto-scaled "channels", MapleStory-style: join the
    // least-full instance under capacity, spawn a new one when all are full.
    PlacementShared Placement = "shared"

    // Dungeon or boss room. Keyed to a party (or a character, solo).
    // Fresh state on creation, torn down after a TTL once empty.
    PlacementPrivate Placement = "private"
)
```

`RoomKey` is `(mapID, placement, ownerKey)` — `ownerKey` empty for shared rooms, the party or character ID for private ones. The directory resolves a `RoomKey` to a live instance, spawning one if needed. Capacity and instance TTL are per-map content, not code.

### Axis 2: layering — who sees which entities

Every entity in a room carries a `LayerID`. A player sees an entity if it is in the **shared layer** or in **their own layer**.

```go
type LayerID uint32   // 0 == shared, visible to everyone in the room

func (r *Room) Visible(viewer, e EntityID) bool {
    l := r.layerOf(e)
    return l == SharedLayer || l == r.layerOf(viewer)
}
```

A player's layer key is **their party ID if partied, otherwise their character ID.** Partying up merges views: your mobs are replaced by your party's. That transition is visible — mobs despawn and a fresh set appears — which is expected behaviour, not a bug, and worth a short fade in the client.

**Players are always in the shared layer.** You always see everyone in the room, chat with them, trade with them, and party with them. Only *hostile and lootable* entities are layered.

### The layer key is the party

A layer is keyed by party ID while partied and by character ID otherwise, so partying up merges the members' mob populations and leaving splits them again. It is the same code path with a different key, not a mode.

Ground loot follows a player when their layer changes; mobs do not. The destination has its own population and moving them across would double it, but losing a drop because a friend sent an invitation is the kind of thing players remember.

The layer is also what answers "who helped with this kill", without damage attribution or tap rules: everyone in the killer's layer, within the exp-share radius, earns a share. A party member who fast-travelled away mid-fight did not help, and a bystander in their own layer standing on the corpse did not either.

Loot rules ride along with the key, because they are a property of the same thing — who shares this layer decides who may pick up what it drops. Free-for-all is the default and needs no rules to explain; round-robin assigns each drop to a member in turn and reserves it for them briefly, cycling only through members actually present, since a drop assigned to somebody in another room is loot nobody can reach.

### Layering is declared per spawn point, not per room

This is the flexibility that makes the model worth having. Each spawn point in the map data declares its layer:

```toml
# in a map's object layer, per spawn_point
layer = "owner"    # per-player/per-party — no contention
layer = "shared"   # everyone in the room fights this one
```

Which composes into the cases that actually matter:

| Scenario | Placement | Spawn layer | Result |
|---|---|---|---|
| Hunting zone | `shared` | `owner` | See everyone, hunt your own mobs, zero contention |
| Dungeon / boss | `private` | `shared` | Your party only, all fighting the same boss |
| World boss in a field | `shared` | `shared` for the boss, `owner` for trash | Private grinding, public raid target in the same map |
| Town | `shared` | — | No mobs at all |
| Solo story instance | `private` | `owner` | Fully isolated |

That third row is the one that pays for the design: a hunting zone where trash is private but a field boss is a public rally point, with no special-casing anywhere.

### What layering gives you for free

- **No spawn contention.** Each layer holds its own copy of every `owner` spawn point's state, with its own independent `nextSpawnAt`. Nobody steals your spawns.
- **No loot stealing.** Ground drops inherit the layer of the mob that dropped them. Cross-layer looting is not prevented by a rule; it is unrepresentable.
- **No kill stealing**, and no need for tap/damage-attribution rules on `owner` mobs.

### What layering costs — and it is the real constraint

Simulation cost scales with **layers × mobs**, not mobs. Forty players in forty layers with sixty spawn points each is 2,400 mob entities in a room that would otherwise hold sixty. That is a direct threat to the 50 ms tick budget, and it is the main thing to watch in this design.

Three mitigations, all in place as of M4:

1. **Proximity spawning.** A spawn point only produces mobs while someone who could see them is within `rooms.spawn_activation_range` (800 units, wider than half a viewport so nothing appears on screen out of nothing). The other half is the cull: mobs that are idle or walking home, with nobody near, are removed on a once-a-second sweep. Without the cull the saving only ever applies to ground a player has not walked yet — cross a map once and every spawn point on it stays populated for as long as the room lives. A culled spawn point clears its respawn timer, so returning finds the area as it was rather than empty for an interval.
2. **Tiered AI ticking.** A mob with no aggro runs its behaviour on a slower beat set per mob (`idle_tick_interval`), staggered by entity ID so a room full of mobs does not run every behaviour on the same tick. Most mobs in most layers are idle, so this is the largest win available, and it is invisible in play because an idle mob has nothing to decide.
3. **Lower room capacity.** Since players no longer compete for spawns, room capacity is purely a social and rendering concern. Cap `shared` hunting rooms around 20–30 and let channels absorb the rest — which is what MapleStory does anyway.

Mobs that are chasing or attacking are never culled: they are by definition next to a player, and one that has just been hit and is walking home must not evaporate while its attacker watches.

`room_entities_total{layer_kind}` and the tick histogram are the metrics that tell you when a map's spawn density is too high for its capacity. Expect to tune per-map capacity down from the naive value.

### The trade-off worth naming

Per-player mobs remove the worst parts of a shared open world — spawn camping, kill stealing, loot sniping, and the new-player experience of arriving at a zone where everything is already dead. That is why modern MMOs do it.

It also removes a real incentive to party in open zones: players group for the exp bonus and for company, not to share a scarce resource. The world feels somewhat more like "single-player with chat" while grinding.

`shared`-layer content is the counterweight, and it should be used deliberately: field bosses, zone events, and rare spawns that pull everyone in a room onto the same target. The design supports mixing them into an otherwise-`owner` map at no cost, so use it.

## The tick loop

Each room instance runs at **20 Hz (50 ms)**. Fixed rate, never variable — variable timesteps make the simulation non-deterministic and prediction impossible to match.

Why 20 Hz: MapleStory-style action combat needs sub-100ms responsiveness, which 20 Hz plus client prediction delivers comfortably. OSRS's 600 ms cadence would feel wrong for platformer combat.

OSRS-style secondary skills (woodcutting, fishing, mining) run on a derived **action tick** every 12 sim ticks = 600 ms, deliberately matching OSRS. Gathering resolves on that slower beat while movement and combat stay at 20 Hz.

### Phases, in order

```
tick(n):
   1. ingest+move   drain input queues, validate sequence numbers, run
                    platformer physics (gravity, AABB, ropes, ladders)
   2. portals       against final positions, before anything can hit: a player
                    standing in a portal is leaving
   3. buffs         damage-over-time and expiry
   4. spawned       projectiles and ground effects, before anything acts -- a
                    bolt that kills something this tick stops it acting
   5. casts         validate (cooldown, cost, range); resolve effects. ALL RNG
                    is rolled server-side, here and in the phases below
   6. ai            mob state machines, aggro, pathing
   7. telegraphs    after AI, so a marker put down this tick is in this tick's
                    snapshot
   8. revive        after everything that could kill, so somebody who came back
                    cannot be downed by a hit that landed before they were up
   9. regen         after revive, so they recover from the bar they came back
                    with
  10. dungeon       stage progression, clear, wipe
  11. events        zone events start and end
  12. actions       gathering, on the derived 600 ms action tick. After
                    everything that could interrupt it; the interruption checks
                    run every tick and the roll only on the beat
  13. drops         corpse and drop lifetimes
  14. spawns        per-spawn-point respawn timers
  15. resources     resource node respawn timers
  16. snapshot      build AOI-filtered delta per player; enqueue to gateway
```

Persistence is not a phase. A checkpoint reads a player's state *on* the room
goroutine through `Capture` and writes it from the session's, on its own
interval — a database write inside a tick is the one thing the budget below
cannot absorb.

Phase ordering is part of the game's observable behaviour (it decides whether a mob that dies on tick *n* can still land the hit it queued on tick *n*). Changing the order changes the game. Treat it as a spec, not an implementation detail.

### Tick budget

The 50 ms budget is a hard SLO. Instrument `room_tick_duration_seconds` as a histogram per map and alert on p99 > 25 ms (50% of budget). A room that consistently overruns must be split or have its capacity lowered — it cannot be fixed by adding nodes.

### Determinism and replay

Each room owns a seeded PRNG advanced only inside the tick loop. Combined with the recorded input log, this makes a room **fully replayable**: `(seed, input log) → identical tick-by-tick outcome`.

This is cheap to build early and disproportionately valuable — it turns "a boss desynced last Tuesday" and "did this player cheat" from unanswerable questions into a replay. It also gives the simulation a regression-test harness for free.

Consequences: no wall-clock reads inside the sim (tick number is the only clock), no map iteration without sorted keys, no goroutines inside a tick.

## Room handoff

Moving a character between rooms is the trickiest correctness-critical path, because it is where the single-writer invariant is most easily broken. The protocol is identical whether the target room is local or on another node — there is deliberately no local fast path, because the shortcut works today and means the distributed path is never exercised until the day it has to.

```
1. source room   → freeze the character, stop applying its input
2. source room   → capture state; checkpoint to Postgres, fenced by the lease
3. source node   → directory places the character: which instance, which node
4. source node   → request world.node.{id}.transfer with state + fencing token
5. target node   → refuse a token older than one already accepted
6. target room   → deserialize, insert frozen, at the named spawn or waypoint
7. target node   → reply with the new entity id
8. source node   → swap the session's handle, leave the source room, release its slot
9. source node   → attach the session to the new room, which Welcomes the client
```

Steps 1–7 are all reversible: a failure at any of them leaves the character exactly where it was, unfrozen and playable, which is why the source is not torn down first. A crash mid-transfer leaves the character recoverable from the step-2 checkpoint, at worst losing the transfer. The character is never live in two rooms.

Step 9 is not an afterthought. Everything a live session holds — the socket, and the channel the room reports portals, waypoints, and loot on — is an in-process reference to the node the *player* is connected to, not the one hosting the room, so none of it can travel in step 4. It is handed over separately, as a `room.Attachment`, once the destination has accepted. A destination that never receives one holds a character who can move and do nothing else.

The same protocol carries every way of moving through the world. A portal names a spawn point; fast travel names a waypoint, which the destination resolves from its own copy of the content rather than trusting coordinates off the wire; a channel switch names an instance and keeps the character where they stood. One handoff, three destinations.

### Room lifecycle

Rooms are created lazily, when the directory first places somebody in one, and stop on their own once they have stood empty for `rooms.idle_ms`. An empty room is not free — a goroutine, twenty wakeups a second, and every mob it has spawned — and a world of many maps is mostly empty rooms. It is not worthless either: a player who steps through a portal and straight back should find the room they left, with its mobs where they left them, so the timeout is generous rather than immediate.

Stopping is a handshake, not a decision the room makes alone. The room asks the node; the node releases the instance from the directory *first*, under the directory's own lock, and only deregisters the room if that succeeded. A refusal means somebody was placed here while the room was counting down and their join is on its way, so the room restarts its clock and keeps ticking. Releasing and deregistering in the other order leaves a window in which a player is placed into a room that stops a moment later.

## Client/server split

The client is deliberately thin:

- **Sends intents only** — "I am holding right", "I cast skill 7 aimed left", "I want to pick up drop 42". Never "I dealt 300 damage" or "I moved to x=412".
- **Renders authoritative snapshots**, interpolating other entities ~100 ms in the past.
- **Predicts its own movement locally** and reconciles against the server.

Prediction requires the movement math to run identically in both places. Rather than maintaining two implementations, `internal/world/sim` is written as a **dependency-free, allocation-light, goroutine-free Go package** and compiled to WebAssembly via TinyGo for the browser. The client runs literally the same code as the server.

That constraint is why `sim` must stay pure: no goroutines, no reflection, no stdlib beyond the basics, no I/O, no clock. It is worth the discipline — physics drift between client and server manifests as rubber-banding that is miserable to diagnose.

A **golden-vector corpus** backstops this. Each fixture in `internal/world/sim/testdata` is self-describing — it carries the world, the starting body, the input script, and the expected frame-by-frame output — so any implementation can replay it. CI runs it twice: once against the Go build (`go test ./internal/world/sim`) and once against the compiled WebAssembly under Node (`client/test/wasm-conformance.mjs`). A single differing bit fails the build, which is what makes prediction trustworthy rather than merely intended.

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
- Two ways in, both gated by the same allowlist at account creation **and** at every login, so revoking access actually revokes it.
  - **OIDC** Authorization Code + PKCE, with the game server as a relying party. No password ever reaches this system.
  - **Local accounts**, where this server holds an Argon2id hash. Requiring an external identity provider is a real barrier for a self-hosted game, so this exists — but it means custodying password hashes, which is a genuine obligation and not a free convenience. Prefer OIDC where it is available.
- An empty allowlist admits nobody. "Empty means open" fails toward a server anyone can join, discovered after the fact.
- Local sign-in is throttled per account and per source address, and an unknown username costs the same time as a wrong password — otherwise the endpoint is a username oracle.
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
| ~~NATS bus~~ | ~~M9~~ | **Done in M9, and it was a swap**: one new file behind the existing interface, and every cross-node test now runs over it unchanged. |
| ~~Redis directory~~ | ~~M9~~ | **Done in M9, and almost a swap**: one new file behind the interface, but the three read methods had to start returning errors — an in-memory directory cannot fail one and a network directory can. |
| Kubernetes | M9 | Roles already split by flag; compose works until then. |
| Grid-based AOI | Large open maps | Snapshot builder takes an AOI filter; whole-room is the trivial filter. |
| Sharded Postgres | >>1000 CCU | Rooms are the hot path; DB is checkpoint-rate, not tick-rate. |
| Native client wrapper | Post-launch | Tauri/Electron wrap of the same web client. |
