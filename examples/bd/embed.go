// Package bd embeds the bd (beads) provider pack for bundling into the gc binary.
package bd

import "embed"

// PackFS contains the bd pack files, including assets/scripts/gc-beads-bd.sh.
//
//go:embed pack.toml doctor template-fragments all:assets
var PackFS embed.FS
