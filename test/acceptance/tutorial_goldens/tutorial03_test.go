//go:build acceptance_c

package tutorialgoldens

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// See TODO.md in this directory for tutorial/workaround cleanup that should
// be burned down before the prose and tests are merged.
func TestTutorial03Sessions(t *testing.T) {
	ws := newTutorialWorkspace(t)
	ws.attachDiagnostics(t, "tutorial-03")

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

	appendFile(t, filepath.Join(myCity, "city.toml"), `

[[agent]]
name = "reviewer"
provider = "codex"
prompt_template = "prompts/reviewer.md"
`)
	writeFile(t, filepath.Join(myCity, "prompts", "reviewer.md"), "# Reviewer\nReview code.\n", 0o644)

	mayorReady := func() bool {
		listOut, listErr := ws.runShell("gc session list", "")
		return listErr == nil && strings.Contains(listOut, "mayor")
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, mayorReady) {
		statusOut, statusErr := ws.runShell("gc status", "")
		if statusErr == nil && !strings.Contains(statusOut, "Controller: stopped") {
			restartOut, restartErr := ws.runShell("gc restart", "")
			if restartErr != nil {
				t.Fatalf("seed city restart: %v\n%s", restartErr, restartOut)
			}
		} else {
			startOut, startErr := ws.runShell("gc start ~/my-city", "")
			if startErr != nil {
				t.Fatalf("seed city start: %v\n%s", startErr, startOut)
			}
		}
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, mayorReady) {
		listOut, _ := ws.runShell("gc session list", "")
		t.Fatalf("mayor session did not materialize during tutorial 03 seed bootstrap:\n%s", listOut)
	}

	ws.noteWarning("tutorial 03 continuity workaround: tutorial 02 does not guarantee a live reviewer session still exists when tutorial 03 begins, so the page driver seeds one explicitly before `gc session peek reviewer`")
	if out, err := ws.runShell("gc session new reviewer --title reviewer --no-attach", ""); err != nil {
		t.Fatalf("seed reviewer session creation: %v\n%s", err, out)
	}

	t.Run("cat city.toml", func(t *testing.T) {
		out, err := ws.runShell("cat city.toml", "")
		if err != nil {
			t.Fatalf("cat city.toml: %v\n%s", err, out)
		}
		for _, want := range []string{
			`name = "my-city"`,
			`name = "reviewer"`,
			`provider = "codex"`,
			`name = "my-project"`,
			`name = "my-api"`,
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("city.toml missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc session peek reviewer", func(t *testing.T) {
		out, err := ws.runShell("gc session peek reviewer", "")
		if err != nil {
			t.Fatalf("gc session peek reviewer: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" || !strings.Contains(strings.ToLower(out), "reviewer") {
			t.Fatalf("peek reviewer output mismatch:\n%s", out)
		}
	})

	t.Run("gc session list", func(t *testing.T) {
		out, err := ws.runShell("gc session list", "")
		if err != nil {
			t.Fatalf("gc session list: %v\n%s", err, out)
		}
		for _, want := range []string{"ID", "TEMPLATE", "mayor", "reviewer"} {
			if !strings.Contains(out, want) {
				t.Fatalf("session list missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc session peek mayor --lines 3", func(t *testing.T) {
		out, err := ws.runShell("gc session peek mayor --lines 3", "")
		if err != nil {
			t.Fatalf("gc session peek mayor --lines 3: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("peek mayor output is empty")
		}
	})

	t.Run("gc session attach mayor", func(t *testing.T) {
		rs, err := ws.startShell("gc session attach mayor", "")
		if err != nil {
			t.Fatalf("gc session attach mayor: %v", err)
		}
		defer func() { _ = rs.stop() }()
		if err := rs.waitFor("Attaching to session", 30*time.Second); err != nil {
			t.Fatalf("attach did not reach tmux handoff: %v", err)
		}
	})

	t.Run(`gc session nudge mayor "What's the current city status?"`, func(t *testing.T) {
		out, err := ws.runShell(`gc session nudge mayor "What's the current city status?"`, "")
		if err != nil {
			t.Fatalf("gc session nudge mayor: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Nudged mayor") && !strings.Contains(out, "Queued nudge for mayor") {
			t.Fatalf("nudge output mismatch:\n%s", out)
		}
	})

	t.Run("gc session list (after nudge)", func(t *testing.T) {
		out, err := ws.runShell("gc session list", "")
		if err != nil {
			t.Fatalf("gc session list after nudge: %v\n%s", err, out)
		}
		if !strings.Contains(out, "mayor") {
			t.Fatalf("session list after nudge missing mayor:\n%s", out)
		}
	})

	t.Run("gc session logs mayor --tail 1", func(t *testing.T) {
		out, err := ws.runShell("gc session logs mayor --tail 1", "")
		if err != nil {
			t.Fatalf("gc session logs mayor --tail 1: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("session logs --tail 1 output is empty")
		}
	})

	t.Run("gc session logs mayor -f", func(t *testing.T) {
		rs, err := ws.startShell("gc session logs mayor -f", "")
		if err != nil {
			t.Fatalf("gc session logs mayor -f: %v", err)
		}
		defer func() { _ = rs.stop() }()

		if _, err := ws.runShell(`gc session nudge mayor "__tutorial03_logs_follow_probe__"`, ""); err != nil {
			t.Fatalf("hidden follow stimulus failed: %v", err)
		}
		if err := rs.waitFor("__tutorial03_logs_follow_probe__", 45*time.Second); err != nil {
			t.Fatalf("session logs follow did not surface new output: %v", err)
		}
	})

	if listOut, err := ws.runShell("gc session list", ""); err == nil {
		ws.noteDiagnostic("final session list:\n%s", listOut)
	}
	if mayorLogs, err := ws.runShell("gc session logs mayor --tail 5", ""); err == nil {
		ws.noteDiagnostic("final mayor logs:\n%s", mayorLogs)
	}
	if reviewerPeek, err := ws.runShell("gc session peek reviewer", ""); err == nil {
		ws.noteDiagnostic("final reviewer peek:\n%s", reviewerPeek)
	}
}
