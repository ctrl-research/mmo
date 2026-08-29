// Package contenttest provides a minimal valid content set for tests.
//
// Tests that need a world -- rooms, the gateway, integration tests -- need
// content that loads, and duplicating a dozen TOML files across test packages
// means they drift and a change to the loader has to be fixed in several
// places. This is the one definition, deliberately small: one mob, one skill,
// one drop table, one map with both an owner-layer and a shared-layer spawn.
package contenttest

import (
	"testing/fstest"

	"github.com/ctrl-research/mmo/internal/content"
)

// FS returns a filesystem holding a complete, valid content set.
//
// The returned map is fresh each call, so a test may mutate one file to
// exercise a failure path without affecting any other test.
func FS() fstest.MapFS {
	return fstest.MapFS{
		"balance.toml":         file(Balance),
		"curves/exp.toml":      file(Curves),
		"items/test.toml":      file(Items),
		"affixes/test.toml":    file(Affixes),
		"droptables/test.toml": file(Drops),
		"skills/test.toml":     file(Skills),
		"buffs/test.toml":      file(Buffs),
		"supports/test.toml":   file(Supports),
		"classes/test.toml":    file(Classes),
		"passives/tree.json":   file(Tree),
		"mobs/test.toml":       file(Mobs),
		"maps/test.tmj":        file(MapTMJ),
		"maps/annex.tmj":       file(AnnexTMJ),
	}
}

// Load builds the test content set, failing the test if it cannot.
func Load() (*content.Content, error) { return content.Load(FS()) }

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// Balance keeps combat numbers small and predictable so a test can reason
// about exact damage rather than approximate it.
const Balance = `
[combat]
crit_multiplier = 1.5
resistance_cap = 0.75
armour_divisor = 10
min_damage = 1
hit_flash_ms = 150
corpse_ms = 500
[drops]
ground_ms = 30000
pickup_range = 48.0
scatter_range = 0.0
[party]
exp_share_range = 600.0
max_size = 6
loot_lock_ms = 20000
[rooms]
spawn_activation_range = 800.0
idle_ms = 60000
[chat]
max_length = 300
[chat.per_minute]
local = 30
global = 6
whisper = 20
party = 30
guild = 30
`

// Buffs covers both halves of what a buff is: one that ticks an effect, and
// one made only of stat modifiers.
const Buffs = `
[buff.test_burn]
name = "Test Burn"
kind = "debuff"
duration_ms = 2000
tick_ms = 500
max_stacks = 3
refresh_on_apply = true
dispellable = true
[[buff.test_burn.effects]]
type = "damage"
element = "fire"
base = { min = 2, max = 4 }

[buff.test_might]
name = "Test Might"
kind = "buff"
duration_ms = 3000
max_stacks = 2
refresh_on_apply = true
[[buff.test_might.stat_mods]]
stat = "attack"
increased = 0.25

[buff.test_hardened]
name = "Test Hardened"
kind = "buff"
duration_ms = 60000
max_stacks = 1
refresh_on_apply = true
[[buff.test_hardened.stat_mods]]
stat = "armour"
increased = 0.50

[buff.test_enrage]
name = "Test Enrage"
kind = "buff"
duration_ms = 60000
max_stacks = 1
refresh_on_apply = true
[[buff.test_enrage.stat_mods]]
stat = "attack"
increased = 2.00
`

// Supports covers the two halves of what a support does: scale an effect, and
// repeat it. The tags are chosen so one attaches to the test skill and one
// does not, which is what the attachment rules need testing against.
const Supports = `
[support.test_heavy]
name = "Test Heavy"
tags = ["melee", "attack"]
mana_mult = 1.5
[[support.test_heavy.modify]]
kind = "damage"
more = 0.5
repeat = 2

[support.test_spellonly]
name = "Test Spell Only"
tags = ["spell"]
mana_mult = 1.2
[[support.test_spellonly.modify]]
kind = "damage"
more = 1.5
`

// Classes is one class whose starting skill is the test skill, so a joined
// character in a room test can actually cast something.
const Classes = `
[class.warrior]
name = "Test Warrior"
description = "A test class."
primary_stat = "strength"
starting_skills = ["slash"]
`

// Tree is a small passive tree: a start, a branch with a notable and a
// keystone, and a fork -- enough shape for allocation rules to be tested
// against, and small enough to read.
const Tree = `{
  "nodes": [
    {"id": 1, "kind": "start", "name": "Test Start", "pos": [0, 0]},
    {"id": 2, "kind": "small", "pos": [80, 0], "stats": [{"stat": "strength", "flat": 8}]},
    {"id": 3, "kind": "small", "pos": [160, 0], "stats": [{"stat": "max_life", "flat": 12}]},
    {"id": 4, "kind": "notable", "name": "Test Notable", "pos": [240, 0], "stats": [{"stat": "attack", "increased": 0.2}]},
    {"id": 5, "kind": "keystone", "name": "Test Keystone", "pos": [320, 0], "stats": [{"stat": "attack", "more": 0.5}, {"stat": "max_life", "more": -0.3}]},
    {"id": 6, "kind": "small", "pos": [160, 80], "stats": [{"stat": "armour", "increased": 0.1}]}
  ],
  "edges": [[1, 2], [2, 3], [3, 4], [4, 5], [3, 6]],
  "class_starts": {"warrior": 1}
}`

const Curves = `
[main]
max_level = 200
scale = 15.0
exponent = 2.4
growth = 0.045
[secondary]
max_level = 99
`

const Items = `
[item."test.gem"]
name = "Test Gem"
kind = "material"
stackable = true
max_stack = 100
level = 1

[item."test.sword"]
name = "Test Sword"
kind = "equipment"
slot = "weapon"
class = "sword"
level = 1

[[item."test.sword".implicit]]
stat = "attack"
kind = "flat"
min = 5
max = 5
`

// Affixes are deliberately few and with fixed ranges, so a test can assert an
// exact resulting stat rather than a plausible one.
const Affixes = `
[affix.test_flat_attack]
name = "Sharp"
type = "prefix"
classes = ["sword"]
stat = "attack"
kind = "flat"

[[affix.test_flat_attack.tiers]]
tier = 1
item_level = 1
min = 10
max = 10
weight = 100

[affix.test_increased_attack]
name = "Keen"
type = "prefix"
classes = ["sword"]
stat = "attack"
kind = "increased"

[[affix.test_increased_attack.tiers]]
tier = 1
item_level = 1
min = 0.50
max = 0.50
weight = 100

[affix.test_life]
name = "of Vigour"
type = "suffix"
stat = "max_life"
kind = "flat"

[[affix.test_life.tiers]]
tier = 1
item_level = 1
min = 20
max = 20
weight = 100
`

// Drops always produce gold and always produce the gem, so a test asserting
// that loot appeared is not flaky.
const Drops = `
[drop_table.test_table]
gold = { min = 5, max = 5, chance = 1.0 }
[[drop_table.test_table.entries]]
item = "test.gem"
chance = 1.0
qty = { min = 1, max = 1 }

[[drop_table.test_table.entries]]
item = "test.sword"
chance = 1.0
`

// Skills mirror the real starter skill's id, because the room grants exactly
// that skill until the passive tree arrives in M6.
const Skills = `
[skill.slash]
name = "Slash"
class = "warrior"
max_rank = 1
cost_mp = 0
cooldown_ms = 500
tags = ["melee", "attack"]
[skill.slash.targeting]
kind = "cone"
range = 72.0
half_height = 40.0
max_targets = 3
[[skill.slash.effects]]
type = "damage"
element = "physical"
base = { min = 1000, max = 1000 }
scaling = { attack = 0.0 }

[skill.mob_bite]
name = "Bite"
max_rank = 1
cooldown_ms = 1000
tags = ["melee", "attack"]
[skill.mob_bite.targeting]
kind = "cone"
range = 48.0
half_height = 32.0
max_targets = 1
[[skill.mob_bite.effects]]
type = "damage"
element = "physical"
base = { min = 2, max = 2 }
scaling = { attack = 0.0 }

[skill.boss_swing]
name = "Swing"
max_rank = 1
cooldown_ms = 500
tags = ["melee", "attack"]
[skill.boss_swing.targeting]
kind = "cone"
range = 120.0
half_height = 40.0
max_targets = 8
[[skill.boss_swing.effects]]
type = "damage"
element = "physical"
base = { min = 5, max = 5 }
scaling = { attack = 0.0 }

[skill.boss_share]
name = "Share"
max_rank = 1
cooldown_ms = 500
tags = ["attack"]
[skill.boss_share.targeting]
kind = "circle"
range = 200.0
max_targets = 8
[[skill.boss_share.effects]]
type = "split_damage"
element = "physical"
base = { min = 600, max = 600 }

[skill.boss_roar]
name = "Roar"
max_rank = 1
cooldown_ms = 500
tags = ["buff"]
[skill.boss_roar.targeting]
kind = "self"
[[skill.boss_roar.effects]]
type = "apply_buff"
buff = "test_hardened"
`

// The dummy has enough HP to survive a hit and little enough to die to two,
// and no armour, so damage arithmetic in tests is exact.
const Mobs = `
[mob.test_dummy]
name = "Test Dummy"
level = 1
hp = 1500
attack = 2
armour = 0
exp = 50
width = 32.0
height = 32.0
drop_table = "test_table"
[mob.test_dummy.ai]
profile = "aggressive_melee"
aggro_range = 200.0
leash_range = 500.0
attack_range = 40.0
move_speed = 40.0
idle_tick_interval = 2
[[mob.test_dummy.abilities]]
skill = "mob_bite"
weight = 1
cooldown_ms = 1000

# A three-phase encounter. The numbers are round so a test can assert on
# exactly which phase a given health value is in: 100%, 60%, and 25% of 1000.
[mob.test_boss]
name = "Test Boss"
level = 5
hp = 1000
attack = 0
armour = 0
exp = 100
width = 48.0
height = 48.0
[mob.test_boss.ai]
profile = "boss"
aggro_range = 600.0
leash_range = 2000.0
attack_range = 60.0
move_speed = 40.0
idle_tick_interval = 1
[[mob.test_boss.phases]]
name = "opening"
at_hp_percent = 100
[[mob.test_boss.phases.abilities]]
skill = "boss_swing"
cooldown_ms = 1000
telegraph_ms = 500
target = "current"
[[mob.test_boss.phases]]
name = "middle"
at_hp_percent = 60
on_enter = "boss_roar"
[[mob.test_boss.phases.abilities]]
skill = "boss_share"
cooldown_ms = 2000
telegraph_ms = 500
target = "self"
[[mob.test_boss.phases]]
name = "final"
at_hp_percent = 25
enrage_after_ms = 1000
enrage_buff = "test_enrage"
[[mob.test_boss.phases.abilities]]
skill = "boss_swing"
cooldown_ms = 500
telegraph_ms = 0
target = "farthest"

[mob.test_statue]
name = "Test Statue"
level = 1
hp = 100000
attack = 0
armour = 0
exp = 1
width = 32.0
height = 32.0
[mob.test_statue.ai]
profile = "passive"
aggro_range = 0.0
leash_range = 0.0
attack_range = 0.0
move_speed = 0.0
idle_tick_interval = 8
`

// The map is a flat floor with a player spawn, an owner-layer mob spawn a
// short walk away, and a shared-layer spawn -- so a test can exercise both
// layering paths without a second map.
const MapTMJ = `{
  "type": "map", "width": 40, "height": 10, "tilewidth": 32, "tileheight": 32,
  "properties": [
    {"name": "mapId", "type": "string", "value": "test"},
    {"name": "placement", "type": "string", "value": "shared"},
    {"name": "capacity", "type": "int", "value": 8}
  ],
  "layers": [{
    "name": "collision", "type": "objectgroup",
    "objects": [
      {"id": 1, "class": "solid", "x": 0, "y": 288, "width": 1280, "height": 32},
      {"id": 2, "class": "spawn_point", "name": "start", "x": 100, "y": 288,
       "properties": [{"name": "isDefault", "type": "bool", "value": true}]},
      {"id": 3, "class": "mob_spawn", "name": "dummies", "x": 400, "y": 288,
       "properties": [
         {"name": "mob_id", "type": "string", "value": "test_dummy"},
         {"name": "layer", "type": "string", "value": "owner"},
         {"name": "respawn_ms", "type": "int", "value": 1000},
         {"name": "max_alive", "type": "int", "value": 2},
         {"name": "radius", "type": "int", "value": 0}
       ]},
      {"id": 4, "class": "mob_spawn", "name": "statue", "x": 700, "y": 288,
       "properties": [
         {"name": "mob_id", "type": "string", "value": "test_statue"},
         {"name": "layer", "type": "string", "value": "shared"},
         {"name": "respawn_ms", "type": "int", "value": 60000},
         {"name": "max_alive", "type": "int", "value": 1}
       ]},
      {"id": 6, "class": "portal", "name": "to_annex", "x": 1216, "y": 256,
       "width": 64, "height": 64,
       "properties": [
         {"name": "target_map", "type": "string", "value": "annex"},
         {"name": "target_spawn", "type": "string", "value": "from_test"}
       ]},
      {"id": 7, "class": "waypoint", "name": "Test Grounds", "x": 200, "y": 288,
       "width": 48, "height": 48,
       "properties": [{"name": "waypoint_id", "type": "string", "value": "wp_test"}]},
      {"id": 5, "class": "mob_spawn", "name": "far", "x": 1250, "y": 288,
       "properties": [
         {"name": "mob_id", "type": "string", "value": "test_dummy"},
         {"name": "layer", "type": "string", "value": "owner"},
         {"name": "respawn_ms", "type": "int", "value": 1000},
         {"name": "max_alive", "type": "int", "value": 1},
         {"name": "radius", "type": "int", "value": 0}
       ]}
    ]
  }]
}`

// AnnexTMJ is a second map, so tests can exercise a portal between two of
// them.
//
// Kept deliberately bare -- ground, two spawn points, a portal back, and a
// waypoint -- because what it is for is being somewhere else, not being
// somewhere interesting.
const AnnexTMJ = `{
  "type": "map", "width": 20, "height": 10, "tilewidth": 32, "tileheight": 32,
  "properties": [
    {"name": "mapId", "type": "string", "value": "annex"},
    {"name": "displayName", "type": "string", "value": "The Annex"},
    {"name": "placement", "type": "string", "value": "shared"},
    {"name": "capacity", "type": "int", "value": 4},
    {"name": "minLevel", "type": "int", "value": 3},
    {"name": "maxLevel", "type": "int", "value": 9}
  ],
  "layers": [{
    "name": "collision", "type": "objectgroup",
    "objects": [
      {"id": 1, "class": "solid", "x": 0, "y": 288, "width": 640, "height": 32},
      {"id": 2, "class": "spawn_point", "name": "start", "x": 64, "y": 288,
       "properties": [{"name": "isDefault", "type": "bool", "value": true}]},
      {"id": 3, "class": "spawn_point", "name": "from_test", "x": 560, "y": 288},
      {"id": 4, "class": "portal", "name": "to_test", "x": 0, "y": 256,
       "width": 48, "height": 64,
       "properties": [
         {"name": "target_map", "type": "string", "value": "test"},
         {"name": "target_spawn", "type": "string", "value": "start"}
       ]},
      {"id": 5, "class": "waypoint", "name": "The Annex", "x": 544, "y": 256,
       "width": 48, "height": 64,
       "properties": [{"name": "waypoint_id", "type": "string", "value": "wp_annex"}]}
    ]
  }]
}`
