// Package core embeds the core bootstrap pack for bundling into the gc
// binary. The same content is also reachable through the bootstrap's global
// packs/** embed, but exposing a dedicated PackFS lets cmd/gc's per-city
// MaterializeBuiltinPacks pipeline handle core uniformly with bd, dolt,
// maintenance, and gastown.
package core

import "embed"

// PackFS contains the core pack files.
//
//go:embed pack.toml all:assets formulas orders all:overlay skills
var PackFS embed.FS
