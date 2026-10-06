package beads

import (
	"os"
	"path/filepath"
	"strings"
)

// Which database does a scope's storage mode select?
//
// A bd scope's .beads/metadata.json names WHICH local bead database bd opens:
// dolt_mode=server sends every read to the managed Dolt server, which serves
// the city's .beads/dolt data directory, and dolt_mode=embedded sends it to the
// scope's own .beads/embeddeddolt/<dolt_database> repository. Those are two
// different databases in two different directories, and nothing copies rows
// between them.
//
// gc canonicalizes a managed scope's metadata to server mode on several
// lifecycle paths (`gc rig add`, `gc start`, `gc supervisor run` — see
// cmd/gc/beads_provider_lifecycle.go ensureCanonicalScopeMetadata). On a
// workspace an operator had initialized in embedded mode, that flip re-points
// the ledger at a server database that has never held any of their beads and
// leaves the previous one on disk, unread. bd does not fail: it connects, runs
// the query against the database it was told to use, matches nothing, and
// returns an empty result with exit 0.
//
// The mapping below is what a caller needs BEFORE changing a scope's mode —
// "what am I about to stop reading?" — so the change can be announced with the
// path attached, rather than discovered later by diffing metadata.json.

// beadsSubdirForDoltMode maps a metadata dolt_mode onto the .beads/
// subdirectory that mode's databases live in.
//
// It mirrors the mapping gc doctor's activeBDStoreFromMetadata applies to
// decide which of the two directories is the ACTIVE store; doctor keeps its own
// copy because it also classifies non-Dolt backends, and
// TestBDSplitStoreCheck_WarnsWhenOnlyTheUnreadStoreExists is where the two are
// exercised against the same on-disk shape. An unrecognized or absent mode maps
// to nothing: a scope whose mode gc cannot classify is one this says nothing
// about.
func beadsSubdirForDoltMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "server":
		return "dolt"
	case "embedded", "local":
		return "embeddeddolt"
	default:
		return ""
	}
}

// BeadDatabaseDirForDoltMode returns the directory a scope's bead database
// lives in under the given dolt_mode, and whether a Dolt repository is actually
// there.
//
// The answer is a fact about two path components and one directory entry, so it
// costs one stat and needs no store open, no server and no network.
//
// Presence is a `.dolt` subdirectory, the same test gc doctor's doltReposUnder
// applies. A mode outside {server, embedded, local}, an empty scope root, an
// empty database name, or a name that is a path rather than a bare identifier
// all report absent.
//
// Presence is NOT population, and no caller may read it as such: a Dolt
// repository `bd init` created a second ago and one an operator has been filing
// beads into for months are the same directory shape, and telling them apart
// requires opening the database. What this backs is an announcement that names
// which directory is being left behind — not a claim about what is in it.
func BeadDatabaseDirForDoltMode(scopeRoot, mode, database string) (string, bool) {
	sub := beadsSubdirForDoltMode(mode)
	database = strings.TrimSpace(database)
	if strings.TrimSpace(scopeRoot) == "" || sub == "" || database == "" || database != filepath.Base(database) {
		return "", false
	}
	path := filepath.Join(scopeRoot, ".beads", sub, database)
	if info, err := os.Stat(filepath.Join(path, ".dolt")); err != nil || !info.IsDir() {
		return "", false
	}
	return path, true
}
