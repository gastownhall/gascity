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
		DrainAck: func(io.Writer) error { return nil },
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

// TestPreassignHookContinuationGroupSkipsHeldSiblings is the #6026 regression:
// a continuation sibling carrying a canonical dispatch hold label
// (beadmeta.DispatchHoldLabels) must never be pre-assigned to the claimant,
// even when otherwise eligible (open, unassigned, route-matching). Pinning an
// agent's name onto a bead deliberately parked hold:mayor/hold:external
// contradicts the hold contract: the required next actor is, by construction,
// not this session.
func TestPreassignHookContinuationGroupSkipsHeldSiblings(t *testing.T) {
	for _, label := range beadmeta.DispatchHoldLabels {
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
			if want := []string{"sib-open"}; !reflect.DeepEqual(assigned, want) {
				t.Fatalf("REGRESSION #6026: assigned = %#v, want %#v (held sibling must be skipped, non-held sibling must still be assigned)", assigned, want)
			}
			if want := []string{"sib-open"}; !reflect.DeepEqual(assignedIDs, want) {
				t.Fatalf("REGRESSION #6026: AssignContinuation called for %#v, want %#v; held sibling must never reach the assign mutation", assignedIDs, want)
			}
			if want := []string{"worker-1"}; !reflect.DeepEqual(gotAssignees, want) {
				t.Fatalf("pinned assignees = %#v, want %#v", gotAssignees, want)
			}
		})
	}
}
