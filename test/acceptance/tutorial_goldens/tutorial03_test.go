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

	if out, err := ws.runShell("gc agent add --name reviewer --dir my-project", ""); err != nil {
		t.Fatalf("seed reviewer scaffold: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(myCity, "agents", "reviewer", "agent.toml"), "dir = \"my-project\"\nprovider = \""+tutorialReviewerProvider()+"\"\n", 0o644)
	writeFile(t, filepath.Join(myCity, "agents", "reviewer", "prompt.template.md"), "# Reviewer\nReview code.\n", 0o644)
	writeFile(t, filepath.Join(myProject, "hello.py"), "print(\"Hello, World!\")\n", 0o644)
	ws.noteWarning("TODO(issue #632): once bare agent/template names reliably resolve to the enclosing rig in acceptance-style paths, simplify tutorial 03 back to bare `reviewer` references from inside ~/my-project")

	var reviewerKeepalive *runningShell
	t.Cleanup(func() {
		if reviewerKeepalive != nil {
			_ = reviewerKeepalive.stop()
		}
	})

	if _, err := ws.waitForSessionByTemplateOrTarget("mayor", "mayor", 30*time.Second, time.Second); err != nil {
		t.Fatalf("resolve mayor session bead: %v", err)
	}
	restartCity := func(context string) {
		if reviewerKeepalive != nil {
			_ = reviewerKeepalive.stop()
			reviewerKeepalive = nil
		}
		ws.noteWarning("tutorial 03 runtime workaround: %s, so the page driver performs a hidden gc stop/gc start cycle and waits for the restarted mayor session to come back", context)
		if out, err := ws.runShell("gc stop", ""); err != nil {
			t.Fatalf("hidden gc stop during tutorial 03 recovery: %v\n%s", err, out)
		}
		if out, err := ws.runShell("gc start", ""); err != nil {
			t.Fatalf("hidden gc start during tutorial 03 recovery: %v\n%s", err, out)
		}
	}

	mayorReady := func() bool {
		peekOut, peekErr := ws.runShell("gc session peek mayor --lines 1", "")
		return peekErr == nil && strings.TrimSpace(peekOut) != ""
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, mayorReady) {
		ws.noteWarning("tutorial 03 runtime workaround: gc init can leave the named mayor session still restarting before the transcript is ready, so the page driver explicitly wakes it and queues a normal city-status request to materialize the transcript")
		if out, err := ws.runShell("gc session wake mayor", ""); err != nil {
			t.Fatalf("wake mayor during seed bootstrap: %v\n%s", err, out)
		}
		if out, err := ws.runShell(`gc session nudge mayor "What's the current city status?"`, ""); err != nil {
			t.Fatalf("seed mayor nudge bootstrap: %v\n%s", err, out)
		}
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, mayorReady) {
		restartCity("gc init left the mayor session unpeekable during seed bootstrap")
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, mayorReady) {
		out, _ := ws.runShell("gc session list", "")
		t.Fatalf("mayor session did not become peekable during tutorial 03 seed bootstrap:\n%s", out)
	}

	var reviewerSessionID string
	var mayorPeekOut string
	var mayorTailLogs string
	var mayorLogsFollow *runningShell
	const cityStatusPrompt = "What's the current city status?"
	t.Cleanup(func() {
		if mayorLogsFollow != nil {
			_ = mayorLogsFollow.stop()
		}
	})

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
	mayorPeekReady := func() bool {
		out, err := ws.runShell("gc session peek mayor --lines 3", "")
		if err != nil || strings.TrimSpace(out) == "" {
			return false
		}
		mayorPeekOut = out
		return true
	}
	if !waitForCondition(t, 60*time.Second, 2*time.Second, mayorPeekReady) {
		ws.noteWarning("tutorial 03 runtime workaround: reviewer setup can leave the named mayor session mid-restart during config reload, so the page driver re-wakes it with a normal city-status request before the visible mayor transcript steps")
		if out, err := ws.runShell("gc session wake mayor", ""); err != nil {
			t.Fatalf("wake mayor after reviewer seed: %v\n%s", err, out)
		}
		if out, err := ws.runShell(`gc session nudge mayor "`+cityStatusPrompt+`"`, ""); err != nil {
			t.Fatalf("re-wake mayor after reviewer seed: %v\n%s", err, out)
		}
	}
	if !waitForCondition(t, 60*time.Second, 2*time.Second, mayorPeekReady) {
		restartCity("reviewer setup left the mayor session unpeekable")
	}
	if !waitForCondition(t, 60*time.Second, 2*time.Second, mayorPeekReady) {
		out, _ := ws.runShell("gc session list", "")
		t.Fatalf("mayor session never became peekable:\n%s", out)
	}

	t.Run("cat pack.toml", func(t *testing.T) {
		out, err := ws.runShell("cat pack.toml", "")
		if err != nil {
			t.Fatalf("cat pack.toml: %v\n%s", err, out)
		}
		for _, want := range []string{
			`name = "my-city"`,
			`schema = 2`,
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("pack.toml missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("cat city.toml", func(t *testing.T) {
		out, err := ws.runShell("cat city.toml", "")
		if err != nil {
			t.Fatalf("cat city.toml: %v\n%s", err, out)
		}
		for _, want := range []string{
			`provider = "claude"`,
			`name = "my-project"`,
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("city.toml missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, myProject) {
			t.Fatalf("city.toml should not contain machine-local rig path %q:\n%s", myProject, out)
		}
	})

	t.Run("cat agents/reviewer/agent.toml", func(t *testing.T) {
		out, err := ws.runShell("cat agents/reviewer/agent.toml", "")
		if err != nil {
			t.Fatalf("cat agents/reviewer/agent.toml: %v\n%s", err, out)
		}
		for _, want := range []string{
			`dir = "my-project"`,
			`provider = "` + tutorialReviewerProvider() + `"`,
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("agents/reviewer/agent.toml missing %q:\n%s", want, out)
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

	t.Run(`gc session nudge mayor "`+cityStatusPrompt+`"`, func(t *testing.T) {
		out, err := ws.runShell(`gc session nudge mayor "`+cityStatusPrompt+`"`, "")
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

	ws.noteWarning("tutorial 03 runtime workaround: named mayor sessions can take a restart cycle before their alias-based transcript path becomes readable, so the page driver first waits for the visible log command as-is and only then nudges the runtime through supported session commands")
	mayorLogsReadable := func() bool {
		out, err := ws.runShell("gc session logs mayor --tail 1", "")
		if err != nil || strings.TrimSpace(out) == "" {
			return false
		}
		mayorTailLogs = out
		return true
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, mayorLogsReadable) {
		ws.noteWarning("tutorial 03 runtime workaround: alias-based log lookup can lag behind the live mayor session, so the page driver explicitly wakes mayor and requeues the city-status prompt before retrying visible log commands")
		if out, err := ws.runShell("gc session wake mayor", ""); err != nil {
			t.Fatalf("wake mayor before log lookup retry: %v\n%s", err, out)
		}
		if out, err := ws.runShell(`gc session nudge mayor "`+cityStatusPrompt+`"`, ""); err != nil {
			t.Fatalf("re-nudge mayor before log lookup retry: %v\n%s", err, out)
		}
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, mayorLogsReadable) {
		restartCity("alias-based mayor transcript lookup stayed unreadable after wake")
		if out, err := ws.runShell(`gc session nudge mayor "`+cityStatusPrompt+`"`, ""); err != nil {
			t.Fatalf("re-nudge mayor after hidden restart: %v\n%s", err, out)
		}
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, mayorLogsReadable) {
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
		mayorLogsFollow = rs
	})

	t.Run(`gc session nudge mayor "`+cityStatusPrompt+`" after starting log follow`, func(t *testing.T) {
		if mayorLogsFollow == nil {
			t.Fatal("logs follow shell was not started")
		}
		baseline := mayorLogsFollow.output()

		out, err := ws.runShell(`gc session nudge mayor "`+cityStatusPrompt+`"`, "")
		if err != nil {
			t.Fatalf("gc session nudge mayor log-follow request: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Nudged mayor") && !strings.Contains(out, "Queued nudge for mayor") {
			t.Fatalf("log-follow nudge output mismatch:\n%s", out)
		}

		if !waitForCondition(t, 90*time.Second, 500*time.Millisecond, func() bool {
			out := mayorLogsFollow.output()
			if !strings.HasPrefix(out, baseline) {
				return len(out) > len(baseline) && strings.Contains(out, "[ASSISTANT]")
			}
			delta := out[len(baseline):]
			return strings.Contains(delta, "[ASSISTANT]")
		}) {
			t.Fatalf("session logs follow did not surface fresh mayor output after nudge:\n%s", mayorLogsFollow.output())
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
