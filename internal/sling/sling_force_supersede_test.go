package sling

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// seedConvoyRoot writes a graph.v2 workflow root for (beadID, formulaName)
// reachable through a tracking input convoy, the shape a convoy-first `--on`
// launch leaves behind. synthetic=false makes the convoy a real grouping
// convoy (a convoy-level launch), status is the root's status.
func seedConvoyRoot(t *testing.T, store beads.Store, beadID, formulaName, status string, synthetic bool) (root, convoy beads.Bead) {
	t.Helper()
	convoyMeta := map[string]string{}
	if synthetic {
		convoyMeta[beadmeta.SyntheticMetadataKey] = "true"
	}
	convoy, err := store.Create(beads.Bead{Title: "input convoy for " + beadID, Type: "convoy", Metadata: convoyMeta})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoycore.TrackItemIn(convoycore.MemberClasses{Convoy: beadstest.Work(store)}, convoy.ID, beadID); err != nil {
		t.Fatal(err)
	}
	root, err = store.Create(beads.Bead{
		Title:    "workflow root for " + beadID,
		Type:     "task",
		Status:   status,
		Assignee: "worker-" + formulaName,
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.FormulaNameMetadataKey:     formulaName,
			beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Create does not honor a non-open status on every store; set it
	// explicitly so a seeded root really is in the requested state.
	switch status {
	case "closed":
		if err := store.Close(root.ID); err != nil {
			t.Fatal(err)
		}
	case "", "open":
	default:
		if err := store.Update(root.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatal(err)
		}
	}
	return root, convoy
}

func mustGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

func forceTestSource(t *testing.T, deps SlingDeps) beads.Bead {
	t.Helper()
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// TestForceSupersedesEveryLiveConvoyTrackedRoot is I1: with N>1 live priors a
// forced launch closes ALL of them and leaves exactly one live root.
func TestForceSupersedesEveryLiveConvoyTrackedRoot(t *testing.T) {
	deps := graphV2ConvoyFirstSlingTestConfig(t)
	source := forceTestSource(t, deps)
	priorA, convoyA := seedConvoyRoot(t, deps.Store, source.ID, "graph-work", "in_progress", true)
	priorB, convoyB := seedConvoyRoot(t, deps.Store, source.ID, "graph-work", "open", true)
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{Force: true})
	if err != nil {
		t.Fatalf("forced AttachFormula: %v", err)
	}

	live := liveGraphV2Roots(t, deps.Store)
	if len(live) != 1 || live[0].ID != res.WorkflowID {
		t.Fatalf("live roots = %+v, want exactly the new root %s", live, res.WorkflowID)
	}
	if want := []string{priorA.ID, priorB.ID}; !slices.Equal(res.Superseded, want) {
		t.Fatalf("result.Superseded = %v, want %v", res.Superseded, want)
	}
	for _, id := range []string{priorA.ID, priorB.ID} {
		got := mustGet(t, deps.Store, id)
		if got.Status != "closed" || got.Metadata[beadmeta.FailureReasonMetadataKey] != "graphv2_force_replaced" {
			t.Errorf("prior %s = status %q reason %q, want closed/graphv2_force_replaced", id, got.Status, got.Metadata[beadmeta.FailureReasonMetadataKey])
		}
	}
	for _, id := range []string{convoyA.ID, convoyB.ID} {
		if got := mustGet(t, deps.Store, id); got.Status != "closed" {
			t.Errorf("superseded root's synthetic convoy %s status = %q, want closed", id, got.Status)
		}
	}
}

// TestForceLeavesUnrelatedRootsAlone is I4: the replaced set is exactly the
// same-formula, synthetic-convoy, non-terminal roots.
func TestForceLeavesUnrelatedRootsAlone(t *testing.T) {
	deps := graphV2ConvoyFirstSlingTestConfig(t)
	source := forceTestSource(t, deps)
	other, otherConvoy := seedConvoyRoot(t, deps.Store, source.ID, "some-other-formula", "in_progress", true)
	closed, _ := seedConvoyRoot(t, deps.Store, source.ID, "graph-work", "closed", true)
	grouped, groupedConvoy := seedConvoyRoot(t, deps.Store, source.ID, "graph-work", "in_progress", false)
	target, _ := seedConvoyRoot(t, deps.Store, source.ID, "graph-work", "in_progress", true)
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{Force: true})
	if err != nil {
		t.Fatalf("forced AttachFormula: %v", err)
	}
	if want := []string{target.ID}; !slices.Equal(res.Superseded, want) {
		t.Fatalf("result.Superseded = %v, want only %v", res.Superseded, want)
	}
	for _, c := range []struct {
		name   string
		id     string
		status string
		reason string
	}{
		{"different formula root", other.ID, "in_progress", ""},
		{"closed root", closed.ID, "closed", ""},
		{"non-synthetic-convoy root", grouped.ID, "in_progress", ""},
	} {
		got := mustGet(t, deps.Store, c.id)
		if got.Status != c.status || got.Metadata[beadmeta.FailureReasonMetadataKey] != c.reason {
			t.Errorf("%s %s = status %q reason %q, want untouched (%s %q)", c.name, c.id, got.Status, got.Metadata[beadmeta.FailureReasonMetadataKey], c.status, c.reason)
		}
	}
	for _, id := range []string{otherConvoy.ID, groupedConvoy.ID} {
		if got := mustGet(t, deps.Store, id); got.Status == "closed" {
			t.Errorf("convoy %s of an untouched root was closed", id)
		}
	}
}

// TestWithoutForceNothingChanges is I3: no --force, same ConflictError, and no
// prior root, convoy or new root is touched.
func TestWithoutForceNothingChanges(t *testing.T) {
	deps := graphV2ConvoyFirstSlingTestConfig(t)
	source := forceTestSource(t, deps)
	prior, convoy := seedConvoyRoot(t, deps.Store, source.ID, "graph-work", "in_progress", true)
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{})
	var conflict *sourceworkflow.ConflictError
	if !errors.As(err, &conflict) || !slices.Contains(conflict.WorkflowIDs, prior.ID) {
		t.Fatalf("AttachFormula without --force = %v, want ConflictError naming %s", err, prior.ID)
	}
	if got := mustGet(t, deps.Store, prior.ID); got.Status != "in_progress" || got.Metadata[beadmeta.FailureReasonMetadataKey] != "" {
		t.Errorf("prior root changed without --force: status %q reason %q", got.Status, got.Metadata[beadmeta.FailureReasonMetadataKey])
	}
	if got := mustGet(t, deps.Store, convoy.ID); got.Status == "closed" {
		t.Error("prior input convoy closed without --force")
	}
	if live := liveGraphV2Roots(t, deps.Store); len(live) != 1 || live[0].ID != prior.ID {
		t.Errorf("live roots = %+v, want only the untouched prior %s", live, prior.ID)
	}
}

// failingGraphStore injects faults into the graph store a forced launch
// supersedes through.
type failingGraphStore struct {
	beads.Store
	// failPromoteNew fails the in_progress promotion of any root not in spare.
	failPromoteNew bool
	spare          map[string]bool
	// failMarkOn fails the gc.failure_reason write on these roots.
	failMarkOn map[string]bool
}

func (s *failingGraphStore) Update(id string, opts beads.UpdateOpts) error {
	if s.failPromoteNew && !s.spare[id] && opts.Status != nil && *opts.Status == "in_progress" {
		return errRefusedPromotion
	}
	return s.Store.Update(id, opts)
}

func (s *failingGraphStore) SetMetadata(id, key, value string) error {
	if s.failMarkOn[id] && key == beadmeta.FailureReasonMetadataKey && value == "graphv2_force_replaced" {
		return errRefusedPromotion
	}
	return s.Store.SetMetadata(id, key, value)
}

func assertPriorsRestored(t *testing.T, store beads.Store, source string, priors map[string]string, newConvoysClosed bool) {
	t.Helper()
	for id, wantStatus := range priors {
		got := mustGet(t, store, id)
		if got.Status != wantStatus || got.Metadata[beadmeta.FailureReasonMetadataKey] != "" {
			t.Errorf("prior %s after failed force = status %q reason %q, want %q with no failure_reason", id, got.Status, got.Metadata[beadmeta.FailureReasonMetadataKey], wantStatus)
		}
	}
	live := liveGraphV2Roots(t, store)
	if len(live) != len(priors) {
		t.Errorf("live roots after failed force = %d (%+v), want the %d priors and nothing else", len(live), live, len(priors))
	}
	for _, r := range live {
		if _, ok := priors[r.ID]; !ok {
			t.Errorf("a root %s that is not a prior is live after a failed force", r.ID)
		}
	}
	// Every synthetic convoy that is not a prior's must be closed.
	priorConvoys := map[string]bool{}
	for id := range priors {
		priorConvoys[mustGet(t, store, id).Metadata[beadmeta.InputConvoyIDMetadataKey]] = true
	}
	convoys, err := convoycore.TrackingConvoysForItem(beadstest.Work(store), source)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range convoys {
		if priorConvoys[c.ID] {
			if c.Status == "closed" {
				t.Errorf("a prior's input convoy %s was closed by a failed force", c.ID)
			}
			continue
		}
		if newConvoysClosed && c.Status != "closed" {
			t.Errorf("the failed launch's synthetic convoy %s is %q, want closed", c.ID, c.Status)
		}
	}
}

// TestForceFailedLaunchRestoresEveryPrior is I2 for a launch that fails to
// start: every prior is live again, the new root and convoy are closed.
func TestForceFailedLaunchRestoresEveryPrior(t *testing.T) {
	deps := graphV2ConvoyFirstSlingTestConfig(t)
	mem := beads.NewMemStore()
	fault := &failingGraphStore{Store: mem, spare: map[string]bool{}}
	deps = withStore(deps, fault)
	source := forceTestSource(t, deps)
	priorA, _ := seedConvoyRoot(t, mem, source.ID, "graph-work", "in_progress", true)
	priorB, _ := seedConvoyRoot(t, mem, source.ID, "graph-work", "open", true)
	fault.failPromoteNew = true
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{Force: true}); err == nil {
		t.Fatal("forced AttachFormula succeeded; the fixture must fail the new root's start")
	}
	assertPriorsRestored(t, mem, source.ID, map[string]string{priorA.ID: "in_progress", priorB.ID: "open"}, true)
}

// TestForceFailureMidSupersedeRestoresEveryPrior is I2 for a failure AFTER the
// first prior was already closed: the second prior's close fails, so the first
// must come back and the replacement must not stay live. Never zero live roots.
func TestForceFailureMidSupersedeRestoresEveryPrior(t *testing.T) {
	deps := graphV2ConvoyFirstSlingTestConfig(t)
	mem := beads.NewMemStore()
	fault := &failingGraphStore{Store: mem, spare: map[string]bool{}, failMarkOn: map[string]bool{}}
	deps = withStore(deps, fault)
	source := forceTestSource(t, deps)
	priorA, _ := seedConvoyRoot(t, mem, source.ID, "graph-work", "in_progress", true)
	priorB, _ := seedConvoyRoot(t, mem, source.ID, "graph-work", "open", true)
	fault.failMarkOn[priorB.ID] = true
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{Force: true}); err == nil {
		t.Fatal("forced AttachFormula succeeded; the second prior's close was set to fail")
	}
	assertPriorsRestored(t, mem, source.ID, map[string]string{priorA.ID: "in_progress", priorB.ID: "open"}, true)
}

// TestConcurrentForcedLaunchesConvergeToOneLiveRoot is I5: the lookup, launch
// and supersede all run inside the per-bead lock, so two simultaneous forced
// launches serialize and the later one supersedes the earlier.
func TestConcurrentForcedLaunchesConvergeToOneLiveRoot(t *testing.T) {
	deps := graphV2ConvoyFirstSlingTestConfig(t)
	source := forceTestSource(t, deps)
	seedConvoyRoot(t, deps.Store, source.ID, "graph-work", "in_progress", true)
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{Force: true})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("forced launch %d: %v", i, err)
		}
	}
	if live := liveGraphV2Roots(t, deps.Store); len(live) != 1 {
		t.Fatalf("live roots after %d concurrent forced launches = %d (%+v), want exactly 1", n, len(live), live)
	}
}
