package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// --- hookRouteIdentitiesEqual ----------------------------------------------

func TestHookRouteIdentitiesEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical canonical", "gascity/builder", "gascity/builder", true},
		{"canonical vs dash-encoded", "gascity/builder", "gascity--builder", true},
		{"canonical vs dot-encoded", "gastown.mayor", "gastown__mayor", true},
		{"both dash-encoded, same identity", "gascity--builder", "gascity--builder", true},
		{"different rigs", "rig-a/planner", "rig-b/planner", false},
		{"different agents, same rig", "gascity/builder", "gascity/reviewer", false},
		{"empty vs non-empty", "", "gascity/builder", false},
		// A legacy bound-template spelling ("dir/binding.name") is deliberately
		// NOT collapsed onto its unbound form here - that migration is owned by
		// canonicalizeLegacyBoundUnassignedRoutedWork, which rewrites the
		// persisted route explicitly rather than treating the two spellings as
		// always-already-equal at compare time.
		{"legacy bound-template spelling is not collapsed", "gascity/gastown.builder", "gascity/builder", false},
		// config accepts "builder" and "Builder" as two separate agents
		// (ValidateAgents keys on a case-sensitive {dir, binding, name}), so
		// treating them as equal here is a cross-agent work-visibility bug
		// (ga-lmy6yj), not a convenience.
		{"case-differing spellings are DISTINCT agents", "gascity/Builder", "gascity/builder", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hookRouteIdentitiesEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("hookRouteIdentitiesEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := hookRouteIdentitiesEqual(tc.b, tc.a); got != tc.want {
				t.Errorf("hookRouteIdentitiesEqual(%q, %q) = %v, want %v (reversed)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// TestHookClaimMatchesRouteToleratesSessionNameEncoding proves the claim
// path's own predicate now shares hookRouteIdentitiesEqual, satisfying
// ga-1xaqgo.2's "display and claim paths share one route-spelling matcher"
// criterion directly against the function gc hook --claim calls.
func TestHookClaimMatchesRouteToleratesSessionNameEncoding(t *testing.T) {
	candidate := beads.Bead{
		ID:       "rt-1",
		Status:   "open",
		Metadata: beads.StringMap{"gc.routed_to": "gascity/builder"},
	}
	if !hookClaimMatchesRoute(candidate, []string{"gascity--builder"}) {
		t.Fatal("dash-encoded route target must match canonical gc.routed_to")
	}
	if hookClaimMatchesRoute(candidate, []string{"gascity--reviewer"}) {
		t.Fatal("a genuinely different agent must not match")
	}
}

// TestHookClaimMatchesRouteWorkflowRunTargetFallback pins issue #5900: a
// graph.v2 workflow root's gc.run_target fallback exists so a genuinely
// root-only (#2763-shape) molecule is claimable as its own unit of work, but
// the same fallback must not resurrect a fully-expanded root whose real
// children have all closed and is only waiting on workflow-finalize.
// gc.workflow_expanded distinguishes the two shapes.
func TestHookClaimMatchesRouteWorkflowRunTargetFallback(t *testing.T) {
	rootOnly := beads.Bead{
		ID:     "wf-root-only",
		Status: "open",
		Metadata: beads.StringMap{
			beadmeta.KindMetadataKey:      beadmeta.KindWorkflow,
			beadmeta.RunTargetMetadataKey: "gascity/builder",
		},
	}
	if !hookClaimMatchesRoute(rootOnly, []string{"gascity/builder"}) {
		t.Fatal("a never-expanded (#2763-shape) workflow root must still fall back to gc.run_target")
	}

	expanded := beads.Bead{
		ID:     "wf-root-expanded",
		Status: "open",
		Metadata: beads.StringMap{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.RunTargetMetadataKey:        "gascity/builder",
			beadmeta.WorkflowExpandedMetadataKey: "true",
		},
	}
	if hookClaimMatchesRoute(expanded, []string{"gascity/builder"}) {
		t.Fatal("a fully-expanded workflow root (gc.workflow_expanded=true) must not fall back to gc.run_target")
	}

	if got := hookClaimRoute(expanded); got != "" {
		t.Fatalf("hookClaimRoute(expanded workflow root) = %q, want empty (never claimable/visible via run_target fallback)", got)
	}
	if got := hookClaimRoute(rootOnly); got != "gascity/builder" {
		t.Fatalf("hookClaimRoute(root-only workflow root) = %q, want %q", got, "gascity/builder")
	}
}

// TestHookClaimMatchesRouteRefusesExpandedWorkflowRootOnRoutedTo extends the
// #5900 guard to the canonical route. Workflow roots carry gc.routed_to, so
// gating only the gc.run_target fallback leaves a fully-expanded root
// claimable the moment it is open and unassigned (reopened after a close).
// Its real child steps are the executable work; the root itself must not
// match a pool worker's route. A root-only molecule, whose root IS the unit
// of work, keeps matching.
func TestHookClaimMatchesRouteRefusesExpandedWorkflowRootOnRoutedTo(t *testing.T) {
	rootOnly := beads.Bead{
		ID:     "wf-root-only",
		Status: "open",
		Metadata: beads.StringMap{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.RoutedToMetadataKey:        "gascity/builder",
		},
	}
	if !hookClaimMatchesRoute(rootOnly, []string{"gascity/builder"}) {
		t.Fatal("a never-expanded workflow root routed by gc.routed_to must stay claimable for initial launch")
	}

	expanded := beads.Bead{
		ID:     "wf-root-expanded",
		Status: "open",
		Metadata: beads.StringMap{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
			beadmeta.RoutedToMetadataKey:         "gascity/builder",
			beadmeta.WorkflowExpandedMetadataKey: "true",
		},
	}
	if hookClaimMatchesRoute(expanded, []string{"gascity/builder"}) {
		t.Fatal("a fully-expanded workflow root (gc.workflow_expanded=true) must not be claimable via gc.routed_to; its child steps are the executable work")
	}
}

// TestHookClaimAndDisplayKeepMarkedTaskAttemptRoot is the negative control for
// the expanded-root guard. Retry and ralph attempt roots carry
// gc.workflow_expanded=true with gc.kind=task and are real work, so a guard
// keyed on the marker alone would hide them from the worker they are routed
// to. The guard needs both the kind and the marker.
func TestHookClaimAndDisplayKeepMarkedTaskAttemptRoot(t *testing.T) {
	attempt := beads.Bead{
		ID:     "attempt-root",
		Status: "open",
		Metadata: beads.StringMap{
			beadmeta.KindMetadataKey:             beadmeta.KindTask,
			beadmeta.RoutedToMetadataKey:         "gascity/builder",
			beadmeta.WorkflowExpandedMetadataKey: "true",
		},
	}
	if !hookClaimMatchesRoute(attempt, []string{"gascity/builder"}) {
		t.Error("a marked attempt root with gc.kind=task must stay claimable via gc.routed_to")
	}
	if !hookCandidateVisible(attempt, []string{"gascity--builder-1"}, []string{"gascity/builder"}) {
		t.Error("a marked attempt root with gc.kind=task must stay visible to a pool worker on its route")
	}
}

// TestHookCandidateReclaimEligibleRefusesExpandedWorkflowRoot pins the
// stale-lease reclaim path to the same route rule as a fresh claim: an
// expanded root held by a stale assignee is still a container, so reclaiming
// it would hand a worker a workflow in place of a step. A root-only root and
// a marked attempt root are real work and stay reclaim-eligible.
func TestHookCandidateReclaimEligibleRefusesExpandedWorkflowRoot(t *testing.T) {
	now := time.Now()
	routeTargets := []string{"gascity/builder"}
	cases := []struct {
		name     string
		metadata beads.StringMap
		want     bool
	}{
		{
			name: "expanded workflow root",
			metadata: beads.StringMap{
				beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
				beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
				beadmeta.RoutedToMetadataKey:         "gascity/builder",
				beadmeta.WorkflowExpandedMetadataKey: "true",
			},
			want: false,
		},
		{
			name: "root-only workflow root",
			metadata: beads.StringMap{
				beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				beadmeta.RoutedToMetadataKey:        "gascity/builder",
			},
			want: true,
		},
		{
			name: "marked attempt root with gc.kind=task",
			metadata: beads.StringMap{
				beadmeta.KindMetadataKey:             beadmeta.KindTask,
				beadmeta.RoutedToMetadataKey:         "gascity/builder",
				beadmeta.WorkflowExpandedMetadataKey: "true",
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := beads.Bead{ID: "stale-1", Status: "in_progress", Assignee: "dead-worker", Metadata: tc.metadata}
			if got := hookCandidateReclaimEligible(candidate, routeTargets, now); got != tc.want {
				t.Errorf("hookCandidateReclaimEligible(%s with a stale assignee) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestDoHookClaimSkipsExpandedWorkflowRootAndClaimsRoutedStep drives the whole
// claim path with the shape a pool worker's query returns once an expanded
// root is open and unassigned: the root ahead of one of its own routed steps.
// The worker must claim the step and never attempt the root.
//
// Two layers refuse this row and doHookClaim runs both, so this test pins the
// outcome, not either layer: it stays green with one of them removed. The hook
// filter (filterUnreadyHookCandidates) drops the unassigned root before the
// claim tiers see it, and the claim matcher (hookClaimMatchesRoute) refuses
// its route. Each layer is pinned where it acts alone:
//   - the matcher by
//     TestDoHookClaimNeverReclaimsStaleExpandedWorkflowRoot, where the root
//     carries an assignee and the filter keeps it;
//   - the filter by
//     TestClaimHookWorkNeverSelectsStoreHoldingOnlyExpandedWorkflowRoot, at
//     federated store selection, which never consults the matcher.
//
// No claim tier takes an unassigned row without asking the matcher, so the
// filter is never the only thing between an unassigned root and a claim
// mutation; store selection is where it decides something by itself.
func TestDoHookClaimSkipsExpandedWorkflowRootAndClaimsRoutedStep(t *testing.T) {
	var attempts []string
	runner := func(string, string) (string, error) {
		return `[
			{"id":"wf-root","status":"open","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"worker","gc.workflow_expanded":"true"}},
			{"id":"wf-step","status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"wf-root"}}
		]`, nil
	}
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			attempts = append(attempts, beadID)
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.Join(attempts, ","); got != "wf-step" {
		t.Fatalf("claim attempts = %q, want wf-step only (the expanded root must never be attempted)", got)
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.Reason != "claimed" || result.BeadID != "wf-step" {
		t.Fatalf("unexpected claim result: %+v", result)
	}
}

// TestDoHookClaimNeverReclaimsStaleExpandedWorkflowRoot pins the claim
// matcher end to end, on the one claim path where the hook filter cannot act.
// An expanded root held by a stale assignee is assigned, so
// filterUnreadyHookCandidates keeps it (an assigned root may be its holder's
// anchor) and it reaches the stale-lease reclaim tier. Only
// hookClaimMatchesRoute stands between it and a reclaim there.
func TestDoHookClaimNeverReclaimsStaleExpandedWorkflowRoot(t *testing.T) {
	var reclaims, attempts []string
	runner := func(string, string) (string, error) {
		return `[
			{"id":"wf-root","status":"in_progress","assignee":"dead-worker","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"worker","gc.workflow_expanded":"true"}},
			{"id":"wf-step","status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"wf-root"}}
		]`, nil
	}
	ops := hookClaimOps{
		Runner: runner,
		ReclaimStale: func(_ context.Context, _ string, _ []string, beadID string) (bool, string, error) {
			reclaims = append(reclaims, beadID)
			return true, "dead-worker", nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			attempts = append(attempts, beadID)
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		EmitHookClaimReclaimedStale: func(string, string, string) {},
	}
	opts := hookClaimOptions{
		Assignee:               "worker-1",
		IdentityCandidates:     []string{"worker-1"},
		RouteTargets:           []string{"worker"},
		AutoReclaimStaleClaims: true,
		JSON:                   true,
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(reclaims) != 0 {
		t.Fatalf("stale-lease reclaim attempted on %v, want none (an expanded workflow root is never reclaim-eligible)", reclaims)
	}
	if got := strings.Join(attempts, ","); got != "wf-step" {
		t.Fatalf("claim attempts = %q, want wf-step only", got)
	}
}

// TestClaimHookWorkNeverSelectsStoreHoldingOnlyExpandedWorkflowRoot pins the
// hook filter end to end, where it decides alone. Federated store selection
// (bestStoreWithWork) asks only whether a store's filtered output has ready
// work and never consults the claim matcher, so the filter is what keeps a
// store whose only candidate is an unassigned expanded root from being chosen
// as the claim store. The root outranks the step on priority, so a selection
// that counted it would commit to its store first and read it a second time
// to re-validate before falling back.
func TestClaimHookWorkNeverSelectsStoreHoldingOnlyExpandedWorkflowRoot(t *testing.T) {
	stores := []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}},
	}
	reads := map[string]int{}
	run := func(_, dir string, _ []string) (string, error) {
		reads[dir]++
		switch dir {
		case "city":
			return `[{"id":"wf-root","status":"open","priority":0,"metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"worker","gc.workflow_expanded":"true"}}]`, nil
		case "riga":
			return `[{"id":"wf-step","status":"open","priority":3,"metadata":{"gc.routed_to":"worker","gc.root_bead_id":"wf-root"}}]`, nil
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return "", nil
		}
	}
	var claimDir string
	ops := hookClaimOps{
		Claim: func(_ context.Context, dir string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimDir = dir
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" }, // suppress stamp noise
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("bd ready --json", "city", stores[0].env, stores, opts, ops, run, func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.BeadID != "wf-step" || result.Reason != "claimed" || claimDir != "riga" {
		t.Fatalf("claim result = %+v in dir %q, want wf-step claimed in riga", result, claimDir)
	}
	if reads["city"] != 1 {
		t.Fatalf("city store read %d times, want 1: it is read once during selection and, holding only an expanded workflow root, must not be selected and re-validated as the claim store", reads["city"])
	}
}

// TestHookRouteIdentitiesEqualDotAxisRegression guards against a matcher that
// only reverses the slash-encoding axis ("--" -> "/") of a session-name
// identity while leaving the dot-encoding axis ("__" -> ".") untouched. A
// route spelled with a literal dot (e.g. a bound-template identity like
// "triager.triager") must still match a session-encoded identity for the
// same agent (e.g. "triager__triager"), or the filter wrongly treats an
// agent's own routed work as belonging to someone else and drops it (ga-4wwxl7).
func TestHookRouteIdentitiesEqualDotAxisRegression(t *testing.T) {
	tests := []struct {
		name     string
		route    string
		identity string
		want     bool
	}{
		{"dot template route vs dot-encoded session identity", "triager.triager", "triager__triager", true},
		{"dir-qualified dot route vs session-encoded identity", "gastown.mayor", "gastown__mayor", true},
		{"slash route vs session-encoded slash identity (pre-existing axis)", "gascity/builder", "gascity--builder", true},
		{"combined slash+dot route vs fully session-encoded identity", "hello-world/gastown.polecat", "hello-world--gastown__polecat", true},
		{"distinct identities must not match", "gascity/builder", "gascity--reviewer", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hookRouteIdentitiesEqual(tt.route, tt.identity); got != tt.want {
				t.Errorf("hookRouteIdentitiesEqual(%q, %q) = %v, want %v", tt.route, tt.identity, got, tt.want)
			}
		})
	}
}

// --- hookCandidateVisible ---------------------------------------------------

func TestHookCandidateVisible(t *testing.T) {
	cases := []struct {
		name         string
		assignee     string
		routedTo     string
		identities   []string
		routeTargets []string
		want         bool
	}{
		{
			name:       "assigned to me",
			assignee:   "gascity/builder",
			identities: []string{"gascity/builder"},
			want:       true,
		},
		{
			name:       "assigned to someone else",
			assignee:   "reviewer-gm-wisp-b6tr3z",
			identities: []string{"gascity/builder"},
			want:       false,
		},
		{
			// Tiers 1 and 2 of the default work query select on --assignee
			// alone and never consult routed_to, so an owned crash-recovery
			// bead may legitimately carry a stale or foreign route. The
			// exemption is blanket: assignee match alone decides visibility,
			// regardless of what routed_to says.
			name:       "assigned to me, with a stale foreign route present",
			assignee:   "gascity/builder",
			routedTo:   "gascity/deployer",
			identities: []string{"gascity/builder"},
			want:       true,
		},
		{
			name:     "assigned to someone else, no identity context at all",
			assignee: "reviewer-gm-wisp-b6tr3z",
			want:     false,
		},
		{
			name: "unassigned and unrouted is always visible",
			want: true,
		},
		{
			name:         "unassigned, routed to me",
			routedTo:     "gascity/builder",
			routeTargets: []string{"gascity/builder"},
			want:         true,
		},
		{
			name:         "unassigned, routed to me in session-name encoding",
			routedTo:     "gascity/builder",
			routeTargets: []string{"gascity--builder"},
			want:         true,
		},
		{
			name:         "unassigned, routed to a different agent",
			routedTo:     "gascity/reviewer",
			routeTargets: []string{"gascity/builder"},
			want:         false,
		},
		{
			name:         "unassigned, routed to a different rig",
			routedTo:     "otherrig/builder",
			routeTargets: []string{"gascity/builder"},
			want:         false,
		},
		{
			name:     "unassigned, routed, but no route context at all",
			routedTo: "gascity/reviewer",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := beads.Bead{ID: "vis-1", Status: "open", Assignee: tc.assignee}
			if tc.routedTo != "" {
				candidate.Metadata = beads.StringMap{"gc.routed_to": tc.routedTo}
			}
			if got := hookCandidateVisible(candidate, tc.identities, tc.routeTargets); got != tc.want {
				t.Errorf("hookCandidateVisible(assignee=%q, routedTo=%q, identities=%v, routeTargets=%v) = %v, want %v",
					tc.assignee, tc.routedTo, tc.identities, tc.routeTargets, got, tc.want)
			}
		})
	}
}

func TestHookCandidateVisibleWorkflowRunTargetFallback(t *testing.T) {
	candidate := beads.Bead{
		ID:     "wf-1",
		Status: "open",
		Metadata: beads.StringMap{
			"gc.kind":       "workflow",
			"gc.run_target": "gascity/builder",
		},
	}
	if !hookCandidateVisible(candidate, nil, []string{"gascity/builder"}) {
		t.Fatal("unrouted workflow candidate must fall back to gc.run_target, matching hookClaimMatchesRoute")
	}
	if hookCandidateVisible(candidate, nil, []string{"gascity/reviewer"}) {
		t.Fatal("workflow run_target for a different agent must not be visible")
	}
}

// TestHookCandidateVisibleExpandedWorkflowRoot is the display-path twin of
// TestHookClaimMatchesRouteRefusesExpandedWorkflowRootOnRoutedTo: an
// unassigned, fully-expanded root is not offered to a pool worker on its
// route, while a root this session already holds stays visible as its
// launch/drain anchor.
func TestHookCandidateVisibleExpandedWorkflowRoot(t *testing.T) {
	expanded := beads.Bead{
		ID:     "wf-root-expanded",
		Status: "open",
		Metadata: beads.StringMap{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
			beadmeta.RoutedToMetadataKey:         "gascity/builder",
			beadmeta.WorkflowExpandedMetadataKey: "true",
		},
	}
	if hookCandidateVisible(expanded, []string{"gascity--builder-1"}, []string{"gascity/builder"}) {
		t.Error("an unassigned fully-expanded workflow root must not be visible to a pool worker on its route")
	}

	anchored := expanded
	anchored.Status = "in_progress"
	anchored.Assignee = "gascity--builder-1"
	if !hookCandidateVisible(anchored, []string{"gascity--builder-1"}, []string{"gascity/builder"}) {
		t.Fatal("a workflow root assigned to this session must stay visible as its anchor")
	}
}

// --- filterForeignHookCandidates --------------------------------------------

func TestFilterForeignHookCandidatesFailsOpen(t *testing.T) {
	visibility := hookVisibility{Identities: []string{"gascity/builder"}, RouteTargets: []string{"gascity/builder"}}

	t.Run("empty output unchanged", func(t *testing.T) {
		if got := filterForeignHookCandidates("", visibility); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("no visibility configured, output unchanged", func(t *testing.T) {
		raw := `[{"id":"x","status":"open","assignee":"someone-else"}]`
		if got := filterForeignHookCandidates(raw, hookVisibility{}); got != raw {
			t.Errorf("got %q, want unchanged %q", got, raw)
		}
	})

	t.Run("non-JSON output unchanged", func(t *testing.T) {
		raw := "hw-1  open  Fix the bug\n"
		if got := filterForeignHookCandidates(raw, visibility); got != raw {
			t.Errorf("got %q, want unchanged %q", got, raw)
		}
	})

	t.Run("non-array JSON unchanged", func(t *testing.T) {
		raw := `{"id":"x"}`
		if got := filterForeignHookCandidates(raw, visibility); got != raw {
			t.Errorf("got %q, want unchanged %q", got, raw)
		}
	})

	t.Run("non-object array item is kept", func(t *testing.T) {
		raw := `["not-an-object"]`
		var got []any
		if err := json.Unmarshal([]byte(filterForeignHookCandidates(raw, visibility)), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d items, want 1 kept", len(got))
		}
	})

	t.Run("item that fails to decode as a Bead is kept", func(t *testing.T) {
		raw := `[{"id":12345,"status":"open","assignee":"someone-else"}]`
		var got []any
		if err := json.Unmarshal([]byte(filterForeignHookCandidates(raw, visibility)), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d items, want 1 kept (fail-open on decode error)", len(got))
		}
	})
}

func TestFilterForeignHookCandidatesDropsForeignKeepsOwnAndUnrouted(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "ga-2a46gb", Status: "open", Assignee: "gascity/builder"},
		{ID: "ga-77refr", Status: "in_progress", Assignee: "reviewer-gm-wisp-b6tr3z"},
		{ID: "ga-20zoji", Status: "open"},
		{ID: "ga-5hdwl6", Status: "open", Metadata: beads.StringMap{"gc.routed_to": "gascity/reviewer"}},
		{ID: "ga-drlztz", Status: "open", Metadata: beads.StringMap{"gc.routed_to": "otherrig/builder"}},
		{ID: "ga-same-route", Status: "open", Metadata: beads.StringMap{"gc.routed_to": "gascity--builder"}},
	}
	raw, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	visibility := hookVisibility{
		Identities:   []string{"gascity/builder"},
		RouteTargets: []string{"gascity/builder"},
	}
	got := filterForeignHookCandidates(string(raw), visibility)

	var kept []beads.Bead
	if err := json.Unmarshal([]byte(got), &kept); err != nil {
		t.Fatalf("unmarshal filtered output: %v", err)
	}
	var ids []string
	for _, b := range kept {
		ids = append(ids, b.ID)
	}
	want := []string{"ga-2a46gb", "ga-20zoji", "ga-same-route"}
	if len(ids) != len(want) {
		t.Fatalf("kept ids = %v, want %v", ids, want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Errorf("kept[%d] = %q, want %q (full: %v)", i, ids[i], id, ids)
		}
	}
}

// --- doHook integration -----------------------------------------------------

func TestDoHookVisibilityIgnoredWhenEmpty(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"hw-1","status":"open","assignee":"someone-else"}]`, nil
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "", false, runner, &stdout, &stderr, hookVisibility{})
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0 (zero-value visibility must not filter); stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("hw-1")) {
		t.Errorf("stdout = %q, want to contain hw-1 (unfiltered)", stdout.String())
	}
}

func TestDoHookDropsForeignAssigneeUnderVisibility(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"ga-77refr","status":"in_progress","assignee":"reviewer-gm-wisp-b6tr3z"}]`, nil
	}
	var stdout, stderr bytes.Buffer
	visibility := hookVisibility{Identities: []string{"gascity/builder"}, RouteTargets: []string{"gascity/builder"}}
	code := doHook("bd ready", "", false, runner, &stdout, &stderr, visibility)
	if code != 1 {
		t.Fatalf("doHook() = %d, want 1 (foreign assignee filtered to no-work); stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("ga-77refr")) {
		t.Errorf("stdout = %q, must not contain the foreign-assigned candidate", stdout.String())
	}
}

func TestDoHookKeepsUnroutedUnassignedWorkUnderVisibility(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"ga-20zoji","status":"open"}]`, nil
	}
	var stdout, stderr bytes.Buffer
	visibility := hookVisibility{Identities: []string{"gascity/builder"}, RouteTargets: []string{"gascity/builder"}}
	code := doHook("bd ready", "", false, runner, &stdout, &stderr, visibility)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0 (legacy unrouted work must stay visible); stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("ga-20zoji")) {
		t.Errorf("stdout = %q, want to contain the unrouted candidate", stdout.String())
	}
}

// TestDoHookGa1xaqgoRegression mirrors the ga-1xaqgo.2 / ga-lmy6yj bug
// report's repro shape: a plain "gc hook" call under native-store schema
// skew (where the store's own gc.routed_to predicate silently no-ops) must
// show only this agent's own and legitimately unrouted work.
func TestDoHookGa1xaqgoRegression(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "ga-2a46gb", Status: "open", Assignee: "gascity/builder"},
		{ID: "ga-77refr", Status: "in_progress", Assignee: "reviewer-gm-wisp-b6tr3z"},
		{ID: "ga-20zoji", Status: "open"},
		{ID: "ga-5hdwl6", Status: "open", Metadata: beads.StringMap{"gc.routed_to": "gascity/reviewer"}},
		{ID: "ga-drlztz", Status: "open", Metadata: beads.StringMap{"gc.routed_to": "otherrig/builder"}},
	}
	raw, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	runner := func(string, string) (string, error) { return string(raw), nil }
	var stdout, stderr bytes.Buffer
	visibility := hookVisibility{Identities: []string{"gascity/builder"}, RouteTargets: []string{"gascity/builder"}}
	code := doHook("bd ready", "", false, runner, &stdout, &stderr, visibility)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, wantID := range []string{"ga-2a46gb", "ga-20zoji"} {
		if !bytes.Contains([]byte(out), []byte(wantID)) {
			t.Errorf("stdout missing own/unrouted candidate %q: %s", wantID, out)
		}
	}
	for _, foreignID := range []string{"ga-77refr", "ga-5hdwl6", "ga-drlztz"} {
		if bytes.Contains([]byte(out), []byte(foreignID)) {
			t.Errorf("stdout leaked foreign candidate %q: %s", foreignID, out)
		}
	}
}

// --- route-target call-site wiring ------------------------------------------

// TestFilterForeignHookCandidatesPoolBaseRouteViaRoutedToIdentity pins that a
// pool slot's own QualifiedName is slot-suffixed ("gascity/polecat-2"), but
// the routed-pool tier matches on the BASE pool name (poolDemandTarget), so
// demand work is written with the base route. hookClaimPrimaryRouteTarget
// (agentutil.RoutedToIdentity) is what lets the slot recognize its own base
// route; hookClaimRouteTargets is the same expansion the claim path uses.
func TestFilterForeignHookCandidatesPoolBaseRouteViaRoutedToIdentity(t *testing.T) {
	base := config.Agent{Dir: "gascity", Name: "polecat"}
	slot := config.Agent{Dir: "gascity", Name: "polecat-2", PoolName: base.QualifiedName()}
	candidates := []beads.Bead{
		{ID: "ga-pool", Status: "open", Metadata: beads.StringMap{"gc.routed_to": base.QualifiedName()}},
	}
	raw, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	// The explicit-arg path blanks GC_TEMPLATE, so this is the full
	// route-target set a slot agent gets.
	routeTargets := hookClaimRouteTargets(hookClaimPrimaryRouteTarget(&slot), slot.QualifiedName(), "")
	var kept []beads.Bead
	if err := json.Unmarshal([]byte(filterForeignHookCandidates(string(raw), hookVisibility{RouteTargets: routeTargets})), &kept); err != nil {
		t.Fatalf("unmarshal filtered output: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("dropped the pool slot's own base-route work: routeTargets=%v kept=%v", routeTargets, kept)
	}

	// Negative control: it is RoutedToIdentity doing the work, not the
	// slot-suffixed qualified name, which must NOT match on its own. If this
	// stops failing, the test above has gone vacuous.
	var keptNarrow []beads.Bead
	narrow := filterForeignHookCandidates(string(raw), hookVisibility{RouteTargets: []string{slot.QualifiedName()}})
	if err := json.Unmarshal([]byte(narrow), &keptNarrow); err != nil {
		t.Fatalf("unmarshal filtered output: %v", err)
	}
	if len(keptNarrow) != 0 {
		t.Errorf("slot-suffixed name matched the base route on its own: kept %v", keptNarrow)
	}
}

// TestFilterForeignHookCandidatesLegacyWorkflowControlAliasMatched pins that
// buildWorkQuery actively probes the legacy "<rig>/workflow-control" spelling
// (legacyWorkflowControlQualifiedName), so a bead can carry that route.
// hookClaimIdentityCandidates is what expands a raw identity into that alias.
func TestFilterForeignHookCandidatesLegacyWorkflowControlAliasMatched(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "ga-legacy", Status: "open", Metadata: beads.StringMap{"gc.routed_to": "gascity/workflow-control"}},
	}
	raw, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	identities := hookClaimIdentityCandidates("gascity/control-dispatcher")
	var kept []beads.Bead
	if err := json.Unmarshal([]byte(filterForeignHookCandidates(string(raw), hookVisibility{RouteTargets: identities})), &kept); err != nil {
		t.Fatalf("unmarshal filtered output: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("dropped legacy workflow-control work: identities=%v kept=%v", identities, kept)
	}

	// Negative control: the unexpanded name alone does not carry the alias.
	var keptNarrow []beads.Bead
	narrow := filterForeignHookCandidates(string(raw), hookVisibility{RouteTargets: []string{"gascity/control-dispatcher"}})
	if err := json.Unmarshal([]byte(narrow), &keptNarrow); err != nil {
		t.Fatalf("unmarshal filtered output: %v", err)
	}
	if len(keptNarrow) != 0 {
		t.Errorf("unexpanded identity matched the legacy alias: kept %v", keptNarrow)
	}
}
