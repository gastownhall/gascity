package unparam_test

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/unparam"
)

const lib = `package p

func add(a, b int) int {
	double := a * 2
	return double + 1
}

// Exported parameters are not checked.
func Exported(a, b int) int {
	double := a * 2
	return double + 1
}

// Use calls add.
func Use() int { return add(1, 2) + add(3, 4) }

func scale(x, factor int) int { return x * factor }

// Scaled calls scale with one factor; a test passes another.
func Scaled() int { return scale(2, 10) }
`

func TestTestUnitReportsUnusedParamOfUnexportedFunc(t *testing.T) {
	got := analyzertest.Run(t, unparam.Analyzer, "example.com/p", map[string]string{
		"p.go":      lib,
		"p_test.go": "package p\n\nvar _ = scale(3, 5)\n",
	})
	if len(got) != 1 || got[0].Line != 3 || !strings.Contains(got[0].Message, "b is unused") {
		t.Fatalf("diagnostics = %+v, want only add's b unused on line 3", got)
	}
}

func TestLibraryOnlyUnitIsLeftToTheTestUnit(t *testing.T) {
	got := analyzertest.Run(t, unparam.Analyzer, "example.com/p", map[string]string{"p.go": lib})
	if len(got) != 0 {
		t.Fatalf("library-only unit reported %+v, want none (scale's factor varies in tests)", got)
	}
}
