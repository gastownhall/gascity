//go:build acceptance_c

package tutorialgoldens

import (
	"fmt"
	"strings"
	"testing"
)

func TestTutorial06Beads(t *testing.T) {
	ws := newTutorialWorkspace(t)
	ws.attachDiagnostics(t, "tutorial-06")

	myCity := expandHome(ws.home(), "~/my-city")
	myProject := expandHome(ws.home(), "~/my-project")
	mustMkdirAll(t, myProject)

	out, err := ws.runShell("gc init ~/my-city --provider claude --skip-provider-readiness", "")
	if err != nil {
		t.Fatalf("seed city init: %v\n%s", err, out)
	}
	ws.setCWD(myCity)

	if out, err := ws.runShell("gc rig add ~/my-project", ""); err != nil {
		t.Fatalf("seed rig add: %v\n%s", err, out)
	}
	if listOut, listErr := ws.runShell("gc session list", ""); listErr != nil || !strings.Contains(listOut, "mayor") {
		startOut, startErr := ws.runShell("gc start ~/my-city", "")
		if startErr != nil {
			t.Fatalf("seed city start: %v\n%s", startErr, startOut)
		}
	}

	updateAPIOut, err := ws.runShell(`bd create "Update API docs"`, "")
	if err != nil {
		t.Fatalf("seed update api docs: %v\n%s", err, updateAPIOut)
	}
	updateAPIID := firstBeadID(updateAPIOut)
	if updateAPIID == "" {
		t.Fatalf("could not parse update-api-docs bead id:\n%s", updateAPIOut)
	}
	if out, err := ws.runShell(fmt.Sprintf("bd label add %s pool:my-project/worker", updateAPIID), ""); err != nil {
		t.Fatalf("seed pool label: %v\n%s", err, out)
	}
	if out, err := ws.runShell("gc formula cook pancakes", ""); err != nil {
		t.Fatalf("seed pancakes cook: %v\n%s", err, out)
	}

	var loginBugID string
	var refactorID string
	var sprintConvoyID string
	var ownedConvoyID string
	var deployConvoyID string

	t.Run("bd list", func(t *testing.T) {
		out, err := ws.runShell("bd list", "")
		if err != nil {
			t.Fatalf("bd list: %v\n%s", err, out)
		}
		for _, want := range []string{"Update API docs", "Status:"} {
			if !strings.Contains(out, want) {
				t.Fatalf("bd list missing %q:\n%s", want, out)
			}
		}
	})

	t.Run(`bd create "Fix the login bug"`, func(t *testing.T) {
		out, err := ws.runShell(`bd create "Fix the login bug"`, "")
		if err != nil {
			t.Fatalf("bd create Fix the login bug: %v\n%s", err, out)
		}
		loginBugID = firstBeadID(out)
		if loginBugID == "" {
			t.Fatalf("could not parse login bug bead id:\n%s", out)
		}
	})

	t.Run(`bd create "Refactor auth module" --type feature`, func(t *testing.T) {
		out, err := ws.runShell(`bd create "Refactor auth module" --type feature`, "")
		if err != nil {
			t.Fatalf("bd create Refactor auth module: %v\n%s", err, out)
		}
		refactorID = firstBeadID(out)
		if refactorID == "" {
			t.Fatalf("could not parse refactor auth module bead id:\n%s", out)
		}
	})

	t.Run("bd close mc-ykp", func(t *testing.T) {
		if loginBugID == "" {
			t.Fatal("missing Fix the login bug bead id")
		}
		out, err := ws.runShell(fmt.Sprintf("bd close %s", loginBugID), "")
		if err != nil {
			t.Fatalf("bd close %s: %v\n%s", loginBugID, err, out)
		}
		if !strings.Contains(out, "Closed") {
			t.Fatalf("bd close output mismatch:\n%s", out)
		}
	})

	t.Run("bd list --status open --flat", func(t *testing.T) {
		out, err := ws.runShell("bd list --status open --flat", "")
		if err != nil {
			t.Fatalf("bd list --status open --flat: %v\n%s", err, out)
		}
		for _, want := range []string{"Refactor auth module", "Update API docs"} {
			if !strings.Contains(out, want) {
				t.Fatalf("open flat list missing %q:\n%s", want, out)
			}
		}
		if loginBugID != "" && strings.Contains(out, loginBugID) {
			t.Fatalf("closed login bug should not still appear in open list:\n%s", out)
		}
	})

	t.Run("bd list --status in_progress --flat", func(t *testing.T) {
		out, err := ws.runShell("bd list --status in_progress --flat", "")
		if err != nil {
			t.Fatalf("bd list --status in_progress --flat: %v\n%s", err, out)
		}
		if !strings.Contains(out, "mayor") && !strings.Contains(out, "session") {
			t.Fatalf("in-progress list should expose live runtime work:\n%s", out)
		}
	})

	t.Run("bd label add mc-a4l priority:high", func(t *testing.T) {
		if refactorID == "" {
			t.Fatal("missing Refactor auth module bead id")
		}
		out, err := ws.runShell(fmt.Sprintf("bd label add %s priority:high", refactorID), "")
		if err != nil {
			t.Fatalf("adding priority label: %v\n%s", err, out)
		}
		if !strings.Contains(out, "priority:high") {
			t.Fatalf("label add output mismatch:\n%s", out)
		}
	})

	t.Run("bd label add mc-a4l frontend", func(t *testing.T) {
		out, err := ws.runShell(fmt.Sprintf("bd label add %s frontend", refactorID), "")
		if err != nil {
			t.Fatalf("adding frontend label: %v\n%s", err, out)
		}
		if !strings.Contains(out, "frontend") {
			t.Fatalf("label add output mismatch:\n%s", out)
		}
	})

	t.Run("bd list --label priority:high --flat", func(t *testing.T) {
		out, err := ws.runShell("bd list --label priority:high --flat", "")
		if err != nil {
			t.Fatalf("bd list --label priority:high --flat: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Refactor auth module") {
			t.Fatalf("label query should show Refactor auth module:\n%s", out)
		}
	})

	t.Run("bd update mc-a4l --set-metadata branch=feature/auth --set-metadata reviewer=sky", func(t *testing.T) {
		out, err := ws.runShell(fmt.Sprintf("bd update %s --set-metadata branch=feature/auth --set-metadata reviewer=sky", refactorID), "")
		if err != nil {
			t.Fatalf("bd update metadata: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Updated") {
			t.Fatalf("metadata update output mismatch:\n%s", out)
		}
	})

	t.Run("bd dep mc-a4l --blocks mc-xp7", func(t *testing.T) {
		out, err := ws.runShell(fmt.Sprintf("bd dep %s --blocks %s", refactorID, updateAPIID), "")
		if err != nil {
			t.Fatalf("bd dep --blocks: %v\n%s", err, out)
		}
		if !strings.Contains(out, "blocks") {
			t.Fatalf("dependency output mismatch:\n%s", out)
		}
	})

	t.Run(`gc convoy create "Sprint 42" mc-ykp mc-a4l mc-xp7`, func(t *testing.T) {
		out, err := ws.runShell(fmt.Sprintf(`gc convoy create "Sprint 42" %s %s %s`, loginBugID, refactorID, updateAPIID), "")
		if err != nil {
			t.Fatalf("gc convoy create Sprint 42: %v\n%s", err, out)
		}
		sprintConvoyID = firstBeadID(out)
		if sprintConvoyID == "" {
			t.Fatalf("could not parse Sprint 42 convoy id:\n%s", out)
		}
	})

	t.Run("gc convoy status mc-d4g", func(t *testing.T) {
		out, err := ws.runShell(fmt.Sprintf("gc convoy status %s", sprintConvoyID), "")
		if err != nil {
			t.Fatalf("gc convoy status %s: %v\n%s", sprintConvoyID, err, out)
		}
		for _, want := range []string{"Sprint 42", "Fix the login bug", "Refactor auth module", "Update API docs"} {
			if !strings.Contains(out, want) {
				t.Fatalf("convoy status missing %q:\n%s", want, out)
			}
		}
	})

	t.Run(`gc convoy create "Auth rewrite" --owned --target integration/auth`, func(t *testing.T) {
		out, err := ws.runShell(`gc convoy create "Auth rewrite" --owned --target integration/auth`, "")
		if err != nil {
			t.Fatalf("gc convoy create Auth rewrite: %v\n%s", err, out)
		}
		ownedConvoyID = firstBeadID(out)
		if ownedConvoyID == "" {
			t.Fatalf("could not parse owned convoy id:\n%s", out)
		}
	})

	t.Run("gc convoy land mc-0ud", func(t *testing.T) {
		out, err := ws.runShell(fmt.Sprintf("gc convoy land %s", ownedConvoyID), "")
		if err != nil {
			t.Fatalf("gc convoy land %s: %v\n%s", ownedConvoyID, err, out)
		}
		if !strings.Contains(out, "Landed") {
			t.Fatalf("convoy land output mismatch:\n%s", out)
		}
	})

	t.Run("gc convoy add mc-d4g mc-xp7", func(t *testing.T) {
		out, err := ws.runShell(fmt.Sprintf("gc convoy add %s %s", sprintConvoyID, updateAPIID), "")
		if err != nil {
			t.Fatalf("gc convoy add %s %s: %v\n%s", sprintConvoyID, updateAPIID, err, out)
		}
		if !strings.Contains(out, "Added") {
			t.Fatalf("convoy add output mismatch:\n%s", out)
		}
	})

	t.Run("gc convoy check", func(t *testing.T) {
		out, err := ws.runShell("gc convoy check", "")
		if err != nil {
			t.Fatalf("gc convoy check: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("gc convoy check output is empty")
		}
	})

	t.Run("gc convoy stranded", func(t *testing.T) {
		out, err := ws.runShell("gc convoy stranded", "")
		if err != nil {
			t.Fatalf("gc convoy stranded: %v\n%s", err, out)
		}
		if !strings.Contains(out, "CONVOY") {
			t.Fatalf("convoy stranded output missing header:\n%s", out)
		}
	})

	t.Run(`gc convoy create "Deploy v2" --owner mayor --merge mr --target main`, func(t *testing.T) {
		out, err := ws.runShell(`gc convoy create "Deploy v2" --owner mayor --merge mr --target main`, "")
		if err != nil {
			t.Fatalf("gc convoy create Deploy v2: %v\n%s", err, out)
		}
		deployConvoyID = firstBeadID(out)
		if deployConvoyID == "" {
			t.Fatalf("could not parse Deploy v2 convoy id:\n%s", out)
		}
	})

	t.Run("gc convoy target mc-zk1 develop", func(t *testing.T) {
		out, err := ws.runShell(fmt.Sprintf("gc convoy target %s develop", deployConvoyID), "")
		if err != nil {
			t.Fatalf("gc convoy target %s develop: %v\n%s", deployConvoyID, err, out)
		}
		if !strings.Contains(out, "develop") {
			t.Fatalf("convoy target output mismatch:\n%s", out)
		}
	})

	t.Run("bd ready --label=pool:my-project/worker --unassigned --limit=1", func(t *testing.T) {
		out, err := ws.runShell("bd ready --label=pool:my-project/worker --unassigned --limit=1", "")
		if err != nil {
			t.Fatalf("bd ready --label=pool:my-project/worker --unassigned --limit=1: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Update API docs") {
			t.Fatalf("pool ready query should surface Update API docs:\n%s", out)
		}
	})

	t.Run("bd list --status open --type task --flat", func(t *testing.T) {
		out, err := ws.runShell("bd list --status open --type task --flat", "")
		if err != nil {
			t.Fatalf("bd list --status open --type task --flat: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Update API docs") {
			t.Fatalf("open task list should contain Update API docs:\n%s", out)
		}
	})

	t.Run("bd show mc-a4l", func(t *testing.T) {
		out, err := ws.runShell(fmt.Sprintf("bd show %s", refactorID), "")
		if err != nil {
			t.Fatalf("bd show %s: %v\n%s", refactorID, err, out)
		}
		for _, want := range []string{"Refactor auth module", "feature/auth", "reviewer: sky"} {
			if !strings.Contains(out, want) {
				t.Fatalf("bd show missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("bd close mc-a4l", func(t *testing.T) {
		out, err := ws.runShell(fmt.Sprintf("bd close %s", refactorID), "")
		if err != nil {
			t.Fatalf("bd close %s: %v\n%s", refactorID, err, out)
		}
		if !strings.Contains(out, "Closed") {
			t.Fatalf("bd close output mismatch:\n%s", out)
		}
	})

	ws.noteDiagnostic("tutorial 05 seeded update-api-docs bead: %s", updateAPIID)
	if sprintConvoyID != "" {
		ws.noteDiagnostic("tutorial 05 Sprint 42 convoy: %s", sprintConvoyID)
	}
}
