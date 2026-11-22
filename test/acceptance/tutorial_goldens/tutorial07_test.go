//go:build acceptance_c

package tutorialgoldens

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTutorial07Orders(t *testing.T) {
	ws := newTutorialWorkspace(t)
	ws.attachDiagnostics(t, "tutorial-07")

	myCity := expandHome(ws.home(), "~/my-city")
	myAPI := expandHome(ws.home(), "~/my-api")
	mustMkdirAll(t, myAPI)

	out, err := ws.runShell("gc init ~/my-city --provider claude --skip-provider-readiness", "")
	if err != nil {
		t.Fatalf("seed city init: %v\n%s", err, out)
	}
	ws.setCWD(myCity)

	if out, err := ws.runShell("gc rig add ~/my-api", ""); err != nil {
		t.Fatalf("seed my-api rig add: %v\n%s", err, out)
	}

	cityToml := filepath.Join(myCity, "city.toml")
	replaceInFile(
		t,
		cityToml,
		fmt.Sprintf("name = %q\n", "my-api"),
		fmt.Sprintf("name = %q\n\n[rigs.imports.dev_ops]\nsource = \"./packs/dev-ops\"\n", "my-api"),
	)

	writeFile(t, filepath.Join(myCity, "formulas", "review.toml"), `formula = "review"

[[steps]]
id = "check"
title = "Check open PRs that need review"
`, 0o644)
	writeFile(t, filepath.Join(myCity, "formulas", "release-notes.toml"), `formula = "release-notes"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "gather"
title = "Gather merged PRs from the last week"

[[steps]]
id = "summarize"
title = "Write release notes"
needs = ["gather"]

[[steps]]
id = "post"
title = "Post release notes to the team channel"
needs = ["summarize"]
`, 0o644)
	writeFile(t, filepath.Join(myCity, "packs", "dev-ops", "pack.toml"), `[pack]
name = "dev-ops"
schema = 2
`, 0o644)
	writeFile(t, filepath.Join(myCity, "packs", "dev-ops", "formulas", "test-suite.toml"), `formula = "test-suite"

[[steps]]
id = "run"
title = "Run the test suite"
`, 0o644)

	reviewOrder := `[order]
description = "Check for PRs that need review"
formula = "review"
trigger = "cooldown"
interval = "5m"
pool = "worker"
`
	depUpdateOrder := `[order]
description = "Check dependency updates"
formula = "review"
trigger = "cooldown"
interval = "1h"
pool = "worker"
`
	releaseNotesOrder := `[order]
description = "Generate release notes"
formula = "release-notes"
trigger = "cooldown"
interval = "24h"
pool = "worker"
`
	testSuiteOrder := `[order]
description = "Run the test suite"
formula = "test-suite"
trigger = "cooldown"
interval = "5m"
pool = "worker"
`

	writeFile(t, filepath.Join(myCity, "orders", "review-check.toml"), reviewOrder, 0o644)
	writeFile(t, filepath.Join(myCity, "orders", "dep-update.toml"), depUpdateOrder, 0o644)
	writeFile(t, filepath.Join(myCity, "orders", "release-notes.toml"), releaseNotesOrder, 0o644)
	writeFile(t, filepath.Join(myCity, "packs", "dev-ops", "orders", "test-suite.toml"), testSuiteOrder, 0o644)

	t.Run("gc order list", func(t *testing.T) {
		out, err := ws.runShell("gc order list", "")
		if err != nil {
			t.Fatalf("gc order list: %v\n%s", err, out)
		}
		for _, want := range []string{"review-check", "dep-update", "release-notes"} {
			if !strings.Contains(out, want) {
				t.Fatalf("order list missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc order show review-check", func(t *testing.T) {
		out, err := ws.runShell("gc order show review-check", "")
		if err != nil {
			t.Fatalf("gc order show review-check: %v\n%s", err, out)
		}
		for _, want := range []string{"review-check", "Formula:", "review"} {
			if !strings.Contains(out, want) {
				t.Fatalf("order show review-check missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc order check", func(t *testing.T) {
		out, err := ws.runShell("gc order check", "")
		if err != nil && !strings.Contains(out, "NAME") {
			t.Fatalf("gc order check: %v\n%s", err, out)
		}
		if !strings.Contains(out, "review-check") {
			t.Fatalf("order check should mention review-check:\n%s", out)
		}
	})

	t.Run("gc order run review-check", func(t *testing.T) {
		out, err := ws.runShell("gc order run review-check", "")
		if err != nil {
			t.Fatalf("gc order run review-check: %v\n%s", err, out)
		}
		if !strings.Contains(out, `Order "review-check" executed`) {
			t.Fatalf("order run output mismatch:\n%s", out)
		}
	})

	t.Run("gc order history", func(t *testing.T) {
		out, err := ws.runShell("gc order history", "")
		if err != nil {
			t.Fatalf("gc order history: %v\n%s", err, out)
		}
		if !strings.Contains(out, "review-check") {
			t.Fatalf("order history should mention review-check:\n%s", out)
		}
	})

	t.Run("gc order history review-check", func(t *testing.T) {
		out, err := ws.runShell("gc order history review-check", "")
		if err != nil {
			t.Fatalf("gc order history review-check: %v\n%s", err, out)
		}
		if !strings.Contains(out, "review-check") {
			t.Fatalf("filtered order history should mention review-check:\n%s", out)
		}
	})

	t.Run("gc order list (with rig order)", func(t *testing.T) {
		out, err := ws.runShell("gc order list", "")
		if err != nil {
			t.Fatalf("gc order list (with rig order): %v\n%s", err, out)
		}
		if !strings.Contains(out, "test-suite") {
			t.Fatalf("order list should include test-suite:\n%s", out)
		}
		if !strings.Contains(out, "RIG") || !strings.Contains(out, "my-api") {
			t.Fatalf("order list with a rig order should show the RIG column and owning rig:\n%s", out)
		}
	})

	t.Run("gc order show test-suite --rig my-api", func(t *testing.T) {
		out, err := ws.runShell("gc order show test-suite --rig my-api", "")
		if err != nil {
			t.Fatalf("gc order show test-suite --rig my-api: %v\n%s", err, out)
		}
		for _, want := range []string{"test-suite", "Formula:", "Target:"} {
			if !strings.Contains(out, want) {
				t.Fatalf("rig order show missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc order run test-suite --rig my-api", func(t *testing.T) {
		out, err := ws.runShell("gc order run test-suite --rig my-api", "")
		if err != nil {
			t.Fatalf("gc order run test-suite --rig my-api: %v\n%s", err, out)
		}
		if !strings.Contains(out, `Order "test-suite" executed`) {
			t.Fatalf("rig order run output mismatch:\n%s", out)
		}
	})

	t.Run("mkdir -p scripts", func(t *testing.T) {
		out, err := ws.runShell("mkdir -p scripts", "")
		if err != nil {
			t.Fatalf("mkdir -p scripts: %v\n%s", err, out)
		}
	})

	t.Run("cat > scripts/lint-changed.sh << 'EOF'", func(t *testing.T) {
		out, err := ws.runShell(tutorialLintScriptShellCommand(t), "")
		if err != nil {
			t.Fatalf("write scripts/lint-changed.sh: %v\n%s", err, out)
		}
	})

	t.Run("chmod +x scripts/lint-changed.sh", func(t *testing.T) {
		out, err := ws.runShell("chmod +x scripts/lint-changed.sh", "")
		if err != nil {
			t.Fatalf("chmod +x scripts/lint-changed.sh: %v\n%s", err, out)
		}
	})

	// The page has readers create these files at the paths their first lines
	// name; the release-notes order replaces the seeded cooldown version.
	writeFile(t, filepath.Join(myCity, "orders", "lint-check.toml"),
		tutorialFileSnippet(t, "docs/tutorials/07-orders.md", "toml", "# orders/lint-check.toml"), 0o644)
	writeFile(t, filepath.Join(myCity, "orders", "release-notes.toml"),
		tutorialFileSnippet(t, "docs/tutorials/07-orders.md", "toml", "# orders/release-notes.toml"), 0o644)

	t.Run("gc reload", func(t *testing.T) {
		out, err := ws.runShell("gc reload", "")
		if err != nil {
			t.Fatalf("gc reload: %v\n%s", err, out)
		}
	})

	t.Run("gc order list (after reload)", func(t *testing.T) {
		out, err := ws.runShell("gc order list", "")
		if err != nil {
			t.Fatalf("gc order list after reload: %v\n%s", err, out)
		}
		for _, want := range []string{"lint-check", "review-check", "release-notes", "0 9 * * 1"} {
			if !strings.Contains(out, want) {
				t.Fatalf("order list after reload missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc order check (after reload)", func(t *testing.T) {
		out, err := ws.runShell("gc order check", "")
		if err != nil && !strings.Contains(out, "NAME") {
			t.Fatalf("gc order check after reload: %v\n%s", err, out)
		}
		for _, want := range []string{"lint-check", "release-notes"} {
			if !strings.Contains(out, want) {
				t.Fatalf("order check after reload missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc order history lint-check", func(t *testing.T) {
		// The exec order fires on the next tick after the reload, then every
		// 30 seconds, so wait for its first run rather than assert once.
		var out string
		if !waitForCondition(t, 3*time.Minute, 5*time.Second, func() bool {
			var err error
			out, err = ws.runShell("gc order history lint-check", "")
			// Before the first run this prints `No order history for "lint-check".`
			return err == nil && !strings.Contains(out, "No order history") && strings.Contains(out, "lint-check")
		}) {
			t.Fatalf("lint-check never ran within 3m of gc reload:\n%s", out)
		}
	})

	t.Run("gc events --type order.failed --since 5m", func(t *testing.T) {
		out, err := ws.runShell("gc events --type order.failed --since 5m", "")
		if err != nil {
			t.Fatalf("gc events --type order.failed --since 5m: %v\n%s", err, out)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "lint-check") {
				t.Fatalf("the stand-in lint script should never fail its order:\n%s", out)
			}
		}
	})
}
