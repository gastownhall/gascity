package beads

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
// So this states what is knowable — no read of this scope has returned a row in
// this process, and a second bead database sits unread beside the one that
// answered — on stderr, once per scope, and lets the read succeed. It carries
// `gc doctor`'s remediation word for word so the two never send an operator in
// different directions, and names an override because a store-layer guard with
// no escape is the difference between a warning and an outage.
//
// # Why it spends no subprocess
//
// The first shape of this notice confirmed the active store was empty with one
// more `bd list --limit 1`, synchronously, inside List and Ready. Store.List
// and Store.Ready take no context, so the store cannot see the budget its
// caller is holding, and that probe was charged to it: with the API's
// per-store status deadline at 250ms and bd answering the probe in 400ms, a
// List bd had ALREADY answered successfully came back to the handler as "list
// timed out: context deadline exceeded". A diagnostic that can turn a
// successful read into an error is worse than the silence it diagnoses, and no
// arrangement of a synchronous subprocess avoids it — binding the child to the
// caller's context kills the child but still spends the budget, and "skip when
// the deadline is close" needs a threshold nobody can pick.
//
// So the probe is gone rather than rescheduled, and the notice runs on evidence
// the read already paid for: the row latch, the on-disk shape (one file read
// and up to two stats), and nothing else. The price is a wider notice — an idle
// ledger reads the same as a re-pointed one from here — which the message says
// out loud. The question the probe was asking belongs to `gc doctor`, which
// enumerates both databases with no caller waiting on it.

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

// UnreadStoreNotice builds the line an empty whole-ledger read prints when no
// read of the scope that answered it has produced a row in this process and a
// second bead database sits unread in the same .beads/. op names the read, and
// activeStore is the .beads/ subdirectory the read was answered from.
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
// It deliberately does NOT claim rows were lost, nor that this scope is broken,
// nor that the active store is empty. A workspace `bd init` created and never
// filed a bead into produces this exact shape, an idle ledger answers the same
// way, and a message that overstates gets ignored the next time it is right.
// What it can say is that no read of this scope has returned a row in this
// process and that a second database is on disk, and it says exactly that.
func UnreadStoreNotice(op, scopeRoot, unread, activeStore string) string {
	return fmt.Sprintf("gc: %s returned no rows for %s from the %q bead store, no read of this scope has returned a row "+
		"in this process, and %s is a Dolt bead database this scope's .beads/metadata.json does not point at. "+
		"Nothing failed — bd opened the store its metadata names, ran the read successfully and matched nothing — so this "+
		"empty answer is indistinguishable from a real one while a second ledger sits unread beside it. gc canonicalizes "+
		"a managed scope's metadata to dolt_mode=server on `gc rig add`, `gc start`, `gc supervisor run`, `gc rig "+
		"set-endpoint` and `gc beads city use-managed|use-external`, which re-points a workspace initialized in embedded "+
		"mode without moving its rows. This is a notice and not a refusal, and it spends no bd subprocess of its own: an "+
		"idle ledger and a workspace `bd init` created but never filed a bead into look the same from here, and the only "+
		"ways to tell them apart are to open the database you were told to keep or to ask bd a second question on your "+
		"read's deadline. Run `gc doctor` (check bd-split-store) to see both databases and which one is active, then "+
		"export from a copy of the unread one, review with `bd import --dry-run`, and import into the active one; keep "+
		"both directories until reconciled. Set %s=1 to silence this while you reconcile.\n",
		op, scopeRoot, activeStore, unread, AllowUnreadStoreReadEnvVar)
}

// unreadStoreGuard is the per-SCOPE verdict behind the notice.
//
// Per-scope rather than per-call is the whole correction. The first version of
// this guard judged per-CALL and refused an empty Ready() on a demonstrably
// populated store, because "this call returned nothing" and "this scope holds
// nothing" are different claims and only the second one is evidence. sawRows
// latches the first, cheapest possible disproof — bd answered a read of this
// scope with a row — and once latched the guard is inert for the life of the
// process.
//
// Per-scope rather than per-STORE is what makes the one-shot bound true where
// it is actually needed. cmd/gc's scopedBdStoreForCity/scopedBdStoreForRig build
// a brand new BdStore for every request that needs a context-bound bd child, and
// internal/api's status handler reaches them through State.ScopedStoreLike on
// every /status — so a verdict memoized on the store degraded to once per READ
// there, and statusWorkCounts fans out over city plus every rig at once. Keying
// on the resolved scope path means a throwaway store inherits the verdict the
// process already reached.
//
// verdictClaimed carries the rest: the override check and the on-disk shape —
// one metadata read and up to two stats. It is a compare-and-swap latch rather
// than a sync.Once on purpose. once.Do makes every concurrent caller WAIT for
// the winner, and the winner here touches the filesystem and writes to stderr;
// on a supervisor with a goroutine per rig a single slow sink would become a
// stall across all of them. Losing the race means the verdict is already being
// reached by someone else, and the right thing for a diagnostic to do about
// that is nothing.
//
// It is claimed BEFORE the evidence is gathered, so the cheap negative — a
// scope with one ledger, which is every healthy city — costs one metadata read
// for the life of the process and two atomic loads on every read after it. The
// cost of that ordering is that a verdict survives a mode flip performed by
// another process; the process doing the flip announces it, and one notice per
// scope is the bound this is trading for.
type unreadStoreGuard struct {
	// sawRows latches when bd answered any read of this scope with at least
	// one row, BEFORE client-side filtering. It is the disproof, so it is
	// checked first and is never unlatched: a scope that once handed back rows
	// is not a workspace pointed at the wrong database.
	sawRows atomic.Bool
	// verdictClaimed bounds the shape check and the notice to one per scope,
	// without making any other read wait for them.
	verdictClaimed atomic.Bool
}

// scopeGuards memoizes one unreadStoreGuard per resolved scope path, so the
// notice is bounded per SCOPE rather than per store object. It grows by one
// small entry per distinct bead scope a process reads — a city and its rigs, a
// handful — and entries live for the process because the verdict does.
var scopeGuards sync.Map // map[string]*unreadStoreGuard

// scopeGuardKey is the registry key every per-scope verdict shares: the
// resolved scope path, or "" for a store with no directory. Sharing it keeps
// two guards over the same scope on the same key.
func scopeGuardKey(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Clean(dir)
}

// guardForScope returns the shared guard for dir, creating it on first use.
//
// LoadOrStore, not a mutex: the losing racer discards a two-field struct and
// takes the winner's, which is the same non-blocking property verdictClaimed
// has. A store with no directory gets the empty key, which is harmless —
// UnreadBeadDatabase declines that shape before any evidence is gathered.
func guardForScope(dir string) *unreadStoreGuard {
	key := scopeGuardKey(dir)
	if g, ok := scopeGuards.Load(key); ok {
		return g.(*unreadStoreGuard)
	}
	g, _ := scopeGuards.LoadOrStore(key, &unreadStoreGuard{})
	return g.(*unreadStoreGuard)
}

// unreadGuard returns this store's scope guard. Stores built by NewBdStore
// resolve it once at construction; the lookup here is the fallback for a
// zero-value BdStore.
func (s *BdStore) unreadGuard() *unreadStoreGuard {
	if s == nil {
		return nil
	}
	if s.unreadStore != nil {
		return s.unreadStore
	}
	return guardForScope(s.dir)
}

// noteServerRows records that bd handed this scope rows for some read.
//
// It is called with the count of rows bd RETURNED, not the count the caller
// received, because client-side filtering (assignee, tier, limit, parent) can
// reduce a real answer to nothing and that reduction says nothing about the
// store. Two atomics in the common case, one in the steady state.
func (s *BdStore) noteServerRows(rows int) {
	if s == nil || rows <= 0 {
		return
	}
	g := s.unreadGuard()
	if g == nil || g.sawRows.Load() {
		return
	}
	g.sawRows.Store(true)
}

// SawRows implements RowWitness from the same per-scope latch the notice
// consults, so a caller acting on the witness and an operator reading the
// notice can never be told different things about the same scope.
//
// It reads the latch and nothing else: no filesystem, no subprocess, one
// atomic load. The per-scope memoization is what makes the answer useful to
// the API server, where cmd/gc rebuilds a throwaway BdStore for every
// context-bound read — a store built a moment ago inherits the verdict the
// process already reached against the same directory.
func (s *BdStore) SawRows() bool {
	g := s.unreadGuard()
	return g != nil && g.sawRows.Load()
}

// noticeIfStoreCannotSeeItsLedger prints the unread-store notice at most once
// per scope, when an UNFILTERED whole-ledger read came back empty and the
// evidence supports it.
//
// Callers must only reach this from a read with no selector: a filtered empty
// result is evidence of nothing, which is the second correction this guard
// carries. The cost ladder is deliberate, in increasing order of expense, and
// it stops short of a subprocess on purpose (see the file header):
//
//  1. sawRows and verdictClaimed — two atomic loads. The first is true for
//     every scope that has ever answered with a row, which is every working
//     city that holds work; the second is true for every scope whose one-shot
//     verdict has already been reached.
//  2. the override env var and the on-disk shape — a getenv, one file read and
//     up to two stats, paid once per scope.
//
// There is no step 3. Confirming the active store is empty would mean asking bd
// a second question inside a read whose budget the caller already set, and a
// diagnostic that can spend a caller's deadline can fail the read it was meant
// to annotate. The notice describes the shape instead, and says so.
func (s *BdStore) noticeIfStoreCannotSeeItsLedger(op string) {
	g := s.unreadGuard()
	if g == nil || g.sawRows.Load() || g.verdictClaimed.Load() {
		return
	}
	if !g.verdictClaimed.CompareAndSwap(false, true) {
		return
	}
	if strings.TrimSpace(os.Getenv(AllowUnreadStoreReadEnvVar)) != "" {
		return
	}
	unread, activeStore, ok := UnreadBeadDatabase(s.dir)
	if !ok {
		return
	}
	_, _ = io.WriteString(s.noticeWriter(), UnreadStoreNotice(op, s.dir, unread, activeStore))
}

// noticeWriter returns where this store's operator notices are written.
// os.Stderr by default, so an operator running `gc ready` sees them and the
// controller's log captures them, without touching the stdout a caller may be
// parsing.
func (s *BdStore) noticeWriter() io.Writer {
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
