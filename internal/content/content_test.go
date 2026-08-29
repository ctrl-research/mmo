package content

import (
	"strings"
	"testing"
	"testing/fstest"

	gamedata "github.com/ctrl-research/mmo/content"
	"github.com/ctrl-research/mmo/internal/fixed"
)

// The shipped content must load. This is the test that catches a typo in a
// drop table before the server refuses to boot with it.
func TestShippedContentLoads(t *testing.T) {
	c, err := Load(gamedata.FS)
	if err != nil {
		t.Fatalf("shipped content failed to load: %v", err)
	}

	if c.Hash == "" {
		t.Error("content hash is empty; the handshake check would be inert")
	}
	for name, n := range map[string]int{
		"items": len(c.Items), "mobs": len(c.Mobs),
		"drops": len(c.Drops), "skills": len(c.Skills), "maps": len(c.Maps),
	} {
		if n == 0 {
			t.Errorf("no %s were loaded", name)
		}
	}
}

// The hash gates the handshake, so it must depend only on content -- never on
// filesystem iteration order, which would differ between machines and reject
// clients for no reason.
func TestContentHashIsStableAcrossLoads(t *testing.T) {
	first, err := Load(gamedata.FS)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Load(gamedata.FS)
		if err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		if again.Hash != first.Hash {
			t.Fatalf("hash changed between loads: %s then %s", first.Hash, again.Hash)
		}
	}
}

func TestContentHashChangesWithContent(t *testing.T) {
	base := minimalFS()
	first, err := Load(base)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	modified := minimalFS()
	modified["mobs/test.toml"] = &fstest.MapFile{
		Data: []byte(strings.Replace(mobsTOML, "hp = 60", "hp = 61", 1)),
	}
	second, err := Load(modified)
	if err != nil {
		t.Fatalf("load modified: %v", err)
	}

	if first.Hash == second.Hash {
		t.Error("changing a mob's HP did not change the content hash")
	}
}

// Cross-reference errors are the ones that actually happen: a renamed item, a
// deleted table, a mob that no longer exists. Each is silent at load and
// baffling at play, so each must fail the boot.
func TestBrokenReferencesAreRejected(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(fstest.MapFS)
		wantErr string
	}{
		{
			name: "mob references a missing drop table",
			mutate: func(f fstest.MapFS) {
				f["mobs/test.toml"] = file(strings.Replace(mobsTOML,
					`drop_table = "test_table"`, `drop_table = "no_such_table"`, 1))
			},
			wantErr: "unknown drop table",
		},
		{
			name: "drop table references a missing item",
			mutate: func(f fstest.MapFS) {
				f["droptables/test.toml"] = file(strings.Replace(dropsTOML,
					`item = "test.item"`, `item = "test.ghost"`, 1))
			},
			wantErr: "unknown item",
		},
		{
			name: "mob references a missing skill",
			mutate: func(f fstest.MapFS) {
				f["mobs/test.toml"] = file(strings.Replace(mobsTOML,
					`skill = "test_hit"`, `skill = "no_such_skill"`, 1))
			},
			wantErr: "unknown skill",
		},
		{
			name: "map spawns a missing mob",
			mutate: func(f fstest.MapFS) {
				f["maps/test.tmj"] = file(strings.Replace(mapTMJ,
					`"value": "test_mob"`, `"value": "no_such_mob"`, 1))
			},
			wantErr: "unknown mob",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := minimalFS()
			tt.mutate(f)
			_, err := Load(f)
			if err == nil {
				t.Fatal("expected a load error, got none")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(fstest.MapFS)
		wantErr string
	}{
		{
			name:    "mob with no HP",
			mutate:  func(f fstest.MapFS) { f["mobs/test.toml"] = file(strings.Replace(mobsTOML, "hp = 60", "hp = 0", 1)) },
			wantErr: "die on spawn",
		},
		{
			name: "mob with a zero-sized body",
			mutate: func(f fstest.MapFS) {
				f["mobs/test.toml"] = file(strings.Replace(mobsTOML, "width = 32.0", "width = 0", 1))
			},
			wantErr: "zero-sized body",
		},
		{
			name: "unknown AI profile",
			mutate: func(f fstest.MapFS) {
				f["mobs/test.toml"] = file(strings.Replace(mobsTOML, `profile = "aggressive_melee"`, `profile = "sneaky"`, 1))
			},
			wantErr: "unknown AI profile",
		},
		{
			// A mob that aggros then instantly leashes loops forever, and the
			// symptom is a mob that twitches rather than an obvious error.
			name: "leash range below aggro range",
			mutate: func(f fstest.MapFS) {
				f["mobs/test.toml"] = file(strings.Replace(mobsTOML, "leash_range = 400.0", "leash_range = 50.0", 1))
			},
			wantErr: "leash",
		},
		{
			name: "drop entry that can never drop",
			mutate: func(f fstest.MapFS) {
				f["droptables/test.toml"] = file(strings.Replace(dropsTOML, "chance = 0.5", "chance = 0.0", 1))
			},
			wantErr: "could never drop",
		},
		{
			name: "unknown item kind",
			mutate: func(f fstest.MapFS) {
				f["items/test.toml"] = file(strings.Replace(itemsTOML, `kind = "material"`, `kind = "widget"`, 1))
			},
			wantErr: "unknown kind",
		},
		{
			// An effect outside the vocabulary must fail the build rather than
			// silently do nothing: a skill that casts, plays its animation,
			// and produces no number is the hardest kind of content bug to
			// track down.
			name: "unknown effect type",
			mutate: func(f fstest.MapFS) {
				f["skills/test.toml"] = file(strings.Replace(skillsTOML, `type = "damage"`, `type = "levitate"`, 1))
			},
			wantErr: "unknown effect type",
		},
		{
			// In the vocabulary, but missing what it needs to do anything.
			// Same failure mode, same answer.
			name: "effect missing its target",
			mutate: func(f fstest.MapFS) {
				f["skills/test.toml"] = file(strings.Replace(skillsTOML, `type = "damage"`, `type = "apply_buff"`, 1))
			},
			wantErr: "names no buff",
		},
		{
			name: "skill with unknown targeting",
			mutate: func(f fstest.MapFS) {
				f["skills/test.toml"] = file(strings.Replace(skillsTOML, `kind = "cone"`, `kind = "spiral"`, 1))
			},
			wantErr: "unknown targeting kind",
		},
		{
			name: "unknown spawn layer",
			mutate: func(f fstest.MapFS) {
				f["maps/test.tmj"] = file(strings.Replace(mapTMJ, `"value": "owner"`, `"value": "everyone"`, 1))
			},
			wantErr: "unknown layer",
		},
		{
			name: "crit multiplier below 1",
			mutate: func(f fstest.MapFS) {
				f["balance.toml"] = file(strings.Replace(balanceTOML, "crit_multiplier = 1.5", "crit_multiplier = 0.8", 1))
			},
			wantErr: "crit_multiplier",
		},
		{
			name: "resistance cap at total immunity",
			mutate: func(f fstest.MapFS) {
				f["balance.toml"] = file(strings.Replace(balanceTOML, "resistance_cap = 0.75", "resistance_cap = 1.0", 1))
			},
			wantErr: "resistance_cap",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := minimalFS()
			tt.mutate(f)
			_, err := Load(f)
			if err == nil {
				t.Fatal("expected a load error, got none")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// Authoring is in decimals because that is readable; the simulation rolls in
// integers because a float comparison is one more place client and server
// could disagree.
func TestChancesConvertToPartsPerMillion(t *testing.T) {
	c := mustLoad(t)
	table := c.Drops["test_table"]

	if got := table.Entries[0].Chance; got != 500_000 {
		t.Errorf("chance 0.5 became %d ppm, want 500000", got)
	}
	if got := table.GoldChance; got != 800_000 {
		t.Errorf("gold chance 0.8 became %d ppm, want 800000", got)
	}
}

func TestRatioToPPMClampsAndRounds(t *testing.T) {
	tests := []struct {
		in   float64
		want int
	}{
		{0, 0}, {-1, 0}, {1, 1_000_000}, {2, 1_000_000},
		{0.5, 500_000}, {0.15, 150_000}, {0.0002, 200}, {0.000001, 1},
	}
	for _, tt := range tests {
		if got := ratioToPPM(tt.in); got != tt.want {
			t.Errorf("ratioToPPM(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// Durations are authored in milliseconds and rounded up, so a cooldown is
// never shorter than the designer asked for.
func TestMsToTicksRoundsUp(t *testing.T) {
	tests := []struct {
		ms   int
		want int
	}{
		{0, 0}, {1, 1}, {50, 1}, {51, 2}, {100, 2}, {1400, 28}, {5000, 100},
	}
	for _, tt := range tests {
		if got := msToTicks(tt.ms, TickRate); got != tt.want {
			t.Errorf("msToTicks(%d) = %d, want %d", tt.ms, got, tt.want)
		}
	}
}

// Speeds are authored per second because that is how a designer thinks; the
// simulation works per tick so it never divides at runtime.
func TestSpeedConvertsToPerTick(t *testing.T) {
	c := mustLoad(t)
	mob := c.Mobs["test_mob"]

	// 45 units/sec at 20 Hz is 2.25 units/tick.
	want := fixed.FromRatio(225, 100)
	if mob.AI.MoveSpeed != want {
		t.Errorf("move speed = %v per tick, want %v", mob.AI.MoveSpeed, want)
	}
}

// A later level costing less than an earlier one would let a player gain two
// levels from one kill and then lose one.
func TestExpCurveNeverDecreases(t *testing.T) {
	c := mustLoad(t)
	for level := 2; level < c.Curves.MaxLevel; level++ {
		prev, _ := c.Curves.ExpToNext(level - 1)
		cur, _ := c.Curves.ExpToNext(level)
		if cur < prev {
			t.Fatalf("level %d costs %d, less than level %d's %d", level, cur, level-1, prev)
		}
	}
}

func TestExpCurveBounds(t *testing.T) {
	c := mustLoad(t)

	if _, ok := c.Curves.ExpToNext(0); ok {
		t.Error("level 0 should not be a valid level")
	}
	if _, ok := c.Curves.ExpToNext(c.Curves.MaxLevel); ok {
		t.Error("the maximum level should have no next level")
	}
	if !c.Curves.IsMaxLevel(c.Curves.MaxLevel) {
		t.Error("IsMaxLevel should hold at the cap")
	}
	if c.Curves.IsMaxLevel(1) {
		t.Error("level 1 is not the cap")
	}
}

// Reproduced from Old School RuneScape, whose table is well known: level 10 is
// 1154 xp, level 50 is 101333, and level 99 is 13034431.
func TestSecondaryCurveMatchesOSRS(t *testing.T) {
	c := mustLoad(t)
	for _, tt := range []struct {
		level int
		want  int64
	}{{10, 1154}, {50, 101333}, {99, 13034431}} {
		if got := c.Curves.SecondaryExp[tt.level]; got != tt.want {
			t.Errorf("secondary level %d = %d xp, want %d", tt.level, got, tt.want)
		}
	}
}

func TestSpawnLayersParse(t *testing.T) {
	c := mustLoad(t)
	m := c.Maps["test"]

	if len(m.MobSpawns) != 2 {
		t.Fatalf("loaded %d mob spawns, want 2", len(m.MobSpawns))
	}

	byLayer := map[SpawnLayer]MobSpawn{}
	for _, s := range m.MobSpawns {
		byLayer[s.Layer] = s
	}
	if _, ok := byLayer[LayerOwner]; !ok {
		t.Error("no owner-layer spawn was loaded")
	}
	if _, ok := byLayer[LayerShared]; !ok {
		t.Error("no shared-layer spawn was loaded")
	}

	// The bug this catches: Tiled writes integers as JSON numbers, which
	// decode to float64, so an int64 assertion silently falls back to the
	// default and every spawn quietly shares one timer.
	owner := byLayer[LayerOwner]
	if owner.RespawnTicks != msToTicks(4000, TickRate) {
		t.Errorf("respawn = %d ticks, want %d; integer properties are not parsing",
			owner.RespawnTicks, msToTicks(4000, TickRate))
	}
	if owner.MaxAlive != 3 {
		t.Errorf("max_alive = %d, want 3", owner.MaxAlive)
	}
}

func TestSkillTags(t *testing.T) {
	c := mustLoad(t)
	s := c.Skills["test_hit"]

	if !s.HasTag("melee") {
		t.Error("expected the melee tag")
	}
	if s.HasTag("spell") {
		t.Error("did not expect the spell tag")
	}
}

func TestMissingContentDirectoryFails(t *testing.T) {
	f := minimalFS()
	delete(f, "mobs/test.toml")
	// The directory itself is now gone, which must be an error rather than an
	// empty mob set.
	if _, err := Load(f); err == nil {
		t.Error("expected an error when a content directory is missing")
	}
}

// --- fixtures ---------------------------------------------------------------

func mustLoad(t *testing.T) *Content {
	t.Helper()
	c, err := Load(minimalFS())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return c
}

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

const balanceTOML = `
[combat]
crit_multiplier = 1.5
resistance_cap = 0.75
armour_divisor = 10
min_damage = 1
hit_flash_ms = 150
corpse_ms = 600
[drops]
ground_ms = 120000
pickup_range = 48.0
scatter_range = 40.0
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

const buffsTOML = `
[buff.test_burn]
name = "Test Burn"
kind = "debuff"
duration_ms = 2000
tick_ms = 500
max_stacks = 3
refresh_on_apply = true
[[buff.test_burn.effects]]
type = "damage"
element = "fire"
base = { min = 2, max = 4 }
`

const supportsTOML = `
[support.test_heavy]
name = "Test Heavy"
tags = ["melee", "attack"]
mana_mult = 1.5
[[support.test_heavy.modify]]
kind = "damage"
more = 0.5
repeat = 2
`

const classesTOML = `
[class.warrior]
name = "Test Warrior"
primary_stat = "strength"
starting_skills = ["test_hit"]
`

const curvesTOML = `
[main]
max_level = 200
scale = 15.0
exponent = 2.4
growth = 0.045
[secondary]
max_level = 99
`

const itemsTOML = `
[item."test.item"]
name = "Test Item"
kind = "material"
stackable = true
max_stack = 100
level = 1

[item."test.blade"]
name = "Test Blade"
kind = "equipment"
slot = "weapon"
class = "sword"
level = 1

[[item."test.blade".implicit]]
stat = "attack"
kind = "flat"
min = 2
max = 4
`

const affixesTOML = `
[affix.test_attack]
name = "Sharp"
type = "prefix"
classes = ["sword"]
stat = "attack"
kind = "flat"

[[affix.test_attack.tiers]]
tier = 2
item_level = 1
min = 1
max = 3
weight = 100

[[affix.test_attack.tiers]]
tier = 1
item_level = 20
min = 4
max = 8
weight = 20

[affix.test_strength]
name = "of the Ox"
type = "suffix"
stat = "strength"
kind = "flat"

[[affix.test_strength.tiers]]
tier = 1
item_level = 1
min = 2
max = 5
weight = 100
`

const dropsTOML = `
[drop_table.test_table]
gold = { min = 3, max = 12, chance = 0.8 }
[[drop_table.test_table.entries]]
item = "test.item"
chance = 0.5
qty = { min = 1, max = 2 }
`

const skillsTOML = `
[skill.test_hit]
name = "Test Hit"
max_rank = 1
cooldown_ms = 1000
tags = ["melee", "attack"]
[skill.test_hit.targeting]
kind = "cone"
range = 50.0
half_height = 32.0
max_targets = 2
[[skill.test_hit.effects]]
type = "damage"
element = "physical"
base = { min = 5, max = 9 }
scaling = { attack = 1.0 }
`

const mobsTOML = `
[mob.test_mob]
name = "Test Mob"
level = 3
hp = 60
attack = 4
armour = 2
exp = 12
width = 32.0
height = 28.0
drop_table = "test_table"
[mob.test_mob.ai]
profile = "aggressive_melee"
aggro_range = 180.0
leash_range = 400.0
attack_range = 40.0
move_speed = 45.0
idle_tick_interval = 8
[[mob.test_mob.abilities]]
skill = "test_hit"
weight = 1
cooldown_ms = 1400
`

const mapTMJ = `{
  "type": "map", "width": 20, "height": 10, "tilewidth": 32, "tileheight": 32,
  "properties": [
    {"name": "mapId", "type": "string", "value": "test"},
    {"name": "placement", "type": "string", "value": "shared"},
    {"name": "capacity", "type": "int", "value": 12}
  ],
  "layers": [{
    "name": "collision", "type": "objectgroup",
    "objects": [
      {"id": 1, "class": "solid", "x": 0, "y": 288, "width": 640, "height": 32},
      {"id": 2, "class": "spawn_point", "name": "start", "x": 64, "y": 288,
       "properties": [{"name": "isDefault", "type": "bool", "value": true}]},
      {"id": 3, "class": "mob_spawn", "name": "mobs", "x": 200, "y": 288,
       "properties": [
         {"name": "mob_id", "type": "string", "value": "test_mob"},
         {"name": "layer", "type": "string", "value": "owner"},
         {"name": "respawn_ms", "type": "int", "value": 4000},
         {"name": "max_alive", "type": "int", "value": 3},
         {"name": "radius", "type": "int", "value": 60}
       ]},
      {"id": 4, "class": "mob_spawn", "name": "boss", "x": 400, "y": 288,
       "properties": [
         {"name": "mob_id", "type": "string", "value": "test_mob"},
         {"name": "layer", "type": "string", "value": "shared"},
         {"name": "respawn_ms", "type": "int", "value": 60000},
         {"name": "max_alive", "type": "int", "value": 1}
       ]}
    ]
  }]
}`

func minimalFS() fstest.MapFS {
	return fstest.MapFS{
		"balance.toml":         file(balanceTOML),
		"curves/exp.toml":      file(curvesTOML),
		"items/test.toml":      file(itemsTOML),
		"affixes/test.toml":    file(affixesTOML),
		"droptables/test.toml": file(dropsTOML),
		"skills/test.toml":     file(skillsTOML),
		"buffs/test.toml":      file(buffsTOML),
		"supports/test.toml":   file(supportsTOML),
		"classes/test.toml":    file(classesTOML),
		"mobs/test.toml":       file(mobsTOML),
		"maps/test.tmj":        file(mapTMJ),
	}
}
