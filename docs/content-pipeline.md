# Content Pipeline

Every item, mob, skill, passive, drop table, map, and curve is a declarative file in git. No content change requires writing Go.

This is the single highest-leverage decision in the project. An MMO is 20% engine and 80% content, and a project where adding a skill means writing a Go function ships ten skills. One where adding a skill means writing twelve lines of TOML ships three hundred.

## Layout

```
content/
  items/        bases.toml, affixes.toml, uniques.toml
  mobs/         *.toml               (stats, AI profile, abilities)
  elites/       modifiers.toml       (rolled onto enhanced mobs)
  skills/       *.toml               (active skills, effect lists)
  buffs/        *.toml               (buffs, debuffs, DoTs, auras)
  passives/     tree.json            (the passive tree — tool-authored)
  classes/      *.toml
  droptables/   *.toml
  maps/         *.tmj                (Tiled) + *.meta.toml
  curves/       exp.toml, secondary_exp.toml
  world/        waypoints.toml, portals.toml
  balance.toml  global constants
```

TOML for hand-authored files: comments survive, diffs are readable, designers can edit it. JSON only where a file is tool-generated and too large to hand-edit (the passive tree). TMJ because it is Tiled's format and Tiled is the right map editor to not write ourselves.

## Loading

At boot, and on SIGHUP in development:

```
1. parse      → all files, fail on syntax error with file:line
2. validate   → schema per type: required fields, ranges, enums
3. link       → resolve every cross-reference into a direct pointer
4. verify     → referential integrity across the whole graph
5. hash       → sha256 over canonicalised content → CONTENT_HASH
6. freeze     → immutable, shared, read-only for the lifetime of the process
```

**Boot fails on invalid content.** Never start with partial content and never fall back to defaults — a server that silently starts with a broken drop table produces bug reports weeks later that trace back to a startup warning nobody read.

Step 4 catches the errors that actually happen in practice:

- a drop table references an item base that does not exist
- a skill applies a buff that was deleted
- a portal targets a map that was renamed
- a passive node grants a stat ID nothing reads
- a mob references a drop table that no longer exists
- a skill costs a resource its class does not have

CI runs the validator on every PR, so content breaks fail the build rather than the server.

Loaded content is **immutable and shared**. Rooms read it concurrently with no locking, which is what keeps the tick loop cheap.

The client fetches the same content (or a public projection of it — drop rates and exact AI thresholds stay server-side) and both sides compare `CONTENT_HASH` at handshake (`protocol.md` § Version and content gating).

## Skills: the effect DSL

A skill is **targeting plus an ordered list of composable effects**, never a Go function. This is what makes hundreds of skills — and PoE-style support interactions — tractable.

```toml
[skill.fire_slash]
name        = "Fire Slash"
class       = "warrior"
max_rank    = 20
cost        = { mp = 12 }
cooldown_ms = 2000
cast_time_ms = 300
animation   = "slash_2h"

[skill.fire_slash.targeting]
kind        = "cone"      # self | cone | circle | line | projectile | target
range       = 180
angle       = 90
max_targets = 6

[[skill.fire_slash.effects]]
type     = "damage"
element  = "fire"
base     = { min = 40, max = 60 }
scaling  = { str = 1.4, int = 0.2 }
per_rank = { base_pct = 8 }        # +8% base damage per rank

[[skill.fire_slash.effects]]
type        = "apply_buff"
buff        = "burning"
duration_ms = 4000
chance      = 0.35
```

### Effect vocabulary

| Type | Effect |
|---|---|
| `damage` | Roll and apply damage through the full pipeline |
| `heal` | Restore HP |
| `restore` | Restore a resource (MP, rage, ...) |
| `apply_buff` / `remove_buff` | Buffs, debuffs, DoTs |
| `shield` | Absorb pool with its own duration |
| `dash` / `teleport` / `knockback` | Movement, resolved by `sim` |
| `spawn_projectile` | Entity with its own effect list on hit |
| `area_persist` | Ground area applying effects on a tick interval |
| `summon` | Temporary allied entity |
| `chain` | Re-apply to nearby targets with falloff |
| `trigger_skill` | Cast another skill — enables combos and procs |

New effect *types* are Go code and should be rare. New *skills* are TOML and should be constant. If you find yourself adding an effect type per skill, the vocabulary is factored wrong.

### Support modifiers

PoE-style supports are transformations applied to a skill's resolved effect list at compute time — never at authoring time:

```toml
[support.multistrike]
tags     = ["melee"]              # only attaches to skills carrying these tags
mana_mult = 1.6
[[support.multistrike.modify]]
target = "effects.damage"
more   = -0.25                    # 25% less damage
repeat = 3                        # ...three times
```

Because supports operate on the effect list rather than on hardcoded skill knowledge, a new support automatically works with every existing compatible skill, and a new skill automatically works with every existing compatible support. That combinatorial property is the entire appeal of the system, and it only exists if supports never special-case individual skills.

## Buffs

```toml
[buff.burning]
name        = "Burning"
kind        = "debuff"
duration_ms = 4000
tick_ms     = 500
stacks      = { max = 5, refresh_on_apply = true }
dispellable = true

[[buff.burning.effects]]
type    = "damage_over_time"
element = "fire"
base    = 8
scaling = { attacker_int = 0.3 }

[[buff.burning.stat_mods]]
stat = "movement_speed"
increased = -0.15
```

Buffs both tick effects and contribute stat modifiers, feeding the same stat pipeline as gear and passives (`data-model.md` § Stats).

## Mobs and enhanced mobs

```toml
[mob.slime_green]
name  = "Green Slime"
level = 5
hp = 120 ; mp = 0
stats = { str = 8, dex = 4, int = 2, armour = 15 }
resistances = { fire = -0.25, ice = 0.20 }
exp = 18
drop_table = "slime_green"
body = { w = 32, h = 28 }

[mob.slime_green.ai]
profile      = "aggressive_melee"   # state machine defined in Go
aggro_range  = 200
leash_range  = 500
move_speed   = 40
attack_range = 36

[[mob.slime_green.abilities]]
skill       = "slime_bounce"
weight      = 1.0
cooldown_ms = 3000
```

AI *profiles* are Go state machines (idle → aggro → chase → attack → leash → dead); AI *parameters* are content. A handful of profiles covers most mobs; bosses get bespoke profiles because their mechanics are the encounter.

**Enhanced mobs** roll modifiers at spawn time from a weighted pool, PoE rare-mob style. Each modifier is stat mods plus optional effects plus a visual aura, so the combination is emergent rather than enumerated:

```toml
[elite_mod.vampiric]
name = "Vampiric"
weight = 10
stat_mods = [{ stat = "life_leech", flat = 0.15 }]
aura = "vfx/red_wisp"

[elite_mod.volatile]
name = "Volatile"
weight = 6
on_death = [{ type = "area_persist", radius = 90, duration_ms = 800,
              effects = [{ type = "damage", element = "fire", base = 60 }] }]
```

A champion rolls 1–2 modifiers, a rare 3–4, with exp and drop quality scaled accordingly. Two vampiric-volatile slimes are a different fight from two ordinary ones, at no content-authoring cost.

## Drop tables

```toml
[drop_table.slime_green]
gold = { min = 5, max = 20, chance = 0.80 }

[[drop_table.slime_green.entries]]
item = "potion.red_small" ; chance = 0.15 ; qty = { min = 1, max = 3 }

[[drop_table.slime_green.entries]]
group = "common_equipment"   # nested table, weighted
chance = 0.06

[[drop_table.slime_green.entries]]
item = "unique.slime_crown" ; chance = 0.0002 ; announce = true
```

All rolls use the room's seeded PRNG (`architecture.md` § Determinism and replay), so a drop is reproducible from a replay — which is how you answer "was that legitimate?" without guessing.

Party loot rules (free-for-all, round-robin, need/greed, or instanced per-player) are a *party* setting applied after the roll, not a property of the table.

## Maps

Authored in **Tiled**, exported as TMJ. Tile layers are geometry; object layers carry gameplay, typed by Tiled custom properties:

| Object type | Properties |
|---|---|
| `spawn_point` | `mob_id`, `respawn_ms`, `max_alive`, `elite_chance`, `layer` |
| `portal` | `target_map`, `target_spawn`, `requires` |
| `platform` | `one_way` (drop-through), `moving`, `path` |
| `rope` / `ladder` | climb geometry |
| `npc` | `npc_id`, `dialogue_id` |
| `event_trigger` | `event_id`, `condition`, `cooldown_ms` |
| `waypoint` | `waypoint_id` |
| `boss_arena` | `boss_id`, `phases`, `entry_requirements` |

`*.meta.toml` alongside each map carries what does not belong in Tiled: instance policy (`shared` / `private`), player capacity, BGM, level range, PvP flag, instance TTL.

**Each spawn point owns its own independent respawn timer** — MapleStory and OSRS behaviour, and a direct consequence of storing `respawn_ms` per object rather than running wave-based spawns.

`layer` decides who fights the mob (`architecture.md` § Axis 2):

- `owner` (default in hunting zones) — instanced per player, or per party when partied. Every layer gets its own copy of the spawn point with its own timer, so there is no contention and no kill stealing.
- `shared` (default in dungeons) — one copy for everyone in the room. Field bosses, zone events, rare spawns.

The two mix freely within one map, which is the point: a hunting zone can have private trash and a public field boss without either being a special case.

Spawn density is the main driver of tick cost under layering, because it multiplies by the number of active layers. Treat a map's `capacity` in `*.meta.toml` and its `owner` spawn count as a single tuning decision, not two.

The collision geometry the server simulates is derived from the tile layers at load and must match exactly what the client renders. It is generated from the same TMJ by the same code path, compiled to WASM — never hand-maintained twice.

## Passive tree

The tree is the one piece of content too large to hand-author. `content/passives/tree.json`:

```json
{
  "nodes": [
    {"id": 1041, "kind": "small",    "pos": [412, -890],
     "stats": [{"stat": "str", "flat": 10}]},
    {"id": 1042, "kind": "notable",  "pos": [455, -920], "name": "Blade Dancer",
     "stats": [{"stat": "attack_speed", "increased": 0.12},
               {"stat": "melee_damage", "increased": 0.20}]},
    {"id": 1043, "kind": "keystone", "pos": [520, -980], "name": "Glass Cannon",
     "stats": [{"stat": "damage", "more": 0.40},
               {"stat": "max_life", "more": -0.30}]}
  ],
  "edges": [[1041, 1042], [1042, 1043]],
  "class_starts": {"warrior": 1002, "mage": 2004, "ranger": 3001}
}
```

A small editor (M6) generates this; hand-editing a thousand-node graph is not a plan. The server validates on load that the graph is connected, that every class start exists, and that allocation from any start can reach every node.

Keystones use `more` multipliers with real drawbacks — that is what makes them build-defining choices rather than strict upgrades.

## Balance constants

Anything a designer might tune lives in `content/balance.toml`, never as a Go literal:

```toml
[combat]
crit_multiplier_base = 1.5
resistance_cap       = 0.75
armour_divisor       = 10
min_damage           = 1

[party]
exp_share_radius = 600
exp_bonus_per_member = 0.10
max_size = 6

[drops]
ground_ttl_ms       = 120000
owner_lock_ms       = 15000    # only the killer may loot, for this long
```

A magic number in Go is a number nobody can tune without a deploy, and every one of them eventually needs tuning.

## Hot reload

In development, `SIGHUP` re-runs load → validate → link → verify and atomically swaps the frozen content pointer if it succeeds. On failure the old content stays live and the error is logged.

Live rooms keep the content generation they started with, so a reload cannot change a boss's stats mid-fight. New instances pick up the new generation.

Production reloads content only on restart. The complexity of safely migrating live rooms across content generations is not worth it while a rolling restart costs a few seconds.
