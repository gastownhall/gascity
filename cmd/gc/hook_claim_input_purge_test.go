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

// seedPurgedInputWorkflow builds a graph.v2 workflow in store: a root naming
// an input convoy that tracked one work bead, an open step and an in-progress
// step under it, and a step that already closed with gc.outcome=pass.
func seedPurgedInputWorkflow(t *testing.T, store beads.Store) (root, open, inProgress, finished beads.Bead, convoyID string) {
	t.Helper()
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	root, err = store.Create(beads.Bead{Title: "mol", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   conv.ID,
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
	return root, open, inProgress, finished, conv.ID
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

// runPurgeClaim runs one `gc hook --claim` for worker-1 over ops.
func runPurgeClaim(t *testing.T, ops hookClaimOps) (stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &out, &errOut)
	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, errOut.String())
	}
	return out.String(), errOut.String()
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
	root, open, inProgress, finished, convoyID := seedPurgedInputWorkflow(t, store)
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
	_, stderr := runPurgeClaim(t, ops)

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
