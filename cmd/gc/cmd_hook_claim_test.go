package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestHookClaimWithBdStoreReloadsCanonicalBeadAfterPartialMutation(t *testing.T) {
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })

	var calls [][]string
	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, _ map[string]string) beads.CommandRunner {
		return func(_ string, name string, args ...string) ([]byte, error) {
			if name != "bd" {
				t.Fatalf("command name = %q, want bd", name)
			}
			calls = append(calls, append([]string(nil), args...))
			switch {
			case reflect.DeepEqual(args, []string{"update", "work-1", "--claim", "--json"}):
				return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"rig/worker"}}]`), nil
			case reflect.DeepEqual(args, []string{"show", "--json", "work-1"}):
				return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"rig/worker","gc.root_bead_id":"root-1","gc.continuation_group":"review"}}]`), nil
			default:
				t.Fatalf("unexpected bd args: %#v", args)
				return nil, nil
			}
		}
	}

	claimed, ok, err := hookClaimWithBdStore(context.Background(), "/rig", nil, "work-1", "worker-1")
	if err != nil {
		t.Fatalf("hookClaimWithBdStore: %v", err)
	}
	if !ok {
		t.Fatal("hookClaimWithBdStore ok = false, want true")
	}
	if claimed.Metadata["gc.root_bead_id"] != "root-1" || claimed.Metadata["gc.continuation_group"] != "review" {
		t.Fatalf("claimed metadata = %#v, want canonical root and continuation group", claimed.Metadata)
	}
	if len(calls) != 2 {
		t.Fatalf("bd calls = %#v, want claim update followed by canonical show", calls)
	}
}

func TestDoHookClaimStopsAfterCommittedClaimReadbackFailure(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[
			{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}},
			{"id":"work-2","status":"open","metadata":{"gc.routed_to":"worker"}}
		]`, nil
	}
	var attempts []string
	drained := false
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			attempts = append(attempts, beadID)
			return beads.Bead{ID: beadID, Assignee: assignee}, true, errors.New("canonical read failed")
		},
		DrainAck: func(io.Writer) error {
			drained = true
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("doHookClaim = %d, want 1", code)
	}
	if got := strings.Join(attempts, ","); got != "work-1" {
		t.Fatalf("claim attempts = %q, want only committed work-1", got)
	}
	if drained {
		t.Fatal("drain acknowledged after committed claim readback failure")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "claimed work-1 but loading canonical bead failed") {
		t.Fatalf("stderr = %q, want committed-claim diagnostic", stderr.String())
	}
}

func TestDoHookClaimUsesSelectedStoreContextForMutationAndContinuation(t *testing.T) {
	var claimedDir string
	var claimedEnv []string
	var listedDir string
	var listedEnv []string
	var assignedDir string
	var assignedEnv []string
	var assignedBead string

	storeDir := "rig-store"
	storeEnv := []string{"BEADS_DIR=rig-store", "GC_RIG_ROOT=rig-root"}
	candidates := []beads.Bead{{
		ID:       "bead-1",
		Status:   "open",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.run_target": "route-1", "gc.root_bead_id": "root-1", "gc.continuation_group": "group-a"},
	}}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedDir = dir
			claimedEnv = append([]string(nil), env...)
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: candidates[0].Metadata}, true, nil
		},
		ListContinuation: func(_ context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
			listedDir = dir
			listedEnv = append([]string(nil), env...)
			if rootID != "root-1" || group != "group-a" {
				t.Fatalf("continuation lookup = (%q, %q), want (root-1, group-a)", rootID, group)
			}
			return []beads.Bead{{ID: "sib-1", Status: "open", Metadata: candidates[0].Metadata}}, nil
		},
		AssignContinuation: func(_ context.Context, dir string, env []string, beadID, assignee string) error {
			assignedDir = dir
			assignedEnv = append([]string(nil), env...)
			assignedBead = beadID
			if assignee != "worker-1" {
				t.Fatalf("assignee = %q, want worker-1", assignee)
			}
			return nil
		},
		DrainAck:                 func(io.Writer) error { return nil },
		EnqueueContinuationNudge: func(string) {},
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", storeDir, hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		Env:                storeEnv,
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if claimedDir != storeDir {
		t.Fatalf("claimedDir = %q, want %q", claimedDir, storeDir)
	}
	if listedDir != storeDir {
		t.Fatalf("listedDir = %q, want %q", listedDir, storeDir)
	}
	if assignedDir != storeDir {
		t.Fatalf("assignedDir = %q, want %q", assignedDir, storeDir)
	}
	if !reflect.DeepEqual(claimedEnv, storeEnv) {
		t.Fatalf("claimedEnv = %#v, want %#v", claimedEnv, storeEnv)
	}
	if !reflect.DeepEqual(listedEnv, storeEnv) {
		t.Fatalf("listedEnv = %#v, want %#v", listedEnv, storeEnv)
	}
	if !reflect.DeepEqual(assignedEnv, storeEnv) {
		t.Fatalf("assignedEnv = %#v, want %#v", assignedEnv, storeEnv)
	}
	if assignedBead != "sib-1" {
		t.Fatalf("assignedBead = %q, want sib-1", assignedBead)
	}
}

func TestHookClaimEnvMapUsesOnlyTheQueryEnvironment(t *testing.T) {
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient-dolt.example.com")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "ambient_database")
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
	t.Setenv("BEADS_POSTGRES_HOST", "ambient-postgres.example.com")

	got := hookClaimEnvMap([]string{
		"BEADS_DOLT_SERVER_PORT=30778",
		"BEADS_POSTGRES_PORT=5432",
	}, "/rig", "worker")

	if value, ok := got["BEADS_DOLT_SERVER_HOST"]; ok {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want absent ambient value", value)
	}
	if value, ok := got["BEADS_POSTGRES_HOST"]; ok {
		t.Fatalf("BEADS_POSTGRES_HOST = %q, want absent ambient value", value)
	}
	if value, ok := got["BEADS_DOLT_SERVER_DATABASE"]; ok {
		t.Fatalf("BEADS_DOLT_SERVER_DATABASE = %q, want absent ambient value", value)
	}
	if value, ok := got["BEADS_DOLT_SERVER_TLS"]; ok {
		t.Fatalf("BEADS_DOLT_SERVER_TLS = %q, want absent ambient value", value)
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "30778" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want 30778", got["BEADS_DOLT_SERVER_PORT"])
	}
	if got["BEADS_POSTGRES_PORT"] != "5432" {
		t.Fatalf("BEADS_POSTGRES_PORT = %q, want 5432", got["BEADS_POSTGRES_PORT"])
	}
}

// TestHookClaimRunnerIsTheExactEnvRunner pins the wiring at the top of this
// file. Every other test in the package replaces
// hookClaimCommandRunnerWithEnvContext with a stub, so reverting it to the
// layered ExecCommandRunnerWithEnvContext would silently restore #5142
// (ambient BEADS_DOLT_* / BEADS_POSTGRES_* selectors reappearing in the claim
// mutation) with the suite still green.
func TestHookClaimRunnerIsTheExactEnvRunner(t *testing.T) {
	got := reflect.ValueOf(hookClaimCommandRunnerWithEnvContext).Pointer()
	want := reflect.ValueOf(beads.ExecCommandRunnerWithExactEnvContext).Pointer()
	if got != want {
		t.Fatal("hookClaimCommandRunnerWithEnvContext must be beads.ExecCommandRunnerWithExactEnvContext; " +
			"the layered runner re-admits ambient store selectors into the claim mutation (#5142)")
	}
}

// TestDoHookClaimSkipsBlockedRoutedHeadAndClaimsReadyBehindIt guards the
// widened-routed-tier fix: a routed tier's oldest candidate can be
// is_blocked (e.g. gated on a PR), and the hook must fall through to a
// Ready routed bead behind it rather than idle-exiting on the blocked head.
func TestDoHookClaimSkipsBlockedRoutedHeadAndClaimsReadyBehindIt(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "blocked-head", Status: "open", IsBlocked: boolPtr(true), Metadata: map[string]string{"gc.routed_to": "route-1"}},
		{ID: "ready-behind", Status: "open", Metadata: map[string]string{"gc.routed_to": "route-1"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	var claimedBead string
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedBead = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if claimedBead != "ready-behind" {
		t.Fatalf("claimedBead = %q, want ready-behind (blocked-head must be skipped)", claimedBead)
	}
}

func TestPreassignHookContinuationGroupPinsSiblingsToSessionID(t *testing.T) {
	tests := []struct {
		name         string
		opts         hookClaimOptions
		wantAssignee string
	}{{
		// The pin means "run this on THIS session". Only the session bead ID is an
		// identity the consumers agree on: ComputeAwakeSet matches it, and the
		// session's own re-poll queries $GC_SESSION_ID. The runtime slot label in
		// Assignee matches neither.
		name:         "session id wins over the runtime slot label",
		opts:         hookClaimOptions{Assignee: "gascity--gc__implementation-worker-5-pool", SessionID: "gcs-session-74f608f2"},
		wantAssignee: "gcs-session-74f608f2",
	}, {
		name:         "blank session id falls back to the claim assignee",
		opts:         hookClaimOptions{Assignee: "worker-1", SessionID: "   "},
		wantAssignee: "worker-1",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claimed := beads.Bead{
				ID:       "work-1",
				Status:   "in_progress",
				Metadata: map[string]string{"gc.kind": "workflow", "gc.root_bead_id": "root-1", "gc.continuation_group": "group-a", "gc.run_target": "route-1"},
			}
			var gotAssignees []string
			opts := tc.opts
			opts.RouteTargets = []string{"route-1"}
			ops := hookClaimOps{
				ListContinuation: func(_ context.Context, _ string, _ []string, rootID, group string) ([]beads.Bead, error) {
					if rootID != "root-1" || group != "group-a" {
						t.Fatalf("continuation lookup = (%q, %q), want (root-1, group-a)", rootID, group)
					}
					return []beads.Bead{
						{ID: "sib-1", Status: "open", Metadata: claimed.Metadata},
						{ID: "sib-2", Status: "open", Metadata: claimed.Metadata},
					}, nil
				},
				AssignContinuation: func(_ context.Context, _ string, _ []string, _, assignee string) error {
					gotAssignees = append(gotAssignees, assignee)
					return nil
				},
			}

			assigned, err := preassignHookContinuationGroup(claimed, opts, ops, ".")
			if err != nil {
				t.Fatalf("preassignHookContinuationGroup() error = %v", err)
			}
			if want := []string{"sib-1", "sib-2"}; !reflect.DeepEqual(assigned, want) {
				t.Fatalf("assigned = %#v, want %#v", assigned, want)
			}
			want := []string{tc.wantAssignee, tc.wantAssignee}
			if !reflect.DeepEqual(gotAssignees, want) {
				t.Fatalf("pinned assignees = %#v, want %#v", gotAssignees, want)
			}
		})
	}
}

// TestDoHookClaimReclaimsStaleAssigneeAndClaimsSameCycle covers ga-7rj87d
// FR1/FR2/FR3: a route-matched candidate whose only claim-eligibility
// failure is an existing Assignee gets a scoped reclaim attempt, and a
// successful reclaim is retried as a normal claim in the SAME hook cycle.
func TestDoHookClaimReclaimsStaleAssigneeAndClaimsSameCycle(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"work-1","assignee":"dead-worker","metadata":{"gc.routed_to":"worker"}}]`, nil
	}
	var reclaimCalls []string
	var claimCalls []string
	ops := hookClaimOps{
		Runner: runner,
		ReclaimStale: func(_ context.Context, _ string, _ []string, beadID string) (bool, string, error) {
			reclaimCalls = append(reclaimCalls, beadID)
			return true, "dead-worker", nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimCalls = append(claimCalls, beadID)
			return beads.Bead{ID: beadID, Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"worker"},
		AutoReclaimStaleClaims: true,
		JSON:                   true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.Join(reclaimCalls, ","); got != "work-1" {
		t.Fatalf("reclaim calls = %q, want scoped to work-1 only (FR2)", got)
	}
	if got := strings.Join(claimCalls, ","); got != "work-1" {
		t.Fatalf("claim calls = %q, want claim retried on work-1 in the same cycle (FR3)", got)
	}
}

// TestDoHookClaimLeavesCandidateUntouchedWhenNothingReclaimed covers
// ga-7rj87d FR4: when the scoped reclaim reports nothing reclaimed, the
// candidate must be left untouched (no claim attempt) and the hook
// continues — current behavior for an unreclaimable stale candidate.
func TestDoHookClaimLeavesCandidateUntouchedWhenNothingReclaimed(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"work-1","assignee":"live-worker","metadata":{"gc.routed_to":"worker"}}]`, nil
	}
	var reclaimCalls []string
	claimCalled := false
	ops := hookClaimOps{
		Runner: runner,
		ReclaimStale: func(_ context.Context, _ string, _ []string, beadID string) (bool, string, error) {
			reclaimCalls = append(reclaimCalls, beadID)
			return false, "", nil
		},
		Claim: func(_ context.Context, _ string, _ []string, _, _ string) (beads.Bead, bool, error) {
			claimCalled = true
			return beads.Bead{}, false, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("query", ".", hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"worker"},
		AutoReclaimStaleClaims: true,
		JSON:                   true,
	}, ops, &stdout, &stderr)

	if len(reclaimCalls) != 1 || reclaimCalls[0] != "work-1" {
		t.Fatalf("reclaim calls = %v, want exactly one scoped attempt on work-1", reclaimCalls)
	}
	if claimCalled {
		t.Fatal("Claim should not be attempted when nothing was reclaimed (FR4)")
	}
}

// TestDoHookClaimFlagOffNeverAttemptsReclaim covers ga-7rj87d NFR4/NFR5: the
// feature is off by default, and the flag-off path must never call
// ReclaimStale at all (byte-for-byte unchanged behavior/latency).
func TestDoHookClaimFlagOffNeverAttemptsReclaim(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"work-1","assignee":"dead-worker","metadata":{"gc.routed_to":"worker"}}]`, nil
	}
	reclaimCalled := false
	ops := hookClaimOps{
		Runner: runner,
		ReclaimStale: func(_ context.Context, _ string, _ []string, _ string) (bool, string, error) {
			reclaimCalled = true
			return true, "dead-worker", nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("query", ".", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if reclaimCalled {
		t.Fatal("ReclaimStale must not be called when AutoReclaimStaleClaims is off (NFR4/NFR5)")
	}
}

// TestDoHookClaimEmitsReclaimedStaleEventOnSuccess covers ga-7rj87d FR5: a
// successful reclaim-then-claim must fire the typed
// hook.claim.reclaimed_stale event exactly once, naming the bead, the
// previous (stale) owner, and the new assignee.
func TestDoHookClaimEmitsReclaimedStaleEventOnSuccess(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"work-1","assignee":"dead-worker","metadata":{"gc.routed_to":"worker"}}]`, nil
	}
	var emitted []string
	ops := hookClaimOps{
		Runner: runner,
		ReclaimStale: func(_ context.Context, _ string, _ []string, _ string) (bool, string, error) {
			return true, "dead-worker", nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		EmitHookClaimReclaimedStale: func(beadID, previousOwner, newAssignee string) {
			emitted = append(emitted, beadID+"|"+previousOwner+"|"+newAssignee)
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("query", ".", hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"worker"},
		AutoReclaimStaleClaims: true,
		JSON:                   true,
	}, ops, &stdout, &stderr)

	want := "work-1|dead-worker|worker-1"
	if len(emitted) != 1 || emitted[0] != want {
		t.Fatalf("emitted = %v, want exactly [%q] (FR5)", emitted, want)
	}
}

// TestDoHookClaimSkipsBudgetDeferredStaleAssigneeCandidate covers the
// ga-7rj87d round-1 review gap (FR1/NFR5): hookCandidateReclaimEligible
// checked only ID/assignee/route and omitted the budget-deferred check that
// hookCandidateClaimable already applies on the fresh-claim path. That let a
// route-matched candidate with a stale assignee bypass the daily
// build-budget gate via the opt-in reclaim path merely by having an
// assignee. A candidate still inside its gc.budget_deferred_until window
// must not be reclaimed even with AutoReclaimStaleClaims on.
func TestDoHookClaimSkipsBudgetDeferredStaleAssigneeCandidate(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"work-1","assignee":"dead-worker","metadata":{"gc.routed_to":"worker","gc.budget_deferred_until":"2099-01-01T00:00:00Z"}}]`, nil
	}
	reclaimCalled := false
	ops := hookClaimOps{
		Runner: runner,
		ReclaimStale: func(_ context.Context, _ string, _ []string, _ string) (bool, string, error) {
			reclaimCalled = true
			return true, "dead-worker", nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("query", ".", hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"worker"},
		AutoReclaimStaleClaims: true,
		JSON:                   true,
	}, ops, &stdout, &stderr)

	if reclaimCalled {
		t.Fatal("ReclaimStale must not be called for a candidate still inside its budget-deferred window, even with AutoReclaimStaleClaims on")
	}
}

// TestPreassignHookContinuationGroupSkipsHeldSiblings is the #6026 regression:
// a continuation sibling carrying a canonical dispatch hold label
// (beadmeta.DispatchHoldLabels) must never be pre-assigned to the claimant,
// even when otherwise eligible (open, unassigned, route-matching). Pinning an
// agent's name onto a bead deliberately parked hold:mayor/hold:external
// contradicts the hold contract: the required next actor is, by construction,
// not this session.
//
// The case and padded variants pin the trimmed, case-insensitive comparison
// that keeps this seam agreeing with the hook-serve filter
// (isHeldHookCandidate); the lookalike sibling pins the other direction —
// hold-ish labels that are not a canonical value are ordinary work and must
// still be pre-assigned.
func TestPreassignHookContinuationGroupSkipsHeldSiblings(t *testing.T) {
	var heldLabels []string
	for _, hold := range beadmeta.DispatchHoldLabels {
		heldLabels = append(heldLabels, hold, strings.ToUpper(hold), " "+hold+" ")
	}
	for _, label := range heldLabels {
		t.Run(label, func(t *testing.T) {
			claimed := beads.Bead{
				ID:       "work-1",
				Status:   "in_progress",
				Metadata: map[string]string{"gc.kind": "workflow", "gc.root_bead_id": "root-1", "gc.continuation_group": "group-a", "gc.run_target": "route-1"},
			}
			var gotAssignees []string
			var assignedIDs []string
			opts := hookClaimOptions{Assignee: "worker-1", RouteTargets: []string{"route-1"}}
			ops := hookClaimOps{
				ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
					return []beads.Bead{
						{ID: "sib-held", Status: "open", Metadata: claimed.Metadata, Labels: []string{label}},
						{ID: "sib-open", Status: "open", Metadata: claimed.Metadata},
						{ID: "sib-lookalike", Status: "open", Metadata: claimed.Metadata, Labels: []string{"mpr-human-hold", "needs-mayor"}},
					}, nil
				},
				AssignContinuation: func(_ context.Context, _ string, _ []string, beadID, assignee string) error {
					assignedIDs = append(assignedIDs, beadID)
					gotAssignees = append(gotAssignees, assignee)
					return nil
				},
			}

			assigned, err := preassignHookContinuationGroup(claimed, opts, ops, ".")
			if err != nil {
				t.Fatalf("preassignHookContinuationGroup() error = %v", err)
			}
			if want := []string{"sib-open", "sib-lookalike"}; !reflect.DeepEqual(assigned, want) {
				t.Fatalf("REGRESSION #6026: assigned = %#v, want %#v (held sibling must be skipped, non-held siblings must still be assigned)", assigned, want)
			}
			if want := []string{"sib-open", "sib-lookalike"}; !reflect.DeepEqual(assignedIDs, want) {
				t.Fatalf("REGRESSION #6026: AssignContinuation called for %#v, want %#v; held sibling must never reach the assign mutation", assignedIDs, want)
			}
			if want := []string{"worker-1", "worker-1"}; !reflect.DeepEqual(gotAssignees, want) {
				t.Fatalf("pinned assignees = %#v, want %#v", gotAssignees, want)
			}
		})
	}
}

// A subprocess can report its committed CAS after the mutation deadline has
// elapsed. Advance the deadline in virtual time while the CAS is in flight.
func TestHookClaimWithBdStoreReadbackAfterMutationDeadline(t *testing.T) {
	for _, tc := range []struct {
		name       string
		projection string
		canonical  string
		conflict   bool
		readErr    error
		wantOK     bool
		wantOwner  string
		wantReads  int
	}{
		{name: "canonical owner and metadata", projection: "worker-1", canonical: "worker-1", wantOK: true, wantOwner: "worker-1", wantReads: 1},
		{name: "committed but unconfirmed", projection: "worker-1", readErr: errors.New("verification unavailable"), wantOK: true, wantOwner: "worker-1", wantReads: 1},
		{name: "foreign canonical owner", projection: "worker-1", canonical: "other-1", wantOwner: "other-1", wantReads: 1},
		{name: "foreign mutation projection", projection: "other-1", wantOwner: "other-1"},
		{name: "conflict identifies winner", conflict: true, canonical: "other-1", wantOwner: "other-1", wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				original := hookClaimCommandRunnerWithEnvContext
				t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = original })
				mutation, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				env := []string{"BEADS_DIR=/rig/.beads", "GC_RIG_ROOT=/rig"}
				wantEnv := hookClaimEnvMap(env, "/rig", "worker-1")
				var readContext context.Context
				mutations, reads := 0, 0
				hookClaimCommandRunnerWithEnvContext = func(ctx context.Context, bound map[string]string) beads.CommandRunner {
					if !reflect.DeepEqual(bound, wantEnv) {
						t.Fatalf("bound environment = %#v, want %#v", bound, wantEnv)
					}
					return func(dir, name string, args ...string) ([]byte, error) {
						if dir != "/rig" || name != "bd" {
							t.Fatalf("command binding = (%q, %q), want (/rig, bd)", dir, name)
						}
						switch {
						case reflect.DeepEqual(args, []string{"update", "work-1", "--claim", "--json"}):
							mutations++
							if !tc.conflict {
								<-ctx.Done()
							}
							if ctx != mutation || (!tc.conflict && !errors.Is(ctx.Err(), context.DeadlineExceeded)) {
								t.Fatal("mutation must retain its original exhausted deadline")
							}
							if tc.conflict {
								return nil, errors.New("already claimed by other-1")
							}
							out, err := json.Marshal([]beads.Bead{{ID: "work-1", Status: "in_progress", Assignee: tc.projection}})
							return out, err
						case reflect.DeepEqual(args, []string{"show", "--json", "work-1"}):
							reads++
							readContext = ctx
							deadline, bounded := ctx.Deadline()
							if ctx.Err() != nil || !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > hookClaimReadbackTimeout {
								t.Fatalf("canonical read lacks a fresh bounded context: err=%v deadline=%v", ctx.Err(), deadline)
							}
							if tc.readErr != nil {
								return nil, tc.readErr
							}
							out, err := json.Marshal([]beads.Bead{{ID: "work-1", Status: "in_progress", Assignee: tc.canonical, Metadata: map[string]string{"canonical": "retained"}}})
							return out, err
						default:
							t.Fatalf("unexpected bd arguments: %q", args)
							return nil, errors.New("unexpected command")
						}
					}
				}
				claimed, ok, err := hookClaimWithBdStore(mutation, "/rig", env, "work-1", "worker-1")
				if ok != tc.wantOK || claimed.Assignee != tc.wantOwner || mutations != 1 || reads != tc.wantReads {
					t.Fatalf("claim = (%+v, %v, %v), mutations=%d reads=%d", claimed, ok, err, mutations, reads)
				}
				if tc.readErr != nil {
					if !errors.Is(err, tc.readErr) || !strings.Contains(err.Error(), "reloading claimed bead") {
						t.Fatalf("error = %v, want committed canonical-read failure", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if ok && err == nil && claimed.Metadata["canonical"] != "retained" {
					t.Fatal("claim returned the mutation projection instead of the canonical row")
				}
				if tc.conflict && readContext != mutation {
					t.Fatal("conflict read must retain the original mutation context")
				}
				if readContext != nil && !tc.conflict && !errors.Is(readContext.Err(), context.Canceled) {
					t.Fatal("canonical-read context was not canceled on return")
				}
			})
		})
	}
}

// Both claim tiers must stop after the real BdStore claim commits and its
// independent canonical read fails; neither may try the second candidate.
func TestHookClaimTiersStopAfterBdStoreReadbackFailure(t *testing.T) {
	for _, ready := range []bool{false, true} {
		name := "fresh"
		if ready {
			name = "ready assignment"
		}
		t.Run(name, func(t *testing.T) {
			original := hookClaimCommandRunnerWithEnvContext
			t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = original })
			var cancelMutation context.CancelFunc
			mutations := 0
			hookClaimCommandRunnerWithEnvContext = func(ctx context.Context, _ map[string]string) beads.CommandRunner {
				return func(_ string, _ string, args ...string) ([]byte, error) {
					switch args[0] {
					case "update":
						mutations++
						if args[1] != "work-1" {
							t.Fatalf("replayed or additional claim: %q", args)
						}
						cancelMutation()
						return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1"}]`), nil
					case "show":
						if ctx.Err() != nil {
							t.Fatalf("read inherited canceled mutation context: %v", ctx.Err())
						}
						return nil, errors.New("canonical verification unavailable")
					default:
						t.Fatalf("unexpected command: %q", args)
						return nil, errors.New("unexpected command")
					}
				}
			}
			ops := hookClaimOps{Claim: func(ctx context.Context, dir string, env []string, id, owner string) (beads.Bead, bool, error) {
				mutation, cancel := context.WithCancel(ctx)
				cancelMutation = cancel
				defer cancel()
				return hookClaimWithBdStore(mutation, dir, env, id, owner)
			}}
			opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, JSON: true}
			candidates := []beads.Bead{
				{ID: "work-1", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
				{ID: "work-2", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
			}
			var stdout, stderr bytes.Buffer
			var result hookClaimResult
			if ready {
				for i := range candidates {
					candidates[i].Assignee = "worker-1"
				}
				result = claimFirstReadyHookAssignment(candidates, opts, ops, "/rig", &stdout, &stderr)
			} else {
				result = claimFirstEligibleHookCandidate(candidates, opts, ops, "/rig", &stdout, &stderr)
			}
			if !result.terminal || result.code != 1 || mutations != 1 || stdout.Len() != 0 {
				t.Fatalf("result=%+v mutations=%d stdout=%q; want terminal failure with no receipt", result, mutations, stdout.String())
			}
			if !strings.Contains(stderr.String(), "claimed work-1 but loading canonical bead failed") {
				t.Fatalf("missing committed-but-unconfirmed diagnostic: %q", stderr.String())
			}
		})
	}
}

// A timely CAS must remain deliverable when canonical readback crosses the
// landing fence; an actually late CAS must still unwind. No wall-clock sleeps.
func TestHookClaimTiersUseCASLandingBeforeCanonicalReadback(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		tier := "fresh"
		if assigned {
			tier = "assigned"
		}
		for _, outcome := range []string{"timely CAS", "late CAS", "failed readback"} {
			t.Run(tier+"/"+outcome, func(t *testing.T) {
				const window = time.Minute
				invokedAt := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
				clock := invokedAt.Add(window - time.Second)
				landing := invokedAt.Add(window + hookClaimMutationTimeout - time.Second)
				if outcome == "late CAS" {
					landing = invokedAt.Add(window + hookClaimMutationTimeout + time.Second)
				}
				env := []string{"BEADS_DIR=/rig/.beads", "GC_RIG_ROOT=/rig"}
				original := hookClaimCommandRunnerWithEnvContext
				t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = original })
				mutations, reads := 0, 0
				hookClaimCommandRunnerWithEnvContext = func(ctx context.Context, bound map[string]string) beads.CommandRunner {
					if !reflect.DeepEqual(bound, hookClaimEnvMap(env, "/rig", "worker-1")) {
						t.Fatalf("claim/read environment changed: %#v", bound)
					}
					return func(dir, name string, args ...string) ([]byte, error) {
						if dir != "/rig" || name != "bd" {
							t.Fatalf("command binding = (%q, %q)", dir, name)
						}
						switch {
						case reflect.DeepEqual(args, []string{"update", "work-1", "--claim", "--json"}):
							mutations++
							clock = landing
							return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1"}]`), nil
						case reflect.DeepEqual(args, []string{"show", "--json", "work-1"}):
							reads++
							if ctx.Err() != nil {
								t.Fatalf("canonical read context failed: %v", ctx.Err())
							}
							clock = clock.Add(5 * time.Second)
							if outcome == "failed readback" {
								return nil, errors.New("canonical verification unavailable")
							}
							return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`), nil
						default:
							t.Fatalf("unexpected or replayed command: %q", args)
							return nil, errors.New("unexpected command")
						}
					}
				}
				rec := &turnBoundClaimRecorder{}
				ops := rec.ops(t, "")
				ops.Claim = hookClaimWithBdStore
				ops.StoreRef = func(string) string { return "rig:fixture" }
				ops.Now = func() time.Time { return clock }
				ops.InvokedAt, ops.ClaimWindow = invokedAt, window
				opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, Env: env, JSON: true}
				candidates := []beads.Bead{
					{ID: "work-1", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
					{ID: "work-2", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
				}
				var stdout, stderr bytes.Buffer
				var result hookClaimResult
				if assigned {
					for i := range candidates {
						candidates[i].Assignee = "worker-1"
					}
					result = claimFirstReadyHookAssignment(candidates, opts, ops, "/rig", &stdout, &stderr)
				} else {
					result = claimFirstEligibleHookCandidate(candidates, opts, ops, "/rig", &stdout, &stderr)
				}
				if mutations != 1 || reads != 1 || !result.terminal {
					t.Fatalf("result=%+v mutations=%d reads=%d; want one CAS and one read", result, mutations, reads)
				}
				if clock.Sub(invokedAt) <= window+hookClaimMutationTimeout {
					t.Fatal("fixture did not cross the landing fence during canonical readback")
				}
				switch outcome {
				case "timely CAS":
					if result.code != 0 || len(rec.releases) != 0 || decodeTurnBoundResult(t, stdout.String()).BeadID != "work-1" {
						t.Fatalf("timely CAS refused: result=%+v releases=%v stdout=%q stderr=%s", result, rec.releases, stdout.String(), stderr.String())
					}
				case "late CAS":
					if result.code != 1 || stdout.Len() != 0 || !reflect.DeepEqual(rec.releases, []string{"work-1"}) || len(rec.claimReleased) != 1 || rec.claimReleased[0].Reason != hookClaimReleaseReasonStraddled {
						t.Fatalf("late CAS not unwound: result=%+v releases=%v events=%+v stdout=%q stderr=%s", result, rec.releases, rec.claimReleased, stdout.String(), stderr.String())
					}
				case "failed readback":
					if result.code != 1 || stdout.Len() != 0 || len(rec.releases) != 0 || !strings.Contains(stderr.String(), "claimed work-1 but loading canonical bead failed") {
						t.Fatalf("committed-but-unconfirmed claim lost: result=%+v releases=%v stdout=%q stderr=%s", result, rec.releases, stdout.String(), stderr.String())
					}
				}
				if rec.drainAcked {
					t.Fatal("claimed work must not be acknowledged as idle")
				}
			})
		}
	}
}

// Consumer-local class-store double: only the mutation and canonical read are
// controlled; the existing graph adapter and claim router remain in the path.
type hookClaimLandingClassStore struct {
	beads.Store
	claim func(string, string) (beads.Bead, bool, error)
	get   func(string) (beads.Bead, error)
}

// Claim completes the controlled class-store mutation.
func (s *hookClaimLandingClassStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	return s.claim(id, assignee)
}

// Get returns the controlled canonical row.
func (s *hookClaimLandingClassStore) Get(id string) (beads.Bead, error) { return s.get(id) }

func TestClassRoutedClaimCapturesLandingBeforeCanonicalReadback(t *testing.T) {
	for _, outcome := range []string{"canonical owner", "failed readback", "conflict", "foreign projection", "foreign canonical owner"} {
		t.Run(outcome, func(t *testing.T) {
			clock := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
			landing := clock.Add(7 * time.Second)
			mutations, reads := 0, 0
			readErr := errors.New("canonical verification unavailable")
			store := &hookClaimLandingClassStore{Store: beads.NewMemStore()}
			store.claim = func(id, assignee string) (beads.Bead, bool, error) {
				mutations++
				clock = landing
				if outcome == "foreign projection" {
					assignee = "other-1"
				}
				return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee}, outcome != "conflict", nil
			}
			store.get = func(id string) (beads.Bead, error) {
				reads++
				clock = clock.Add(5 * time.Second)
				if outcome == "failed readback" {
					return beads.Bead{}, readErr
				}
				owner := "worker-1"
				if outcome == "conflict" || outcome == "foreign canonical owner" {
					owner = "other-1"
				}
				return beads.Bead{ID: id, Status: "in_progress", Assignee: owner, Metadata: map[string]string{"canonical": "retained"}}, nil
			}
			route, err := newHookClaimClassRoute(store)
			if err != nil {
				t.Fatal(err)
			}
			route.resident["work-1"] = true
			ops := classRoutedHookClaimOps(hookClaimOps{
				Now: func() time.Time { return clock },
				Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
					t.Fatal("resident class claim reached the work store")
					return beads.Bead{}, false, errors.New("unexpected work claim")
				},
			}, route)
			claimed, ok, landedAt, err := ops.claimWithLanding(context.Background(), "/rig", nil, "work-1", "worker-1")
			wantOK := outcome == "canonical owner" || outcome == "failed readback"
			wantReads := 1
			if outcome == "foreign projection" {
				wantReads = 0
			}
			if ok != wantOK || mutations != 1 || reads != wantReads {
				t.Fatalf("claim=(%+v,%v,%v) mutations=%d reads=%d", claimed, ok, err, mutations, reads)
			}
			if outcome != "conflict" && !landedAt.Equal(landing) {
				t.Fatalf("landing=%s, want successful CAS completion %s", landedAt, landing)
			}
			if outcome == "failed readback" {
				if !errors.Is(err, readErr) {
					t.Fatalf("committed read failure = %v, want %v", err, readErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if outcome == "canonical owner" && claimed.Metadata["canonical"] != "retained" {
				t.Fatal("canonical metadata lost")
			}
			if !wantOK && claimed.Assignee != "other-1" {
				t.Fatalf("rejection lost foreign owner: %+v", claimed)
			}
		})
	}
}
