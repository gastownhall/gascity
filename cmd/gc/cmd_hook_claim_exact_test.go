package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// TestHookClaimExactPlanFencesObservedRow pins what the exact-claim plan
// fences on: every identity fact the observed candidate carried (status,
// assignee — empty for a fresh routed row —, step, root, route, continuation
// group) plus the pool claim's effects (status transition, assignee only when
// the row was unassigned).
func TestHookClaimExactPlanFencesObservedRow(t *testing.T) {
	candidate := beads.Bead{
		ID:     "work-1",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to":          "worker",
			"gc.root_bead_id":       "root-1",
			"gc.step_id":            "build",
			"gc.continuation_group": "lane-7",
		},
	}

	req := hookClaimExactPlan(candidate, "worker-1")

	if req.ID != "work-1" {
		t.Fatalf("request id = %q, want work-1", req.ID)
	}
	wantStatus, wantAssignee, wantStep := "open", "", "build"
	wantRoot, wantRoute, wantGroup := "root-1", "worker", "lane-7"
	got := req.Want
	if got.Status == nil || *got.Status != wantStatus {
		t.Errorf("want.status = %v, want %q", got.Status, wantStatus)
	}
	if got.Assignee == nil || *got.Assignee != wantAssignee {
		t.Errorf("want.assignee = %v, want pointer to %q (unassigned)", got.Assignee, wantAssignee)
	}
	if got.StepID == nil || *got.StepID != wantStep {
		t.Errorf("want.step_id = %v, want %q", got.StepID, wantStep)
	}
	if got.RootBeadID == nil || *got.RootBeadID != wantRoot {
		t.Errorf("want.root_bead_id = %v, want %q", got.RootBeadID, wantRoot)
	}
	if got.RoutedTo == nil || *got.RoutedTo != wantRoute {
		t.Errorf("want.routed_to = %v, want %q", got.RoutedTo, wantRoute)
	}
	if got.ContinuationGroup == nil || *got.ContinuationGroup != wantGroup {
		t.Errorf("want.continuation_group = %v, want %q", got.ContinuationGroup, wantGroup)
	}
	if req.OnSuccess.Status == nil || *req.OnSuccess.Status != "in_progress" {
		t.Errorf("onSuccess.status = %v, want in_progress", req.OnSuccess.Status)
	}
	if req.OnSuccess.Assignee == nil || *req.OnSuccess.Assignee != "worker-1" {
		t.Errorf("onSuccess.assignee = %v, want worker-1 (row was unassigned)", req.OnSuccess.Assignee)
	}
}

// TestHookClaimExactPlanOmitsRouteFactsTheProjectionLacked: routed_to and
// continuation_group become preconditions only when the observed projection
// carried them — a work query that does not project them must not fence on
// facts it never saw.
func TestHookClaimExactPlanOmitsRouteFactsTheProjectionLacked(t *testing.T) {
	req := hookClaimExactPlan(beads.Bead{
		ID:       "work-1",
		Status:   "open",
		Metadata: map[string]string{"gc.root_bead_id": "root-1", "gc.step_id": "build"},
	}, "worker-1")

	if req.Want.RoutedTo != nil {
		t.Fatalf("want.routed_to = %v, want nil (not projected)", req.Want.RoutedTo)
	}
	if req.Want.ContinuationGroup != nil {
		t.Fatalf("want.continuation_group = %v, want nil (not projected)", req.Want.ContinuationGroup)
	}
}

// TestHookClaimExactThroughStoreClassifiesOutcomes pins the exact-claim
// classifier: lost races are rejections (the hook moves on), a claimed result
// owned by someone else is a rejection, a committed write whose readback
// failed keeps the claimed verdict alongside the error, and a write that
// landed on a moved row reports moved so the caller gives the bead back.
func TestHookClaimExactThroughStoreClassifiesOutcomes(t *testing.T) {
	ours := beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "worker-1"}
	cases := []struct {
		name       string
		res        molecule.ClaimExactResult
		err        error
		wantVerd   hookClaimExactVerdict
		wantErr    bool
		wantBeadID string
	}{
		{
			name:       "claimed",
			res:        molecule.ClaimExactResult{Bead: ours, Outcome: molecule.ClaimExactClaimed, Fence: molecule.ClaimExactFenceRevision},
			wantVerd:   hookClaimExactClaimedVerdict,
			wantBeadID: "work-1",
		},
		{
			name:       "claimed but owned elsewhere",
			res:        molecule.ClaimExactResult{Bead: beads.Bead{ID: "work-1", Assignee: "rival"}, Outcome: molecule.ClaimExactClaimed},
			wantVerd:   hookClaimExactRejectedVerdict,
			wantBeadID: "work-1",
		},
		{
			name:       "precondition failed",
			res:        molecule.ClaimExactResult{Bead: beads.Bead{ID: "work-1", Assignee: "rival"}, Outcome: molecule.ClaimExactPreconditionFailed},
			wantVerd:   hookClaimExactRejectedVerdict,
			wantBeadID: "work-1",
		},
		{
			name:       "stale generation",
			res:        molecule.ClaimExactResult{Bead: beads.Bead{ID: "work-1"}, Outcome: molecule.ClaimExactStale},
			wantVerd:   hookClaimExactRejectedVerdict,
			wantBeadID: "work-1",
		},
		{
			name:       "observation moved",
			res:        molecule.ClaimExactResult{Bead: beads.Bead{ID: "work-1", Assignee: "worker-1"}, Outcome: molecule.ClaimExactObservationMoved, Fence: molecule.ClaimExactFenceAssignment},
			wantVerd:   hookClaimExactMovedVerdict,
			wantBeadID: "work-1",
		},
		{
			name:       "committed write unreadable",
			res:        molecule.ClaimExactResult{Bead: ours, Outcome: molecule.ClaimExactClaimed},
			err:        molecule.ErrClaimExactReadback,
			wantVerd:   hookClaimExactClaimedVerdict,
			wantErr:    true,
			wantBeadID: "work-1",
		},
		{
			name:     "plain store error",
			res:      molecule.ClaimExactResult{},
			err:      beads.ErrConditionalWriteUnsupported,
			wantVerd: "",
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead, verdict, err := hookClaimExactThroughStore("worker-1", tc.res, tc.err)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if verdict != tc.wantVerd {
				t.Fatalf("verdict = %q, want %q", verdict, tc.wantVerd)
			}
			if bead.ID != tc.wantBeadID {
				t.Fatalf("bead id = %q, want %q", bead.ID, tc.wantBeadID)
			}
		})
	}
}

// exactClaimHarness drives one `gc hook --claim` over a served graph-step
// candidate, with the exact seam faked and the pool claim spied, so tests can
// pin which seam a graph step took and what it was fenced on.
type exactClaimHarness struct {
	runner      string
	claimCalls  []string
	exactReqs   []molecule.ClaimExactRequest
	exactResult molecule.ClaimExactResult
	exactErr    error
	reclaimed   bool
	reclaimFrom string
	rejections  [][3]string
	reclaimedEv [][3]string
}

func (h *exactClaimHarness) run(t *testing.T, opts hookClaimOptions) (string, int) {
	t.Helper()
	wf := beads.NewMemStore()
	if _, err := wf.Create(beads.Bead{ID: "root-1", Title: "root", Status: "open", Metadata: map[string]string{"gc.kind": "workflow"}}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return h.runner, nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			h.claimCalls = append(h.claimCalls, beadID)
			return beads.Bead{}, false, nil
		},
		ClaimExact: func(_ context.Context, _ string, _ []string, _ string, req molecule.ClaimExactRequest) (molecule.ClaimExactResult, error) {
			h.exactReqs = append(h.exactReqs, req)
			return h.exactResult, h.exactErr
		},
		ReclaimStale: func(_ context.Context, _ string, _ []string, _ string) (bool, string, error) {
			return h.reclaimed, h.reclaimFrom, nil
		},
		WorkflowStore: func(context.Context, string, []string, string, string) beads.Store { return wf },
		DrainAck:      func(io.Writer) error { return nil },
		EmitClaimRejected: func(beadID, winner, actor string) {
			h.rejections = append(h.rejections, [3]string{beadID, winner, actor})
		},
		EmitClaimFailed:          func(string, hookClaimOptions, error) {},
		StampWorkMeta:            func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		StampSessionClaim:        func(string, string) error { return nil },
		PublishRunMap:            func(string, string, ...string) error { return nil },
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
		EnqueueContinuationNudge: func(string, string, io.Writer) {},
		ResolveWorkBranch:        func(hookClaimWorkTree) string { return "" },
		ResolveSessionWorkDir:    func(string) string { return "" },
		EmitHookClaimReclaimedStale: func(beadID, from, to string) {
			h.reclaimedEv = append(h.reclaimedEv, [3]string{beadID, from, to})
		},
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", t.TempDir(), opts, ops, &stdout, &stderr)
	return stdout.String(), code
}

// TestDoHookClaimClaimsGraphStepThroughExactSeamNeverPoolClaim: a served
// graph step claims through the exact seam only; the unfenced pool claim is
// never issued for it, and the work receipt names the fenced bead.
func TestDoHookClaimClaimsGraphStepThroughExactSeamNeverPoolClaim(t *testing.T) {
	h := &exactClaimHarness{
		runner: `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"build"}}]`,
		exactResult: molecule.ClaimExactResult{
			Bead:    beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "worker-1", Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": "root-1", "gc.step_id": "build"}},
			Outcome: molecule.ClaimExactClaimed,
			Fence:   molecule.ClaimExactFenceRevision,
		},
	}
	stdout, code := h.run(t, hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	})

	if code != 0 {
		t.Fatalf("doHookClaim = %d; stdout=%s", code, stdout)
	}
	if len(h.claimCalls) != 0 {
		t.Fatalf("pool claim issued for a graph step: %v", h.claimCalls)
	}
	if len(h.exactReqs) != 1 || h.exactReqs[0].ID != "work-1" {
		t.Fatalf("exact claims = %+v, want exactly one on work-1", h.exactReqs)
	}
	if !strings.Contains(stdout, `"action":"work"`) || !strings.Contains(stdout, `"bead_id":"work-1"`) {
		t.Fatalf("no work receipt for the exact claim: %s", stdout)
	}
}

// TestDoHookClaimExactRejectedNeverFallsBackToPoolClaim: when the exact seam
// rejects (the observed step moved under a rival claim), the hook reports the
// rejection and moves on — it must never retry the bead through the unfenced
// pool claim.
func TestDoHookClaimExactRejectedNeverFallsBackToPoolClaim(t *testing.T) {
	h := &exactClaimHarness{
		runner: `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"build"}}]`,
		exactResult: molecule.ClaimExactResult{
			Bead:    beads.Bead{ID: "work-1", Status: "open", Assignee: "rival", Metadata: map[string]string{"gc.root_bead_id": "root-1", "gc.step_id": "test"}},
			Outcome: molecule.ClaimExactPreconditionFailed,
		},
	}
	stdout, _ := h.run(t, hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	})

	if len(h.claimCalls) != 0 {
		t.Fatalf("rejected exact claim fell back to the pool claim: %v", h.claimCalls)
	}
	if len(h.exactReqs) != 1 {
		t.Fatalf("exact claims = %d, want 1", len(h.exactReqs))
	}
	if strings.Contains(stdout, `"action":"work"`) {
		t.Fatalf("handed out work for a rejected exact claim: %s", stdout)
	}
	if len(h.rejections) != 1 || h.rejections[0] != [3]string{"work-1", "rival", "worker-1"} {
		t.Fatalf("rejection events = %v, want one naming the rival winner", h.rejections)
	}
}

// TestDoHookClaimReclaimsStaleAssigneeThenExactClaimsSameCycle: ga-7rj87d's
// same-cycle retry for a reclaimed stale lease, on a graph step: the reclaim
// reverts the lease (row open, unassigned) and the exact claim that follows
// must fence on that post-reclaim row — assignee "" — while keeping the
// observed step/root identity, and the reclaimed_stale event still fires.
func TestDoHookClaimReclaimsStaleAssigneeThenExactClaimsSameCycle(t *testing.T) {
	h := &exactClaimHarness{
		runner:      `[{"id":"work-1","status":"open","assignee":"dead-worker","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"build"}}]`,
		reclaimed:   true,
		reclaimFrom: "dead-worker",
		exactResult: molecule.ClaimExactResult{
			Bead:    beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "worker-1", Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": "root-1", "gc.step_id": "build"}},
			Outcome: molecule.ClaimExactClaimed,
			Fence:   molecule.ClaimExactFenceRevision,
		},
	}
	stdout, code := h.run(t, hookClaimOptions{
		Assignee:               "worker-1",
		IdentityCandidates:     []string{"worker-1"},
		RouteTargets:           []string{"worker"},
		AutoReclaimStaleClaims: true,
		JSON:                   true,
	})

	if code != 0 {
		t.Fatalf("doHookClaim = %d; stdout=%s", code, stdout)
	}
	if len(h.claimCalls) != 0 {
		t.Fatalf("pool claim issued for a graph step: %v", h.claimCalls)
	}
	if len(h.exactReqs) != 1 {
		t.Fatalf("exact claims = %d, want the same-cycle retry", len(h.exactReqs))
	}
	req := h.exactReqs[0]
	if req.Want.Assignee == nil || *req.Want.Assignee != "" {
		t.Fatalf("exact claim fenced on assignee %v, want the post-reclaim unassigned row", req.Want.Assignee)
	}
	if req.Want.StepID == nil || *req.Want.StepID != "build" {
		t.Fatalf("exact claim step = %v, want the observed build step", req.Want.StepID)
	}
	if !strings.Contains(stdout, `"action":"work"`) {
		t.Fatalf("no work receipt for the reclaimed+exact claim: %s", stdout)
	}
	if len(h.reclaimedEv) != 1 || h.reclaimedEv[0] != [3]string{"work-1", "dead-worker", "worker-1"} {
		t.Fatalf("reclaimed_stale events = %v, want one naming the stale owner", h.reclaimedEv)
	}
}
