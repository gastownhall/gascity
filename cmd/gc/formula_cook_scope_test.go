package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// writeCookScopeCity writes a minimal formula_v2-enabled city and returns its
// formulas directory.
func writeCookScopeCity(t *testing.T, cityDir string) string {
	t.Helper()
	toml := withBuiltinProviderAliasesTOMLForTest(`
[workspace]
name = "my-city"
provider = "claude"

[daemon]
formula_v2 = true
`, "claude") + testControlDispatcherAgentTOML("")
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	formulaDir := filepath.Join(cityDir, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	return formulaDir
}

func writeCookFormula(t *testing.T, formulaDir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(formulaDir, name+".formula.toml"), []byte(strings.TrimSpace(body)+"\n"), 0o644); err != nil {
		t.Fatalf("write formula %s: %v", name, err)
	}
}

// cookScopeReadback returns the branch of the single root that carries a target
// scope, and the branch a "Work on <branch>" step title substituted, so a test
// can assert the two agree.
func cookScopeReadback(t *testing.T, store beads.Store) (rootRes targetscope.Resolution, stepBranch string, sawRoot bool) {
	t.Helper()
	all, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("listing store: %v", err)
	}
	for _, b := range all {
		if raw := b.Metadata[beadmeta.TargetScopeMetadataKey]; raw != "" {
			rootRes = targetscope.Parse(raw)
			sawRoot = true
		}
		if strings.HasPrefix(b.Title, "Work on ") {
			stepBranch = strings.TrimPrefix(b.Title, "Work on ")
		}
	}
	return rootRes, stepBranch, sawRoot
}

// The load-bearing correctness property for the cook boundary, end to end:
// scope.branch equals the branch the materialized work actually runs against.
//
// Cook is UNLIKE an order here — cook materializes with molecule.Instantiate
// (recipe, Options{Vars: cookVars}), so cookVars DO reach substitution. A caller
// --var base_branch=release must therefore land on BOTH the substituted step
// title AND the stamped scope. This drives the real `gc formula cook` and
// asserts the stamped scope and the substituted title are the same branch.
func TestFormulaCookStandaloneGraphScopeEqualsSubstitutedBranch(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	cityDir := t.TempDir()
	formulaDir := writeCookScopeCity(t, cityDir)
	writeCookFormula(t, formulaDir, "graph-scoped", `
formula = "graph-scoped"
version = 2
contract = "graph.v2"

[vars.base_branch]
default = "FORMULA-DEFAULT"

[[steps]]
id = "work"
title = "Work on {{base_branch}}"
`)
	formulatest.SetupHermeticCookEnv(t, cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaCookCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"graph-scoped", "--var", "base_branch=release", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formula cook: %v\nstderr=%s", err, stderr.String())
	}

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rootRes, stepBranch, sawRoot := cookScopeReadback(t, store)
	if !sawRoot {
		t.Fatal("no root carried a target scope; the cook boundary did not stamp")
	}
	if !rootRes.Valid() {
		t.Fatalf("root scope state = %v, want valid", rootRes.State)
	}
	if stepBranch != rootRes.Scope.Branch {
		t.Fatalf("step title branch %q disagrees with scope.branch %q — the stamped scope is not the executed branch", stepBranch, rootRes.Scope.Branch)
	}
	if rootRes.Scope.Branch != "release" {
		t.Fatalf("scope.branch = %q, want release (the caller --var that substitution uses)", rootRes.Scope.Branch)
	}
}

// The default-only case: with no branch supplied anywhere, scope.branch is the
// formula default, and it equals what substitution uses. This is what including
// the FormulaDefaults layer buys — the scope is not unknown while substitution
// quietly uses the default.
func TestFormulaCookStandaloneGraphDefaultOnlyScope(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	cityDir := t.TempDir()
	formulaDir := writeCookScopeCity(t, cityDir)
	writeCookFormula(t, formulaDir, "graph-scoped", `
formula = "graph-scoped"
version = 2
contract = "graph.v2"

[vars.base_branch]
default = "the-default"

[[steps]]
id = "work"
title = "Work on {{base_branch}}"
`)
	formulatest.SetupHermeticCookEnv(t, cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaCookCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"graph-scoped", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formula cook: %v\nstderr=%s", err, stderr.String())
	}

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rootRes, stepBranch, sawRoot := cookScopeReadback(t, store)
	if !sawRoot || !rootRes.Valid() {
		t.Fatalf("root scope sawRoot=%v state=%v, want a present-valid stamp", sawRoot, rootRes.State)
	}
	if rootRes.Scope.Branch != "the-default" || stepBranch != "the-default" {
		t.Fatalf("scope.branch=%q step=%q, want both the-default", rootRes.Scope.Branch, stepBranch)
	}
}

// The 1d inline path. A legacy (non-graph) formula cooks through molecule.Cook,
// which compiles and instantiates in one call. Inlining it to compile -> stamp
// -> instantiate is what lets the root carry a scope; a legacy cook that still
// went through molecule.Cook would leave the root ABSENT.
func TestFormulaCookLegacyInlineStampsScope(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	cityDir := t.TempDir()
	formulaDir := writeCookScopeCity(t, cityDir)
	writeCookFormula(t, formulaDir, "legacy-scoped", `
formula = "legacy-scoped"
version = 1

[vars.base_branch]
default = "main"

[[steps]]
id = "work"
title = "Work on {{base_branch}}"
`)
	formulatest.SetupHermeticCookEnv(t, cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaCookCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"legacy-scoped", "--var", "base_branch=legacy-release", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formula cook: %v\nstderr=%s", err, stderr.String())
	}

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rootRes, stepBranch, sawRoot := cookScopeReadback(t, store)
	if !sawRoot {
		t.Fatal("legacy inline cook stamped no scope; molecule.Cook was not inlined")
	}
	if !rootRes.Valid() || rootRes.Scope.Branch != "legacy-release" {
		t.Fatalf("scope state=%v branch=%q, want valid legacy-release", rootRes.State, rootRes.Scope.Branch)
	}
	if stepBranch != "legacy-release" {
		t.Fatalf("step title branch %q disagrees with scope.branch; substitution and scope diverged", stepBranch)
	}
}

// A cook --attach declares the attach target's scope on the target bead itself,
// not only on the sub-DAG root. The target is normalized into a single-item
// input convoy in THIS store, so it is a member: without the member declaration
// the one bead a close gate reads directly would resolve ABSENT and fall back to
// its stale flat keys, the exact poison the object removes.
func TestFormulaCookAttachGraphV2DeclaresTargetMemberScope(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	cityDir := t.TempDir()
	formulaDir := writeCookScopeCity(t, cityDir)
	writeCookFormula(t, formulaDir, "graph-scoped-convoy", `
formula = "graph-scoped-convoy"
version = 2
contract = "graph.v2"

[vars.base_branch]
default = "release-x"

[[steps]]
id = "work"
title = "Do {{convoy_id}} on {{base_branch}}"
`)
	formulatest.SetupHermeticCookEnv(t, cityDir)

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	source, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	var stdout, stderr bytes.Buffer
	cmd := newFormulaCookCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"graph-scoped-convoy", "--attach", source.ID, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formula cook --attach: %v\nstderr=%s", err, stderr.String())
	}

	after, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("re-open store: %v", err)
	}
	got, err := after.Get(source.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	res := targetscope.Parse(got.Metadata[beadmeta.TargetScopeMetadataKey])
	if !res.Valid() {
		t.Fatalf("attach target scope state = %v, want the member declaration to have committed a valid scope", res.State)
	}
	if res.Scope.Branch != "release-x" {
		t.Fatalf("attach target scope.branch = %q, want release-x (the formula default the cook resolved)", res.Scope.Branch)
	}
}

// A formula consuming no branch carrier still gets a present-valid field-empty
// object (§2c). Absence is the only state that re-enables the cwd writers, so
// "nothing to declare" must never be implemented as writing nothing.
func TestFormulaCookCarrierlessStampsUnknownScope(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	cityDir := t.TempDir()
	formulaDir := writeCookScopeCity(t, cityDir)
	writeCookFormula(t, formulaDir, "graph-carrierless", `
formula = "graph-carrierless"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Work with no branch"
`)
	formulatest.SetupHermeticCookEnv(t, cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaCookCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"graph-carrierless", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formula cook: %v\nstderr=%s", err, stderr.String())
	}

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rootRes, _, sawRoot := cookScopeReadback(t, store)
	if !sawRoot {
		t.Fatal("carrierless cook stamped nothing; §2c requires a present-valid field-empty object")
	}
	if !rootRes.Valid() {
		t.Fatalf("scope state = %v, want present-valid (field-empty), never absent", rootRes.State)
	}
	if rootRes.Scope.Branch != "" {
		t.Fatalf("scope.branch = %q, want empty (unknown) for a carrierless formula", rootRes.Scope.Branch)
	}
}
