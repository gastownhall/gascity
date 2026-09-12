package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/deps"
	"github.com/gastownhall/gascity/internal/fsys"
)

const bdReadyProjectionMinVersion = "1.0.5"

// ErrReadyProjectionUnsupported is the named degraded state a cache enters when
// its backing store cannot serve the ready projection AT ALL, as opposed to
// having failed to serve it this cycle.
//
// It is a degrade rather than a partial result because it costs the snapshot
// exactly one column: the ROWS are whole, so List/Get/DepList keep serving from
// cache. That distinction is load-bearing — a cache that folds this into
// primePartialErr declines every cache-only read for the life of the process
// and sends each one to a live bd subprocess.
//
// It does NOT mean readiness is unaffected. Without the column every bead's
// IsBlocked is nil, and cachedBeadReady then derives readiness from the bead's
// OWN direct blocks/waits-for/conditional-blocks deps. That predicate is weaker
// than bd's is_blocked, which propagates blocked-ness transitively down
// parent-child edges: a child of a blocked parent reads blocked to bd and ready
// to the cache. The error direction is permissive — the cache would OFFER work
// whose gate has not opened — which for the control dispatcher is worse than
// hiding it (#3218). So readiness reads decline the cache and take their live
// backing.Ready fallback (CachingStore.readyReadsMustGoLive) while every other
// cached read keeps being served.
//
// It is reported exactly once per SCOPE: the verdict is latched in a registry
// keyed by scope path, not on the store object, because cmd/gc rebuilds a store
// per request and the control dispatcher rebuilds one every few seconds. Each
// cache over the scope still learns the verdict — it must, to decline its ready
// reads — but the operator notice and the failing subprocess are spent once.
var ErrReadyProjectionUnsupported = errors.New("ready projection unsupported by this bead store")

// readyProjectionDegrade is the latched verdict for one scope: the reason its
// ledger cannot answer the ready projection.
type readyProjectionDegrade struct {
	cause error
}

// readyProjectionScopeGuard bounds the degrade verdict, its operator notice,
// and the subprocess that discovers it to one per SCOPE.
//
// Per-scope rather than per-store object is the same correction unreadStoreGuard
// already carries (scopeGuards, unread_store_notice.go). A verdict memoized on
// the store object is memoized on nothing: cmd/gc's scoped stores are built per
// request, and cmd/gc's control-ready path rebuilds a store per
// controlReadyCacheTTL (3s) per scope for the life of the dispatcher. Bounding
// there turns "once per store" into "once per rebuild" — an unbounded operator
// notice, and a latch that never actually saves the failing 6-16s `bd sql` it
// exists to save.
type readyProjectionScopeGuard struct {
	// degrade latches the reason this scope cannot serve the projection. It is
	// never cleared: a ledger in front of a running process does not grow the
	// capability, and re-probing costs a guaranteed-failing subprocess per
	// cache prime and per reconcile.
	degrade atomic.Pointer[readyProjectionDegrade]
	// announced bounds the operator notice to one per scope. Compare-and-swap
	// rather than sync.Once for the reason verdictClaimed is: the winner writes
	// to a sink an operator owns, and no other caller should wait behind it.
	announced atomic.Bool
	// blockedDoor latches that some store over this scope proved `bd sql`
	// refused at runtime while `bd blocked` answered, so every later store
	// starts on the blocked door instead of re-deriving the SQL door from
	// metadata and re-spending the failing 6-16s `bd sql`. It is orthogonal to
	// degrade: degrade means "this scope cannot serve the projection at all"
	// (both doors gone), while blockedDoor means "the SQL door is proven
	// refused, use `bd blocked`" — a scope answering through the blocked door
	// has NOT reached the degrade verdict. Set once, never cleared, for the same
	// reason degrade is: the backend in front of the process does not regain
	// `bd sql` mid-run, and re-probing costs the very subprocess this saves.
	blockedDoor atomic.Bool
}

// readyProjectionGuards memoizes one guard per resolved scope path. It grows by
// one small entry per distinct bead scope a process reads — a city and its
// rigs — and entries live for the process because the verdict does.
var readyProjectionGuards sync.Map // map[string]*readyProjectionScopeGuard

// readyProjectionGuardForScope returns the shared guard for dir, creating it on
// first use. LoadOrStore, not a mutex: the losing racer discards a two-field
// struct and takes the winner's. A store with no directory gets the empty key,
// which is the right bucket rather than a leak: those stores run bd in the
// process's working directory, so they all address one scope.
func readyProjectionGuardForScope(dir string) *readyProjectionScopeGuard {
	key := scopeGuardKey(dir)
	if g, ok := readyProjectionGuards.Load(key); ok {
		return g.(*readyProjectionScopeGuard)
	}
	g, _ := readyProjectionGuards.LoadOrStore(key, &readyProjectionScopeGuard{})
	return g.(*readyProjectionScopeGuard)
}

// readyProjectionGuard returns this store's scope guard. Stores built by
// NewBdStore resolve it once at construction; the lookup here is the fallback
// for a zero-value BdStore.
func (s *BdStore) readyProjectionGuard() *readyProjectionScopeGuard {
	if s == nil {
		return nil
	}
	if s.readyProjectionScope != nil {
		return s.readyProjectionScope
	}
	return readyProjectionGuardForScope(s.dir)
}

type bdReadyProjectionRow struct {
	ID        string       `json:"id"`
	IsBlocked optionalBool `json:"is_blocked"`
}

func (s *BdStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	if len(items) == 0 {
		return items, nil
	}
	ids := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		// Message and nudge beads are notifications, not dependency-blocked ready
		// work, and bd's denormalized is_blocked column can flap NULL<->false for
		// them. Enriching those rows makes the CachingStore reconciler re-emit
		// bead.updated on every cycle (an event flood that starves gc-hook work
		// queries). Leave their IsBlocked at bd's nil fallback so the reconcile
		// diff converges.
		if skipBDReadyProjectionEnrichment(item) {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		ids = append(ids, item.ID)
	}
	if len(ids) == 0 {
		return items, nil
	}
	door, enabled, err := s.bdReadyProjectionEnabled()
	if err != nil {
		return items, err
	}
	if !enabled {
		return items, nil
	}

	projection, err := s.fetchReadyProjection(door, ids)
	if err != nil {
		return items, err
	}
	enriched := make([]Bead, len(items))
	copy(enriched, items)
	for i := range enriched {
		if skipBDReadyProjectionEnrichment(enriched[i]) {
			continue
		}
		blocked, ok := projection[enriched[i].ID]
		if !ok {
			continue
		}
		enriched[i].IsBlocked = cloneBoolPtr(&blocked)
	}
	return enriched, nil
}

func skipBDReadyProjectionEnrichment(item Bead) bool {
	return item.ID == "" ||
		item.Status == "closed" ||
		item.IsBlocked != nil ||
		item.Type == "message" ||
		beadHasLabel(item, "gc:nudge")
}

// readyProjectionDoor names the bd verb that fills the is_blocked column for
// one scope.
//
// There are two, and which one a scope takes is decided by the backend gate
// below rather than by preference. They are NOT interchangeable: `bd sql`
// returns the column verbatim for every active row, while `bd blocked` returns
// only the rows bd can name a blocker for (see fetchReadyProjectionViaBlocked).
// So the SQL door stays primary wherever it works, and the blocked door is
// reached only where the alternative is no column at all.
type readyProjectionDoor int

const (
	// readyProjectionDoorSQL projects `select id,is_blocked from issues|wisps`.
	// It is the column itself, and it is what every Dolt and DoltLite scope
	// takes — unchanged by this file's second door.
	readyProjectionDoorSQL readyProjectionDoor = iota
	// readyProjectionDoorBlocked projects `bd blocked --json`, bd's own verb
	// over its own blocked role. It answers on every backend, including the
	// hosted-Postgres work stores where `bd sql` is not implemented.
	readyProjectionDoorBlocked
)

func (s *BdStore) bdReadyProjectionEnabled() (readyProjectionDoor, bool, error) {
	// The scope verdict outranks anything this store object knows: a store
	// built after some other store over the same scope reached it must neither
	// re-spend the discovery nor re-announce it. It still REPORTS the degrade,
	// on every call, because that error is how each cache over the scope learns
	// to decline its readiness reads.
	if cause := s.latchedReadyProjectionDegrade(); cause != nil {
		return readyProjectionDoorSQL, false, fmt.Errorf("bd ready projection scope verdict: %w: %w", ErrReadyProjectionUnsupported, cause)
	}
	s.readyProjectionMu.Lock()
	defer s.readyProjectionMu.Unlock()
	// Probe the capability once per process. Operators must restart gc after
	// changing bd versions or re-pointing a scope at another backend to
	// re-evaluate ready-projection support.
	if s.readyProjectionChecked {
		return s.readyProjectionDoorValue, s.readyProjectionEnabled, s.readyProjectionVersionErr
	}
	// A backend gc does not implement no longer costs the scope its column: it
	// costs the scope `bd sql`. The gate's reason for withholding that call is
	// that gc cannot assume an unknown backend's SCHEMA carries gc's
	// issues/wisps projection — and that reason does not reach `bd blocked`,
	// which is bd's own verb over bd's own role, answered by whatever storage
	// bd opened. The second reason a fresh store starts on the blocked door is
	// the scope-latched runtime refusal: a metadata-registered backend bd
	// nevertheless opened embedded (isBdSQLUnsupportedInEmbeddedMode) had its
	// `bd sql` proven refused by an earlier store, and switchToBlockedDoor
	// recorded that on the shared guard so this rebuild does not re-spend it.
	door := readyProjectionDoorSQL
	if s.readyProjectionBackendRefusal() != nil || s.readyProjectionBlockedDoorLatched() {
		door = readyProjectionDoorBlocked
	}
	s.readyProjectionDoorValue = door
	out, err := s.runner(s.dir, "bd", "version")
	if err != nil {
		return door, false, fmt.Errorf("bd ready projection version gate: %w", err)
	}
	version, err := parseBDVersion(string(out))
	if err != nil {
		return door, false, fmt.Errorf("bd ready projection version gate: %w", err)
	}
	s.readyProjectionChecked = true
	// A bd that predates the projection leaves every IsBlocked nil, which is the
	// same state as a ledger that refuses `bd sql`, and owes the same fail-safe.
	// It used to return (false, nil) — no error, so no degrade — on the reading
	// that "the absence costs only the enrichment". That held only while
	// depsComplete was hardcoded false on this store: with the snapshot's own
	// edges now serving readiness (bdstore_inline_deps.go), a silent (false, nil)
	// hands every readiness handle to the dependency-derived predicate, which
	// does not propagate down parent-child and so offers the control dispatcher
	// work whose gate has not opened (#3218). Naming the degrade costs an old bd
	// its CACHED readiness read — those take a live `bd ready`, every other
	// cached read keeps serving — and costs a current bd nothing.
	//
	// Unlike the backend and runtime refusals it is memoized on the STORE rather
	// than latched in the scope guard: which bd is on PATH is a property of the
	// process, not of the ledger sitting in that directory, and the guard is
	// never cleared. Latching it there would let one store's verdict outlive a
	// bd upgrade for every later store over the same scope.
	if deps.CompareVersions(version, bdReadyProjectionMinVersion) < 0 {
		s.readyProjectionEnabled = false
		s.readyProjectionVersionErr = fmt.Errorf("bd ready projection version gate: %w: bd %s predates %s, which introduced the is_blocked projection",
			ErrReadyProjectionUnsupported, version, bdReadyProjectionMinVersion)
		return door, false, s.readyProjectionVersionErr
	}
	s.readyProjectionEnabled = true
	return door, true, nil
}

// switchToBlockedDoor records that this scope's `bd sql` was refused at runtime
// while `bd blocked` answers, so this store and every store built later over the
// same scope go straight to the blocked door.
//
// The choice is latched in the scope guard, not just on this store object,
// because cmd/gc builds a store per request and the control-ready scan rebuilds
// one per scope every controlReadyCacheTTL: a store-local flag would let the
// next store re-derive the SQL door from metadata and re-spend the failing
// 6-16s `bd sql` a few times a minute, forever. It is a DISTINCT verdict from
// the degrade the guard also carries — "SQL door proven refused, use `bd
// blocked`", not "this scope is out of doors at all" — so it rides its own
// field (readyProjectionScopeGuard.blockedDoor), which bdReadyProjectionEnabled
// consults before the metadata-derived door.
func (s *BdStore) switchToBlockedDoor() {
	s.readyProjectionMu.Lock()
	defer s.readyProjectionMu.Unlock()
	s.readyProjectionDoorValue = readyProjectionDoorBlocked
	if g := s.readyProjectionGuard(); g != nil {
		g.blockedDoor.Store(true)
	}
}

// readyProjectionBlockedDoorLatched reports whether some store over this scope
// already proved `bd sql` refused at runtime and switched to the blocked door.
// A fresh store consults it before deriving the door from metadata, so the
// proven refusal survives the per-request / per-controlReadyCacheTTL store
// rebuilds instead of re-spending the failing `bd sql` on each one.
func (s *BdStore) readyProjectionBlockedDoorLatched() bool {
	g := s.readyProjectionGuard()
	if g == nil {
		return false
	}
	return g.blockedDoor.Load()
}

// readyProjectionBackendRefusal reports why this scope's backend cannot answer
// `bd sql`, or nil when nothing on disk says it cannot.
//
// The version gate above asks the wrong question. `bd sql` is a raw-database
// escape hatch, and whether bd implements it is a property of the BACKEND it
// opened, not of the bd release: bd refuses it outright unless it holds a SQL
// session ("'bd sql' is not yet supported in embedded mode", cmd/bd/sql.go).
// gc's own composition root already names which backends this build implements
// — reads their metadata shape, projects their environment, manages their
// runtime (contract.RegisteredBackends). A scope served through the linked
// beads library under any other name is one gc knows nothing about, so
// assuming its `bd sql` works, and that its schema carries gc's issues/wisps
// projection, is a guess. Withholding the call is the honest default, and it
// costs that scope the enrichment rather than its correctness: readiness reads
// there decline the cache and take bd's own verdict live (see
// ErrReadyProjectionUnsupported), while every other cached read keeps serving.
//
// A scope naming a registered backend — including metadata that names none, and
// including metadata gc cannot read — reaches nil here and takes exactly the
// path it took before, so the Dolt cities are untouched. Nothing on disk proves
// a Dolt SQL server is REACHABLE, only that gc implements the backend; a bd
// that then refuses anyway is caught by the runtime latch in
// fetchReadyProjection.
func (s *BdStore) readyProjectionBackendRefusal() error {
	_, _, err := contract.LoadMetadataState(fsys.OSFS{}, filepath.Join(s.dir, ".beads", "metadata.json"))
	if err == nil || !errors.Is(err, contract.ErrUnknownBackend) {
		return nil
	}
	return err
}

// disableReadyProjectionLocked latches the projection off for this scope and
// states the reason once. Callers hold readyProjectionMu.
//
// The latch is one-way for the same reason the conditional-release one is: the
// ledger in front of the process cannot grow a capability mid-run, and
// re-probing per cycle costs a guaranteed-failing subprocess on every cache
// prime and every reconcile — which is exactly the defect this closes.
func (s *BdStore) disableReadyProjectionLocked(reason error) {
	s.readyProjectionEnabled = false
	s.readyProjectionChecked = true
	s.latchReadyProjectionDegrade(reason)
}

// latchReadyProjectionDegrade records the scope verdict and announces it once.
//
// The notice states what actually changes for the operator: readiness reads on
// this scope stop being served from cache and take a live `bd ready` instead,
// every other cached read keeps serving, and the failing `bd sql` is not spent
// again. It deliberately does not claim the cache is unaffected — the
// dependency-derived predicate the cache would otherwise use is not equivalent
// to bd's is_blocked (see ErrReadyProjectionUnsupported).
func (s *BdStore) latchReadyProjectionDegrade(cause error) {
	g := s.readyProjectionGuard()
	if g == nil {
		return
	}
	g.degrade.CompareAndSwap(nil, &readyProjectionDegrade{cause: cause})
	if !g.announced.CompareAndSwap(false, true) {
		return
	}
	_, _ = fmt.Fprintf(s.noticeWriter(),
		"gc: ready-projection enrichment disabled for %s: %v\n"+
			"gc: ready reads on this scope answer from a live `bd ready` instead of the cache; other cached reads keep serving and no further projection subprocess is spent.\n",
		s.dir, cause)
}

// latchedReadyProjectionDegrade returns the reason this SCOPE cannot serve the
// ready projection, or nil when no store over it has reached that verdict.
func (s *BdStore) latchedReadyProjectionDegrade() error {
	g := s.readyProjectionGuard()
	if g == nil {
		return nil
	}
	verdict := g.degrade.Load()
	if verdict == nil {
		return nil
	}
	return verdict.cause
}

// latchReadyProjectionUnsupported records bd's own refusal of `bd sql`, so no
// later cycle — and no store built later over the same scope — spends the call
// again.
func (s *BdStore) latchReadyProjectionUnsupported(cause error) {
	s.readyProjectionMu.Lock()
	defer s.readyProjectionMu.Unlock()
	s.disableReadyProjectionLocked(cause)
}

func (s *BdStore) fetchReadyProjection(door readyProjectionDoor, ids []string) (map[string]bool, error) {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		// An id under a relocated class's reserved prefix is not a row of this
		// ledger, so bd's answer about it would be an empty that reads as
		// "unblocked-unknown". Drop it from the request rather than refusing the
		// batch: this projection is handed EVERY active bead by cache
		// prime/reconcile, and a whole-batch refusal cost every other row its
		// is_blocked on every cycle, permanently — the call sites only
		// recordProblem and continue, and the refusal is a pure function of
		// config, so it never healed. A dropped id keeps its last cached value
		// (preserveCachedReadyProjectionLocked), which is the documented benign
		// state and exactly what its absence produced before this guard existed.
		//
		// The drop is silent on purpose. Nothing should mint a reserved class
		// prefix into this ledger — only the relocated class engine mints under
		// one, and a migration preserves the copied rows' original work-prefix
		// ids — so this is a should-never-happen that costs one row an
		// optimization, and a per-cycle log for it would be the reconcile-path
		// noise skipBDReadyProjectionEnrichment above exists to avoid.
		if len(s.relocatedClassesForID(id)) > 0 {
			continue
		}
		wanted[id] = struct{}{}
	}
	if len(wanted) == 0 {
		return map[string]bool{}, nil
	}

	if door == readyProjectionDoorBlocked {
		return s.projectViaBlockedDoor(wanted, nil)
	}
	result := make(map[string]bool, len(wanted))

	// bd exposes this as an active-row projection: the SQL filters out closed
	// rows so cache prime/reconcile cost stays O(active work) instead of
	// scanning unbounded closed issue/wisp history every cycle. The ids
	// argument is a cache-side allow-list so callers can keep their requested
	// surface bounded. A row that races closed between the list snapshot and
	// this fetch drops out of the projection; the reconciler preserves its last
	// cached is_blocked (preserveCachedReadyProjectionLocked) so the absence
	// does not flap a spurious bead.updated.
	out, err := s.runner(s.dir, "bd", "sql", readyProjectionSQL(), "--json")
	if err != nil {
		if isBdSQLUnsupportedInEmbeddedMode(err) {
			// Belt-and-braces to the backend gate: a scope whose metadata does
			// not name its backend, or names one gc implements while bd opened
			// it some other way, only learns this from bd's own answer. It is a
			// permanent property of the ledger, so switchToBlockedDoor latches
			// the choice in the scope guard — shared by every store rooted here
			// — and every later rebuild consults it before the metadata-derived
			// door. Without that scope latch the switch lived only on this store
			// object, and cmd/gc rebuilds the store per request while the
			// control-ready scan rebuilds one per scope every
			// controlReadyCacheTTL: the next store re-picked the SQL door and
			// re-spent this failing 6-16s subprocess on every prime and every
			// reconcile, indefinitely.
			s.switchToBlockedDoor()
			return s.projectViaBlockedDoor(wanted, err)
		}
		return nil, fmt.Errorf("bd sql ready projection: %w", err)
	}
	var rows []bdReadyProjectionRow
	if err := json.Unmarshal(extractJSON(out), &rows); err != nil {
		return nil, fmt.Errorf("bd sql ready projection: parsing JSON: %w", err)
	}
	for _, row := range rows {
		if row.ID == "" || !row.IsBlocked.set {
			continue
		}
		if _, ok := wanted[row.ID]; !ok {
			continue
		}
		result[row.ID] = row.IsBlocked.value
	}
	return result, nil
}

func readyProjectionSQL() string {
	return "select id,is_blocked from issues where status <> 'closed' union all select id,is_blocked from wisps where status <> 'closed'"
}

// projectViaBlockedDoor runs the blocked door and classifies its failure the
// way the SQL door's is classified, because the two failures mean different
// things and owe different verdicts.
//
// bd refusing the VERB is a permanent property of the ledger in front of the
// process, so it latches: the scope is out of doors and every later cache over
// it is told so without spending a subprocess. Anything else — a timeout, a
// connection reset, a busy server — is this cycle's failure, so it is returned
// plain. That leaves the snapshot partial (CachingStore.applyReadyProjection),
// which already declines cached readiness reads, and the next reconcile retries.
// Latching a blip would cost a long-lived dispatcher its projection for the life
// of the process.
//
// withheld, when non-nil, is bd's own refusal of `bd sql`. It rides along on
// the latched cause so an operator who meets the notice can see both doors at
// once instead of one. A caller that arrived here from the backend gate passes
// nil and the gate's reason is read back below — on the failure path only, so
// the healthy path costs no metadata read per cycle.
func (s *BdStore) projectViaBlockedDoor(wanted map[string]struct{}, withheld error) (map[string]bool, error) {
	projection, err := s.fetchReadyProjectionViaBlocked(wanted)
	if err == nil {
		return projection, nil
	}
	if !isBdBlockedUnsupported(err) {
		return nil, err
	}
	if withheld == nil {
		withheld = s.readyProjectionBackendRefusal()
	}
	cause := err
	if withheld != nil {
		cause = fmt.Errorf("%w; bd sql was withheld: %w", err, withheld)
	}
	s.latchReadyProjectionUnsupported(cause)
	return nil, fmt.Errorf("bd ready projection: %w: %w", ErrReadyProjectionUnsupported, cause)
}

// isBdBlockedUnsupported reports whether bd refused the blocked verb itself, as
// opposed to failing while answering it.
//
// It matches on bd's text for the same reason its `bd sql` sibling does: no
// sentinel crosses the subprocess boundary, so the error arrives as bytes. The
// classification is deliberately narrow and fails SAFE in both directions — a
// refusal it fails to recognize costs a retry per cycle rather than a wrong
// answer, and a blip it misreads as a refusal costs the projection, never the
// readiness verdict, because the degrade sends readiness to a live `bd ready`.
func isBdBlockedUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "blocked") {
		return false
	}
	return strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "not yet supported") ||
		strings.Contains(msg, "unsupported")
}

// bdBlockedRow is the one field gc reads off `bd blocked --json`. bd renders a
// full types.BlockedIssue there — the whole issue plus BlockedBy and
// BlockedByCount — and gc wants none of it: membership in the response IS the
// verdict.
type bdBlockedRow struct {
	ID string `json:"id"`
}

// fetchReadyProjectionViaBlocked fills the is_blocked column from `bd blocked
// --json` for a scope whose backend does not implement `bd sql`.
//
// # Why this is the same column
//
// bd's blocked verb is not a direct-dependency answer. It seeds its row set
// with `SELECT id FROM issues|wisps WHERE is_blocked = 1 AND status <> 'closed'
// AND status <> 'pinned'` (issueops.GetBlockedIssuesInTx) — literally the
// denormalized, transitively-propagated column that bd's own ready filters with
// `is_blocked = 0` (sqlbuild.BuildReadyWorkWhere). Same column, same closure,
// so a child of a blocked parent reads blocked here exactly as it does to
// `bd ready`. That equality is the whole reason this door is allowed to exist:
// a subtly different definition would offer the control dispatcher work whose
// gate has not opened (#3218).
//
// # Absence means not blocked, explicitly
//
// bd returns only blocked rows, so every requested id missing from the response
// is written false rather than left absent. Leaving it absent would leave
// Bead.IsBlocked nil, and cachedBeadReady then falls back to the bead's OWN
// direct blocks/waits-for/conditional-blocks deps — the weaker predicate this
// projection exists to replace. The write is unconditional for that reason.
//
// # The one place it is not the column
//
// After seeding from is_blocked, bd narrows its OUTPUT to rows whose blocker it
// can NAME: an active blocking dep whose target is a resident non-closed row,
// or, failing that, any parent-child edge. A row carrying is_blocked = 1 with
// neither is dropped from the response and therefore reads false here. bd's own
// recompute (issueops.shouldBeBlockedDisjunction) can only produce such a row
// from a stale flag — the state `bd recompute-blocked` and `bd sync` exist to
// repair, and the state issueops.countStaleIsBlockedSQL counts — or from a
// waits-for onto a spawner that closed while its gate stayed shut, which in a
// gc-minted graph is also a molecule step and so carries the parent-child edge
// that attributes it (formula.collectRecipeDeps mints waits-for only between
// steps of one formula). Measured on maintainer-city's hosted-Postgres work
// store: 117 active rows, `bd blocked` names 3, `bd ready` returns 112, and the
// remaining 3 are accounted for by bd's own non-is_blocked ready predicates (2
// in_progress rows, which bd's ready never returns because cmd/bd/ready.go
// hardcodes Status "open", and 1 molecule, which sqlbuild.ReadyWorkExcludeTypes
// drops) — zero unattributable rows.
//
// This is why the SQL door stays primary rather than being replaced: where
// `bd sql` works it returns the column with no attribution filter at all.
//
// # Cost
//
// One subprocess per cache prime and per reconcile, independent of the number
// of beads: bd answers the whole scope in one call the way `bd sql` did. Inside
// bd the work scales with the BLOCKED set, not the ledger — 2·|blocked|
// dependency lookups plus a batched row fetch. Measured against maintainer-city
// live: 2.9s, against a `bd sql` that costs 6.5s there and then fails. What it
// buys is every readiness read between reconciles, which today each spend a
// live `bd ready` at ~2.5-4.4s.
func (s *BdStore) fetchReadyProjectionViaBlocked(wanted map[string]struct{}) (map[string]bool, error) {
	out, err := s.runner(s.dir, "bd", "blocked", "--json")
	if err != nil {
		return nil, fmt.Errorf("bd blocked ready projection: %w", err)
	}
	var rows []bdBlockedRow
	if err := json.Unmarshal(extractJSON(out), &rows); err != nil {
		return nil, fmt.Errorf("bd blocked ready projection: parsing JSON: %w", err)
	}
	blocked := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		blocked[row.ID] = struct{}{}
	}
	result := make(map[string]bool, len(wanted))
	for id := range wanted {
		_, isBlocked := blocked[id]
		result[id] = isBlocked
	}
	return result, nil
}
