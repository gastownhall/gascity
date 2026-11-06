package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestHookClaimSkipsHandedOffEligibleCandidate pins the handoff fence: an
// unassigned, route-matched bead whose handoff_stage marker is set has already
// been handed off, so a fresh `gc hook --claim` must leave it untouched and
// claim the next ready bead instead.
func TestHookClaimSkipsHandedOffEligibleCandidate(t *testing.T) {
	const (
		identity  = "gk__polecat-1"
		handedOff = "gk-nmxnj"
		readyID   = "gk-ready"
	)
	runner := func(string, string) (string, error) {
		return `[
			{"id":"` + handedOff + `","status":"open","issue_type":"task","assignee":"","metadata":{"gc.routed_to":"` + identity + `","handoff_stage":"target_recorded"}},
			{"id":"` + readyID + `","status":"open","issue_type":"task","assignee":"","metadata":{"gc.routed_to":"` + identity + `"}}
		]`, nil
	}
	claimedID := ""
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			if id == handedOff {
				t.Fatalf("store.Claim called for handed-off bead %q; a set handoff_stage marker must refuse the claim", id)
			}
			claimedID = id
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           identity,
		IdentityCandidates: hookClaimIdentityCandidates(identity),
		RouteTargets:       hookClaimRouteTargets(identity),
		JSON:               true,
	}
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.BeadID != readyID || claimedID != readyID {
		t.Fatalf("want ready bead %q served as work, got action=%q reason=%q bead=%q claimed=%q",
			readyID, result.Action, result.Reason, result.BeadID, claimedID)
	}
}
