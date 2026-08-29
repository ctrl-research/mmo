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

## M4 — A world of rooms — **done**

*Where the scaling design gets exercised for real.*

- [x] Room lifecycle: create, populate, idle, tear down
- [x] Portals and the full handoff protocol — including the local case, which is not special-cased
- [x] `shared` policy: auto-scaled channels, join least-full, capacity limits
- [x] `private` policy: one instance per owner key
- [x] Proximity spawning and tiered AI ticking — the two mitigations that keep layering inside the tick budget
- [x] World map UI, waypoint unlock and teleport
- [x] Multiple maps with level ranges
- [x] Simulated multi-node in tests: two world roles in one process, forced to communicate over the bus
- [ ] Layer key follows party membership — deferred to M5, see below

**Exit criterion, verified by test and in a browser.** The architectural half is
`TestPortalTransfersBetweenNodes`: two nodes sharing a bus and a directory, a
character walking a portal from a room on one to a room on the other. It was
checked against two mutations — pinning both rooms to one node, and pointing
the transfer at a subject nobody answers — and fails on each, so it is not
passing by accident.

| Claim | How it was checked |
| --- | --- |
| A portal crosses nodes over the bus | `TestPortalTransfersBetweenNodes`, plus both mutations above |
| The source is torn down only after the destination accepts | The source slot is asserted released; every failure path leaves the character where it was |
| A stale fencing token cannot resurrect a character | `TestStaleTransferTokensAreRefused` |
| The session follows the character | `TestASessionFollowsTheCharacterThroughAPortal` |
| Distant spawn points stay empty, and walking away culls | Four tests in `internal/world/room/lifecycle_test.go`, mutation-checked |
| Empty rooms retire and release their instance | `TestIdleRoomsAreTornDown`; seen firing after exactly 60 s in a live server |
| Channel switching | Clicked in a browser: a second Welcome, position preserved, 0 hard corrections |
| Waypoints unlock by being walked over and appear on the world map | Walked over in a browser after a transfer |

**A bug worth recording, because tests did not catch it.** Everything a live
session holds — the socket, the channel the room reports portals and loot on
-- is an in-process reference to the node the *player* is connected to, so none
of it can travel in the transfer request. The first implementation left it
behind. The character arrived able to move and unable to do anything else: no
loot, no waypoints, and no second portal, with nothing in the logs to say so.
Every test passed, because each one took exactly one portal. It showed up in a
browser within a minute. `room.Attachment` now carries all of it, handed over
after the destination accepts, and `TestASessionFollowsTheCharacterThroughAPortal`
fails without it.

**Deferred deliberately.** The layer key is the character ID rather than the
party ID, because parties are M5 and there is nothing to key on yet. It is the
same code path with a different key — `layerFor(LayerID)` — not a placeholder
to be replaced. Private instances have no TTL either: they are torn down by the
same idle timeout as every other room, and a dungeon that should persist past
its party leaving is a reason to add one, which M7 will have and M4 does not.

**Also worth knowing:** culled mobs clear their spawn timer, so returning to an
area finds it repopulated rather than empty for a respawn interval. And a
character can be in a room with no connection at all — mid-transfer, or inside
a reconnect window — which is why the room tolerates a null sink instead of
checking for one in the tick loop.

---

## M5 — Social — **done**

- [x] Chat: global, local (room), whisper, party, guild — with rate limits and mutes
- [x] Party: invite, join, leave, kick, leader transfer, member frames
- [x] Layer key follows party membership: partying merges mob views, leaving splits them
- [x] Party exp sharing by radius, and configurable loot rules
- [x] Guild: create, roster, ranks and permissions, MOTD, guild chat
- [x] Friends and presence
- [x] Cross-room delivery entirely over the bus (no direct room-to-room calls)

**Exit criterion, verified by test and in a browser.** Every cross-node claim is
tested with the two characters on *different* nodes, because a single-node test
would pass with a shared pointer: a whisper, a party invitation, a roster
change, and guild chat all have to find somebody whose session is somewhere
else. The whisper-privacy test was mutation-checked by routing whispers to the
global subject and dropping the recipient filter; it fails.

| Claim | How it was checked |
| --- | --- |
| Global, party, and guild chat reach another node | Three tests in `internal/world/social_test.go`, and two browser clients |
| A whisper reaches only its recipient | `TestWhisperReachesOnlyItsRecipient`, with a third character on the recipient's own node; mutation-checked |
| Local chat never leaves the room | `TestLocalChatStaysInTheRoom`, speaker and listener in different maps |
| Rate limits and mutes bite, and say why | `TestGlobalChatIsRateLimited`, `TestAMutedCharacterIsToldWhy` |
| Partying merges the mob layer; leaving splits it | `TestPartyingMergesTheMobLayer`, `TestLeavingAPartySplitsTheLayerAgain`, plus room-level population counts |
| Experience is shared in-layer and in-range only | Three tests, mutation-checked by disabling each guard in turn |
| Round-robin assigns drops in turn, skipping absent members | Three tests in `internal/world/room/social_test.go` |
| Only the leader kicks, promotes, or sets the loot rule | `TestOnlyTheLeaderCanKick`, `TestLootRuleIsTheLeadersToSet` |
| Only the leader changes guild ranks; officers set the MOTD | `TestOnlyTheLeaderChangesRanks`, `TestGuildMOTDNeedsRank` |
| One guild per character, and names are unique | `TestACharacterCanOnlyBeInOneGuild`, `TestGuildNamesAreUnique` — both enforced by an index, not a check-then-insert |
| Party and guild membership survive a logout | Two tests; the returning member is back in their party's layer |
| Friends show live status without the list being live | `TestFriendsListShowsWhoIsOnline` |

**Two bugs worth recording, both found in a browser.**

A player's own party frame had an empty health bar. Every other member's frame
is filled from the vitals they publish once a second, and nobody publishes to
themselves — so the one frame that is always on screen was the one with no
data. `publishVitals` now keeps a copy as well as sending one.

The second was not M5's at all, but M5 was the first thing to trip over it. The
in-process lease table counts tokens from one, so a server that had run before
handed out tokens *below* the ones already in the database — and the fencing
predicate correctly rejected every returning character's first checkpoint as a
stale write. It looked like "your character was claimed elsewhere" seconds
after logging in. The counter is now seeded from the stored high-water mark at
startup, which is the property Redis gets for free by keeping the counter
outside the process.

**A UI decision worth naming.** Party invitations first used `window.confirm`.
A native modal blocks the event loop, which stops rendering *and* stops the
simulation stepping — so an invitation arriving mid-fight froze the character
in place while the mobs kept hitting them on the server. Prompts are in-game
now, and they take the keyboard the same way the chat line does.

**Deferred deliberately.** There is no block list and no chat history: both are
moderation features that want a moderation surface, and there is no admin UI to
put one in. Mutes are set from the command line (`mmo mute`), which is the
honest minimum — the server enforces them, and an operator can apply one
without writing SQL by hand.

---

## M6 — Skill trees and classes — **done**

*Where the game becomes itself.*

- [x] Effect DSL interpreter with the full effect vocabulary
- [x] Active skills with ranks, cooldowns, resource costs
- [x] Buff and debuff system, stacking rules, DoTs
- [x] Three classes with distinct starting skills and mechanics
- [x] Support modifiers attaching to skills by tag
- [x] Skill bar, cooldown UI, buff bar
- [x] Passive tree: allocation, connectivity validation, respec
- [x] A passive tree generator (`make tree`)

**Both halves of the exit criterion are met.** *"A support modifier attaches to
a skill it was never explicitly written for, and works"* — tested against every
matching skill rather than a chosen one. *"Two characters of the same class with
different trees play measurably differently"* — allocation reaches the stat
block through the same pipeline as gear, and `TestAllocatingChangesTheStatBlock`
asserts it.

Shipped in two parts, the engine first: the tree is built out of the engine,
and one change containing both would have been unreviewable.

| Claim | How it was checked |
| --- | --- |
| A support attaches to skills it never named | `TestASupportAttachesToSkillsItWasNeverWrittenFor` walks every skill Swiftness matches and asserts each buff got longer |
| Supports need every tag, not any | `TestSupportsNeedEveryTag` — matching any would put a melee support on a fireball |
| A support reaches a projectile's payload | `TestSupportsReachNestedEffects` — otherwise a fire support does nothing on any projectile skill |
| Supports never edit the skill | `TestApplyingASupportDoesNotEditTheSkill` — every room on the node shares that struct |
| The trade is real | `TestMultistrikeTradesDamageForRepeats` — less per hit, more total, and it costs mana |
| Shields absorb before health and expire unspent | Two tests, mutation-checked |
| Stacks multiply modifiers | `TestStacksMultiplyAModifier`, mutation-checked |
| Buffs expire and give their modifiers back | `TestBuffsExpireAndGiveBackTheirModifiers` |
| Projectiles travel, hit, and expire on a miss | Two tests — a bolt that finds nothing must stop existing |
| Ground areas tick and then stop | `TestAnAreaKeepsApplyingAndThenExpires` |
| A chain hops and skips its origin | `TestAChainReachesASecondTargetForLess`, mutation-checked |
| A trigger loop cannot reach the tick | Refused at content load, with the whole chain tracked rather than only self-reference |
| Three classes play differently | Verified in a browser: a mage spawns with Firebolt and Frost Nova, casts both, and the cooldown sweeps |

**Design notes worth keeping.**

*One struct for every effect kind*, rather than an interface per kind. A support
rewrites effects it has never heard of, so every effect has to be inspectable
and copyable without knowing its type. The cost is fields that mean nothing for
most kinds, which is visible and harmless; the alternative costs the entire
support system.

*A buff is two things at once* — effects on a beat, and stat modifiers feeding
the same pipeline as gear. Keeping them one mechanism is why a support that
lengthens durations works on both a damage-over-time and a strength buff
without knowing what either is.

*Buff durations are resolved at load.* A support that scales a duration would
otherwise silently do nothing on every skill that used a buff's default — which
is nearly all of them. This was a real bug, caught by the test that walks every
matching skill rather than a hand-picked one.

*Rank before supports.* A support that multiplies damage should multiply the
ranked damage; the other order makes a support worth progressively less the more
a skill is levelled, which is the opposite of what anyone expects.

*A triggered skill does not inherit its trigger's supports.* A support that
repeated an effect would repeat the trigger, and a support chain feeding itself
is a loop the content check cannot see.

### The tree

Generated by `cmd/treegen` and committed, the same arrangement as the golden
movement fixtures and the sprites. The split is deliberate: the *shape* — three
clusters, branches radiating out, notables at intervals, keystones at the ends,
bridges between neighbours — is mechanical and belongs in code, while what the
nodes *do* is design and lives in a file somebody reads. A generator that also
invented the modifiers would produce a tree that was large and said nothing.

Four rules, and they are the whole of the tree's design:

- You start at your class's node, free and unrefundable.
- A node is available only next to one you hold, so a distant keystone costs
  the path to it.
- You cannot hold more than your level has paid for.
- A refund may not strand anything — taking a node from the middle of a path
  would leave everything past it held and unreachable, which is a build nobody
  could have reached by allocating.

| Claim | How it was checked |
| --- | --- |
| Every class reaches every node | Validated at load, and asserted on the shipped tree |
| A stranded node fails the build | `TestAStrandedNodeIsRejected` — the failure the reachability check exists for |
| Six ways a tree can be broken all fail the build | `TestBrokenTreesAreRejected` |
| Every keystone trades something | `TestEveryKeystoneHasADrawback` |
| Allocation needs a neighbour | `TestAllocatingNeedsANeighbour` |
| Points come from levels and bound allocation | `TestAllocationIsBoundedByLevel` |
| Refunds cannot strand | `TestRefundingCannotStrandNodes` |
| Allocating changes the stat block | `TestAllocatingChangesTheStatBlock` — a tree that does not reach the stats is bookkeeping |
| The tree screen works | Clicked in a browser: allocated a node, spent the point, and the second click was refused with a reason |

**A bug the tree found in already-merged code.** `ratioToPPM` clamps negatives
to zero — correct for a probability, which is what it was written for, and
wrong for a stat modifier. Every keystone drawback, Chilled's slow, and
Weakened's penalty were being silently discarded, so keystones gave their
upside for free. Caught by asking "does each keystone trade something" rather
than by checking one. There is now a separate signed conversion, and a test
that a debuff made only of negative modifiers still is one.

**A second one:** the session's `classID` was never set, so anything reading it
after login — re-granting starting skills on a bar change — used the empty
string and fell back to an arbitrary class.

**Also fixed in the first half:** first login was doing a dozen database round trips to seed
a character's skills and bar. It now does two statements. That showed up as a
reconnect test failing under full-suite load and passing in isolation — extra
work on a path a player waits on is extra work on a path a player waits on.

---

## M7 — Dungeons and bossing

- [x] Boss AI with phases, telegraphed attacks, enrage
- [x] Team-play mechanics that require coordination — a hit divided among everyone it lands on, so what one player cannot survive six can
- [x] Boss health UI, telegraph rendering
- [ ] Instanced dungeon flow: entry requirements, lockouts, progression, completion
- [ ] Enhanced mobs: champion and rare tiers rolling elite modifiers
- [ ] Zone events: interactive triggers, timed spawns, mini-bosses
- [x] Death and recovery: a downed state, a revive clock, a penalty, and spawn protection
- [ ] Wipe flow (a whole party down at once inside an instance)

**Exit:** a six-player party clears a dungeon and kills a boss whose mechanics genuinely require coordination.

The encounter half is done and the dungeon half is not. The order is deliberate:
a dungeon with nothing worth fighting at the end of it is a corridor, and the
entry rules, lockouts and completion state are all in service of an encounter
that has to exist first.

A boss is `profile = "boss"`: a Go state machine that runs phases written in
content. Phases are entered as health falls and never left, each listing what
the boss may do while it is in that one, so a fight changes shape rather than
being one rotation with bigger numbers. The scripting language stays out and
the encounter stays authorable.

| Claim | How it was checked |
| --- | --- |
| Phases advance on health and never come back | `TestBossAdvancesPhasesAsHealthFallsAndNeverReturns` — healing a boss to full must not reset the fight |
| A hit that crosses two thresholds lands in the right phase | `TestBossSkipsPhasesItsHealthHasPassed` — walking down one phase per tick would give a party free openings |
| The enrage clock belongs to the phase, not the fight | `TestTheEnrageClockRestartsWithEachPhase` — otherwise the last phase enrages the instant it is reached |
| An attack is announced before it lands | `TestABossAnnouncesAnAttackBeforeItLands` — and nothing may land during the wind-up |
| The marker is the hitbox | `TestTheMarkerIsExactlyWhatTheAttackWillHit` — computed separately, the two drift apart the first time either is tuned |
| Moving out of a marker works | `TestWalkingOutOfAMarkerAvoidsTheAttack` — a boss that turned mid-wind-up would make the marker a lie |
| A split hit is genuinely divided | `TestSplitDamageIsDividedAmongEveryoneItHits` — four shares add up to the solo hit |
| A boss does not commit to what it cannot reach | `TestABossDoesNotWindUpAnAttackThatCannotReach`, found in play: it rooted itself telegraphing at a player on a ledge |
| A boss out of reach closes the gap | `TestABossKeepsClosingOnATargetItCannotReach` — horizontal distance alone is half an answer in a platformer |
| Dying costs something and is over | `TestADownedCharacterStaysDownForTheClock`, `TestDeathCostsProgressTowardTheCurrentLevel` — of progress *within* the level, so a death never costs a level |
| A body is not a character | `TestADownedCharacterDoesNotWalk`, `…DoesNotTakeAPortal`, `…CannotLoot`, `TestMobsIgnoreADownedCharacter` |
| Coming back is not a death loop | `TestComingBackIsBrieflySafe`, `TestAttackingEndsTheReviveGrace` — found in play: reviving next to the slime that killed me killed me again |

Player death had been a stub since M2 (*"restored in place, lands with
persistence"*). It is a prerequisite rather than a nicety: a party cannot wipe
in a dungeon if nothing in the game can die.

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
