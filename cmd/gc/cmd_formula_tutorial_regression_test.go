package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitTutorialProviderWritesWorkspaceProvider(t *testing.T) {
	configureSupervisorHooksForTests()
	configureIsolatedRuntimeEnv(t)
	t.Setenv("PATH", os.Getenv("PATH"))

	cityDir := filepath.Join(t.TempDir(), "my-city")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "--skip-provider-readiness", "--provider", "claude", cityDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(init) = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if !strings.Contains(string(data), `provider = "claude"`) {
		t.Fatalf("tutorial init contract: city.toml should record workspace provider claude, got:\n%s", string(data))
	}
}

func TestFormulaShowTutorialStepCountMatchesRenderedSteps(t *testing.T) {
	cityDir := writeTutorialFormulaCity(t, "pancakes", `
formula = "pancakes"
description = "Make pancakes from scratch"

[[steps]]
id = "dry"
title = "Mix dry ingredients"

[[steps]]
id = "wet"
title = "Mix wet ingredients"

[[steps]]
id = "combine"
title = "Combine wet and dry"
needs = ["dry", "wet"]

[[steps]]
id = "cook"
title = "Cook the pancakes"
needs = ["combine"]

[[steps]]
id = "serve"
title = "Serve"
needs = ["cook"]
`)

	t.Chdir(cityDir)

	var stdout bytes.Buffer
	cmd := newFormulaShowCmd(&stdout, &bytes.Buffer{})
	cmd.SetArgs([]string{"pancakes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formula show execute: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Steps (5):") {
		t.Fatalf("formula show should report 5 rendered steps, got:\n%s", out)
	}
}

func TestFormulaShowTutorialConditionUsesDefaultVars(t *testing.T) {
	cityDir := writeTutorialFormulaCity(t, "deploy-flow", `
formula = "deploy-flow"

[vars]
env = "dev"

[[steps]]
id = "build"
title = "Build"

[[steps]]
id = "deploy"
title = "Deploy to staging"
condition = "{{env}} == staging"
`)

	t.Chdir(cityDir)

	var stdout bytes.Buffer
	cmd := newFormulaShowCmd(&stdout, &bytes.Buffer{})
	cmd.SetArgs([]string{"deploy-flow"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formula show execute: %v", err)
	}

	out := stdout.String()
	if strings.Contains(out, "deploy-flow.deploy") {
		t.Fatalf("formula show should apply default vars to conditions and omit deploy step, got:\n%s", out)
	}
}

func TestFormulaShowDoesNotRejectRequiredVars(t *testing.T) {
	cityDir := writeTutorialFormulaCity(t, "required-vars", `
formula = "required-vars"
description = "Formula with required vars"

[vars.epic]
description = "Epic ticket ID"
required = true

[vars.feature]
description = "Feature slug"
required = true

[[steps]]
id = "implement"
title = "[{{epic}}] Implement: {{feature}}"
`)

	t.Chdir(cityDir)

	var stdout bytes.Buffer
	cmd := newFormulaShowCmd(&stdout, &bytes.Buffer{})
	cmd.SetArgs([]string{"required-vars"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formula show should succeed without --var flags on required-var formulas: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "{{epic}}") || !strings.Contains(out, "{{feature}}") {
		t.Fatalf("formula show should display placeholders intact, got:\n%s", out)
	}
}

func writeTutorialFormulaCity(t *testing.T, formulaName, formulaBody string) string {
	t.Helper()

	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityDir := t.TempDir()
	writeFile := func(rel, body string) {
		t.Helper()
		path := filepath.Join(cityDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	writeFile("city.toml", "[workspace]\nname = \"my-city\"\nprovider = \"claude\"\n")
	writeFile("formulas/"+formulaName+".formula.toml", formulaBody)
	return cityDir
}
