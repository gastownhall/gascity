package beads

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Why an empty read from a re-pointed workspace is worth saying something about.
//
// A bd scope's .beads/metadata.json names WHICH local bead database bd opens:
// dolt_mode=server sends every read to the managed Dolt server, which serves
// the city's .beads/dolt data directory, and dolt_mode=embedded sends it to the
// scope's own .beads/embeddeddolt/<dolt_database> repository. Those are two
// different databases in two different directories, and nothing copies rows
// between them.
//
// gc canonicalizes a managed scope's metadata to server mode on `gc rig add`,
// `gc start`, `gc supervisor run`, `gc rig set-endpoint` and `gc beads city
// use-managed|use-external` (cmd/gc/beads_provider_lifecycle.go
// ensureCanonicalScopeMetadata). On a workspace an operator initialized in
// embedded mode, that flip re-points the ledger at a server database which has
// never held one of their beads and leaves the populated one on disk, unread.
// bd does not fail: it connects, runs the query against the database it was
// told to use, matches nothing, and returns an empty result with exit 0.
//
// That is a projection which cannot see its OWN ledger reporting the empty set,
// and it defeats a federated reader from the inside: `gc ready` fails loudly on
// a dead rig or an unreadable binding, but when the CITY WORK leg answers empty
// because its metadata was rewritten underneath it, the federation reports a
// confident short answer. It bites a SINGLE-STORE city exactly as hard as a
// split one — no coordination class is involved, only a workspace pointed at
// the wrong database — so nothing here consults [storage.classes].
//
// # Why this is a notice and not a refusal
//
// The flip announces itself at the moment it happens
// (announceStorageModeChange), but the read that pays for it happens later, in
// another process, and gets no signal at all. Closing that gap needs evidence,
// and the evidence available at read time does not reach a refusal:
//
//   - "Empty active store beside a second Dolt repository" is the SAME on-disk
//     shape as a plain `bd init -p X` workspace adopted by `gc rig add`. bd's
//     default mode is embedded, so `bd init` creates an EMPTY
//     .beads/embeddeddolt/X/.dolt, and gc's canonicalization then leaves this
//     condition true forever on a city that was never broken.
//   - Separating the two requires OPENING the unread database — a store open, a
//     Dolt file lock and a possible schema migration on the very directory `gc
//     doctor`'s splitStoreFixHint tells the operator to preserve ("keep both
//     directories until reconciled"). A guard is not allowed to mutate the
//     backup an operator was told to keep, and a read path is not allowed to
//     block on a second database open.
//   - Refusing anyway is a fleet outage, not a warning: federateBeadLegs aborts
//     the whole federation on any leg error, the generated work query appends
//     `|| exit $?`, and the API ready arm hits totalOutage(). A city that is
//     merely IDLE would fail closed.
//
// So this states what is knowable — this store has never answered with a row,
// an unfiltered probe of it finds no row, and a second bead database sits
// unread beside it — on stderr, once per store, and lets the read succeed. It
// carries `gc doctor`'s remediation word for word so the two never send an
// operator in different directions, and names an override because a store-layer
// guard with no escape is the difference between a warning and an outage.

// AllowUnreadStoreReadEnvVar silences the unread-store notice for a process
// that already knows about the second database.
//
// It is the symmetrical escape to GC_BD_ALLOW_RELOCATED_CLASS_READ, and it
// lives at the STORE layer rather than at the `gc bd` CLI seam because this
// guard fires from BdStore.List and BdStore.Ready on every path that reaches
// them — a knob honored on only some of them would advertise an escape that
// does not work, which is the same class of false statement the guard exists to
// remove.
//
// The state it exists for is the one `gc doctor` prescribes: reconciliation
// says "keep both directories until reconciled", which means an operator is
// deliberately parked in the shape this notice describes, possibly for days.
const AllowUnreadStoreReadEnvVar = "GC_BD_ALLOW_UNREAD_STORE_READ"

// UnreadBeadDatabase reports a bead database sitting in scopeRoot/.beads/ that
// the scope's metadata.json does not point at. It returns the path to that
// database and the name of the .beads/ subdirectory the metadata DOES point at,
// so a caller can name both sides of the disagreement.
//
// The answer is a fact about two directories and one small JSON file, so it
// costs a read and two stats and needs no store open, no server and no network.
//
// It reports nothing — the safe direction — for every shape it cannot decide:
// no metadata file, malformed metadata, a non-Dolt backend, a mode outside
// {server, embedded, local}, a missing dolt_database, and (deliberately) a
// dolt_database that is a path rather than a bare name, which is not a shape gc
// writes. A city that has never had an embedded workspace has no
// .beads/embeddeddolt at all and can never match.
//
// "A database" means a directory holding a Dolt repository, the same test `gc
// doctor`'s doltReposUnder applies: a `.dolt` subdirectory. An empty parent
// directory left by a previous tool is not a ledger and does not match.
//
// Presence is NOT population. What this fact supports is a notice naming a
// directory, never a claim about what is in it.
func UnreadBeadDatabase(scopeRoot string) (unread, activeStore string, ok bool) {
	// An unset scope root would resolve ".beads" against the process working
	// directory and answer about whatever city that happens to be; a store with
	// no directory has no ledger to be read past.
	if strings.TrimSpace(scopeRoot) == "" {
		return "", "", false
	}
	data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "metadata.json"))
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

// UnreadStoreNotice builds the line an empty whole-ledger read prints when the
// store that answered it has never produced a row and a second bead database
// sits unread in the same .beads/. op names the read, and activeStore is the
// .beads/ subdirectory the read was answered from.
//
// It is exported so the three places that talk about this one situation — the
// announcement at flip time (cmd/gc), `gc doctor`'s bd-split-store check, and
// this read-time notice — can be pinned against each other rather than drifting
// into three different stories.
//
// The message is written for whoever hits it without the source in front of
// them: which read came back empty, which database answered it, which one was
// not consulted, why nothing errored, why this is not being treated as proof of
// a fault, the remediation, and the way to silence it.
//
// It deliberately does NOT claim rows were lost, nor that this scope is broken.
// A workspace `bd init` created and never filed a bead into produces this exact
// shape, and a message that overstates gets ignored the next time it is right.
func UnreadStoreNotice(op, scopeRoot, unread, activeStore string) string {
	return fmt.Sprintf("gc: %s returned no rows for %s from the %q bead store, and an unfiltered probe of that store "+
		"found no row at all, while %s is a Dolt bead database this scope's .beads/metadata.json does not point at. "+
		"Nothing failed — bd opened the store its metadata names, ran the read successfully and matched nothing — so this "+
		"empty answer is indistinguishable from a real one while a second ledger sits unread beside it. gc canonicalizes "+
		"a managed scope's metadata to dolt_mode=server on `gc rig add`, `gc start`, `gc supervisor run`, `gc rig "+
		"set-endpoint` and `gc beads city use-managed|use-external`, which re-points a workspace initialized in embedded "+
		"mode without moving its rows. This is a notice and not a refusal because a workspace `bd init` created and never "+
		"filed a bead into looks the same on disk, and telling the two apart means opening the unread database. Run `gc "+
		"doctor` (check bd-split-store) to see what each store holds, then export from a copy of the unread one, review "+
		"with `bd import --dry-run`, and import into the active one; keep both directories until reconciled. Set %s=1 to "+
		"silence this while you reconcile.\n",
		op, scopeRoot, activeStore, unread, AllowUnreadStoreReadEnvVar)
}

// unreadStoreGuard is the per-STORE verdict behind the notice.
//
// Per-store is the whole correction. The first version of this guard judged
// per-CALL and refused an empty Ready() on a demonstrably populated store,
// because "this call returned nothing" and "this store holds nothing" are
// different claims and only the second one is evidence. sawRows latches the
// first, cheapest possible disproof — the store answered a read with a row —
// and once latched the guard is inert for the life of the store.
//
// verdictClaimed carries the expensive half: the on-disk shape check and, only
// if that matches, ONE unfiltered `bd list --limit 1` against the active store.
// The hot path never pays for it twice, and a healthy city never pays for it at
// all — it stops at an atomic load, or at the metadata read when a read
// genuinely comes back empty.
//
// It is a compare-and-swap latch rather than a sync.Once on purpose. once.Do
// makes every concurrent caller WAIT for the winner, and the winner here runs a
// bd subprocess; on a supervisor with a goroutine per rig that turns one slow
// probe into a stall across all of them. Losing the race means the verdict is
// already being reached by someone else, and the right thing for a diagnostic
// to do about that is nothing.
//
// It is claimed BEFORE the evidence is gathered, so the cheap negative — a
// scope with one ledger, which is every healthy city — costs one metadata read
// for the life of the store and two atomic loads on every read after it. The
// cost of that ordering is that a store held across a mode flip performed by
// ANOTHER process keeps its verdict; the process doing the flip announces it
// and builds its own stores afterwards, and "memoized per BdStore" is what
// makes this affordable on List and Ready at all.
type unreadStoreGuard struct {
	// sawRows latches when this store's bd answered any read with at least one
	// row, BEFORE client-side filtering. It is the disproof, so it is checked
	// first and is never unlatched: a store that once held rows is not a
	// workspace pointed at the wrong database.
	sawRows atomic.Bool
	// verdictClaimed bounds the probe and the notice to one per store, without
	// making any other read wait for them.
	verdictClaimed atomic.Bool
}

// noteServerRows records that bd handed this store rows for some read.
//
// It is called with the count of rows bd RETURNED, not the count the caller
// received, because client-side filtering (assignee, tier, limit, parent) can
// reduce a real answer to nothing and that reduction says nothing about the
// store. Two atomics in the common case, one in the steady state.
func (s *BdStore) noteServerRows(rows int) {
	if s == nil || rows <= 0 || s.unreadStore.sawRows.Load() {
		return
	}
	s.unreadStore.sawRows.Store(true)
}

// noticeIfStoreCannotSeeItsLedger prints the unread-store notice at most once
// per store, when an UNFILTERED whole-ledger read came back empty and the
// evidence supports it.
//
// Callers must only reach this from a read with no selector: a filtered empty
// result is evidence of nothing, which is the second correction this guard
// carries. The cost ladder is deliberate, in increasing order of expense:
//
//  1. sawRows and verdictClaimed — two atomic loads. The first is true for
//     every store that has ever answered with a row, which is every store on a
//     working city that holds work; the second is true for every store whose
//     one-shot verdict has already been reached.
//  2. the override env var and the on-disk shape — a getenv, one file read and
//     up to two stats, paid once per store.
//  3. the probe — one `bd list --json --all --limit 1` subprocess, paid once
//     per store and ONLY when a second bead database is actually on disk.
//
// A probe that fails, or that finds a row, ends the matter: neither outcome
// establishes that the active store is empty, and this says nothing it cannot
// establish.
func (s *BdStore) noticeIfStoreCannotSeeItsLedger(op string) {
	if s == nil || s.unreadStore.sawRows.Load() || s.unreadStore.verdictClaimed.Load() {
		return
	}
	if !s.unreadStore.verdictClaimed.CompareAndSwap(false, true) {
		return
	}
	if strings.TrimSpace(os.Getenv(AllowUnreadStoreReadEnvVar)) != "" {
		return
	}
	unread, activeStore, ok := UnreadBeadDatabase(s.dir)
	if !ok {
		return
	}
	populated, err := s.holdsAnyRow()
	if err != nil || populated {
		if populated {
			s.unreadStore.sawRows.Store(true)
		}
		return
	}
	_, _ = io.WriteString(s.unreadStoreNoticeSink(), UnreadStoreNotice(op, s.dir, unread, activeStore))
}

// holdsAnyRow reports whether the ACTIVE store holds a single row of any kind.
//
// This is the per-store question the notice is actually about, and it is asked
// with no selector at all — every status, infra rows and gate rows included —
// capped at one row so the answer costs the same on an empty ledger and on a
// hundred-thousand-bead one.
//
// It builds its own argv rather than routing through listViaBDList because that
// path forces a client-side limit for the default (issues) tier and would send
// `--limit 0`, fetching the entire ledger to answer a yes/no question.
//
// The flags are the ones listViaBDList passes unconditionally, deliberately:
// `--include-templates` would widen the question slightly (a scope holding only
// template rows probes as empty) but bd 1.0.4 rejects flags this tree still
// gates behind a version opt-in, and a probe that errors on an older bd silently
// disables the notice. A narrower question that always runs beats a wider one
// that sometimes does not.
func (s *BdStore) holdsAnyRow() (bool, error) {
	out, err := s.runBDTransientRead("list", "--json", "--all", "--include-infra", "--include-gates", "--limit", "1")
	if err != nil {
		return false, err
	}
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	if len(issues) > 0 {
		return true, nil
	}
	return false, parseErr
}

// unreadStoreNoticeSink returns where this store's notice is written. os.Stderr
// by default, so an operator running `gc ready` sees it and the controller's
// log captures it, without touching the stdout a caller may be parsing.
func (s *BdStore) unreadStoreNoticeSink() io.Writer {
	if s.noticeSink != nil {
		return s.noticeSink
	}
	return os.Stderr
}

// WithBdStoreNoticeSink redirects this store's operator notices away from
// stderr. Tests use it to capture the unread-store notice; production stores
// leave it unset.
func WithBdStoreNoticeSink(w io.Writer) BdStoreOption {
	return func(s *BdStore) {
		s.noticeSink = w
	}
}

// readyReadIsWholeFrontier reports whether a Ready query asks bd for the whole
// frontier rather than one agent's slice of it.
//
// bdReadyArgs sends exactly one selector — --assignee — so an empty answer to
// any other Ready query is an answer about this store's whole ready set. An
// assignee-scoped one is not: "nothing is ready for THIS agent" is the steady
// state of a healthy city and the single largest source of empty Ready results
// in the fleet.
func readyReadIsWholeFrontier(q ReadyQuery) bool {
	return strings.TrimSpace(q.Assignee) == ""
}

// listReadIsWholeLedger reports whether a List query is an unfiltered scan of
// the whole ledger.
//
// AllowScan without a filter is the only List shape whose empty answer is a
// statement about the store; everything else is a statement about a predicate.
// It is also the shape verifyCanonicalBdScopeStoreReady uses to gate `gc rig
// add`, which is why this guard must never turn it into an error.
func listReadIsWholeLedger(q ListQuery) bool {
	return q.AllowScan && !q.HasFilter()
}
