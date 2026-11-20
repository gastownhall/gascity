package dispatch

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// skippedBlockerWorkflow is a workflow root and its workflow-finalize control
// whose direct blocks blockers are already closed with the given outcomes.
type skippedBlockerWorkflow struct {
	root      beads.Bead
	finalizer beads.Bead
	blockers  []beads.Bead
}

// skippedIDs returns the IDs of the blockers closed gc.outcome=skipped,
// sorted ascending: the value gc.skipped_blockers must carry.
func (w skippedBlockerWorkflow) skippedIDs() []string {
	var ids []string
	for _, b := range w.blockers {
		if b.Metadata[beadmeta.OutcomeMetadataKey] == beadmeta.OutcomeSkipped {
			ids = append(ids, b.ID)
		}
	}
	return slices.Sorted(slices.Values(ids))
}

// newSkippedBlockerWorkflow builds, in store, an open root with rootMeta
// added to its workflow metadata, one closed blocker per outcome, and a
// finalizer over them. The blocks edges are added in descending-ID order so
// the finalizer's DepList hands the blockers back unsorted.
func newSkippedBlockerWorkflow(t *testing.T, store beads.Store, rootMeta map[string]string, outcomes ...string) skippedBlockerWorkflow {
	t.Helper()
	meta := map[string]string{
		beadmeta.KindMetadataKey: "workflow",
		"gc.formula_contract":    "graph.v2",
	}
	for k, v := range rootMeta {
		meta[k] = v
	}
	w := skippedBlockerWorkflow{root: mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: meta,
	})}
	for _, outcome := range outcomes {
		w.blockers = append(w.blockers, mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "step " + outcome,
			Type:     "task",
			Status:   "closed",
			Metadata: map[string]string{beadmeta.OutcomeMetadataKey: outcome},
		}))
	}
	w.finalizer = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       "workflow-finalize",
			beadmeta.RootBeadIDMetadataKey: w.root.ID,
		},
	})
	for i := len(w.blockers) - 1; i >= 0; i-- {
		mustDepAdd(t, store, w.finalizer.ID, w.blockers[i].ID, "blocks")
	}
	mustDepAdd(t, store, w.root.ID, w.finalizer.ID, "blocks")
	return w
}

// A workflow whose finalizer blockers include steps closed gc.outcome=skipped
// still passes (a skip is not a failure), and the root says which blockers
// were skipped: gc.skipped_blockers carries their IDs, sorted and
// comma-joined. Without it a root that passed only because work was skipped
// reads exactly like one whose every step ran (#6096).
func TestProcessWorkflowFinalizeMarksSkippedBlockersOnPass(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	w := newSkippedBlockerWorkflow(t, store, nil,
		beadmeta.OutcomeSkipped, beadmeta.OutcomePass, beadmeta.OutcomeSkipped)

	// Premise: DepList returns the skipped blockers out of order, so a marker
	// that is not sorted fails below.
	deps, err := store.DepList(w.finalizer.ID, "down")
	if err != nil {
		t.Fatalf("DepList(finalizer): %v", err)
	}
	var depOrder []string
	for _, dep := range deps {
		if slices.Contains(w.skippedIDs(), dep.DependsOnID) {
			depOrder = append(depOrder, dep.DependsOnID)
		}
	}
	if slices.IsSorted(depOrder) {
		t.Fatalf("premise: DepList skipped order %v is already sorted", depOrder)
	}

	result, err := ProcessControl(beadstest.Graph(store), w.finalizer, ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want processed workflow-pass (a skip is not a failure)", result)
	}
	root, err := store.Get(w.root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if root.Status != "closed" || root.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("root = %s/%q, want closed/pass", root.Status, root.Metadata[beadmeta.OutcomeMetadataKey])
	}
	want := strings.Join(w.skippedIDs(), ",")
	if got := root.Metadata["gc.skipped_blockers"]; got != want {
		t.Fatalf("root gc.skipped_blockers = %q, want %q (the skipped blockers, sorted; the passed one is not listed)", got, want)
	}
}

// workflowRootCloseMetadata is the single gate for the marker: only a pass
// with at least one skipped blocker adds it, and every other close is the
// one-key gc.outcome map setOutcomeAndClose writes.
func TestWorkflowRootCloseMetadata(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		outcome string
		skipped []string
		want    map[string]string
	}{
		{name: "pass, nil", outcome: beadmeta.OutcomePass, want: map[string]string{"gc.outcome": "pass"}},
		{name: "pass, empty", outcome: beadmeta.OutcomePass, skipped: []string{}, want: map[string]string{"gc.outcome": "pass"}},
		{name: "pass, skipped", outcome: beadmeta.OutcomePass, skipped: []string{"a", "b"}, want: map[string]string{"gc.outcome": "pass", "gc.skipped_blockers": "a,b"}},
		{name: "fail, skipped", outcome: beadmeta.OutcomeFail, skipped: []string{"a"}, want: map[string]string{"gc.outcome": "fail"}},
		{name: "canceled, skipped", outcome: beadmeta.OutcomeCanceled, skipped: []string{"a"}, want: map[string]string{"gc.outcome": "canceled"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := workflowRootCloseMetadata(tc.outcome, tc.skipped); !maps.Equal(got, tc.want) {
				t.Fatalf("workflowRootCloseMetadata(%q, %v) = %v, want %v", tc.outcome, tc.skipped, got, tc.want)
			}
		})
	}
}

// processSkippedBlockerWorkflow runs w's finalizer and returns the result and
// the root as it closed.
func processSkippedBlockerWorkflow(t *testing.T, store beads.Store, w skippedBlockerWorkflow) (ControlResult, beads.Bead) {
	t.Helper()
	result, err := ProcessControl(beadstest.Graph(store), w.finalizer, ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	root, err := store.Get(w.root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	return result, root
}

// A pass in which no blocker was skipped writes no marker: absence, not an
// empty value, is what tells a watcher every blocker ran.
func TestProcessWorkflowFinalizeAllPassedWritesNoSkipMarker(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	w := newSkippedBlockerWorkflow(t, store, nil, beadmeta.OutcomePass, beadmeta.OutcomePass)
	result, root := processSkippedBlockerWorkflow(t, store, w)
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want processed workflow-pass", result)
	}
	if root.Status != "closed" || root.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("root = %s/%q, want closed/pass", root.Status, root.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if got, ok := root.Metadata["gc.skipped_blockers"]; ok {
		t.Fatalf("root gc.skipped_blockers = %q, want the key absent when no blocker was skipped", got)
	}
}

// A skipped blocker in a workflow that does not pass changes nothing: the
// outcome is today's, and no marker is written, whichever route made the
// workflow non-pass (a failed blocker, a root the control dispatcher already
// quarantined, or a run cancel).
func TestProcessWorkflowFinalizeNonPassWritesNoSkipMarker(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		rootMeta   map[string]string
		quarantine bool
		outcomes   []string
		action     string
		outcome    string
	}{
		{
			name:     "failed blocker",
			outcomes: []string{beadmeta.OutcomeSkipped, beadmeta.OutcomeFail},
			action:   "workflow-fail",
			outcome:  beadmeta.OutcomeFail,
		},
		{
			name:       "control-quarantined root",
			quarantine: true,
			outcomes:   []string{beadmeta.OutcomeSkipped, beadmeta.OutcomePass},
			action:     "workflow-fail",
			outcome:    beadmeta.OutcomeFail,
		},
		{
			name:     "cancel requested",
			rootMeta: map[string]string{beadmeta.CancelRequestedMetadataKey: "true"},
			outcomes: []string{beadmeta.OutcomeSkipped, beadmeta.OutcomePass},
			action:   "workflow-canceled",
			outcome:  beadmeta.OutcomeCanceled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := beads.NewMemStore()
			w := newSkippedBlockerWorkflow(t, store, tc.rootMeta, tc.outcomes...)
			if tc.quarantine {
				quarantineWorkflowRoot(t, store, w.root.ID)
			}
			result, root := processSkippedBlockerWorkflow(t, store, w)
			if !result.Processed || result.Action != tc.action {
				t.Fatalf("result = %+v, want processed %s", result, tc.action)
			}
			if root.Status != "closed" || root.Metadata[beadmeta.OutcomeMetadataKey] != tc.outcome {
				t.Fatalf("root = %s/%q, want closed/%s", root.Status, root.Metadata[beadmeta.OutcomeMetadataKey], tc.outcome)
			}
			if got, ok := root.Metadata["gc.skipped_blockers"]; ok {
				t.Fatalf("root gc.skipped_blockers = %q, want the key absent on a %s close", got, tc.outcome)
			}
		})
	}
}

// The marker rides the blocker scan the finalizer already makes and the root
// close it already writes: a pass with skipped blockers costs exactly the
// store calls of the same workflow with every blocker passed
// (call_budget_test.go pins the workflow-finalize ceilings).
func TestProcessWorkflowFinalizeSkipMarkerCostsNoStoreCall(t *testing.T) {
	t.Parallel()

	count := func(outcomes ...string) *callCountingStore {
		t.Helper()
		store := beads.NewMemStore()
		w := newSkippedBlockerWorkflow(t, store, nil, outcomes...)
		counting := &callCountingStore{Store: store}
		result, err := ProcessControl(beadstest.Graph(counting), w.finalizer, ProcessOptions{Work: beadstest.Work(counting)})
		if err != nil {
			t.Fatalf("ProcessControl(workflow-finalize): %v", err)
		}
		if result.Action != "workflow-pass" {
			t.Fatalf("result = %+v, want workflow-pass", result)
		}
		return counting
	}
	skipped := count(beadmeta.OutcomeSkipped, beadmeta.OutcomePass, beadmeta.OutcomeSkipped)
	passed := count(beadmeta.OutcomePass, beadmeta.OutcomePass, beadmeta.OutcomePass)
	if skipped.reads != passed.reads || skipped.writes != passed.writes {
		t.Fatalf("skipped-blocker pass = %d reads/%d writes, all-passed = %d reads/%d writes, want equal\nskipped:\n  %s\npassed:\n  %s",
			skipped.reads, skipped.writes, passed.reads, passed.writes,
			strings.Join(skipped.ops, "\n  "), strings.Join(passed.ops, "\n  "))
	}
}
