package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The assigned-ready tier is federated: `gc ready --assignee` reads city-wide,
// so a graph step assigned to this worker in a RELOCATED class store arrives on
// this tier while the claim still runs against the agent's work-directory bd
// context, which cannot resolve that id. These cases pin what the hook does with
// that shape.

// assignedGraphRow is the split-city shape the federated assigned tier serves:
// an OPEN graph step assigned to this worker, living in a store the claim's bd
// context cannot reach.
const assignedGraphRow = `[{"id":"gcg-abc123","status":"open","assignee":"worker-1","metadata":{"gc.kind":"wisp"}}]`

// routedWorkRow is ordinary claimable routed work in a DIFFERENT store.
const routedWorkRow = `[{"id":"hw-riga","status":"open","metadata":{"gc.routed_to":"worker"}}]`

func assignedFederationClaimOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}
}

// TestClaimHookWorkAssignedTierUnresolvableBeadDoesNotStrandLaterStore is the
// load-bearing case. bestStoreWithWork RANKS an assigned row ahead of a routed
// one (hookTierAssigned < hookTierRouted), so the store holding the unclaimable
// graph step is SELECTED ahead of the store holding genuinely claimable work. If
// the assigned tier answers a not-found claim with a terminal exit 1, the
// federated drop-and-retry loop never runs and every other store's work is
// stranded behind one permanently unclaimable id.
func TestClaimHookWorkAssignedTierUnresolvableBeadDoesNotStrandLaterStore(t *testing.T) {
	stores := []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}},
	}
	run := func(_, dir string, _ []string) (string, error) {
		switch dir {
		case "city":
			return assignedGraphRow, nil
		case "riga":
			return routedWorkRow, nil
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return "", nil
		}
	}
	var attempts []string
	var claimDir string
	ops := hookClaimOps{
		Claim: func(_ context.Context, dir string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			attempts = append(attempts, beadID)
			if beadID == "gcg-abc123" {
				// BdStore.Claim's not-found path: bd update --claim against a
				// store that does not hold the id.
				return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", beadID, beads.ErrNotFound)
			}
			claimDir = dir
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
		DrainAck:          func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("gc ready --json", "city", stores[0].env, stores, assignedFederationClaimOpts(), ops, run, func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0: an assigned graph step this store cannot resolve must not exit before the store holding claimable work is tried; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %q", err, stdout.String())
	}
	if result.BeadID != "hw-riga" || result.Reason != "claimed" {
		t.Fatalf("claim result = %+v, want hw-riga claimed (the later store's work must still be reachable)", result)
	}
	if claimDir != "riga" {
		t.Fatalf("winning claim ran against dir %q, want riga", claimDir)
	}
	if got := strings.Join(attempts, ","); got != "gcg-abc123,hw-riga" {
		t.Fatalf("claim attempts = %q, want gcg-abc123,hw-riga (the unresolvable assigned id is skipped, not fatal)", got)
	}
}

// TestClaimHookWorkAssignedTierUnresolvableBeadDrainsClaimsErrored pins the
// contract the KNOWN GAP paragraph promises for this tier when there is nothing
// else to do: a structured drain whose reason names the write failure, not a
// bare exit 1 with an empty stdout that a --json caller cannot read.
func TestClaimHookWorkAssignedTierUnresolvableBeadDrainsClaimsErrored(t *testing.T) {
	stores := []hookStore{{dir: "city", env: []string{"GC_STORE=city"}}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir != "city" {
			t.Fatalf("unexpected store dir %q", dir)
		}
		return assignedGraphRow, nil
	}
	drained := false
	ops := hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", beadID, beads.ErrNotFound)
		},
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
		DrainAck: func(io.Writer) error {
			drained = true
			return nil
		},
	}

	emitted := false
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("gc ready --json", "city", stores[0].env, stores, assignedFederationClaimOpts(), ops, run, func(string, error) { emitted = true }, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0 (structured drain); stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !drained {
		t.Fatal("drain ack was not called: the assigned tier exited without completing the drain contract")
	}
	if emitted {
		t.Fatal("a skipped claim error is not a work-query failure and must not emit one")
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %q", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonClaimsErrored {
		t.Fatalf("claim result = %+v, want drain/claims_errored", result)
	}
}

// TestClaimHookWorkAssignedTierOperationalErrorStaysTerminal is the other half
// of the fix: only an error that proves the bead is NOT in this store is
// downgraded. A transient write failure leaves ownership unresolved, so the
// assigned tier must still fail closed rather than skip its own bead and claim
// unrelated fresh work.
func TestClaimHookWorkAssignedTierOperationalErrorStaysTerminal(t *testing.T) {
	stores := []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}},
	}
	run := func(_, dir string, _ []string) (string, error) {
		switch dir {
		case "city":
			return assignedGraphRow, nil
		case "riga":
			return routedWorkRow, nil
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return "", nil
		}
	}
	var attempts []string
	ops := hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			attempts = append(attempts, beadID)
			return beads.Bead{}, false, fmt.Errorf("claiming bead %q: store write timeout", beadID)
		},
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
		DrainAck:          func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("gc ready --json", "city", stores[0].env, stores, assignedFederationClaimOpts(), ops, run, func(string, error) {}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 1: an unresolved mutation failure on this session's OWN assigned bead must fail closed, not claim unrelated work; stdout=%q", code, stdout.String())
	}
	if got := strings.Join(attempts, ","); got != "gcg-abc123" {
		t.Fatalf("claim attempts = %q, want gcg-abc123 only (no fresh claim after an unresolved mutation failure)", got)
	}
	if !strings.Contains(stderr.String(), "promoting ready assignment gcg-abc123") {
		t.Fatalf("stderr = %q, want the failed promotion named", stderr.String())
	}
}
