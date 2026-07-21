package sling

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// scopedFormulaDir writes a formula that DECLARES base_branch with a default,
// which is what makes base_branch a carrier this formula actually consumes.
func scopedFormulaDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	body := `formula = "` + name + `"
version = 1

[vars.base_branch]
default = "main"

[[steps]]
id = "work"
title = "Work on {{base_branch}}"
`
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}
	return dir
}

func scopedLaunchDeps(t *testing.T, formulaDir string) (SlingDeps, config.Agent) {
	t.Helper()
	single := 1
	agent := config.Agent{Name: "worker", Dir: "rig", MaxActiveSessions: &single}
	cfg := &config.City{Agents: []config.Agent{agent}}
	cfg.FormulaLayers.City = []string{formulaDir}
	deps := SlingDeps{
		CityName: "test-city",
		CityPath: t.TempDir(),
		Cfg:      cfg,
		Store:    beads.NewMemStore(),
		StoreRef: "city:test-city",
		Resolver: testResolver{},
		Notify:   testNotifier{},
		Runner:   func(_, _ string, _ map[string]string) (string, error) { return "", nil },
	}
	return deps, agent
}

// launchedRoot finds the materialized root: the one bead the launch created
// with no parent. It is located by shape rather than by SlingResult, because
// the field carrying the root id differs across the launch shapes and the
// assertion is about what landed in the store either way.
func launchedRoot(t *testing.T, deps SlingDeps) beads.Bead {
	t.Helper()
	all, err := deps.Store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("listing store: %v", err)
	}
	// The formula root is the bead the launch stamped with gc.formula_name.
	// Parentlessness alone is not enough: sling also creates its own
	// bookkeeping bead with no parent.
	var roots []beads.Bead
	for _, b := range all {
		if b.Metadata[beadmeta.FormulaNameMetadataKey] != "" {
			roots = append(roots, b)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("found %d formula roots after launch, want exactly one", len(roots))
	}
	return roots[0]
}

func launchedRootScope(t *testing.T, deps SlingDeps) targetscope.Resolution {
	t.Helper()
	return targetscope.Parse(launchedRoot(t, deps).Metadata[beadmeta.TargetScopeMetadataKey])
}

// The boundary stamps. Before this seam, a launched root carried no scope at
// all, so every reader fell through to whatever cwd the claim happened to
// stamp. §5 forbids leaving any Ready-visible launch unscoped.
func TestSlingFormulaStampsTargetScopeOnTheLaunchedRoot(t *testing.T) {
	dir := scopedFormulaDir(t, "scoped-launch")
	deps, agent := scopedLaunchDeps(t, dir)

	opts := testOpts(agent, "scoped-launch")
	opts.Vars = []string{"gc_target_branch=release"}

	_, err := slingFormula(opts, deps)
	if err != nil {
		t.Fatalf("slingFormula: %v", err)
	}
	got := launchedRootScope(t, deps)
	if !got.Valid() {
		t.Fatalf("launched root scope state = %v, want a present-valid object", got.State)
	}
	if got.Scope.Branch != "release" {
		t.Fatalf("scope.branch = %q, want the explicit release", got.Scope.Branch)
	}
}

// E2E #16, the wire assertion: the reconciled winner must reach the carrier
// the formula substitutes, so template substitution and the close gate read
// the SAME branch. Explicit gc_target_branch outranks the formula default, and
// the consumed base_branch must be overwritten to match — not left at main.
func TestSlingFormulaConsumedCarrierEqualsTheDeclaredScope(t *testing.T) {
	dir := scopedFormulaDir(t, "scoped-carrier")
	deps, agent := scopedLaunchDeps(t, dir)

	opts := testOpts(agent, "scoped-carrier")
	opts.Vars = []string{"gc_target_branch=release"}

	_, err := slingFormula(opts, deps)
	if err != nil {
		t.Fatalf("slingFormula: %v", err)
	}
	scope := launchedRootScope(t, deps)
	if !scope.Valid() {
		t.Fatalf("launched root scope state = %v, want valid", scope.State)
	}

	// The step title substitutes {{base_branch}}. It is the observable proof
	// that substitution consumed the winner rather than the stale default.
	steps, err := deps.Store.List(beads.ListQuery{ParentID: launchedRoot(t, deps).ID, IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("launch produced no steps to check substitution against")
	}
	wantTitle := "Work on " + scope.Scope.Branch
	if steps[0].Title != wantTitle {
		t.Fatalf("step title = %q, want %q — substitution and the declared scope disagree", steps[0].Title, wantTitle)
	}
}

// The default-only case. With no branch supplied anywhere, scope.branch must
// be the formula's own default rather than unknown: otherwise substitution
// quietly uses main while the scope claims to know nothing.
func TestSlingFormulaDefaultOnlyBranchReachesTheScope(t *testing.T) {
	dir := scopedFormulaDir(t, "scoped-default")
	deps, agent := scopedLaunchDeps(t, dir)

	_, err := slingFormula(testOpts(agent, "scoped-default"), deps)
	if err != nil {
		t.Fatalf("slingFormula: %v", err)
	}
	got := launchedRootScope(t, deps)
	if !got.Valid() {
		t.Fatalf("launched root scope state = %v, want valid", got.State)
	}
	if got.Scope.Branch != "main" {
		t.Fatalf("scope.branch = %q, want the formula default main", got.Scope.Branch)
	}
}

// Within-layer disagreement is an operator error, and it must reject the
// launch rather than silently pick one. Nothing may be materialized.
func TestSlingFormulaRejectsConflictingExplicitBranchCarriers(t *testing.T) {
	dir := scopedFormulaDir(t, "scoped-conflict")
	deps, agent := scopedLaunchDeps(t, dir)

	opts := testOpts(agent, "scoped-conflict")
	opts.Vars = []string{"base_branch=one", "gc_target_branch=two"}

	if _, err := slingFormula(opts, deps); err == nil {
		t.Fatal("conflicting explicit branch carriers launched, want a rejection")
	}
	all, err := deps.Store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("listing store: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("rejected launch materialized %d beads, want none", len(all))
	}
}

// A formula that declares no branch carrier still gets a scope object rather
// than nothing: absence is what lets a later claim write cwd (§2c).
func TestSlingFormulaStampsUnknownScopeForACarrierlessFormula(t *testing.T) {
	dir := t.TempDir()
	body := "formula = \"carrierless\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n"
	if err := os.WriteFile(filepath.Join(dir, "carrierless.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}
	deps, agent := scopedLaunchDeps(t, dir)

	_, err := slingFormula(testOpts(agent, "carrierless"), deps)
	if err != nil {
		t.Fatalf("slingFormula: %v", err)
	}
	got := launchedRootScope(t, deps)
	if !got.Valid() {
		t.Fatalf("scope state = %v, want a present-valid field-empty object, never absent", got.State)
	}
}
