//go:build gascity_doltlite_lib

package beads

import _ "github.com/mattn/go-sqlite3"

// doltliteSQLDriverName names the C-backed sqlite driver used by libdoltlite builds.
const doltliteSQLDriverName = "sqlite3"
