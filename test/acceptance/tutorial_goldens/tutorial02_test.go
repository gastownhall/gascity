//go:build acceptance_c

package tutorialgoldens

import (
	"fmt"
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

	appendFile(t, filepath.Join(myCity, "city.toml"), `

[[agent]]
name = "reviewer"
dir = "my-project"
provider = "`+tutorialReviewerProvider()+`"
prompt_template = "prompts/reviewer.md"
`)

	ws.noteWarning("tutorial 02 starts from the state tutorial 01 leaves behind, so the page driver seeds the existing hello.py artifact before exercising the reviewer flow")
	writeFile(t, filepath.Join(myProject, "hello.py"), "print(\"Hello, World!\")\n", 0o644)
	ws.noteWarning("TODO(issue #632): once bare agent names reliably resolve to the enclosing rig in acceptance-style paths, simplify tutorial 02 back to `gc prime reviewer` and `gc sling reviewer ...` from inside ~/my-project")

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
			if reviewTaskID == "" {
				return false
			}
			if data, err := os.ReadFile(filepath.Join(myProject, "review.md")); err != nil || strings.TrimSpace(string(data)) == "" {
				return false
			}
			statusOut, err := ws.runShell(fmt.Sprintf("bd show %s", reviewTaskID), "")
			return err == nil && strings.Contains(strings.ToLower(statusOut), "closed")
		}) {
			t.Fatalf("review.md was not created and closed in time for ls")
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
