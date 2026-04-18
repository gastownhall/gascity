// Package bd embeds the bd (beads) provider pack for bundling into the gc binary.
package bd

import "embed"

// PackFS contains the bd pack files: pack.toml, doctor/, and template-fragments/.
//
//go:embed pack.toml doctor template-fragments
var PackFS embed.FS
