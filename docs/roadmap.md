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
- [x] Instanced dungeon flow: entry requirements, lockouts, progression, completion
- [x] Enhanced mobs: champion and rare tiers rolling elite modifiers
- [x] Zone events: interactive triggers, timed spawns, mini-bosses
- [x] Death and recovery: a downed state, a revive clock, a penalty, and spawn protection
- [x] Wipe flow (a whole party down at once inside an instance)

**Exit:** a six-player party clears a dungeon and kills a boss whose mechanics genuinely require coordination.

Every item is built and the mechanics are covered, but the exit itself is not
yet evidence: nothing here has been through a real six-player clear. The
split-damage test proves six shares add up to the solo hit, and
`TestAPartySharesOneDungeonInstance` proves a party lands in one instance — the
untested part is six humans, network and all, which is a play session rather
than a test to write.

The encounter came before the dungeon, deliberately: a dungeon with nothing
worth fighting at the end of it is a corridor, and the entry rules, lockouts
and completion state are all in service of an encounter that has to exist
first.

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
| A dungeon has an order | `TestADungeonOpensOneStageAtATime`, `TestClearingAStageOpensTheNext` — progression is spawning, not doors |
| A stage is not clear early or twice | `TestAStageIsNotClearBeforeItHasSpawned`, `TestAStageWithMobsAliveIsNotClear`, `TestADungeonStageDoesNotRespawn` |
| Both endings end it | `TestClearingTheLastStageEndsTheRun`, `TestAPartyAllDownAtOnceWipes`, and a wipe writes no lockout |
| A dropped connection is not a death | `TestADisconnectedPlayerDoesNotWipeTheRun` and its inverse |
| **A party shares one instance** | `TestAPartySharesOneDungeonInstance`, `TestAPartyWalkingIntoADungeonLandsTogether`, `TestLoggingBackInRejoinsThePartysInstance` |
| A champion is a real change, not a name | `TestAChampionIsStrongerThanWhatItCameFrom` — health, damage and reward all move |
| A champion never edits shared content | `TestAChampionDoesNotEditItsDefinition` — the definition is read concurrently by every room on the node |
| A boss never rolls one | `TestABossNeverRollsElite` — randomness on top of three phases is not a fight you can learn |
| An event's mobs do not exist before it starts | `TestAnEventsMobsDoNotExistBeforeItStarts` — an event whose spawns ran anyway is just a busier map |
| A timed event runs, and only with somebody there | `TestATimedEventStartsAndProducesMobs`, `TestATimedEventWaitsForSomebodyToBeThere` — an empty room burning its period means a player arrives during the cooldown |
| An event ends, and takes its mobs with it | `TestEndingAnEventClearsWhatItProduced` — otherwise the first run leaves the zone permanently harder |
| A second run is a full wave, not a trickle | `TestAnEventWaitsOutItsCooldown`, `TestASecondRunProducesAFullWave` — the population counter has to reset with the run |
| A shrine is the player's decision | `TestAShrineEventNeverStartsByItself`, `TestTouchingAShrineStartsItsEvent`, `TestAShrineIsAnEntityPlayersCanSee` |
| A shrine is not a button | `TestAShrineCannotBeSpammed` — without the cooldown it is a thing somebody stands next to forever |
| Bad event content does not load | `TestBrokenEventsAreRejected` (12 cases) — a renamed spawn point otherwise announces itself to the whole room and produces nothing |

Progression is **spawning, not doors**. A stage's mobs do not exist until the
stage before it is cleared, so "kill the guards, then the king" needs no keys
and no geometry that changes underfoot — which also means the client's
collision is never told anything, and prediction cannot drift over a wall that
is solid on one side and not the other.

Zone events reuse that gate rather than adding one. A gated spawn point
produces nothing until something opens it, and "something" is a dungeon stage
or a zone event with equal ease — so a timed slime tide and a dungeon's second
room are the same three lines of spawn code with a different owner. They differ
in exactly one flag: a dungeon stage produces its population **once** (a stage
that respawned could never be cleared), and an event's points keep producing
for as long as it runs (a tide that stopped after three slimes would not be a
tide).

The two triggers answer different questions and so get one knob each. A
**timed** event is the world acting on its own, and is what makes standing in a
zone worth doing; its `every_ms` is measured from the end of the last run, and
it will not burn a period in an empty room. A **shrine** is the player choosing
to start something, and is what makes walking past one a decision; its
`cooldown_ms` is what stops it being a button. Giving both knobs to both
triggers was the first thing tried, and it made "when does this start again"
have two answers — which is one of them being wrong. The loader now refuses
either combination by name.

Private placement turned out to be keyed by **character**, not party — M4 built
it with a comment saying the owner would become the party ID "from M5", and M5
never came back to it. Every member of a party got their own copy of the
instance: nothing errored, nobody saw anyone else, and the dungeon quietly
stopped being one. That is now fixed at all four routing sites, including
login, where the party has to be read *before* placement rather than restored
after it.

Player death had been a stub since M2 (*"restored in place, lands with
persistence"*). It is a prerequisite rather than a nicety: a party cannot wipe
in a dungeon if nothing in the game can die.

---

## M8 — Secondary skills

- [x] Resource nodes on maps with independent respawn timers
- [x] The 600 ms action tick, layered on the 20 Hz sim tick
- [x] Gathering: woodcutting, mining, fishing, herbalism
- [x] Processing: smithing, cooking, alchemy
- [x] OSRS exp curve, 1–99 per skill, levelling from use
- [x] Tool tiers and level gating
- [x] Skills panel

**Exit:** chop trees for twenty minutes, gain levels, smith what you gathered into something you can equip.

**Met, and played rather than inferred.** Mined twenty copper ore on the forest
outcrop, smithed ten bars, forged three Copper Swords, and equipped one —
attack 125 → 131 from the rolled implicit, with Smithing going 5 → 9 on the way
and the chat line to prove it. The panel's ingredient counts followed the
materials down as they were spent.

Gathering came before processing, the same order M7 used: there is no point in
a forge before there is anything to put in it, and every question about what
smithing should cost is a question about how fast gathering produces.

The **action tick** is derived rather than a second loop — "every twelfth
simulation tick" is 600 ms, and it means the room has one clock that nothing can
drift from. It belongs to the *room* rather than to the player who started an
action, which is OSRS's behaviour and worth stating because it is observable:
two players who begin half a beat apart still resolve together.

Gathering **reuses the layering model** rather than adding contention rules. A
resource node is an entity in a layer like a mob, so a per-player tree is free
and a shared one is a deliberate choice per placement. That is the same
mechanism that removed spawn camping in M4, applied to the thing OSRS is famous
for players queueing at.

Two clocks and two durabilities, on purpose. The **item** is written the moment
it is gathered, because an item has to exist somewhere and "in a tick loop's
memory" is not somewhere. The **experience** rides the ordinary checkpoint,
because a yield lands every few seconds for as long as somebody keeps at it and
one write per log would make woodcutting the busiest table in the database. The
room is authoritative for the session either way.

| Claim | How it was checked |
| --- | --- |
| Gathering runs on the 600 ms beat, not the 50 ms one | `TestGatheringResolvesOnTheActionTickAndNotTheSimulationTick` — on the wrong clock it would produce twelve times as much and every number would be tuned against something nobody meant |
| The beat belongs to the room | `TestEveryoneGathersOnTheSameBeat` — per-player timers would drift a party apart for no visible reason |
| Holding the key does not gather faster | `TestHoldingTheKeyDoesNotGatherFaster` — and announces itself once, not once per tick |
| A yield is an item *and* experience | `TestAYieldGrantsAnItemAndExperience`, `TestSecondaryExperienceAccumulatesAndLevels` |
| A node runs out and comes back, whole | `TestANodeRunsOutAfterItsYields`, `TestASpentNodeComesBack` — a node that returned part-used would be worth less every respawn |
| Finishing a tree is not an error | `TestUsingUpANodeIsNotReportedAsAFailure` — the player succeeded |
| Everything interrupts it, at once | `TestWalkingAwayStopsGathering`, `TestTakingDamageStopsGathering`, `TestAFrozenPlayerStopsGathering`, `TestUnequippingTheToolStopsGathering` |
| A corpse does not chop | `TestADownedCharacterDoesNotGather`, `…CannotStartGathering`, `TestBeingKilledStopsGathering` — the first two set the downed state directly, because a real death also sets the in-combat flag and the combat check would otherwise be doing the work |
| Every refusal says why | `TestGatheringNeedsTheRightToolAndSaysSo`, `TestAToolTooWeakForANodeIsRefused`, `TestANodeAboveYourLevelIsRefusedByName`, `TestGatheringOutOfRangeIsRefused` — "nothing happened" is the one failure a player cannot debug |
| A tool is a key *and* a speed-up | `TestGatherChanceRisesWithLevelAndTool` — tested as arithmetic, because a test that samples an RNG fails in CI eventually |
| A node is per player unless it says otherwise | `TestAnOwnerLayerNodeIsOnePerPlayer`, `TestAPlayerCannotGatherSomebodyElsesNode`, `TestASharedNodeExistsOnceForTheRoom` |
| Reconnecting neither resumes nor rolls back | `TestReconnectingDoesNotResumeGathering`, `TestAttachingDoesNotRollBackExperience` — the room's copy is the newer one between checkpoints |
| Bad content does not load | `TestBrokenSecondaryContentIsRejected` (18 cases) |
| **A stackable material takes one slot** | `TestGrantingAStackableMaterialFillsOneSlot` and five more — found in play, not by a test |

**Stacking was missing, and gathering is what made it matter.** `Grant` took a
slot per unit. Nobody noticed while loot was the only source of materials — a
boar drops one hide — and six copper ore in six slots was the first thing
visible in the browser. A 24-slot bag fills in about two minutes once a player
can chop for twenty, so the exit criterion was unreachable. Merging on grant
fixes gathering and loot together.

Two smaller things the browser found and no test would have: the gathering line
was drawn straight through the health bar, and the skills panel told a player
they needed a `fishing_rod`. A tool's class is an identifier and what a player
is told is prose; they are now separate fields, and the fixture deliberately
gives one skill a tool name that differs from its class so a refusal printing
the wrong one fails.

Resource nodes have no art. They are shaped silhouettes keyed on the skill —
a trunk and a crown, an outcrop with an ore seam, ripples, a low cluster —
because one silhouette for all of them meant a copper rock drawn as a tree, and
the name on the label is not what anyone reads while scanning a map.

### Processing

Gathering and crafting share the action tick and the "what is this character
doing" slot — starting either ends the other, because two runs against one bag
means the bag loses. What they do *not* share is who decides.

Gathering **produces from nothing**: the roll lands or it does not, the
experience is earned the moment it does, and the item is a consequence the
session stores afterwards. Nothing can fail once the roll has landed.

Crafting **spends**. The materials are in the inventory, which the room cannot
see and must not guess at, and consuming them is a database write that can
legitimately come back "you do not have those any more". So a run is two steps:
the room asks, the session consumes and produces in one transaction, and the
room grants the experience only when it hears back. A room that paid on asking
would pay a smith for bars they never made.

That transaction is the load-bearing part. Consuming three bars and then failing
to insert the sword destroys items; retrying the insert alone duplicates one.
Both are the failures `MoveItem` exists to avoid, in a shape where there are
several inputs and the arithmetic is destructive.

**Being hit does not interrupt a craft**, deliberately, where it does interrupt
a gather. A station is somewhere a player has chosen to stand still, and a mob
wandering past should not cost them the bar. It has a consequence the loader
cannot check: a camp inside a mob spawn is a place to stand and die, which is
exactly what happened the first time the camp was placed in the forest. The
camp now lives in the tutorial map's one genuinely safe strip — the forest has
none, because the slime tide sweeps most of its floor.

| Claim | How it was checked |
| --- | --- |
| The room asks and grants nothing yet | `TestARunAsksTheSessionAndGrantsNothingYet`, `TestAnAnsweredRunGrantsExperience` |
| One run in flight at a time | `TestOnlyOneRunIsInFlightAtATime` — two would ask for the same materials and both be told yes |
| Running out is an ending, not an error | `TestRunningOutOfMaterialsEndsTheRun` — and grants nothing |
| A run repeats without another key press | `TestACompletedRunStartsTheNext`, `TestTheSecondRunTakesAsLongAsTheFirst` — the second test needs a three-beat recipe; a one-beat one cannot see a clock that never restarts |
| A longer recipe is longer | `TestALongerRecipeTakesLonger`, `TestCraftingRunsOnTheActionTick` |
| Walking away ends it, being hit does not | `TestWalkingAwayStopsCrafting`, `TestTakingDamageDoesNotStopCrafting` |
| A corpse does not smith | `TestADownedCharacterDoesNotCraft`, `…CannotStartCrafting`, `TestAFrozenPlayerStopsCrafting` |
| One action at a time | `TestStartingACraftStopsGathering`, `TestStartingAGatherStopsCrafting` — the fixture puts the forge *beside* a tree on purpose, or the gather would be interrupted for range and the test would pass without the rule |
| Every refusal says why | `TestARecipeAboveYourLevelIsRefused`, `TestARecipeAtTheWrongStationIsRefused`, `TestCraftingOutOfRangeIsRefused` |
| A station is shared and asked, not pushed | `TestAStationIsASharedEntityEveryoneCanSee`, `TestAskingAStationWhatItMakesReachesTheSession` |
| Reconnecting does not resume a run | `TestReconnectingDoesNotResumeCrafting` — it would keep spending materials invisibly |
| **One run is one transaction** | `TestCraftWithMissingInputsChangesNothing`, `TestCraftIntoAFullBagChangesNothing`, `TestConcurrentCraftsDoNotSpendTheSameMaterials` |
| An input spread over stacks is still an input | `TestCraftConsumesAcrossSeveralStacks` — otherwise availability depends on how the bag happened to fill |
| Both halves are journalled, distinguishably | `TestCraftIsJournalledOnBothSides`, `TestSpendingAWholeStackIsStillJournalled` — a bar consumed into a sword and a bar thrown away are not the same event |
| Bad content does not load | `TestBrokenRecipeContentIsRejected` (15 cases) |

**Three station labels in one camp were an unreadable bar of text** — the third
time this exact failure has appeared, after champion names in M7 and the first
attempt at this camp. Shortening the names was not enough, and spacing them out
only moved the collision onto the shrine's label. Stations now name themselves
only when the character is close enough to use one, which is the same rule mob
names already follow and the only one that does not need re-tuning per map.

---

## M9 — Scale out

*Only now, and only because the seams were built in M0.*

- [x] `nats` bus implementation
- [x] `redis` directory implementation (and presence and parties with it)
- [x] Cross-process room handles, so a world node can be deployed more than once
- [x] Split roles into separate deployments (`--roles=gateway` runs with no simulation)
- [x] k8s manifests
- [x] Headless bot client for load generation (`cmd/mmobot`)
- [x] Grafana dashboards checked in (`deploy/grafana/mmo.json`)
- [x] Load test: 1000 bots across 3 world nodes (`deploy/loadtest.sh`)
- [x] Chaos testing: kill a world node, verify leases expire and characters recover
- [x] Graceful drain on shutdown: hand off rooms, checkpoint, disconnect cleanly

**Exit:** 1000 concurrent bots across three world nodes, tick p99 within budget, and killing a node loses at most one checkpoint interval for its players.

### The bus

The claim in the sequencing notes below is that M9 is configuration if M4 was
honest. For the bus, it was: `internal/bus/nats.go` is one new file behind the
interface M0 defined, and **every existing cross-node test now runs over a real
NATS server unchanged** — portal transfer between nodes, global chat, whispers,
party invites. Nothing above `internal/bus` was touched.

The two implementations are held to one contract by a conformance suite rather
than a suite each. Two suites drift, and the drift stays invisible until roles
are actually split and a subject that worked in one process stops working. What
stayed implementation-specific is what genuinely belongs to a transport:
`inproc` dropping rather than blocking, and its own subscription-map locking.

| Claim | How it was checked |
| --- | --- |
| Both buses route identically | `TestBusWildcardsRouteIdentically` and sixteen more — seventeen shared tests, each run against both |
| A refusal is not a timeout | `TestBusResponderErrorReachesTheRequester`, `TestBusRequestWithNoResponder` — "the destination refused" and "the cluster is broken" need different responses |
| A late reply is not the next answer | `TestBusLateReplyIsNotMistakenForTheNextOne` — `inproc` needs a correlation id for this; NATS gets it from a fresh inbox. The contract is the same, so it is asserted against both |
| A bad subject is a bad subject on both | `TestBusSubscribeToAnEmptyPatternIsASubjectError` — the error *type*, because a caller distinguishes "wrong subject" from "unreachable cluster" by type |
| Two connections are two nodes | `TestNATSCarriesMessagesBetweenConnections`, and the whole cluster suite with `MMO_TEST_NATS_URL` set |
| A borrowed connection is the caller's | `TestNATSDoesNotCloseABorrowedConnection` — closing one this package did not open takes every other user of it offline |

**A NATS subscription is not live until it is flushed.** This was the one real
bug, and only a real server could have found it: a subscribe is a protocol
message like any other, so until it reaches the server the subscription does not
exist. The first version flushed on every *publish*, which established
subscriptions by accident while costing a network round trip per message —
exactly the wrong trade for a bus carrying per-tick traffic. Removing that flush
made a cross-connection test stop receiving, which is how the real problem
surfaced. `Subscribe` flushes now; `Publish` does not.

**Close waits for the drain.** `Drain` returns immediately and finishes
asynchronously, so the first version's `Close` returned while its subscriptions
were still being delivered to. That showed up as a flaky test and would have
shown up in production as a node that kept answering after it was drained —
which is precisely what the graceful-drain item further down this list is
supposed to prevent.

### The directory

The same shape as the bus: one new file behind the existing interface, and the
whole cross-node suite now runs with **a real Redis directory and a real NATS bus
at once** — two nodes registering separately, agreeing about placement over the
network. `TestPortalTransfersBetweenNodes` asserts the two rooms land on
*different* nodes, so that is Redis placement doing the spreading rather than a
shared pointer.

Every operation that must not race is a Lua script, because that is where the
correctness lives. "Find the least-full channel and reserve a slot in it" done as
a read then a write lets two simultaneous joins take the same last slot — and for
a private key it lets two members of one party each get their own dungeon, which
is the exact bug M7 found on the other side of this interface. 200 concurrent
joins against real Redis is part of the suite.

**Node liveness is a TTL heartbeat.** A node registers on construction and
heartbeats until it closes; when it stops, its liveness entry expires and it
stops receiving new rooms. That is the directory's half of surviving a dead node:
the rooms it hosted are gone either way, but continuing to *place* on it would
mean players sent to rooms nobody is running. The registration itself is left
behind on purpose — its score is what keeps placement order stable across a
restart, so a rolling deploy does not reshuffle load.

**The interface changed**, which is the one place M0's claim did not quite hold:
`Lookup`, `InstancesFor` and `List` returned no error. For an in-memory
directory that is fine; for a network one it is not, because an unreachable
Redis answering "this map has no channels" or "that instance does not exist" is
not a degraded answer but a wrong one, and a caller acts on it by refusing a
channel switch to a channel that is running fine. Three call sites.

| Claim | How it was checked |
| --- | --- |
| Both directories behave identically | 21 shared tests, each run against Memory and Redis, plus 7 Redis-only ones |
| Placement and reservation are atomic | `TestConcurrentJoinsNeverExceedCapacity` (200 joiners), `TestConcurrentJoinAndLeave`, against real Redis |
| A private key is one instance | `TestPrivateRoomRejectsRatherThanSplitting`, and `TestAPartySharesOneDungeonInstance` end to end over Redis |
| Two nodes agree | `TestRedisDirectoryIsSharedBetweenNodes` — created on one, seen and joined on the other, released from either |
| Rooms spread across live nodes | `TestRedisPlacementSpreadsAcrossLiveNodes`, and `TestPortalTransfersBetweenNodes` which requires it |
| A dead node stops receiving rooms | `TestRedisPlacementSkipsADeadNode` |
| No live node is its own error | `TestRedisPlacementWithNoLiveNodeIsRefused` — `ErrNoLiveNode`, not `ErrNoCapacity`: capacity means the world is full, this means there is no world |
| A restart keeps its place in the order | `TestRedisRegistrationOrderSurvivesARestart` — re-registering the *first* node, because re-registering the last cannot change the order however the score is assigned |
| **Releasing leaves nothing behind** | `TestRedisReleaseLeavesNoBookkeepingBehind` and the TryRelease twin — found by mutation testing, and the load counter is the one that matters: a counter that only rises makes placement permanently wrong |

### Presence

The last read-only piece of shared state, and the smallest: two indexes,
characters by ID and IDs by normalised name. It is what a whisper is routed
through — "which node is holding Alice" is a question one node asks about a
session on another, and with a table per process the answer was always "nobody".

It went to Redis rather than Postgres because losing it costs everyone their
friends list for a moment and nothing else. There is deliberately **no TTL**: a
session announces once and re-announces on transfer, so a TTL would need a
heartbeat per character. Instead a node clears its own records on startup
(`ForgetNode`), which covers the restart case at no steady cost. What is not
covered is a node that dies and never comes back, whose characters stay listed —
that belongs with the chaos-testing item above, where a node death is actually
simulated.

**Lua cannot compute the lookup key.** The scripts need a record's normalised
name to drop its stale name index, and `string.lower` is ASCII-only and does not
trim, so it disagrees with `NormaliseName` for any name with an accent or a
stray space. A disagreement there leaves a name pointing at a character forever,
which reads as a whisper delivered to somebody who no longer answers to it. So
the normalised form is *stored in the record* rather than derived: one definition
of the lookup key, and it lives in Go.

**The conformance suite found a bug in the implementation that already
shipped.** Dropping a name index unconditionally revokes a name another
character has taken over — names are unique, so this needs an announce and a
rename to interleave across nodes, but "narrow" is not "impossible" and the
symptom is a player nobody can whisper. Redis had the guard because writing Lua
forces you to think about what is already in the key; `MemoryPresence` did not,
and had been the only implementation for eight milestones. Asserting the
contract against both is what surfaced it.

Presence also had **no unit tests at all** before this — it was exercised only
through the cluster suite, which meant a whisper reaching the wrong node would
have been diagnosed as a chat bug.

| Claim | How it was checked |
| --- | --- |
| Both tables behave identically | 15 shared tests, each run against Memory and Redis, plus 2 Redis-only |
| A rename is not reachable under both names | `TestPresenceRenameDropsTheOldName` |
| A contested name belongs to whoever holds it | `TestPresenceForgetDoesNotRevokeAContestedName`, `TestPresenceRenameDoesNotRevokeAContestedName` — the pair that found the Memory bug |
| Normalisation is Go's, not Lua's | `TestPresenceHandlesNonASCIINames` — reintroducing `string.lower` makes it fail, which is the only reason to trust the stored `norm` field |
| Dropping a record drops its name | `TestRedisPresenceLeavesNoOrphanedNames` — Redis-only because it is invisible through the interface: `ByName` resolves via `ByID`, so an orphaned name reads exactly like a name that was never there. Silent, unbounded, one entry per name the server has ever seen |
| A node clears only its own | `TestPresenceForgetNodeClearsOnlyThatNode` |
| Two nodes see one table | `TestRedisPresenceIsSharedBetweenNodes`, and the cross-node chat, whisper, friends and party-invite tests over real Redis |

**The interface changed for the same reason the directory's did**: `ByName`,
`ByID` and `List` returned no error, and an unreachable Redis answering "nobody
is online by that name" is a wrong answer a caller acts on. Five call sites — a
whisper now says it could not look the name up, and the friends list and guild
roster log and show a partial list rather than an empty one.

**One Redis client, not three.** `main.go` had been opening a pool per user —
directory, leases, token storage — all to the same server. Adding presence would
have made four, so `openRedis` now opens one and threads it through. The dead
`Watch`/`watcher` machinery on `MemoryPresence` went at the same time: nothing
outside the file referenced it, and friends lists poll `ByID`.

### Parties

The last coordination table, and the one with actual rules: eleven methods,
nested state, a leader whose powers move, and an invitation that expires. The
bus and the directory were one new file behind an unchanged interface; this one
is the same shape but the interface has more to say.

**Every method is one Lua script**, because every method is a check followed by
a write. "Is there room?" then "add them" lets two simultaneous accepts both
take the last slot. "Am I the leader?" then "kick them" lets somebody demoted in
between kick anyway. Redis runs a script to completion with nothing interleaved,
which is the only reason any of the guards mean anything -- and
`TestPartyConcurrentAcceptsNeverExceedCapacity` fires twenty accepts at a party
with three free slots and asserts exactly three land.

**An invitation is a key with a TTL, not a field with a stored deadline.** Redis
does the expiring, so no clock crosses the wire and nothing is left behind for
somebody who was asked once and never logged in again. The TTL is per-instance
so the tests can use fifty milliseconds and watch a real invitation actually
expire, rather than asserting against a fake clock that proves only that the
arithmetic is right.

**cjson encodes an empty table as `{}`.** A disbanded party's roster is empty by
definition, and `{}` is an object, which does not decode into a JSON array --
so the first version returned a decode error for the ordinary case of the last
member leaving. The fix is to drop the field rather than empty it: an absent
`members` decodes to a nil slice, which is exactly the contract ("a disbanded
party comes back with no members").

**Two mutation survivors, both real.** One was the check that stops somebody
already in a party accepting a stale invitation -- reachable, because starting a
party of your own touches no invitation, so the offer is still standing when you
answer it. The other was the cleanup of a membership index pointing at a roster
that is gone: not reachable through the scripts, which are atomic, but very much
reachable in a store running an eviction policy -- and without it that character
can never party again, because starting one checks that index. Losing party
state is supposed to cost a regroup, so being permanently stranded is the one
outcome that is not allowed.

Writing the suite also turned up an undocumented design property worth pinning
down: an invitation is keyed by invitee, so **a second invitation silently
replaces the first**. That is right for a client showing one prompt, but it
means being asked by somebody else withdraws the earlier offer, which is now
asserted rather than left as an accident of the key layout.

| Claim | How it was checked |
| --- | --- |
| Both tables behave identically | 43 shared tests against Memory and Redis, plus 4 Redis-only |
| The guards are load-bearing | 32 mutations across both implementations, all caught |
| Two accepts cannot take one slot | `TestPartyConcurrentAcceptsNeverExceedCapacity`, twenty at once against real Redis |
| A party of one is not a party | `TestPartyDisbandsWhenOneMemberIsLeft`, and `TestPartyDisbandingLetsMembersRegroup` for the half that strands people |
| Leadership moves rather than dissolving | `TestPartyTheLeaderLeavingPassesLeadership`, `TestPartyPromotingHandsOverThePowers` |
| Only the leader may kick, promote, set loot | Three tests, plus `TestPartyTheLeaderMayNotKickThemselves` -- which would disband by the back door, skipping the handover |
| Disbanding leaves nothing behind | `TestRedisPartiesLeaveNoBookkeepingBehind` -- Redis-only, because an orphaned roster reads exactly like no party at all |
| Nobody is stranded by lost state | `TestRedisPartiesRecoverFromAVanishedRoster` |
| Two nodes see one table | `TestRedisPartiesAreSharedBetweenNodes`, and the cross-node party, layer and dungeon tests over real Redis |

**The hot path had to change with it.** `recordVitals` read the party from the
directory on every vitals push -- once a second per member, so thirty-six reads
a second for a full party, each of which would now be a network round trip
asking for something the last party update already carried. The session caches
the roster it was last given instead. `syncPartyMembership` was reading the same
party twice in a row for two different fields, which over a network is both a
wasted trip and a torn read: the party can disband between them, and the session
would subscribe to one party while applying another's loot rule.

**`Of` reports errors** for the reason `Lookup` and `ByName` do. Five call
sites. The one that is not a log-and-continue is login: placement depends on
which party you are in, so a failed read there is refused rather than guessed --
guessing puts somebody logging back into a dungeon in a fresh instance of their
own, next to the party still running it.

**A defect from the previous slice, fixed here.** `RedisLeases.Close` closed its
client, which was harmless while every user opened its own; consolidating onto
one shared client made it a component unilaterally closing a connection the
directory, presence and token storage are still using. Nothing called it, so it
was latent rather than live.

### Rooms in another process

The piece every other M9 item was quietly waiting for, and the one the code had
been pointing at since M0: `Node.resolve` returned an explicit "cross-process
room handles arrive in M9" error, and the cluster harness said its shared room
registry "stands in for M9's remote handles".

Both are gone. A session on one node drives a room on another over the bus, and
**the cluster suite now runs with a registry per node** -- so the two nodes
share the bus, the directory, the leases and the database, which is exactly what
two processes share, and nothing else. Thirty tests fail if the remote handle is
taken away, which is the measure of how much of the game was resting on that
shared pointer.

The split is asymmetric on purpose. Three commands return something and are
request/reply; the other thirteen are published and not waited for. Input
arrives every client tick, and a round trip per keypress would put the network
between a key and the simulation -- the room already treats input as a queue
that can starve, and replays the last one when it does, which is the same
failure a dropped publish produces and is already handled.

**A publish to a responder is not dropped, it is reinterpreted.** This was the
real bug, and it is the kind only a test that drives the thing directly can
find. `Respond` reads every message as a `BusEnvelope`, because that is how a
request carries its reply subject -- and *any* bytes decode as some envelope. So
publishing a command straight to a responder's subject succeeded, the handler
ran with an empty payload, and nothing happened. Thirteen of the sixteen
methods did nothing at all, and the transfer tests still passed because
transfers only use the three that wait for an answer. The fix is `bus.Notify`,
which wraps the payload the way a request does minus the reply subject, and is
now part of the bus conformance suite.

**Content does not travel.** Portals, stations, recipes and dungeons are named
and resolved from the receiving node's own content, the same way an incoming
transfer already resolves its spawn point. Sending the resolved object would
make the wire format grow with the game's content and would let two nodes
running different builds disagree silently about what a station makes.

**An empty stat block is not a neutral one.** The "more" layer is a product, so
a block that fails to decode and is applied as zeroes gives the character no
life and no damage. `stats.Rebuild` refuses a block of the wrong length -- which
is what a sender built against a different stat list produces -- and the room
keeps the block it had, which is stale but playable.

| Claim | How it was checked |
| --- | --- |
| The whole game works across processes | The cluster suite with a registry per node: transfers, chat, parties, dungeons, loot, waypoints. 30 tests fail without the remote handle |
| Input actually crosses | `TestRemoteRoomAppliesInput` -- the highest-frequency call, published rather than awaited, so nothing else would notice it going nowhere |
| Leaving actually crosses | `TestRemoteRoomLeaveRemovesTheCharacter` -- a Leave that never lands is a ghost nobody can remove |
| A one-way message reaches a responder | `TestBusNotifyReachesAResponder`, against both buses |
| A retired room is retryable, not an error | `TestRemoteRoomReportsAMissingRoomAsClosed` -- idle rooms retire, so this is a normal Tuesday, not a failed login |
| A failed capture is not an empty snapshot | `TestRemoteRoomCaptureFailureIsNotAnEmptySnapshot` -- the caller checkpoints what it gets back |
| Absent is not empty | `TestAttachmentKeepsAbsentDistinctFromEmpty` -- "leave what the room has" against "set it to nothing", which proto3 cannot express |
| The portal index decides where you go | `TestResolvePortalEventUsesTheIndex` -- every map in the test content has one portal, so a version ignoring the index passes everything else |
| A stale room's messages are dropped | `TestStaleRoomCallbacksAreDropped` |
| Everything round-trips | Nine encode/decode tests, including fixed-point positions, which would land a character inside the floor if rounded |
| The guards hold | 25 mutations of the wire, the handle, the server and the callbacks -- all caught. Five of the first seven survivors were the publish bug above |

**Known limit:** the callback subject is per character, so a message from a room
the character has just left can still arrive. The entity id on each callback
narrows it -- a message about a body they no longer have is dropped -- but
entity ids are per room, so the old room could have handed out the same one.
The cost of being wrong is one stale frame that the next snapshot corrects.
Closing it properly means a subject per attachment rather than per character.

### A gateway that runs no simulation

`--roles=gateway` used to refuse to start without a world node in the same
process, with an error saying so. It now runs on its own: it terminates
sockets, checks what arrives, and forwards it to whichever world node holds the
character. It owns no rooms, holds no leases, and runs no tick loop.

**The gateway no longer touches a room at all.** It used to reach through the
session for a `room.Handle` and call `Input` on it, which is fine in one process
and impossible across two. The four calls a connected player makes constantly --
move, cast, interact, craft -- moved onto `PlayerSession`, so the session is the
whole of the gateway's API surface and `Handle()` returns nil for a remote one
deliberately. A proxy there would be an invitation to add a fifth.

**Validation stays on the gateway.** That is the trust boundary and where the
untrusted bytes arrive, so the clamping and the length checks happen before
anything crosses. What crosses is the already-checked request, which is why the
wire types mirror the Go request structs rather than the client's messages.

**The same asymmetry as the room protocol**: the four constant calls are
published and not waited for; everything else is a request that returns either
nothing or a refusal worth showing the player. A refusal has to arrive as a
refusal -- the gateway prints these straight to the screen, and somebody who
tried to equip an item they do not own should not read "context deadline
exceeded".

**`InRoom` is answered locally.** The gateway asks it about every message a
client sends, so the first version's round trip put a request and a reply in
front of every keypress -- the exact cost the fire-and-forget commands exist to
avoid. It is also faithful answered from this side: a character is placed by the
time Enter returns and stays placed until the session ends.

**The bug the browser found.** Everything above passed its tests, and the game
was unplayable. `Enter` is called with the *handshake* context, which is
cancelled the moment the handshake finishes -- so the subscription carrying
everything bound for the player's screen was closed before they finished
loading. The world node published into a subject nobody was listening on. Every
Go test passed because they all called `Enter` with `context.Background()`.

What it looked like in a browser: a character standing in a fully rendered
world, `hp 0/0`, and `net 0 snaps`. There is now a test that passes a context
shaped like the real one and cancels it, which fails without the fix.

| Claim | How it was checked |
| --- | --- |
| A gateway with no world node can play | Ten tests against a `RemoteWorld` holding no Node, on its own bus connection |
| Input crosses two processes | `TestAGatewaysInputMovesTheCharacter`, and a browser: pos 212 → 248, `0.00 px (0 hard)` misprediction |
| Messages keep arriving after login | `TestAGatewayKeepsReceivingAfterTheHandshakeContextEnds` |
| A refusal arrives as a refusal | `TestAGatewaySeesARefusal` |
| A busy character is refused, not hunted for | `TestAGatewayDoesNotShopAroundForABusyCharacter` -- the lease is cluster-wide, so every node would say the same thing |
| An empty cluster says so | `TestAGatewaySaysWhenThereIsNoWorldNode` -- different from "everything refused" |
| A gateway must say where to send messages | `TestAWorldNodeRefusesAGatewayWithNoReturnAddress` -- otherwise a player holds their character hostage and sees nothing |
| The guards hold | 20 mutations across both sides, all caught |
| It actually works | Two processes, a browser, a character created, entered, and moved: 489 snapshots, HP regenerating, nine other entities |

**Known limit:** a gateway picks a world node at random from the ones the
directory says are alive. Random rather than least-loaded because the
directory's load counter counts rooms, not sessions, and "the node hosting the
fewest rooms" is not an answer to "where should this player go". A real
answer needs session counts, which belongs with the load test.

### Deploying it

The manifests live in the homelab cluster repo rather than here, alongside the
Postgres and Dragonfly components the app already composes:
`apps/base/projects/mmo/release/`. One `--roles=all` HelmRelease became three --
gateway, world, and the NATS the two halves talk over.

**The world nodes are a StatefulSet, for the names rather than for storage.**
There are no volumes and nothing on disk survives a restart. A world node
identifies itself to the directory by hostname, and the directory keeps every
node it has seen in registration order so placement is stable across restarts.
Under a Deployment every rollout invents new hostnames: a new node each deploy,
and a registration list that only grows. `mmo-world-0` comes back as itself.

**Their Service is headless.** Nothing load-balances a world node -- a gateway
addresses one by name over the bus after the directory says which, and a room is
reached the same way. A virtual IP in front of them would be a hop no traffic
takes.

**The gateway is pinned to one replica, and not because it cannot scale.** It
runs no simulation and holds no character state, which is the entire point of
splitting it out. It is one because `SESSION_SECRET` is not set yet: without it
each gateway generates its own signing key at boot, so a ticket issued over HTTP
by one gateway is rejected by whichever gateway the WebSocket lands on. With one
replica that is invisible; with two it is a login that fails about half the time.

**Registering is an offer to host rooms, and a gateway must not make it.** This
was the bug writing the manifests found: `NewRedis` registers the process as a
placeable node and heartbeats, so a gateway that opened a Redis directory
appeared in `LiveNodes` -- and placement would eventually choose it to run a map
it has no simulation for. The player sent there waits out a timeout on a room
nobody is going to start. A gateway now opens `NewRedisReader`, which never
registers, and `TestRedisReaderIsNeverPlacedOn` asserts both halves of that: it
is not listed as live, and a room placed through it still lands on a world node.

Verified by running it: two world nodes and a gateway as three processes against
one Redis, NATS and Postgres. Only `world-1` and `world-2` appear in the
directory. A character created and played through the gateway got 180 snapshots,
took damage from mobs simulated on `world-1`, and the gateway logged no errors.

### The bot client

`cmd/mmobot` drives a population of headless players at a running server:

```
mmobot --server=http://localhost:8088 --bots=200 --ramp=20s --duration=2m
```

**Every bot takes the browser's path.** Dev sign-in, pick a character, ask for a
ticket over authenticated HTTP, open a socket, say hello. A load tool that
skipped any of it would be measuring something the game does not do -- and the
ticket in particular is the whole reason a gateway can be scaled at all.

Building it found the wire format the hard way, twice. Both are the kind of
thing only a real client hits:

- **Every frame is an `Envelope`**, even when it holds one message, because the
  server batches a tick's worth into one. A bare `ClientMessage` decodes as an
  envelope containing nothing, and the server says "malformed handshake" --
  which sounds like a framing bug in the transport rather than the protocol
  working exactly as designed.
- **The handshake carries a content hash**, and the server refuses a client
  whose content does not match. The bot asks `/healthz` for the protocol version
  and the hash rather than compiling them in, so a bot built against a different
  commit fails at the handshake with a message that says so.

**The bots behave like a population, not a benchmark.** Each turns around on its
own schedule and casts on its own beat, jittered -- a thousand clients acting in
lockstep produces a load pattern no real population makes, and would hide
exactly the smoothing the room's action tick exists to provide. They walk
through portals, so a run that starts in the tutorial ends up spread across
maps.

**The summary reports the peak, not the final count.** It is printed after
everyone has hung up, so reporting the live number said "0 of 150 connected" for
a run where all 150 played happily.

**A dead gauge, found by looking at what the load produced.**
`mmo_world_instances` was registered and never set, so it read zero for the life
of the process -- on a dashboard indistinguishable from a node hosting nothing,
which is the thing you would be looking at the dashboard to find out. The node
now reports its room count whenever it changes.

Measured on one machine, one process, 150 bots:

| | |
| --- | --- |
| input | 3000/s |
| snapshots | 3000/s, **20.0 per bot** -- the full tick rate, nobody starved |
| round trip | p50 650µs, p99 1.1ms, max 4ms |
| bandwidth | 33 KiB/s up, 1.2 MiB/s down |
| failures | none, no drops, no kicks |

That is the client's side. The server's own tick times are on its admin port,
and the two are meant to be read together: `mmo_room_tick_duration_seconds` says
whether it kept up, and these say what it was keeping up with.

### The dashboard

`deploy/grafana/mmo.json` — fourteen panels in three rows, in the order the
questions get asked. Is it keeping up (tick p99 against the 50 ms budget,
overruns). Who is on (characters and entities per map, connections, instances).
Throughput and backpressure (input received against dropped, snapshots out,
and dropped *output*, which is the server giving up on a client rather than
letting a tick block).

It lives here rather than in the cluster repo, and that is the point. **A
dashboard is the one piece of a system nothing compiles and nobody tests**, and
its failure mode is a panel that renders an empty graph -- which looks exactly
like a system doing nothing. `mmo_world_instances` was registered and never set
for months and the way that was found was somebody happening to look.

So there are two tests, and they point in opposite directions:

- `TestDashboardOnlyUsesRealMetrics` -- every metric the dashboard names is one
  the server exports. Renaming a metric in the code now fails a test rather than
  emptying a panel.
- `TestEveryMetricIsOnTheDashboard` -- every metric the server exports is on the
  dashboard, or listed with a reason for not being. This is the direction that
  catches a metric added and then forgotten, which is how a system ends up
  instrumented and unobserved.

**A counter that does not exist yet reads as a broken metric.**
`mmo_room_tick_overruns_total` is label-partitioned by map, and a Prometheus
counter vec has no series until something increments it -- so the overrun panels
showed "No data" on a perfectly healthy server, which on a dashboard is the same
shape as a renamed metric or a target nobody is scraping. The one time you need
to trust that panel is the one time it has never been touched. `ObserveTick` now
resolves the counter whether or not the tick overran, so a healthy map reports
zero.

**Verified against a real Prometheus**, not by reading the JSON: a server in a
container, a Prometheus scraping it, thirty bots driving load, and every one of
the dashboard's twenty queries evaluated through the HTTP API. All twenty return
data -- which is the only way to know that `histogram_quantile` over
`..._bucket` with a `le` grouping is written correctly, and the check that
turned up the overruns problem.

The cluster side -- two `VMServiceScrape`s on the admin port and a
`GrafanaDashboard` fetching the JSON from this repo -- is in the homelab
repository. Nothing was scraping the server before, so the dashboard would have
had nothing to draw.

**A misconfiguration found while setting this up.** Sharing a room directory
without sharing a bus is broken by construction: the directory places rooms on
any node registered in it, a node is reached over the bus, so rooms land on
nodes the process cannot talk to. Every component reports itself healthy and the
only symptom is logins failing with "could not enter the world". A Redis
directory with an in-process bus is still perfectly good for one node -- it is
the only node in there -- so this is a warning at startup when another node is
actually registered, not a refusal.

### A thousand bots

`deploy/loadtest.sh --bots=1000 --worlds=3 --gateways=2`. Three world nodes and
two gateways as separate processes against one Postgres, Redis and NATS, with
the bots split across the gateways the way a load balancer would.

| | |
| --- | --- |
| bots connected | **1000 of 1000**, none dropped, none refused |
| input | 20,000/s |
| snapshots | 20,000/s, **20.0 per bot per second** -- the full tick rate |
| bandwidth | 220 KiB/s up, 8.4 MiB/s down |
| round trip | p50 610µs, p99 1.3ms |
| room instances | 36 across three nodes |
| **tick p99** | **1.90ms / 1.91ms / 1.91ms** against a 50 ms budget |
| **overruns** | **zero** |
| server CPU | ~3.1 of 12 cores for all five processes |

**What this proves and what it does not.** Everything runs on one machine, so
this measures the machine as much as the design -- though the interesting part is
that the *servers* used about three cores while the machine as a whole sat at
load 24, and the tick loop was never starved. Roughly 320 players per core.

It is also a thousand players across **thirty-six rooms**, not a thousand in
one. That is the shape the design intends -- capacity per instance, channels
above it -- but a room's tick cost is what was measured, and no room held more
than thirty.

**Two gateways, which the deployment cannot do yet.** The load test passes a
shared `SESSION_SECRET`, because a ticket is issued over HTTP by one gateway and
redeemed on the socket by whichever one the client lands on. That is the single
thing keeping the deployed gateway at one replica, and this is the first time
the multi-gateway path has been exercised at all.

**The bug the load test was for.** At 300 bots, 163 of them were dropped with
"your character was claimed elsewhere" and the server logged 163 fenced
checkpoints. Nothing had claimed anything: the fencing counter lives in Redis
and the tokens it is compared against live in Postgres, so a counter that
restarts below the database's high-water mark issues tokens the fencing
predicate correctly rejects -- and every character that has played before loses
its next checkpoint.

The in-memory lease implementation has always seeded itself from the database
for exactly this reason, with a comment saying so. The Redis one did not. Redis
is described throughout this document as reconstructible, and losing it as
costing a disconnect rather than data; that counter is the one value in there
where it is not true. An eviction policy, a flush, or a restore from an empty
snapshot all lose it -- and the homelab deployment runs Dragonfly with a
`maxmemory` limit.

`RedisLeases.Seed` now raises it, monotonically, so several nodes seeding from
the same database cannot undo each other. It warns only when it actually had to
raise it: warning on the healthy case means warning on every start, which is how
a real warning gets ignored.

**Three findings that were the harness, not the server**, each of which read as
a server fault first:

- Both bot processes were given the same name prefix (`printf '%c' 97` prints
  `9`, not `a`), so two sets of bots fought over one set of characters and half
  were refused. The lease was right; the test was wrong.
- Runs back to back collide for `LeaseTTL`: a killed server does not release
  its leases, so the next run's bots are refused with "already in play" for
  thirty seconds. The script waits them out now.
- Failures were grouped by the first words of the error, which turned 157
  distinct causes into 157 identical lines reading "waiting for the welcome" --
  a count of how many bots were affected and no clue why, which is the one thing
  the run was for. Grouping prefers the close status now.

### Killing a world node

`deploy/loadtest.sh --bots=150 --worlds=3 --gateways=2 --kill-after=45`. SIGKILL,
not SIGTERM: a graceful shutdown is a different test, and the point here is the
case nobody gets to prepare for -- the process is gone without releasing a
lease, checkpointing, or telling anybody, and recovery has to come from state
that was already durable.

It found two bugs, and neither was the one the item was written to check. Leases
expired exactly as designed. What did not work was everything around them.

**A gateway did not notice its world node had died.** The calls a connected
player makes constantly are published and not waited for -- that is the whole
point of them -- so a gateway whose world node is gone keeps accepting input and
forwarding it to a subject nobody is subscribed to. The first chaos run said it
plainly: at the kill, snapshots went from **20.0 per player per second to 8.8,
where they stayed for the remaining 105 seconds**, while the gateway reported
every player as `up / 0 failed` the whole time. A third of the players were
sitting in a world that had stopped moving, with an open connection and no
error, and they were never told -- so they never reconnected, so they never
recovered.

`RemoteWorld` now watches the directory for the nodes holding its players. One
call per gateway rather than one per player, and a node has to be missing from
two consecutive checks before its players are disconnected -- node liveness is
already a TTL three heartbeats deep, so a stall in the gateway should not be
enough on its own to throw everybody off a node that is fine.

**A dead node's rooms stayed in the directory and kept being handed out.** A room
is hosted by a process; when that process dies the room dies with it, but its
registration does not. Two rooms stayed registered on the killed node holding
**thirty and nineteen phantom players**, and every reconnecting player placed
into one waited out a request to a node that would never answer. Forever,
because nothing removed the registration -- which is why the second chaos run
still had sixteen players who never came back and eighty-three reconnects
refused with "could not enter the world".

Placement now skips a room whose host is not alive, and reaps it on the way
past: left in place it keeps its slot count and its share of the node's load
forever, and every future placement steps over it again. The same check guards a
channel switch, and the channel *list* stops advertising doors that lead
nowhere. A private room is the sharpest case -- one instance by definition, so
counting the dead one told a party their dungeon was full for the rest of time.

With both fixed:

| | |
| --- | --- |
| players at the kill | 150 across three nodes |
| dropped | 48, all told why |
| reconnected | **48 of 48** |
| back to full | ~75 seconds |
| **progress lost** | **none -- every character came back with everything it had** |
| reconnects refused | none |
| surviving nodes | tick p99 0.89–1.25ms, zero overruns, rooms held 20 Hz |

**The measurement was wrong first.** The bots compared cumulative experience
before and after, on the assumption that it only goes up. It does not: dying
charges a share of the progress made toward the current level, and these bots
run at mobs constantly. The first version reported hundreds of characters
"losing progress" in a run where nothing had gone wrong. It now compares only
across a reconnect, which is the only place a crash can cost anything.

**Bots reconnect now**, which is what makes "characters recover" a thing that
can be measured rather than asserted. Backoff starts above a lease TTL --
until the dead node's lease lapses the character genuinely is still owned, and
asking sooner only collects refusals -- and is jittered, because a node dying
disconnects everyone it was holding at once and a thousand clients retrying in
lockstep is a thundering herd at the moment the cluster has least to spare.

**Two things this left open**, both recorded rather than guessed at:

- ~~`mmo_room_players` and `mmo_room_entities` are labelled by map alone.~~
  **Fixed** -- see "Nothing was missing" below.
- ~~Under higher load the snapshot rate settles below the tick rate.~~
  **Resolved, and it was not happening.** See "Nothing was missing" below: the
  figure was a whole-run average including the ramp, and the steady rate at a
  thousand players is the full twenty per second.

### Leaving on purpose

The chaos item established what an unannounced death costs. A drain is the same
shutdown with the order reversed:

1. **Withdraw from placement**, so nothing new arrives at a process that is
   leaving. `Directory.Withdraw` removes the liveness entry rather than waiting
   for it to expire -- otherwise a character arrives fifteen seconds into a
   shutdown, at the one node that cannot look after it.
2. **Stop accepting characters** handed over by other nodes, for the same reason.
3. **For each character: checkpoint, release the lease, and only then tell the
   player.**
4. **Give up the rooms**, so nothing is left registered on a node that has gone
   -- the state a killed node leaves behind for placement to reap.

**Step 3's order is the whole point.** Telling the player first would be
friendlier: their client could be reconnecting while this node finishes writing.
But a client that reconnects quickly lands on another node and takes the lease
before the checkpoint here is written, and the fencing predicate then correctly
rejects it -- the player losing exactly the progress a drain exists to protect.
So: write, release, then speak. There is a test that asserts the lease is
already free at the moment the player is told, because the ordering is the only
thing making this better than a kill.

**The first version could not finish.** It timed out at its full twenty-second
budget with a quarter of the characters never seen off. Two reasons, and the
second was the real one:

- `Close` waits for the session's own goroutine, bounded by a transfer's
  timeout, because a transfer halfway through moving a character must not be cut
  in half. Done one character at a time that is fifty such waits in series. Every
  session is now told to wind down before any of them is waited for, so the
  waiting happens at once.
- **Sixteen concurrent checkpoints against a database pool of ten.** More
  concurrent writers than connections does not make the writes faster; it makes
  them queue inside the pool, where the wait is invisible and counts against the
  deadline just the same. A quarter of the characters spent the entire budget
  waiting for a connection. The drain now sizes itself to `Store.MaxConns`, and
  gives each character a slice of the remaining budget rather than letting one
  stuck write spend all of it.

After both: **54 characters seen off in 6.7 seconds.**

| | after a kill | after a drain |
| --- | --- | --- |
| players told | only once the gateway noticed | immediately, with a reason |
| lease | waits out its TTL | released |
| rooms | left registered, reaped later by placement | given up |
| back to full | ~75 seconds | ~75 seconds |
| snapshot rate after | settled at 15.2 per player | back to **20.0** |

The snapshot rate returning to full while the kill case did not is a real
difference, and it is recovery churn rather than anything structural: after a
kill, characters are still reconnecting and transferring for a while, and a
player mid-transfer is frozen and sent nothing. There is now a gauge for that.

**One residual, stated rather than explained.** Across 93 recoveries, 91
characters came back with everything they had and two came back about twelve
experience short. That window overlapped the harness tearing the cluster down,
and a control run produces no reconnects at all to compare against, so I could
not attribute it to the drain. It is not nothing and it is not established.

### Nothing was missing

Two of M9's write-ups carried an open question: under load the snapshot rate
seemed to settle below the tick rate -- 15.9 per player at a thousand against
20.0 at a hundred -- while rooms ticked at exactly 20 Hz with zero overruns and
nothing dropped anywhere. Ticks being simulated that somebody was not being sent.

**There was no shortfall.** The figure was the load tool's whole-run average,
which includes the ramp: a run spending forty-five seconds connecting a thousand
bots and then ninety seconds at full load has an average well below the steady
rate, because most of that first window had most of the bots not yet connected.
The tool labelled it as a whole-run average and I read past the label, twice.
Measured after the ramp, a thousand players receive **20.0 snapshots a second
each**, which is every tick.

The corrected numbers are in the load test table above. They are better than
what was reported: 20,000 inputs and 20,000 snapshots a second, 8.4 MiB/s out.

The summary now measures from the end of the ramp and says so, because a label
that gets read past is a label that does not work.

**The hypothesis was wrong too, which is how the instrumentation got built.**
The suspicion was frozen players: the snapshot phase skips a player whose
connection dropped or who is mid-transfer, so a room can tick perfectly and
still send some of its players nothing. That is real, and worth a gauge -- from
outside it is indistinguishable from the server failing to keep up. But measured
mid-run at both six hundred and a thousand players it was **zero**, and the
snapshot rate was twenty. The mechanism exists and was not the cause.

`mmo_room_frozen` is that gauge, with a panel that says what it is for.

**And a real bug, found on the way.** `mmo_room_players` and
`mmo_room_entities` were labelled by map alone, and every room set its own count
under that label -- so a node hosting three channels of one map reported
whichever channel ticked last. The dashboard understated the population by
however many channels it did not happen to count, silently, and the only clue
was the number looking a little low. It was reporting about thirty players per
map where the truth was two hundred and fifty.

The room observer now takes a `TickReport` carrying the instance, and the
metrics keep per-room figures internally while exporting the per-map total --
one series per map rather than one per room, since rooms come and go and a
gauge labelled by instance would accumulate a series for every channel the
process had ever run. A retiring room is reported too, or its last headcount
would count forever and the total would only rise.

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
