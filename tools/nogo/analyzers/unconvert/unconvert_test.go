package unconvert_test

import (
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/unconvert"
)

func TestReportsRedundantConversion(t *testing.T) {
	got := analyzertest.Run(t, unconvert.Analyzer, "example.com/p", map[string]string{
		"p.go": `package p

type T int

func f(x T, y int) (T, T) {
	return T(x), T(y)
}
`,
	})
	if len(got) != 1 || got[0].Line != 6 || got[0].Message != "unnecessary conversion" {
		t.Fatalf("diagnostics = %+v, want one unnecessary conversion on line 6", got)
	}
}
