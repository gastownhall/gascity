package targetscope

import (
	"strings"
	"testing"
)

const testStoreRoot = "/srv/store"

func mustResolve(t *testing.T, src Sources) Resolved {
	t.Helper()
	got, err := Resolve(src, testStoreRoot)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return got
}

// Each alias independently feeds scope.branch — none is privileged.
func TestEachBranchAliasFeedsScope(t *testing.T) {
	for _, alias := range BranchAliases {
		t.Run(alias, func(t *testing.T) {
			got := mustResolve(t, Sources{Explicit: []string{alias + "=release"}})
			if got.Scope.Branch != "release" {
				t.Fatalf("branch = %q, want release", got.Scope.Branch)
			}
		})
	}
}

// Disagreeing aliases within one layer reject. There is no silent winner.
func TestWithinLayerBranchDisagreementRejects(t *testing.T) {
	tests := []struct {
		name string
		src  Sources
	}{
		{"base vs target carrier", Sources{Explicit: []string{"base_branch=X", "target_branch=Y"}}},
		{"dedicated vs carrier", Sources{Explicit: []string{"gc_target_branch=Z", "base_branch=X"}}},
		{"duplicate same key", Sources{Explicit: []string{"base_branch=X", "base_branch=Y"}}},
		{"rig layer self-conflict", Sources{Rig: map[string]string{"base_branch": "X", "target_branch": "Y"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Resolve(tc.src, testStoreRoot); err == nil {
				t.Fatal("expected a rejection, got none")
			}
		})
	}
}

// Equal duplicates and equal aliases are accepted.
func TestWithinLayerAgreementAccepted(t *testing.T) {
	for _, src := range []Sources{
		{Explicit: []string{"base_branch=main", "base_branch=main"}},
		{Explicit: []string{"base_branch=main", "target_branch=main", "gc_target_branch=main"}},
	} {
		got := mustResolve(t, src)
		if got.Scope.Branch != "main" {
			t.Fatalf("branch = %q, want main", got.Scope.Branch)
		}
	}
}

// Cross-layer precedence: explicit beats member beats rig beats routing beats
// formula default.
func TestCrossLayerPrecedence(t *testing.T) {
	full := Sources{
		Explicit:        []string{"gc_target_branch=from-explicit"},
		Member:          Scope{V: 1, Branch: "from-member"},
		MemberSet:       true,
		Rig:             map[string]string{"base_branch": "from-rig"},
		Routing:         map[string]string{"base_branch": "from-routing"},
		FormulaDefaults: map[string]string{"base_branch": "from-default"},
	}
	tests := []struct {
		name   string
		mutate func(*Sources)
		want   string
		layer  LayerName
	}{
		{"explicit wins", func(s *Sources) {}, "from-explicit", LayerExplicit},
		{"member next", func(s *Sources) { s.Explicit = nil }, "from-member", LayerMember},
		{"rig next", func(s *Sources) { s.Explicit = nil; s.MemberSet = false }, "from-rig", LayerRig},
		{"routing next", func(s *Sources) { s.Explicit = nil; s.MemberSet = false; s.Rig = nil }, "from-routing", LayerRouting},
		{"default last", func(s *Sources) { s.Explicit = nil; s.MemberSet = false; s.Rig = nil; s.Routing = nil }, "from-default", LayerFormula},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := full
			tc.mutate(&src)
			got := mustResolve(t, src)
			if got.Scope.Branch != tc.want {
				t.Fatalf("branch = %q, want %q", got.Scope.Branch, tc.want)
			}
			if got.BranchLayer != tc.layer {
				t.Fatalf("layer = %q, want %q", got.BranchLayer, tc.layer)
			}
		})
	}
}

// THE WIRE INVARIANT (the split this design closes). An explicit
// gc_target_branch must reach the carrier the formula actually reads, so the
// branch substitution uses and the branch the close gate validates are the same
// string.
func TestConsumedCarrierEqualsScopeBranch(t *testing.T) {
	resolved := mustResolve(t, Sources{
		Explicit: []string{"gc_target_branch=release"},
		Routing:  map[string]string{"base_branch": "main"},
	})
	if resolved.Scope.Branch != "release" {
		t.Fatalf("scope.branch = %q, want release", resolved.Scope.Branch)
	}

	vars := map[string]string{"base_branch": "main"}
	ApplyToVars(vars, resolved, true, false)

	if vars["base_branch"] != "release" {
		t.Fatalf("consumed base_branch = %q, want release — the workflow would "+
			"execute against a different branch than the close gate validates", vars["base_branch"])
	}
	if vars["base_branch"] != resolved.Scope.Branch {
		t.Fatalf("post-condition violated: vars[base_branch]=%q != scope.branch=%q",
			vars["base_branch"], resolved.Scope.Branch)
	}
}

// Same invariant for a target_branch formula.
func TestConsumedTargetBranchCarrierEqualsScopeBranch(t *testing.T) {
	resolved := mustResolve(t, Sources{
		Explicit: []string{"gc_target_branch=release"},
		Routing:  map[string]string{"target_branch": "main"},
	})
	vars := map[string]string{"target_branch": "main"}
	ApplyToVars(vars, resolved, false, true)
	if vars["target_branch"] != resolved.Scope.Branch {
		t.Fatalf("vars[target_branch]=%q != scope.branch=%q", vars["target_branch"], resolved.Scope.Branch)
	}
}

// An empty explicit carrier is absent for collection and does NOT survive: it
// is overwritten by the reconciled winner.
func TestEmptyCarrierIsOverwritten(t *testing.T) {
	resolved := mustResolve(t, Sources{
		Explicit: []string{"base_branch=", "gc_target_branch=release"},
	})
	if resolved.Scope.Branch != "release" {
		t.Fatalf("scope.branch = %q, want release", resolved.Scope.Branch)
	}
	vars := map[string]string{"base_branch": ""}
	ApplyToVars(vars, resolved, true, false)
	if vars["base_branch"] != "release" {
		t.Fatalf("base_branch = %q, want release (not left empty, not the routed value)", vars["base_branch"])
	}
}

// Default-only: the scope and the carrier both end up as the formula default.
func TestDefaultOnlyKeepsScopeAndCarrierIdentical(t *testing.T) {
	resolved := mustResolve(t, Sources{FormulaDefaults: map[string]string{"base_branch": "main"}})
	if resolved.Scope.Branch != "main" {
		t.Fatalf("scope.branch = %q, want main", resolved.Scope.Branch)
	}
	vars := map[string]string{}
	ApplyToVars(vars, resolved, true, false)
	if vars["base_branch"] != resolved.Scope.Branch {
		t.Fatalf("vars[base_branch]=%q != scope.branch=%q", vars["base_branch"], resolved.Scope.Branch)
	}
}

// A formula consuming neither carrier gets no branch var, and nothing can
// diverge from the scope because there is no consumed branch to disagree.
func TestFormulaWithNoCarrierGetsNoBranchVar(t *testing.T) {
	resolved := mustResolve(t, Sources{Explicit: []string{"gc_target_branch=release"}})
	vars := map[string]string{}
	ApplyToVars(vars, resolved, false, false)
	if _, ok := vars[VarBaseBranch]; ok {
		t.Fatal("base_branch must not be written for a formula that does not consume it")
	}
	if resolved.Scope.Branch != "release" {
		t.Fatalf("scope.branch = %q, want release", resolved.Scope.Branch)
	}
}

// The typed scope inputs are claimed by the resolver and must never reach
// template substitution, the root key, or the persisted runtime-vars blob.
func TestScopeInputVarsAreStrippedFromConsumedMap(t *testing.T) {
	resolved := mustResolve(t, Sources{
		Explicit: []string{"gc_target_branch=release", "gc_target_worktree=/srv/wt"},
	})
	vars := map[string]string{
		"gc_target_branch":   "release",
		"gc_target_worktree": "/srv/wt",
		"other":              "kept",
	}
	ApplyToVars(vars, resolved, true, false)
	for _, name := range ScopeInputVars {
		if _, ok := vars[name]; ok {
			t.Fatalf("%s leaked into the consumed variable map", name)
		}
	}
	if vars["other"] != "kept" {
		t.Fatal("unrelated vars must be preserved")
	}
}

// A relative worktree is normalized to absolute AT THE BOUNDARY, so the single
// absolute string is what gets copied to root and members.
func TestWorktreeNormalizedAtBoundary(t *testing.T) {
	resolved := mustResolve(t, Sources{Explicit: []string{"gc_target_worktree=worktrees/T"}})
	want := testStoreRoot + "/worktrees/T"
	if resolved.Scope.Worktree != want {
		t.Fatalf("worktree = %q, want %q", resolved.Scope.Worktree, want)
	}
	// And it must persist as valid — a relative value would be present-invalid.
	blob, err := Marshal(resolved.Scope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if Parse(blob).State != StateValid {
		t.Fatal("boundary-normalized scope must persist as valid")
	}
}

// No trusted source anywhere yields a field-empty {v:1} — valid and unknown,
// never absent, so the cwd writers stay suppressed.
func TestNoTrustedSourceYieldsValidUnknown(t *testing.T) {
	resolved := mustResolve(t, Sources{})
	if !resolved.Scope.IsUnknown() {
		t.Fatalf("scope = %+v, want field-empty", resolved.Scope)
	}
	blob, err := Marshal(resolved.Scope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := Parse(blob); got.State != StateValid {
		t.Fatalf("state = %v, want valid — an absent object would re-enable the cwd stamp", got.State)
	}
}

// The conflict message must name the disagreeing carriers; an operator hitting
// this needs to know which two vars to reconcile.
func TestConflictErrorNamesTheCarriers(t *testing.T) {
	_, err := Resolve(Sources{Explicit: []string{"base_branch=X", "target_branch=Y"}}, testStoreRoot)
	if err == nil {
		t.Fatal("expected a conflict")
	}
	for _, want := range []string{"base_branch", "target_branch", "X", "Y"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}
