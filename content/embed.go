// Package content embeds the game's data files into the binary.
//
// Embedding keeps the server a single self-contained artefact, which is the
// point of the self-hosting goal: one binary plus Postgres and Redis, with no
// separate asset directory to deploy, version, or get out of step with the
// code that reads it.
//
// The parsing and validation live in internal/content, which takes an fs.FS so
// it neither knows nor cares whether the data was embedded or read from disk.
// That is what makes the --content-dir override for live editing possible.
package content

import "embed"

// FS holds every content file.
//
//go:embed all:affixes
//go:embed all:buffs
//go:embed balance.toml
//go:embed all:classes
//go:embed all:curves
//go:embed all:droptables
//go:embed all:dungeons
//go:embed all:elites
//go:embed all:events
//go:embed all:items
//go:embed all:maps
//go:embed all:mobs
//go:embed all:passives
//go:embed all:recipes
//go:embed all:resources
//go:embed all:stations
//go:embed all:secondary
//go:embed all:skills
//go:embed all:supports
var FS embed.FS
