package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// bdLedgerRow is one row of the fake bd ledger below: the issue fields the
// cache reads plus the two dependency columns bd projects from the same query.
type bdLedgerRow struct {
	id        string
	ephemeral bool
	blocked   bool
	closed    bool
	assignee  string
	deps      []Dep
	// offscope marks a row bd's ledger HOLDS but this scope's cache never
	// receives: gc routes relocated classes (the `gcg-` graph beads) to another
	// store, so `bd list` and `bd query` for this scope do not hand it over.
	// bd's own answers — ready, sql, blocked — still see it, which is what
	// makes an edge onto it invisible to the cache's predicate and visible to
	// bd's column.
	offscope bool
}

// fakeBdLedger answers the bd subcommands a CachingStore prime spends, the way
// a real bd does.
//
// It renders `dependencies` and `dependency_count` from ONE source — the row's
// edges — because that is what bd does: sqlbuild.SearchCountsSQL projects
// deps_json (JSON_ARRAYAGG over the dependency table) and dep_count (COUNT(*)
// over the same table WHERE type='blocks') side by side in the counts
// mega-query, for the issues family and the wisps family alike. Wiring them to
// one source here is what makes the witness in bdstore_inline_deps.go a real
// measurement rather than a fixture-shaped tautology: truncateDeps below breaks
// the two apart and the witness must notice.
type fakeBdLedger struct {
	rows []bdLedgerRow
	// depListRefusal, when set, is how `bd dep list` fails. maintainer-city's
	// bd/Postgres work store answers exactly this (ga-7i7ts), which is why the
	// completeness verdict cannot be built on DepListBatch.
	depListRefusal string
	// truncateDeps drops these ids' inline edges while leaving their
	// dependency_count intact, modeling a bd whose list JSON is short.
	truncateDeps map[string]bool
	// omitDependencyCount renders rows without the count column, modeling a bd
	// that cannot testify about its own projection.
	omitDependencyCount bool

	calls [][]string
}

func (f *fakeBdLedger) run(_, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	joined := strings.Join(args, " ")
	switch {
	case joined == "version":
		return []byte("bd version 1.1.0\n"), nil
	case args[0] == "list":
		return f.renderRows(func(r bdLedgerRow) bool { return !r.ephemeral && !r.closed && !r.offscope })
	case args[0] == "query":
		return f.renderRows(func(r bdLedgerRow) bool { return r.ephemeral && !r.closed && !r.offscope })
	case args[0] == "ready":
		// bd ready filters is_blocked = 0 (sqlbuild.ReadyWhere). This is the
		// backing's own verdict, the one the cache may never exceed.
		return f.renderRows(func(r bdLedgerRow) bool { return !r.blocked && !r.closed })
	case args[0] == "blocked":
		return f.renderBlocked()
	case args[0] == "show":
		// `bd show --json` answers from types.IssueDetails, which embeds
		// types.Issue — and NOTHING in beads carries a `json:"is_blocked"` tag.
		// So every row a Get refresh installs arrives with IsBlocked nil, no
		// matter how healthy bd is. That is the whole mechanism behind
		// ga-cfhgr: renderRows never emits the key either.
		return f.renderRows(func(r bdLedgerRow) bool { return r.id == args[len(args)-1] })
	case args[0] == "update":
		return f.applyUpdate(args[2:])
	case args[0] == "close":
		return f.applyClose(args[1:])
	case args[0] == "sql":
		return f.renderReadyProjection()
	case args[0] == "dep" && len(args) > 1 && args[1] == "list":
		if f.depListRefusal != "" {
			return nil, errors.New(f.depListRefusal)
		}
		return f.renderDepRecords(args[2:])
	}
	return nil, fmt.Errorf("unexpected command: %s", joined)
}

// applyUpdate mutates the ledger the way `bd update` does. Only the fields the
// staleness tests write are supported; anything else is a loud failure rather
// than a silent no-op.
func (f *fakeBdLedger) applyUpdate(args []string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("bd update: no id")
	}
	id := args[0]
	for i := 1; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--assignee":
			if err := f.mutate(id, func(r *bdLedgerRow) { r.assignee = args[i+1] }); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("bd update: unsupported flag %s", args[i])
		}
	}
	return []byte("{}"), nil
}

// applyClose mutates the ledger the way `bd close` does. It deliberately does
// NOT recompute any other row's is_blocked: the fixtures that close a bead
// declare a topology where bd's own verdict is unchanged by the close, which is
// exactly what makes the cache's post-close answer falsifiable.
func (f *fakeBdLedger) applyClose(args []string) ([]byte, error) {
	closed := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--force", "--json":
		case "--reason":
			i++
		default:
			if err := f.mutate(args[i], func(r *bdLedgerRow) { r.closed = true }); err != nil {
				return nil, err
			}
			closed = true
		}
	}
	if !closed {
		return nil, errors.New("bd close: no id")
	}
	return []byte("{}"), nil
}

func (f *fakeBdLedger) mutate(id string, apply func(*bdLedgerRow)) error {
	for i := range f.rows {
		if f.rows[i].id == id {
			apply(&f.rows[i])
			return nil
		}
	}
	return fmt.Errorf("bd: no such bead %q", id)
}

func (f *fakeBdLedger) renderRows(keep func(bdLedgerRow) bool) ([]byte, error) {
	out := make([]map[string]any, 0, len(f.rows))
	for _, row := range f.rows {
		if !keep(row) {
			continue
		}
		status := "open"
		if row.closed {
			status = "closed"
		}
		issue := map[string]any{
			"id":         row.id,
			"title":      row.id,
			"status":     status,
			"issue_type": "task",
		}
		if row.ephemeral {
			issue["ephemeral"] = true
		}
		if row.assignee != "" {
			issue["assignee"] = row.assignee
		}
		blocking := 0
		deps := make([]map[string]string, 0, len(row.deps))
		for _, dep := range row.deps {
			if dep.Type == "blocks" {
				blocking++
			}
			deps = append(deps, map[string]string{
				"issue_id":      dep.IssueID,
				"depends_on_id": dep.DependsOnID,
				"type":          dep.Type,
			})
		}
		if !f.omitDependencyCount {
			issue["dependency_count"] = blocking
		}
		if len(deps) > 0 && !f.truncateDeps[row.id] {
			issue["dependencies"] = deps
		}
		out = append(out, issue)
	}
	return json.Marshal(out)
}

func (f *fakeBdLedger) renderReadyProjection() ([]byte, error) {
	out := make([]map[string]any, 0, len(f.rows))
	for _, row := range f.rows {
		// The projection query filters `status <> 'closed'` on both tiers
		// (bdReadyProjectionSQL), so a closed row simply has no is_blocked to
		// report — the same shape the cache must survive between reconciles.
		if row.closed {
			continue
		}
		out = append(out, map[string]any{"id": row.id, "is_blocked": row.blocked})
	}
	return json.Marshal(out)
}

// renderBlocked answers `bd blocked --json` the way issueops.GetBlockedIssuesInTx
// does, in both halves, because only the second half can falsify the door built
// on it:
//
//  1. Seed from the denormalized is_blocked column —
//     `WHERE is_blocked = 1 AND status <> 'closed'` — which is the same column
//     `bd ready` filters with `is_blocked = 0` above. Rendering both from
//     bdLedgerRow.blocked is what makes the two answers provably the same
//     closure here as they are in bd.
//  2. Narrow to rows whose blocker bd can NAME: an active blocking dep whose
//     target is a live ledger row, or failing that any parent-child edge, whose
//     parent is attributed without regard to the parent's own state. A row with
//     neither is dropped from bd's OUTPUT even though its column says blocked.
//
// Modeling (2) is the point. Without it every fixture row would come back and
// a gc-side reader that mistook membership for the column would pass for the
// wrong reason.
func (f *fakeBdLedger) renderBlocked() ([]byte, error) {
	live := make(map[string]struct{}, len(f.rows))
	for _, row := range f.rows {
		if !row.closed {
			live[row.id] = struct{}{}
		}
	}
	out := make([]map[string]any, 0, len(f.rows))
	for _, row := range f.rows {
		if !row.blocked || row.closed {
			continue
		}
		var blockers []string
		for _, dep := range row.deps {
			if !isReadyBlockingDependencyType(dep.Type) {
				continue
			}
			if _, ok := live[dep.DependsOnID]; !ok {
				continue
			}
			blockers = append(blockers, dep.DependsOnID)
		}
		if len(blockers) == 0 {
			for _, dep := range row.deps {
				if dep.Type == "parent-child" {
					blockers = []string{dep.DependsOnID}
					break
				}
			}
		}
		if len(blockers) == 0 {
			continue
		}
		out = append(out, map[string]any{
			"id":               row.id,
			"title":            row.id,
			"status":           "open",
			"blocked_by":       blockers,
			"blocked_by_count": len(blockers),
		})
	}
	return json.Marshal(out)
}

func (f *fakeBdLedger) renderDepRecords(ids []string) ([]byte, error) {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[strings.TrimPrefix(id, "--")] = true
	}
	out := make([]map[string]string, 0)
	for _, row := range f.rows {
		if !wanted[row.id] {
			continue
		}
		for _, dep := range row.deps {
			out = append(out, map[string]string{
				"issue_id":      dep.IssueID,
				"depends_on_id": dep.DependsOnID,
				"type":          dep.Type,
			})
		}
	}
	return json.Marshal(out)
}

func (f *fakeBdLedger) callsNamed(sub ...string) [][]string {
	var found [][]string
	for _, call := range f.calls {
		if len(call) < 1+len(sub) {
			continue
		}
		match := true
		for i, want := range sub {
			if call[1+i] != want {
				match = false
				break
			}
		}
		if match {
			found = append(found, call)
		}
	}
	return found
}

// bdReadyDisagreementLedger is #5184's fixture, moved onto a bd-backed store and
// extended with the tier that actually carries molecule steps.
//
//	blocker (open) <-- blocks -- parent <-- parent-child -- child
//	gcg-offscope (in bd's ledger, not in this cache) <-- blocks -- xstore
//	parent <-- parent-child -- wisp-step   (ephemeral)
//	unrelated <-- blocks -- wisp-gate      (ephemeral)
//	unrelated <-- waits-for -- gate-open   (gate already opened)
//
// child and wisp-step carry no blocking dep of their own, so the cache's
// dependency-derived predicate calls them ready while bd marks them
// is_blocked=1 through the parent-child edge. xstore carries one, but its target
// is not resident in this scope's cache, and cachedBeadReady treats a dep as
// blocking only when statusByID holds the target — so that edge is invisible to
// it too. wisp-gate is the wisp tier's blocking edge, so the tier bd serves
// through `bd query` rather than `bd list` also has a row that can prove — and
// disprove — the projection. unrelated is the control both models call ready.
//
// Two rows exist so the projection can be disproved in BOTH directions.
//
// gcg-offscope is xstore's blocker as a row of bd's ledger rather than as a
// dangling id. That is the only internally consistent way to state what the
// fixture claims: bd's recompute joins the target table
// (issueops.shouldBeBlockedDisjunction), so a dep onto an id bd cannot see
// leaves is_blocked = 0 and bd's ready OFFERS the row. A cross-store blocker is
// therefore a row bd holds and gc's class routing keeps out of THIS cache,
// which is exactly what offscope models. It is itself blocked so bd's ready
// hides it too, keeping every backing-verdict assertion in this file unchanged.
//
// gate-open is the reverse disagreement, and it is what makes "absence means
// not blocked" falsifiable. Its waits-for gate has opened, so bd's column reads
// is_blocked = 0 and bd's ready OFFERS it — while the cache's direct-dep
// predicate, which calls any non-closed blocking target a blocker, would HIDE
// it. A projection that leaves unblocked rows nil instead of writing false
// falls back to that predicate and starves the row.
func bdReadyDisagreementLedger() *fakeBdLedger {
	return &fakeBdLedger{
		depListRefusal: `exit status 1: Error: operation "IssueRelations" not supported by the postgres backend`,
		rows: []bdLedgerRow{
			{id: "bd-blocker"},
			{id: "bd-parent", blocked: true, deps: []Dep{{IssueID: "bd-parent", DependsOnID: "bd-blocker", Type: "blocks"}}},
			{id: "bd-child", blocked: true, deps: []Dep{{IssueID: "bd-child", DependsOnID: "bd-parent", Type: "parent-child"}}},
			{id: "bd-xstore", blocked: true, deps: []Dep{{IssueID: "bd-xstore", DependsOnID: "gcg-offscope", Type: "blocks"}}},
			{id: "gcg-offscope", offscope: true, blocked: true, deps: []Dep{{IssueID: "gcg-offscope", DependsOnID: "bd-blocker", Type: "blocks"}}},
			{id: "bd-unrelated"},
			{id: "bd-gate-open", deps: []Dep{{IssueID: "bd-gate-open", DependsOnID: "bd-unrelated", Type: "waits-for"}}},
			{id: "bd-wisp-step", ephemeral: true, blocked: true, deps: []Dep{{IssueID: "bd-wisp-step", DependsOnID: "bd-parent", Type: "parent-child"}}},
			{id: "bd-wisp-gate", ephemeral: true, blocked: true, deps: []Dep{{IssueID: "bd-wisp-gate", DependsOnID: "bd-unrelated", Type: "blocks"}}},
		},
	}
}

func primedBdCache(t *testing.T, ledger *fakeBdLedger) (*CachingStore, *BdStore) {
	t.Helper()
	return primeBdCacheOverScope(t, ledger, t.TempDir())
}

// primedBdCacheOnUnimplementedBackend primes the same fixture over a scope whose
// metadata names a backend gc does not register — maintainer-city's shape. That
// scope's `bd sql` is withheld by the backend gate, so its is_blocked column can
// only come from `bd blocked`.
func primedBdCacheOnUnimplementedBackend(t *testing.T, ledger *fakeBdLedger) (*CachingStore, *BdStore) {
	t.Helper()
	scope := t.TempDir()
	writeScopeMetadata(t, scope, map[string]any{
		"database":   "dolt",
		"backend":    "postgres",
		"dolt_mode":  "server",
		"project_id": "d2e95604-e869-478c-ad1a-ddee6e8bc3fc",
	})
	return primeBdCacheOverScope(t, ledger, scope)
}

func primeBdCacheOverScope(t *testing.T, ledger *fakeBdLedger, scope string) (*CachingStore, *BdStore) {
	t.Helper()
	store := NewBdStore(scope, ledger.run)
	cache := NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache, store
}

// TestBdBackedCacheServesTheCompleteReadyProjection is ga-tgpfm.
//
// CachingStore.cachedReadyCompleteOnly gates on depsComplete, which came from
// BdStore.listIncludesCompleteDependencies — a hardcoded false. So every scope
// on a BdStore answered ReadyContext with "bead cache unavailable" forever and
// took a live `bd ready` per tick; on maintainer-city, whose work class is
// cross-region hosted Postgres, that read costs ~4.4s and the order loop starved.
//
// Red before the fix, on the unmodified fixture:
//
//	ReadyContext error = reading complete ready projection from cache: bead cache unavailable, want rows served from cache
func TestBdBackedCacheServesTheCompleteReadyProjection(t *testing.T) {
	ledger := bdReadyDisagreementLedger()
	cache, store := primedBdCache(t, ledger)

	rows, err := cache.ReadyContext(context.Background())
	if err != nil {
		t.Fatalf("ReadyContext error = %v, want rows served from cache", err)
	}
	want := wantReadyIDs("bd-blocker", "bd-gate-open", "bd-unrelated")
	if got := sortedIDs(rows); !equalIDs(got, want) {
		t.Fatalf("ReadyContext = %v, want %v", got, want)
	}

	if !store.listIncludesCompleteDependencies() {
		t.Fatal("BdStore did not witness the inline dependency projection bd's list JSON carried")
	}
	cache.mu.RLock()
	complete := cache.depsComplete
	cache.mu.RUnlock()
	if !complete {
		t.Fatal("cache depsComplete = false after priming from a store whose rows carry their edges")
	}
}

// bdReadyDisagreementHidden names the rows bd's own Ready hides in
// bdReadyDisagreementLedger, and the reason the cache's direct-dep predicate
// cannot reach the same verdict without the is_blocked column.
var bdReadyDisagreementHidden = map[string]string{
	"bd-child":     "blocked-ness propagates down parent-child and the cache's direct-dep predicate cannot see it",
	"bd-wisp-step": "blocked-ness propagates down parent-child and the cache's direct-dep predicate cannot see it",
	"bd-xstore":    "its blocker gcg-offscope is not resident in this scope's cache, and cachedBeadReady treats a dep as blocking only when statusByID holds the target",
}

// TestBdBackedCachedReadyNeverOffersWorkTheBackingHides is #5183/#5184's
// invariant on the BdStore path: a cached ready read may never return a bead the
// backing's own Ready() excludes.
//
// depsComplete=true hands readiness to cachedBeadReady, which answers from bd's
// is_blocked column when it is present and falls back to the bead's OWN direct
// blocks/waits-for/conditional-blocks deps when it is not. The fallback is
// strictly weaker in the two ways this fixture pins — it does not propagate down
// parent-child, and it ignores an edge whose target is not resident in the same
// scope — and either gap offers the control dispatcher a step whose gate has not
// opened (#3218). The column is what makes the two answers equal.
//
// The subtests below re-assert the invariant in the states a long-lived cache
// actually spends its life in, not just the freshly primed one (ga-cfhgr).
// `bd show --json` answers from types.IssueDetails and NOTHING in beads carries
// a `json:"is_blocked"` tag, so every routine refresh hands the cache a row with
// no verdict — and before ga-cfhgr each of these overwrote the column and left
// the weaker predicate answering, with no degrade latched.
func TestBdBackedCachedReadyNeverOffersWorkTheBackingHides(t *testing.T) {
	t.Run("after prime", func(t *testing.T) {
		ledger := bdReadyDisagreementLedger()
		cache, store := primedBdCache(t, ledger)
		stage := newReadyInvariantStage(t, cache, bdLiveReady(t, store, "bd-blocker", "bd-gate-open", "bd-unrelated"))

		stage.assertNeverExceedsBacking("after prime", bdReadyDisagreementHidden)
		stage.assertStillAnswers("after prime")
	})

	// The same invariant on the scope whose column comes from `bd blocked`
	// instead of `bd sql` — maintainer-city's shape, where the backend gate
	// withholds `bd sql` because gc does not implement the backend.
	//
	// Both halves are load-bearing and they pull in opposite directions.
	// assertNeverExceedsBacking would pass vacuously on a cache that declined
	// every readiness read, which is exactly today's behavior and exactly what
	// this door exists to stop; assertStillAnswers is what makes the subset
	// claim mean something. Red before the fix:
	//
	//	CachedReady declined (bd blocked door, after prime): a backing that can answer is_blocked must keep serving readiness from cache
	t.Run("bd blocked door", func(t *testing.T) {
		ledger := bdReadyDisagreementLedger()
		cache, store := primedBdCacheOnUnimplementedBackend(t, ledger)
		stage := newReadyInvariantStage(t, cache, bdLiveReady(t, store, "bd-blocker", "bd-gate-open", "bd-unrelated"))

		stage.assertNeverExceedsBacking("bd blocked door, after prime", bdReadyDisagreementHidden)
		stage.assertStillAnswers("bd blocked door, after prime")

		// The gate's whole reason for withholding `bd sql` on an unregistered
		// backend is that gc cannot assume that backend's SCHEMA. So the door
		// must not have been opened by accident.
		if calls := ledger.callsNamed("sql"); len(calls) != 0 {
			t.Fatalf("an unimplemented backend spent %v; the schema assumption behind `bd sql` is exactly what the gate refuses", calls)
		}
		if calls := ledger.callsNamed("blocked"); len(calls) == 0 {
			t.Fatal("no `bd blocked` was spent; the column cannot have come from bd")
		}

		// Absence means NOT BLOCKED, written rather than left nil. bd-gate-open
		// is the row that can tell the difference: its waits-for gate has
		// opened, so bd offers it, while the direct-dependency predicate a nil
		// verdict falls back to calls its open target a blocker and hides it.
		if blocked := cachedIsBlockedForTest(cache, "bd-gate-open"); blocked == nil || *blocked {
			t.Fatalf("cached is_blocked for bd-gate-open = %v, want false written explicitly: a nil verdict hands the row to the weaker predicate, which starves it", blocked)
		}
		for _, id := range []string{"bd-child", "bd-wisp-step", "bd-xstore"} {
			if blocked := cachedIsBlockedForTest(cache, id); blocked == nil || !*blocked {
				t.Fatalf("cached is_blocked for %s = %v, want bd's true", id, blocked)
			}
		}

		// One subprocess per prime, not one per bead: the door is a whole-scope
		// projection exactly as `bd sql` was.
		if calls := ledger.callsNamed("blocked"); len(calls) != 1 {
			t.Fatalf("prime spent %d `bd blocked` calls, want exactly 1 for the whole scope: %v", len(calls), calls)
		}
	})

	// A write-through refresh (CachingStore.Update -> backing.Get) reinstalls the
	// row from `bd show`, which cannot carry is_blocked.
	t.Run("after update", func(t *testing.T) {
		ledger := bdReadyDisagreementLedger()
		cache, store := primedBdCache(t, ledger)
		stage := newReadyInvariantStage(t, cache, bdLiveReady(t, store, "bd-blocker", "bd-gate-open", "bd-unrelated"))

		for _, id := range []string{"bd-child", "bd-xstore"} {
			assignee := "agent"
			if err := cache.Update(id, UpdateOpts{Assignee: &assignee}); err != nil {
				t.Fatalf("Update(%s): %v", id, err)
			}
			stage.assertNeverExceedsBacking("after Update("+id+")", bdReadyDisagreementHidden)
			// Mechanism, not just outcome: the refresh changed nothing the
			// verdict depends on, so the verdict is KEPT rather than dropped
			// and re-derived — which is why the cache can still answer below.
			if blocked := cachedIsBlockedForTest(cache, id); blocked == nil || !*blocked {
				t.Fatalf("cached is_blocked for %s after Update = %v, want the projection's true preserved across a refresh that cannot carry it", id, blocked)
			}
		}
		// The refresh changed nothing readiness depends on, so the cache must
		// still ANSWER: the whole point is that it stops taking the live read.
		stage.assertStillAnswers("after Update")
	})

	// The dirty-row overlay refreshes through the same projection-less `bd show`
	// before serving a cached read.
	t.Run("after dirty overlay ready", func(t *testing.T) {
		ledger := bdReadyDisagreementLedger()
		cache, store := primedBdCache(t, ledger)
		stage := newReadyInvariantStage(t, cache, bdLiveReady(t, store, "bd-blocker", "bd-gate-open", "bd-unrelated"))

		markDirtyForTest(cache, "bd-child")
		if _, err := cache.Ready(); err != nil {
			t.Fatalf("Ready over dirty overlay: %v", err)
		}
		stage.assertNeverExceedsBacking("after dirty-overlay Ready", bdReadyDisagreementHidden)
		stage.assertStillAnswers("after dirty-overlay Ready")
	})

	// A close invalidates its dependents' projection so the direct-dep predicate
	// recomputes — correct for a direct blocks edge, blind to the parent-child
	// propagation bd's column carries.
	t.Run("after close", func(t *testing.T) {
		ledger := bdReadyCloseInvalidationLedger()
		cache, store := primedBdCache(t, ledger)

		before := bdLiveReady(t, store, "bd-gate", "bd-x")
		newReadyInvariantStage(t, cache, before).assertStillAnswers("before close")

		if err := cache.Close("bd-x"); err != nil {
			t.Fatalf("Close(bd-x): %v", err)
		}
		stage := newReadyInvariantStage(t, cache, bdLiveReady(t, store, "bd-gate"))
		stage.assertNeverExceedsBacking("after Close(bd-x)", map[string]string{
			"bd-both": "its parent bd-parent is still blocked by the open bd-gate, and bd propagates that down the parent-child edge",
		})
		// This is the cost, stated: bd-both carries a parent-child edge, so its
		// verdict is one only the column can give. The cache declines rather
		// than guesses, and the caller takes bd's own answer.
		stage.assertDeclines("after Close(bd-x)", "bd-both")

		// And it is a WINDOW, not a latch: the next reconcile refills the
		// column from bd and the cache serves readiness again.
		cache.runReconciliation()
		stage.assertStillAnswers("after Close(bd-x) + reconcile")
	})
}

// bdLiveReady reads bd's own verdict and pins the fixture to it, so a fixture
// that stopped modeling bd's is_blocked filter fails here rather than silently
// weakening every assertion built on it.
func bdLiveReady(t *testing.T, store *BdStore, want ...string) []string {
	t.Helper()
	live, err := store.Ready(ReadyQuery{TierMode: TierBoth})
	if err != nil {
		t.Fatalf("bd Ready: %v", err)
	}
	got := sortedIDs(live)
	if !equalIDs(got, wantReadyIDs(want...)) {
		t.Fatalf("bd Ready = %v, want %v: the fixture must model bd's is_blocked filter", got, wantReadyIDs(want...))
	}
	return got
}

// bdReadyCloseInvalidationLedger is the close-path shape:
//
//	bd-gate (open) <-- blocks -- bd-parent <-- parent-child -- bd-both
//	bd-x (open)    <-- blocks -- bd-both
//
// Closing bd-x satisfies bd-both's only DIRECT blocking edge, so the cache's
// dependency-derived predicate calls it ready the moment
// clearDependentReadyProjectionsLocked drops its is_blocked. bd does not:
// bd-both's parent is still blocked by the open bd-gate, and blocked-ness
// propagates down parent-child. That makes this the one close whose verdict is
// unchanged for every other row — so the fixture's static is_blocked column
// stays honest across the mutation.
func bdReadyCloseInvalidationLedger() *fakeBdLedger {
	return &fakeBdLedger{
		depListRefusal: `exit status 1: Error: operation "IssueRelations" not supported by the postgres backend`,
		rows: []bdLedgerRow{
			{id: "bd-gate"},
			{id: "bd-x"},
			{id: "bd-parent", blocked: true, deps: []Dep{{IssueID: "bd-parent", DependsOnID: "bd-gate", Type: "blocks"}}},
			{id: "bd-both", blocked: true, deps: []Dep{
				{IssueID: "bd-both", DependsOnID: "bd-x", Type: "blocks"},
				{IssueID: "bd-both", DependsOnID: "bd-parent", Type: "parent-child"},
			}},
		},
	}
}

// TestBdDependencyCompletenessSpendsNoSubprocess is the cost half. The verdict
// runs on every cache prime and every reconcile of every BdStore scope, so it
// must be free: it is read off rows bd already returned.
//
// `bd dep list` is not merely expensive here, it is UNSUPPORTED — the fixture
// refuses it the way maintainer-city's bd/Postgres work store does (ga-7i7ts) —
// so a DepListBatch-backed verdict would have spent a guaranteed-failing ~4.4s
// subprocess per prime and per reconcile and still left depsComplete false.
func TestBdDependencyCompletenessSpendsNoSubprocess(t *testing.T) {
	ledger := bdReadyDisagreementLedger()
	cache, _ := primedBdCache(t, ledger)

	if deps := ledger.callsNamed("dep", "list"); len(deps) != 0 {
		t.Fatalf("prime spent %v; the completeness verdict must cost no subprocess", deps)
	}
	if _, err := cache.ReadyContext(context.Background()); err != nil {
		t.Fatalf("ReadyContext: %v", err)
	}
	if deps := ledger.callsNamed("dep", "list"); len(deps) != 0 {
		t.Fatalf("the cached ready read spent %v", deps)
	}
	if ready := ledger.callsNamed("ready"); len(ready) != 0 {
		t.Fatalf("the cached ready read fell back to a live %v", ready)
	}
}

// TestBdCachedDepListMatchesTheLiveDepList is what depsComplete promises beyond
// readiness: CachingStore.DepList stops delegating and serves "down" edges from
// the snapshot, so the snapshot's edges must BE bd's edges. This is the seam
// where a short inline projection would surface as silently missing edges.
func TestBdCachedDepListMatchesTheLiveDepList(t *testing.T) {
	ledger := bdReadyDisagreementLedger()
	ledger.depListRefusal = ""
	cache, store := primedBdCache(t, ledger)

	for _, id := range []string{"bd-blocker", "bd-parent", "bd-child", "bd-xstore", "bd-unrelated", "bd-wisp-step", "bd-wisp-gate"} {
		live, err := store.DepListBatch([]string{id})
		if err != nil {
			t.Fatalf("live DepListBatch(%s): %v", id, err)
		}
		cached, err := cache.DepList(id, "down")
		if err != nil {
			t.Fatalf("cached DepList(%s): %v", id, err)
		}
		if len(cached) != len(live[id]) {
			t.Fatalf("DepList(%s) from cache = %+v, want bd's %+v", id, cached, live[id])
		}
		for i := range cached {
			if cached[i] != live[id][i] {
				t.Fatalf("DepList(%s)[%d] from cache = %+v, want bd's %+v", id, i, cached[i], live[id][i])
			}
		}
	}
}

// TestBdRefusesCompletenessWhenBdContradictsItsOwnProjection is the falsifier.
//
// The claim is not "bd carries edges inline"; it is "the edges bd carried are
// all of them". bd hands back its own count of each row's blocking edges from
// the same query, so a row whose count exceeds the edges it carried is proof the
// projection is short — and one such row disqualifies the whole ledger, forever,
// because a cache that believed a short projection would read a blocked bead as
// ready.
// The two tiers arrive through different bd subcommands — `bd list` for issues,
// `bd query` for wisps — so each is checked with the other left whole, and
// neither call site can be dropped without a test noticing.
func TestBdRefusesCompletenessWhenBdContradictsItsOwnProjection(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  string
	}{
		{"issues tier", "bd-parent"},
		{"wisp tier", "bd-wisp-gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := bdReadyDisagreementLedger()
			ledger.truncateDeps = map[string]bool{tc.row: true}
			cache, store := primedBdCache(t, ledger)

			if store.listIncludesCompleteDependencies() {
				t.Fatalf("BdStore claimed complete dependencies though bd's own dependency_count for %s contradicts the edges it carried", tc.row)
			}
			cache.mu.RLock()
			complete := cache.depsComplete
			cache.mu.RUnlock()
			if complete {
				t.Fatal("cache depsComplete = true over a short inline projection")
			}
			if _, err := cache.ReadyContext(context.Background()); !errors.Is(err, ErrCacheUnavailable) {
				t.Fatalf("ReadyContext error = %v, want ErrCacheUnavailable: an unproven projection must decline", err)
			}

			// One contradicted page disqualifies the ledger for good: a later
			// clean listing is not evidence the earlier truncation was benign.
			ledger.truncateDeps = nil
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("re-Prime: %v", err)
			}
			if store.listIncludesCompleteDependencies() {
				t.Fatal("a later clean listing un-latched the contradiction")
			}
		})
	}
}

// TestBdWitnessesTheProjectionOnEitherTier pins that both tiers are observed.
// bd serves issues through `bd list` and wisps through `bd query`, and wisps are
// where molecule steps live — a ledger whose only edges are on the wisp tier
// must still be able to prove its projection, and vice versa.
func TestBdWitnessesTheProjectionOnEitherTier(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ephemeral bool
	}{
		{"issues tier", false},
		{"wisp tier", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &fakeBdLedger{rows: []bdLedgerRow{
				{id: "bd-target"},
				{
					id:        "bd-holder",
					ephemeral: tc.ephemeral,
					blocked:   true,
					deps:      []Dep{{IssueID: "bd-holder", DependsOnID: "bd-target", Type: "blocks"}},
				},
			}}
			_, store := primedBdCache(t, ledger)
			if !store.listIncludesCompleteDependencies() {
				t.Fatalf("the %s listing carried an edge bd counted, and it was not witnessed", tc.name)
			}
		})
	}
}

// TestBdRefusesCompletenessOnALedgerThatProvesNothing is the fail-safe control
// for the witness, and the reason the verdict is not simply "bd is new enough".
//
// An adapter that never populated the field would answer "no edges" for every
// bead, and a cache that believed it would flatten the whole topology into
// "everything is ready". Nothing short of a row that actually carried edges
// proves the projection is live, so a ledger with none — or a bd that will not
// say how many edges a row has — keeps the pre-fix behavior: correct, and
// merely slower.
func TestBdRefusesCompletenessOnALedgerThatProvesNothing(t *testing.T) {
	t.Run("no row carries an edge", func(t *testing.T) {
		ledger := &fakeBdLedger{rows: []bdLedgerRow{{id: "bd-1"}, {id: "bd-2"}}}
		cache, store := primedBdCache(t, ledger)
		if store.listIncludesCompleteDependencies() {
			t.Fatal("an edge-free listing is not evidence that this ledger projects edges")
		}
		if _, err := cache.ReadyContext(context.Background()); !errors.Is(err, ErrCacheUnavailable) {
			t.Fatalf("ReadyContext error = %v, want ErrCacheUnavailable", err)
		}
	})

	t.Run("bd omits dependency_count", func(t *testing.T) {
		ledger := bdReadyDisagreementLedger()
		ledger.omitDependencyCount = true
		_, store := primedBdCache(t, ledger)
		if store.listIncludesCompleteDependencies() {
			t.Fatal("a bd that will not count its own edges cannot vouch for the projection")
		}
	})
}

// TestBdWithoutTheBlockedColumnSendsReadyToTheLiveVerdict closes the hole
// depsComplete=true would otherwise open on an old bd.
//
// The is_blocked projection landed in bd 1.0.5. Before this bead the version
// gate answered (false, nil) — no error, so no degrade — on the reading that the
// absence "costs only the enrichment". That held only while depsComplete was
// hardcoded false. With the snapshot's own edges now serving readiness, a silent
// (false, nil) would hand every readiness handle to the dependency-derived
// predicate, which is exactly the #3218 regression. So the version gate now
// names the degrade and readiness declines the cache.
func TestBdWithoutTheBlockedColumnSendsReadyToTheLiveVerdict(t *testing.T) {
	ledger := bdReadyDisagreementLedger()
	oldBd := &fakeBdLedger{rows: ledger.rows, depListRefusal: ledger.depListRefusal}
	inner := oldBd.run
	runner := func(dir, name string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "version" {
			oldBd.calls = append(oldBd.calls, append([]string{name}, args...))
			return []byte("bd version 1.0.4\n"), nil
		}
		return inner(dir, name, args...)
	}

	store := NewBdStore(t.TempDir(), runner)
	cache := NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if !cache.readyReadsMustGoLive() {
		t.Error("a bd that cannot answer is_blocked must latch the ready-projection degrade")
	}
	rows, err := cache.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	want := wantReadyIDs("bd-blocker", "bd-gate-open", "bd-unrelated")
	if got := sortedIDs(rows); !equalIDs(got, want) {
		t.Fatalf("Ready = %v, want %v: the transitively blocked rows must not be offered", got, want)
	}
	if _, ok := cache.CachedReady(); ok {
		t.Error("CachedReady answered from a cache with no is_blocked column; the control dispatcher reads this handle")
	}
	if _, err := cache.ReadyContext(context.Background()); !errors.Is(err, ErrCacheUnavailable) {
		t.Errorf("ReadyContext error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.Handles().Cached.Ready(); !errors.Is(err, ErrCacheUnavailable) {
		t.Errorf("cached reader Ready error = %v, want ErrCacheUnavailable", err)
	}

	// The rows are whole, so everything that does not need the column keeps
	// serving from cache — the separation #5183 established.
	cached, ok := cache.CachedList(ListQuery{AllowScan: true, TierMode: TierBoth})
	if !ok {
		t.Fatal("CachedList declined: the degrade must not make non-readiness reads unavailable")
	}
	// Every row bd hands THIS scope, which is every row but the relocated one:
	// gc routes `gcg-` beads to another store, so `bd list`/`bd query` never
	// return them here.
	resident := 0
	for _, row := range oldBd.rows {
		if !row.offscope {
			resident++
		}
	}
	if len(cached) != resident {
		t.Fatalf("CachedList returned %d rows, want %d", len(cached), resident)
	}
}
