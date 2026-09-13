package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Regression coverage for gas-kg6: `gc hook --claim` handed back a bead parked
// on a canonical dispatch hold (beadmeta.DispatchHoldLabels) as action=work.
//
// A hold:external / hold:mayor bead is one whose required next actor is, by
// construction, not this session. The existing/ready-assignment paths keyed off
// status+assignee only and never consulted the hold dimension, so a worker that
// correctly parked its bead was re-served that same bead on every subsequent
// tick and could never reach the ready queue. The jam was triggered BY the
// documented-correct behavior, which is what made it self-reinforcing.
//
// These tests pin the claim-path contract; the work-query shell tier that feeds
// it is pinned separately by internal/config/workquery_inprogress_hold_test.go.
//
// Scope note (ga-5736js): this is the hook's WORK-SERVING decision, not
// demand/liveness accounting. The assignee-scoped tiers that answer "does a
// session still need to exist" (filterReadyByAssignee,
// ephemeralAssignedReadyProbeScript) stay hold-transparent by design and are
// deliberately left untouched.

const holdTestIdentity = "gastown__polecat-az-wisp-ak1hw"

// heldWorkQueryRow renders one work-query row carrying the given hold label.
func heldWorkQueryRow(id, status, assignee, label string) string {
	return `{"id":"` + id + `","status":"` + status + `","issue_type":"task","assignee":"` +
		assignee + `","labels":["` + label + `","mysql-cutover"]}`
}

func holdTestClaimOptions() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           holdTestIdentity,
		IdentityCandidates: hookClaimIdentityCandidates(holdTestIdentity),
		RouteTargets:       hookClaimRouteTargets(holdTestIdentity),
		JSON:               true,
	}
}

// TestHookClaimDoesNotServeHeldExistingAssignment is the primary gas-kg6
// regression: the exact observed sequence. An in_progress bead assigned to this
// session and parked hold:external must not come back as action=work.
func TestHookClaimDoesNotServeHeldExistingAssignment(t *testing.T) {
	for _, label := range beadmeta.DispatchHoldLabels {
		t.Run(label, func(t *testing.T) {
			const heldID = "gas-4cu"
			runner := func(string, string) (string, error) {
				return `[` + heldWorkQueryRow(heldID, "in_progress", holdTestIdentity, label) + `]`, nil
			}
			ops := hookClaimOps{
				Runner: runner,
				Claim: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
					t.Fatalf("store.Claim called for held bead %q; a parked hold must never reach the claim mutation", id)
					return beads.Bead{}, false, nil
				},
			}
			var stdout, stderr bytes.Buffer
			doHookClaim("bd ready --json", "/tmp/work", holdTestClaimOptions(), ops, &stdout, &stderr)

			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
			}
			if result.Action == "work" {
				t.Fatalf("REGRESSION gas-kg6: hook served bead %q parked on %s as action=work (reason=%q); a held bead is not actionable by its assignee",
					result.BeadID, label, result.Reason)
			}
			if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
				t.Fatalf("want action=drain reason=%s for a hook holding only a held bead, got action=%q reason=%q",
					hookClaimReasonNoWork, result.Action, result.Reason)
			}
		})
	}
}

// TestHookClaimSkipsHeldAssignmentAndClaimsReadyWork is the acceptance
// criterion recorded on gas-kg6: a worker that parks hold:external and
// re-claims is handed READY work, not the parked bead.
func TestHookClaimSkipsHeldAssignmentAndClaimsReadyWork(t *testing.T) {
	const (
		heldID  = "gas-4cu"
		readyID = "gas-kuw"
	)
	runner := func(string, string) (string, error) {
		return `[
			` + heldWorkQueryRow(heldID, "in_progress", holdTestIdentity, beadmeta.HoldExternalLabel) + `,
			{"id":"` + readyID + `","status":"open","issue_type":"task","assignee":"","metadata":{"gc.routed_to":"` + holdTestIdentity + `"}}
		]`, nil
	}
	claimedID := ""
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			if id == heldID {
				t.Fatalf("store.Claim called for held bead %q; want the ready bead %q", id, readyID)
			}
			claimedID = id
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
		},
	}
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", holdTestClaimOptions(), ops, &stdout, &stderr)

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.BeadID != readyID || result.Action != "work" {
		t.Fatalf("want ready bead %q served as work, got action=%q reason=%q bead=%q",
			readyID, result.Action, result.Reason, result.BeadID)
	}
	if claimedID != readyID {
		t.Fatalf("store.Claim claimed %q, want %q", claimedID, readyID)
	}
}

// TestHookClaimDoesNotPromoteHeldReadyAssignment covers the second claim path:
// an OPEN bead preassigned to this session and parked on a hold must not be
// promoted to in_progress by claimFirstReadyHookAssignment.
func TestHookClaimDoesNotPromoteHeldReadyAssignment(t *testing.T) {
	const heldID = "gas-held-open"
	runner := func(string, string) (string, error) {
		return `[` + heldWorkQueryRow(heldID, "open", holdTestIdentity, beadmeta.HoldMayorLabel) + `]`, nil
	}
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
			t.Fatalf("store.Claim promoted held ready assignment %q; a parked hold must not be promoted", id)
			return beads.Bead{}, false, nil
		},
	}
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", holdTestClaimOptions(), ops, &stdout, &stderr)

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action == "work" {
		t.Fatalf("REGRESSION gas-kg6: held open assignment %q served as action=work (reason=%q)", result.BeadID, result.Reason)
	}
}

// TestFilterUnreadyHookCandidatesDropsHeldBeads pins the shared defensive
// filter directly — it is the single seam every hook path runs through
// (doHook display, cross-store federation, and the claim).
func TestFilterUnreadyHookCandidatesDropsHeldBeads(t *testing.T) {
	now := time.Now()
	in := `[
		{"id":"held-mayor","status":"in_progress","labels":["` + beadmeta.HoldMayorLabel + `"]},
		{"id":"held-external","status":"in_progress","labels":["` + beadmeta.HoldExternalLabel + `"]},
		{"id":"held-both","status":"open","labels":["` + beadmeta.HoldMayorLabel + `","` + beadmeta.HoldExternalLabel + `"]},
		{"id":"plain","status":"open","labels":["mysql-cutover"]},
		{"id":"nolabels","status":"open"},
		{"id":"nulllabels","status":"open","labels":null}
	]`
	got := filterUnreadyHookCandidates(in, now)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("filtered output is not a JSON array: %v (output %q)", err, got)
	}
	want := []string{"plain", "nolabels", "nulllabels"}
	if len(rows) != len(want) {
		t.Fatalf("filterUnreadyHookCandidates kept %d rows, want %d: %s", len(rows), len(want), got)
	}
	for i, id := range want {
		if rows[i]["id"] != id {
			t.Fatalf("row %d id = %v, want %q (held beads must be dropped, unlabeled work must be kept): %s", i, rows[i]["id"], id, got)
		}
	}
}

// TestFilterUnreadyHookCandidatesKeepsNonHoldLabels is the load-bearing
// negative control: an over-broad "any label starting with hold" or
// "any label at all" filter would strand ordinary labeled work.
func TestFilterUnreadyHookCandidatesKeepsNonHoldLabels(t *testing.T) {
	in := `[{"id":"wk-1","status":"open","labels":["holding-pattern","hold","mpr-human-hold","needs-mayor"]}]`
	got := filterUnreadyHookCandidates(in, time.Now())

	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("filtered output is not a JSON array: %v (output %q)", err, got)
	}
	if len(rows) != 1 || rows[0]["id"] != "wk-1" {
		t.Fatalf("non-canonical hold-ish labels were filtered; only %v are holds. got %s",
			beadmeta.DispatchHoldLabels, got)
	}
}
