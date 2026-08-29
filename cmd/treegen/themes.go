package main

// What each part of the tree is about.
//
// The tree's shape is generated, but what the nodes *do* is written here,
// because that is the design work. A generator that also invented the
// modifiers would produce a tree that was large and said nothing.

// mod is one modifier on a node, in the shape the tree file wants.
type mod struct {
	Stat      string  `json:"stat"`
	Flat      int     `json:"flat,omitempty"`
	Increased float64 `json:"increased,omitempty"`
	More      float64 `json:"more,omitempty"`
}

// theme is one cluster's identity: what its small nodes give, and the notables
// and keystones worth travelling to.
type theme struct {
	class string
	name  string

	// smalls are drawn from in order along a branch, repeating. Keeping them
	// few and repeating is deliberate: a branch should read as "this way is
	// strength", not as a list of unrelated bonuses.
	smalls []mod

	notables  []named
	keystones []named
}

type named struct {
	name string
	mods []mod
}

// The three clusters. Each is recognisably about one thing, and each has one
// route that leans toward a neighbour, which is what makes a hybrid build
// possible without a special case for it.
var themes = []theme{
	{
		class: "warrior",
		name:  "Iron",
		smalls: []mod{
			{Stat: "strength", Flat: 8},
			{Stat: "max_life", Flat: 12},
			{Stat: "armour", Increased: 0.06},
			{Stat: "attack", Increased: 0.05},
		},
		notables: []named{
			{"Bulwark", []mod{
				{Stat: "armour", Increased: 0.24},
				{Stat: "max_life", Flat: 30},
			}},
			{"Heavy Hand", []mod{
				{Stat: "attack", Increased: 0.18},
				{Stat: "strength", Flat: 15},
			}},
			{"Second Wind", []mod{
				{Stat: "max_life", Increased: 0.12},
				{Stat: "armour", Increased: 0.10},
			}},
			{"Executioner", []mod{
				{Stat: "crit_multiplier", Increased: 0.25},
				{Stat: "attack", Increased: 0.10},
			}},
			{"Thick Hide", []mod{
				{Stat: "armour", Increased: 0.18},
				{Stat: "fire_resistance", Flat: 10},
			}},
			{"Momentum", []mod{
				{Stat: "attack_speed", Increased: 0.14},
				{Stat: "movement_speed", Increased: 0.08},
			}},
		},
		keystones: []named{
			// Every keystone is a trade. One that is strictly better is one
			// that should have been a notable.
			{"Unbreakable", []mod{
				{Stat: "armour", More: 0.60},
				{Stat: "attack_speed", More: -0.25},
			}},
			{"Reckless", []mod{
				{Stat: "attack", More: 0.45},
				{Stat: "max_life", More: -0.30},
			}},
		},
	},
	{
		class: "mage",
		name:  "Ember",
		smalls: []mod{
			{Stat: "intelligence", Flat: 8},
			{Stat: "max_mana", Flat: 15},
			{Stat: "fire_resistance", Flat: 6},
			{Stat: "attack", Increased: 0.05},
		},
		notables: []named{
			{"Kindling", []mod{
				{Stat: "attack", Increased: 0.20},
				{Stat: "intelligence", Flat: 12},
			}},
			{"Deep Well", []mod{
				{Stat: "max_mana", Increased: 0.25},
				{Stat: "intelligence", Flat: 10},
			}},
			{"Frostbite", []mod{
				{Stat: "cold_resistance", Flat: 15},
				{Stat: "crit_chance", Increased: 0.20},
			}},
			{"Conduit", []mod{
				{Stat: "lightning_resistance", Flat: 15},
				{Stat: "attack_speed", Increased: 0.12},
			}},
			{"Runic Skin", []mod{
				{Stat: "max_mana", Flat: 40},
				{Stat: "armour", Increased: 0.12},
			}},
			{"Overload", []mod{
				{Stat: "crit_multiplier", Increased: 0.20},
				{Stat: "intelligence", Flat: 10},
			}},
		},
		keystones: []named{
			{"Glass Cannon", []mod{
				{Stat: "attack", More: 0.50},
				{Stat: "max_life", More: -0.35},
			}},
			{"Mind Over Matter", []mod{
				{Stat: "max_mana", More: 0.50},
				{Stat: "armour", More: -0.40},
			}},
		},
	},
	{
		class: "ranger",
		name:  "Swift",
		smalls: []mod{
			{Stat: "dexterity", Flat: 8},
			{Stat: "crit_chance", Increased: 0.06},
			{Stat: "attack_speed", Increased: 0.04},
			{Stat: "movement_speed", Increased: 0.03},
		},
		notables: []named{
			{"Keen Eye", []mod{
				{Stat: "crit_chance", Increased: 0.30},
				{Stat: "dexterity", Flat: 12},
			}},
			{"Fleet", []mod{
				{Stat: "movement_speed", Increased: 0.14},
				{Stat: "attack_speed", Increased: 0.10},
			}},
			{"Deadly Aim", []mod{
				{Stat: "crit_multiplier", Increased: 0.30},
			}},
			{"Sure Footing", []mod{
				{Stat: "dexterity", Flat: 18},
				{Stat: "armour", Increased: 0.10},
			}},
			{"Quickened", []mod{
				{Stat: "attack_speed", Increased: 0.16},
			}},
			{"Evasive", []mod{
				{Stat: "movement_speed", Increased: 0.10},
				{Stat: "max_life", Flat: 25},
			}},
		},
		keystones: []named{
			{"Precision", []mod{
				{Stat: "crit_chance", More: 0.75},
				{Stat: "attack", More: -0.20},
			}},
			{"Hit and Run", []mod{
				{Stat: "movement_speed", More: 0.40},
				{Stat: "armour", More: -0.50},
			}},
		},
	},
}

// bridges are the modifiers on the nodes joining one cluster to the next.
//
// Deliberately plain: the route between clusters should cost points and give
// little, so travelling to another class's keystone is a real commitment
// rather than something you do on the way past.
var bridge = []mod{
	{Stat: "strength", Flat: 4},
	{Stat: "dexterity", Flat: 4},
	{Stat: "intelligence", Flat: 4},
}
