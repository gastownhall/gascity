package executionevent

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// countingGraphStore records how many reads of each shape a reconcile pass
// issues against one graph store. The tick's cost model is "sequential store
// round trips against a remote ledger", so the count — not the wall clock — is
// the thing a latency regression shows up in.
type countingGraphStore struct {
	beads.Store
	gets           int
	lists          int
	listByMetadata int
}

func (s *countingGraphStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

func (s *countingGraphStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	return s.Store.List(q)
}

// reads is the round-trip total: the unit a remote-ledger leg's latency is
// actually denominated in.
func (s *countingGraphStore) reads() int { return s.gets + s.lists + s.listByMetadata }

func (s *countingGraphStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.listByMetadata++
	return s.Store.ListByMetadata(filters, limit, opts...)
}

// TestReconcileCompletedStoresDecidesStepStatusFromListRows pins the deletion of
// the per-step Get (projector.go's old line 365). currentSteps' own
// ListByMetadata already carried every step row's Status and metadata; re-Getting
// each one turned the completions leg into O(roots x steps) sequential round
// trips against a store whose RTT on mc is ~5.4s.
//
// The negative is "zero Gets". Its control is the emitted-fact count: a pass
// that projected nothing would also issue zero Gets, so the two assertions have
// to fail differently for the measurement to mean anything.
func TestReconcileCompletedStoresDecidesStepStatusFromListRows(t *testing.T) {
	const roots, stepsPerRoot = 3, 4
	backing := beads.NewMemStore()
	closed := "closed"
	for r := range roots {
		root := mustCreateProjectionRoot(t, backing, "")
		for s := range stepsPerRoot {
			step := mustCreateProjectionStep(t, backing, stepBeadID(r, s), root.ID, "build", "[]")
			if err := backing.Update(step.ID, beads.UpdateOpts{
				Status:   &closed,
				Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-session"},
			}); err != nil {
				t.Fatalf("close step %s: %v", step.ID, err)
			}
		}
	}

	graph := &countingGraphStore{Store: backing}
	recorder := events.NewFake()
	emitted := ReconcileCompletedStores(recorder, []beads.GraphStore{{Store: graph}}, "execution-reconcile")

	// Control: the pass really did project the closed steps. Without this a
	// projection that silently stopped emitting would satisfy the Get budget.
	if want := roots * stepsPerRoot; emitted != want {
		t.Fatalf("emitted %d completion facts, want %d — the Get budget below would be met by a pass that projects nothing", emitted, want)
	}
	if graph.gets != 0 {
		t.Fatalf("reconcile issued %d per-step Get(s), want 0: currentSteps' ListByMetadata already carried Status and metadata", graph.gets)
	}
	// Second control: the counter is wired to a method the code under test can
	// actually reach. ProjectCurrent Gets the root, so a Get counter that never
	// increments — the way the assertion above could pass vacuously — fails here.
	if _, err := ProjectCurrent(beads.GraphStore{Store: graph}, beads.WorkStore{}, firstRootID(t, backing)); err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	if graph.gets == 0 {
		t.Fatal("the Get counter never incremented even on a path that Gets; the zero above is not a measurement")
	}
}

// TestReconcileCompletedStoresListsStepsOncePerRoot pins the remaining read
// budget so a future change cannot trade the deleted Gets for extra Lists. One
// ListByMetadata selects the roots; one more per root selects its steps.
func TestReconcileCompletedStoresListsStepsOncePerRoot(t *testing.T) {
	const roots = 3
	backing := beads.NewMemStore()
	for r := range roots {
		root := mustCreateProjectionRoot(t, backing, "")
		mustCreateProjectionStep(t, backing, stepBeadID(r, 0), root.ID, "build", "[]")
	}
	graph := &countingGraphStore{Store: backing}
	ReconcileCompletedStores(events.NewFake(), []beads.GraphStore{{Store: graph}}, "execution-reconcile")
	if want := 1 + roots; graph.listByMetadata != want {
		t.Fatalf("reconcile issued %d ListByMetadata call(s), want %d (1 roots list + 1 steps list per root)", graph.listByMetadata, want)
	}
}

func stepBeadID(root, step int) string {
	return "gcg-step-" + string(rune('a'+root)) + string(rune('0'+step))
}

func firstRootID(t *testing.T, store beads.Store) string {
	t.Helper()
	roots, err := store.ListByMetadata(
		map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
		0,
		beads.IncludeClosed,
		beads.WithBothTiers,
	)
	if err != nil || len(roots) == 0 {
		t.Fatalf("listing roots: %v (%d found)", err, len(roots))
	}
	return roots[0].ID
}

// TestReconcileCompletedRootsTouchesOnlyEventNamedRoots is the S2 delta
// property. reconcile_execution_completions was 72.4s +/- 0.9s of a ~360s tick
// (ga-l7jdg): every workflow root ever, closed ones included, walked on every
// tick against a corpus that only grows. Only the roots the journal named since
// the last pass can have changed, so only those may be read.
//
// The control is the backstop over the identical corpus: it touches all N. Two
// assertions that fail differently — a delta that read nothing at all would
// satisfy the O(1) budget and fail the emit count, and a delta that read
// everything would satisfy the emit count and fail the budget.
func TestReconcileCompletedRootsTouchesOnlyEventNamedRoots(t *testing.T) {
	const roots = 12
	backing := beads.NewMemStore()
	closed := "closed"
	var rootIDs []string
	for r := range roots {
		root := mustCreateProjectionRoot(t, backing, "")
		rootIDs = append(rootIDs, root.ID)
		step := mustCreateProjectionStep(t, backing, stepBeadID(r, 0), root.ID, "build", "[]")
		if err := backing.Update(step.ID, beads.UpdateOpts{
			Status:   &closed,
			Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-session"},
		}); err != nil {
			t.Fatalf("close step: %v", err)
		}
	}

	graph := &countingGraphStore{Store: backing}
	recorder := events.NewFake()
	named := []string{rootIDs[3]}
	emitted := ReconcileCompletedRoots(recorder, []beads.GraphStore{{Store: graph}}, named, "execution-reconcile")
	if emitted != 1 {
		t.Fatalf("delta emitted %d fact(s) for 1 named root, want 1", emitted)
	}
	// One batched read to fetch the named roots, one steps read per named root.
	if want := 1 + len(named); graph.reads() != want {
		t.Fatalf("delta issued %d store read(s) for %d named root(s) out of %d, want %d", graph.reads(), len(named), roots, want)
	}

	// Control: the backstop over the same corpus visits every root.
	full := &countingGraphStore{Store: backing}
	if got := ReconcileCompletedStores(recorder, []beads.GraphStore{{Store: full}}, "execution-reconcile"); got != roots-1 {
		t.Fatalf("backstop emitted %d, want %d (every root but the one the delta already repaired)", got, roots-1)
	}
	if full.reads() <= graph.reads() {
		t.Fatalf("the backstop cost %d read(s) and the delta %d; the delta is not narrower and the budget above measures nothing", full.reads(), graph.reads())
	}
}

// TestReconcileCompletedRootsWithNoNamedRootsReadsNothing is the steady-tick
// property: a tick whose journal named no root must not touch a store OR the
// journal. The full pass re-read the whole retained completion journal every
// time it ran, which under overload was every tick.
func TestReconcileCompletedRootsWithNoNamedRootsReadsNothing(t *testing.T) {
	backing := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, backing, "")
	mustCreateProjectionStep(t, backing, "gcg-step-idle", root.ID, "build", "[]")
	graph := &countingGraphStore{Store: backing}
	provider := &countingEventProvider{Provider: events.NewFake()}

	if got := ReconcileCompletedRoots(provider, []beads.GraphStore{{Store: graph}}, nil, "execution-reconcile"); got != 0 {
		t.Fatalf("delta with no named roots emitted %d, want 0", got)
	}
	if graph.reads() != 0 {
		t.Fatalf("delta with no named roots issued %d store read(s), want 0", graph.reads())
	}
	if provider.listCalls != 0 {
		t.Fatalf("delta with no named roots issued %d journal read(s), want 0", provider.listCalls)
	}
	// Control: naming a root makes the same call read both.
	if got := ReconcileCompletedRoots(provider, []beads.GraphStore{{Store: graph}}, []string{root.ID}, "execution-reconcile"); got != 0 {
		t.Fatalf("delta over an open step emitted %d, want 0", got)
	}
	if graph.reads() == 0 || provider.listCalls == 0 {
		t.Fatalf("naming a root cost %d store read(s) and %d journal read(s); the zeros above are not a measurement", graph.reads(), provider.listCalls)
	}
}

// TestReconcileCompletedBackstopHealsAStrandedCloseTheDeltaCannotSee is the
// convergence control for this leg, and the reason the full pass survives.
//
// A controller can crash between the durable graph-step close and the
// best-effort journal append, and graph stores emit no bead.closed at all by
// design. So a close can exist with NO event naming it: the delta pass is
// correct to emit nothing (it cannot know), and the backstop must repair it —
// exactly once, with the second pass silent.
func TestReconcileCompletedBackstopHealsAStrandedCloseTheDeltaCannotSee(t *testing.T) {
	backing := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, backing, "")
	step := mustCreateProjectionStep(t, backing, "gcg-stranded", root.ID, "build", "[]")
	closed := "closed"
	if err := backing.Update(step.ID, beads.UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-session"},
	}); err != nil {
		t.Fatalf("close step: %v", err)
	}
	recorder := events.NewFake()
	stores := []beads.GraphStore{{Store: backing}}

	// No event named the root, so the delta lane has nothing to pass in.
	if got := ReconcileCompletedRoots(recorder, stores, nil, "execution-reconcile"); got != 0 {
		t.Fatalf("delta emitted %d for an unannounced close, want 0", got)
	}
	if got := ReconcileCompletedStores(recorder, stores, "execution-reconcile"); got != 1 {
		t.Fatalf("backstop emitted %d, want 1", got)
	}
	if got := ReconcileCompletedStores(recorder, stores, "execution-reconcile"); got != 0 {
		t.Fatalf("second backstop emitted %d, want 0 (not idempotent)", got)
	}
	// And the delta lane agrees with the journal the backstop wrote.
	if got := ReconcileCompletedRoots(recorder, stores, []string{root.ID}, "execution-reconcile"); got != 0 {
		t.Fatalf("delta after the backstop emitted %d, want 0", got)
	}
}

// TestCompletionBackstopResumesFromItsCursorWithoutRepeatingRoots pins the
// chunked backstop's monotonic progress: under repeated tiny budgets every root
// is visited exactly once per sweep, and a pass never restarts from zero.
func TestCompletionBackstopResumesFromItsCursorWithoutRepeatingRoots(t *testing.T) {
	const roots = 7
	backing := beads.NewMemStore()
	closed := "closed"
	for r := range roots {
		root := mustCreateProjectionRoot(t, backing, "")
		step := mustCreateProjectionStep(t, backing, stepBeadID(r, 0), root.ID, "build", "[]")
		if err := backing.Update(step.ID, beads.UpdateOpts{
			Status:   &closed,
			Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-session"},
		}); err != nil {
			t.Fatalf("close step: %v", err)
		}
	}
	recorder := events.NewFake()
	stores := []beads.GraphStore{{Store: backing}}
	backstop := &CompletionBackstop{ChunkSize: 2}

	visited, emitted, passes := 0, 0, 0
	for {
		passes++
		if passes > roots+4 {
			t.Fatalf("the sweep did not finish in %d passes; the cursor is not advancing", passes)
		}
		result := backstop.Pass(recorder, stores, "execution-reconcile")
		if result.RootsVisited > 2 {
			t.Fatalf("pass %d visited %d roots, want at most the chunk size 2", passes, result.RootsVisited)
		}
		visited += result.RootsVisited
		emitted += result.Emitted
		if result.SweepComplete {
			break
		}
	}
	if visited != roots {
		t.Fatalf("one chunked sweep visited %d root(s), want %d exactly once each", visited, roots)
	}
	if emitted != roots {
		t.Fatalf("one chunked sweep emitted %d fact(s), want %d", emitted, roots)
	}
	// Control: the next sweep re-visits every root (it is a convergence scan,
	// not a one-shot) and emits nothing, so "visited exactly once" above is a
	// statement about one sweep rather than about a cursor that never resets.
	second := 0
	for {
		result := backstop.Pass(recorder, stores, "execution-reconcile")
		second += result.RootsVisited
		if result.Emitted != 0 {
			t.Fatalf("the second sweep emitted %d fact(s), want 0", result.Emitted)
		}
		if result.SweepComplete {
			break
		}
	}
	if second != roots {
		t.Fatalf("the second sweep visited %d root(s), want %d", second, roots)
	}
}
