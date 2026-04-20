//go:build acceptance_c

package tutorialgoldens

import (
	"strings"
	"testing"
)

func TestExtractShellHeredocBody(t *testing.T) {
	snippet := `~/my-city
$ cat > formulas/pancakes.toml << 'EOF'
formula = "pancakes"
description = "Make pancakes from scratch"
EOF`

	got, ok := extractShellHeredocBody(snippet, tutorialPancakesFormulaCommand, "EOF")
	if !ok {
		t.Fatal("expected heredoc body to be extracted")
	}

	want := "formula = \"pancakes\"\ndescription = \"Make pancakes from scratch\"\n"
	if got != want {
		t.Fatalf("heredoc body mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestLoadTutorialPancakesFormulaFromDocs(t *testing.T) {
	got := loadTutorialPancakesFormula(t)
	for _, want := range []string{
		`formula = "pancakes"`,
		`description = "Make pancakes from scratch"`,
		`needs = ["cook"]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("loadTutorialPancakesFormula missing %q:\n%s", want, got)
		}
	}
}
