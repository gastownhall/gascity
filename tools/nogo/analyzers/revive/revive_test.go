package revive_test

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/revive"
)

const stutter = `// Package widget makes widgets.
package widget

// WidgetMaker makes widgets.
type WidgetMaker struct{}

func Undocumented() {}
`

func TestReportsDefaultRules(t *testing.T) {
	got := analyzertest.Run(t, revive.Analyzer, "example.com/widget", map[string]string{"widget.go": stutter})
	var msgs []string
	for _, d := range got {
		msgs = append(msgs, d.Message)
	}
	joined := strings.Join(msgs, "\n")
	for _, want := range []string{"stutters", "exported function Undocumented should have comment"} {
		if !strings.Contains(joined, want) {
			t.Errorf("diagnostics %q lack %q", joined, want)
		}
	}
}

func TestExcludeDropsMatchingPathAndText(t *testing.T) {
	if err := revive.Analyzer.Flags.Set("exclude", "example.com/widget/::stutters"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = revive.Analyzer.Flags.Set("exclude", "") })
	got := analyzertest.Run(t, revive.Analyzer, "example.com/widget", map[string]string{"widget.go": stutter})
	if len(got) != 1 || !strings.Contains(got[0].Message, "Undocumented") {
		t.Fatalf("diagnostics = %+v, want only the Undocumented finding", got)
	}
}

func TestParseExclusionsRejectsMalformedEntries(t *testing.T) {
	for _, value := range []string{"no-separator", "(::x", "x::("} {
		if _, err := revive.ParseExclusions(value); err == nil {
			t.Errorf("ParseExclusions(%q) succeeded, want error", value)
		}
	}
}
