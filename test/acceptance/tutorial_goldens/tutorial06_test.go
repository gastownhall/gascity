//go:build acceptance_c

package tutorialgoldens

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
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
		fmt.Sprintf("name = %q\npath = %q\n", "my-api", myAPI),
		fmt.Sprintf("name = %q\npath = %q\n\n[rigs.imports.dev_ops]\nsource = \"./packs/dev-ops\"\n", "my-api", myAPI),
	)

	writeFile(t, filepath.Join(myCity, "formulas", "review.toml"), `formula = "review"

[[steps]]
id = "check"
title = "Check open PRs that need review"
`, 0o644)
	writeFile(t, filepath.Join(myCity, "formulas", "release-notes.toml"), `formula = "release-notes"

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

	t.Run("gc start", func(t *testing.T) {
		ws.noteWarning("tutorial 07 workaround: gc init currently leaves a standalone controller running, so the page driver stops that controller immediately before the visible gc start step")
		if statusOut, statusErr := ws.runShell("gc status", ""); statusErr == nil && !strings.Contains(statusOut, "Controller: stopped") {
			if stopOut, stopErr := ws.runShell("gc stop", ""); stopErr != nil {
				t.Fatalf("hidden gc stop before visible gc start: %v\n%s", stopErr, stopOut)
			}
		}
		out, err := ws.runShell("gc start", "")
		if err != nil {
			t.Fatalf("gc start: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("gc start output is empty")
		}
	})

	t.Run("gc order list (after start)", func(t *testing.T) {
		out, err := ws.runShell("gc order list", "")
		if err != nil {
			t.Fatalf("gc order list after start: %v\n%s", err, out)
		}
		if !strings.Contains(out, "review-check") {
			t.Fatalf("order list after start missing review-check:\n%s", out)
		}
	})

	t.Run("gc order check (after start)", func(t *testing.T) {
		out, err := ws.runShell("gc order check", "")
		if err != nil && !strings.Contains(out, "NAME") {
			t.Fatalf("gc order check after start: %v\n%s", err, out)
		}
		if !strings.Contains(out, "review-check") {
			t.Fatalf("order check after start should mention review-check:\n%s", out)
		}
	})
}
