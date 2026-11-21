package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

	req := hookClaimExactPlan(candidate, "worker-1", hookClaimOptions{})

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
	}, "worker-1", hookClaimOptions{})

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
	// exactBy, when set, answers each exact claim by bead ID instead of the
	// single exactResult, so one run can mix outcomes across candidates.
	exactBy        map[string]molecule.ClaimExactResult
	releases       [][2]string
	releasedEvents []hookClaimReleaseRecord
	reclaimed      bool
	reclaimFrom    string
	rejections     [][3]string
	reclaimedEv    [][3]string
	stampCalls     int
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
			if h.exactBy != nil {
				return h.exactBy[req.ID], h.exactErr
			}
			return h.exactResult, h.exactErr
		},
		Release: func(_ context.Context, _ string, _ []string, beadID, assignee string) (bool, error) {
			h.releases = append(h.releases, [2]string{beadID, assignee})
			return true, nil
		},
		EmitClaimReleased: func(rec hookClaimReleaseRecord) {
			h.releasedEvents = append(h.releasedEvents, rec)
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

// TestDoHookClaimExactMovedReleasesTheLandedClaimAndMovesOn: a guarded claim
// that committed onto a row whose observed step had already moved (outcome
// observation_moved) is this session's to unwind: the hook releases the
// landed claim, fires bead.claim_released with reason observation_moved, and
// keeps walking the remaining candidates instead of wedging or draining.
func TestDoHookClaimExactMovedReleasesTheLandedClaimAndMovesOn(t *testing.T) {
	h := &exactClaimHarness{
		runner: `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"build"}},` +
			`{"id":"work-2","status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"test"}}]`,
		exactBy: map[string]molecule.ClaimExactResult{
			"work-1": {
				Bead:    beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "worker-1", Metadata: map[string]string{"gc.root_bead_id": "root-2", "gc.step_id": "other"}},
				Outcome: molecule.ClaimExactObservationMoved,
				Fence:   molecule.ClaimExactFenceAssignment,
			},
			"work-2": {
				Bead:    beads.Bead{ID: "work-2", Status: "in_progress", Assignee: "worker-1", Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": "root-1", "gc.step_id": "test"}},
				Outcome: molecule.ClaimExactClaimed,
				Fence:   molecule.ClaimExactFenceAssignment,
			},
		},
	}
	stdout, code := h.run(t, hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	})
	if code != 0 {
		t.Fatalf("doHookClaim = %d; stdout=%s", code, stdout)
	}
	if len(h.releases) != 1 || h.releases[0] != [2]string{"work-1", "worker-1"} {
		t.Fatalf("releases = %v, want exactly the moved claim work-1 given back as worker-1", h.releases)
	}
	if len(h.releasedEvents) != 1 || h.releasedEvents[0] != (hookClaimReleaseRecord{BeadID: "work-1", Assignee: "worker-1", Reason: "observation_moved"}) {
		t.Fatalf("claim_released events = %v, want one work-1 release with reason observation_moved", h.releasedEvents)
	}
	if len(h.exactReqs) != 2 || h.exactReqs[0].ID != "work-1" || h.exactReqs[1].ID != "work-2" {
		t.Fatalf("exact claims = %v, want work-1 then work-2", h.exactReqs)
	}
	if !strings.Contains(stdout, `"action":"work"`) || !strings.Contains(stdout, `"bead_id":"work-2"`) {
		t.Fatalf("hook wedged on the moved claim instead of serving work-2: %s", stdout)
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

// TestDoHookClaimPromotesReadyGraphStepThroughExactSeam: a ready assignment
// (open, already owned by this session's identity) that is a graph step
// promotes through the exact seam, fenced on the observed row including its
// assignee spelling — and the effects keep the stored owner, so the claim
// only advances the status.
func TestDoHookClaimPromotesReadyGraphStepThroughExactSeam(t *testing.T) {
	h := &exactClaimHarness{
		runner: `[{"id":"work-1","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"build"}}]`,
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
		t.Fatalf("pool claim issued for a ready graph step: %v", h.claimCalls)
	}
	if len(h.exactReqs) != 1 || h.exactReqs[0].ID != "work-1" {
		t.Fatalf("exact claims = %+v, want exactly one on work-1", h.exactReqs)
	}
	req := h.exactReqs[0]
	if req.Want.Assignee == nil || *req.Want.Assignee != "worker-1" {
		t.Fatalf("exact claim fenced on assignee %v, want the stored owner spelling", req.Want.Assignee)
	}
	if req.OnSuccess.Assignee != nil {
		t.Fatalf("exact claim effects reassign %q; a ready promotion keeps the stored owner", *req.OnSuccess.Assignee)
	}
	if !strings.Contains(stdout, `"action":"work"`) || !strings.Contains(stdout, `"reason":"ready_assignment"`) {
		t.Fatalf("no ready-assignment receipt for the exact claim: %s", stdout)
	}
}

// TestDoHookClaimReadyGraphStepFencedOnAssignee: the ready tier's exact claim
// fences on the observed owner; a row whose assignee moved to a rival is a
// rejection naming the rival, never a pool-claim fallback and never work.
func TestDoHookClaimReadyGraphStepFencedOnAssignee(t *testing.T) {
	h := &exactClaimHarness{
		runner: `[{"id":"work-1","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"build"}}]`,
		exactResult: molecule.ClaimExactResult{
			Bead:    beads.Bead{ID: "work-1", Status: "open", Assignee: "rival", Metadata: map[string]string{"gc.root_bead_id": "root-1", "gc.step_id": "build"}},
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
		t.Fatalf("assignee-moved ready step fell back to the pool claim: %v", h.claimCalls)
	}
	if len(h.exactReqs) != 1 {
		t.Fatalf("exact claims = %d, want 1", len(h.exactReqs))
	}
	if strings.Contains(stdout, `"action":"work"`) {
		t.Fatalf("handed out work for a moved ready assignment: %s", stdout)
	}
	if len(h.rejections) != 1 || h.rejections[0] != [3]string{"work-1", "rival", "worker-1"} {
		t.Fatalf("rejection events = %v, want one naming the rival owner", h.rejections)
	}
}

// TestDoHookClaimExactClaimStampsSessionIdentityInOneWrite: a session-run
// claim of a graph step folds the durable session back-reference and the
// write-once gc.claimed_at into the claim's single fenced write, so the
// post-claim identity stamp finds every key current and issues no second
// metadata write.
func TestDoHookClaimExactClaimStampsSessionIdentityInOneWrite(t *testing.T) {
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
		Env:                []string{"GC_SESSION_ID=sess-1", "GC_SESSION_NAME=gc__role-mc-sess1"},
		JSON:               true,
	})

	if code != 0 {
		t.Fatalf("doHookClaim = %d; stdout=%s", code, stdout)
	}
	if len(h.exactReqs) != 1 {
		t.Fatalf("exact claims = %d, want 1", len(h.exactReqs))
	}
	meta := h.exactReqs[0].OnSuccess.Metadata
	if meta["gc.session_id"] != "sess-1" {
		t.Fatalf("claim effects session id = %q, want sess-1 folded into the claim write", meta["gc.session_id"])
	}
	if meta["gc.session_name"] != "gc__role-mc-sess1" {
		t.Fatalf("claim effects session name = %q, want the session's name", meta["gc.session_name"])
	}
	if meta["gc.claimed_at"] == "" {
		t.Fatal("claim effects carry no gc.claimed_at; the write-once stamp belongs in the claim write")
	}
	if h.stampCalls != 0 {
		t.Fatalf("post-claim stamp writes = %d, want 0 (identity already durable in the claim write)", h.stampCalls)
	}
	if !strings.Contains(stdout, `"action":"work"`) {
		t.Fatalf("no work receipt: %s", stdout)
	}
}

// TestHookClaimExactPlanCarriesSessionIdentityByRule pins the session-key
// rules the claim effects inherit from hookClaimIdentityPatch: session keys
// only for a session-run claim of a non-control bead, claimed_at write-once
// (already-present values are never rewritten), and nothing stamped when no
// session identity is available.
func TestHookClaimExactPlanCarriesSessionIdentityByRule(t *testing.T) {
	sessionEnv := []string{"GC_SESSION_ID=sess-1", "GC_SESSION_NAME=gc__role-mc-sess1"}
	base := beads.Bead{
		ID:       "work-1",
		Status:   "open",
		Metadata: map[string]string{"gc.root_bead_id": "root-1", "gc.step_id": "build"},
	}

	// Session run: both back-reference keys plus the write-once claimed_at.
	withSession := hookClaimExactPlan(base, "worker-1", hookClaimOptions{Env: sessionEnv})
	if withSession.OnSuccess.Metadata["gc.session_id"] != "sess-1" ||
		withSession.OnSuccess.Metadata["gc.session_name"] != "gc__role-mc-sess1" ||
		withSession.OnSuccess.Metadata["gc.claimed_at"] == "" {
		t.Fatalf("session-run effects = %v, want session id+name and claimed_at", withSession.OnSuccess.Metadata)
	}

	// claimed_at is write-once: an already-claimed row is not re-stamped and
	// its other keys still fold in.
	claimedOnce := base
	claimedOnce.Metadata = map[string]string{"gc.root_bead_id": "root-1", "gc.step_id": "build", "gc.claimed_at": "2026-01-01T00:00:00Z"}
	rewritten := hookClaimExactPlan(claimedOnce, "worker-1", hookClaimOptions{Env: sessionEnv})
	if got := rewritten.OnSuccess.Metadata["gc.claimed_at"]; got != "" {
		t.Fatalf("claimed_at effects = %q, want none (write-once)", got)
	}
	if rewritten.OnSuccess.Metadata["gc.session_id"] != "sess-1" {
		t.Fatalf("session id effects = %q, want sess-1", rewritten.OnSuccess.Metadata["gc.session_id"])
	}

	// Control beads stay session-free by design.
	control := base
	control.Metadata = map[string]string{"gc.root_bead_id": "root-1", "gc.step_id": "build", beadmeta.KindMetadataKey: beadmeta.KindDrain}
	controlPlan := hookClaimExactPlan(control, "worker-1", hookClaimOptions{Env: sessionEnv})
	if controlPlan.OnSuccess.Metadata["gc.session_id"] != "" {
		t.Fatalf("control-bead effects session id = %q, want none", controlPlan.OnSuccess.Metadata["gc.session_id"])
	}
	if controlPlan.OnSuccess.Metadata["gc.claimed_at"] == "" {
		t.Fatal("control-bead effects carry no claimed_at; the claim timestamp is unconditional")
	}

	// No session identity available: only the write-once claimed_at.
	bare := hookClaimExactPlan(base, "worker-1", hookClaimOptions{})
	if bare.OnSuccess.Metadata["gc.session_id"] != "" || bare.OnSuccess.Metadata["gc.session_name"] != "" {
		t.Fatalf("no-session effects = %v, want no session keys", bare.OnSuccess.Metadata)
	}
	if bare.OnSuccess.Metadata["gc.claimed_at"] == "" {
		t.Fatal("no-session effects carry no claimed_at")
	}
}

// TestDoHookClaimExactClaimFencesOnObservedGeneration: when the observed
// candidate carries gc.claim_generation, the exact claim fences on that
// generation — a row re-claimed underneath the observation is stale, not a
// claim — and a row with no generation claims with the fence open (advance
// whatever is there).
func TestDoHookClaimExactClaimFencesOnObservedGeneration(t *testing.T) {
	h := &exactClaimHarness{
		runner: `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"build","gc.claim_generation":"5"}}]`,
		exactResult: molecule.ClaimExactResult{
			Bead:    beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "worker-1", Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": "root-1", "gc.step_id": "build", "gc.claim_generation": "6"}},
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
	if len(h.exactReqs) != 1 {
		t.Fatalf("exact claims = %d, want 1", len(h.exactReqs))
	}
	from := h.exactReqs[0].FromGeneration
	if from == nil || *from != "5" {
		t.Fatalf("exact claim FromGeneration = %v, want the observed generation 5", from)
	}
}

// TestDoHookClaimExactClaimLeavesGenerationFenceOpenWhenUnobserved: a
// projection that did not carry gc.claim_generation claims without the
// generation fence (nil FromGeneration advances whatever the row holds)
// rather than guessing one.
func TestDoHookClaimExactClaimLeavesGenerationFenceOpenWhenUnobserved(t *testing.T) {
	h := &exactClaimHarness{
		runner: `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"build"}}]`,
		exactResult: molecule.ClaimExactResult{
			Bead:    beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "worker-1"},
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
	if len(h.exactReqs) != 1 {
		t.Fatalf("exact claims = %d, want 1", len(h.exactReqs))
	}
	if from := h.exactReqs[0].FromGeneration; from != nil {
		t.Fatalf("exact claim FromGeneration = %v, want nil (generation unobserved)", from)
	}
}

// TestDoHookClaimExactClaimDropsToAssignmentGuardWithoutIfRevision: against a
// bd whose update lacks --if-revision (the revision tier reports the
// capability missing), the exact claim re-fences the same observed row on the
// status+assignee guard - bd update --if-status open --if-assignee "" - and
// still never issues an unfenced bd update --claim.
func TestDoHookClaimExactClaimDropsToAssignmentGuardWithoutIfRevision(t *testing.T) {
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })

	rootRow := `{"id":"root-1","title":"root","status":"open","issue_type":"task","created_at":"2026-11-21T00:00:00Z",` +
		`"metadata":{"gc.kind":"workflow"}}`
	openRow := `{"id":"work-1","title":"work","status":"open","issue_type":"task","created_at":"2026-11-21T00:00:00Z",` +
		`"metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"build"}}`
	claimedRow := `{"id":"work-1","title":"work","status":"in_progress","assignee":"worker-1","issue_type":"task","created_at":"2026-11-21T00:00:00Z",` +
		`"metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"build","gc.claim_generation":"1","gc.claimed_at":"2026-11-21T00:01:00Z"}}`
	claimed := false
	var guardedWrites []string
	hookClaimCommandRunnerWithEnvContext = func(context.Context, map[string]string) beads.CommandRunner {
		return func(_ string, name string, args ...string) ([]byte, error) {
			switch {
			case name == "bd" && len(args) == 2 && args[1] == "--help":
				// A bd old enough to lack --if-revision on every write verb.
				return []byte("usage: bd " + args[0] + " [flags]"), nil
			case name == "bd" && len(args) >= 2 && args[0] == "show":
				// bd show --json prints an array of issues, and the reader's
				// fuzzy-ID guard demands each answer carry the row it asked
				// for, so root-1 and work-1 get their own rows.
				for _, arg := range args {
					switch arg {
					case "root-1":
						return []byte("[" + rootRow + "]"), nil
					case "work-1":
						if claimed {
							return []byte("[" + claimedRow + "]"), nil
						}
						return []byte("[" + openRow + "]"), nil
					}
				}
				return nil, fmt.Errorf("unexpected show in assignment-tier test: %s %s", name, strings.Join(args, " "))
			case name == "bd" && args[0] == "update" && containsArg(args, "--if-status") && containsArg(args, "--if-assignee"):
				guardedWrites = append(guardedWrites, strings.Join(args, " "))
				claimed = true
				return []byte(claimedRow), nil
			case name == "bd" && args[0] == "update":
				t.Fatalf("unguarded bd update issued for an exact claim: %s", strings.Join(args, " "))
				return nil, nil
			case name == "bd" && args[0] == "close" || name == "bd" && args[0] == "assign":
				return []byte(`{}`), nil
			default:
				return nil, fmt.Errorf("unexpected command in assignment-tier test: %s %s", name, strings.Join(args, " "))
			}
		}
	}

	observed, err := json.Marshal([]beads.Bead{{
		ID: "work-1", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": "root-1", "gc.step_id": "build"},
	}})
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	wf := beads.NewMemStore()
	if _, err := wf.Create(beads.Bead{ID: "root-1", Title: "root", Status: "open", Metadata: map[string]string{"gc.kind": "workflow"}}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	ops := hookClaimOps{
		Runner:            func(string, string) (string, error) { return string(observed), nil },
		DrainAck:          func(io.Writer) error { return nil },
		EmitClaimFailed:   func(string, hookClaimOptions, error) {},
		EmitClaimRejected: func(string, string, string) {},
		StampWorkMeta:     func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		PublishRunMap:     func(string, string, ...string) error { return nil },
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		WorkflowStore:     func(context.Context, string, []string, string, string) beads.Store { return wf },
	}
	// No ClaimExact override: the default bd-context seam must find the
	// assignment guard on its own.

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doHookClaim = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(guardedWrites) != 1 {
		t.Fatalf("guarded writes = %q, want exactly one assignment-guarded claim write (stdout=%s stderr=%s)", guardedWrites, stdout.String(), stderr.String())
	}
	write := guardedWrites[0]
	for _, want := range []string{"--if-status open", "--if-assignee ", "--status in_progress", "--assignee worker-1", "gc.claim_generation=1"} {
		if !strings.Contains(write, want) {
			t.Fatalf("guarded write %q lacks %q", write, want)
		}
	}
	if !strings.Contains(stdout.String(), `"action":"work"`) {
		t.Fatalf("no work receipt through the assignment tier: %s (stderr=%s)", stdout.String(), stderr.String())
	}
}
