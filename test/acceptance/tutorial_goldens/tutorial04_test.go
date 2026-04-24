//go:build acceptance_c

package tutorialgoldens

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTutorial05Formulas(t *testing.T) {
	ws := newTutorialWorkspace(t)
	ws.attachDiagnostics(t, "tutorial-05")

	myCity := expandHome(ws.home(), "~/my-city")
	myProject := expandHome(ws.home(), "~/my-project")
	myAPI := expandHome(ws.home(), "~/my-api")
	mustMkdirAll(t, myProject)
	mustMkdirAll(t, myAPI)

	out, err := ws.runShell("gc init ~/my-city --provider claude --skip-provider-readiness", "")
	if err != nil {
		t.Fatalf("seed city init: %v\n%s", err, out)
	}
	ws.setCWD(myCity)
	for _, cmd := range []string{"gc rig add ~/my-project", "gc rig add ~/my-api"} {
		if out, err := ws.runShell(cmd, ""); err != nil {
			t.Fatalf("seed rig add %q: %v\n%s", cmd, err, out)
		}
	}
	ws.noteWarning("tutorial 05 continuity workaround: the page assumes helper/reviewer agents and both rigs already exist, so the page driver seeds my-project, my-api, and those supporting agents before exercising the visible worker + formula commands")
	if out, err := ws.runShell("gc agent add --name helper", ""); err != nil {
		t.Fatalf("seed helper scaffold: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(myCity, "agents", "helper", "prompt.template.md"), "# Helper Agent\nHandle supporting work.\n", 0o644)
	if out, err := ws.runShell("gc agent add --name reviewer --dir my-project", ""); err != nil {
		t.Fatalf("seed reviewer scaffold: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(myCity, "agents", "reviewer", "agent.toml"), "dir = \"my-project\"\nprovider = \""+tutorialReviewerProvider()+"\"\n", 0o644)
	writeFile(t, filepath.Join(myCity, "agents", "reviewer", "prompt.template.md"), "# Reviewer Agent\nReview code.\n", 0o644)

	writeFile(t, filepath.Join(myCity, "formulas", "greeting.toml"), `formula = "greeting"

[vars]
name = "world"

[[steps]]
id = "say-hello"
title = "Say hello to {{name}}"
`, 0o644)

	writeFile(t, filepath.Join(myCity, "formulas", "feature-work.toml"), `formula = "feature-work"

[vars.title]
description = "What this feature is about"
required = true

[vars.branch]
description = "Target branch"
default = "main"

[vars.priority]
description = "How urgent is this"
default = "normal"
enum = ["low", "normal", "high", "critical"]

[[steps]]
id = "implement"
title = "Implement {{title}}"
description = "Work on {{title}} against {{branch}} (priority: {{priority}})"
`, 0o644)

	writeFile(t, filepath.Join(myCity, "formulas", "deploy-flow.toml"), `formula = "deploy-flow"

[vars]
env = "dev"

[[steps]]
id = "build"
title = "Build"

[[steps]]
id = "deploy"
title = "Deploy to staging"
condition = "{{env}} == staging"
`, 0o644)

	writeFile(t, filepath.Join(myCity, "formulas", "retry-deploy.toml"), `formula = "retry-deploy"

[[steps]]
id = "retries"
title = "Attempt deployment"

[steps.loop]
count = 3

[[steps.loop.body]]
id = "attempt"
title = "Try to deploy"
`, 0o644)

	var pancakesRootID string

	t.Run("cat > formulas/pancakes.toml << 'EOF'", func(t *testing.T) {
		cmd := tutorialPancakesFormulaShellCommand(t)
		if out, err := ws.runShell(cmd, ""); err != nil {
			t.Fatalf("writing pancakes formula: %v\n%s", err, out)
		}
	})

	t.Run("gc formula list", func(t *testing.T) {
		out, err := ws.runShell("gc formula list", "")
		if err != nil {
			t.Fatalf("gc formula list: %v\n%s", err, out)
		}
		for _, want := range []string{"pancakes", "greeting", "feature-work", "deploy-flow", "retry-deploy"} {
			if !strings.Contains(out, want) {
				t.Fatalf("formula list missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc formula show pancakes", func(t *testing.T) {
		out, err := ws.runShell("gc formula show pancakes", "")
		if err != nil {
			t.Fatalf("gc formula show pancakes: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Formula: pancakes") {
			t.Fatalf("formula show missing header:\n%s", out)
		}
		if !strings.Contains(out, "Steps (5):") {
			t.Fatalf("tutorial contract: pancakes should render 5 visible steps, got:\n%s", out)
		}
	})

	t.Run("gc agent add --name worker", func(t *testing.T) {
		out, err := ws.runShell("gc agent add --name worker", "")
		if err != nil {
			t.Fatalf("gc agent add --name worker: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Scaffolded agent 'worker'") {
			t.Fatalf("gc agent add output mismatch:\n%s", out)
		}
	})

	t.Run("cat > agents/worker/prompt.template.md << 'EOF'", func(t *testing.T) {
		cmd := `cat > agents/worker/prompt.template.md << 'EOF'
# Worker Agent
You are a general-purpose Gas City worker. Execute assigned work carefully and report the result.
EOF`
		if out, err := ws.runShell(cmd, ""); err != nil {
			t.Fatalf("writing worker prompt: %v\n%s", err, out)
		}
	})

	t.Run("gc sling mayor pancakes --formula", func(t *testing.T) {
		out, err := ws.runShell("gc sling mayor pancakes --formula", "")
		if err != nil {
			t.Fatalf("gc sling mayor pancakes --formula: %v\n%s", err, out)
		}
		if !strings.Contains(strings.ToLower(out), "slung formula") {
			t.Fatalf("formula sling output mismatch:\n%s", out)
		}
	})

	t.Run("gc formula cook pancakes", func(t *testing.T) {
		ws.setCWD(myProject)
		out, err := ws.runShell("gc formula cook pancakes", "")
		if err != nil {
			t.Fatalf("gc formula cook pancakes: %v\n%s", err, out)
		}
		pancakesRootID = firstBeadID(out)
		if pancakesRootID == "" {
			t.Fatalf("could not parse pancakes root id:\n%s", out)
		}
	})

	t.Run("gc sling worker mp-2wx", func(t *testing.T) {
		if pancakesRootID == "" {
			t.Fatal("missing pancakes root id")
		}
		out, err := ws.runShell(fmt.Sprintf("gc sling worker %s", pancakesRootID), "")
		if err != nil {
			t.Fatalf("gc sling worker %s: %v\n%s", pancakesRootID, err, out)
		}
		if !strings.Contains(out, "Slung") {
			t.Fatalf("gc sling worker output mismatch:\n%s", out)
		}
	})

	t.Run(`gc formula cook greeting --var name="Alice"`, func(t *testing.T) {
		ws.setCWD(myCity)
		out, err := ws.runShell(`gc formula cook greeting --var name="Alice"`, "")
		if err != nil {
			t.Fatalf("gc formula cook greeting --var name=Alice: %v\n%s", err, out)
		}
		if !strings.Contains(out, "greeting.say-hello") {
			t.Fatalf("cook greeting output mismatch:\n%s", out)
		}
	})

	t.Run("gc formula cook greeting", func(t *testing.T) {
		out, err := ws.runShell("gc formula cook greeting", "")
		if err != nil {
			t.Fatalf("gc formula cook greeting: %v\n%s", err, out)
		}
		if !strings.Contains(out, "greeting.say-hello") {
			t.Fatalf("cook greeting default output mismatch:\n%s", out)
		}
	})

	t.Run(`gc formula show greeting --var name="Alice"`, func(t *testing.T) {
		out, err := ws.runShell(`gc formula show greeting --var name="Alice"`, "")
		if err != nil {
			t.Fatalf("gc formula show greeting: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Say hello to Alice") {
			t.Fatalf("show greeting should substitute Alice:\n%s", out)
		}
	})

	t.Run(`gc formula cook feature-work --var title="Auth overhaul" --var branch="develop"`, func(t *testing.T) {
		out, err := ws.runShell(`gc formula cook feature-work --var title="Auth overhaul" --var branch="develop"`, "")
		if err != nil {
			t.Fatalf("gc formula cook feature-work branch variant: %v\n%s", err, out)
		}
		if !strings.Contains(out, "feature-work.implement") {
			t.Fatalf("feature-work cook output mismatch:\n%s", out)
		}
	})

	t.Run(`gc formula cook feature-work --var title="Auth overhaul" --var priority="critical"`, func(t *testing.T) {
		out, err := ws.runShell(`gc formula cook feature-work --var title="Auth overhaul" --var priority="critical"`, "")
		if err != nil {
			t.Fatalf("gc formula cook feature-work priority variant: %v\n%s", err, out)
		}
		if !strings.Contains(out, "feature-work.implement") {
			t.Fatalf("feature-work cook output mismatch:\n%s", out)
		}
	})

	t.Run(`gc formula show feature-work --var title="Auth system"`, func(t *testing.T) {
		out, err := ws.runShell(`gc formula show feature-work --var title="Auth system"`, "")
		if err != nil {
			t.Fatalf("gc formula show feature-work: %v\n%s", err, out)
		}
		for _, want := range []string{"Formula: feature-work", "Implement Auth system"} {
			if !strings.Contains(out, want) {
				t.Fatalf("feature-work show missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc formula show deploy-flow --var env=dev", func(t *testing.T) {
		out, err := ws.runShell("gc formula show deploy-flow --var env=dev", "")
		if err != nil {
			t.Fatalf("gc formula show deploy-flow env=dev: %v\n%s", err, out)
		}
		if strings.Contains(out, "deploy-flow.deploy") {
			t.Fatalf("deploy-flow env=dev should omit deploy step:\n%s", out)
		}
	})

	t.Run("gc formula show deploy-flow --var env=staging", func(t *testing.T) {
		out, err := ws.runShell("gc formula show deploy-flow --var env=staging", "")
		if err != nil {
			t.Fatalf("gc formula show deploy-flow env=staging: %v\n%s", err, out)
		}
		if !strings.Contains(out, "deploy-flow.deploy") {
			t.Fatalf("deploy-flow env=staging should include deploy step:\n%s", out)
		}
	})

	t.Run("gc formula show retry-deploy", func(t *testing.T) {
		out, err := ws.runShell("gc formula show retry-deploy", "")
		if err != nil {
			t.Fatalf("gc formula show retry-deploy: %v\n%s", err, out)
		}
		for _, want := range []string{
			"retry-deploy.retries.iter1.attempt",
			"retry-deploy.retries.iter2.attempt",
			"retry-deploy.retries.iter3.attempt",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("retry-deploy show missing %q:\n%s", want, out)
			}
		}
	})

	if data, err := os.ReadFile(filepath.Join(myCity, "city.toml")); err == nil {
		ws.noteDiagnostic("final city.toml:\n%s", string(data))
	}
}
