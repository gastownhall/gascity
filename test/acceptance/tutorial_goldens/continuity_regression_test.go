//go:build acceptance_c

package tutorialgoldens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestTutorialContinuity_HelloPyCarriesAcrossPages(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	page01 := snapshot.pages["docs/tutorials/01-cities-and-rigs.md"]
	page02 := snapshot.pages["docs/tutorials/02-agents.md"]
	page03 := snapshot.pages["docs/tutorials/03-sessions.md"]

	page01Text := collectPageText(page01)
	page02Text := collectPageText(page02)
	page03Text := collectPageText(page03)

	for _, want := range []string{
		`gc sling my-project/claude "Write hello world in python to the file hello.py"`,
		"$ ls\nhello.py",
	} {
		if !strings.Contains(page01Text, want) {
			t.Fatalf("tutorial 01 is missing %q", want)
		}
	}
	if !strings.Contains(page02Text, `gc sling my-project/reviewer "Review hello.py and write review.md with feedback"`) {
		t.Fatal("tutorial 02 no longer reviews the hello.py artifact produced by tutorial 01")
	}
	if !strings.Contains(page03Text, `mp-p956, “Review hello.py and write review.md with feedback.”`) {
		t.Fatal("tutorial 03 no longer follows the hello.py review bead from tutorial 02")
	}
}

func TestTutorialContinuity_SessionLookupFlowIsExplicit(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	page03 := snapshot.pages["docs/tutorials/03-sessions.md"]
	page03Text := collectPageText(page03)

	for _, want := range []string{
		"gc session list --template my-project/reviewer",
		"gc session peek mc-8sfd",
	} {
		if !strings.Contains(page03Text, want) {
			t.Fatalf("tutorial 03 is missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"gc session peek reviewer",
		"helper",
	} {
		if strings.Contains(page03Text, unwanted) {
			t.Fatalf("tutorial 03 still contains stale session guidance %q", unwanted)
		}
	}
}

func TestTutorialContinuity_CommunicationUsesVisibleWakeAndRigScopedReviewer(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	page04 := snapshot.pages["docs/tutorials/04-communication.md"]
	page04Text := collectPageText(page04)

	if !strings.Contains(page04Text, `gc session nudge mayor "Check mail and hook status, then act accordingly"`) {
		t.Fatal("tutorial 04 no longer shows the explicit mayor wake-up step")
	}
	if !strings.Contains(page04Text, `gc sling my-project/reviewer "Review the auth module changes"`) {
		t.Fatal("tutorial 04 no longer shows the mayor routing work to the rig-scoped reviewer")
	}
}

func TestTutorialContinuity_FormulasIntroducesWorkerBeforeUse(t *testing.T) {
	page05Text := readTutorialPageText(t, "05-formulas.md")

	if !strings.Contains(page05Text, `name = "worker"`) {
		t.Fatal("tutorial 05 slings work to worker without first establishing that agent in the prose")
	}
}

func TestTutorialContinuity_BeadsExplainsBlockedReadyQuery(t *testing.T) {
	page06Text := readTutorialPageText(t, "06-beads.md")

	for _, want := range []string{
		"`mc-xp7` won't appear in any agent's work query until `mc-a4l` is closed",
		"this query won't return",
		"Once `mc-a4l` closes, rerun the same query",
	} {
		if !strings.Contains(page06Text, want) {
			t.Fatalf("tutorial 06 is missing the blocked-work explanation %q", want)
		}
	}
}

func collectPageText(page *tutorialPage) string {
	if page == nil {
		return ""
	}
	parts := make([]string, 0, len(page.Commands)+len(page.Snippets))
	for _, cmd := range page.Commands {
		parts = append(parts, cmd.Text)
	}
	for _, snippet := range page.Snippets {
		parts = append(parts, snippet.Body)
	}
	return strings.Join(parts, "\n")
}

func readTutorialPageText(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(helpers.FindModuleRoot(), canonicalTutorialRoot, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tutorial page %s: %v", name, err)
	}
	return string(data)
}
