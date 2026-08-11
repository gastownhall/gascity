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
	deps      []Dep
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
		return f.renderRows(func(r bdLedgerRow) bool { return !r.ephemeral })
	case args[0] == "query":
		return f.renderRows(func(r bdLedgerRow) bool { return r.ephemeral })
	case args[0] == "ready":
		// bd ready filters is_blocked = 0 (sqlbuild.ReadyWhere). This is the
		// backing's own verdict, the one the cache may never exceed.
		return f.renderRows(func(r bdLedgerRow) bool { return !r.blocked })
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

func (f *fakeBdLedger) renderRows(keep func(bdLedgerRow) bool) ([]byte, error) {
	out := make([]map[string]any, 0, len(f.rows))
	for _, row := range f.rows {
		if !keep(row) {
			continue
		}
		issue := map[string]any{
			"id":         row.id,
			"title":      row.id,
			"status":     "open",
			"issue_type": "task",
		}
		if row.ephemeral {
			issue["ephemeral"] = true
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
		out = append(out, map[string]any{"id": row.id, "is_blocked": row.blocked})
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
//	gcg-offscope (not in this scope) <-- blocks -- xstore
//	parent <-- parent-child -- wisp-step   (ephemeral)
//	unrelated <-- blocks -- wisp-gate      (ephemeral)
//
// child and wisp-step carry no blocking dep of their own, so the cache's
// dependency-derived predicate calls them ready while bd marks them
// is_blocked=1 through the parent-child edge. xstore carries one, but its target
// is not resident in this scope's cache, and cachedBeadReady treats a dep as
// blocking only when statusByID holds the target — so that edge is invisible to
// it too. wisp-gate is the wisp tier's blocking edge, so the tier bd serves
// through `bd query` rather than `bd list` also has a row that can prove — and
// disprove — the projection. unrelated is the control both models call ready.
func bdReadyDisagreementLedger() *fakeBdLedger {
	return &fakeBdLedger{
		depListRefusal: `exit status 1: Error: operation "IssueRelations" not supported by the postgres backend`,
		rows: []bdLedgerRow{
			{id: "bd-blocker"},
			{id: "bd-parent", blocked: true, deps: []Dep{{IssueID: "bd-parent", DependsOnID: "bd-blocker", Type: "blocks"}}},
			{id: "bd-child", blocked: true, deps: []Dep{{IssueID: "bd-child", DependsOnID: "bd-parent", Type: "parent-child"}}},
			{id: "bd-xstore", blocked: true, deps: []Dep{{IssueID: "bd-xstore", DependsOnID: "gcg-offscope", Type: "blocks"}}},
			{id: "bd-unrelated"},
			{id: "bd-wisp-step", ephemeral: true, blocked: true, deps: []Dep{{IssueID: "bd-wisp-step", DependsOnID: "bd-parent", Type: "parent-child"}}},
			{id: "bd-wisp-gate", ephemeral: true, blocked: true, deps: []Dep{{IssueID: "bd-wisp-gate", DependsOnID: "bd-unrelated", Type: "blocks"}}},
		},
	}
}

func primedBdCache(t *testing.T, ledger *fakeBdLedger) (*CachingStore, *BdStore) {
	t.Helper()
	store := NewBdStore(t.TempDir(), ledger.run)
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
	want := wantReadyIDs("bd-blocker", "bd-unrelated")
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
func TestBdBackedCachedReadyNeverOffersWorkTheBackingHides(t *testing.T) {
	ledger := bdReadyDisagreementLedger()
	cache, store := primedBdCache(t, ledger)

	live, err := store.Ready(ReadyQuery{TierMode: TierBoth})
	if err != nil {
		t.Fatalf("bd Ready: %v", err)
	}
	want := sortedIDs(live)
	if got := wantReadyIDs("bd-blocker", "bd-unrelated"); !equalIDs(want, got) {
		t.Fatalf("bd Ready = %v, want %v: the fixture must model bd's is_blocked filter", want, got)
	}

	readers := []struct {
		name string
		read func() ([]Bead, error)
	}{
		{"Ready", func() ([]Bead, error) { return cache.Ready() }},
		{"ReadyContext", func() ([]Bead, error) { return cache.ReadyContext(context.Background()) }},
		{"CachedReady", func() ([]Bead, error) {
			rows, ok := cache.CachedReady()
			if !ok {
				return nil, ErrCacheUnavailable
			}
			return rows, nil
		}},
		{"Handles().Cached.Ready", func() ([]Bead, error) { return cache.Handles().Cached.Ready() }},
	}
	for _, reader := range readers {
		rows, err := reader.read()
		if err != nil {
			// Declining is a correct answer: the caller then takes the live
			// backing verdict. Answering with MORE than the backing is not.
			if errors.Is(err, ErrCacheUnavailable) {
				continue
			}
			t.Fatalf("%s: %v", reader.name, err)
		}
		got := sortedIDs(rows)
		for _, hidden := range []string{"bd-child", "bd-wisp-step"} {
			if containsID(got, hidden) {
				t.Errorf("%s offered %s, a child of a blocked parent that bd's own ready hides: "+
					"blocked-ness propagates down parent-child and the cache's direct-dep predicate cannot see it (backing = %v, cache = %v)",
					reader.name, hidden, want, got)
			}
		}
		if containsID(got, "bd-xstore") {
			t.Errorf("%s offered bd-xstore, whose blocker gcg-offscope is not resident in this scope's cache: "+
				"cachedBeadReady treats a dep as blocking only when statusByID holds the target (backing = %v, cache = %v)",
				reader.name, want, got)
		}
		if extra := idsBeyond(got, want); len(extra) > 0 {
			t.Errorf("%s returned %v beyond bd's own ready (backing = %v)", reader.name, extra, want)
		}
	}

	// Declining is the fail-safe, not the fix. A store that only ever declined
	// would satisfy the subset checks above while costing maintainer-city the
	// live read this bead exists to remove.
	cached, ok := cache.CachedReady()
	if !ok {
		t.Fatal("CachedReady declined: a bd that can answer is_blocked must keep serving readiness from cache")
	}
	if got := sortedIDs(cached); !equalIDs(got, want) {
		t.Fatalf("CachedReady = %v, want %v (bd's own verdict)", got, want)
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
	want := wantReadyIDs("bd-blocker", "bd-unrelated")
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
	if len(cached) != len(oldBd.rows) {
		t.Fatalf("CachedList returned %d rows, want %d", len(cached), len(oldBd.rows))
	}
}
