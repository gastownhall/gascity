//go:build acceptance_c

package tutorialgoldens

import (
	"os"
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
prompt_template = "prompts/worker.md"

[[agent]]
name = "worker"
dir = "my-project"
prompt_template = "prompts/worker.md"
depends_on = ["mayor"]

[[agent]]
name = "reviewer"
dir = "my-project"
provider = "codex"
prompt_template = "prompts/reviewer.md"
`)
	writeFile(t, filepath.Join(myCity, "prompts", "reviewer.md"), "# Reviewer\nReview code.\n", 0o644)

	if listOut, listErr := ws.runShell("gc session list", ""); listErr != nil || !strings.Contains(listOut, "mayor") {
		startOut, startErr := ws.runShell("gc start ~/my-city", "")
		if startErr != nil {
			t.Fatalf("seed city start: %v\n%s", startErr, startOut)
		}
	}

	statusOut, statusErr := ws.runShell("gc status", "")
	if statusErr != nil {
		t.Fatalf("seed city status: %v\n%s", statusErr, statusOut)
	}
	if !strings.Contains(statusOut, "helper") || !strings.Contains(statusOut, "worker") || !strings.Contains(statusOut, "reviewer") {
		ws.noteWarning("tutorial 03 continuity workaround: hidden helper/worker/reviewer config append does not land synchronously in the live controller, so the page driver forces a restart before seeding helper/hal")
		restartOut, restartErr := ws.runShell("gc restart", "")
		if restartErr != nil {
			t.Fatalf("seed city restart after hidden config append: %v\n%s", restartErr, restartOut)
		}
	}

	ws.noteWarning("tutorial 03 continuity workaround: tutorial 02 does not create helper/hal sessions with `gc session new`, so the page driver seeds them explicitly (including aliasing `hal` so later session commands are addressable)")
	ws.noteWarning("tutorial 03 continuity workaround: tutorial 02 does not establish the documented helper / my-project/worker prerequisite state, so the page driver seeds that state explicitly")

	for _, cmd := range []string{
		"gc session new helper --no-attach",
		`gc session new helper --alias hal --title hal --no-attach`,
	} {
		if out, err := ws.runShell(cmd, ""); err != nil {
			t.Fatalf("seed session creation %q: %v\n%s", cmd, err, out)
		}
	}

	t.Run("gc session list", func(t *testing.T) {
		out, err := ws.runShell("gc session list", "")
		if err != nil {
			t.Fatalf("gc session list: %v\n%s", err, out)
		}
		for _, want := range []string{"helper", "hal", "mayor"} {
			if !strings.Contains(out, want) {
				t.Fatalf("session list missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("gc session suspend hal", func(t *testing.T) {
		out, err := ws.runShell("gc session suspend hal", "")
		if err != nil {
			t.Fatalf("gc session suspend hal: %v\n%s", err, out)
		}
		if !strings.Contains(strings.ToLower(out), "suspend") {
			t.Fatalf("suspend output mismatch:\n%s", out)
		}
	})

	t.Run("gc session list (hal suspended)", func(t *testing.T) {
		out, err := ws.runShell("gc session list", "")
		if err != nil {
			t.Fatalf("gc session list after suspend: %v\n%s", err, out)
		}
		if !strings.Contains(out, "hal") || !strings.Contains(strings.ToLower(out), "suspended") {
			t.Fatalf("session list should show hal suspended:\n%s", out)
		}
	})

	t.Run("gc session wake hal", func(t *testing.T) {
		out, err := ws.runShell("gc session wake hal", "")
		if err != nil {
			t.Fatalf("gc session wake hal: %v\n%s", err, out)
		}
		if !strings.Contains(strings.ToLower(out), "cleared") && !strings.Contains(strings.ToLower(out), "woke") {
			t.Fatalf("wake output mismatch:\n%s", out)
		}
	})

	t.Run("gc session list --state all", func(t *testing.T) {
		ws.noteWarning("tutorial 03 sleep/wake workaround: mayor defaults to named_session mode=always, so the page driver temporarily switches mayor to on_demand, reduces idle_timeout from 1h to 5s, waits for the controller to acknowledge the mode flip, explicitly suspends the already-running mayor session, explicitly wakes it once under the new policy, and then waits for idle-timeout sleep before the visible nudge step")
		replaceInFile(
			t,
			filepath.Join(myCity, "city.toml"),
			`[[named_session]]
template = "mayor"
mode = "always"`,
			`[[named_session]]
template = "mayor"
mode = "on_demand"`,
		)
		replaceInFile(
			t,
			filepath.Join(myCity, "city.toml"),
			`prompt_template = "prompts/mayor.md"`,
			"prompt_template = \"prompts/mayor.md\"\nidle_timeout = \"5s\"",
		)
		var closeOut string
		closeReady := waitForCondition(t, 30*time.Second, 1*time.Second, func() bool {
			var waitErr error
			closeOut, waitErr = ws.runShell("gc session close mayor", "")
			if waitErr == nil {
				return true
			}
			return strings.Contains(closeOut, "configured always-on named sessions cannot be closed while config-managed")
		})
		if !closeReady {
			t.Fatalf("hidden mayor close probe for sleep/wake demo did not become available:\n%s", closeOut)
		}
		suspendOut, err := ws.runShell("gc session suspend mayor", "")
		if err != nil {
			t.Fatalf("hidden mayor suspend for sleep/wake demo: %v\n%s", err, suspendOut)
		}
		var suspendedOut string
		suspended := waitForCondition(t, 30*time.Second, 1*time.Second, func() bool {
			var waitErr error
			suspendedOut, waitErr = ws.runShell("gc session list --state all", "")
			if waitErr != nil {
				return false
			}
			mayorLine := sessionListRowForTarget(suspendedOut, "mayor")
			return mayorLine == "" || !strings.Contains(mayorLine, " active ")
		})
		if !suspended {
			t.Fatalf("mayor did not stop after hidden suspend:\n%s", suspendedOut)
		}
		out, err := ws.runShell("gc session wake mayor", "")
		if err != nil {
			t.Fatalf("hidden mayor wake for sleep/wake demo: %v\n%s", err, out)
		}
		var statusOut string
		ok := waitForCondition(t, 30*time.Second, 1*time.Second, func() bool {
			var err error
			statusOut, err = ws.runShell("gc session list --state all", "")
			if err != nil {
				return false
			}
			return strings.Contains(sessionListRowForTarget(statusOut, "mayor"), " asleep ")
		})
		if !ok {
			t.Fatalf("mayor did not reach asleep state in time:\n%s", statusOut)
		}
	})

	t.Run(`gc session nudge mayor "Any open tasks?"`, func(t *testing.T) {
		out, err := ws.runShell(`gc session nudge mayor "Any open tasks?"`, "")
		if err != nil {
			t.Fatalf("gc session nudge mayor: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Nudged mayor") {
			t.Fatalf("nudge output mismatch:\n%s", out)
		}
	})

	t.Run("gc session close hal", func(t *testing.T) {
		out, err := ws.runShell("gc session close hal", "")
		if err != nil {
			t.Fatalf("gc session close hal: %v\n%s", err, out)
		}
		if !strings.Contains(strings.ToLower(out), "closed") {
			t.Fatalf("close output mismatch:\n%s", out)
		}
	})

	t.Run("gc session prune --before 7d", func(t *testing.T) {
		out, err := ws.runShell("gc session prune --before 7d", "")
		if err != nil {
			t.Fatalf("gc session prune --before 7d: %v\n%s", err, out)
		}
		_ = out
	})

	t.Run("gc restart", func(t *testing.T) {
		cityToml := filepath.Join(myCity, "city.toml")
		if data, err := os.ReadFile(cityToml); err == nil && !strings.Contains(string(data), `nudge = "Check mail and hook status, then act accordingly."`) {
			replaceInFile(
				t,
				cityToml,
				`prompt_template = "prompts/mayor.md"`+"\n"+`idle_timeout = "5s"`,
				"prompt_template = \"prompts/mayor.md\"\n"+
					"nudge = \"Check mail and hook status, then act accordingly.\"\n"+
					"idle_timeout = \"5s\"",
			)
		}
		if data, err := os.ReadFile(cityToml); err == nil {
			if strings.Contains(string(data), "[[named_session]]\ntemplate = \"mayor\"\nmode = \"on_demand\"") {
				replaceInFile(
					t,
					cityToml,
					"[[named_session]]\ntemplate = \"mayor\"\nmode = \"on_demand\"",
					"[[named_session]]\ntemplate = \"mayor\"\nmode = \"always\"",
				)
			} else if !strings.Contains(string(data), "[[named_session]]") {
				appendFile(t, cityToml, `

[[named_session]]
template = "mayor"
scope = "city"
mode = "always"
`)
			}
		}
		out, err := ws.runShell("gc restart", "")
		if err != nil {
			t.Fatalf("gc restart: %v\n%s", err, out)
		}
	})

	t.Run("gc session list (after named mayor restart)", func(t *testing.T) {
		var out string
		ok := waitForCondition(t, 30*time.Second, 1*time.Second, func() bool {
			var err error
			out, err = ws.runShell("gc session list", "")
			return err == nil && strings.Contains(out, "mayor") && strings.Contains(out, "active")
		})
		if !ok {
			t.Fatalf("mayor session did not come back active after restart:\n%s", out)
		}
	})

	t.Run("gc restart (with on-demand workers)", func(t *testing.T) {
		ws.noteWarning("tutorial 03 continuity workaround: the published inline worker examples use `scope = \"rig\"`, but inline city.toml agents and named sessions must use explicit `dir` values; the page driver narrows this walkthrough to `my-project/worker` until the prose is fixed")
		appendFile(t, filepath.Join(myCity, "city.toml"), `

[[named_session]]
template = "worker"
dir = "my-project"
mode = "on_demand"
`)
		out, err := ws.runShell("gc restart", "")
		if err != nil {
			t.Fatalf("gc restart with on-demand workers: %v\n%s", err, out)
		}
	})

	t.Run("gc session list (workers on-demand)", func(t *testing.T) {
		var out string
		ok := waitForCondition(t, 30*time.Second, 1*time.Second, func() bool {
			var err error
			out, err = ws.runShell("gc session list", "")
			return err == nil && strings.Contains(out, "mayor") && !strings.Contains(out, "worker")
		})
		if !ok {
			t.Fatalf("session list should show only mayor before on-demand worker activation:\n%s", out)
		}
	})

	t.Run(`gc sling my-project/worker "Add input validation to the API"`, func(t *testing.T) {
		out, err := ws.runShell(`gc sling my-project/worker "Add input validation to the API"`, "")
		if err != nil {
			t.Fatalf("gc sling my-project/worker: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Slung") {
			t.Fatalf("worker sling output mismatch:\n%s", out)
		}
	})

	t.Run("gc session list (worker active)", func(t *testing.T) {
		var out string
		ok := waitForCondition(t, 45*time.Second, 1*time.Second, func() bool {
			var err error
			out, err = ws.runShell("gc session list", "")
			return err == nil && strings.Contains(out, "worker")
		})
		if !ok {
			t.Fatalf("worker session did not appear after sling:\n%s", out)
		}
	})

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

	t.Run("gc session peek mayor --lines 6", func(t *testing.T) {
		out, err := ws.runShell("gc session peek mayor --lines 6", "")
		if err != nil {
			t.Fatalf("gc session peek mayor --lines 6: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("peek mayor output is empty")
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
}

func sessionListRowForTarget(out, target string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "ID ") || strings.HasPrefix(line, "202") {
			continue
		}
		if strings.Contains(line, " "+target+" ") {
			return " " + line + " "
		}
	}
	return ""
}
