//go:build acceptance_c

package tutorialgoldens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTutorial02Agents(t *testing.T) {
	ws := newTutorialWorkspace(t)
	ws.attachDiagnostics(t, "tutorial-02")

	myCity := expandHome(ws.home(), "~/my-city")
	myProject := expandHome(ws.home(), "~/my-project")
	mustMkdirAll(t, myProject)

	out, err := ws.runShell("gc init ~/my-city --provider claude --skip-provider-readiness", "")
	if err != nil {
		t.Fatalf("seed city init: %v\n%s", err, out)
	}
	ws.setCWD(myCity)

	out, err = ws.runShell("gc rig add ~/my-project", "")
	if err != nil {
		t.Fatalf("seed rig add: %v\n%s", err, out)
	}

	writeFile(t, filepath.Join(myProject, "hello.py"), "print(\"Hello, World!\")\n", 0o644)
	appendFile(t, filepath.Join(myCity, "city.toml"), `

[[agent]]
name = "reviewer"
scope = "rig"
provider = "codex"
prompt_template = "prompts/reviewer.md"
`)

	if listOut, listErr := ws.runShell("gc session list", ""); listErr != nil || !strings.Contains(listOut, "mayor") {
		startOut, startErr := ws.runShell("gc start ~/my-city", "")
		if startErr != nil {
			t.Fatalf("seed city start: %v\n%s", startErr, startOut)
		}
	}

	var reviewTaskID string

	t.Run("gc prime", func(t *testing.T) {
		out, err := ws.runShell("gc prime", "")
		if err != nil {
			t.Fatalf("gc prime: %v\n%s", err, out)
		}
		for _, want := range []string{"# Gas City Agent", "bd ready", "bd close <id>"} {
			if !strings.Contains(out, want) {
				t.Fatalf("gc prime missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("cat > prompts/reviewer.md << 'EOF'", func(t *testing.T) {
		cmd := `cat > prompts/reviewer.md << 'EOF'
# Code Reviewer Agent
You are an agent in a Gas City workspace. Check for available work and execute it.

## Your tools
- ` + "`bd ready`" + ` — see available work items
- ` + "`bd show <id>`" + ` — see details of a work item
- ` + "`bd close <id>`" + ` — mark work as done

## How to work
1. Check for available work: ` + "`bd ready`" + `
2. Pick a bead and execute the work described in its title
3. When done, close it: ` + "`bd close <id>`" + `
4. Check for more work. Repeat until the queue is empty.

## Reviewing Code
Read the code and provide feedback on bugs, security issues, and style.
EOF`
		if out, err := ws.runShell(cmd, ""); err != nil {
			t.Fatalf("writing reviewer prompt: %v\n%s", err, out)
		}
	})

	t.Run("gc prime reviewer", func(t *testing.T) {
		out, err := ws.runShell("gc prime reviewer", "")
		if err != nil {
			t.Fatalf("gc prime reviewer: %v\n%s", err, out)
		}
		for _, want := range []string{"# Code Reviewer Agent", "## Reviewing Code", "bugs, security issues, and style"} {
			if !strings.Contains(out, want) {
				t.Fatalf("gc prime reviewer missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("cd ~/my-project", func(t *testing.T) {
		ws.setCWD(myProject)
	})

	t.Run(`gc sling reviewer "Review hello.py and write review.md with feedback"`, func(t *testing.T) {
		out, err := ws.runShell(`gc sling reviewer "Review hello.py and write review.md with feedback"`, "")
		if err != nil {
			t.Fatalf("gc sling reviewer: %v\n%s", err, out)
		}
		reviewTaskID = firstBeadID(out)
		if reviewTaskID == "" {
			t.Fatalf("could not parse review bead id from:\n%s", out)
		}
		if !strings.Contains(out, "Slung") {
			t.Fatalf("gc sling output missing routing summary:\n%s", out)
		}
	})

	t.Run("ls", func(t *testing.T) {
		if !waitForCondition(t, 2*time.Minute, 2*time.Second, func() bool {
			_, err := os.Stat(filepath.Join(myProject, "review.md"))
			return err == nil
		}) {
			t.Fatalf("review.md was not created in time for ls")
		}
		out, err := ws.runShell("ls", "")
		if err != nil {
			t.Fatalf("ls: %v\n%s", err, out)
		}
		for _, want := range []string{"hello.py", "review.md"} {
			if !strings.Contains(out, want) {
				t.Fatalf("ls missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("cat review.md", func(t *testing.T) {
		out, err := ws.runShell("cat review.md", "")
		if err != nil {
			t.Fatalf("cat review.md: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Review") {
			t.Fatalf("review.md should contain a review heading or summary:\n%s", out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("review.md is empty")
		}
	})

	t.Run("gc session peek reviewer", func(t *testing.T) {
		out, err := ws.runShell("gc session peek reviewer", "")
		if err != nil {
			t.Fatalf("gc session peek reviewer: %v\n%s", err, out)
		}
		if !strings.Contains(out, "reviewer") {
			t.Fatalf("peek reviewer output missing reviewer context:\n%s", out)
		}
	})

	t.Run("gc session list", func(t *testing.T) {
		out, err := ws.runShell("gc session list", "")
		if err != nil {
			t.Fatalf("gc session list: %v\n%s", err, out)
		}
		for _, want := range []string{"ID", "TEMPLATE", "mayor"} {
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
		ws.setCWD(myCity)
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
		if !strings.Contains(out, "Nudged mayor") {
			t.Fatalf("nudge output mismatch:\n%s", out)
		}
	})

	if reviewTaskID != "" {
		ws.noteDiagnostic("tutorial 02 reviewer bead: %s", reviewTaskID)
	}
	if data, err := os.ReadFile(filepath.Join(myCity, "city.toml")); err == nil {
		ws.noteDiagnostic("final city.toml:\n%s", string(data))
	}
	if data, err := os.ReadFile(filepath.Join(myProject, "review.md")); err == nil {
		ws.noteDiagnostic("review.md:\n%s", string(data))
	}
}
