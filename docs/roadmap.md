# Roadmap

## The discipline

Build **vertically, not horizontally.** A thin slice that runs end-to-end — login through simulation through persistence — beats a complete inventory system with nothing to put in it.

This matters most for netcode. Prediction, reconciliation, interpolation, and room handoff are the parts that are painful to retrofit and cheap to build first. Content is the opposite: it is endless, it is the fun part, and it will absorb unlimited time. **Netcode first, content later**, or the netcode never gets built properly.

Every milestone ends in something playable. If a milestone cannot be demonstrated by playing it, it is scoped wrong.

---

## M0 — Foundation and movement

*The slice that proves the architecture.*

- Go module, single `cmd/mmo` binary with `--roles`
- `docker compose up`: Postgres, Redis, server, client dev server
- Protobuf schema + Go/TS codegen wired into the build
- `Bus` interface + `inproc` implementation
- `Directory` interface + `memory` implementation
- WebSocket gateway, envelope batching
- Room instance with the 20 Hz tick loop and its phase order
- `internal/world/sim`: fixed-point platformer physics — gravity, AABB, one-way platforms, ropes, ladders
- TinyGo → WASM build of `sim`, loaded by the client
- PixiJS client: Tiled map rendering, sprite, camera
- Client prediction + server reconciliation + entity interpolation
- Prometheus metrics: tick duration histogram, players per instance
- Golden-vector test corpus for `sim`, replayed in CI

**Exit:** two browser clients, one hardcoded map, running and jumping, each seeing the other move smoothly. Movement feels instant locally. Tick p99 is on a dashboard.

**Do not leave M0 until movement feels good.** Everything after this is built on top of it, and a mushy movement feel is not something you fix later — it is something you rebuild everything for.

---

## M1 — Combat

- Mobs loaded from content files; spawn points with independent respawn timers
- AI state machine: idle → aggro → chase → attack → leash → dead
- One melee skill with cooldown, cost, and a cone hitbox
- Damage pipeline end to end (`data-model.md` § Damage resolution)
- Death, respawn, exp award, level-up
- Gold and item drops on the ground, with pickup
- Floating damage numbers, HP bars, death and hit feedback
- Seeded per-room PRNG; room replay harness

**Exit:** walk to a slime, kill it, watch damage numbers, gain exp, level up, pick up the drop. Replay the room from `(seed, input log)` and get identical results.

---

## M2 — Identity and persistence

- OIDC relying party: Authorization Code + PKCE, providers from config
- Allowlist enforcement on account creation *and* login
- Session JWT + refresh cookie; single-use 30 s WebSocket tickets
- Postgres schema and forward-only migrations
- Character create / select / delete
- Character lease with fencing tokens, end to end
- Checkpoint on interval, on logout, on handoff; write-through for the things that must never be lost
- 60-second reconnect grace period

**Exit:** log in with a real identity provider, create a character, kill things, log out, log back in, everything is where you left it. A second process cannot load the same character.

---

## M3 — Items, equipment, stats

- Item bases, affix pools, tiers, rarity rolls
- Item instances with rolled affixes; the exactly-one-location invariant
- Item journal (`item_events`) — **build it now, not after the first dupe**
- Inventory with slots, drag-and-drop, stacking
- Equipment slots, level and class requirements
- Full stat pipeline: base → flat → increased (summed) → more (multiplied)
- Tooltips with affix tiers
- Vendors, buy/sell
- Paperdoll: equipped gear renders on the character

**Exit:** kill a mob, get a rare sword with three affixes, equip it, watch damage change by exactly the amount the tooltip predicted.

---

## M4 — A world of rooms

*Where the scaling design gets exercised for real.*

- Room lifecycle: create, populate, idle, tear down
- Portals and the full handoff protocol — **including the local case, which must not be special-cased**
- `shared` policy: auto-scaled channels, join least-full, capacity limits
- `private` policy: party-keyed instances with TTL
- World map UI, waypoint unlock and teleport
- Multiple maps with level ranges
- Simulated multi-node in tests: two world roles in one process, forced to communicate over the bus

**Exit:** walk through a portal into a different map hosted by a different world role, with no visible interruption. Channel switching works. A party gets its own dungeon instance.

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
