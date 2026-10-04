package sling

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
)

func executionOwnershipFixture(t *testing.T) (SlingDeps, beads.Bead, beads.Bead, config.Agent) {
	t.Helper()
	dir := t.TempDir()
	writeGraphV2ConvoyFormula(t, dir)
	deps := testDeps(graphV2SlingTestConfig(t, dir), runtime.NewFake(), func(string, string, map[string]string) (string, error) { return "", nil })
	deps.CityPath = t.TempDir()
	member, err := deps.Store.Create(beads.Bead{Title: "input work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := deps.Store.Create(beads.Bead{Title: "input", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoy.TrackItem(deps.Store, input.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	return deps, member, input, config.Agent{Name: "executor", MaxActiveSessions: intPtr(1)}
}

func launchOwnershipWorkflow(deps SlingDeps, input beads.Bead, a config.Agent) (*molecule.Result, error) {
	return InstantiateSlingFormula(context.Background(), "graph-work", deps.Cfg.FormulaLayers.City,
		molecule.Options{Vars: map[string]string{"convoy_id": input.ID}}, "", "default", "", a, deps)
}

// A workflow owns its input work, not merely its worker step. Both launch
// shapes must contend on the input bead's existing assignment CAS.
func TestExecutionOwnershipDirectThenWorkflow(t *testing.T) {
	for _, state := range []string{"routed", "claimed"} {
		t.Run(state, func(t *testing.T) {
			deps, member, input, a := executionOwnershipFixture(t)
			if state == "claimed" {
				owner := "direct-session"
				if err := deps.Store.Update(member.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
					t.Fatal(err)
				}
			} else if err := deps.Store.SetMetadata(member.ID, beadmeta.RoutedToMetadataKey, "planner"); err != nil {
				t.Fatal(err)
			}
			if _, err := launchOwnershipWorkflow(deps, input, a); err == nil {
				t.Fatal("workflow launched over direct ownership")
			}
			if roots := liveGraphV2Roots(t, deps.Store); len(roots) != 0 {
				t.Fatalf("loser created roots: %+v", roots)
			}
		})
	}
}

func TestExecutionOwnershipWorkflowThenDirect(t *testing.T) {
	deps, member, input, a := executionOwnershipFixture(t)
	if _, err := launchOwnershipWorkflow(deps, input, a); err != nil {
		t.Fatal(err)
	}
	for _, reassign := range []bool{false, true} {
		opts := SlingOpts{BeadOrFormula: member.ID, Target: config.Agent{Name: "planner"}, NoFormula: true, Force: true, Reassign: reassign}
		if _, err := DoSling(opts, deps, deps.Store); err == nil {
			t.Fatalf("direct dispatch stole workflow input (reassign=%v)", reassign)
		}
	}
	b, err := deps.Store.Get(member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee == "" {
		t.Fatal("workflow left input available to a direct bd claim")
	}
}

// Start both writers from the same input revision, using a barrier rather
// than timing. The direct side models bd's assignment compare-and-swap.
func TestExecutionOwnershipConcurrentClaim(t *testing.T) {
	deps, member, input, a := executionOwnershipFixture(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var directErr, workflowErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		owner := "direct-session"
		directErr = deps.Store.(beads.ConditionalWriter).UpdateIfMatch(member.ID, member.Revision, beads.UpdateOpts{Assignee: &owner})
	}()
	go func() { defer wg.Done(); <-start; _, workflowErr = launchOwnershipWorkflow(deps, input, a) }()
	close(start)
	wg.Wait()
	if (directErr == nil) == (workflowErr == nil) {
		t.Fatalf("want exactly one winner: direct=%v workflow=%v", directErr, workflowErr)
	}
}

type ownershipCreateFailure struct{ *beads.MemStore }

func (s ownershipCreateFailure) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, errors.New("materialization failed")
}

func TestExecutionOwnershipFailedLaunchReleasesInput(t *testing.T) {
	deps, member, input, a := executionOwnershipFixture(t)
	deps.GraphStore = ownershipCreateFailure{beads.NewMemStore()}
	if _, err := launchOwnershipWorkflow(deps, input, a); err == nil {
		t.Fatal("want materialization failure")
	}
	b, err := deps.Store.Get(member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee != "" || b.Status != member.Status {
		t.Fatalf("failed launch retained ownership: %+v", b)
	}
	opts := SlingOpts{BeadOrFormula: member.ID, Target: config.Agent{Name: "planner"}, NoFormula: true}
	if _, err := DoSling(opts, deps, deps.Store); err != nil {
		t.Fatalf("direct dispatch after failed launch: %v", err)
	}
}

func TestExecutionOwnershipConcurrentDispatch(t *testing.T) {
	deps, member, input, a := executionOwnershipFixture(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var directErr, workflowErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, directErr = DoSling(SlingOpts{BeadOrFormula: member.ID, Target: config.Agent{Name: "planner"}, NoFormula: true}, deps, deps.Store)
	}()
	go func() { defer wg.Done(); <-start; _, workflowErr = launchOwnershipWorkflow(deps, input, a) }()
	close(start)
	wg.Wait()
	if (directErr == nil) == (workflowErr == nil) {
		t.Fatalf("want exactly one dispatch owner: direct=%v workflow=%v", directErr, workflowErr)
	}
}

func TestExecutionOwnershipBatchCannotStealWorkflowInput(t *testing.T) {
	deps, _, input, a := executionOwnershipFixture(t)
	if _, err := launchOwnershipWorkflow(deps, input, a); err != nil {
		t.Fatal(err)
	}
	result, err := DoSlingBatch(SlingOpts{BeadOrFormula: input.ID, Target: config.Agent{Name: "planner"}, NoFormula: true, Force: true}, deps, deps.Store)
	if err == nil || result.Routed != 0 || result.Failed != 1 {
		t.Fatalf("batch dispatch = %+v, %v", result, err)
	}
}

func TestExecutionOwnershipFailedDirectRouteRestoresInput(t *testing.T) {
	deps, member, input, a := executionOwnershipFixture(t)
	deps.Runner = func(string, string, map[string]string) (string, error) { return "", errors.New("route failed") }
	if _, err := DoSling(SlingOpts{BeadOrFormula: member.ID, Target: config.Agent{Name: "planner"}, NoFormula: true}, deps, deps.Store); err == nil {
		t.Fatal("want route failure")
	}
	b, err := deps.Store.Get(member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee != "" || b.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("failed direct route retained ownership: %+v", b)
	}
	if _, err := launchOwnershipWorkflow(deps, input, a); err != nil {
		t.Fatal(err)
	}
}

type ownershipPromotionFailure struct{ *beads.MemStore }

func (s ownershipPromotionFailure) Update(id string, opts beads.UpdateOpts) error {
	if opts.Status != nil && *opts.Status == "in_progress" {
		return errors.New("promotion failed")
	}
	return s.MemStore.Update(id, opts)
}

func TestExecutionOwnershipFailedStartReleasesInput(t *testing.T) {
	deps, member, _, a := executionOwnershipFixture(t)
	deps.GraphStore = ownershipPromotionFailure{beads.NewMemStore()}
	if _, err := DoSling(SlingOpts{BeadOrFormula: member.ID, Target: a, OnFormula: "graph-work"}, deps, deps.Store); err == nil {
		t.Fatal("want promotion failure")
	}
	b, err := deps.Store.Get(member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee != "" {
		t.Fatalf("failed start retained workflow ownership: %+v", b)
	}
	if roots := liveGraphV2Roots(t, deps.GraphStore); len(roots) != 0 {
		t.Fatalf("failed start left executable roots: %+v", roots)
	}
}
