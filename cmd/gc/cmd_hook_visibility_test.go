package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
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
		t.Fatal("a fully-expanded workflow root (gc.workflow_expanded=true) must not fall back to gc.run_target; its child steps own executable work")
	}

	if got := hookClaimRoute(expanded); got != "" {
		t.Fatalf("hookClaimRoute(expanded workflow root) = %q, want empty (never claimable/visible via run_target fallback)", got)
	}
	if got := hookClaimRoute(rootOnly); got != "gascity/builder" {
		t.Fatalf("hookClaimRoute(root-only workflow root) = %q, want %q", got, "gascity/builder")
	}
}

// A stale explicit route must not turn a graph latch into executable work or
// capacity demand. Otherwise each drained worker is replaced by another one.
func TestHookClaimAndDemandRejectRoutedTopology(t *testing.T) {
	for _, tc := range []struct {
		name, kind, expanded string
		want                 bool
	}{
		{"expanded workflow", beadmeta.KindWorkflow, "true", false},
		{"scope", beadmeta.KindScope, "", false},
		{"spec", beadmeta.KindSpec, "", false},
		{"root-only workflow", beadmeta.KindWorkflow, "", true},
		{"worker step", "", "", true},
		{"control step", beadmeta.KindWorkflowFinalize, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := beads.Bead{
				ID: "probe-step", Status: "open", Type: "task",
				Metadata: beads.StringMap{
					beadmeta.KindMetadataKey:             tc.kind,
					beadmeta.WorkflowExpandedMetadataKey: tc.expanded,
					beadmeta.RoutedToMetadataKey:         "rig/worker",
				},
			}
			if got := hookCandidateClaimable(candidate, []string{"rig/worker"}, time.Now()); got != tc.want {
				t.Errorf("fresh claim = %v, want %v", got, tc.want)
			}
			if got := len(controllerDemandRouteCandidates(candidate)) != 0; got != tc.want {
				t.Errorf("worker demand = %v, want %v", got, tc.want)
			}
			candidate.Assignee = "stale-worker"
			if got := hookCandidateReclaimEligible(candidate, []string{"rig/worker"}, time.Now()); got != tc.want {
				t.Errorf("stale reclaim = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFilterUnreadyHookCandidatesPreservesOwnedWorkflowAnchor(t *testing.T) {
	input := `[{"id":"fresh","status":"open","metadata":{"gc.kind":"workflow","gc.workflow_expanded":"true"}},{"id":"owned","status":"in_progress","assignee":"rig/worker","metadata":{"gc.kind":"workflow","gc.workflow_expanded":"true"}}]`
	got := filterUnreadyHookCandidates(input, time.Now())
	var candidates []beads.Bead
	if err := json.Unmarshal([]byte(got), &candidates); err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != "owned" {
		t.Fatalf("candidates = %s, want only the owned continuation anchor", got)
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

// Root parking suppresses worker claims; it must not suppress the independent
// finalizer after the live child completes. Exercise the hook and dispatcher
// against one ledger, retaining the compiler's informational finalizer edge.
func TestExpandedWorkflowParkedRootFinalizesAfterChildCloses(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name       string
		labels     []string
		deferUntil *time.Time
		indefinite bool
	}{
		{name: "future deferral", deferUntil: &future},
		{name: "indefinite deferral", indefinite: true},
		{name: "expired deferral", deferUntil: &past},
		{name: "manual external hold", labels: []string{"hold:external"}},
		{name: "manual operator hold", labels: []string{"hold:mayor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			root := f.create(beads.Bead{
				Title: "expanded workflow", Type: "molecule", Labels: tc.labels, DeferUntil: tc.deferUntil, IndefinitelyDeferred: tc.indefinite,
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.workflow_expanded": "true", "gc.routed_to": "worker"},
			})
			child := f.create(beads.Bead{
				Title: "live child", Type: "task", Assignee: "live-owner",
				Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.routed_to": "worker"},
			})
			liveStatus := "in_progress"
			if err := f.store.Update(child.ID, beads.UpdateOpts{Status: &liveStatus}); err != nil {
				t.Fatal(err)
			}
			finalizer := f.create(beads.Bead{
				Title: "finalize", Type: "task",
				Metadata: map[string]string{"gc.kind": "workflow-finalize", "gc.root_bead_id": root.ID},
			})
			for _, dep := range []beads.Dep{
				{IssueID: finalizer.ID, DependsOnID: child.ID, Type: "blocks"},
				{IssueID: root.ID, DependsOnID: finalizer.ID, Type: "tracks"},
			} {
				if err := f.store.DepAdd(dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
					t.Fatal(err)
				}
			}
			f.served = []string{root.ID, child.ID}
			if id, _ := f.run(); id != "" {
				t.Fatalf("hook served %s while child is live", id)
			}
			if _, err := dispatch.ProcessControl(f.store, finalizer, dispatch.ProcessOptions{}); !errors.Is(err, dispatch.ErrControlPending) {
				t.Fatalf("finalizer before child close: %v, want pending", err)
			}
			if b := f.get(child.ID); b.Status != "in_progress" || b.Assignee != "live-owner" {
				t.Fatalf("pending finalizer changed live child: %+v", b)
			}
			f.closeAs(child.ID, "pass", nil)
			if id, _ := f.run(); id != "" {
				t.Fatalf("hook served %s after child close", id)
			}
			if routes := controllerDemandRouteCandidates(f.get(root.ID)); len(routes) != 0 {
				t.Fatalf("root demand = %v", routes)
			}
			result, err := dispatch.ProcessControl(f.store, finalizer, dispatch.ProcessOptions{})
			if err != nil || !result.Processed || result.Action != "workflow-pass" {
				t.Fatalf("finalizer = %+v, %v", result, err)
			}
			for _, id := range []string{root.ID, child.ID, finalizer.ID} {
				b := f.get(id)
				if b.Status != "closed" || b.Metadata["gc.outcome"] != "pass" {
					t.Fatalf("%s = %s/%s", id, b.Status, b.Metadata["gc.outcome"])
				}
			}
			if len(f.claims) != 0 {
				t.Fatalf("non-worker topology reached claim mutation: %v", f.claims)
			}
		})
	}
}

// Both hooks receive the same stale query snapshot: an expanded routed root,
// a live owned child and one ready sibling. The real SQLite claim CAS must
// yield one owned receipt and leave the root and live owner untouched.
func TestConcurrentWorkflowHookClaimsHaveOneStepOwner(t *testing.T) {
	opened, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*beads.SQLiteStore)
	t.Cleanup(func() {
		if err := store.CloseStore(); err != nil {
			t.Error(err)
		}
	})
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		got, err := store.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	root := create(beads.Bead{Title: "expanded root", Type: "molecule", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.workflow_expanded": "true", "gc.routed_to": "worker",
	}})
	live := create(beads.Bead{Title: "live child", Type: "task", Assignee: "live-owner", Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.routed_to": "worker"}})
	liveStatus := "in_progress"
	if err := store.Update(live.ID, beads.UpdateOpts{Status: &liveStatus}); err != nil {
		t.Fatal(err)
	}
	live, err = store.Get(live.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := create(beads.Bead{Title: "ready child", Type: "task", Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.routed_to": "worker"}})
	snapshot, err := json.Marshal([]beads.Bead{root, live, ready})
	if err != nil {
		t.Fatal(err)
	}
	var reads, workers sync.WaitGroup
	reads.Add(2)
	workers.Add(2)
	release, bothRead, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	type answer struct {
		code           int
		stdout, stderr string
	}
	answers := make(chan answer, 2)
	dir := t.TempDir()
	for _, owner := range []string{"session-a", "session-b"} {
		go func() {
			defer workers.Done()
			// Reuse the hook fixture's no-op external notifications. Runner returns
			// the shared snapshot; claims, input checks and ancestry reads use SQLite.
			// Release, ListContinuation and AssignContinuation remain fixture-backed;
			// this scenario exercises the competing claims, not those paths.
			ops := newHookWorkflowStopFixture(t).ops()
			ops.Runner = func(string, string) (string, error) { reads.Done(); <-release; return string(snapshot), nil }
			ops.Claim = func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
				return hookClaimThroughStore(id, assignee, func() (beads.Bead, bool, error) { return store.Claim(id, assignee) }, store.Get)
			}
			ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store { return store }
			ops.InputDone = func(_ context.Context, _ string, _ []string, candidate beads.Bead, identities []string) (string, bool, error) {
				return workflowInputDone(store, candidate, identities)
			}
			ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) { return store.Get(id) }
			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", dir, hookClaimOptions{Assignee: owner, IdentityCandidates: []string{owner}, RouteTargets: []string{"worker"}, JSON: true}, ops, &stdout, &stderr)
			answers <- answer{code, stdout.String(), stderr.String()}
		}()
	}
	go func() { reads.Wait(); close(bothRead) }()
	go func() { workers.Wait(); close(finished) }()
	awaitClose(t, bothRead, "both workflow query snapshots")
	close(release)
	awaitClose(t, finished, "competing workflow hook claims")
	wins := 0
	winner := ""
	for range 2 {
		a := <-answers
		if a.code != 0 {
			t.Fatalf("hook = %d: %s", a.code, a.stderr)
		}
		var result hookClaimJSONResult
		if err := json.Unmarshal([]byte(a.stdout), &result); err != nil {
			t.Fatal(err)
		}
		if result.Action == "work" {
			wins++
			winner = result.Assignee
			if result.BeadID != ready.ID {
				t.Fatalf("claimed %s, want ready child %s", result.BeadID, ready.ID)
			}
		} else if result.Action != "drain" {
			t.Fatalf("unexpected hook result: %+v", result)
		}
	}
	if wins != 1 || (winner != "session-a" && winner != "session-b") {
		t.Fatalf("work receipts = %d, owner = %q; want exactly one competing session", wins, winner)
	}
	rows, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil || len(rows) != 3 {
		t.Fatalf("workflow rows = %d, %v; want the original root and two children", len(rows), err)
	}
	for _, before := range []beads.Bead{root, live, ready} {
		after, err := store.Get(before.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantOwner, wantStatus := before.Assignee, before.Status
		if before.ID == ready.ID {
			wantOwner, wantStatus = winner, "in_progress"
			if after.ClaimFence != 1 {
				t.Fatalf("ready child ownership transitions = %d, want one", after.ClaimFence)
			}
		}
		if after.Assignee != wantOwner || after.Status != wantStatus {
			t.Fatalf("%s = %s/%s, want %s/%s", after.ID, after.Status, after.Assignee, wantStatus, wantOwner)
		}
	}
}
