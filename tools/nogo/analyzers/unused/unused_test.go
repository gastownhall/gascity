package unused_test

import (
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/unused"
)

const lib = `package p

// Exported is used by importers.
func Exported() int { return used() }

func used() int { return 1 }

func testOnly() int { return 2 }

func dead() {}

type deadType struct{}
`

func TestTestUnitReportsUnusedObjectsCountingTestUses(t *testing.T) {
	got := analyzertest.Run(t, unused.Analyzer, "example.com/p", map[string]string{
		"p.go":      lib,
		"p_test.go": "package p\n\nvar _ = testOnly()\n",
	})
	want := []string{"func dead is unused", "type deadType is unused"}
	if len(got) != len(want) {
		t.Fatalf("diagnostics = %+v, want %v", got, want)
	}
	for i, msg := range want {
		if got[i].Message != msg {
			t.Errorf("diagnostic %d = %q, want %q", i, got[i].Message, msg)
		}
	}
}

func TestLibraryOnlyUnitIsLeftToTheTestUnit(t *testing.T) {
	got := analyzertest.Run(t, unused.Analyzer, "example.com/p", map[string]string{"p.go": lib})
	if len(got) != 0 {
		t.Fatalf("library-only unit reported %+v, want none (testOnly would be a false positive)", got)
	}
}
