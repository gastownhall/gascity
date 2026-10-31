package readywork

// Cross-store blockers on real split fixtures: a real SQLite binding (the store
// a split city's graph class is served from), a real bd/Dolt work store (the
// linked beads library over embedded Dolt), and the in-memory leaf the migration
// clear leaves kept edges in. The two real backends answer a blocker they do not
// hold in OPPOSITE ways, and each test pins the owner rule over both: a blocker
// held in the other store blocks until it is closed there; one no store holds
// blocks its row and never fails the read.

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// sqliteBinding opens the class binding a split city serves its graph class
// from: a real SQLite store minting gcg-.
func sqliteBinding(t *testing.T) beads.Store {
	t.Helper()
	store, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening the SQLite binding: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore()
		}
	})
	return store
}

func splitLegs(work, binding beads.Store) []Leg {
	return []Leg{
		testLeg("work", work),
		{Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}},
	}
}

func closeBead(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if err := store.Close(id); err != nil {
		t.Fatalf("closing %s: %v", id, err)
	}
}

func readyIDs(t *testing.T, legs []Leg, q Query) []string {
	t.Helper()
	rows, err := Read(legs, q)
	if err != nil {
		t.Fatalf("Read(%+v): %v", q, err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func setInProgress(t *testing.T, store beads.Store, id, assignee string) {
	t.Helper()
	status := "in_progress"
	if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("marking %s in progress: %v", id, err)
	}
}

// TestReadInProgressUnresolvableBlockerBlocksItsRowNotTheTier is R8: one work
// row whose blocking edge names an id no store holds (an edge the migration
// clear kept into an id since purged from the binding) used to fail the WHOLE
// crash-recovery tier, so a seat could not resume any of its work. Now that row
// is served as gated, with a reason, and every clean row is served beside it.
func TestReadInProgressUnresolvableBlockerBlocksItsRowNotTheTier(t *testing.T) {
	work := beads.NewMemStore() // a raw leaf: the edge was kept by the clear, not added through a strict door
	mine := mustCreate(t, work, beads.Bead{Title: "mine", Type: "task"})
	other := mustCreate(t, work, beads.Bead{Title: "other", Type: "task"})
	setInProgress(t, work, mine.ID, "worker-1")
	setInProgress(t, work, other.ID, "worker-1")
	if err := work.DepAdd(other.ID, "gcg-7", "blocks"); err != nil {
		t.Fatal(err)
	}
	legs := splitLegs(work, sqliteBinding(t))

	for _, limit := range []int{0, 1, 2} {
		rows, err := Read(legs, Query{Status: StatusInProgress, Assignee: "worker-1", Limit: limit})
		if err != nil {
			t.Fatalf("limit=%d: the in-progress tier failed outright because %s has a blocker no store holds: %v", limit, other.ID, err)
		}
		want := 2
		if limit == 1 {
			want = 1
		}
		if len(rows) != want {
			t.Fatalf("limit=%d: served %d rows, want %d: %+v", limit, len(rows), want, rows)
		}
		for _, row := range rows {
			switch row.ID {
			case mine.ID:
				if len(row.BlockedBy) != 0 {
					t.Errorf("limit=%d: clean row %s carries blockers %+v", limit, mine.ID, row.BlockedBy)
				}
			case other.ID:
				if len(row.BlockedBy) != 1 {
					t.Fatalf("limit=%d: %s blocked_by = %+v, want the one unresolvable blocker", limit, other.ID, row.BlockedBy)
				}
				got := row.BlockedBy[0]
				if got.ID != "gcg-7" || got.Status != BlockerUnresolved || !strings.Contains(got.Reason, "no store") {
					t.Errorf("limit=%d: blocker = %+v, want gcg-7 %q with a reason naming that no store holds it", limit, got, BlockerUnresolved)
				}
			}
		}
	}
}

// TestReadInProgressCrossStoreBlockerReportsTheStatusItsHomeStoreHolds pins
// the resumed holder's view of a blocker in the other store: the status the
// binding holds, not a failure and not an absence.
func TestReadInProgressCrossStoreBlockerReportsTheStatusItsHomeStoreHolds(t *testing.T) {
	work := beads.NewMemStore()
	binding := sqliteBinding(t)
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task"})
	row := mustCreate(t, work, beads.Bead{Title: "work", Type: "task"})
	setInProgress(t, work, row.ID, "worker-1")
	if err := work.DepAdd(row.ID, step.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	legs := splitLegs(work, binding)

	blockerOf := func() ReadyBeadBlocker {
		t.Helper()
		rows, err := Read(legs, Query{Status: StatusInProgress, Assignee: "worker-1"})
		if err != nil || len(rows) != 1 || len(rows[0].BlockedBy) != 1 {
			t.Fatalf("Read = %+v, %v; want %s with its one blocker", rows, err, row.ID)
		}
		return rows[0].BlockedBy[0]
	}
	if got := blockerOf(); got.ID != step.ID || got.Status != "open" || !strings.Contains(got.Reason, "graph") {
		t.Fatalf("open cross-store blocker = %+v, want %s open, with a reason naming the graph store", got, step.ID)
	}
	closeBead(t, binding, step.ID)
	if got := blockerOf(); got.Status != "closed" {
		t.Fatalf("closed cross-store blocker = %+v, want status closed", got)
	}
}

// TestReadServesAStrandedSQLiteRowOnceItsWorkBlockerCloses is the stranded half
// of R9 on the real SQLite binding: a graph row whose blocker lives in the work
// store is withheld by the binding's own Ready FOREVER (the LEFT JOIN reads the
// absent blocker as never closed). gc's reader serves it the moment the blocker
// closes in the work store, and not before.
func TestReadServesAStrandedSQLiteRowOnceItsWorkBlockerCloses(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	binding := sqliteBinding(t)
	blocker := mustCreate(t, work, beads.Bead{Title: "work blocker", Type: "task"})
	item := mustCreate(t, binding, beads.Bead{Title: "graph item", Type: "task"})
	if err := binding.DepAdd(item.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("writing the cross-store edge into the binding: %v", err)
	}
	legs := splitLegs(work, binding)

	if got := readyIDs(t, legs, Query{}); slices.Contains(got, item.ID) {
		t.Fatalf("ready = %v served %s while its work blocker %s is open", got, item.ID, blocker.ID)
	}
	closeBead(t, work, blocker.ID)
	own, err := binding.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(own, func(b beads.Bead) bool { return b.ID == item.ID }) {
		t.Fatalf("fixture: the SQLite binding's own Ready served %s; it is meant to strand it behind the absent blocker", item.ID)
	}
	for _, q := range []Query{{}, {Limit: 1}, {Limit: 5, Sort: SortOldest}} {
		if got := readyIDs(t, legs, q); !slices.Contains(got, item.ID) {
			t.Errorf("ready(%+v) = %v; %s stays stranded although its blocker %s closed in the work store", q, got, item.ID, blocker.ID)
		}
	}
}

// TestReadWithholdsAPrematureBdRowUntilItsGraphBlockerCloses is the premature
// half of R9 on a real bd/Dolt work store: bd classifies a blocker it does not
// hold as external and serves the dependent at once. gc's reader withholds it
// until the blocker closes in the binding, and withholds one whose blocker no
// store holds at all.
func TestReadWithholdsAPrematureBdRowUntilItsGraphBlockerCloses(t *testing.T) {
	work := splittest.NewNativeWorkStore(t, "gc")
	binding := sqliteBinding(t)
	// The graph step is ready work of its own; ranking it last keeps it out
	// of the bounded answers below, which are about the work rows.
	first, last := 0, 4
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task", Priority: &last})
	gated := mustCreate(t, work, beads.Bead{Title: "gated", Type: "task", Priority: &first})
	dangling := mustCreate(t, work, beads.Bead{Title: "dangling", Type: "task", Priority: &first})
	clean := mustCreate(t, work, beads.Bead{Title: "clean", Type: "task", Priority: &first})
	// bd's dependency door accepts a target it does not hold (only the bd CLI
	// store refuses a cross-prefix edge), which is how a kept or create-time
	// edge reaches a real work ledger.
	if err := work.DepAdd(gated.ID, step.ID, "blocks"); err != nil {
		t.Fatalf("writing the cross-store edge into the bd store: %v", err)
	}
	if err := work.DepAdd(dangling.ID, "gcg-404", "blocks"); err != nil {
		t.Fatalf("writing the dangling edge into the bd store: %v", err)
	}

	own, err := work.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	var ownIDs []string
	for _, b := range own {
		ownIDs = append(ownIDs, b.ID)
	}
	if !slices.Contains(ownIDs, gated.ID) {
		t.Skipf("fixture: this bd serves no external-blocker row as ready (own ready = %v); the premature case does not arise", ownIDs)
	}

	legs := splitLegs(work, binding)
	for _, q := range []Query{{}, {Limit: 1}, {Limit: 3, Sort: SortOldest}} {
		got := readyIDs(t, legs, q)
		if slices.Contains(got, gated.ID) {
			t.Errorf("ready(%+v) = %v served %s while its graph blocker %s is open", q, got, gated.ID, step.ID)
		}
		if slices.Contains(got, dangling.ID) {
			t.Errorf("ready(%+v) = %v served %s, whose blocker no store holds", q, got, dangling.ID)
		}
		if !slices.Contains(got, clean.ID) {
			t.Errorf("ready(%+v) = %v dropped the clean row %s", q, got, clean.ID)
		}
	}
	closeBead(t, binding, step.ID)
	if got := readyIDs(t, legs, Query{}); !slices.Contains(got, gated.ID) {
		t.Errorf("ready = %v; %s is still withheld after its graph blocker %s closed", got, gated.ID, step.ID)
	}
}

// TestReadBoundedAnswerRefillsPastAWithheldRow pins the bound: when the gate
// withholds the rows a bounded read cut at, the answer is still the top N of
// what may be served, not a short one.
func TestReadBoundedAnswerRefillsPastAWithheldRow(t *testing.T) {
	work := beads.NewMemStore()
	binding := sqliteBinding(t)
	high, low, last := 0, 3, 4
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task", Priority: &last})
	for i := 0; i < boundedSlack+3; i++ {
		b := mustCreate(t, work, beads.Bead{Title: "gated", Type: "task", Priority: &high})
		if err := work.DepAdd(b.ID, step.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	// The MemStore leaf is the stranding backend, so its own Ready withholds
	// these; make them premature instead, as bd would, by serving them through
	// a leg that ignores the edge.
	served := mustCreate(t, work, beads.Bead{Title: "servable", Type: "task", Priority: &low})
	legs := []Leg{testLeg("work", externalBlockerStore{Store: work}), {Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}}}
	got := readyIDs(t, legs, Query{Limit: 1})
	if !slices.Equal(got, []string{served.ID}) {
		t.Fatalf("ready(limit=1) = %v, want [%s]: every higher-priority row is gated by the open graph step %s", got, served.ID, step.ID)
	}
}

// externalBlockerStore serves a MemStore's ready set the way bd/Dolt does: a
// blocker the store does not hold never blocks. It embeds the Store interface,
// not the MemStore, so it does not claim the MemStore's stranding listing
// (beads.AbsentBlockedReader): bd strands nothing.
type externalBlockerStore struct{ beads.Store }

func (s externalBlockerStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	q := beads.ReadyQuery{}
	if len(query) > 0 {
		q = query[0]
	}
	all, err := s.List(beads.ListQuery{Status: "open", TierMode: q.TierMode})
	if err != nil {
		return nil, err
	}
	var out []beads.Bead
	for _, b := range all {
		blocked := false
		deps, err := s.DepList(b.ID, "down")
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if !beads.IsReadyBlockingDependencyType(d.Type) {
				continue
			}
			target, err := s.Get(d.DependsOnID)
			if err != nil {
				continue // external: not held here, so not blocking
			}
			if target.Status != "closed" {
				blocked = true
			}
		}
		if !blocked && beads.IsReadyCandidateForTier(b, time.Now().UTC(), q.TierMode) {
			out = append(out, b)
		}
	}
	return out, nil
}

// ReadyOrdered cuts Ready in the canonical order, so the reader's bounded read
// is really bounded and the gate can withhold every row it cut at.
func (s externalBlockerStore) ReadyOrdered(query beads.ReadyQuery, _ beads.ReadyOrder) ([]beads.Bead, bool, error) {
	rows, err := s.Ready(query)
	if err != nil {
		return nil, false, err
	}
	prio := func(b beads.Bead) int {
		if b.Priority == nil {
			return 2
		}
		return *b.Priority
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if prio(rows[i]) != prio(rows[j]) {
			return prio(rows[i]) < prio(rows[j])
		}
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.Before(rows[j].CreatedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	if query.Limit > 0 && len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}
	return rows, true, nil
}

// TestReadKeepsBdReadySemanticsOnASingleStoreCityWithRigs is the owner
// decision's compatibility half: a city with no class binding, with or without
// rigs, keeps bd's ready answer byte for byte. bd serves a row whose blocking
// edge names an external:<id>, an external capability or another rig's bead,
// and so does gc ready — in bd's own order, row for row.
func TestReadKeepsBdReadySemanticsOnASingleStoreCityWithRigs(t *testing.T) {
	city := splittest.NewNativeWorkStore(t, "gc")
	rig := splittest.NewNativeWorkStore(t, "rg")
	rigBlocker := mustCreate(t, rig, beads.Bead{Title: "rig blocker", Type: "task"})
	capDep := mustCreate(t, city, beads.Bead{Title: "capability dep", Type: "task"})
	extID := mustCreate(t, city, beads.Bead{Title: "external id dep", Type: "task"})
	crossRig := mustCreate(t, city, beads.Bead{Title: "cross-rig dep", Type: "task"})
	dangling := mustCreate(t, city, beads.Bead{Title: "dangling dep", Type: "task"})
	mustCreate(t, city, beads.Bead{Title: "clean", Type: "task"})
	for _, e := range [][2]string{
		{capDep.ID, "external:rg:some-capability"},
		{extID.ID, "external:" + rigBlocker.ID},
		{crossRig.ID, rigBlocker.ID},
		{dangling.ID, "discovered-from:gc-404"},
	} {
		if err := city.DepAdd(e[0], e[1], "blocks"); err != nil {
			t.Fatalf("DepAdd(%s -> %s): %v", e[0], e[1], err)
		}
	}
	legs := []Leg{testLeg("city", city), testLeg("rig:rg", rig)}
	if IsSplit(legs) {
		t.Fatal("fixture: a city with no class binding reads as split")
	}
	var want []string
	for _, leg := range legs {
		own, err := leg.Store.Ready(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range own {
			want = append(want, b.ID)
		}
	}
	for _, id := range []string{capDep.ID, extID.ID, crossRig.ID, dangling.ID} {
		if !slices.Contains(want, id) {
			t.Skipf("fixture: this bd does not serve %s itself (own ready %v); nothing to keep", id, want)
		}
	}
	got := readyIDs(t, legs, Query{})
	if !sameSet(got, want) {
		t.Fatalf("single-store gc ready = %v, want bd's own answer %v", got, want)
	}
	if limited := readyIDs(t, legs, Query{Limit: 2}); len(limited) != 2 {
		t.Fatalf("single-store gc ready(limit=2) = %v, want two of bd's rows", limited)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := slices.Clone(a)
	y := slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// TestReadOnASplitCityResolvesExternalRefsAndCrossRigBlockers pins the
// external: half of the owner decision on a split city with a real bd/Dolt
// work store and rig: an external:<id> edge names the real bead and holds its
// row until that bead closes; an external capability never blocks, as in bd; a
// cross-rig blocker holds its row until it closes in the rig.
func TestReadOnASplitCityResolvesExternalRefsAndCrossRigBlockers(t *testing.T) {
	work := splittest.NewNativeWorkStore(t, "gc")
	rig := splittest.NewNativeWorkStore(t, "rg")
	binding := sqliteBinding(t)
	last := 4
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task", Priority: &last})
	rigBlocker := mustCreate(t, rig, beads.Bead{Title: "rig blocker", Type: "task", Priority: &last})
	extRow := mustCreate(t, work, beads.Bead{Title: "external id dep", Type: "task"})
	capRow := mustCreate(t, work, beads.Bead{Title: "capability dep", Type: "task"})
	rigRow := mustCreate(t, work, beads.Bead{Title: "cross-rig dep", Type: "task"})
	for _, e := range [][2]string{
		{extRow.ID, "external:" + step.ID},
		{capRow.ID, "external:rg:some-capability"},
		{rigRow.ID, rigBlocker.ID},
	} {
		if err := work.DepAdd(e[0], e[1], "blocks"); err != nil {
			t.Fatalf("DepAdd(%s -> %s): %v", e[0], e[1], err)
		}
	}
	legs := []Leg{testLeg("work", work), testLeg("rig:rg", rig), {Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}}}
	got := readyIDs(t, legs, Query{})
	if slices.Contains(got, extRow.ID) || slices.Contains(got, rigRow.ID) {
		t.Fatalf("ready = %v served %s or %s while its blocker is open", got, extRow.ID, rigRow.ID)
	}
	if !slices.Contains(got, capRow.ID) {
		t.Fatalf("ready = %v withholds %s, whose only blocker is an external capability bd never blocks on", got, capRow.ID)
	}
	closeBead(t, binding, step.ID)
	closeBead(t, rig, rigBlocker.ID)
	got = readyIDs(t, legs, Query{})
	if !slices.Contains(got, extRow.ID) || !slices.Contains(got, rigRow.ID) {
		t.Fatalf("ready = %v after both blockers closed, want %s and %s served", got, extRow.ID, rigRow.ID)
	}
}

// TestReadHoldsARowUntilItsBlockerReleasesInEveryStore pins the two halves of
// "closed in every store": a blocker one store holds closed and another holds
// open blocks; a blocker closed with a failed work outcome
// (beads.DependencySatisfied) blocks.
func TestReadHoldsARowUntilItsBlockerReleasesInEveryStore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, work, binding beads.Store) (row beads.Bead)
	}{
		{"a second copy is still open", func(t *testing.T, work, binding beads.Store) beads.Bead {
			// The same id held twice: closed in the binding, open in a rig
			// store (an uncleared relic's original).
			rig := beads.NewMemStore()
			rig.HonorExplicitIDs = true
			bl := mustCreate(t, binding, beads.Bead{Title: "relic", Type: "task", Priority: intRef(4)})
			closeBead(t, binding, bl.ID)
			if _, err := rig.Create(beads.Bead{ID: bl.ID, Title: "relic original", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			row := mustCreate(t, work, beads.Bead{Title: "row", Type: "task"})
			if err := work.DepAdd(row.ID, "external:"+bl.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			extraLegs = []Leg{testLeg("rig:r", rig)}
			return row
		}},
		{"closed with a failed outcome", func(t *testing.T, work, binding beads.Store) beads.Bead {
			bl := mustCreate(t, binding, beads.Bead{Title: "failed step", Type: "task", Priority: intRef(4)})
			if err := binding.SetMetadata(bl.ID, "gc.work_outcome", "blocked"); err != nil {
				t.Fatal(err)
			}
			closeBead(t, binding, bl.ID)
			row := mustCreate(t, work, beads.Bead{Title: "row", Type: "task"})
			if err := work.DepAdd(row.ID, "external:"+bl.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			extraLegs = nil
			return row
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := beads.NewMemStore()
			binding := sqliteBinding(t)
			row := tc.setup(t, work, binding)
			legs := append([]Leg{testLeg("work", externalBlockerStore{Store: work})}, extraLegs...)
			legs = append(legs, Leg{Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}})
			if got := readyIDs(t, legs, Query{}); slices.Contains(got, row.ID) {
				t.Fatalf("ready = %v served %s; its blocker has not released in every store", got, row.ID)
			}
		})
	}
}

// extraLegs carries a case's additional legs into its run (set by setup).
var extraLegs []Leg

// countingBdLeg counts the round trips a bd-semantics work leg is asked for.
type countingBdLeg struct {
	externalBlockerStore
	gets, lists, depLists atomic.Int64
}

func (s *countingBdLeg) Get(id string) (beads.Bead, error) {
	s.gets.Add(1)
	return s.externalBlockerStore.Get(id)
}

func (s *countingBdLeg) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists.Add(1)
	return s.externalBlockerStore.List(q)
}

func (s *countingBdLeg) DepList(id, d string) ([]beads.Dep, error) {
	s.depLists.Add(1)
	return s.externalBlockerStore.DepList(id, d)
}

func (s *countingBdLeg) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	s.depLists.Add(1)
	out := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		deps, err := s.externalBlockerStore.DepList(id, "down")
		if err != nil {
			return nil, err
		}
		out[id] = deps
	}
	return out, nil
}

// TestReadCrossStoreCostOnASplitCity bounds what the cross-store rule costs a
// claim-tier read (limit 1) on a split city whose bd work leg serves rows with
// ordinary closed same-store blockers: one batched edge read and no blocker
// read at all on the work leg (bd judged same-prefix blockers itself; the
// binding is asked only whether it holds them, in one local batch).
func TestReadCrossStoreCostOnASplitCity(t *testing.T) {
	mem := beads.NewMemStore()
	work := &countingBdLeg{externalBlockerStore: externalBlockerStore{Store: mem}}
	for i := 0; i < 50; i++ {
		b1 := mustCreate(t, mem, beads.Bead{Title: "done", Type: "task"})
		b2 := mustCreate(t, mem, beads.Bead{Title: "done2", Type: "task"})
		closeBead(t, mem, b1.ID)
		closeBead(t, mem, b2.ID)
		r := mustCreate(t, mem, beads.Bead{Title: "ready", Type: "task"})
		if err := mem.DepAdd(r.ID, b1.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
		if err := mem.DepAdd(r.ID, b2.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	legs := []Leg{testLeg("work", work), {Label: "graph", Store: sqliteBinding(t), ClassBinding: true, ReservedPrefixes: []string{"gcg"}}}
	work.lists.Store(0)
	rows, err := Read(legs, Query{Limit: 1})
	if err != nil || len(rows) != 1 {
		t.Fatalf("Read(limit=1) = %v, %v", rows, err)
	}
	t.Logf("limit=1 on the work leg: Gets=%d Lists=%d edge reads=%d", work.gets.Load(), work.lists.Load(), work.depLists.Load())
	if work.gets.Load() != 0 {
		t.Errorf("the work leg was asked for %d single blocker read(s), want 0", work.gets.Load())
	}
	if work.depLists.Load() > 1 {
		t.Errorf("the work leg was asked for %d edge read(s), want at most 1 batched read", work.depLists.Load())
	}
	// Ready itself lists the leg; any further list is a blocker batch read,
	// and bd already judged these blockers.
	if work.lists.Load() > 1 {
		t.Errorf("the work leg was asked for %d list(s), want only Ready's own", work.lists.Load())
	}
}

// TestReadInProgressKeepsBdSemanticsOnASingleStoreCity: on a city with no
// class binding the crash-recovery tier's blocked_by is bd's answer. bd never
// blocks on an external: target, a capability, or a cross-rig id the serving
// store does not hold, so those rows carry no blocker; a same-store open
// blocker still reads as one (the control).
func TestReadInProgressKeepsBdSemanticsOnASingleStoreCity(t *testing.T) {
	city := splittest.NewNativeWorkStore(t, "gc")
	rig := splittest.NewNativeWorkStore(t, "rg")
	rb := mustCreate(t, rig, beads.Bead{Title: "rig blocker", Type: "task"})
	own := mustCreate(t, city, beads.Bead{Title: "own open blocker", Type: "task"})
	mk := func(title, target string) string {
		t.Helper()
		b := mustCreate(t, city, beads.Bead{Title: title, Type: "task"})
		setInProgress(t, city, b.ID, "w1")
		if target != "" {
			if err := city.DepAdd(b.ID, target, "blocks"); err != nil {
				t.Fatal(err)
			}
		}
		return b.ID
	}
	external := []string{mk("ext id", "external:"+rb.ID), mk("cross rig", rb.ID), mk("cap", "external:rg:cap")}
	held := mk("own blocker", own.ID)
	legs := []Leg{testLeg("city", city), testLeg("rig:rg", rig)}
	rows, err := Read(legs, Query{Status: StatusInProgress, Assignee: "w1"})
	if err != nil {
		t.Fatalf("in-progress tier: %v", err)
	}
	byID := map[string]ReadyBead{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	for _, id := range external {
		if r, ok := byID[id]; !ok || len(r.BlockedBy) != 0 {
			t.Errorf("row %s blocked_by = %+v (served %v), want served with no blocker: bd never blocks on it", id, r.BlockedBy, ok)
		}
	}
	if r := byID[held]; len(r.BlockedBy) != 1 || r.BlockedBy[0].Status != "open" {
		t.Errorf("row %s blocked_by = %+v, want its same-store open blocker", held, r.BlockedBy)
	}
}

// TestReadHoldsARowWhoseLaterCopyIsStillOpen pins every copy, not the first:
// the blocker's first copy in leg order is closed and a later one is open.
func TestReadHoldsARowWhoseLaterCopyIsStillOpen(t *testing.T) {
	work := beads.NewMemStore()
	rig := beads.NewMemStore()
	rig.HonorExplicitIDs = true
	binding := sqliteBinding(t)
	bl := mustCreate(t, binding, beads.Bead{Title: "relic", Type: "task", Priority: intRef(4)})
	if _, err := rig.Create(beads.Bead{ID: bl.ID, Title: "closed copy", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	closeBead(t, rig, bl.ID)
	row := mustCreate(t, work, beads.Bead{Title: "row", Type: "task"})
	if err := work.DepAdd(row.ID, "external:"+bl.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	legs := []Leg{
		testLeg("work", externalBlockerStore{Store: work}),
		testLeg("rig:r", rig), // the closed copy, first in leg order
		{Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}}, // the open copy
	}
	if got := readyIDs(t, legs, Query{}); slices.Contains(got, row.ID) {
		t.Fatalf("ready = %v served %s; its blocker is still open in the graph store", got, row.ID)
	}
}

// TestReadHoldsABdRowOnAMigratedRelicTheBindingHoldsOpen: a bd row's
// same-prefix blocker that the work store no longer holds — a relic the
// migration moved into the binding — is read by bd as a missing same-prefix
// target and never blocks. gc's reader asks the binding and holds the row.
func TestReadHoldsABdRowOnAMigratedRelicTheBindingHoldsOpen(t *testing.T) {
	work := beads.NewMemStore()
	binding := sqliteBinding(t)
	relic, err := binding.(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{ID: "gc-relic-1", Title: "relic step", Type: "task", Priority: intRef(4)})
	if err != nil {
		t.Fatal(err)
	}
	row := mustCreate(t, work, beads.Bead{Title: "row", Type: "task"})
	if beads.BdIDPrefix(row.ID) != beads.BdIDPrefix(relic.ID) {
		t.Fatalf("fixture: %s and %s do not share bd's prefix", row.ID, relic.ID)
	}
	if err := work.DepAdd(row.ID, relic.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	legs := []Leg{testLeg("work", externalBlockerStore{Store: work}), {Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}}}
	if got := readyIDs(t, legs, Query{}); slices.Contains(got, row.ID) {
		t.Fatalf("ready = %v served %s while its relic blocker %s is open in the binding", got, row.ID, relic.ID)
	}
	closeBead(t, binding, relic.ID)
	if got := readyIDs(t, legs, Query{}); !slices.Contains(got, row.ID) {
		t.Fatalf("ready = %v withholds %s after its relic blocker closed", got, row.ID)
	}
}

// TestReadInProgressOmitsAnExternalRefToTheServingStoresOwnBead: bd treats an
// "external:" target as external even when it names a bead the serving store
// holds, and never blocks on it; on a city with no binding gc does the same.
func TestReadInProgressOmitsAnExternalRefToTheServingStoresOwnBead(t *testing.T) {
	city := splittest.NewNativeWorkStore(t, "gc")
	own := mustCreate(t, city, beads.Bead{Title: "own open bead", Type: "task"})
	row := mustCreate(t, city, beads.Bead{Title: "row", Type: "task"})
	setInProgress(t, city, row.ID, "w1")
	if err := city.DepAdd(row.ID, "external:"+own.ID, "blocks"); err != nil {
		t.Skipf("this bd refuses an external ref to its own bead: %v", err)
	}
	rows, err := Read([]Leg{testLeg("city", city)}, Query{Status: StatusInProgress, Assignee: "w1"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("Read = %+v, %v", rows, err)
	}
	if len(rows[0].BlockedBy) != 0 {
		t.Fatalf("blocked_by = %+v; bd never blocks on an external: target", rows[0].BlockedBy)
	}
}

// TestReadInProgressReportsEverySpellingOfOneBlocker: a row whose edges name
// one blocker twice — as written and as external:<id> — reports both edges,
// each with the blocker's status, on a split city.
func TestReadInProgressReportsEverySpellingOfOneBlocker(t *testing.T) {
	work := beads.NewMemStore()
	binding := sqliteBinding(t)
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task"})
	row := mustCreate(t, work, beads.Bead{Title: "row", Type: "task"})
	setInProgress(t, work, row.ID, "w1")
	for _, target := range []string{step.ID, "external:" + step.ID} {
		if err := work.DepAdd(row.ID, target, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := Read(splitLegs(work, binding), Query{Status: StatusInProgress, Assignee: "w1"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("Read = %+v, %v", rows, err)
	}
	if got := rows[0].BlockedBy; len(got) != 2 || got[0].Status != "open" || got[1].Status != "open" {
		t.Fatalf("blocked_by = %+v; want both spellings of %s, each open", got, step.ID)
	}
}

// TestReadReJudgesTheRowsACachedBdLegServes is the premature half of R9 behind
// the wrapping production puts on its work legs (#8360 review, Finding 1). A
// cache forwards the absent-blocker listing whatever store it wraps, so a
// reader that took the forwarded methods for "this store strands" trusted
// every row a cached bd leg served as already judged, and served a row whose
// blocker is open in another store.
func TestReadReJudgesTheRowsACachedBdLegServes(t *testing.T) {
	mem := beads.NewMemStore()
	work := beads.NewCachingStoreForTest(externalBlockerStore{Store: mem}, nil)
	if _, strands := beads.ReadyBlockedByAbsentFor(work); strands {
		t.Error("a cache over a store that strands nothing reports an absent-blocker listing")
	}
	binding := sqliteBinding(t)
	last := 4
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task", Priority: &last})
	external := mustCreate(t, mem, beads.Bead{Title: "external ref", Type: "task"})
	crossPrefix := mustCreate(t, mem, beads.Bead{Title: "cross-prefix", Type: "task"})
	dangling := mustCreate(t, mem, beads.Bead{Title: "dangling", Type: "task"})
	clean := mustCreate(t, mem, beads.Bead{Title: "clean", Type: "task"})
	for _, e := range [][2]string{
		{external.ID, "external:" + step.ID},
		{crossPrefix.ID, step.ID},
		{dangling.ID, "gcg-404"},
	} {
		if err := mem.DepAdd(e[0], e[1], "blocks"); err != nil {
			t.Fatalf("DepAdd(%s -> %s): %v", e[0], e[1], err)
		}
	}
	legs := splitLegs(work, binding)
	for _, q := range []Query{{}, {Limit: 1}, {Limit: 3, Sort: SortOldest}} {
		got := readyIDs(t, legs, q)
		for _, id := range []string{external.ID, crossPrefix.ID, dangling.ID} {
			if slices.Contains(got, id) {
				t.Errorf("ready(%+v) = %v served %s while its blocker is open or held by no store", q, got, id)
			}
		}
		if !slices.Contains(got, clean.ID) {
			t.Errorf("ready(%+v) = %v dropped the clean row %s", q, got, clean.ID)
		}
	}
	closeBead(t, binding, step.ID)
	got := readyIDs(t, legs, Query{})
	if !slices.Contains(got, external.ID) || !slices.Contains(got, crossPrefix.ID) {
		t.Errorf("ready = %v after %s closed, want %s and %s served", got, step.ID, external.ID, crossPrefix.ID)
	}
	if slices.Contains(got, dangling.ID) {
		t.Errorf("ready = %v served %s, whose blocker no store holds", got, dangling.ID)
	}
}

// countingStrandingLeg is a stranding work leg (the in-memory store) that
// counts the reads by id it is asked for: beads.GetMany's blocker reads.
type countingStrandingLeg struct {
	*beads.MemStore
	byID atomic.Int64
}

// Get counts a read by id: the in-memory store has no exact batch read, so
// beads.GetMany reads each id it is asked for with one Get.
func (s *countingStrandingLeg) Get(id string) (beads.Bead, error) {
	s.byID.Add(1)
	return s.MemStore.Get(id)
}

// TestReadHoldsAStrandingLegsRowOnACopyItDidNotJudge pins every copy on a
// stranding leg (#8360 review, Codex): its Ready judged only its own copy of a
// blocker, so a closed copy there must not hide an open copy of the same id in
// another store. A binding's own reserved-namespace blocker has no other copy
// and costs no read of another store.
func TestReadHoldsAStrandingLegsRowOnACopyItDidNotJudge(t *testing.T) {
	t.Run("a work row's blocker is closed here and open in the binding", func(t *testing.T) {
		work := beads.NewMemStore()
		binding := sqliteBinding(t)
		blocker := mustCreate(t, work, beads.Bead{Title: "migrated blocker", Type: "task"})
		closeBead(t, work, blocker.ID)
		if _, err := binding.(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{ID: blocker.ID, Title: "migrated copy", Type: "task", Priority: intRef(4)}); err != nil {
			t.Fatal(err)
		}
		row := mustCreate(t, work, beads.Bead{Title: "row", Type: "task"})
		if err := work.DepAdd(row.ID, blocker.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
		legs := splitLegs(work, binding)
		for _, q := range []Query{{}, {Limit: 1}} {
			if got := readyIDs(t, legs, q); slices.Contains(got, row.ID) {
				t.Errorf("ready(%+v) = %v served %s; its blocker %s is still open in the binding", q, got, row.ID, blocker.ID)
			}
		}
		closeBead(t, binding, blocker.ID)
		if got := readyIDs(t, legs, Query{}); !slices.Contains(got, row.ID) {
			t.Errorf("ready = %v withholds %s after its blocker closed in every store", got, row.ID)
		}
	})
	t.Run("a binding row's blocker is closed here and open in the work store", func(t *testing.T) {
		work := beads.NewMemStore()
		work.HonorExplicitIDs = true
		binding := sqliteBinding(t)
		relic, err := binding.(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{ID: "gc-relic-7", Title: "relic", Type: "task", Priority: intRef(4)})
		if err != nil {
			t.Fatal(err)
		}
		closeBead(t, binding, relic.ID)
		if _, err := work.Create(beads.Bead{ID: relic.ID, Title: "the original, still open", Type: "task", Priority: intRef(4)}); err != nil {
			t.Fatal(err)
		}
		step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task"})
		if err := binding.DepAdd(step.ID, relic.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
		legs := splitLegs(work, binding)
		for _, q := range []Query{{}, {Limit: 1}} {
			if got := readyIDs(t, legs, q); slices.Contains(got, step.ID) {
				t.Errorf("ready(%+v) = %v served %s; its blocker %s is still open in the work store", q, got, step.ID, relic.ID)
			}
		}
		closeBead(t, work, relic.ID)
		if got := readyIDs(t, legs, Query{}); !slices.Contains(got, step.ID) {
			t.Errorf("ready = %v withholds %s after its blocker closed in every store", got, step.ID)
		}
	})
	t.Run("a binding row's reserved-namespace blocker is judged by the binding alone", func(t *testing.T) {
		work := &countingStrandingLeg{MemStore: beads.NewMemStore()}
		binding := sqliteBinding(t)
		done := mustCreate(t, binding, beads.Bead{Title: "done step", Type: "task", Priority: intRef(4)})
		closeBead(t, binding, done.ID)
		step := mustCreate(t, binding, beads.Bead{Title: "next step", Type: "task"})
		if err := binding.DepAdd(step.ID, done.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
		if got := readyIDs(t, splitLegs(work, binding), Query{}); !slices.Contains(got, step.ID) {
			t.Fatalf("ready = %v withholds %s, whose only blocker is closed in the binding", got, step.ID)
		}
		if n := work.byID.Load(); n != 0 {
			t.Errorf("the work leg was asked for %d blocker read(s) for a blocker only the binding can hold, want 0", n)
		}
	})
}

// TestLegGateJudgesInFullARowACacheServesPastItsAbsentBlocker: a
// cache's own ready projection reads a target it does not hold as no blocker
// (the in-memory engine stamps no verdict of its own on its rows), so a cached
// snapshot carries the row its backing store strands. That row reaches the
// gate as a row the leg served, and the listing names it: it is judged in
// full, and held until its blocker closes in the store that holds it, even
// though no class binding holds that blocker.
func TestLegGateJudgesInFullARowACacheServesPastItsAbsentBlocker(t *testing.T) {
	mem := beads.NewMemStore()
	rig := beads.NewMemStore()
	rig.HonorExplicitIDs = true
	binding := sqliteBinding(t)
	blocker, err := rig.Create(beads.Bead{ID: "rg-1", Title: "rig blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	row := mustCreate(t, mem, beads.Bead{Title: "row", Type: "task"})
	if err := mem.DepAdd(row.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	work := beads.NewCachingStoreForTest(mem, nil)
	if err := work.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	cached, err := beads.HandlesFor(work).Cached.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("cached ready: %v", err)
	}
	if countID(cached, row.ID) != 1 {
		t.Fatalf("fixture: the cache's ready projection = %v does not serve %s; the case does not arise", cached, row.ID)
	}
	legs := []Leg{testLeg("work", work), testLeg("rig:rg", rig), {Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}}}
	q := beads.ReadyQuery{TierMode: beads.TierBoth}
	gated, err := NewLegGate(legs, q).Gate(context.Background(), 0, cached)
	if err != nil {
		t.Fatal(err)
	}
	if countID(gated, row.ID) != 0 {
		t.Fatalf("gated cached snapshot = %v serves %s while its blocker %s is open in the rig", gated, row.ID, blocker.ID)
	}
	closeBead(t, rig, blocker.ID)
	gated, err = NewLegGate(legs, q).Gate(context.Background(), 0, cached)
	if err != nil {
		t.Fatal(err)
	}
	if n := countID(gated, row.ID); n != 1 {
		t.Fatalf("gated cached snapshot = %v serves %s %d time(s) after its blocker closed, want once", gated, row.ID, n)
	}
}

func countID(rows []beads.Bead, id string) int {
	n := 0
	for _, b := range rows {
		if b.ID == id {
			n++
		}
	}
	return n
}

// TestLegGateReadsEachBlockerOncePerGate pins the memo a pass's gate keeps
// (#8360 review, Finding 2): demand gates a live and a cached snapshot of
// every store, and one gate reads each blocker, each row's edges and each
// leg's listing once, however many snapshots it gates.
func TestLegGateReadsEachBlockerOncePerGate(t *testing.T) {
	mem := beads.NewMemStore()
	work := &countingBdLeg{externalBlockerStore: externalBlockerStore{Store: mem}}
	rigMem := beads.NewMemStore()
	rig := &countingStrandingLeg{MemStore: rigMem}
	binding := sqliteBinding(t)
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task", Priority: intRef(4)})
	var rows []beads.Bead
	for i := 0; i < 3; i++ {
		row := mustCreate(t, mem, beads.Bead{Title: "gated", Type: "task"})
		if err := mem.DepAdd(row.ID, "external:"+step.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	legs := []Leg{testLeg("work", work), testLeg("rig:r", rig), {Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}}}
	gate := NewLegGate(legs, beads.ReadyQuery{TierMode: beads.TierBoth})
	for i := 0; i < 3; i++ {
		got, err := gate.Gate(context.Background(), 0, rows)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("gate %d served %v while %s is open", i, got, step.ID)
		}
	}
	if n := work.depLists.Load(); n != 1 {
		t.Errorf("the work leg's edges were read %d time(s) over three gates, want 1", n)
	}
	if n := rig.byID.Load(); n != 1 {
		t.Errorf("the rig leg was asked for the blocker %d time(s) over three gates, want 1", n)
	}
	if got, err := NewLegGate([]Leg{testLeg("work", work)}, beads.ReadyQuery{}).Gate(context.Background(), 0, rows); err != nil || len(got) != len(rows) {
		t.Fatalf("a city with no binding gated its rows to %v, %v; want them as read", got, err)
	}
}

// TestReadInProgressReportsAnOpenCopyBehindAClosedBlockedOne (#8360 review,
// Finding 9): a blocker whose first copy in leg order is closed blocked and
// whose later copy is open reports the open copy, so a reader that judges by
// status holds the row, as the ready arm does.
func TestReadInProgressReportsAnOpenCopyBehindAClosedBlockedOne(t *testing.T) {
	work := beads.NewMemStore()
	rig := beads.NewMemStore()
	rig.HonorExplicitIDs = true
	binding := sqliteBinding(t)
	bl := mustCreate(t, binding, beads.Bead{Title: "relic, open", Type: "task", Priority: intRef(4)})
	if _, err := rig.Create(beads.Bead{ID: bl.ID, Title: "copy closed blocked", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	if err := rig.SetMetadata(bl.ID, "gc.work_outcome", "blocked"); err != nil {
		t.Fatal(err)
	}
	closeBead(t, rig, bl.ID)
	row := mustCreate(t, work, beads.Bead{Title: "row", Type: "task"})
	setInProgress(t, work, row.ID, "w1")
	if err := work.DepAdd(row.ID, "external:"+bl.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	legs := []Leg{
		testLeg("work", work),
		testLeg("rig:r", rig), // the closed-blocked copy, first in leg order
		{Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}},
	}
	rows, err := Read(legs, Query{Status: StatusInProgress, Assignee: "w1"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("Read = %+v, %v", rows, err)
	}
	if got := rows[0].BlockedBy; len(got) != 1 || got[0].Status != "open" {
		t.Fatalf("blocked_by = %+v; want %s reported open, as its graph copy is", got, bl.ID)
	}
}

// closeBlocked closes id with gc.work_outcome=blocked: a close that does not
// release its dependents (beads.DependencySatisfied).
func closeBlocked(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if err := store.SetMetadata(id, "gc.work_outcome", "blocked"); err != nil {
		t.Fatal(err)
	}
	closeBead(t, store, id)
}

// TestInProgressHoldsAClaimWhoseBlockerIsClosedBlockedInEveryCopy (#8360
// review): a blocker whose every copy is closed with gc.work_outcome=blocked
// releases nothing, and the ready arm and the drain hold its dependents. The
// in-progress tier published such a copy as "closed", and both of its serve
// gates (hasOpenBlocker, the hook's openBlockerCountJQ) read only the status,
// so --until-unblocked resumed the claim. It holds both a claim blocked across
// stores and one blocked in its own store, and the cut stops at the first
// clean claim behind them; a normal close of the same blocker is the control.
func TestInProgressHoldsAClaimWhoseBlockerIsClosedBlockedInEveryCopy(t *testing.T) {
	work := beads.NewMemStore()
	binding := sqliteBinding(t)
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task"})
	ownBlocker := mustCreate(t, work, beads.Bead{Title: "own blocker", Type: "task"})
	closeBlocked(t, binding, step.ID)
	closeBlocked(t, work, ownBlocker.ID)
	byStep := inProgressRow(t, work, "blocked by a graph step closed blocked", 0, step.ID)
	byOwn := inProgressRow(t, work, "blocked by its own store's bead closed blocked", 1, ownBlocker.ID)
	clean := inProgressRow(t, work, "clean claim", 2)
	legs := splitLegs(work, binding)

	cut := untilUnblockedAndWhole(t, legs)
	if got, want := rowIDs(cut), []string{byStep.ID, byOwn.ID, clean.ID}; !slices.Equal(got, want) {
		t.Fatalf("cut = %v, want %v: a claim whose blocker released nothing is gated", got, want)
	}
	for _, row := range cut[:2] {
		if len(row.BlockedBy) != 1 {
			t.Fatalf("row %s blocked_by = %+v, want its one blocker", row.ID, row.BlockedBy)
		}
		if got := row.BlockedBy[0]; got.Status != BlockerClosedBlocked || !hasOpenBlocker(row.BlockedBy) {
			t.Errorf("row %s blocker = %+v, want status %q, which every blocked_by reader counts as blocking", row.ID, got, BlockerClosedBlocked)
		}
	}

	// The control: a normal close releases.
	if err := binding.SetMetadata(step.ID, "gc.work_outcome", "done"); err != nil {
		t.Fatal(err)
	}
	if got := rowIDs(untilUnblockedAndWhole(t, legs)); !slices.Equal(got, []string{byStep.ID}) {
		t.Fatalf("after the graph step's outcome cleared: cut = %v, want it to end at %s", got, byStep.ID)
	}
}

// TestInProgressJudgesTheServingLegsCopyAgainstEveryOtherCopy (#8360 review):
// the in-progress tier read a blocker its serving leg holds from that copy
// alone, so a closed copy there hid an open copy in the binding (a migrated
// relic) and the cut resumed a claim the ready arm holds. Every copy is
// judged, as the ready arm judges it.
func TestInProgressJudgesTheServingLegsCopyAgainstEveryOtherCopy(t *testing.T) {
	work := beads.NewMemStore()
	work.HonorExplicitIDs = true
	binding := sqliteBinding(t)
	relic, err := binding.(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{ID: "gc-relic-1", Title: "relic, open in the binding", Type: "task", Priority: intRef(4)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := work.Create(beads.Bead{ID: relic.ID, Title: "frozen copy, closed", Type: "task", Priority: intRef(4)}); err != nil {
		t.Fatal(err)
	}
	closeBead(t, work, relic.ID)
	held := inProgressRow(t, work, "blocked by the relic", 0, relic.ID)
	clean := inProgressRow(t, work, "clean claim", 1)
	legs := splitLegs(work, binding)

	cut := untilUnblockedAndWhole(t, legs)
	if got, want := rowIDs(cut), []string{held.ID, clean.ID}; !slices.Equal(got, want) {
		t.Fatalf("cut = %v, want %v: the relic is still open in the binding", got, want)
	}
	if got := cut[0].BlockedBy; len(got) != 1 || got[0].Status != "open" || !strings.Contains(got[0].Reason, "graph") {
		t.Fatalf("blocked_by = %+v, want %s open, with a reason naming the graph store that holds it open", got, relic.ID)
	}

	closeBead(t, binding, relic.ID)
	cut = untilUnblockedAndWhole(t, legs)
	if got := rowIDs(cut); !slices.Equal(got, []string{held.ID}) {
		t.Fatalf("after the binding's copy closed: cut = %v, want it to end at %s", got, held.ID)
	}
	if got := cut[0].BlockedBy; len(got) != 1 || got[0].Status != "closed" {
		t.Fatalf("blocked_by = %+v, want %s closed in every copy", got, relic.ID)
	}
}
