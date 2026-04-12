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
	mustMkdirAll(t, myProject)

	out, err := ws.runShell("gc init ~/my-city --provider claude --skip-provider-readiness", "")
	if err != nil {
		t.Fatalf("seed city init: %v\n%s", err, out)
	}
	ws.setCWD(myCity)

	for _, cmd := range []string{"gc rig add ~/my-project"} {
		if out, err := ws.runShell(cmd, ""); err != nil {
			t.Fatalf("seed rig add %q: %v\n%s", cmd, err, out)
		}
	}

	appendFile(t, filepath.Join(myCity, "city.toml"), `

[[agent]]
name = "reviewer"
dir = "my-project"
provider = "`+tutorialReviewerProvider()+`"
prompt_template = "prompts/reviewer.md"
`)
	writeFile(t, filepath.Join(myCity, "prompts", "reviewer.md"), "# Reviewer\nReview code.\n", 0o644)
	writeFile(t, filepath.Join(myProject, "hello.py"), "print(\"Hello, World!\")\n", 0o644)
	ws.noteWarning("TODO(issue #632): once bare agent/template names reliably resolve to the enclosing rig in acceptance-style paths, simplify tutorial 03 back to bare `reviewer` references from inside ~/my-project")

	var reviewerKeepalive *runningShell
	t.Cleanup(func() {
		if reviewerKeepalive != nil {
			_ = reviewerKeepalive.stop()
		}
	})

	if err := ws.waitForPeekableSession("mayor", "mayor", 30*time.Second, time.Second); err != nil {
		t.Fatalf("mayor should be an always-on named session immediately after init: %v", err)
	}

	var reviewerSessionID string
	var mayorPeekOut string
	var mayorPeekBaseline string
	var mayorTailLogs string
	const logsFollowProbe = "__tutorial03_logs_follow_probe__"

	ws.noteWarning("tutorial 03 starts from the live reviewer polecat created in tutorial 02, so the page driver seeds that prior session state by slinging the same review work before exercising the visible session lookup flow")
	if out, err := ws.runShell(`gc sling my-project/reviewer "Review hello.py and write review.md with feedback"`, ""); err != nil {
		t.Fatalf("seed reviewer work sling: %v\n%s", err, out)
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, func() bool {
		sessionID, _, err := ws.firstSessionByTemplate("my-project/reviewer")
		if err != nil || sessionID == "" {
			return false
		}
		reviewerSessionID = sessionID
		return true
	}) {
		listOut, _ := ws.runShell("gc session list --template my-project/reviewer", "")
		t.Fatalf("reviewer session target did not materialize for %s:\n%s", reviewerSessionID, listOut)
	}
	ws.noteWarning("tutorial 03 uses an example bead id in `gc session peek mc-8sfd`, so the page driver resolves the seeded reviewer session to its live session bead `%s` before running the equivalent command", reviewerSessionID)
	if !waitForCondition(t, 60*time.Second, 2*time.Second, func() bool {
		out, err := ws.runShell("gc session peek "+reviewerSessionID, "")
		return err == nil && strings.TrimSpace(out) != ""
	}) {
		rs, err := ws.startShell("gc session attach "+reviewerSessionID, "")
		if err != nil {
			t.Fatalf("seed reviewer attach bootstrap: %v", err)
		}
		if err := rs.waitFor("Attaching to session", 30*time.Second); err != nil {
			_ = rs.stop()
			t.Fatalf("seed reviewer attach bootstrap did not reach tmux handoff: %v", err)
		}
		reviewerKeepalive = rs
	}
	if !waitForCondition(t, 60*time.Second, 2*time.Second, func() bool {
		out, err := ws.runShell("gc session peek "+reviewerSessionID, "")
		return err == nil && strings.TrimSpace(out) != ""
	}) {
		listOut, _ := ws.runShell("gc session list --template my-project/reviewer", "")
		t.Fatalf("reviewer session %s never became peekable:\n%s", reviewerSessionID, listOut)
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
	if out, err := ws.runShell("gc session peek mayor --lines 12", ""); err == nil && strings.TrimSpace(out) != "" {
		mayorPeekBaseline = out
	}

	t.Run("cat city.toml", func(t *testing.T) {
		out, err := ws.runShell("cat city.toml", "")
		if err != nil {
			t.Fatalf("cat city.toml: %v\n%s", err, out)
		}
		for _, want := range []string{
			`name = "my-city"`,
			`name = "reviewer"`,
			`dir = "my-project"`,
			`provider = "` + tutorialReviewerProvider() + `"`,
			`name = "my-project"`,
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("city.toml missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc session list --template my-project/reviewer", func(t *testing.T) {
		out, err := ws.runShell("gc session list --template my-project/reviewer", "")
		if err != nil {
			t.Fatalf("gc session list --template my-project/reviewer: %v\n%s", err, out)
		}
		for _, want := range []string{"ID", "TEMPLATE", "my-project/reviewer", reviewerSessionID} {
			if !strings.Contains(out, want) {
				t.Fatalf("session list --template my-project/reviewer missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc session peek mc-8sfd", func(t *testing.T) {
		out, err := ws.runShell("gc session peek "+reviewerSessionID, "")
		if err != nil {
			t.Fatalf("gc session peek %s: %v\n%s", reviewerSessionID, err, out)
		}
		lower := strings.ToLower(out)
		if strings.TrimSpace(out) == "" || (!strings.Contains(lower, "reviewer") && !strings.Contains(lower, "codex")) {
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
			return strings.Contains(out, "mayor")
		})
		if !ok {
			t.Fatalf("session list after nudge should surface mayor:\n%s", out)
		}
	})

	ws.noteWarning("tutorial 03 runtime workaround: after the visible mayor nudge, wait for `peek` to return updated, non-empty output before exercising transcript commands; the session can be active while Claude is still on its welcome screen")
	if !waitForCondition(t, 90*time.Second, 2*time.Second, func() bool {
		out, err := ws.runShell("gc session peek mayor --lines 12", "")
		if err != nil || strings.TrimSpace(out) == "" {
			return false
		}
		mayorPeekOut = out
		return strings.TrimSpace(out) != strings.TrimSpace(mayorPeekBaseline)
	}) {
		out, _ := ws.runShell("gc session peek mayor --lines 12", "")
		t.Fatalf("mayor did not return updated peek output after the visible nudge before the log steps:\n%s", out)
	}

	ws.noteWarning("tutorial 03 runtime workaround: after the visible mayor nudge, wait for the alias-based `gc session logs mayor` path itself to become readable before exercising the documented log commands")
	if !waitForCondition(t, 2*time.Minute, 2*time.Second, func() bool {
		out, err := ws.runShell("gc session logs mayor --tail 1", "")
		if err != nil || strings.TrimSpace(out) == "" {
			return false
		}
		mayorTailLogs = out
		return true
	}) {
		out, _ := ws.runShell("gc session list", "")
		t.Fatalf("mayor transcript never became readable through alias mayor:\n%s", out)
	}

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

		if _, err := ws.runShell(`gc session nudge mayor "Reply with `+logsFollowProbe+` and nothing else."`, ""); err != nil {
			t.Fatalf("hidden follow stimulus failed: %v", err)
		}
		if err := rs.waitFor(logsFollowProbe, 90*time.Second); err != nil {
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
	if reviewerSessionID != "" {
		if reviewerPeek, err := ws.runShell("gc session peek "+reviewerSessionID, ""); err == nil {
			ws.noteDiagnostic("final reviewer peek:\n%s", reviewerPeek)
		}
	}
}
