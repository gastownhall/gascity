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

	ws.noteWarning("tutorial 02 starts from the state tutorial 01 leaves behind, so the page driver seeds the existing hello.py artifact before exercising the reviewer flow")
	writeFile(t, filepath.Join(myProject, "hello.py"), "print(\"Hello, World!\")\n", 0o644)
	ws.noteWarning("TODO(issue #632): once bare agent names reliably resolve to the enclosing rig in acceptance-style paths, simplify tutorial 02 back to `gc prime reviewer` and `gc sling reviewer ...` from inside ~/my-project")

	var reviewTaskID string

	t.Run("gc agent add --name reviewer", func(t *testing.T) {
		out, err := ws.runShell("gc agent add --name reviewer", "")
		if err != nil {
			t.Fatalf("gc agent add --name reviewer: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Scaffolded agent 'reviewer'") {
			t.Fatalf("gc agent add output mismatch:\n%s", out)
		}
	})

	t.Run("cat > agents/reviewer/agent.toml << 'EOF'", func(t *testing.T) {
		cmd := `cat > agents/reviewer/agent.toml << 'EOF'
dir = "my-project"
provider = "` + tutorialReviewerProvider() + `"
EOF`
		if out, err := ws.runShell(cmd, ""); err != nil {
			t.Fatalf("writing reviewer agent.toml: %v\n%s", err, out)
		}
	})

	t.Run("gc prime", func(t *testing.T) {
		out, err := ws.runShell("gc prime", "")
		if err != nil {
			t.Fatalf("gc prime: %v\n%s", err, out)
		}
		for _, want := range []string{"# Gas City Agent", "bd update <id> --claim", "gc.continuation_group", "--metadata-field gc.routed_to=\"$GC_TEMPLATE\"", "--no-assignee", "gc runtime drain-ack"} {
			if !strings.Contains(out, want) {
				t.Fatalf("gc prime missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("cat > agents/reviewer/prompt.template.md << 'EOF'", func(t *testing.T) {
		cmd := `cat > agents/reviewer/prompt.template.md << 'EOF'
# Code Reviewer Agent
You are an agent in a Gas City workspace. Claim routed work before executing it.

## Your tools
- ` + "`gc hook`" + ` — find routed work
- ` + "`bd update <id> --claim`" + ` — atomically claim unassigned work
- ` + "`bd show <id> --json`" + ` — verify assignee and metadata
- ` + "`bd close <id>`" + ` — mark work done
- ` + "`gc runtime drain-ack`" + ` — end the session when idle

## How to work
1. Check assigned work: ` + "`bd ready --assignee=\"$GC_SESSION_NAME\" --json --limit=1`" + `
2. If none is assigned, run ` + "`gc hook`" + `
3. Claim unassigned routed work with ` + "`bd update <id> --claim`" + `
4. Verify ` + "`assignee`" + ` and ` + "`gc.continuation_group`" + ` metadata with ` + "`bd show <id> --json`" + `
5. Review the code, write the requested feedback, and close the bead
6. If no assigned continuation work is ready, run ` + "`gc runtime drain-ack && exit`" + `

## Reviewing Code
Read the code and provide feedback on bugs, security issues, and style.
EOF`
		if out, err := ws.runShell(cmd, ""); err != nil {
			t.Fatalf("writing reviewer prompt: %v\n%s", err, out)
		}
	})

	t.Run("gc prime my-project/reviewer", func(t *testing.T) {
		out, err := ws.runShell("gc prime my-project/reviewer", "")
		if err != nil {
			t.Fatalf("gc prime my-project/reviewer: %v\n%s", err, out)
		}
		for _, want := range []string{"# Code Reviewer Agent", "## Reviewing Code", "bugs, security issues, and style"} {
			if !strings.Contains(out, want) {
				t.Fatalf("gc prime my-project/reviewer missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("cd ~/my-project", func(t *testing.T) {
		ws.setCWD(myProject)
	})

	t.Run(`gc sling my-project/reviewer "Review hello.py and write review.md with feedback"`, func(t *testing.T) {
		out, err := ws.runShell(`gc sling my-project/reviewer "Review hello.py and write review.md with feedback"`, "")
		if err != nil {
			t.Fatalf("gc sling my-project/reviewer: %v\n%s", err, out)
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
		if !waitForCondition(t, 5*time.Minute, 2*time.Second, func() bool {
			if data, err := os.ReadFile(filepath.Join(myProject, "review.md")); err != nil || strings.TrimSpace(string(data)) == "" {
				return false
			}
			return true
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
		if strings.TrimSpace(out) == "" {
			t.Fatal("review.md is empty")
		}
		if !strings.Contains(strings.ToLower(out), "review") && !strings.Contains(strings.ToLower(out), "finding") {
			t.Fatalf("review.md should contain review content:\n%s", out)
		}
	})

	if reviewTaskID != "" {
		ws.noteDiagnostic("tutorial 02 reviewer bead: %s", reviewTaskID)
	}
	if data, err := os.ReadFile(filepath.Join(myCity, "city.toml")); err == nil {
		ws.noteDiagnostic("final city.toml:\n%s", string(data))
	}
	if data, err := os.ReadFile(filepath.Join(myCity, "agents", "reviewer", "agent.toml")); err == nil {
		ws.noteDiagnostic("final agents/reviewer/agent.toml:\n%s", string(data))
	}
	if data, err := os.ReadFile(filepath.Join(myProject, "review.md")); err == nil {
		ws.noteDiagnostic("review.md:\n%s", string(data))
	}
}
