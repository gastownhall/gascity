package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// These rows pin the foreign workflow hold on `gc hook --claim`'s eligible
// tier: an unstamped standalone workflow root is the latch a pool seat is
// handed whole, and neither ancestry gate looks DOWN at its member steps. A
// root whose steps are in_progress under other sessions is withheld — left
// routed and untouched — and the seat moves on to the rest of its pool work.

const (
	liveOwnershipIdentity = "gk__polecat-1"
	liveOwnershipRoute    = "gk/polecat"
	liveOwnershipRoot     = "gk-root"
	liveOwnershipTask     = "gk-task"
)

// liveOwnershipRootBead is an open, unassigned, route-matched graph.v2 root
// without the gc.workflow_expanded stamp: the shape the route matcher's
// run_target fallback and the eligible tier still hand out whole.
func liveOwnershipRootBead() beads.Bead {
	return beads.Bead{
		ID:     liveOwnershipRoot,
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:     beadmeta.KindWorkflow,
			beadmeta.RoutedToMetadataKey: liveOwnershipRoute,
		},
	}
}

func liveOwnershipTaskBead() beads.Bead {
	return beads.Bead{
		ID:       liveOwnershipTask,
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: liveOwnershipRoute},
	}
}

// liveOwnershipMember is a step of the root, as the workflow store lists it.
func liveOwnershipMember(id, status, assignee string, meta map[string]string) beads.Bead {
	metadata := map[string]string{beadmeta.RootBeadIDMetadataKey: liveOwnershipRoot}
	for k, v := range meta {
		metadata[k] = v
	}
	return beads.Bead{ID: id, Status: status, Type: "task", Assignee: assignee, Metadata: metadata}
}

func liveOwnershipOptions() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           liveOwnershipIdentity,
		IdentityCandidates: hookClaimIdentityCandidates(liveOwnershipIdentity, "gcg-session-own", "polecat-own"),
		RouteTargets:       hookClaimRouteTargets(liveOwnershipRoute),
		JSON:               true,
	}
}

// liveOwnershipOutcome is what one `gc hook --claim` did: the decoded
// result, the claim CASes attempted in order, stderr, and the workflow store
// the members were listed from.
type liveOwnershipOutcome struct {
	result   hookClaimJSONResult
	claims   []string
	stderr   string
	workflow *beads.MemStore
}

// claimed reports whether a claim CAS ran for id.
func (o liveOwnershipOutcome) claimed(id string) bool {
	for _, claim := range o.claims {
		if claim == id {
			return true
		}
	}
	return false
}

// liveOwnershipRun is one `gc hook --claim` over the work query rows, with
// the workflow store holding members.
func liveOwnershipRun(t *testing.T, rows []beads.Bead, members []beads.Bead, opts hookClaimOptions) liveOwnershipOutcome {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	workflow := beads.NewMemStoreFrom(0, members, nil)
	var claims []string
	var ops hookClaimOps
	ops.Runner = func(string, string) (string, error) { return string(raw), nil }
	ops.Claim = func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
		claims = append(claims, id)
		return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
	}
	ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store { return workflow }
	ops.EmitClaimFailed = func(string, hookClaimOptions, error) {}
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	return liveOwnershipOutcome{result: result, claims: claims, stderr: stderr.String(), workflow: workflow}
}

// TestHookClaimLiveOwnershipWithholdsAndMovesOn is the incident: the pool
// handed a seat a graph.v2 root whole while two other sessions held its steps
// in_progress. The root must be left unclaimed and the later task served.
func TestHookClaimLiveOwnershipWithholdsAndMovesOn(t *testing.T) {
	members := []beads.Bead{
		liveOwnershipRootBead(),
		liveOwnershipMember("gk-step-1", "in_progress", "gk__polecat-2", nil),
		liveOwnershipMember("gk-step-2", "in_progress", "gk__polecat-3", nil),
		liveOwnershipMember("gk-step-3", "open", "", nil),
	}
	out := liveOwnershipRun(t,
		[]beads.Bead{liveOwnershipRootBead(), liveOwnershipTaskBead()},
		members, liveOwnershipOptions())

	if out.claimed(liveOwnershipRoot) {
		t.Fatalf("claim CAS ran for root %s while its steps are held by other sessions; claims=%v", liveOwnershipRoot, out.claims)
	}
	if !out.claimed(liveOwnershipTask) || out.result.Action != "work" || out.result.BeadID != liveOwnershipTask {
		t.Fatalf("want the later task %s served, got action=%q reason=%q bead=%q\nstderr: %s",
			liveOwnershipTask, out.result.Action, out.result.Reason, out.result.BeadID, out.stderr)
	}
	if !strings.Contains(out.stderr, "gk-step-1, gk-step-2") {
		t.Fatalf("stderr does not name the held members in order:\n%s", out.stderr)
	}
	root, err := out.workflow.Get(liveOwnershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "open" || root.Assignee != "" || root.Metadata[beadmeta.RoutedToMetadataKey] != liveOwnershipRoute {
		t.Fatalf("withheld root changed: status=%q assignee=%q routed_to=%q; want it left open, unassigned and routed",
			root.Status, root.Assignee, root.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// TestHookClaimLiveOwnershipIdentityAndRecovery pins who counts as a foreign
// holder. A member's assignee is authoritative; with none, its claim
// provenance (gc.session_name, gc.session_id) decides, and a member with no
// holder at all is no evidence of foreign ownership. Open and terminal
// siblings never withhold, and the root listing itself is not a member.
func TestHookClaimLiveOwnershipIdentityAndRecovery(t *testing.T) {
	name := beadmeta.SessionNameMetadataKey
	id := beadmeta.SessionIDMetadataKey
	cases := []struct {
		label    string
		members  []beads.Bead
		withheld bool
	}{
		{"no members", nil, false},
		{"own assignee", []beads.Bead{liveOwnershipMember("s1", "in_progress", liveOwnershipIdentity, nil)}, false},
		{"own session alias as assignee", []beads.Bead{liveOwnershipMember("s1", "in_progress", "polecat-own", nil)}, false},
		{"own session_name provenance", []beads.Bead{liveOwnershipMember("s1", "in_progress", "", map[string]string{name: "polecat-own"})}, false},
		{"own session_id provenance", []beads.Bead{liveOwnershipMember("s1", "in_progress", "", map[string]string{id: "gcg-session-own"})}, false},
		{"own session_id despite a different session_name", []beads.Bead{liveOwnershipMember("s1", "in_progress", "", map[string]string{name: "polecat-9", id: "gcg-session-own"})}, false},
		{"no holder at all", []beads.Bead{liveOwnershipMember("s1", "in_progress", "", nil)}, false},
		{"open and closed foreign siblings", []beads.Bead{
			liveOwnershipMember("s1", "open", "gk__polecat-2", nil),
			liveOwnershipMember("s2", "closed", "gk__polecat-3", nil),
		}, false},
		{"root listed in_progress under another session", []beads.Bead{
			{ID: liveOwnershipRoot, Status: "in_progress", Assignee: "gk__polecat-2"},
		}, false},
		{"foreign assignee", []beads.Bead{liveOwnershipMember("s1", "in_progress", "gk__polecat-2", nil)}, true},
		{"foreign assignee wins over own provenance", []beads.Bead{liveOwnershipMember("s1", "in_progress", "gk__polecat-2", map[string]string{name: "polecat-own", id: "gcg-session-own"})}, true},
		{"dead-seat provenance with the assignee cleared", []beads.Bead{liveOwnershipMember("s1", "in_progress", "", map[string]string{name: "polecat-9", id: "gcg-session-9"})}, true},
		{"foreign session_id only", []beads.Bead{liveOwnershipMember("s1", "in_progress", "", map[string]string{id: "gcg-session-9"})}, true},
		{"status spelled loosely", []beads.Bead{liveOwnershipMember("s1", " IN_PROGRESS ", "gk__polecat-2", nil)}, true},
		{"one own, one foreign", []beads.Bead{
			liveOwnershipMember("s1", "in_progress", liveOwnershipIdentity, nil),
			liveOwnershipMember("s2", "in_progress", "gk__polecat-2", nil),
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			out := liveOwnershipRun(t,
				[]beads.Bead{liveOwnershipRootBead(), liveOwnershipTaskBead()},
				tc.members, liveOwnershipOptions())
			want := liveOwnershipRoot
			if tc.withheld {
				want = liveOwnershipTask
			}
			if out.result.Action != "work" || out.result.BeadID != want {
				t.Fatalf("want %s served, got action=%q bead=%q claims=%v\nstderr: %s",
					want, out.result.Action, out.result.BeadID, out.claims, out.stderr)
			}
			if tc.withheld == out.claimed(liveOwnershipRoot) {
				t.Fatalf("root claim CAS ran=%v, want %v; claims=%v", out.claimed(liveOwnershipRoot), !tc.withheld, out.claims)
			}
		})
	}

	// Recovery: the hold lifts as soon as the foreign member settles or is
	// handed back, with nothing on the root changed in between.
	for _, settle := range []string{"closed", "open"} {
		t.Run("member "+settle, func(t *testing.T) {
			members := []beads.Bead{liveOwnershipMember("s1", "in_progress", "gk__polecat-2", nil)}
			rows := []beads.Bead{liveOwnershipRootBead(), liveOwnershipTaskBead()}
			if out := liveOwnershipRun(t, rows, members, liveOwnershipOptions()); out.claimed(liveOwnershipRoot) {
				t.Fatalf("root claimed while its member is held: claims=%v", out.claims)
			}
			members[0].Status = settle
			if settle == "open" {
				members[0].Assignee = ""
			}
			if out := liveOwnershipRun(t, rows, members, liveOwnershipOptions()); out.result.BeadID != liveOwnershipRoot {
				t.Fatalf("want the root served once its member is %s, got bead=%q claims=%v\nstderr: %s",
					settle, out.result.BeadID, out.claims, out.stderr)
			}
		})
	}
}
