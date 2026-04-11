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
name = "helper"
provider = "claude"
prompt_template = "prompts/worker.md"

[[agent]]
name = "worker"
provider = "claude"
prompt_template = "prompts/worker.md"

[[agent]]
name = "reviewer"
provider = "`+tutorialReviewerProvider()+`"
prompt_template = "prompts/reviewer.md"

[[agent]]
name = "reviewer"
dir = "my-project"
provider = "`+tutorialReviewerProvider()+`"
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

	var reviewerSessionID string
	var reviewerTarget string
	var mayorPeekOut string
	var mayorTailLogs string

	ws.noteWarning("tutorial 03 continuity workaround: tutorial 02 does not guarantee a live reviewer session still exists when tutorial 03 begins, so the page driver seeds one explicitly before `gc session peek reviewer`")
	if out, err := ws.runShell("gc session new reviewer --title reviewer --no-attach", ""); err != nil {
		t.Fatalf("seed reviewer session creation: %v\n%s", err, out)
	} else {
		reviewerSessionID = firstBeadID(out)
		if reviewerSessionID == "" {
			t.Fatalf("seed reviewer session creation did not return a session bead id:\n%s", out)
		}
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, func() bool {
		target, err := ws.sessionTargetByID(reviewerSessionID, "reviewer")
		if err != nil || target == "" {
			return false
		}
		reviewerTarget = target
		return true
	}) {
		listOut, _ := ws.runShell("gc session list --template reviewer", "")
		t.Fatalf("reviewer session target did not materialize for %s:\n%s", reviewerSessionID, listOut)
	}
	ws.noteWarning("tutorial 03 prose workaround: the published `gc session peek reviewer` target is not a stable session handle, so the page driver resolves the spawned reviewer session target `%s` first", reviewerTarget)
	ws.noteWarning("tutorial 03 runtime workaround: the hidden reviewer seed is created with `--no-attach`, so the page driver waits for `%s` to become peekable before the visible `gc session peek reviewer` step", reviewerTarget)
	if !waitForCondition(t, 60*time.Second, 2*time.Second, func() bool {
		out, err := ws.runShell("gc session peek "+reviewerTarget, "")
		return err == nil && strings.TrimSpace(out) != ""
	}) {
		listOut, _ := ws.runShell("gc session list --template reviewer", "")
		t.Fatalf("reviewer session %s never became peekable:\n%s", reviewerTarget, listOut)
	}
	ws.noteWarning("tutorial 03 runtime workaround: the mayor session can materialize before the runtime/transcript are ready, so the page driver waits for `peek` and `logs` readiness before the visible steps")
	if !waitForCondition(t, 60*time.Second, 2*time.Second, func() bool {
		out, err := ws.runShell("gc session peek mayor --lines 3", "")
		if err != nil || strings.TrimSpace(out) == "" {
			return false
		}
		mayorPeekOut = out
		return true
	}) {
		out, _ := ws.runShell("gc session list", "")
		t.Fatalf("mayor session never became peekable:\n%s", out)
	}
	ws.noteWarning("tutorial 03 continuity workaround: the page later renders helper and hal sessions without establishing them, so the page driver seeds both hidden helper sessions before the second session-list example")
	for _, cmd := range []string{
		"gc session new helper --title helper --no-attach",
		"gc session new helper --alias hal --title hal --no-attach",
	} {
		if out, err := ws.runShell(cmd, ""); err != nil {
			t.Fatalf("seed helper/hal session creation %q: %v\n%s", cmd, err, out)
		}
	}

	t.Run("cat city.toml", func(t *testing.T) {
		out, err := ws.runShell("cat city.toml", "")
		if err != nil {
			t.Fatalf("cat city.toml: %v\n%s", err, out)
		}
		for _, want := range []string{
			`name = "my-city"`,
			`name = "reviewer"`,
			`provider = "` + tutorialReviewerProvider() + `"`,
			`name = "my-project"`,
			`name = "my-api"`,
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("city.toml missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc session peek reviewer", func(t *testing.T) {
		out, err := ws.runShell("gc session peek "+reviewerTarget, "")
		if err != nil {
			t.Fatalf("gc session peek %s: %v\n%s", reviewerTarget, err, out)
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
		var out string
		ok := waitForCondition(t, 30*time.Second, 2*time.Second, func() bool {
			var err error
			out, err = ws.runShell("gc session list", "")
			if err != nil {
				return false
			}
			return strings.Contains(out, "mayor") &&
				strings.Contains(out, "helper") &&
				strings.Contains(out, "hal")
		})
		if !ok {
			t.Fatalf("session list after nudge should surface mayor/helper/hal:\n%s", out)
		}
	})

	t.Run("gc session logs mayor --tail 1", func(t *testing.T) {
		if !waitForCondition(t, 5*time.Minute, 2*time.Second, func() bool {
			out, err := ws.runShell("gc session logs mayor --tail 1", "")
			if err != nil || strings.TrimSpace(out) == "" {
				return false
			}
			mayorTailLogs = out
			return true
		}) {
			out, _ := ws.runShell("gc session list", "")
			t.Fatalf("mayor transcript never became readable:\n%s", out)
		}
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
	if strings.TrimSpace(mayorPeekOut) != "" {
		ws.noteDiagnostic("seed mayor peek readiness output:\n%s", mayorPeekOut)
	}
	if strings.TrimSpace(mayorTailLogs) != "" {
		ws.noteDiagnostic("seed mayor tail-log readiness output:\n%s", mayorTailLogs)
	}
	if mayorLogs, err := ws.runShell("gc session logs mayor --tail 5", ""); err == nil {
		ws.noteDiagnostic("final mayor logs:\n%s", mayorLogs)
	}
	if reviewerTarget != "" {
		if reviewerPeek, err := ws.runShell("gc session peek "+reviewerTarget, ""); err == nil {
			ws.noteDiagnostic("final reviewer peek:\n%s", reviewerPeek)
		}
	}
}
