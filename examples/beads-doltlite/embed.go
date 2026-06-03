package beadsdoltlite

import "embed"

//go:embed pack.toml doctor commands formulas orders all:assets
var PackFS embed.FS
