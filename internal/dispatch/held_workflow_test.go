package dispatch

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/molecule"
)

// A workflow launched for a blocked bead carries that bead's blocker edges on
// every bead but its teardown tail, its controls included
// (molecule.Options.BlockedBy), and a convoy drain holds an item's beads on a
// predecessor item's root the same way. Such an edge gates readiness only. The
// tests in this file pin that the control dispatcher grades and resolves a
// control by its own workflow alone, whatever else the control blocks on.

// outsideBlocker is a blocker outside a workflow that the workflow's beads
// also block on.
type outsideBlocker struct {
	name string
	// create returns the id the edges name, creating the bead behind it, if
	// any, in store.
	create func(t *testing.T, store beads.Store) string
}

// outsideBlockers are the blockers a held launch copies from its source bead
// that a store lets the workflow run past: a prerequisite and another run's
// step, each closed failed, which readiness lets through, and an external ref
// and an id the store does not hold, which bd never blocks on.
func outsideBlockers() []outsideBlocker {
	return []outsideBlocker{
		{name: "a prerequisite closed failed", create: func(t *testing.T, store beads.Store) string {
			return mustCreateWorkflowBead(t, store, beads.Bead{
				Title:    "prerequisite",
				Type:     "task",
				Status:   "closed",
				Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail},
			}).ID
		}},
		{name: "a step of another workflow closed failed", create: func(t *testing.T, store beads.Store) string {
			return mustCreateWorkflowBead(t, store, beads.Bead{
				Title:  "another run's step",
				Type:   "task",
				Status: "closed",
				Metadata: map[string]string{
					beadmeta.RootBeadIDMetadataKey: "wf-other",
					beadmeta.OutcomeMetadataKey:    beadmeta.OutcomeFail,
				},
			}).ID
		}},
		{name: "an external ref", create: func(*testing.T, beads.Store) string { return "external:other-rig:shipped" }},
		{name: "an id the store does not hold", create: func(*testing.T, beads.Store) string { return "other-404" }},
	}
}

func mustCreateHeldTestWorkflowRoot(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	return mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	})
}

func mustCreateHeldTestFinalizer(t *testing.T, store beads.Store, rootID string) beads.Bead {
	t.Helper()
	return mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindWorkflowFinalize,
			beadmeta.RootBeadIDMetadataKey: rootID,
		},
	})
}

// TestProcessWorkflowFinalizeGradesOnlyItsOwnWorkflow pins that
// workflow-finalize settles a run by its own steps: an edge onto a blocker
// outside the workflow neither fails a run whose steps all passed nor fails
// finalization on every pass.
func TestProcessWorkflowFinalizeGradesOnlyItsOwnWorkflow(t *testing.T) {
	t.Parallel()

	for _, outside := range outsideBlockers() {
		t.Run(outside.name, func(t *testing.T) {
			t.Parallel()

			store := beads.NewMemStore()
			workflow := mustCreateHeldTestWorkflowRoot(t, store)
			step := mustCreateWorkflowBead(t, store, beads.Bead{
				Title:  "work",
				Type:   "task",
				Status: "closed",
				Metadata: map[string]string{
					beadmeta.RootBeadIDMetadataKey: workflow.ID,
					beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				},
			})
			finalizer := mustCreateHeldTestFinalizer(t, store, workflow.ID)
			outsideID := outside.create(t, store)
			// The held launch's edge goes in first, so a reader that took
			// every blocker for the finalizer's own would meet it first.
			mustDepAdd(t, store, finalizer.ID, outsideID, "blocks")
			mustDepAdd(t, store, finalizer.ID, step.ID, "blocks")
			mustDepAdd(t, store, workflow.ID, finalizer.ID, "tracks")

			result, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
			if err != nil {
				t.Fatalf("ProcessControl(workflow-finalize): %v", err)
			}
			if !result.Processed || result.Action != "workflow-pass" {
				t.Fatalf("result = %+v, want processed workflow-pass: %s is outside the workflow", result, outsideID)
			}
			root := mustGetBead(t, store, workflow.ID)
			if root.Status != "closed" || root.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
				t.Fatalf("root status/outcome = %q/%q, want closed/pass", root.Status, root.Metadata[beadmeta.OutcomeMetadataKey])
			}
		})
	}
}

// TestProcessWorkflowFinalizeFailsOnItsOwnStepAndNamesIt is the control for
// TestProcessWorkflowFinalizeGradesOnlyItsOwnWorkflow: a failed step of the
// workflow still fails the run, and the failure names that step, not a failed
// blocker outside the workflow listed before it.
func TestProcessWorkflowFinalizeFailsOnItsOwnStepAndNamesIt(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	workflow := mustCreateHeldTestWorkflowRoot(t, store)
	prerequisite := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prerequisite",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.FailureReasonMetadataKey: "prerequisite_failed",
		},
	})
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "work",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:    workflow.ID,
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.FailureReasonMetadataKey: "work_failed",
			beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
		},
	})
	finalizer := mustCreateHeldTestFinalizer(t, store, workflow.ID)
	mustDepAdd(t, store, finalizer.ID, prerequisite.ID, "blocks")
	mustDepAdd(t, store, finalizer.ID, step.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "tracks")

	stamp := resolveFinalizeFailureDiagnostics(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
	if got := stamp[beadmeta.FailureSubjectMetadataKey]; got != step.ID {
		t.Fatalf("failure subject = %q, want the workflow's own failed step %s, not the prerequisite %s outside it", got, step.ID, prerequisite.ID)
	}
	if got := stamp[beadmeta.FailureReasonMetadataKey]; got != "work_failed" {
		t.Fatalf("failure reason = %q, want work_failed", got)
	}

	result, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("result = %+v, want processed workflow-fail", result)
	}
	if got := mustGetBead(t, store, workflow.ID).Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomeFail {
		t.Fatalf("root outcome = %q, want fail", got)
	}
}

// TestProcessFanoutSpawnedGradesOnlyItsOwnWorkflow pins the same rule for a
// spawned fanout, which settles by its source and children: an edge onto a
// blocker outside the workflow never grades it.
func TestProcessFanoutSpawnedGradesOnlyItsOwnWorkflow(t *testing.T) {
	t.Parallel()

	for _, outside := range outsideBlockers() {
		t.Run(outside.name, func(t *testing.T) {
			t.Parallel()

			store := beads.NewMemStore()
			workflow := mustCreateHeldTestWorkflowRoot(t, store)
			source := mustCreateWorkflowBead(t, store, beads.Bead{
				Title:  "survey",
				Type:   "task",
				Status: "closed",
				Metadata: map[string]string{
					beadmeta.RootBeadIDMetadataKey: workflow.ID,
					beadmeta.StepRefMetadataKey:    "demo.survey",
					beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				},
			})
			child := mustCreateWorkflowBead(t, store, beads.Bead{
				Title:  "review",
				Type:   "task",
				Status: "closed",
				Metadata: map[string]string{
					beadmeta.RootBeadIDMetadataKey: workflow.ID,
					beadmeta.StepRefMetadataKey:    "demo.survey.item.1.review",
					beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				},
			})
			fanout := mustCreateWorkflowBead(t, store, beads.Bead{
				Title: "Expand fanout for survey",
				Type:  "task",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:        beadmeta.KindFanout,
					beadmeta.RootBeadIDMetadataKey:  workflow.ID,
					beadmeta.ControlForMetadataKey:  "demo.survey",
					beadmeta.FanoutStateMetadataKey: beadmeta.SpawnStateSpawned,
				},
			})
			outsideID := outside.create(t, store)
			mustDepAdd(t, store, fanout.ID, outsideID, "blocks")
			mustDepAdd(t, store, fanout.ID, source.ID, "blocks")
			mustDepAdd(t, store, fanout.ID, child.ID, "blocks")

			result, err := ProcessControl(beadstest.Graph(store), fanout, ProcessOptions{Work: beadstest.Work(store)})
			if err != nil {
				t.Fatalf("ProcessControl(fanout spawned): %v", err)
			}
			if !result.Processed || result.Action != "fanout-pass" {
				t.Fatalf("result = %+v, want processed fanout-pass: %s is outside the workflow", result, outsideID)
			}
			after := mustGetBead(t, store, fanout.ID)
			if after.Status != "closed" || after.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
				t.Fatalf("fanout status/outcome = %q/%q, want closed/pass", after.Status, after.Metadata[beadmeta.OutcomeMetadataKey])
			}
		})
	}
}

// TestControlSubjectSkipsBlockersThatNameNoBead pins that a control resolving
// its subject skips an external ref and an id the store does not hold, as it
// skips a bead of another workflow (TestResolveBlockingSubjectIDSkipsCrossRootDrainGate),
// rather than failing on every pass. One such edge beside the subject leaves
// the subject alone, which is taken without a read; two leave it among targets
// that must be read.
func TestControlSubjectSkipsBlockersThatNameNoBead(t *testing.T) {
	t.Parallel()

	resolvers := []struct {
		kind    string
		resolve func(beads.GraphStore, beads.Bead) (string, error)
	}{
		{kind: beadmeta.KindRalph, resolve: resolveBlockingSubjectID},
		{kind: beadmeta.KindScopeCheck, resolve: resolveScopeCheckSubjectID},
	}
	outside := []struct {
		name    string
		targets []string
	}{
		{name: "an external ref", targets: []string{"external:other-rig:shipped"}},
		{name: "an external ref and an id the store does not hold", targets: []string{"external:other-rig:shipped", "other-404"}},
	}
	for _, resolver := range resolvers {
		for _, edges := range outside {
			targets := edges.targets
			t.Run(resolver.kind+" beside "+edges.name, func(t *testing.T) {
				t.Parallel()

				store := beads.NewMemStore()
				subject := mustCreateWorkflowBead(t, store, beads.Bead{
					Title:    "step",
					Type:     "task",
					Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "wf-held"},
				})
				control := mustCreateWorkflowBead(t, store, beads.Bead{
					Title: "control",
					Type:  "task",
					Metadata: map[string]string{
						beadmeta.KindMetadataKey:       resolver.kind,
						beadmeta.RootBeadIDMetadataKey: "wf-held",
					},
				})
				for _, target := range targets {
					mustDepAdd(t, store, control.ID, target, "blocks")
				}
				mustDepAdd(t, store, control.ID, subject.ID, "blocks")

				got, err := resolver.resolve(beadstest.Graph(store), control)
				if err != nil {
					t.Fatalf("resolving the subject: %v", err)
				}
				if got != subject.ID {
					t.Fatalf("subject = %s, want the control's own step %s beside %v", got, subject.ID, targets)
				}
			})
		}
	}
}

// TestCopyRetryDepsCopiesEdgesThatNameNoBeadAsWritten pins that a ralph retry
// clone keeps an edge onto an external ref or an id the store does not hold
// as written, as the graph-apply retry path does
// (appendRalphRetryGraphEdges), rather than failing the retry on a read: a
// held workflow's beads carry such edges from their source bead. The resume
// check looks for every edge the copy writes.
func TestCopyRetryDepsCopiesEdgesThatNameNoBeadAsWritten(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	attemptStep := mustCreateWorkflowBead(t, store, beads.Bead{Title: "iteration step", Type: "task"})
	clone := mustCreateWorkflowBead(t, store, beads.Bead{Title: "iteration step retry", Type: "task"})
	prerequisite := mustCreateWorkflowBead(t, store, beads.Bead{Title: "prerequisite", Type: "task"})
	edges := []beads.Dep{
		{DependsOnID: "external:other-rig:shipped", Type: "blocks"},
		{DependsOnID: "other-404", Type: "waits-for"},
		{DependsOnID: prerequisite.ID, Type: "blocks"},
	}
	for _, edge := range edges {
		mustDepAdd(t, store, attemptStep.ID, edge.DependsOnID, edge.Type)
	}
	graph := beadstest.Graph(store)

	present, err := copiedDepsPresent(graph, attemptStep.ID, clone.ID, map[string]string{})
	if err != nil {
		t.Fatalf("copiedDepsPresent before the copy: %v", err)
	}
	if present {
		t.Fatal("copiedDepsPresent = true before the copy, want false: it must look for every edge the copy writes")
	}

	if err := copyRetryDeps(graph, attemptStep.ID, clone.ID, map[string]string{}); err != nil {
		t.Fatalf("copyRetryDeps: %v", err)
	}
	deps, err := store.DepList(clone.ID, "down")
	if err != nil {
		t.Fatalf("DepList(clone): %v", err)
	}
	for _, edge := range edges {
		want := beads.Dep{IssueID: clone.ID, DependsOnID: edge.DependsOnID, Type: edge.Type}
		if !slices.Contains(deps, want) {
			t.Errorf("clone has no %s edge on %s; deps=%v", edge.Type, edge.DependsOnID, deps)
		}
	}

	present, err = copiedDepsPresent(graph, attemptStep.ID, clone.ID, map[string]string{})
	if err != nil {
		t.Fatalf("copiedDepsPresent after the copy: %v", err)
	}
	if !present {
		t.Fatal("copiedDepsPresent = false after the copy, want true")
	}
}

// TestHeldWorkflowSettlesByItsOwnStepsOnceReleased drives a workflow held on
// a blocker through the control dispatcher to settlement. Every control of it
// (the retry, the scope-check, workflow-finalize) carries the blocker's edge;
// once the store lets the workflow run, the run is graded by its own steps
// alone and settles pass, and its teardown runs after.
//
// Not parallel: cookTeardownMoleculeWith flips process-wide formula switches.
func TestHeldWorkflowSettlesByItsOwnStepsOnceReleased(t *testing.T) {
	t.Run("its prerequisite closed failed", func(t *testing.T) {
		store := beads.NewMemStore()
		prerequisite := mustCreateWorkflowBead(t, store, beads.Bead{Title: "prerequisite", Type: "task"})
		rootID := cookTeardownMoleculeWith(t, store, molecule.Options{BlockedBy: []molecule.Blocker{{ID: prerequisite.ID}}})

		runMolecule(t, store, rootID, beadmeta.OutcomePass)
		if root := mustGetBead(t, store, rootID); root.Status != "open" {
			t.Fatalf("root status = %q while %s is open, want open: the workflow waits for it:\n%s", root.Status, prerequisite.ID, memberReport(t, store, rootID))
		}

		// Readiness lets a blocker closed failed through, so the run goes on.
		if err := updateMetadataAndClose(beadstest.Graph(store), prerequisite.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}); err != nil {
			t.Fatalf("closing the prerequisite: %v", err)
		}
		runMolecule(t, store, rootID, beadmeta.OutcomePass)
		assertHeldWorkflowSettledPass(t, store, rootID)
	})

	t.Run("it waits on an external ref", func(t *testing.T) {
		store := beads.NewMemStore()
		rootID := cookTeardownMoleculeWith(t, store, molecule.Options{})
		holdOnExternalRef(t, store, rootID, "external:other-rig:shipped")

		runMolecule(t, store, rootID, beadmeta.OutcomePass)
		assertHeldWorkflowSettledPass(t, store, rootID)
	})
}

// holdOnExternalRef wires ref onto every bead of the workflow rooted at rootID
// but its teardown tail, as bd's graph apply writes the BlockedBy hold of a
// source bead blocked on an external ref: a typed blocks edge, which bd never
// blocks on. (A store without graph apply takes the hold through each bead's
// Needs, which reads "external:<rig>:<capability>" as an edge of type
// "external", so the edge is wired here rather than through the cook.)
func holdOnExternalRef(t *testing.T, store beads.Store, rootID, ref string) {
	t.Helper()
	members, err := molecule.ListSubtree(beadstest.GraphMolecule(store), rootID)
	if err != nil {
		t.Fatalf("listing workflow members: %v", err)
	}
	inTeardownTail := molecule.TeardownTailExclusionFromMembers(members)
	for _, member := range members {
		if !inTeardownTail(member) {
			mustDepAdd(t, store, member.ID, ref, "blocks")
		}
	}
}

func assertHeldWorkflowSettledPass(t *testing.T, store beads.Store, rootID string) {
	t.Helper()
	root := mustGetBead(t, store, rootID)
	if root.Status != "closed" || root.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("root status/outcome = %q/%q, want closed/pass: every step passed:\n%s",
			root.Status, root.Metadata[beadmeta.OutcomeMetadataKey], memberReport(t, store, rootID))
	}
	assertNoOpenMembers(t, store, rootID)
}
