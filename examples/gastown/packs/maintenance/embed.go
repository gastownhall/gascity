// Package maintenance embeds the maintenance infrastructure pack for bundling into the gc binary.
package maintenance

import "embed"

// PackFS contains the maintenance pack files.
//
//go:embed pack.toml doctor formulas orders all:agents template-fragments all:assets
var PackFS embed.FS
