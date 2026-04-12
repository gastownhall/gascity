//go:build acceptance_c

package tutorialgoldens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTutorial04Communication(t *testing.T) {
	ws := newTutorialWorkspace(t)
	ws.attachDiagnostics(t, "tutorial-04")

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
	ws.noteWarning("TODO(issue #632): once bare agent names reliably resolve to the enclosing rig in acceptance-style paths, simplify tutorial 04's rig-local reviewer references from `my-project/reviewer` to bare `reviewer` where the shell is already in the rig")

	mayorSessionID, err := ws.waitForSessionByTemplateOrTarget("mayor", "mayor", 30*time.Second, time.Second)
	if err != nil {
		t.Fatalf("resolve mayor session bead: %v", err)
	}

	mayorReady := func() bool {
		peekOut, peekErr := ws.runShell("gc session peek mayor --lines 1", "")
		return peekErr == nil && strings.TrimSpace(peekOut) != ""
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, mayorReady) {
		ws.noteWarning("tutorial 04 runtime workaround: gc init seeds a named mayor session bead with resume metadata, so the page driver clears the stale resume key before bootstrapping a fresh headless submit")
		if out, err := ws.runShell("bd update "+mayorSessionID+" --unset-metadata session_key --unset-metadata started_config_hash --set-metadata continuation_reset_pending=true", ""); err != nil {
			t.Fatalf("clear mayor stale resume metadata: %v\n%s", err, out)
		}
		if out, err := ws.runShell(`gc session submit mayor "__tutorial04_bootstrap__"`, ""); err != nil {
			t.Fatalf("seed mayor submit bootstrap: %v\n%s", err, out)
		}
	}
	if !waitForCondition(t, 30*time.Second, 1*time.Second, mayorReady) {
		out, _ := ws.runShell("gc session list", "")
		t.Fatalf("mayor session did not become peekable during tutorial 04 seed bootstrap:\n%s", out)
	}

	t.Run(`gc mail send mayor -s "Review needed" -m "Please look at the auth module changes in my-project"`, func(t *testing.T) {
		out, err := ws.runShell(`gc mail send mayor -s "Review needed" -m "Please look at the auth module changes in my-project"`, "")
		if err != nil {
			t.Fatalf("gc mail send mayor: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Sent message") {
			t.Fatalf("mail send output mismatch:\n%s", out)
		}
	})

	t.Run("gc mail check mayor", func(t *testing.T) {
		out, err := ws.runShell("gc mail check mayor", "")
		if err != nil {
			t.Fatalf("gc mail check mayor: %v\n%s", err, out)
		}
		if !strings.Contains(strings.ToLower(out), "unread") {
			t.Fatalf("mail check output mismatch:\n%s", out)
		}
	})

	t.Run("gc mail inbox mayor", func(t *testing.T) {
		out, err := ws.runShell("gc mail inbox mayor", "")
		if err != nil {
			t.Fatalf("gc mail inbox mayor: %v\n%s", err, out)
		}
		for _, want := range []string{"Review needed", "auth module changes in my-project"} {
			if !strings.Contains(out, want) {
				t.Fatalf("mail inbox missing %q:\n%s", want, out)
			}
		}
	})

	t.Run(`gc session nudge mayor "Check mail and hook status, then act accordingly"`, func(t *testing.T) {
		out, err := ws.runShell(`gc session nudge mayor "Check mail and hook status, then act accordingly"`, "")
		if err != nil {
			t.Fatalf("gc session nudge mayor: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Nudged mayor") && !strings.Contains(out, "Queued nudge for mayor") {
			t.Fatalf("nudge output mismatch:\n%s", out)
		}
	})

	t.Run("gc session peek mayor --lines 6", func(t *testing.T) {
		var out string
		ok := waitForCondition(t, 45*time.Second, 2*time.Second, func() bool {
			var err error
			out, err = ws.runShell("gc session peek mayor --lines 6", "")
			if err != nil || strings.TrimSpace(out) == "" {
				return false
			}
			return strings.Contains(out, "Review needed") ||
				strings.Contains(out, "auth module changes in my-project") ||
				strings.Contains(out, "reviewer")
		})
		if !ok {
			t.Fatalf("peek mayor did not surface communication flow in time:\n%s", out)
		}
	})

	if mayorPeek, err := ws.runShell("gc session peek mayor --lines 12", ""); err == nil {
		ws.noteDiagnostic("final mayor peek:\n%s", mayorPeek)
	}
	if mayorLogs, err := ws.runShell("gc session logs mayor --tail 5", ""); err == nil {
		ws.noteDiagnostic("final mayor logs:\n%s", mayorLogs)
	}
	if data, err := os.ReadFile(filepath.Join(myCity, "city.toml")); err == nil {
		ws.noteDiagnostic("final city.toml:\n%s", string(data))
	}
}
