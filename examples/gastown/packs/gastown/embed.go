// Package gastown embeds the gastown orchestration pack for bundling into the gc binary.
package gastown

import "embed"

// PackFS contains the gastown pack files.
//
//go:embed pack.toml commands doctor formulas orders all:agents assets template-fragments all:overlay
var PackFS embed.FS
