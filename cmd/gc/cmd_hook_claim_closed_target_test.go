package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// hookClaimRefusalRun is one doHookClaim over routed candidates whose claims
// are all refused (ok=false, nil error) with the readback refusals names.
type hookClaimRefusalRun struct {
	result   hookClaimJSONResult
	claimed  []string
	failed   map[string]error
	rejected []string
	stderr   string
}

func runHookClaimRefusals(t *testing.T, ids []string, refusals map[string]beads.Bead) hookClaimRefusalRun {
	t.Helper()
	var rows []beads.Bead
	for _, id := range ids {
		rows = append(rows, beads.Bead{ID: id, Status: "open", Metadata: map[string]string{"gc.routed_to": "rig/worker"}})
	}
	candidates, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	run := hookClaimRefusalRun{failed: map[string]error{}}
	ops := hookClaimOps{
		Runner:   func(string, string) (string, error) { return string(candidates), nil },
		DrainAck: func(io.Writer) error { return nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			run.claimed = append(run.claimed, beadID)
			return refusals[beadID], false, nil
		},
		EmitClaimFailed:   func(beadID string, _ hookClaimOptions, err error) { run.failed[beadID] = err },
		EmitClaimRejected: func(beadID, _, _ string) { run.rejected = append(run.rejected, beadID) },
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:           "rig/worker-1",
		IdentityCandidates: []string{"rig/worker-1"},
		RouteTargets:       []string{"rig/worker"},
		Env:                []string{"GC_TEMPLATE=rig/worker"},
		DrainAck:           true,
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &run.result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	run.stderr = stderr.String()
	return run
}

// TestHookClaimClosedCanonicalRowIsAClaimFailure (#5857): a routed candidate
// whose claim CAS is refused because the canonical row the claim resolved is
// closed — the owning store's copy of a stale replica the reader served — is
// not a race lost to a live claimant. Every seat spawned for the row will be
// refused the same way, so it is reported as a claim failure (the claim-loop
// guard's input), the next candidate is still tried, and the seat drains
// claims_errored.
func TestHookClaimClosedCanonicalRowIsAClaimFailure(t *testing.T) {
	run := runHookClaimRefusals(t, []string{"canary-x", "work-y"}, map[string]beads.Bead{
		"canary-x": {ID: "canary-x", Status: "closed", Assignee: "other-session"},
	})
	if want := []string{"canary-x", "work-y"}; !slices.Equal(run.claimed, want) {
		t.Fatalf("claims tried = %v, want %v: the closed row must not stop the next candidate", run.claimed, want)
	}
	if err, ok := run.failed["canary-x"]; !ok || !errors.Is(err, errHookClaimTargetClosed) || err.Error() != errHookClaimTargetClosed.Error() {
		t.Fatalf("claim failures = %v, want canary-x with %q", run.failed, errHookClaimTargetClosed)
	}
	if len(run.failed) != 1 {
		t.Fatalf("claim failures = %v, want only canary-x", run.failed)
	}
	if slices.Contains(run.rejected, "canary-x") {
		t.Fatalf("claim_rejected emitted for canary-x: a closed row is not held by a live claimant")
	}
	if run.result.Action != "drain" || run.result.Reason != hookClaimReasonClaimsErrored {
		t.Fatalf("claim result = %+v, want drain/%s", run.result, hookClaimReasonClaimsErrored)
	}
	if !strings.Contains(run.stderr, "skipping canary-x") || !strings.Contains(run.stderr, "other-session") {
		t.Fatalf("stderr = %q, want the skip of canary-x naming its holder", run.stderr)
	}
}
