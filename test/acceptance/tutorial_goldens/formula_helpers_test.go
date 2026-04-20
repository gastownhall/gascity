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
