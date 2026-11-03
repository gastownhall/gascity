package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphv2"
)

// purgeFixtureCityConfig is the city the purge fixtures' frame is cut over:
// its HQ namespace is the MemStore's minted "gc" prefix.
func purgeFixtureCityConfig() *config.City {
	return &config.City{Workspace: config.Workspace{Prefix: "gc"}}
}

// purgeFrameOverStores is the purge frame the claim path builds, over a work
// store and rig stores already open.
func purgeFrameOverStores(t *testing.T, cfg *config.City, work beads.Store, rigs map[string]beads.Store) workflowInputPurgeFrame {
	t.Helper()
	frame, err := workflowInputPurgeFrameOver(newWaitDependencyPlanReader(assembleResidencyTopology(cfg, work, rigs, nil, nil), false))
	if err != nil {
		t.Fatalf("workflowInputPurgeFrameOver: %v", err)
	}
	return frame
}

// seedInputWorkflow builds a graph.v2 workflow in store: a root naming
// convoyID as its input convoy, an open step and an in-progress step under it,
// and a step that already closed with gc.outcome=pass.
func seedInputWorkflow(t *testing.T, store beads.Store, convoyID string) (root, open, inProgress, finished beads.Bead) {
	t.Helper()
	root, err := store.Create(beads.Bead{Title: "mol", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   convoyID,
		beadmeta.RoutedToMetadataKey:        "worker",
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step := func(title string) beads.Bead {
		b, err := store.Create(beads.Bead{Title: title, Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.RoutedToMetadataKey:   "worker",
		}})
		if err != nil {
			t.Fatalf("create step %s: %v", title, err)
		}
		return b
	}
	open = step("open step")
	inProgress = step("in-progress step")
	if err := store.Update(inProgress.ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr("worker-2")}); err != nil {
		t.Fatalf("start step: %v", err)
	}
	finished = step("finished step")
	if err := store.SetMetadata(finished.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
		t.Fatalf("stamp finished step: %v", err)
	}
	if err := store.Close(finished.ID); err != nil {
		t.Fatalf("close finished step: %v", err)
	}
	return root, open, inProgress, finished
}

// seedSyntheticInputWorkflow builds a workflow over the single-item synthetic
// input convoy a graph.v2 invocation pours for target.
func seedSyntheticInputWorkflow(t *testing.T, store beads.Store) (target, conv, root, open beads.Bead) {
	t.Helper()
	target, err := store.Create(beads.Bead{Title: "the work", Type: "task"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	conv, err = graphv2.CreateSingleItemInputConvoy(store, target)
	if err != nil {
		t.Fatalf("pour input convoy: %v", err)
	}
	root, open, _, _ = seedInputWorkflow(t, store, conv.ID)
	return target, conv, root, open
}

// purgeClaimOps are the claim ops of a single-store city over store: the real
// verdict, proof and retirement, and stub claim-time writes that record which
// candidate was claimed.
func purgeClaimOps(t *testing.T, store beads.Store, frame workflowInputPurgeFrame, candidates []beads.Bead, claimed *string) hookClaimOps {
	t.Helper()
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			*claimed = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		InputVerdict: func(_ context.Context, _ string, _ []string, candidate beads.Bead, identityCandidates []string) (workflowInputVerdict, error) {
			return workflowInputDoneAcross(store, store, candidate, identityCandidates)
		},
		InputPurged: func(verdict workflowInputVerdict) ([]string, error) {
			return proveWorkflowInputPurged(frame, verdict)
		},
		RetirePurgedWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return retirePurgedWorkflow(store, rootID)
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			t.Fatalf("unexpected done-skip of %s", rootID)
			return nil
		},
		ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
			return nil, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, _, _ string) error { return nil },
		ResolveWorkBranch:  func(hookClaimWorkTree) string { return "" },
		StampWorkMeta: func(_ context.Context, _ string, _ []string, _, _ string, _ map[string]string) error {
			return nil
		},
		PublishRunMap: func(_, _ string, _ ...string) error { return nil },
		DrainAck:      func(io.Writer) error { return nil },
	}
}

// runPurgeClaim runs one `gc hook --claim` for worker-1 over ops, requires its
// exit code (1 is "no work"), and returns its stderr.
func runPurgeClaim(t *testing.T, ops hookClaimOps, wantCode int) string {
	t.Helper()
	var out, errOut bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &out, &errOut)
	if code != wantCode {
		t.Fatalf("doHookClaim = %d, want %d (stderr=%q)", code, wantCode, errOut.String())
	}
	return errOut.String()
}

func assertRetiredAsInputPurged(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != "closed" ||
			got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail ||
			got.Metadata[beadmeta.FailureClassMetadataKey] != beadmeta.FailureClassHard ||
			got.Metadata[beadmeta.FailureReasonMetadataKey] != beadmeta.FailureReasonInputPurged ||
			got.Metadata["close_reason"] != hookRetirePurgedWorkflowCloseReason {
			t.Fatalf("%s = %s %v; want closed fail/hard/input_purged", id, got.Status, got.Metadata)
		}
	}
}

// TestDoHookClaimRetiresWorkflowWhoseInputConvoyWasPurged is #8729: a purged
// input convoy reads as an empty membership, which the done-check fails open
// on, so every seat was re-served the workflow's steps forever. Once the
// convoy is proved absent over the whole city frame, the claim retires the
// root and its non-terminal steps as fail/hard/input_purged, serves none of
// them, and goes on to claim the next candidate.
func TestDoHookClaimRetiresWorkflowWhoseInputConvoyWasPurged(t *testing.T) {
	store := beads.NewMemStore()
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	convoyID := conv.ID
	root, open, inProgress, finished := seedInputWorkflow(t, store, convoyID)
	other, err := store.Create(beads.Bead{Title: "unrelated work", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	if err := store.Delete(convoyID); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	frame := purgeFrameOverStores(t, purgeFixtureCityConfig(), store, nil)

	claimed := ""
	ops := purgeClaimOps(t, store, frame, []beads.Bead{open, other}, &claimed)
	stderr := runPurgeClaim(t, ops, 0)

	if claimed != other.ID {
		t.Fatalf("claimed = %q, want the next candidate %s (stderr=%q)", claimed, other.ID, stderr)
	}
	assertRetiredAsInputPurged(t, store, root.ID, open.ID, inProgress.ID)
	if got, err := store.Get(finished.ID); err != nil || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("finished step = %v %v; want its pass outcome kept", got.Metadata, err)
	}
	if !strings.Contains(stderr, "input is purged") || !strings.Contains(stderr, convoyID) {
		t.Fatalf("stderr = %q; want the retirement and its evidence logged", stderr)
	}
}

// A purge of the tracked work, not the convoy, leaves the synthetic convoy
// tracking a dangling edge: membership is one unresolved placeholder. The
// placeholder and the target the convoy recorded are the evidence, and both
// proved absent retire the workflow.
func TestDoHookClaimRetiresWorkflowWhoseSyntheticInputTargetWasPurged(t *testing.T) {
	store := beads.NewMemStore()
	target, _, root, open := seedSyntheticInputWorkflow(t, store)
	if err := store.Delete(target.ID); err != nil {
		t.Fatalf("purge target: %v", err)
	}
	frame := purgeFrameOverStores(t, purgeFixtureCityConfig(), store, nil)

	claimed := ""
	stderr := runPurgeClaim(t, purgeClaimOps(t, store, frame, []beads.Bead{open}, &claimed), 1)

	if claimed != "" {
		t.Fatalf("claimed %s, a step of a workflow whose input is purged (stderr=%q)", claimed, stderr)
	}
	assertRetiredAsInputPurged(t, store, root.ID, open.ID)
	if !strings.Contains(stderr, target.ID) {
		t.Fatalf("stderr = %q; want the purged target named as evidence", stderr)
	}
}

// A purge that cascaded the tracks edge away leaves the synthetic convoy with
// no members at all. The target the convoy recorded in its metadata still
// names what it tracked, and that target proved absent retires the workflow.
func TestDoHookClaimRetiresWorkflowWhoseRecordedInputTargetWasPurgedWithItsEdge(t *testing.T) {
	store := beads.NewMemStore()
	target, conv, root, open := seedSyntheticInputWorkflow(t, store)
	if err := store.DepRemove(conv.ID, target.ID); err != nil {
		t.Fatalf("cascade the tracks edge: %v", err)
	}
	if err := store.Delete(target.ID); err != nil {
		t.Fatalf("purge target: %v", err)
	}
	frame := purgeFrameOverStores(t, purgeFixtureCityConfig(), store, nil)

	claimed := ""
	stderr := runPurgeClaim(t, purgeClaimOps(t, store, frame, []beads.Bead{open}, &claimed), 1)

	if claimed != "" {
		t.Fatalf("claimed %s, a step of a workflow whose input is purged (stderr=%q)", claimed, stderr)
	}
	assertRetiredAsInputPurged(t, store, root.ID, open.ID)
	if !strings.Contains(stderr, target.ID) {
		t.Fatalf("stderr = %q; want the recorded target named as evidence", stderr)
	}
}
