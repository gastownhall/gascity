package errorlint_test

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/errorlint"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
)

func TestGolangciDefaultsEnabled(t *testing.T) {
	for _, flag := range []string{"errorf", "errorf-multi", "asserts", "comparison"} {
		if got := errorlint.Analyzer.Flags.Lookup(flag).Value.String(); got != "true" {
			t.Errorf("-%s = %s, want true (golangci-lint default)", flag, got)
		}
	}
}

func TestReportsSentinelComparison(t *testing.T) {
	got := analyzertest.Run(t, errorlint.Analyzer, "example.com/p", map[string]string{
		"p.go": `package p

type sentinel struct{}

func (sentinel) Error() string { return "sentinel" }

var errSentinel error = sentinel{}

func is(err error) bool { return err == errSentinel }
`,
	})
	if len(got) != 1 || got[0].Line != 9 || !strings.Contains(got[0].Message, "comparing with ==") {
		t.Fatalf("diagnostics = %+v, want a comparison finding on line 9", got)
	}
}
