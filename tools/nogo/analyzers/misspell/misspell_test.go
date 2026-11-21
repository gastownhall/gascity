package misspell_test

import (
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/misspell"
)

// The fixture words are assembled from halves so this file does not itself
// trip the analyzer it tests.
var (
	receive  = "rec" + "ieve"
	occurred = "occ" + "ured"
	color    = "col" + "our"
)

func TestReportsMisspellingsAnywhereInTheFile(t *testing.T) {
	got := analyzertest.Run(t, misspell.Analyzer, "example.com/p", map[string]string{
		"p.go": `// Package p is fine.
package p

// ` + receive + ` is misspelled in a comment.
func f() string {
	return "an ` + occurred + ` string"
}

// ` + color + ` is British; the US locale flags it.
var c = 1
`,
	})
	want := []analyzertest.Diagnostic{
		{File: "p.go", Line: 4, Message: "`" + receive + "` is a misspelling of `receive`"},
		{File: "p.go", Line: 6, Message: "`" + occurred + "` is a misspelling of `occurred`"},
		{File: "p.go", Line: 9, Message: "`" + color + "` is a misspelling of `color`"},
	}
	if len(got) != len(want) {
		t.Fatalf("diagnostics = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("diagnostic %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
