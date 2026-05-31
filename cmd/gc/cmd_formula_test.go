package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// TestResolveFormulaScope_RigFlagWins verifies that an explicit --rig flag
// takes priority over the cwd, and that the rig's FormulaLayers are used.
func TestResolveFormulaScope_RigFlagWins(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "my-project")
	otherPath := filepath.Join(cityPath, "other-rig")
	for _, p := range []string{rigPath, otherPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}

	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "my-project", Path: rigPath},
			{Name: "other-rig", Path: otherPath},
		},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"my-project": {"/city/formulas", "/rigs/my-project/formulas"},
				"other-rig":  {"/city/formulas", "/rigs/other-rig/formulas"},
			},
		},
	}

	t.Chdir(otherPath) // cwd would otherwise resolve to other-rig
	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = "my-project"

	scope, err := resolveFormulaScope(cfg, cityPath)
	if err != nil {
		t.Fatalf("resolveFormulaScope: %v", err)
	}
	if scope.storeRoot != rigPath {
		t.Errorf("storeRoot = %q, want %q", scope.storeRoot, rigPath)
	}
	want := []string{"/city/formulas", "/rigs/my-project/formulas"}
	if !reflect.DeepEqual(scope.searchPaths, want) {
		t.Errorf("searchPaths = %v, want %v", scope.searchPaths, want)
	}
}

// TestResolveFormulaScope_CwdInsideRig falls back to cwd when --rig is unset.
// Asserts searchPaths too — the core bug in #1004 was search paths dropping
// back to city layers even when storeRoot was rig-correct.
func TestResolveFormulaScope_CwdInsideRig(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "my-project", Path: rigPath},
		},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"my-project": {"/city/formulas", "/rigs/my-project/formulas"},
			},
		},
	}

	t.Chdir(rigPath)
	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = ""

	scope, err := resolveFormulaScope(cfg, cityPath)
	if err != nil {
		t.Fatalf("resolveFormulaScope: %v", err)
	}
	if scope.storeRoot != rigPath {
		t.Errorf("storeRoot = %q, want %q", scope.storeRoot, rigPath)
	}
	want := []string{"/city/formulas", "/rigs/my-project/formulas"}
	if !reflect.DeepEqual(scope.searchPaths, want) {
		t.Errorf("searchPaths = %v, want %v", scope.searchPaths, want)
	}
}

// TestResolveFormulaScope_CityScopeWhenNoRig returns city defaults when the
// cwd is inside the city root but outside any declared rig and --rig is unset.
func TestResolveFormulaScope_CityScopeWhenNoRig(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
		},
	}

	t.Chdir(cityPath)
	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = ""

	scope, err := resolveFormulaScope(cfg, cityPath)
	if err != nil {
		t.Fatalf("resolveFormulaScope: %v", err)
	}
	if scope.storeRoot != cityPath {
		t.Errorf("storeRoot = %q, want %q", scope.storeRoot, cityPath)
	}
	want := []string{"/city/formulas"}
	if !reflect.DeepEqual(scope.searchPaths, want) {
		t.Errorf("searchPaths = %v, want %v", scope.searchPaths, want)
	}
}

// TestResolveFormulaScope_UnknownRigErrors surfaces a clear error when the
// user passes a --rig name that doesn't exist.
func TestResolveFormulaScope_UnknownRigErrors(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "real", Path: filepath.Join(cityPath, "real")}},
	}

	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = "ghost"

	_, err := resolveFormulaScope(cfg, cityPath)
	if err == nil {
		t.Fatal("expected error for unknown rig, got nil")
	}
	if !strings.Contains(err.Error(), `rig "ghost" not found`) {
		t.Errorf("error = %v, want substring 'rig \"ghost\" not found'", err)
	}
}

// TestResolveFormulaScope_UnboundRigErrors rejects a declared rig that has
// no path binding — matching the gc bd error semantics.
func TestResolveFormulaScope_UnboundRigErrors(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "unbound", Path: ""}},
	}

	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = "unbound"

	_, err := resolveFormulaScope(cfg, cityPath)
	if err == nil {
		t.Fatal("expected error for unbound rig, got nil")
	}
	if !strings.Contains(err.Error(), "no path binding") {
		t.Errorf("error = %v, want substring 'no path binding'", err)
	}
}

// TestRigFormulaVarsForScope verifies that rig-scoped formula_vars flow
// through the scope resolver so `gc formula show --rig <name>` can surface
// them as "(rig default=...)" annotations.
func TestRigFormulaVarsForScope(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "mo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{
			{
				Name: "mo",
				Path: rigPath,
				FormulaVars: map[string]string{
					"test_command": "make test-fast",
				},
			},
		},
	}

	t.Run("--rig populates FormulaVars via rigByName", func(t *testing.T) {
		prev := rigFlag
		t.Cleanup(func() { rigFlag = prev })
		rigFlag = "mo"

		r, ok := rigByName(cfg, "mo")
		if !ok {
			t.Fatalf("rigByName(mo) not found")
		}
		if got := r.FormulaVars["test_command"]; got != "make test-fast" {
			t.Errorf("FormulaVars[test_command] = %q, want %q", got, "make test-fast")
		}
	})

	t.Run("no --rig yields empty FormulaVars", func(t *testing.T) {
		prev := rigFlag
		t.Cleanup(func() { rigFlag = prev })
		rigFlag = ""

		t.Chdir(cityPath)
		// Without --rig and outside a rig cwd, formula_vars are not injected.
		vars := rigFormulaVarsForScope(cfg, cityPath)
		if len(vars) != 0 {
			t.Errorf("rigFormulaVarsForScope = %v, want empty (no rig context)", vars)
		}
	})
}

// TestResolveFormulaScope_RigFallsBackToCityLayers covers the case where a
// rig is resolved but has no rig-specific FormulaLayers entry; SearchPaths
// should fall back to city layers.
func TestResolveFormulaScope_RigFallsBackToCityLayers(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "bare-rig")

	cfg := &config.City{
		Rigs: []config.Rig{{Name: "bare-rig", Path: rigPath}},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
		},
	}

	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = "bare-rig"

	scope, err := resolveFormulaScope(cfg, cityPath)
	if err != nil {
		t.Fatalf("resolveFormulaScope: %v", err)
	}
	if scope.storeRoot != rigPath {
		t.Errorf("storeRoot = %q, want %q", scope.storeRoot, rigPath)
	}
	want := []string{"/city/formulas"}
	if !reflect.DeepEqual(scope.searchPaths, want) {
		t.Errorf("searchPaths = %v, want %v (city fallback)", scope.searchPaths, want)
	}
}

func TestFormulaShowJSONFromRecipe(t *testing.T) {
	defaultValue := "main"
	priority := 1
	recipe := &formula.Recipe{
		Name:        "mol-build",
		Description: "Build {{branch}}",
		Phase:       "liquid",
		Vars: map[string]*formula.VarDef{
			"branch": {
				Description: "branch to build",
				Default:     &defaultValue,
			},
			"target": {
				Description: "target name",
				Required:    true,
			},
		},
		Steps: []formula.RecipeStep{
			{ID: "mol-build", Title: "Build", Type: "molecule", IsRoot: true},
			{ID: "mol-build.test", Title: "Test {{target}}", Type: "task", Priority: &priority, Labels: []string{"ci"}},
		},
		Deps: []formula.RecipeDep{{StepID: "mol-build.test", DependsOnID: "mol-build", Type: "parent-child"}},
	}

	var stdout bytes.Buffer
	payload := formulaShowJSONFromRecipe(
		recipe,
		"/city",
		formulaScope{searchPaths: []string{"/city/formulas"}},
		map[string]string{"target": "fast"},
		map[string]string{"target": "unit"},
		map[string]string{"branch": "main", "target": "unit"},
	)
	if err := writeCLIJSONLine(&stdout, payload); err != nil {
		t.Fatalf("writeCLIJSONLine: %v", err)
	}

	var got struct {
		SchemaVersion string `json:"schema_version"`
		Name          string `json:"name"`
		Description   string `json:"description"`
		Vars          []struct {
			Name       string  `json:"name"`
			RigDefault *string `json:"rig_default"`
		} `json:"vars"`
		Steps []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("formula show JSON is invalid: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" || got.Name != "mol-build" || got.Description != "Build main" {
		t.Fatalf("payload = %+v", got)
	}
	if len(got.Vars) != 2 || got.Vars[1].Name != "target" || got.Vars[1].RigDefault == nil || *got.Vars[1].RigDefault != "fast" {
		t.Fatalf("vars = %+v", got.Vars)
	}
	if len(got.Steps) != 2 || got.Steps[1].Title != "Test unit" {
		t.Fatalf("steps = %+v", got.Steps)
	}
}

func TestFormulaCatalogEntriesUseResolvedWinnersAndSort(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()

	writeFormulaTestFile(t, cityDir, "build-run", `formula = "build-run"
description = "city build"

[catalog]
name = "build-run"
description = "City build workflow"

[[steps]]
id = "run"
title = "Run"
`)
	writeFormulaTestFile(t, cityDir, "internal-helper", `formula = "internal-helper"
description = "not user runnable"

[[steps]]
id = "run"
title = "Run"
`)
	writeFormulaTestFile(t, rigDir, "review", `formula = "review"
description = "review"

[catalog]
name = "review"
description = "Review a completed implementation."

[[steps]]
id = "review"
title = "Review"
`)
	writeFormulaTestFile(t, rigDir, "build-run", `formula = "build-run"
description = "rig build"

[catalog]
name = "build-run"
description = "Rig-specific build workflow"

[[steps]]
id = "run"
title = "Run"
`)

	got, warnings := formulaCatalogEntries([]string{cityDir, rigDir})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	want := []formulaCatalogEntryJSON{
		{Name: "build-run", Description: "Rig-specific build workflow"},
		{Name: "review", Description: "Review a completed implementation."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog entries = %+v, want %+v", got, want)
	}
}

func TestFormulaCatalogEntriesUseFormulaRefForDiscovery(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	root := t.TempDir()
	runGitForFormulaTest(t, root, "init", "-b", "main")
	runGitForFormulaTest(t, root, "config", "user.email", "test@example.com")
	runGitForFormulaTest(t, root, "config", "user.name", "test")
	runGitForFormulaTest(t, root, "config", "commit.gpgsign", "false")

	formulaDir := filepath.Join(root, "formulas")
	writeFormulaTestFile(t, formulaDir, "from-ref", `formula = "from-ref"

[catalog]
name = "from-ref"
description = "Committed workflow"

[[steps]]
id = "run"
title = "Run"
`)
	runGitForFormulaTest(t, root, "add", "formulas/from-ref.formula.toml")
	runGitForFormulaTest(t, root, "commit", "-m", "add committed catalog formula")
	runGitForFormulaTest(t, root, "checkout", "-b", "feature")
	if err := os.Remove(filepath.Join(formulaDir, "from-ref.formula.toml")); err != nil {
		t.Fatalf("remove committed formula from working tree: %v", err)
	}
	writeFormulaTestFile(t, formulaDir, "working-only", `formula = "working-only"

[catalog]
name = "working-only"
description = "Uncommitted workflow"

[[steps]]
id = "run"
title = "Run"
`)
	t.Setenv("GC_FORMULA_REF", "main")

	got, warnings := formulaCatalogEntries([]string{formulaDir})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	want := []formulaCatalogEntryJSON{{Name: "from-ref", Description: "Committed workflow"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog entries = %+v, want %+v", got, want)
	}
}

func TestFormulaCatalogEntriesWarnForPerFileFailures(t *testing.T) {
	dir := t.TempDir()
	writeFormulaTestFile(t, dir, "build-run", `formula = "build-run"

[catalog]
name = "build-run"
description = "Build workflow"

[[steps]]
id = "run"
title = "Run"
`)
	writeFormulaTestFile(t, dir, "broken-helper", `formula = "broken-helper"
[catalog
`)
	writeFormulaTestFile(t, dir, "bad-catalog", `formula = "bad-catalog"

[catalog]
name = "other-name"
description = "Bad metadata"

[[steps]]
id = "run"
title = "Run"
`)

	got, warnings := formulaCatalogEntries([]string{dir})
	want := []formulaCatalogEntryJSON{{Name: "build-run", Description: "Build workflow"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog entries = %+v, want %+v", got, want)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings len = %d, want 2: %+v", len(warnings), warnings)
	}
	if warnings[0].Code == "" || warnings[0].Message == "" {
		t.Fatalf("warning[0] = %+v, want structured code and message", warnings[0])
	}
	if warnings[1].Code == "" || warnings[1].Message == "" {
		t.Fatalf("warning[1] = %+v, want structured code and message", warnings[1])
	}
}

func TestFormulaCatalogEntriesWarnForInvalidMetadata(t *testing.T) {
	t.Run("name mismatch", func(t *testing.T) {
		dir := t.TempDir()
		writeFormulaTestFile(t, dir, "build-run", `formula = "build-run"

[catalog]
name = "build"
description = "Build workflow"

[[steps]]
id = "run"
title = "Run"
`)

		got, warnings := formulaCatalogEntries([]string{dir})
		if len(got) != 0 {
			t.Fatalf("catalog entries = %+v, want none", got)
		}
		if len(warnings) != 1 {
			t.Fatalf("warnings len = %d, want 1: %+v", len(warnings), warnings)
		}
		if !strings.Contains(warnings[0].Message, `catalog.name "build" must match formula name "build-run"`) {
			t.Fatalf("warning = %+v", warnings[0])
		}
	})

	t.Run("missing description", func(t *testing.T) {
		dir := t.TempDir()
		writeFormulaTestFile(t, dir, "review", `formula = "review"

[catalog]
name = "review"

[[steps]]
id = "run"
title = "Run"
`)

		got, warnings := formulaCatalogEntries([]string{dir})
		if len(got) != 0 {
			t.Fatalf("catalog entries = %+v, want none", got)
		}
		if len(warnings) != 1 {
			t.Fatalf("warnings len = %d, want 1: %+v", len(warnings), warnings)
		}
		if !strings.Contains(warnings[0].Message, `catalog.description is required`) {
			t.Fatalf("warning = %+v", warnings[0])
		}
	})
}

func TestFormulaCatalogCommandJSONFromEntries(t *testing.T) {
	var stdout bytes.Buffer
	payload := formulaCatalogJSONFromEntries([]formulaCatalogEntryJSON{
		{Name: "build-run", Description: "Build workflow"},
	}, []jsonContractWarning{{Code: "formula_catalog_parse_failed", Message: "skipped broken-helper"}})
	if err := writeCLIJSONLine(&stdout, payload); err != nil {
		t.Fatalf("writeCLIJSONLine: %v", err)
	}

	var got struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Formulas      []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
		} `json:"formulas"`
		Summary struct {
			Count int `json:"count"`
		} `json:"summary"`
		Warnings []jsonContractWarning `json:"warnings"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("formula catalog JSON is invalid: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" || !got.OK || got.Summary.Count != 1 {
		t.Fatalf("payload = %+v", got)
	}
	if len(got.Formulas) != 1 || got.Formulas[0].Name != "build-run" || got.Formulas[0].Description != "Build workflow" {
		t.Fatalf("formulas = %+v", got.Formulas)
	}
	if got.Formulas[0].Source != "" {
		t.Fatalf("catalog JSON leaked source path: %+v", got.Formulas[0])
	}
	if len(got.Warnings) != 1 || got.Warnings[0].Code != "formula_catalog_parse_failed" {
		t.Fatalf("warnings = %+v", got.Warnings)
	}
}

func TestFormulaCatalogCommandSetupErrorsSurfaceDiagnostics(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "catalog-test"
provider = "claude"

[[agent]]
name = "worker"
start_command = "echo hello"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	t.Chdir(cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	t.Run("text", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--city", cityDir, "--rig", "ghost", "formula", "catalog"}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		if got := stderr.String(); !strings.Contains(got, "gc formula catalog:") || !strings.Contains(got, `rig "ghost" not found`) {
			t.Fatalf("stderr = %q, want command-scoped rig diagnostic", got)
		}
	})

	t.Run("json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--city", cityDir, "--rig", "ghost", "formula", "catalog", "--json"}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		if got := stdout.String(); !strings.Contains(got, `"ok":false`) || !strings.Contains(got, `rig \"ghost\" not found`) {
			t.Fatalf("stdout = %q, want structured raw rig diagnostic", got)
		}
		if strings.Contains(stdout.String(), "command failed; see stderr for diagnostics") {
			t.Fatalf("stdout = %q, want specific diagnostic instead of errExit fallback", stdout.String())
		}
	})

	t.Run("subcommand returns errExit after text diagnostic", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		prevCityFlag, prevRigFlag := cityFlag, rigFlag
		t.Cleanup(func() {
			cityFlag = prevCityFlag
			rigFlag = prevRigFlag
		})
		cityFlag = cityDir
		rigFlag = "ghost"

		cmd := newFormulaCatalogCmd(&stdout, &stderr)
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		err := cmd.Execute()
		if !errors.Is(err, errExit) {
			t.Fatalf("error = %v, want errExit", err)
		}
		if got := stderr.String(); !strings.Contains(got, "gc formula catalog:") || !strings.Contains(got, `rig "ghost" not found`) {
			t.Fatalf("stderr = %q, want command-scoped rig diagnostic", got)
		}
	})
}

func writeFormulaTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir formula dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".formula.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write formula %s: %v", name, err)
	}
}

func runGitForFormulaTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
