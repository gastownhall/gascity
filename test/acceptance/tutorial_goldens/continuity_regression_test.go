//go:build acceptance_c

package tutorialgoldens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestTutorial02Continuity_Tutorial01NoLongerCreatesHelloPy(t *testing.T) {
	page01Path := filepath.Join(helpers.FindModuleRoot(), canonicalTutorialRoot, "01-cities-and-rigs.md")
	page02Path := filepath.Join(helpers.FindModuleRoot(), canonicalTutorialRoot, "02-agents.md")

	page01Bytes, err := os.ReadFile(page01Path)
	if err != nil {
		t.Fatalf("read tutorial 01 snapshot: %v", err)
	}
	page02Bytes, err := os.ReadFile(page02Path)
	if err != nil {
		t.Fatalf("read tutorial 02 snapshot: %v", err)
	}

	page01Text := string(page01Bytes)
	page02Text := string(page02Bytes)

	if !strings.Contains(page02Text, `gc sling reviewer "Review hello.py and write review.md with feedback"`) {
		t.Fatal("tutorial 02 continuity guard missing expected hello.py review step")
	}
	if !strings.Contains(page01Text, `gc sling claude "Add a README.md with a project description"`) {
		t.Fatal("tutorial 01 continuity guard missing expected README sling step")
	}
	if !strings.Contains(page01Text, "$ ls\nREADME.md") {
		t.Fatal("tutorial 01 continuity guard missing expected README-only result listing")
	}

	t.Fatalf("tutorial 02 asks readers to review hello.py, but tutorial 01's published happy path slings README work and only shows README.md as the resulting artifact")
}

func TestTutorial03Continuity_Tutorial02DoesNotGuaranteeLiveReviewerSession(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	page02 := snapshot.pages["docs/tutorials/02-agents.md"]
	page03 := snapshot.pages["docs/tutorials/03-sessions.md"]

	if page02 == nil || page03 == nil {
		t.Fatal("missing pinned tutorial pages for continuity check")
	}

	page02Text := collectPageText(page02)
	page03Text := collectPageText(page03)

	if !strings.Contains(page03Text, "gc session peek reviewer") {
		t.Fatal("tutorial 03 continuity guard missing expected reviewer peek step")
	}
	if strings.Contains(page02Text, "gc session new reviewer") || strings.Contains(page02Text, "gc session attach reviewer") {
		t.Fatal("tutorial 03 continuity guard expected tutorial 02 to omit explicit reviewer-session preservation steps")
	}

	t.Fatalf("tutorial 03 starts by peeking a live reviewer session, but tutorial 02 never gives readers a step that preserves or recreates that session between pages")
}

func TestTutorial03Continuity_HelperAndHalAppearWithoutSetup(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	page02 := snapshot.pages["docs/tutorials/02-agents.md"]
	page03 := snapshot.pages["docs/tutorials/03-sessions.md"]

	if page02 == nil || page03 == nil {
		t.Fatal("missing pinned tutorial pages for continuity check")
	}

	page02Text := collectPageText(page02)
	page03Text := collectPageText(page03)

	if !strings.Contains(page03Text, "helper") || !strings.Contains(page03Text, "hal") {
		t.Fatal("tutorial 03 continuity guard missing expected helper/hal references")
	}
	if strings.Contains(page02Text, "helper") || strings.Contains(page02Text, "hal") {
		t.Fatal("tutorial 03 continuity guard expected helper/hal to be absent from tutorial 02")
	}

	t.Fatalf("tutorial 03 renders helper/hal sessions in its examples, but neither tutorial 02 nor tutorial 03 establishes those agents or sessions before they appear")
}

func TestTutorial04Continuity_RigScopedReviewerRouteIsNotEstablished(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	page02 := snapshot.pages["docs/tutorials/02-agents.md"]
	page03 := snapshot.pages["docs/tutorials/03-sessions.md"]
	page04 := snapshot.pages["docs/tutorials/04-communication.md"]

	if page02 == nil || page03 == nil || page04 == nil {
		t.Fatal("missing pinned tutorial pages for continuity check")
	}

	page04Text := collectPageText(page04)

	if !strings.Contains(page04Text, "gc sling my-project/reviewer") {
		t.Fatal("tutorial 04 continuity guard missing expected my-project/reviewer route")
	}
	if pageDefinesAgentDir(page02, "reviewer", "my-project") || pageDefinesAgentDir(page03, "reviewer", "my-project") {
		t.Fatal("tutorial 04 continuity guard expected earlier tutorials to omit a rig-scoped reviewer definition")
	}

	t.Fatalf("tutorial 04 shows the mayor routing work to `my-project/reviewer`, but tutorials 02 and 03 only define a city-scoped `reviewer` with no `dir = \"my-project\"`")
}

func TestTutorial05Continuity_PriorPagesDoNotEstablishHelperAndWorkerAgents(t *testing.T) {
	page05Path := filepath.Join(helpers.FindModuleRoot(), canonicalTutorialRoot, "05-formulas.md")
	page05Bytes, err := os.ReadFile(page05Path)
	if err != nil {
		t.Fatalf("read tutorial 05 snapshot: %v", err)
	}
	page05Text := string(page05Bytes)

	previousText := make([]string, 0, 4)
	for _, name := range []string{"01-cities-and-rigs.md", "02-agents.md", "03-sessions.md", "04-communication.md"} {
		pagePath := filepath.Join(helpers.FindModuleRoot(), canonicalTutorialRoot, name)
		pageBytes, err := os.ReadFile(pagePath)
		if err != nil {
			t.Fatalf("read %s snapshot: %v", name, err)
		}
		previousText = append(previousText, string(pageBytes))
	}
	allPreviousText := strings.Join(previousText, "\n")

	if !strings.Contains(page05Text, "helper") || !strings.Contains(page05Text, "worker") {
		t.Fatal("tutorial 05 continuity guard missing expected helper/worker references")
	}
	if strings.Contains(allPreviousText, "[[agent]]\nname = \"worker\"") || strings.Contains(allPreviousText, "[[agent]]\nname = \"helper\"") {
		t.Fatal("tutorial 05 continuity guard expected earlier tutorials to omit explicit helper/worker agent definitions")
	}

	t.Fatalf("tutorial 05 assumes helper/worker agents exist, but tutorials 01-04 never give readers a published step that defines them")
}

func TestTutorial06Continuity_DependencyStepBlocksLaterPoolReadyExample(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	page06 := snapshot.pages["docs/tutorials/06-beads.md"]
	if page06 == nil {
		t.Fatal("missing pinned tutorial 06 page")
	}

	seenBlocksDependency := false
	seenReadyQuery := false
	seenUnblockingClose := false

	for _, cmd := range page06.Commands {
		switch {
		case strings.Contains(cmd.Text, "bd dep ") && strings.Contains(cmd.Text, "--blocks"):
			seenBlocksDependency = true
		case seenBlocksDependency && strings.Contains(cmd.Text, "bd close mc-a4l"):
			seenUnblockingClose = true
		case seenBlocksDependency && strings.Contains(cmd.Text, "bd ready --label=pool:my-project/worker --unassigned --limit=1"):
			seenReadyQuery = true
			if !seenUnblockingClose {
				t.Fatalf("tutorial 06 asks readers to query ready pool work after adding a blocking dependency, but it never closes the blocker before the ready query")
			}
		}
	}

	if !seenBlocksDependency || !seenReadyQuery {
		t.Fatal("tutorial 06 continuity guard missing expected dependency or ready-query steps")
	}
}

func collectPageText(page *tutorialPage) string {
	if page == nil {
		return ""
	}
	var parts []string
	for _, cmd := range page.Commands {
		parts = append(parts, cmd.Text)
	}
	for _, snippet := range page.Snippets {
		parts = append(parts, snippet.Body)
	}
	return strings.Join(parts, "\n")
}

func pageDefinesAgentDir(page *tutorialPage, agentName, dir string) bool {
	if page == nil {
		return false
	}
	for _, snippet := range page.Snippets {
		body := snippet.Body
		if strings.Contains(body, "[[agent]]") &&
			strings.Contains(body, `name = "`+agentName+`"`) &&
			strings.Contains(body, `dir = "`+dir+`"`) {
			return true
		}
	}
	return false
}
