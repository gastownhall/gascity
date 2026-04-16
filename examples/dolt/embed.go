// Package dolt embeds the dolt database management pack for bundling into the gc binary.
package dolt

import "embed"

// PackFS contains the dolt pack files: pack.toml, doctor/, commands/, scripts/, formulas/, and orders/.
//
//go:embed pack.toml doctor commands scripts formulas orders
var PackFS embed.FS
