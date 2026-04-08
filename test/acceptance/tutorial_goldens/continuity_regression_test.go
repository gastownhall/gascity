//go:build acceptance_c

package tutorialgoldens

import (
	"strings"
	"testing"
)

func TestTutorial03Continuity_Tutorial02DoesNotCreateSessionsWithGCSessionNew(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	page02 := snapshot.pages["docs/tutorials/02-agents.md"]
	page03 := snapshot.pages["docs/tutorials/03-sessions.md"]

	if page02 == nil || page03 == nil {
		t.Fatal("missing pinned tutorial pages for continuity check")
	}

	mentionsSessionNew := false
	for _, snippet := range page02.Snippets {
		if strings.Contains(snippet.Body, "gc session new") {
			mentionsSessionNew = true
			break
		}
	}
	if !mentionsSessionNew && strings.Contains(page03.Title, "Tutorial 03 - Sessions") {
		t.Fatalf("tutorial 03 claims tutorial 02 created sessions with `gc session new`, but tutorial 02 has no published shell step or snippet that does so")
	}
}

func TestTutorial03Continuity_Tutorial02DoesNotEstablishMyAPIHelperWorkerHal(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	page02 := snapshot.pages["docs/tutorials/02-agents.md"]
	page03 := snapshot.pages["docs/tutorials/03-sessions.md"]

	if page02 == nil || page03 == nil {
		t.Fatal("missing pinned tutorial pages for continuity check")
	}

	page02Text := collectPageText(page02)
	var missing []string
	for _, token := range []string{"my-api", "helper", "worker", "hal"} {
		if !strings.Contains(page02Text, token) {
			missing = append(missing, token)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("tutorial 03 depends on prerequisites not established by tutorial 02: %s", strings.Join(missing, ", "))
	}
}

func TestTutorial05Continuity_DependencyStepBlocksLaterPoolReadyExample(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	page05 := snapshot.pages["docs/tutorials/05-beads.md"]
	if page05 == nil {
		t.Fatal("missing pinned tutorial 05 page")
	}

	seenBlocksDependency := false
	seenReadyQuery := false
	seenUnblockingClose := false

	for _, cmd := range page05.Commands {
		switch {
		case strings.Contains(cmd.Text, "bd dep ") && strings.Contains(cmd.Text, "--blocks"):
			seenBlocksDependency = true
		case seenBlocksDependency && strings.Contains(cmd.Text, "bd close mc-a4l"):
			seenUnblockingClose = true
		case seenBlocksDependency && strings.Contains(cmd.Text, "bd ready --label=pool:my-project/worker --unassigned --limit=1"):
			seenReadyQuery = true
			if !seenUnblockingClose {
				t.Fatalf("tutorial 05 asks readers to query ready pool work after adding a blocking dependency, but it never closes the blocker before the ready query")
			}
		}
	}

	if !seenBlocksDependency || !seenReadyQuery {
		t.Fatal("tutorial 05 continuity guard missing expected dependency or ready-query steps")
	}
}

func collectPageText(page *tutorialPage) string {
	var parts []string
	for _, cmd := range page.Commands {
		parts = append(parts, cmd.Text)
	}
	for _, snippet := range page.Snippets {
		parts = append(parts, snippet.Body)
	}
	return strings.Join(parts, "\n")
}
