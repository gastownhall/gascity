// Package bd embeds the bd (beads) provider pack for bundling into the gc binary.
package bd

import "embed"

// PackFS contains the bd pack files, including assets/scripts/gc-beads-bd.sh
// and the nested dolt pack imported by pack.toml.
//
//go:embed pack.toml doctor template-fragments all:assets dolt/pack.toml dolt/doctor dolt/commands dolt/formulas dolt/orders dolt/agents all:dolt/assets
var PackFS embed.FS
