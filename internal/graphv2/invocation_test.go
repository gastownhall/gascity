package graphv2

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formulatest"
)

func TestPrepareInvocationCreatesInputConvoyForBeadTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "work.formula.toml", `
formula = "work"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	inv, err := PrepareInvocation(context.Background(), store, "work", []string{dir}, target.ID, nil)
	if err != nil {
		t.Fatalf("PrepareInvocation: %v", err)
	}
	if inv.InputConvoy == "" {
		t.Fatalf("invocation = %+v, want input convoy", inv)
	}
	if got := inv.Vars[ConvoyIDVar]; got != inv.InputConvoy {
		t.Fatalf("vars[%s] = %q, want %q", ConvoyIDVar, got, inv.InputConvoy)
	}
	members, err := convoycore.Members(store, inv.InputConvoy, true)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 1 || members[0].ID != target.ID {
		t.Fatalf("members = %+v, want target %s", members, target.ID)
	}
	created, err := store.Get(inv.InputConvoy)
	if err != nil {
		t.Fatalf("Get(input convoy): %v", err)
	}
	if created.Type != "convoy" {
		t.Fatalf("input convoy type = %q, want convoy", created.Type)
	}
	wantMetadata := map[string]string{"gc.synthetic": "true"}
	if !maps.Equal(created.Metadata, wantMetadata) {
		t.Fatalf("input convoy metadata = %+v, want %+v", created.Metadata, wantMetadata)
	}

	again, err := PrepareInvocation(context.Background(), store, "work", []string{dir}, target.ID, nil)
	if err != nil {
		t.Fatalf("PrepareInvocation again: %v", err)
	}
	if again.InputConvoy == inv.InputConvoy {
		t.Fatalf("input convoy was reused: first=%s second=%s", inv.InputConvoy, again.InputConvoy)
	}
}

func TestPrepareInvocationUsesFormulaCompilerRequirement(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "work.formula.toml", `
formula = "work"
type = "workflow"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	isGraph, _, err := IsGraphV2Formula("work", []string{dir})
	if err != nil {
		t.Fatalf("IsGraphV2Formula: %v", err)
	}
	if !isGraph {
		t.Fatal("IsGraphV2Formula = false, want formula_compiler requirement to opt into graph semantics")
	}
	inv, err := PrepareInvocation(context.Background(), store, "work", []string{dir}, target.ID, nil)
	if err != nil {
		t.Fatalf("PrepareInvocation: %v", err)
	}
	if inv.InputConvoy == "" {
		t.Fatalf("InputConvoy empty for formula_compiler graph invocation: %+v", inv)
	}
}

func TestPrepareInvocationHonorsFormulaRef(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	invocationGitOK(t)
	root := initInvocationRepo(t)
	formulaDir := filepath.Join(root, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFormula(t, formulaDir, "work.formula.toml", `
formula = "work"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	runInvocationGit(t, root, "add", "formulas/work.formula.toml")
	runInvocationGit(t, root, "commit", "-m", "graph formula")

	writeFormula(t, formulaDir, "work.formula.toml", `
formula = "work"
version = 1
type = "workflow"

[[steps]]
id = "legacy"
title = "Legacy working tree edit"
`)
	t.Setenv("GC_FORMULA_REF", "main")

	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	inv, err := PrepareInvocation(context.Background(), store, "work", []string{formulaDir}, target.ID, nil)
	if err != nil {
		t.Fatalf("PrepareInvocation: %v", err)
	}
	if inv.InputConvoy == "" {
		t.Fatalf("InputConvoy empty; graph.v2 loader read working-tree formula instead of GC_FORMULA_REF")
	}
	if got := inv.Vars[ConvoyIDVar]; got != inv.InputConvoy {
		t.Fatalf("vars[%s] = %q, want %q", ConvoyIDVar, got, inv.InputConvoy)
	}
}

func TestPrepareInvocationRejectsClosedTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "work.formula.toml", `
formula = "work"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	if err := store.Close(target.ID); err != nil {
		t.Fatalf("Close target: %v", err)
	}

	_, err = PrepareInvocation(context.Background(), store, "work", []string{dir}, target.ID, nil)
	if err == nil {
		t.Fatal("PrepareInvocation succeeded, want closed target error")
	}
	if !strings.Contains(err.Error(), "is closed") {
		t.Fatalf("error = %q, want closed target", err)
	}
}

func TestPrepareInvocationDoesNotReusePreviousInputConvoy(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "work.formula.toml", `
formula = "work"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	existing, err := store.Create(beads.Bead{
		Title:    "existing input",
		Type:     "convoy",
		Metadata: map[string]string{"gc.synthetic": "true"},
	})
	if err != nil {
		t.Fatalf("Create existing input convoy: %v", err)
	}

	inv, err := PrepareInvocation(context.Background(), store, "work", []string{dir}, target.ID, nil)
	if err != nil {
		t.Fatalf("PrepareInvocation: %v", err)
	}
	if inv.InputConvoy == existing.ID {
		t.Fatalf("InputConvoy = %q, want a fresh input convoy", inv.InputConvoy)
	}
	members, err := convoycore.Members(store, inv.InputConvoy, true)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 1 || members[0].ID != target.ID {
		t.Fatalf("members = %+v, want target %s", members, target.ID)
	}
}

func TestPrepareInvocationUsesExistingConvoyTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "work.formula.toml", `
formula = "work"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	store := beads.NewMemStore()
	convoy, err := store.Create(beads.Bead{Title: "input", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create convoy: %v", err)
	}

	inv, err := PrepareInvocation(context.Background(), store, "work", []string{dir}, convoy.ID, nil)
	if err != nil {
		t.Fatalf("PrepareInvocation: %v", err)
	}
	if inv.InputConvoy != convoy.ID {
		t.Fatalf("invocation = %+v, want existing convoy", inv)
	}
}

func TestPrepareInvocationRejectsCallerReservedVars(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "work.formula.toml", `
formula = "work"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	_, err = PrepareInvocation(context.Background(), store, "work", []string{dir}, target.ID, map[string]string{"issue": target.ID})
	if err == nil {
		t.Fatal("PrepareInvocation succeeded, want reserved var error")
	}
	if !strings.Contains(err.Error(), "reserved variable") {
		t.Fatalf("error = %q, want reserved variable", err)
	}
}

func TestPrepareInvocationRejectsMissingParentRuntimeVarsBeforeNormalizingTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "work.formula.toml", `
formula = "work"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}} with {{missing}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	_, err = PrepareInvocation(context.Background(), store, "work", []string{dir}, target.ID, nil)
	if err == nil {
		t.Fatal("PrepareInvocation succeeded, want missing runtime var error")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %q, want missing runtime var", err)
	}
	inputConvoys, err := store.List(beads.ListQuery{Type: "convoy"})
	if err != nil {
		t.Fatalf("List input convoys: %v", err)
	}
	if len(inputConvoys) != 0 {
		t.Fatalf("input convoys = %+v, want none before validation succeeds", inputConvoys)
	}
}

func TestPrepareInvocationTargetlessRejectsConvoyReferenceFromExpansion(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 2
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work"
expand = "needs-convoy"
`)
	writeFormula(t, dir, "needs-convoy.formula.toml", `
formula = "needs-convoy"
version = 2
contract = "graph.v2"
type = "expansion"

[[template]]
id = "{target}.expanded"
title = "Expanded {{convoy_id}}"
`)

	_, err := PrepareInvocation(context.Background(), beads.NewMemStore(), "parent", []string{dir}, "", nil)
	if err == nil {
		t.Fatal("PrepareInvocation succeeded, want targetless expanded convoy_id error")
	}
	if !strings.Contains(err.Error(), "convoy_id requires a targeted graph.v2 invocation") {
		t.Fatalf("error = %q, want expanded convoy_id target error", err)
	}
}

func TestPrepareInvocationTargetlessRejectsConditionedConvoyReferenceFromExpansionBeforeFiltering(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 2
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work"
expand = "conditioned-convoy"
`)
	writeFormula(t, dir, "conditioned-convoy.formula.toml", `
formula = "conditioned-convoy"
version = 2
type = "expansion"

[[template]]
id = "{target}.targetless-only"
title = "Targetless-only work"
condition = "!{{convoy_id}}"
`)

	_, err := PrepareInvocation(context.Background(), beads.NewMemStore(), "parent", []string{dir}, "", nil)
	if err == nil {
		t.Fatal("PrepareInvocation succeeded, want targetless expanded condition convoy_id error")
	}
	if !strings.Contains(err.Error(), "convoy_id requires a targeted graph.v2 invocation") {
		t.Fatalf("error = %q, want expanded condition convoy_id target error", err)
	}
}

func TestPrepareInvocationTargetlessRejectsConditionedConvoyReferenceBeforeFiltering(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "work.formula.toml", `
formula = "work"
version = 2
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "skip-when-targeted"
title = "Only targetless"
condition = "!{{convoy_id}}"
`)

	_, err := PrepareInvocation(context.Background(), beads.NewMemStore(), "work", []string{dir}, "", nil)
	if err == nil {
		t.Fatal("PrepareInvocation succeeded, want targetless conditioned convoy_id error")
	}
	if !strings.Contains(err.Error(), "convoy_id requires a targeted graph.v2 invocation") {
		t.Fatalf("error = %q, want conditioned convoy_id target error", err)
	}
}

func TestPreparePreviewInvocationUsesPreviewInputConvoyForBeadTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "work.formula.toml", `
formula = "work"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	inv, err := PreparePreviewInvocation(context.Background(), store, "work", []string{dir}, target.ID, nil)
	if err != nil {
		t.Fatalf("PreparePreviewInvocation: %v", err)
	}
	want := previewInputConvoyPrefix + target.ID
	if inv.InputConvoy != want {
		t.Fatalf("preview invocation = %+v, want preview input convoy %q", inv, want)
	}
	if got := inv.Vars[ConvoyIDVar]; got != want {
		t.Fatalf("vars[%s] = %q, want %q", ConvoyIDVar, got, want)
	}
	matches, err := store.List(beads.ListQuery{Type: "convoy"})
	if err != nil {
		t.Fatalf("List convoys: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("preview created input convoys = %+v, want none", matches)
	}
}

func TestPrepareInvocationRejectsUnsupportedDrainFromExpansionBeforeNormalizingTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 2
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work"
expand = "bad-drain"
`)
	writeFormula(t, dir, "bad-drain.formula.toml", `
formula = "bad-drain"
version = 2
contract = "graph.v2"
type = "expansion"

[[template]]
id = "{target}.drain"
title = "Drain"

[template.drain]
context = "separate"
formula = "item"
max_units = 101
`)
	writeFormula(t, dir, "item.formula.toml", `
formula = "item"
version = 2
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Item {{convoy_id}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	_, err = PrepareInvocation(context.Background(), store, "parent", []string{dir}, target.ID, nil)
	if err == nil {
		t.Fatal("PrepareInvocation succeeded, want expanded drain validation error")
	}
	if !strings.Contains(err.Error(), `max_units must be <= 100`) {
		t.Fatalf("error = %q, want expanded drain max_units error", err)
	}
	matches, err := store.List(beads.ListQuery{Type: "convoy"})
	if err != nil {
		t.Fatalf("List input convoys: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("input convoys = %+v, want none before validation succeeds", matches)
	}
}

func TestPrepareInvocationRejectsNonGraphDrainItemFormulaBeforeNormalizingTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "drain"
title = "Drain"

[steps.drain]
context = "separate"
formula = "item"
`)
	writeFormula(t, dir, "item.formula.toml", `
formula = "item"
version = 1
type = "workflow"

[[steps]]
id = "work"
title = "Legacy item"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	_, err = PrepareInvocation(context.Background(), store, "parent", []string{dir}, target.ID, nil)
	if err == nil {
		t.Fatal("PrepareInvocation succeeded, want drain item graph.v2 error")
	}
	if !strings.Contains(err.Error(), "must declare contract = \"graph.v2\"") {
		t.Fatalf("error = %q, want graph.v2 item formula message", err)
	}
	matches, err := store.List(beads.ListQuery{Type: "convoy"})
	if err != nil {
		t.Fatalf("List input convoys: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("input convoys = %+v, want none before validation succeeds", matches)
	}
}

func TestPrepareInvocationRejectsDrainItemFormulaMissingRuntimeVars(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "drain"
title = "Drain"

[steps.drain]
context = "separate"
formula = "item"
`)
	writeFormula(t, dir, "item.formula.toml", `
formula = "item"
version = 1
contract = "graph.v2"
type = "workflow"

[vars]
[vars.extra]
required = true

[[steps]]
id = "work"
title = "Item {{convoy_id}} {{extra}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	_, err = PrepareInvocation(context.Background(), store, "parent", []string{dir}, target.ID, nil)
	if err == nil {
		t.Fatal("PrepareInvocation succeeded, want drain item runtime var error")
	}
	if !strings.Contains(err.Error(), "runtime vars") || !strings.Contains(err.Error(), "extra") {
		t.Fatalf("error = %q, want missing item runtime var", err)
	}
}

func TestPrepareInvocationPassesParentRuntimeVarsToDrainItemValidation(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "drain"
title = "Drain"

[steps.drain]
context = "separate"
formula = "item"
`)
	writeFormula(t, dir, "item.formula.toml", `
formula = "item"
version = 1
contract = "graph.v2"
type = "workflow"

[vars]
[vars.extra]
required = true

[[steps]]
id = "work"
title = "Item {{convoy_id}} {{extra}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	inv, err := PrepareInvocation(context.Background(), store, "parent", []string{dir}, target.ID, map[string]string{"extra": "provided"})
	if err != nil {
		t.Fatalf("PrepareInvocation: %v", err)
	}
	if got := inv.Vars["extra"]; got != "provided" {
		t.Fatalf("vars[extra] = %q, want provided", got)
	}
}

func TestPrepareInvocationPassesParentDefaultVarsToDrainItemValidation(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 1
contract = "graph.v2"
type = "workflow"

[vars]
[vars.extra]
default = "from-default"

[[steps]]
id = "drain"
title = "Drain"

[steps.drain]
context = "separate"
formula = "item"
`)
	writeFormula(t, dir, "item.formula.toml", `
formula = "item"
version = 1
contract = "graph.v2"
type = "workflow"

[vars]
[vars.extra]
required = true

[[steps]]
id = "work"
title = "Item {{convoy_id}} {{extra}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	inv, err := PrepareInvocation(context.Background(), store, "parent", []string{dir}, target.ID, nil)
	if err != nil {
		t.Fatalf("PrepareInvocation: %v", err)
	}
	if got := inv.Vars["extra"]; got != "from-default" {
		t.Fatalf("vars[extra] = %q, want formula default", got)
	}
}

func TestRuntimeVarsMetadataExcludesReservedVars(t *testing.T) {
	raw := RuntimeVarsMetadata(map[string]string{
		ConvoyIDVar: "CONVOY-1",
		"issue":     "BD-1",
		"extra":     "provided",
	})
	if strings.Contains(raw, ConvoyIDVar) || strings.Contains(raw, "issue") {
		t.Fatalf("RuntimeVarsMetadata = %q, want reserved vars excluded", raw)
	}
	parsed, err := ParseRuntimeVarsMetadata(raw)
	if err != nil {
		t.Fatalf("ParseRuntimeVarsMetadata: %v", err)
	}
	if len(parsed) != 1 || parsed["extra"] != "provided" {
		t.Fatalf("parsed = %#v, want only extra", parsed)
	}
}

func writeFormula(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.TrimSpace(contents)+"\n"), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
}

func invocationGitOK(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH")
	}
}

func initInvocationRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runInvocationGit(t, root, "init", "-b", "main")
	runInvocationGit(t, root, "config", "user.email", "test@example.com")
	runInvocationGit(t, root, "config", "user.name", "test")
	runInvocationGit(t, root, "config", "commit.gpgsign", "false")
	return root
}

func runInvocationGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
