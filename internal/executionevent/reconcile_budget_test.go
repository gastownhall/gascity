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
	listByMetadata int
}

func (s *countingGraphStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

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
