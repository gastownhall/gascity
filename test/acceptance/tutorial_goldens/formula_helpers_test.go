//go:build acceptance_c

package tutorialgoldens

import (
	"strings"
	"testing"
)

const tutorialPancakesFormulaCommand = "cat > formulas/pancakes.toml << 'EOF'"

func tutorialPancakesFormulaShellCommand(t *testing.T) string {
	t.Helper()
	return tutorialPancakesFormulaCommand + "\n" + loadTutorialPancakesFormula(t) + "EOF"
}

func loadTutorialPancakesFormula(t *testing.T) string {
	t.Helper()

	snapshot := loadTutorialSnapshot(t)
	page := snapshot.pages["docs/tutorials/05-formulas.md"]
	if page == nil {
		t.Fatal("tutorial 05 snapshot missing")
	}

	for _, snippet := range page.Snippets {
		if snippet.Language != "shell" {
			continue
		}
		body, ok := extractShellHeredocBody(snippet.Body, tutorialPancakesFormulaCommand, "EOF")
		if ok {
			return body
		}
	}

	t.Fatal("tutorial 05 pancakes heredoc not found in docs snapshot")
	return ""
}

func extractShellHeredocBody(shellSnippet, command, terminator string) (string, bool) {
	lines := strings.Split(shellSnippet, "\n")
	wantCommand := "$ " + command
	started := false
	body := make([]string, 0, len(lines))

	for _, line := range lines {
		if !started {
			if line == wantCommand {
				started = true
			}
			continue
		}
		if line == terminator {
			return strings.Join(body, "\n") + "\n", true
		}
		body = append(body, line)
	}

	return "", false
}

const tutorialLintScriptCommand = "cat > scripts/lint-changed.sh << 'EOF'"

// tutorialLintScriptShellCommand is tutorial 07's documented heredoc that
// writes the stand-in lint script, read from the page so the golden runs the
// script the page shows.
func tutorialLintScriptShellCommand(t *testing.T) string {
	t.Helper()
	page := loadTutorialSnapshot(t).pages["docs/tutorials/07-orders.md"]
	if page == nil {
		t.Fatal("tutorial 07 snapshot missing")
	}
	for _, snippet := range page.Snippets {
		if snippet.Language != "shell" {
			continue
		}
		if body, ok := extractShellHeredocBody(snippet.Body, tutorialLintScriptCommand, "EOF"); ok {
			return tutorialLintScriptCommand + "\n" + body + "EOF"
		}
	}
	t.Fatal("tutorial 07 lint-changed.sh heredoc not found in docs snapshot")
	return ""
}

// tutorialFileSnippet returns the body of the fenced snippet on a tutorial
// page whose first line names the file it holds (for example
// "# orders/lint-check.toml"), the way the page asks readers to create it.
func tutorialFileSnippet(t *testing.T, pageRel, language, firstLine string) string {
	t.Helper()
	page := loadTutorialSnapshot(t).pages[pageRel]
	if page == nil {
		t.Fatalf("%s snapshot missing", pageRel)
	}
	for _, snippet := range page.Snippets {
		if snippet.Language != language {
			continue
		}
		if first, _, _ := strings.Cut(snippet.Body, "\n"); strings.TrimSpace(first) == firstLine {
			return snippet.Body + "\n"
		}
	}
	t.Fatalf("%s has no %s snippet starting %q", pageRel, language, firstLine)
	return ""
}
