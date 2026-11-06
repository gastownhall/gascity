package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
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

// liveOwnershipStore is the workflow store, counting the member listings the
// hold makes: one entry per List, naming the root it filtered on.
type liveOwnershipStore struct {
	*beads.MemStore
	lists []string
}

func (s *liveOwnershipStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists = append(s.lists, query.Metadata[beadmeta.RootBeadIDMetadataKey])
	return s.MemStore.List(query)
}

// liveOwnershipOutcome is what one `gc hook --claim` did: the decoded
// result, the claim CASes attempted in order, stderr, and the workflow store
// the members were listed from.
type liveOwnershipOutcome struct {
	result   hookClaimJSONResult
	claims   []string
	stderr   string
	workflow *liveOwnershipStore
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
func liveOwnershipRun(t *testing.T, rows []beads.Bead, members []beads.Bead, opts hookClaimOptions, ops hookClaimOps) liveOwnershipOutcome {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	workflow := &liveOwnershipStore{MemStore: beads.NewMemStoreFrom(0, members, nil)}
	var claims []string
	ops.Runner = func(string, string) (string, error) { return string(raw), nil }
	ops.Claim = func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
		claims = append(claims, id)
		return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
	}
	if ops.WorkflowStore == nil {
		ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store { return workflow }
	}
	if ops.EmitClaimFailed == nil {
		ops.EmitClaimFailed = func(string, hookClaimOptions, error) {}
	}
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
		members, liveOwnershipOptions(), hookClaimOps{})

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
				tc.members, liveOwnershipOptions(), hookClaimOps{})
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
			if out := liveOwnershipRun(t, rows, members, liveOwnershipOptions(), hookClaimOps{}); out.claimed(liveOwnershipRoot) {
				t.Fatalf("root claimed while its member is held: claims=%v", out.claims)
			}
			members[0].Status = settle
			if settle == "open" {
				members[0].Assignee = ""
			}
			if out := liveOwnershipRun(t, rows, members, liveOwnershipOptions(), hookClaimOps{}); out.result.BeadID != liveOwnershipRoot {
				t.Fatalf("want the root served once its member is %s, got bead=%q claims=%v\nstderr: %s",
					settle, out.result.BeadID, out.claims, out.stderr)
			}
		})
	}
}

// TestHookClaimLiveOwnershipScopeAndReads pins the hold's scope: only an
// unstamped workflow root with no root above it and no drain-control link is
// listed, so every other candidate costs the hook no store read, and a root
// costs exactly one member List. The ready tier is outside the hold: a root
// already assigned to this session is its own.
func TestHookClaimLiveOwnershipScopeAndReads(t *testing.T) {
	const parent = "gk-parent"
	withMeta := func(b beads.Bead, meta map[string]string) beads.Bead {
		for k, v := range meta {
			b.Metadata[k] = v
		}
		return b
	}
	cases := []struct {
		label     string
		candidate beads.Bead
		lists     int
		served    bool
	}{
		{"plain task", withMeta(liveOwnershipTaskBead(), map[string]string{}), 0, true},
		{"workflow root under another root", withMeta(liveOwnershipRootBead(), map[string]string{beadmeta.RootBeadIDMetadataKey: parent}), 0, true},
		{"drain-control item root", withMeta(liveOwnershipRootBead(), map[string]string{beadmeta.DrainControlIDMetadataKey: parent}), 0, true},
		{"expanded workflow root", withMeta(liveOwnershipRootBead(), map[string]string{beadmeta.WorkflowExpandedMetadataKey: "true"}), 0, false},
		{"root assigned to this session", func() beads.Bead {
			b := liveOwnershipRootBead()
			b.Assignee = liveOwnershipIdentity
			return b
		}(), 0, true},
		{"self-rooted workflow root", withMeta(liveOwnershipRootBead(), map[string]string{beadmeta.RootBeadIDMetadataKey: liveOwnershipRoot}), 1, false},
		{"standalone workflow root", liveOwnershipRootBead(), 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			// A foreign-held step under the candidate: had the hold read it,
			// the candidate would have been withheld.
			members := []beads.Bead{
				{ID: parent, Status: "open", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
				{ID: "gk-step", Status: "in_progress", Type: "task", Assignee: "gk__polecat-2", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: tc.candidate.ID}},
			}
			out := liveOwnershipRun(t, []beads.Bead{tc.candidate}, members, liveOwnershipOptions(), hookClaimOps{})
			if got := len(out.workflow.lists); got != tc.lists {
				t.Fatalf("member Lists = %d %v, want %d", got, out.workflow.lists, tc.lists)
			}
			if tc.lists == 1 && out.workflow.lists[0] != tc.candidate.ID {
				t.Fatalf("member List filtered on root %q, want %q", out.workflow.lists[0], tc.candidate.ID)
			}
			if served := out.result.Action == "work" && out.result.BeadID == tc.candidate.ID; served != tc.served {
				t.Fatalf("candidate served=%v, want %v: action=%q reason=%q bead=%q\nstderr: %s",
					served, tc.served, out.result.Action, out.result.Reason, out.result.BeadID, out.stderr)
			}
		})
	}
}

// liveOwnershipSplit is a split city: a work store and the relocated graph
// binding, with a workflow root and a foreign-held member step minted in the
// binding.
type liveOwnershipSplit struct {
	work    *liveOwnershipStore
	binding beads.Store
	route   *hookClaimClassRoute
	root    beads.Bead
	claims  []string
	readErr error
}

func newLiveOwnershipSplit(t *testing.T) *liveOwnershipSplit {
	t.Helper()
	f := &liveOwnershipSplit{
		work:    &liveOwnershipStore{MemStore: beads.NewMemStoreFrom(0, nil, nil)},
		binding: newSplitEnvClassLeaf(t),
	}
	var err error
	f.root, err = f.binding.Create(beads.Bead{Title: "workflow root", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:     beadmeta.KindWorkflow,
		beadmeta.RoutedToMetadataKey: liveOwnershipRoute,
	}})
	if err != nil {
		t.Fatal(err)
	}
	step, err := f.binding.Create(beads.Bead{Title: "running step", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: f.root.ID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	status, holder := "in_progress", "gk__polecat-2"
	if err := f.binding.Update(step.ID, beads.UpdateOpts{Status: &status, Assignee: &holder}); err != nil {
		t.Fatal(err)
	}
	f.route = newClaimRouteFor(t, f.binding)
	return f
}

// baseOps are the work-scope seams over the work store, the way the default
// seams run them over the agent's bd store. The work-scope claim records the
// ids it is asked for and proves absence the way bd does.
func (f *liveOwnershipSplit) baseOps() hookClaimOps {
	return hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			f.claims = append(f.claims, id)
			if _, err := f.work.Get(id); err != nil {
				return beads.Bead{}, false, err
			}
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee}, true, nil
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			if f.readErr != nil {
				return beads.Bead{}, f.readErr
			}
			return f.work.Get(id)
		},
		WorkflowStore:   func(context.Context, string, []string, string, string) beads.Store { return f.work },
		EmitClaimFailed: func(string, hookClaimOptions, error) {},
	}
}

// TestClassRoutedWorkflowMembersFollowRootResidence pins the hold on a split
// city. A graph.v2 root and its steps live in the relocated binding; before a
// claim nothing is memoized resident, so the stop gate's memo-only workflow
// store would list them from the work store and find none. The member
// listing follows the root's residence instead, by the same read the
// claim-time seams use: the work leg first, the binding on a proven absence.
func TestClassRoutedWorkflowMembersFollowRootResidence(t *testing.T) {
	t.Run("graph-resident root is withheld without a claim CAS", func(t *testing.T) {
		f := newLiveOwnershipSplit(t)
		ops := classRoutedHookClaimOps(f.baseOps(), f.route)
		var stdout, stderr bytes.Buffer
		res := claimFirstEligibleHookCandidate([]beads.Bead{f.root}, liveOwnershipOptions(), ops, "/work", &stdout, &stderr)
		if res.terminal || res.claimsErrored {
			t.Fatalf("eligible tier = %+v, want a non-terminal, error-free withhold\nstdout: %s\nstderr: %s", res, stdout.String(), stderr.String())
		}
		if len(f.claims) != 0 {
			t.Fatalf("claim CAS attempted for %v; the root's steps are held in the binding", f.claims)
		}
		root, err := f.binding.Get(f.root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if root.Status != "open" || root.Assignee != "" {
			t.Fatalf("binding root status=%q assignee=%q, want it left open and unassigned", root.Status, root.Assignee)
		}
		if len(f.work.lists) != 0 {
			t.Fatalf("members listed from the work store %v; the root resides in the binding", f.work.lists)
		}
	})

	t.Run("graph-resident members are listed from the binding", func(t *testing.T) {
		f := newLiveOwnershipSplit(t)
		ops := classRoutedHookClaimOps(f.baseOps(), f.route)
		members, err := ops.listWorkflowMembers(context.Background(), "/work", nil, f.root.ID)
		if err != nil || len(members) != 1 || members[0].Assignee != "gk__polecat-2" {
			t.Fatalf("listWorkflowMembers(%s) = %+v, %v; want the binding's held step", f.root.ID, members, err)
		}
	})

	t.Run("work-resident root keeps the work listing", func(t *testing.T) {
		f := newLiveOwnershipSplit(t)
		f.work.HonorExplicitIDs = true
		for _, b := range []beads.Bead{liveOwnershipRootBead(), liveOwnershipMember("gk-step", "in_progress", "gk__polecat-2", nil)} {
			if _, err := f.work.Create(b); err != nil {
				t.Fatal(err)
			}
		}
		ops := classRoutedHookClaimOps(f.baseOps(), f.route)
		if _, err := ops.listWorkflowMembers(context.Background(), "/work", nil, liveOwnershipRoot); err != nil {
			t.Fatal(err)
		}
		if len(f.work.lists) != 1 || f.work.lists[0] != liveOwnershipRoot {
			t.Fatalf("work listings = %v, want exactly one for %s", f.work.lists, liveOwnershipRoot)
		}
	})

	t.Run("an operational work read error does not escalate", func(t *testing.T) {
		f := newLiveOwnershipSplit(t)
		f.readErr = errors.New("bd: connection refused")
		ops := classRoutedHookClaimOps(f.baseOps(), f.route)
		members, err := ops.listWorkflowMembers(context.Background(), "/work", nil, liveOwnershipRoot)
		if err != nil || len(members) != 0 {
			t.Fatalf("listWorkflowMembers = %+v, %v; want the work leg's (empty) answer", members, err)
		}
		if len(f.work.lists) != 1 {
			t.Fatalf("work listings = %v; a failed residence read must leave the work leg in charge", f.work.lists)
		}
	})
}

// TestHookClaimLiveOwnershipUnavailable pins the hold's availability trade,
// the stop gate's: a member listing that cannot be made is evidence of
// nothing, so the hook says so on stderr and proceeds to the claim it would
// have attempted without the hold, rather than idling every seat behind one
// store fault.
func TestHookClaimLiveOwnershipUnavailable(t *testing.T) {
	held := []beads.Bead{liveOwnershipMember("gk-step", "in_progress", "gk__polecat-2", nil)}
	rows := []beads.Bead{liveOwnershipRootBead(), liveOwnershipTaskBead()}
	const disclosure = "foreign workflow hold check unavailable for " + liveOwnershipRoot + ": "

	t.Run("member listing fails", func(t *testing.T) {
		fault := errors.New("bd: database is locked")
		out := liveOwnershipRun(t, rows, held, liveOwnershipOptions(), hookClaimOps{
			ListWorkflowMembers: func(context.Context, string, []string, string) ([]beads.Bead, error) {
				return nil, fault
			},
		})
		if out.result.BeadID != liveOwnershipRoot || !out.claimed(liveOwnershipRoot) {
			t.Fatalf("want the root claimed as without the hold, got bead=%q claims=%v", out.result.BeadID, out.claims)
		}
		if !strings.Contains(out.stderr, disclosure) || !strings.Contains(out.stderr, fault.Error()) || !strings.Contains(out.stderr, "; proceeding") {
			t.Fatalf("stderr does not disclose the unavailable check:\n%s", out.stderr)
		}
	})

	t.Run("workflow store opens nothing", func(t *testing.T) {
		out := liveOwnershipRun(t, rows, held, liveOwnershipOptions(), hookClaimOps{
			WorkflowStore: func(context.Context, string, []string, string, string) beads.Store { return nil },
		})
		if out.result.BeadID != liveOwnershipRoot || !strings.Contains(out.stderr, disclosure) {
			t.Fatalf("want the root claimed with the disclosure, got bead=%q\nstderr: %s", out.result.BeadID, out.stderr)
		}
	})

	t.Run("no listing seam and no workflow store", func(t *testing.T) {
		var claims []string
		ops := hookClaimOps{
			Claim: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
				claims = append(claims, id)
				return beads.Bead{}, false, errors.New("store refused the write")
			},
			EmitClaimFailed: func(string, hookClaimOptions, error) {},
		}
		var stdout, stderr bytes.Buffer
		res := claimFirstEligibleHookCandidate([]beads.Bead{liveOwnershipRootBead()}, liveOwnershipOptions(), ops, "/work", &stdout, &stderr)
		if len(claims) != 1 || claims[0] != liveOwnershipRoot || !res.claimsErrored {
			t.Fatalf("want the root's claim attempted as without the hold, got claims=%v result=%+v", claims, res)
		}
		if !strings.Contains(stderr.String(), disclosure) {
			t.Fatalf("stderr does not disclose the unavailable check:\n%s", stderr.String())
		}
	})
}

// TestHookClaimLiveOwnershipLoopReporting pins how a withheld root reaches
// the claim-loop guard. The controller keeps counting the root as routed
// demand while its steps run, so every seat spawned for it would drain; each
// withhold is reported as bead.claim_failed against the ROOT, with text that
// names no member and no count, so distinct seats seeing different members
// still repeat one error and the guard trips. The drain stays a healthy
// no_work: the withhold is not a write failure.
func TestHookClaimLiveOwnershipLoopReporting(t *testing.T) {
	const template = "gk.polecat"
	h := newClaimLoopHarness(t)
	var texts []string
	for seat := 1; seat <= claimLoopTripFailures; seat++ {
		opts := liveOwnershipOptions()
		opts.Env = []string{"GC_TEMPLATE=" + template, fmt.Sprintf("GC_SESSION_ID=gcg-session-%d", seat)}
		members := []beads.Bead{liveOwnershipMember(fmt.Sprintf("gk-step-%d", seat), "in_progress", fmt.Sprintf("gk__polecat-%d", seat+10), nil)}
		var reported []string
		out := liveOwnershipRun(t, []beads.Bead{liveOwnershipRootBead()}, members, opts, hookClaimOps{
			EmitClaimFailed: func(beadID string, opts hookClaimOptions, err error) {
				reported = append(reported, beadID)
				texts = append(texts, err.Error())
				evt, ok := claimFailedEvent(beadID, opts, err)
				if !ok {
					t.Fatalf("claimFailedEvent(%s) recorded nothing for a seat of %s", beadID, template)
				}
				evt.Ts = h.now
				h.guard.observe(evt)
			},
		})
		if len(out.claims) != 0 {
			t.Fatalf("seat %d attempted claim CASes %v", seat, out.claims)
		}
		if out.result.Action != "drain" || out.result.Reason != "no_work" {
			t.Fatalf("seat %d result action=%q reason=%q, want a healthy no_work drain\nstderr: %s", seat, out.result.Action, out.result.Reason, out.stderr)
		}
		if len(reported) != 1 || reported[0] != liveOwnershipRoot {
			t.Fatalf("seat %d reported claim failures for %v, want exactly the root %s", seat, reported, liveOwnershipRoot)
		}
		held := func() bool {
			counts, demand := h.guard.withhold(
				map[string]int{template: 1},
				map[string]scaleCheckDemand{template: {Count: 1, WorkBeadIDs: []string{liveOwnershipRoot}}})
			return counts[template] == 0 && len(demand[template].WorkBeadIDs) == 0
		}()
		if want := seat == claimLoopTripFailures; held != want {
			t.Fatalf("after seat %d the root is withheld from default demand = %v, want %v", seat, held, want)
		}
		h.advance(time.Minute)
	}
	for _, text := range texts {
		if text != texts[0] {
			t.Fatalf("withhold texts differ between seats: %q", texts)
		}
	}
	if want := "workflow root " + liveOwnershipRoot + " withheld: member steps in_progress under other sessions"; texts[0] != want {
		t.Fatalf("withhold text = %q, want %q", texts[0], want)
	}
	// A write to the root resumes its demand, as for any held bead.
	h.guard.observe(events.Event{Type: events.BeadUpdated, Subject: liveOwnershipRoot, Ts: h.now})
	if h.records() != 0 {
		t.Fatalf("guard still tracks %d records after the root changed", h.records())
	}
}
