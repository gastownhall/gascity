// Package dolt embeds the dolt database management pack for bundling into the gc binary.
package dolt

import "embed"

// PackFS contains the dolt pack files: pack.toml, doctor/, commands/, formulas/, orders/, and assets/.
//
//go:embed pack.toml doctor commands formulas orders all:assets
var PackFS embed.FS
