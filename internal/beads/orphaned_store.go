package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Why this guard exists.
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
// leaves the populated one on disk, unread. bd does not fail: it connects,
// runs the query against the database it was told to use, matches nothing, and
// returns an empty result with exit 0.
//
// That is a projection that cannot see its OWN ledger, reporting the empty set.
// It is the same silent-empty shape as a blind read of a relocated coordination
// class (bdsql_relocation.go), and it defeats the loud-leg guarantee of a
// federated reader from the inside: `gc ready` can fail loudly on a dead rig or
// an unreadable binding, but if the CITY WORK leg answers empty because its
// metadata was rewritten underneath it, the federation reports a confident
// short answer. It bites a single-store city exactly as hard as a split one —
// there is no coordination class involved, only a workspace pointed at the
// wrong database.
//
// The fact is decidable from disk alone, and `gc doctor`'s bd-split-store check
// already names it (internal/doctor/checks.go: "legacy split store detected:
// active .beads/<x>, inactive .beads/<y> contains N Dolt repo(s)"). What was
// missing is enforcement at read time — a check an operator has to remember to
// run is not a guard against an answer they already believe.

// ErrOrphanedBeadStore is returned instead of an empty result when a scope's
// .beads/ holds a bead database its metadata no longer points at. Callers that
// need to distinguish this from a genuine empty result match it with errors.Is.
var ErrOrphanedBeadStore = errors.New("bead store read returned empty while an unread bead database sits beside it")

// beadsSubdirForDoltMode maps a metadata dolt_mode onto the .beads/
// subdirectory that mode's databases live in.
//
// It is the same mapping gc doctor's activeBDStoreFromMetadata uses to decide
// which of the two directories is the ACTIVE store, kept as one function so the
// diagnostic and the runtime guard cannot disagree about which database a mode
// selects. An unrecognized or absent mode maps to nothing: a scope whose mode gc
// cannot classify is one this guard says nothing about.
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

// OrphanedBeadDatabase reports a bead database left behind in scopeRoot/.beads/
// that the scope's metadata.json does not point at. It returns the path to that
// database and the name of the .beads/ subdirectory the metadata DOES point at,
// so a caller can name both sides of the disagreement.
//
// The answer is a fact about two directories and one JSON file, so it costs two
// stats and a small read and needs no store open, no server and no network.
//
// It reports nothing — the safe direction — for every shape it cannot decide:
// no metadata file, malformed metadata, a non-Dolt backend, a mode outside
// {server, embedded, local}, a missing dolt_database, and (deliberately) a
// dolt_database that is a path rather than a bare name, which is not a shape
// gc writes. A city that has never had an embedded workspace has no
// .beads/embeddeddolt at all and can never match.
//
// "A database" means a directory holding a Dolt repository, which is the same
// test gc doctor's doltReposUnder applies: a `.dolt` subdirectory. An empty
// parent directory left by a previous tool is not a ledger and does not match.
func OrphanedBeadDatabase(scopeRoot string) (orphan, activeStore string, ok bool) {
	// An unset scope root would resolve ".beads" against the process working
	// directory and answer about whatever city that happens to be — a store with
	// no directory has no ledger to be orphaned from.
	if strings.TrimSpace(scopeRoot) == "" {
		return "", "", false
	}
	beadsDir := filepath.Join(scopeRoot, ".beads")
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		return "", "", false
	}
	var meta struct {
		Backend      string `json:"backend"`
		Database     string `json:"database"`
		DoltMode     string `json:"dolt_mode"`
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", "", false
	}
	if !strings.EqualFold(strings.TrimSpace(meta.Backend), "dolt") &&
		!strings.EqualFold(strings.TrimSpace(meta.Database), "dolt") {
		return "", "", false
	}
	database := strings.TrimSpace(meta.DoltDatabase)
	if database == "" || database != filepath.Base(database) {
		return "", "", false
	}
	active := beadsSubdirForDoltMode(meta.DoltMode)
	if active == "" {
		return "", "", false
	}
	for _, mode := range []string{"server", "embedded"} {
		if beadsSubdirForDoltMode(mode) == active {
			continue
		}
		if path, present := BeadDatabaseDirForDoltMode(scopeRoot, mode, database); present {
			return path, active, true
		}
	}
	return "", "", false
}

// BeadDatabaseDirForDoltMode returns the directory a scope's bead database
// lives in under the given dolt_mode, and whether a Dolt repository is actually
// there.
//
// It is the fact a caller needs BEFORE changing a scope's mode — "what am I
// about to stop reading?" — and the same fact OrphanedBeadDatabase needs after
// one has already changed. Sharing it keeps the warning gc prints when it
// re-points a workspace and the refusal a later read returns talking about the
// same directory.
//
// Presence is a `.dolt` subdirectory, the same test gc doctor's doltReposUnder
// applies. A mode outside {server, embedded, local}, an empty database name, or
// a name that is a path rather than a bare identifier all report absent.
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

// OrphanedBeadStoreRefusal builds the error an empty read returns when the
// scope holds an unread bead database. op names the read that stopped, and
// active is the store the read was answered from.
//
// The message is written for whoever hits this without the source in front of
// them: it says which read came back empty, which database answered it, which
// one was not consulted, why nothing errored, and the two on-disk resolutions —
// point the metadata back, or retire the stale database — plus the diagnostic
// that enumerates both.
//
// It deliberately does NOT claim rows were lost. It claims only that the empty
// answer cannot be distinguished from a real one while a second ledger sits
// unread, which is the whole of what is knowable from disk.
func OrphanedBeadStoreRefusal(op, scopeRoot, orphan, active string) error {
	return fmt.Errorf("%w: %s returned no rows for %s from the %q store, but %s is a bead database this scope's "+
		".beads/metadata.json does not point at. Nothing failed — bd opened the store its metadata names, ran the read "+
		"successfully and matched nothing — so the empty result is indistinguishable from a real one while a second "+
		"ledger sits unread beside it. gc canonicalizes a managed scope's metadata to dolt_mode=server on `gc rig add`, "+
		"`gc start` and `gc supervisor run`, which re-points a workspace that was initialized in embedded mode without "+
		"moving its rows. Run `gc doctor` (check bd-split-store) to see both databases, then either point "+
		".beads/metadata.json back at the one that holds the data, or export from a copy of the unread one, review with "+
		"`bd import --dry-run`, and import into the one being read — after which removing it clears this refusal",
		ErrOrphanedBeadStore, op, scopeRoot, active, orphan)
}

// emptyReadIsUntrustworthy returns the refusal for a read of this scope that
// came back empty while an unread bead database sits in the same .beads/.
//
// It is called only on the empty path, so a scope that answers with rows pays
// nothing: the store bd opened is demonstrably the populated one and the second
// directory is inert history. That is also why this is not a store-open guard —
// refusing to open would brick every already-migrated scope that still carries
// a stale directory, and those scopes answer correctly.
func (s *BdStore) emptyReadIsUntrustworthy(op string) error {
	orphan, active, ok := OrphanedBeadDatabase(s.dir)
	if !ok {
		return nil
	}
	return OrphanedBeadStoreRefusal(op, s.dir, orphan, active)
}
