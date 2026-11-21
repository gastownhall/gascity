package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// graphCancelRun is a standalone graph-owned run handed out whole: its root is
// routed to the worker, carries its own lifecycle and whatever cancel state a
// test gives it, and holds an ordinary step and a teardown step.
type graphCancelRun struct {
	root, step, teardown beads.Bead
}

func seedGraphCancelRun(f *hookWorkflowStopFixture, lifecycle string, rootExtra map[string]string) graphCancelRun {
	meta := map[string]string{
		beadmeta.KindMetadataKey:           beadmeta.KindWorkflow,
		"gc.formula_contract":              "graph.v2",
		beadmeta.RoutedToMetadataKey:       "worker",
		beadmeta.InputLifecycleMetadataKey: lifecycle,
	}
	for k, v := range rootExtra {
		meta[k] = v
	}
	var run graphCancelRun
	run.root = f.create(beads.Bead{Title: "run", Type: "molecule", Metadata: meta})
	run.step = f.step("report", run.root.ID, map[string]string{beadmeta.StepIDMetadataKey: "report"})
	run.teardown = f.step("cleanup", run.root.ID, map[string]string{
		beadmeta.StepIDMetadataKey:    "cleanup",
		beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
	})
	f.served = []string{run.root.ID}
	return run
}

// graphCancelOps is the fixture's ops with an input check that reports state
// for every candidate's root and the lifecycle that root carries now, and a
// retirement that fails the test: a graph-owned run is never input-retired, so
// any sweep these tests see is the cancel recovery's.
func graphCancelOps(f *hookWorkflowStopFixture, state workflowInputState, verdictErr error) hookClaimOps {
	ops := f.ops()
	ops.InputVerdict = func(_ context.Context, _ string, _ []string, candidate beads.Bead, _ []string, _ workflowInputAbsenceProver) (workflowInputVerdict, error) {
		if verdictErr != nil {
			return workflowInputVerdict{}, verdictErr
		}
		rootID := strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID == "" {
			rootID = candidate.ID
		}
		root := f.get(rootID)
		return workflowInputVerdict{RootID: rootID, State: state, Lifecycle: root.Metadata[beadmeta.InputLifecycleMetadataKey]}, nil
	}
	ops.RetireWorkflow = func(_ context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
		f.t.Fatalf("RetireWorkflow(%s) ran; only the cancel recovery may sweep this run", verdict.RootID)
		return nil
	}
	return ops
}

// runHookClaimWith invokes one `gc hook --claim` with ops and returns its exit
// code, the bead it handed out (empty when none) and its stderr.
func runHookClaimWith(f *hookWorkflowStopFixture, ops hookClaimOps) (int, string, string) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", f.t.TempDir(), hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}, ops, &stdout, &stderr)
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		var result hookClaimJSONResult
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			f.t.Fatalf("decode claim result %q: %v", line, err)
		}
		if result.Action == "work" {
			return code, result.BeadID, stderr.String()
		}
	}
	return code, "", stderr.String()
}

func (f *hookWorkflowStopFixture) wantOpen(ids ...string) {
	f.t.Helper()
	for _, id := range ids {
		if got := f.get(id); got.Status == "closed" {
			f.t.Fatalf("%s closed with outcome %q, want it left open", id, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
}

// TestHookClaimRecoversFailedCancelOfGraphRootWithSettledInput: a graph-owned
// run whose cancel stamped intent (or the canceled outcome) but never wound it
// down, and whose input is already terminal, has no other backstop — graph
// ownership exempts it from input retirement. Claiming its root finishes the
// cancel: the run closes canceled, the teardown tail is spared, and the root
// is not handed out.
func TestHookClaimRecoversFailedCancelOfGraphRootWithSettledInput(t *testing.T) {
	for name, cancel := range map[string]map[string]string{
		"cancel_requested": {beadmeta.CancelRequestedMetadataKey: "true"},
		"outcome_canceled": {beadmeta.OutcomeMetadataKey: beadmeta.OutcomeCanceled},
	} {
		t.Run(name, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			run := seedGraphCancelRun(f, beadmeta.InputLifecycleGraph, cancel)

			code, got, stderr := runHookClaimWith(f, graphCancelOps(f, workflowInputComplete, nil))
			if code != 0 || got != "" {
				t.Fatalf("doHookClaim = %d handing out %q, want 0 handing out nothing; stderr=%s", code, got, stderr)
			}
			f.wantClosed(run.root.ID, beadmeta.OutcomeCanceled)
			f.wantClosed(run.step.ID, beadmeta.OutcomeCanceled)
			f.wantOpen(run.teardown.ID)
			if marker := f.get(run.root.ID).Metadata[beadmeta.CancelRequestedMetadataKey]; marker != "true" {
				t.Fatalf("root gc.cancel_requested = %q, want true", marker)
			}
			if !strings.Contains(stderr, run.root.ID) || !strings.Contains(stderr, "canceled") {
				t.Fatalf("stderr = %q, want a diagnostic naming the recovered root %s", stderr, run.root.ID)
			}
		})
	}
}

// TestHookClaimWithholdsCanceledGraphRootWithUnsettledInput: while the input
// is still pending, or cannot be read, a canceled graph root is withheld
// before any claim and nothing of its run is closed.
func TestHookClaimWithholdsCanceledGraphRootWithUnsettledInput(t *testing.T) {
	cases := map[string]struct {
		state workflowInputState
		err   error
	}{
		"pending":    {state: workflowInputPending},
		"read_error": {err: errors.New("input store unavailable")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			run := seedGraphCancelRun(f, beadmeta.InputLifecycleGraph, map[string]string{beadmeta.CancelRequestedMetadataKey: "true"})

			code, got, stderr := runHookClaimWith(f, graphCancelOps(f, tc.state, tc.err))
			if code != 0 || got != "" {
				t.Fatalf("doHookClaim = %d handing out %q, want 0 handing out nothing; stderr=%s", code, got, stderr)
			}
			f.wantNeverClaimed(run.root.ID)
			f.wantOpen(run.root.ID, run.step.ID, run.teardown.ID)
			if !strings.Contains(stderr, run.root.ID) {
				t.Fatalf("stderr = %q, want a diagnostic naming the withheld root %s", stderr, run.root.ID)
			}
		})
	}
}

// TestHookClaimServesLiveGraphRootWithSettledInput: a graph root that was not
// canceled is handed out as before once its input settles; settlement alone
// never triggers the cancel recovery.
func TestHookClaimServesLiveGraphRootWithSettledInput(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	run := seedGraphCancelRun(f, beadmeta.InputLifecycleGraph, nil)

	code, got, stderr := runHookClaimWith(f, graphCancelOps(f, workflowInputComplete, nil))
	if code != 0 || got != run.root.ID {
		t.Fatalf("doHookClaim = %d handing out %q, want 0 handing out %s; stderr=%s", code, got, run.root.ID, stderr)
	}
	f.wantOpen(run.step.ID, run.teardown.ID)
}

// TestHookClaimLeavesCanceledExternalRootToExistingRules: the recovery is
// bounded to graph-owned runs. A canceled standalone root of an
// external-lifecycle run whose input is still pending keeps the existing
// claim behavior and none of its run is swept.
func TestHookClaimLeavesCanceledExternalRootToExistingRules(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	run := seedGraphCancelRun(f, beadmeta.InputLifecycleExternal, map[string]string{beadmeta.CancelRequestedMetadataKey: "true"})

	code, got, stderr := runHookClaimWith(f, graphCancelOps(f, workflowInputPending, nil))
	if code != 0 || got != run.root.ID {
		t.Fatalf("doHookClaim = %d handing out %q, want 0 handing out %s; stderr=%s", code, got, run.root.ID, stderr)
	}
	f.wantOpen(run.step.ID, run.teardown.ID)
}

// TestHookClaimNoRecoveryWithoutCancelIntent: a graph root whose
// gc.cancel_requested marker is blank carries no cancel intent, so settled
// input alone serves the root and closes nothing of its run.
func TestHookClaimNoRecoveryWithoutCancelIntent(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	run := seedGraphCancelRun(f, beadmeta.InputLifecycleGraph, map[string]string{beadmeta.CancelRequestedMetadataKey: " "})

	code, got, stderr := runHookClaimWith(f, graphCancelOps(f, workflowInputComplete, nil))
	if code != 0 || got != run.root.ID {
		t.Fatalf("doHookClaim = %d handing out %q, want 0 handing out %s; stderr=%s", code, got, run.root.ID, stderr)
	}
	f.wantOpen(run.root.ID, run.step.ID, run.teardown.ID)
}

// TestHookClaimConvergedCancelRootIsWithheldNotReclosed: a candidate whose
// graph root already closed under its cancel (read stale by the poll) is
// withheld, and the stop gate neither re-closes the root nor sweeps the run.
func TestHookClaimConvergedCancelRootIsWithheldNotReclosed(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	run := seedGraphCancelRun(f, beadmeta.InputLifecycleGraph, map[string]string{beadmeta.CancelRequestedMetadataKey: "true"})
	f.closeAs(run.root.ID, beadmeta.OutcomeCanceled, map[string]string{"close_reason": "converged"})

	var stderr bytes.Buffer
	opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}}
	withheld := hookClaimWorkflowStop(context.Background(), run.root, "worker-1", opts, graphCancelOps(f, workflowInputComplete, nil), t.TempDir(), &stderr)
	if !withheld {
		t.Fatalf("stop gate did not withhold the converged-canceled root; stderr=%s", stderr.String())
	}
	if reason := f.get(run.root.ID).Metadata["close_reason"]; reason != "converged" {
		t.Fatalf("root close_reason = %q, want the converged close left as is", reason)
	}
	f.wantOpen(run.step.ID, run.teardown.ID)
	if !strings.Contains(stderr.String(), run.root.ID) {
		t.Fatalf("stderr = %q, want a diagnostic naming the withheld root %s", stderr.String(), run.root.ID)
	}
}
