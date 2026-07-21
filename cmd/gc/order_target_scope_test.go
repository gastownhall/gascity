package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// scopedOrderFormulaDir writes a formula that DECLARES base_branch with a
// default, which is what makes base_branch a carrier this formula consumes.
func scopedOrderFormulaDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := `
formula = "order-scoped"
version = 1

[vars.base_branch]
default = "main"

[[steps]]
id = "work"
title = "Work on {{base_branch}}"
`
	if err := os.WriteFile(filepath.Join(dir, "order-scoped.toml"), []byte(strings.TrimSpace(body)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func orderRecipeRootScope(t *testing.T, dir string, a orders.Order) targetscope.Resolution {
	t.Helper()
	recipe, scope, err := prepareOrderWispRecipe(context.Background(), beads.NewMemStore(), a, []string{dir}, nil, "")
	if err != nil {
		t.Fatalf("prepareOrderWispRecipe: %v", err)
	}
	if err := stampOrderWispRoot(recipe, scope); err != nil {
		t.Fatalf("stampOrderWispRoot: %v", err)
	}
	return targetscope.Parse(recipe.Steps[0].Metadata[beadmeta.TargetScopeMetadataKey])
}

// The order boundary stamps. Both order paths (controller dispatch and
// `gc order run`) reach molecule.Instantiate directly without entering any
// sling stamper, so before this seam every order root was ABSENT and the first
// claim wrote whatever cwd the controller happened to hold.
func TestOrderWispRootCarriesTheDeclaredScope(t *testing.T) {
	dir := scopedOrderFormulaDir(t)
	a := orders.Order{Name: "scoped-order", Trigger: "manual", Formula: "order-scoped", FormulaLayer: dir, Rig: "rig-a"}

	got := orderRecipeRootScope(t, dir, a)
	if !got.Valid() {
		t.Fatalf("order root scope state = %v, want a present-valid object", got.State)
	}
	if got.Scope.Branch != "main" {
		t.Fatalf("scope.branch = %q, want the formula default main", got.Scope.Branch)
	}
}

// The load-bearing correctness property for the order boundary: scope.branch
// equals the branch the materialized work actually runs against, end to end.
//
// Both order boundaries materialize with molecule.Instantiate(recipe,
// molecule.Options{}) — empty options — so a step body's {{base_branch}} is
// substituted from the FORMULA DEFAULT, ignoring the order's caller vars and
// its rig formula_vars. Resolving the scope from the rig or the caller layer
// would stamp a branch the work never touches — the exact §3c divergence the
// equality invariant forbids. This test drives the real dispatch and asserts
// the stamped scope and the substituted step title are the SAME branch.
func TestOrderScopeEqualsWhatTheWorkSubstitutesEndToEnd(t *testing.T) {
	cityDir := t.TempDir()
	formulaDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, "fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"
[daemon]
formula_v2 = true
[[rigs]]
name = "fixture"
path = "fixture"
[[rigs.formula_vars]]
base_branch = "RIG-BRANCH-IGNORED-BY-SUBSTITUTION"
[[agent]]
name = "worker"
dir = "fixture"
max_active_sessions = 2
[[agent]]
name = "control-dispatcher"
dir = "fixture"
max_active_sessions = 1
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	// The rig declares a base_branch that would win §3a precedence 3 if it were
	// a scope source; the test proves it is NOT, because it is not what the
	// work substitutes.
	if err := os.WriteFile(filepath.Join(formulaDir, "wf.toml"), []byte(`
formula = "wf"
version = 2
contract = "graph.v2"

[vars.base_branch]
default = "FORMULA-DEFAULT"

[[steps]]
id = "work"
title = "Work on {{base_branch}}"
metadata = { "gc.run_target" = "worker" }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	a := orders.Order{Name: "probe", Rig: "fixture", Formula: "wf", Trigger: "manual", FormulaLayer: formulaDir}
	store := beads.NewMemStore()
	var stdout, stderr bytes.Buffer
	// Caller supplies a branch too — proving neither the rig nor the caller
	// layer reaches substitution, only the formula default does.
	code := doOrderRunWithJSON([]orders.Order{a}, a.Name, a.Rig, cityDir, beads.OrdersStore{Store: store}, nil, false, map[string]string{"base_branch": "CALLER-BRANCH-IGNORED"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRunWithJSON = %d, want 0; stderr: %s", code, stderr.String())
	}

	all, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("listing store: %v", err)
	}
	var rootBranch, stepTitle string
	for _, b := range all {
		if raw := b.Metadata[beadmeta.TargetScopeMetadataKey]; raw != "" {
			res := targetscope.Parse(raw)
			if !res.Valid() {
				t.Fatalf("root scope state = %v, want valid", res.State)
			}
			rootBranch = res.Scope.Branch
		}
		if strings.HasPrefix(b.Title, "Work on ") {
			stepTitle = b.Title
		}
	}
	if rootBranch == "" {
		t.Fatal("no root carried a target scope; the order boundary did not stamp")
	}
	if stepTitle != "Work on "+rootBranch {
		t.Fatalf("step title %q disagrees with scope.branch %q — the stamped scope is not the executed branch", stepTitle, rootBranch)
	}
	if rootBranch != "FORMULA-DEFAULT" {
		t.Fatalf("scope.branch = %q, want FORMULA-DEFAULT (the value substitution actually uses)", rootBranch)
	}
}

// A formula consuming no branch carrier still gets a present-valid field-empty
// object (§2c). Absence is the only state that re-enables the cwd writers, so
// "nothing to declare" must never be implemented as writing nothing.
func TestOrderCarrierlessFormulaStillStampsAnUnknownScope(t *testing.T) {
	dir := t.TempDir()
	body := "formula = \"order-carrierless\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n"
	if err := os.WriteFile(filepath.Join(dir, "order-carrierless.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := orders.Order{Name: "carrierless-order", Trigger: "manual", Formula: "order-carrierless", FormulaLayer: dir}

	recipe, scope, err := prepareOrderWispRecipe(context.Background(), beads.NewMemStore(), a, []string{dir}, nil, "")
	if err != nil {
		t.Fatalf("prepareOrderWispRecipe: %v", err)
	}
	if err := stampOrderWispRoot(recipe, scope); err != nil {
		t.Fatalf("stampOrderWispRoot: %v", err)
	}
	got := targetscope.Parse(recipe.Steps[0].Metadata[beadmeta.TargetScopeMetadataKey])
	if !got.Valid() {
		t.Fatalf("scope state = %v, want a present-valid field-empty object, never absent", got.State)
	}
}
