package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
)

// stampedWorktreeEvidence is the complete managed-worktree evidence a
// provisioner publishes onto one attempt after `gc worktree ensure`: the
// agreeing work dir pair, the branch and the eight ownership keys. Its
// provenance names that attempt, so a clone carrying it can never verify.
func stampedWorktreeEvidence() map[string]string {
	return map[string]string{
		beadmeta.WorkDirMetadataKey:            "/wt/r1/gc-a",
		beadmeta.LegacyWorkDirMetadataKey:      "/wt/r1/gc-a",
		beadmeta.WorkBranchMetadataKey:         "work/gc-a",
		beadmeta.WorktreeRepoMetadataKey:       "/rigs/r1",
		beadmeta.WorktreeRootMetadataKey:       "/wt/r1",
		beadmeta.WorktreeBaseRefMetadataKey:    "main",
		beadmeta.WorktreeBaseSHAMetadataKey:    "4a43c56500000000000000000000000000000000",
		beadmeta.WorktreeCreatorMetadataKey:    "provisioner",
		beadmeta.WorktreeOwnerMetadataKey:      "provisioner",
		beadmeta.WorktreeGenerationMetadataKey: "1",
		beadmeta.WorktreeLifecycleMetadataKey:  "active",
	}
}

func withStampedWorktreeEvidence(meta map[string]string) map[string]string {
	for key, value := range stampedWorktreeEvidence() {
		meta[key] = value
	}
	return meta
}

// assertNoWorktreeEvidence fails for every managed-worktree evidence key a
// cloned attempt still carries.
func assertNoWorktreeEvidence(t *testing.T, label string, meta map[string]string) {
	t.Helper()
	for key := range stampedWorktreeEvidence() {
		if value, ok := meta[key]; ok {
			t.Errorf("%s: carries %s=%q from the attempt it was cloned from; want it born unbound", label, key, value)
		}
	}
}

// Every clone path strips a stamped attempt's managed-worktree evidence: the
// provenance names the predecessor, so the controller would refuse to seat the
// clone and every seat outside that worktree would leave it.
func TestRetryAttemptCloneOfStampedAttemptIsBornUnbound(t *testing.T) {
	t.Parallel()

	meta := withStampedWorktreeEvidence(map[string]string{beadmeta.StepIDMetadataKey: "build"})
	clearRetryEphemera(meta)
	assertNoWorktreeEvidence(t, "clearRetryEphemera", meta)

	prev := beads.Bead{ID: "gc-a", Title: "build", Type: "task", Metadata: withStampedWorktreeEvidence(map[string]string{
		beadmeta.StepIDMetadataKey: "build",
	})}
	clone := retryAttemptBead(prev, "gc-logical", "build.run.2", 2, (*config.City)(nil))
	assertNoWorktreeEvidence(t, "retryAttemptBead", clone.Metadata)
}

// A work dir and branch with no ownership evidence are not a managed worktree:
// a drain member's directory or a claim's ambient branch stamp. A clone keeps
// them, as it always has.
func TestRetryAttemptCloneKeepsAnUnmanagedWorkDir(t *testing.T) {
	t.Parallel()

	prev := beads.Bead{ID: "gc-a", Title: "build", Type: "task", Metadata: map[string]string{
		beadmeta.WorkDirMetadataKey:       "/rigs/r1/frontend",
		beadmeta.LegacyWorkDirMetadataKey: "/rigs/r1/frontend",
		beadmeta.WorkBranchMetadataKey:    "main",
	}}
	clone := retryAttemptBead(prev, "gc-logical", "build.run.2", 2, (*config.City)(nil))
	for key, want := range prev.Metadata {
		if got := clone.Metadata[key]; got != want {
			t.Errorf("clone %s = %q, want %q kept", key, got, want)
		}
	}
}

// A clone of an attempt with partial ownership evidence (the shape
// worktreeSpecForBead refuses) is born unbound too, not born malformed.
func TestRetryAttemptCloneOfPartialEvidenceIsBornUnbound(t *testing.T) {
	t.Parallel()

	prev := beads.Bead{ID: "gc-a", Title: "build", Type: "task", Metadata: map[string]string{
		beadmeta.WorkDirMetadataKey:            "/wt/r1/gc-a",
		beadmeta.WorkBranchMetadataKey:         "work/gc-a",
		beadmeta.WorktreeGenerationMetadataKey: "1",
	}}
	clone := retryAttemptBead(prev, "gc-logical", "build.run.2", 2, (*config.City)(nil))
	assertNoWorktreeEvidence(t, "retryAttemptBead", clone.Metadata)
}

// The legacy retry-eval path, end to end: a transient failure of a stamped run
// mints run 2 with none of run 1's managed-worktree evidence.
func TestProcessRetryEvalCloneOfStampedRunIsBornUnbound(t *testing.T) {
	t.Parallel()

	store := newBdClosePolicyStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "build",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.build",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "hard_fail",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "build attempt 1",
		Type:     "task",
		Status:   "closed",
		Assignee: "worker-2",
		Labels:   []string{"pool:worker"},
		Metadata: withStampedWorktreeEvidence(map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.build.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.outcome":         "fail",
			"gc.failure_class":   "transient",
			"gc.failure_reason":  "rate_limited",
		}),
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "build eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.build.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	result, err := ProcessControl(beadstest.Graph(store), eval1, ProcessOptions{
		Work:           beadstest.Work(store),
		RecycleSession: func(beads.Bead) error { return nil },
	})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval transient): %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry", result)
	}
	open, err := store.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	var run2 beads.Bead
	for _, bead := range open {
		if bead.Metadata["gc.step_ref"] == "demo.build.run.2" {
			run2 = bead
		}
	}
	if run2.ID == "" {
		t.Fatal("no run 2 minted")
	}
	assertNoWorktreeEvidence(t, "run 2", run2.Metadata)
}

// The legacy Ralph clone paths strip it from the subject, every member and
// the check, through both the store-create and the graph-apply path.
func TestRalphRetryClonesOfStampedIterationAreBornUnbound(t *testing.T) {
	t.Parallel()

	t.Run("appendRalphRetryLegacy", func(t *testing.T) {
		t.Parallel()
		store := beads.NewMemStore()
		workflow := mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "workflow",
			Type:     "task",
			Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
		})
		logical := mustCreateWorkflowBead(t, store, beads.Bead{
			Title: "review-loop",
			Type:  "task",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:       beadmeta.KindRalph,
				beadmeta.StepIDMetadataKey:     "review-loop",
				beadmeta.RootBeadIDMetadataKey: workflow.ID,
			},
		})
		newBead := func(desc ralphIterationOneBead) beads.Bead {
			meta := withStampedWorktreeEvidence(desc.metadata(workflow.ID))
			if desc.kind == beadmeta.KindScope || desc.kind == beadmeta.KindCheck {
				meta[beadmeta.LogicalBeadIDMetadataKey] = logical.ID
			}
			return mustCreateWorkflowBead(t, store, beads.Bead{Title: desc.title, Type: "task", Metadata: meta})
		}
		subject := newBead(ralphIterOneSubject)
		member := newBead(ralphIterOnePlainMember)
		check := newBead(ralphIterOneCheck)
		mapping, err := appendRalphRetryLegacy(beadstest.Graph(store), logical.ID, subject, check,
			map[string]beads.Bead{subject.ID: subject, member.ID: member},
			1, 2, "review-loop.iteration.1", "review-loop.iteration.2", nil)
		if err != nil {
			t.Fatalf("appendRalphRetryLegacy: %v", err)
		}
		for label, oldID := range map[string]string{"subject": subject.ID, "member": member.ID, "check": check.ID} {
			newID := mapping[oldID]
			if newID == "" {
				t.Fatalf("no clone mapped for the %s", label)
			}
			assertNoWorktreeEvidence(t, label, mustGetBead(t, store, newID).Metadata)
		}
	})

	t.Run("buildRalphRetryGraphNode", func(t *testing.T) {
		t.Parallel()
		for label, desc := range map[string]ralphIterationOneBead{
			"subject": ralphIterOneSubject,
			"member":  ralphIterOnePlainMember,
			"check":   ralphIterOneCheck,
		} {
			old := beads.Bead{ID: desc.stepRef, Ref: desc.stepRef, Metadata: withStampedWorktreeEvidence(desc.metadata("workflow"))}
			node := buildRalphRetryGraphNode(old, "logical", "review-loop.iteration.1", "review-loop.iteration.2", 1, 2,
				map[string]bool{old.ID: true}, nil)
			assertNoWorktreeEvidence(t, label, node.Metadata)
		}
	})
}
