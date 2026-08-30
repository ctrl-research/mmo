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
| `split_damage` | One hit, divided evenly among everyone it lands on |
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

Every kind is validated for the fields it actually needs. An effect missing them does nothing at all, and an effect that silently does nothing is the hardest kind of content bug to find: the skill casts, the animation plays, and the number never appears. `summon` is the one entry above not yet implemented — it needs AI ownership and pet commands, which is a milestone rather than an effect.

**One struct holds every kind**, rather than an interface per kind. A support rewrites effects it has never heard of, so every effect must be inspectable and copyable without knowing its type. The cost is fields that mean nothing for most kinds; the alternative costs the entire support system.

**References are resolved at load.** A skill that applies a buff that does not exist, or triggers a skill that triggers it back, fails the build — the loop check tracks the whole chain rather than only self-reference, because a three-skill cycle would not terminate inside a tick either.

**Buff durations are filled in at load** where a skill does not override them. Without that, a support that scales durations silently does nothing on every skill using a buff's default — which is nearly all of them.

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

Because supports operate on the effect list rather than on hardcoded skill knowledge, a new support automatically works with every existing compatible skill, and a new skill automatically works with every existing compatible support. That combinatorial property is the entire appeal of the system, and it only exists if supports never special-case individual skills. **There is deliberately no field for naming a skill.**

Three rules keep it honest:

- **Every tag, not any.** "melee" and "attack" together mean melee attacks; matching either would attach a melee support to a fireball.
- **Every support costs mana.** One that costs nothing is one everybody takes, and therefore not a decision.
- **Supports reach nested effects.** A fire support has to work on a fireball whether the fire is applied directly or carried by a bolt the skill launches.

Ordering matters twice. Rank is applied *before* supports, so a support multiplies the ranked damage rather than the rank-one damage — the other way round makes a support worth progressively less the more a skill is levelled. And the linked supports apply in the order they are linked, because two that both scale damage compose differently depending on which repeats first.

A triggered skill does **not** inherit the triggering skill's supports. A support that repeated an effect would repeat the trigger, and a support chain feeding itself is a loop the content check cannot see.

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

Buffs both tick effects and contribute stat modifiers, feeding the same stat pipeline as gear and passives (`data-model.md` § Stats). Keeping them one mechanism is the point: "burning" and "+20% attack for ten seconds" are the same kind of thing, which is why a support that lengthens durations works on both without knowing what either does.

Stacking is the interesting knob. A buff that **refreshes on apply** is one a player maintains; one that only **stacks** is one they build up and then spend. Stacks multiply both the modifiers and the ticked effects, so three stacks hit three times as hard rather than three times as often.

The room layers buffs over the stat block the session pushes in, rather than asking for a rebuilt one. The two change on completely different clocks — equipment when somebody equips something, buffs several times a second in a fight — and rebuilding from items every time a stack of Burning ticked would mean the room asking the session for stats mid-tick.

## Classes

A class is deliberately thin: a name, a starting position on the tree, and what it can cast. It is **not** a package of hard-coded mechanics, because the mechanics are skills and passives and those are data.

That thinness is what makes "two characters of the same class play differently" possible at all. If a class carried its own behaviour, the class would be the build.

```toml
[class.mage]
name = "Mage"
description = "Controls the ground, and would rather not be stood on."
primary_stat = "intelligence"
starting_skills = ["firebolt", "frost_nova"]
```

A class whose starting skill does not exist produces a character who cannot act, and the symptom is a button that does nothing — so it fails the build.

Which skills a class *may* learn comes from the skill's own `class` field rather than a list on the class: adding a skill to a class is one line in the skill, rather than two files that can disagree.

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

AI *profiles* are Go state machines (idle → aggro → chase → attack → leash → dead); AI *parameters* are content. A handful of profiles covers most mobs.

### Boss encounters

`profile = "boss"` replaces that state machine with the one in `boss.go`, and the encounter is written in content. A boss's mechanics *are* the encounter, so "which ability, at what health, after how long a wind-up" has to be authorable — but the machine that runs them stays Go, because expressing it as parameters means inventing a scripting language nobody asked for.

```toml
[mob.slime_king.ai]
profile      = "boss"
aggro_range  = 520
attack_range = 120

[[mob.slime_king.phases]]
name          = "The Throne Cracks"
at_hp_percent = 65          # entered at or below this, and never left
on_enter      = "king_quake"

[[mob.slime_king.phases.abilities]]
skill        = "king_crush"
cooldown_ms  = 13000
telegraph_ms = 1600         # wind-up before it lands
target       = "self"       # current | random | farthest | self
```

Three things make a boss a fight rather than a damage race:

**Phases**, entered as health falls and never returned to. A boss that went back on being healed would be a boss whose fight resets on a mistake. Phases must be listed in descending health order, and a hit that crosses two thresholds lands in the phase its health says rather than walking down one per tick.

**Telegraphs.** An ability with a `telegraph_ms` puts down a marker, roots the boss, and lands when the wind-up ends. The marker is the skill's own hitbox, anchored where the boss is standing and facing the direction it committed to — not an approximation drawn alongside it. Everything the marker promises follows from the boss not moving or turning until the attack resolves. An attack that is merely announced can be survived by moving; one that simply happens can only be survived by having enough health.

A boss only commits to an ability that would actually land, which is the same hitbox test again. Without it, a boss whose target stands on a ledge roots itself and telegraphs forever at a player in no danger at all — and a marker that appears when nothing is at stake teaches players to ignore markers.

**An enrage clock**, per phase rather than per fight, applying a buff when it runs out. A boss you can beat by refusing to engage with it is a boss with no mechanics.

### Asking for a party

`split_damage` is the vocabulary's answer to "this needs more than one person": one roll, divided evenly among everyone the cast lands on, then resolved through the ordinary damage path so each share is mitigated and rolled for a critical like any other hit. Set the whole number past what anyone at that level survives and a player who takes it alone dies while a party that takes it together does not. There is no gear answer and no skill answer — only six people in the same place at the same time.

Nothing else in the vocabulary can express that, because every other effect resolves against one target without knowing whether there are others. The division therefore happens at cast time, where the target list exists.

### Enhanced mobs

Enhanced mobs roll modifiers at spawn time from a weighted pool, PoE rare-mob style. Each modifier is stat mods plus optional effects plus a visual aura, so the combination is emergent rather than enumerated:

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

`cmd/treegen` generates this (`make tree`); hand-editing a several-hundred-node graph is not a plan, and a graphical editor is a project rather than a tool. The generator is the source, the JSON is the output, and the diff is reviewed — the same arrangement as the golden movement fixtures and the sprites.

The split inside the generator is the point: the **shape** (three clusters, branches radiating out, notables at intervals, keystones at the ends, bridges between neighbours) is mechanical and lives in code, while what the nodes **do** lives in a file somebody reads. A generator that also invented the modifiers would produce a tree that was large and said nothing.

The server validates on load that every edge names a real node, that every class start exists and is a start node, that no node costs a point and does nothing, that every stat named is real, and — the one that matters — that **every node is reachable from every class start**. A stranded node is one nobody can ever take, and it is silent at runtime.

Keystones use `more` multipliers with real drawbacks — that is what makes them build-defining choices rather than strict upgrades, and a test asserts every keystone gives something *and* costs something. A keystone everybody takes is a keystone that should have been a notable.

### Signed modifiers

A drawback is a negative number, and the conversion from authored decimals to parts-per-million has to keep the sign. There are two conversions for that reason: one clamped to [0, 1] for probabilities, and one signed for stat modifiers. Using the probability one for a modifier silently discards every drawback — which it did, until a test asked whether each keystone actually traded something.

## Dungeons

A dungeon is a private map with a shape: you go in as a party, you clear it in
order, and you come out. `content/dungeons/*.toml`:

```toml
[dungeon.slime_grove]
name = "Slime Grove"
map = "grove"          # must be placement = "private"
min_level = 10
lockout_ms = 1800000
exit_map = "forest"    # where a run ends, cleared or wiped
exit_spawn = "from-grove"

[[dungeon.slime_grove.stages]]
name = "The Guards"
spawns = ["guards"]    # spawn point names on the map

[[dungeon.slime_grove.stages]]
name = "The Slime King"
spawns = ["king"]
```

**Progression is spawning, not doors.** A stage's spawn points produce nothing
until the stage begins, and the stage is cleared when every one of them has
produced its whole population and none of it is left alive. So "kill the
guards, then the king" needs no keys and no geometry that changes underfoot —
and the client's collision is never told anything, so prediction cannot drift
over a wall that is solid on one side and not the other.

Inside a dungeon a spawn point produces its population **once**; `respawn_ms`
is ignored. A respawning stage is a stage that can never be cleared.

**A run ends two ways.** The last stage clears, or every connected player is
down at the same moment. Only a clear writes a lockout — being beaten by a
dungeon is punishment enough without also being barred from trying again.
Either way the party is sent to `exit_map` after a short pause, long enough to
loot what the boss dropped and to read what happened.

The loader checks each dungeon against its map: every spawn point on it must
belong to exactly one stage, and every privately placed map must have a
dungeon. Both mistakes load cleanly and then fail in play — a stage naming a
renamed spawn point leaves a party in an empty room with no way to progress.

Lockouts are per **character**, so a group carrying a friend through does not
spend the friend's, and leaving a party cannot launder one.

## Death

Four knobs in `balance.toml`, all under `[combat]`:

| Key | What it decides |
|---|---|
| `downed_ms` | How long a character lies at zero health before returning |
| `revive_grace_ms` | How long they cannot be harmed after coming back |
| `death_exp_penalty` | The share of progress toward the *current level* that is lost |

## Regeneration

Also `[combat]`:

| Key | What it decides |
|---|---|
| `mana_regen` | Fraction of maximum mana restored per second, out of combat |
| `mana_regen_in_combat` | The same, while fighting — must not exceed the rate above |
| `life_regen` | Fraction of maximum health per second, out of combat only |
| `combat_ms` | How long after *taking* damage a character counts as fighting |

Two mana rates rather than one. With none at all a caster who spends their mana
can never attack again; with a single generous rate there is never a reason to
stop casting. The lower in-combat rate makes a long fight something to pace,
and the higher out-of-combat one means the pacing is recovered by stepping away
rather than by logging out. Health returns out of combat only — regenerating
mid-fight would undo the fight.

Taking damage is what marks combat, not dealing it: a player hitting something
that cannot fight back is not in a fight, and making them wait as though they
were would punish clearing a zone.

The fractions are carried between seconds rather than rounded. A fifth of a
point per second either rounds to zero — and a level one caster with fifty mana
regenerates nothing, forever — or is rounded up to one, which makes the two
rates identical at that pool size and the distinction between them a lie.

The penalty is deliberately a fraction of progress within the level rather than
of total experience. A death therefore never costs a level, and never costs a
level 90 character forty times what it costs a level 10 one — which is what a
flat percentage of total experience does, and is how a death penalty stops
being a setback and becomes a reason to stop playing.

The grace ends the moment the character attacks. Without that it would not be a
chance to get clear, it would be a free opening: walk into a fight
untouchable, swing first, and only then become a target.

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

## Art

Sprites are **not content**, and they are not in `content/`. The server never reads them: it simulates positions and sizes, and what those look like is the client's business. Putting art in `content/` would fold it into the content hash, so a recoloured slime would kick every connected player out over a mismatch.

They live in `client/public/sprites`, generated by `cmd/spritegen` and committed:

```
make sprites      # regenerate, then look at the diff
```

The generator is the source and the PNGs are the output, the same arrangement as the golden movement fixtures. Art is drawn from a palette and a set of shapes rather than by hand because there is no artist; a jointed mannequin whose poses are six numbers means a walk cycle is a table, and changing the proportions changes every frame at once instead of eight.

Alongside the sheets it writes `sprites.json`, which says where every frame is. Nothing in the client hard-codes a frame position: two numbers in two places drift, and in a sprite sheet the drift shows up as a character sliced down the middle rather than as anything that fails.

**Sizes come from content.** A mob's sprite is the size its content entry says it is, asserted by a test, for two reasons: a sprite that does not match its hitbox makes every "that should have hit" argument unanswerable, and the client picks a mob's sprite *by* its dimensions — a snapshot carries a display name and a size, not a content id. A mismatch does not draw the wrong size, it draws nothing.

## Hot reload

In development, `SIGHUP` re-runs load → validate → link → verify and atomically swaps the frozen content pointer if it succeeds. On failure the old content stays live and the error is logged.

Live rooms keep the content generation they started with, so a reload cannot change a boss's stats mid-fight. New instances pick up the new generation.

Production reloads content only on restart. The complexity of safely migrating live rooms across content generations is not worth it while a rolling restart costs a few seconds.
