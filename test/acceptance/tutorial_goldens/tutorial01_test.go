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

func TestTutorial01Cities(t *testing.T) {
	t.Run("PrimaryWizardFlow", func(t *testing.T) {
		ws := newTutorialWorkspace(t)
		ws.attachDiagnostics(t, "tutorial-01-primary")

		myCity := expandHome(ws.home(), "~/my-city")
		myProject := expandHome(ws.home(), "~/my-project")
		mustMkdirAll(t, myProject)

		var helloTaskID string

		t.Run("brew install gascity", func(t *testing.T) {
			if _, err := os.Stat(goldenGCBinary); err != nil {
				t.Fatalf("gc binary missing: %v", err)
			}
			ws.noteWarning("tutorial 01 setup: satisfied `brew install gascity` via harness bootstrap")
			t.Log("workaround: `brew install gascity` is satisfied by the acceptance harness bootstrap")
		})

		t.Run("gc version", func(t *testing.T) {
			out, err := ws.runShell("gc version", "")
			if err != nil {
				t.Fatalf("gc version: %v\n%s", err, out)
			}
			if strings.TrimSpace(out) == "" {
				t.Fatal("gc version output is empty")
			}
		})

		t.Run("gc init ~/my-city", func(t *testing.T) {
			ws.noteWarning("tutorial 01 documents the interactive wizard, but the acceptance harness uses the equivalent non-interactive `gc init ~/my-city --provider claude` path because the wizard requires a real TTY")
			out, err := ws.runShell("gc init ~/my-city --provider claude", "")
			if err != nil {
				t.Fatalf("gc init wizard: %v\n%s", err, out)
			}
			for _, want := range []string{
				"Welcome to Gas City!",
				`Initialized city "my-city" with default provider "claude".`,
				"Registering city with supervisor",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("gc init output missing %q:\n%s", want, out)
				}
			}
			if _, err := os.Stat(filepath.Join(myCity, "city.toml")); err != nil {
				t.Fatalf("city.toml missing after init: %v", err)
			}
		})

		t.Run("gc cities", func(t *testing.T) {
			out, err := ws.runShell("gc cities", "")
			if err != nil {
				t.Fatalf("gc cities: %v\n%s", err, out)
			}
			if !strings.Contains(out, "my-city") {
				t.Fatalf("gc cities should list my-city:\n%s", out)
			}
		})

		t.Run("gc init ~/my-city --provider claude", func(t *testing.T) {
			wsProvider := newTutorialWorkspace(t)
			wsProvider.attachDiagnostics(t, "tutorial-01-provider-branch")

			out, err := wsProvider.runShell("gc init ~/my-city --provider claude", "")
			if err != nil {
				t.Fatalf("gc init --provider claude: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(expandHome(wsProvider.home(), "~/my-city"), "city.toml")); err != nil {
				t.Fatalf("city.toml missing after explicit provider init: %v", err)
			}
			if !strings.Contains(strings.ToLower(out), "created") && !strings.Contains(strings.ToLower(out), "registered") {
				t.Fatalf("gc init --provider output missing creation marker:\n%s", out)
			}
		})

		t.Run("cd ~/my-city", func(t *testing.T) {
			if _, err := os.Stat(myCity); err != nil {
				t.Fatalf("my-city missing: %v", err)
			}
			ws.setCWD(myCity)
		})

		t.Run("ls", func(t *testing.T) {
			out, err := ws.runShell("ls", "")
			if err != nil {
				t.Fatalf("ls: %v\n%s", err, out)
			}
			for _, want := range []string{"city.toml", "formulas", "orders", "prompts"} {
				if !strings.Contains(out, want) {
					t.Fatalf("ls output missing %q:\n%s", want, out)
				}
			}
		})

		t.Run("cat city.toml", func(t *testing.T) {
			out, err := ws.runShell("cat city.toml", "")
			if err != nil {
				t.Fatalf("cat city.toml: %v\n%s", err, out)
			}
			for _, want := range []string{
				`name = "my-city"`,
				`provider = "claude"`,
				`name = "mayor"`,
				`prompt_template = "prompts/mayor.md"`,
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("city.toml missing %q:\n%s", want, out)
				}
			}
		})

		t.Run("gc status", func(t *testing.T) {
			out, err := ws.runShell("gc status", "")
			if err != nil {
				t.Fatalf("gc status: %v\n%s", err, out)
			}
			for _, want := range []string{"my-city", "Controller:"} {
				if !strings.Contains(out, want) {
					t.Fatalf("gc status missing %q:\n%s", want, out)
				}
			}
		})

		t.Run("gc rig add ~/my-project", func(t *testing.T) {
			out, err := ws.runShell("gc rig add ~/my-project", "")
			if err != nil {
				t.Fatalf("gc rig add: %v\n%s", err, out)
			}
			if !strings.Contains(out, "Rig added") {
				t.Fatalf("gc rig add output missing success marker:\n%s", out)
			}
		})

		t.Run("cat city.toml (with rig)", func(t *testing.T) {
			out, err := ws.runShell("cat city.toml", "")
			if err != nil {
				t.Fatalf("cat city.toml: %v\n%s", err, out)
			}
			if !strings.Contains(out, `name = "my-project"`) {
				t.Fatalf("city.toml missing rig entry:\n%s", out)
			}
			if !strings.Contains(out, myProject) {
				t.Fatalf("city.toml missing rig path %q:\n%s", myProject, out)
			}
		})

		t.Run("gc rig list", func(t *testing.T) {
			out, err := ws.runShell("gc rig list", "")
			if err != nil {
				t.Fatalf("gc rig list: %v\n%s", err, out)
			}
			if !strings.Contains(out, "my-project") {
				t.Fatalf("gc rig list missing my-project:\n%s", out)
			}
		})

		t.Run("cd ~/my-project", func(t *testing.T) {
			ws.setCWD(myProject)
		})

		t.Run(`gc sling my-project/claude "Write hello world in python to the file hello.py"`, func(t *testing.T) {
			out, err := ws.runShell(`gc sling my-project/claude "Write hello world in python to the file hello.py"`, "")
			if err != nil {
				t.Fatalf("gc sling rig task: %v\n%s", err, out)
			}
			helloTaskID = firstBeadID(out)
			if helloTaskID == "" {
				t.Fatalf("could not parse hello.py task id from gc sling output:\n%s", out)
			}
		})

		t.Run("gc bd show mp-ff9 --watch", func(t *testing.T) {
			if helloTaskID == "" {
				t.Fatal("missing hello.py task id from prior sling step")
			}
			rs, err := ws.startShell(fmt.Sprintf("gc bd show %s --watch", helloTaskID), "")
			if err != nil {
				t.Fatalf("gc bd show --watch start: %v", err)
			}
			defer func() { _ = rs.stop() }()

			if err := rs.waitFor(helloTaskID, 30*time.Second); err != nil {
				t.Fatalf("gc bd show --watch did not render target bead: %v", err)
			}
			if !waitForCondition(t, 5*time.Minute, 2*time.Second, func() bool {
				data, err := os.ReadFile(filepath.Join(myProject, "hello.py"))
				return err == nil && strings.TrimSpace(string(data)) != ""
			}) {
				t.Fatalf("hello.py was not created in time\n%s", rs.output())
			}
		})

		t.Run("ls (rig)", func(t *testing.T) {
			out, err := ws.runShell("ls", "")
			if err != nil {
				t.Fatalf("ls in rig: %v\n%s", err, out)
			}
			if !strings.Contains(out, "hello.py") {
				t.Fatalf("rig ls missing hello.py:\n%s", out)
			}
		})
	})
}
