# Roadmap

## The discipline

Build **vertically, not horizontally.** A thin slice that runs end-to-end — login through simulation through persistence — beats a complete inventory system with nothing to put in it.

This matters most for netcode. Prediction, reconciliation, interpolation, and room handoff are the parts that are painful to retrofit and cheap to build first. Content is the opposite: it is endless, it is the fun part, and it will absorb unlimited time. **Netcode first, content later**, or the netcode never gets built properly.

Every milestone ends in something playable. If a milestone cannot be demonstrated by playing it, it is scoped wrong.

---

## M0 — Foundation and movement — **done**

*The slice that proves the architecture.*

- [x] Go module, single `cmd/mmo` binary with `--roles`
- [x] `docker compose up`: Postgres, Redis, server (Postgres and Redis are staged for M2, not yet used)
- [x] Protobuf schema + Go/TS codegen wired into the build, via `buf` from `tools/go.mod` — no system `protoc`
- [x] `Bus` interface + `inproc` implementation
- [x] `Directory` interface + `memory` implementation
- [x] WebSocket gateway, envelope batching, single-use tickets
- [x] Room instance with the 20 Hz tick loop and its phase order
- [x] `LayerID` on every entity and a layer-aware visibility test
- [x] `internal/world/sim`: fixed-point platformer physics — gravity, AABB with substepping, one-way platforms, drop-through, ropes, coyote time, jump buffering, variable jump height
- [x] WASM build of `sim`, loaded by the client
- [x] PixiJS client: map rendering, camera, entity sprites, diagnostic HUD
- [x] Client prediction + server reconciliation + entity interpolation
- [x] Prometheus metrics: tick duration histogram, players and entities per instance
- [x] Golden-vector corpus, replayed in CI against **both** the Go and WASM builds

**Exit criteria, and how each was verified:**

| Criterion | Status |
|---|---|
| Two clients in one map, each seeing the other | Verified — Go integration tests over real sockets, and two browser tabs (`others 1`, name label rendered) |
| Prediction matches the server | Verified — 12 vectors, 640 frames, bit-identical between the Go and WASM builds |
| Tick cost observable | Verified — `mmo_room_tick_duration_seconds` histogram, bucketed around the 50 ms budget |
| Movement feels good | **Not yet judged.** Needs a human at a foreground browser; see below. |

**Still open before M1.** Movement *feel* is the one exit criterion that cannot be automated, and it is the one that matters most — everything after this is built on top of it, and mushy movement is not something you patch later. `DefaultTuning` in `internal/world/sim/world.go` is a considered first guess (96-unit jump, 8 units/tick run speed), not a tuned one. Play it, then adjust; the golden corpus will show exactly what changed.

TinyGo is also still unpinned: the WASM module is 1.9 MB from the stock Go toolchain where TinyGo would produce roughly 50 KB. The `sim` package was written to TinyGo's constraints — no goroutines, no reflection, no allocation — so this is a build change, not a rewrite.

---

## M1 — Combat — **done**

- [x] Mobs loaded from content files; spawn points with independent respawn timers
- [x] **Per-layer mob instancing**: `owner` spawn points instantiate per player, each layer with its own spawn timers and its own RNG stream; `shared` spawn points are common to the room
- [x] Ground drops inherit the layer of the mob that dropped them
- [x] AI state machine: idle → aggro → chase → attack → leash → dead
- [x] One melee skill with cooldown, cost, and a bounded hitbox
- [x] Damage pipeline end to end, including the armour curve
- [x] Death, respawn, exp award, level-up (including multi-level from one kill)
- [x] Gold and item drops on the ground, with pickup
- [x] Floating damage numbers, HP bars, hit and death feedback
- [x] Seeded per-room PRNG; determinism verified across scripted runs

**Exit criteria, and how each was verified:**

| Criterion | Status |
|---|---|
| Kill a mob, gain exp, level up, pick up the drop | Verified — end to end over a real socket |
| Two players see each other but hunt independent mobs | Verified — over the wire, including that the shared-layer boss reaches both |
| Cannot touch another player's mobs or loot | Verified — and this caught a real bug (see below) |
| Deterministic from a seed | Verified — 120 scripted ticks compared entity by entity across repeated runs |

**Not yet judged:** combat *feel* — swing timing, hit reaction, whether the
damage numbers read at speed. Same caveat as M0's movement feel: it needs a
human at a foreground browser, and the numbers in `content/` are a considered
first guess rather than a tuned one.

**Deferred to where it belongs.** Critical hits are threaded through the damage
pipeline and the wire format but never fire, because nothing grants crit chance
until equipment lands in M3. Item drops are acknowledged and consumed rather
than stored, for the same reason — there is no inventory yet. Player death
restores in place; a real death penalty needs persistence (M2).

---

---

## M2 — Identity and persistence — **done**

- [x] OIDC relying party: Authorization Code + PKCE, providers from config
- [x] Allowlist enforcement on account creation *and* login
- [x] Session JWT + rotating refresh cookie; single-use 30 s WebSocket tickets
- [x] Postgres schema and forward-only migrations, under an advisory lock
- [x] Character create / select / delete, with a per-account limit
- [x] Character lease with fencing tokens, end to end
- [x] Checkpoint on interval, on disconnect, and on logout
- [x] 60-second reconnect grace period

**Exit criteria, and how each was verified:**

| Criterion | Status |
|---|---|
| Play, log out, log back in, everything is where you left it | Verified over a real socket and a real database |
| A second process cannot load the same character | Verified — a second session is refused with a typed close code |
| Sign in with a real identity provider | **Flow verified with development login only.** The OIDC path is implemented and unit-tested, but has not been run against a live provider. |

**Not yet exercised against a real IdP.** Discovery, PKCE, and ID-token
verification are implemented and tested in isolation; nobody has yet pointed
this at Google or Keycloak and signed in. `deploy/providers.example.toml` has
the shape. Expect the first real attempt to turn up a redirect-URI mismatch,
which is the usual way this goes wrong.

**Deferred deliberately.** Write-through for high-value events (a boss drop, a
trade) is documented but not yet split out from the periodic checkpoint,
because there is nothing valuable enough to lose until inventories exist in M3.
Character deletion is soft, freeing the name, and nothing purges old rows yet.

---

---

## M3 — Items, equipment, stats — **done**

- [x] Item bases, affix pools, tiers, rarity rolls
- [x] Item instances with rolled affixes; the exactly-one-location invariant
- [x] Item journal (`item_events`), built before the first dupe rather than after
- [x] Inventory with slots and stacking
- [x] Equipment slots with level requirements
- [x] Full stat pipeline: base → flat → increased (summed) → more (multiplied)
- [x] Tooltips with affix tiers and a predicted stat change
- [x] Critical hits, now that equipment can grant the chance
- [ ] Vendors, buy/sell — deferred, see below
- [ ] Paperdoll rendering — deferred, see below

**Exit criterion, verified in a browser:** a rare Leather Vest with three
affixes predicted `+13 Armour, +3 Strength, +18 Life, +15% Fire Resistance`;
equipping it changed those stats by exactly `+13, +3, +18, +15%`.

**Deferred deliberately.** Vendors need a gold sink worth having and an economy
to balance against, neither of which exists yet; buying and selling into a
vacuum would be numbers with no meaning. Paperdoll rendering needs sprites, and
the client draws flat colour boxes — layered equipment on a rectangle would be
work thrown away when art arrives. Drag-and-drop is likewise deferred: click to
equip covers the actual need, and dragging matters once bank tabs and trade
windows exist to drag between.

**Also deferred:** stacking is modelled and stored but nothing yet merges two
stacks of the same item into one, so twenty potions occupy twenty slots.

---

---

## M4 — A world of rooms

*Where the scaling design gets exercised for real.*

- Room lifecycle: create, populate, idle, tear down
- Portals and the full handoff protocol — **including the local case, which must not be special-cased**
- `shared` policy: auto-scaled channels, join least-full, capacity limits
- `private` policy: party-keyed instances with TTL
- Layer key follows party membership: partying merges mob views, leaving splits them
- Proximity spawning and tiered AI ticking — the two mitigations that keep layering inside the tick budget
- World map UI, waypoint unlock and teleport
- Multiple maps with level ranges
- Simulated multi-node in tests: two world roles in one process, forced to communicate over the bus

**Exit:** walk through a portal into a different map hosted by a different world role, with no visible interruption. Channel switching works. A party gets its own dungeon instance, and partying up in a hunting zone merges the members' mob layers.

---

## M5 — Social

- Chat: global, local (room), whisper, party, guild — with rate limits and mutes
- Party: invite, join, leave, kick, leader transfer, member frames
- Party exp sharing by radius, and configurable loot rules
- Guild: create, roster, ranks and permissions, MOTD, guild chat
- Friends and presence
- Cross-room delivery entirely over the bus (no direct room-to-room calls)

**Exit:** six players in a party across two rooms, chatting on every channel, sharing exp and loot correctly.

---

## M6 — Skill trees and classes

- Effect DSL interpreter with the full effect vocabulary
- Active skills with ranks, cooldowns, resource costs
- Buff and debuff system, stacking rules, DoTs, auras
- Passive tree: allocation, connectivity validation, respec
- A passive tree editor tool (generates `tree.json`)
- Three classes with distinct starting positions and mechanics
- Support modifiers attaching to skills by tag
- Skill bar, cooldown UI, buff bar

**Exit:** two characters of the same class with different trees play measurably differently. A support modifier attaches to a skill it was never explicitly written for, and works.

---

## M7 — Dungeons and bossing

- Instanced dungeon flow: entry requirements, lockouts, progression, completion
- Boss AI with phases, telegraphed attacks, enrage
- Team-play mechanics that require coordination — shared shields, combo triggers, mechanics that cannot be soloed
- Enhanced mobs: champion and rare tiers rolling elite modifiers
- Zone events: interactive triggers, timed spawns, mini-bosses
- Boss health UI, telegraph rendering, wipe and recovery flow

**Exit:** a six-player party clears a dungeon and kills a boss whose mechanics genuinely require coordination.

---

## M8 — Secondary skills

- Resource nodes on maps with independent respawn timers
- The 600 ms action tick, layered on the 20 Hz sim tick
- Gathering: woodcutting, mining, fishing, herbalism
- Processing: smithing, cooking, alchemy
- OSRS exp curve, 1–99 per skill, levelling from use
- Tool tiers and level gating
- Skills panel

**Exit:** chop trees for twenty minutes, gain levels, smith what you gathered into something you can equip.

---

## M9 — Scale out

*Only now, and only because the seams were built in M0.*

- `nats` bus implementation
- `redis` directory implementation
- Split roles into separate deployments; k8s manifests
- Headless bot client for load generation
- Grafana dashboards checked in
- Load test: 1000 bots across 3 world nodes
- Chaos testing: kill a world node, verify leases expire and characters recover
- Graceful drain on shutdown: hand off rooms, checkpoint, disconnect cleanly

**Exit:** 1000 concurrent bots across three world nodes, tick p99 within budget, and killing a node loses at most one checkpoint interval for its players.

---

## Sequencing notes

**M0 is the risky one.** Prediction and reconciliation are the hardest things here and everything else assumes they work. Budget generously and do not move on while movement feels wrong.

**M4 is the one that validates the whole architecture.** If room handoff works cleanly over the bus with roles split in-process, M9 is configuration. If M4 gets special-cased for the local path, M9 is a rewrite. This is the milestone to be strict on.

**M3's item journal is not optional.** It is a few hours of work during M3 and it is the difference between "we rolled back the dupe" and "we wiped the economy."

**M6 is where the game becomes itself.** Everything before it is infrastructure; the skill tree is what makes builds interesting. It is placed after items and social deliberately — a skill tree with no gear to support it and nobody to play with cannot be evaluated.

**Content authoring runs in parallel from M3 onward** and never stops. It is not a milestone because it has no end.

## Explicitly out of scope for now

Recorded so they are decisions rather than oversights:

- PvP and any form of arena or ranking
- Trading post / auction house (direct trade only until the economy justifies it)
- Quests and dialogue trees
- Housing, pets, mounts, cosmetics
- Mobile client
- Anything monetised
