# Data Model

Covers persistence, the invariants that prevent item duplication, and the stat/progression math.

## Two kinds of state

| | Live state | Durable state |
|---|---|---|
| Where | Memory, on the owning world node | Postgres |
| Changes at | 20 Hz (tick rate) | Checkpoint rate (~30 s) |
| Examples | Position, velocity, HP, cooldowns, buffs, aggro | Character, items, skills, guild, allowlist |
| On crash | Lost | Survives |

**Postgres is a checkpoint store, not the simulation's working memory.** Writing character position at 20 Hz would melt the database and buy nothing — position is worthless a second later. The design accepts losing up to one checkpoint interval of progress on an unclean crash, and is careful about *which* things are allowed to be in that window (§ What must never be lost).

## The single-writer invariant

> **At any instant, exactly one process may mutate a given character.**

Item duplication — the defining bug of every MMO that has ever shipped one — is almost always a violation of this. Two processes load the same character, both believe they own its inventory, both write. One sword becomes two.

Enforced with a **lease plus a fencing token**:

```
acquire:  INCR  lease:counter                        → token (monotonic, global)
          SET   lease:char:{id} "{node}:{token}" NX PX 30000
renew:    every 10s while held (Lua CAS: renew only if still ours)
release:  Lua CAS delete — only if still ours
```

Holding the lease is not sufficient on its own. A node can be paused by GC or a network partition long enough for its lease to expire and be granted elsewhere, then wake up and write with a lease it believes it still holds. So the token is carried into the database and checked on every write:

```sql
UPDATE characters
   SET state = $1, lease_token = $2, updated_at = now()
 WHERE id = $3
   AND lease_token <= $2;   -- a stale writer's token is lower; the write is rejected
```

`UPDATE ... WHERE lease_token <= $token` is the actual guarantee. Redis provides *mutual exclusion most of the time*; the fencing check in Postgres provides *correctness always*. A rejected write means the node lost ownership: it must discard its in-memory copy rather than retry, and log loudly — a rejected fenced write is always a bug or a real partition, never routine.

Consequences:
- A character is loaded from Postgres exactly once per session, at lease acquisition.
- Room handoff transfers the lease explicitly (`architecture.md` § Room handoff).
- `lease_acquire_failures_total` and `fenced_write_rejected_total` are alerting metrics, not debug counters.

## Items

### Every item is an instance with exactly one location

```sql
CREATE TABLE item_instances (
    id           uuid PRIMARY KEY,
    base_id      text NOT NULL,          -- content key, e.g. "sword.iron"
    rarity       item_rarity NOT NULL,   -- normal | magic | rare | unique
    item_level   int NOT NULL,
    affixes      jsonb NOT NULL,         -- ROLLED values, never re-derived
    stack_size   int NOT NULL DEFAULT 1,

    container_id uuid NOT NULL REFERENCES containers(id),
    slot         int  NOT NULL,

    created_at   timestamptz NOT NULL DEFAULT now(),

    UNIQUE (container_id, slot)
);
CREATE INDEX ON item_instances (container_id);
```

Three rules, and they are load-bearing:

1. **`container_id` is `NOT NULL`.** There is no "in limbo" state. An item is always somewhere: an inventory, an equipment slot, a bank tab, a trade window, a mail attachment, or the ground. Nullable ownership is how items get lost and how they get duplicated, because every code path then needs to handle the null case and one of them won't.

2. **`UNIQUE (container_id, slot)`** — the database refuses to put two items in one slot. Not an assertion in Go that can be skipped by a new code path.

3. **Moving an item is an `UPDATE` of `(container_id, slot)`.** Never `DELETE` + `INSERT`. A delete-then-insert that fails between the two statements destroys an item; a retry of the insert alone duplicates one.

Ground drops and trade windows are real containers with real rows, not special cases. Uniformity is the point: one code path, one set of constraints, one place for the bug to not be.

### Rolled affixes are stored, never re-derived

```json
{
  "implicit": [{"id": "phys_dmg_flat", "roll": [4, 7]}],
  "prefixes": [
    {"id": "phys_dmg_pct", "tier": 3, "roll": [62]},
    {"id": "flat_str",     "tier": 5, "roll": [24]}
  ],
  "suffixes": [
    {"id": "crit_chance_pct", "tier": 2, "roll": [31]}
  ]
}
```

The item generator rolls once, at drop time, using the room's seeded PRNG, and the result is stored. Re-deriving stats from a seed at load time means a content rebalance silently rewrites items already in players' stashes — the fastest way to lose a playerbase.

Affix *definitions* (name, tiers, value ranges, spawn weights, allowed base types) live in content files; only the rolled outcome lives in the database.

### The item journal

Every item movement appends a row. Append-only, never updated:

```sql
CREATE TABLE item_events (
    id         bigserial PRIMARY KEY,
    item_id    uuid NOT NULL,
    kind       text NOT NULL,   -- drop|pickup|trade|equip|vendor|craft|destroy|mail
    from_container uuid,
    to_container   uuid,
    actor_char_id  uuid,
    tick       bigint,
    at         timestamptz NOT NULL DEFAULT now()
);
```

This costs one insert per item move — trivial next to the tick loop — and buys three things worth far more: duplication becomes *detectable* (an item with two concurrent `to_container` events), support requests become answerable, and a dupe exploit becomes *reversible* instead of a server wipe.

Build this in M3, when items land. Retrofitting an audit log after a dupe has already happened means you cannot tell which items were legitimate.

### Trades are one transaction

Both parties lock their offer, both confirm, then the server executes a **single Postgres transaction** moving every item on both sides plus currency. No intermediate state is ever visible; there is no window where items exist on both sides or neither.

```
BEGIN
  SELECT ... FOR UPDATE on both containers   -- consistent lock ordering by container id
  verify offers unchanged since confirm
  UPDATE item_instances SET container_id, slot   -- both directions
  UPDATE characters SET gold                     -- both directions
  INSERT item_events                             -- both directions
COMMIT
```

Locks are acquired in a deterministic order (sorted by container UUID) so two simultaneous trades between the same pair cannot deadlock. Both characters' leases must be held by the executing node; cross-node trades route through the node holding one lease after transferring the other.

## Schema sketch

```sql
-- Identity ---------------------------------------------------------------
accounts        (id, created_at, last_login_at, banned_until, notes)
identities      (id, account_id, provider, subject, email, UNIQUE(provider, subject))
allowlist       (id, provider, match_kind, match_value, note, added_by, added_at)
                -- match_kind: subject | email | email_domain

-- Characters -------------------------------------------------------------
characters      (id, account_id, name UNIQUE, class_id, level, exp,
                 map_id, spawn_point, gold,
                 state jsonb,          -- HP/MP, cooldowns, buffs, position
                 lease_token bigint NOT NULL DEFAULT 0,
                 created_at, deleted_at)

character_stats     (character_id, allocated jsonb)      -- manual stat points
character_passives  (character_id, node_id, PRIMARY KEY(character_id, node_id))
character_skills    (character_id, skill_id, rank, PRIMARY KEY(character_id, skill_id))
secondary_skills    (character_id, skill_id, exp, PRIMARY KEY(character_id, skill_id))
waypoints           (character_id, waypoint_id, unlocked_at)

-- Items ------------------------------------------------------------------
containers      (id, kind, owner_type, owner_id, capacity)
                -- kind: inventory | equipment | bank | trade | mail | ground
item_instances  (...)   -- above
item_events     (...)   -- above

-- Social -----------------------------------------------------------------
guilds          (id, name UNIQUE, created_at, leader_char_id, motd, level)
guild_members   (guild_id, character_id, rank, joined_at)
friends         (character_id, friend_char_id, PRIMARY KEY(character_id, friend_char_id))
chat_mutes      (character_id, channel, until, reason)
```

Parties are **not** persisted — they are session state in Redis. A party does not survive a server restart, and pretending otherwise creates orphaned rows nobody cleans up.

## What must never be lost

The 30-second checkpoint window is acceptable for position and combat state. It is **not** acceptable for anything a player would notice losing. These write through to Postgres immediately, before the client is told they succeeded:

- Item acquisition from a boss or a rare drop
- Trades and vendor transactions
- Currency changes
- Level-ups and passive point allocation
- Waypoint unlocks and quest state
- Anything purchased or crafted

Everything else rides the periodic checkpoint. The rule of thumb: **if losing it would make a player file a ticket, write it through.** A player who loses 30 seconds of trash-mob exp shrugs; a player who loses the unique that dropped off a boss quits.

## Stats

Path of Exile's stat model, because it is the reason PoE builds are interesting, and it is *specifically* the additive/multiplicative distinction that makes it work:

```
final = (base + Σ flat_added)
        × (1 + Σ increased)         ← ADDITIVE with each other
        × Π (1 + more_i)            ← MULTIPLICATIVE with each other
```

Three sources of `+40% increased damage` give `+120%`, total. Three sources of `40% more damage` give `2.74×`. This is what makes "more" multipliers build-defining and rare, and "increased" the common currency of gear — and it gives players a real optimisation problem instead of a single best item at every slot.

The stat engine recomputes on any change to equipment, passives, buffs, or level, caches the result, and invalidates on change. It never runs inside the tick's hot path.

```go
type StatBlock struct {
    Base      map[StatID]Fixed
    Flat      map[StatID]Fixed
    Increased map[StatID]Fixed   // summed
    More      map[StatID][]Fixed // multiplied
}
```

All values are **fixed-point**, matching the simulation (`protocol.md` § Delta compression). Floating-point damage numbers that differ in the last bit between client preview and server resolution generate bug reports forever.

### Damage resolution

```
1. weapon base damage roll                    (seeded PRNG, server-side only)
2. + flat added damage
3. × (1 + Σ increased damage)
4. × Π (1 + more damage)
5. crit? roll chance → × crit multiplier
6. − mitigation:  reduction = armour / (armour + 10 × incoming)   -- diminishing
7. − elemental resistance (capped, e.g. 75%)
8. floor at 1
```

Step 6 is PoE's armour formula: strong against many small hits, weak against one large hit. It gives armour and resistance genuinely different roles instead of being two words for the same thing.

## Progression curves

Both curves are **generated from a formula into a checked-in table**, then loaded as content. Designers tune the table; nobody redeploys Go to rebalance levelling.

**Main level (1–200), MapleStory-flavoured** — steep enough that levels feel earned, shallow enough early that a new character reaches interesting skills quickly:

```
expToNext(L) = floor(A · L^2.4 · (1 + B)^L)
```

**Secondary skills (1–99), OSRS's actual curve**, because it is well-tuned and instantly legible to anyone who has played it:

```
xpAt(L) = floor( (1/4) · Σ[i=1..L-1] floor( i + 300 · 2^(i/7) ) )
```

Secondary skills level from *use*, not from combat exp: woodcutting rises by chopping, fishing by fishing. They resolve on the 600 ms action tick (`architecture.md` § The tick loop), deliberately matching OSRS's cadence.

## Content versus database

The line, stated once because it gets blurred constantly:

| Content files (git) | Database |
|---|---|
| Item base types, affix pools and tiers | Rolled item instances |
| Mob stats, AI profiles, drop tables | Nothing — mobs are ephemeral |
| Skill and passive definitions | Which passives a character allocated |
| Map geometry, spawn points, portals | Which waypoints are unlocked |
| Exp curves, damage constants | Character level and exp |

**Rule: content files define what *can* exist; the database records what *does* exist.**

Rebalancing an affix pool changes future drops and leaves existing items untouched — which is what players expect, and what the "store rolled values" rule above delivers.

## Migrations

Plain SQL, sequentially numbered, forward-only, in `internal/store/migrations/`. Applied on boot after acquiring an advisory lock so that multiple starting nodes cannot race.

Forward-only is deliberate: down-migrations are written for a rollback that almost never happens, are almost never tested, and give false confidence. Roll forward, and treat schema changes as expand → backfill → contract so a deploy is never simultaneously required with a schema flip.
