//go:build acceptance_c

package tutorialgoldens

import (
	"os"
	"path/filepath"
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

func TestWriteTutorialPancakesFormulaMatchesTutorialSnippet(t *testing.T) {
	cityRoot := t.TempDir()
	writeTutorialPancakesFormula(t, cityRoot)

	got, err := os.ReadFile(filepath.Join(cityRoot, "formulas", "pancakes.toml"))
	if err != nil {
		t.Fatalf("read pancakes formula: %v", err)
	}

	want := loadTutorialPancakesFormula(t)
	if string(got) != want {
		t.Fatalf("seeded pancakes formula drifted from docs snippet\nwant:\n%s\ngot:\n%s", want, string(got))
	}
}
