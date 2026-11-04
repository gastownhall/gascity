package gocritic_test

import (
	"strings"
	"testing"

	gocriticlinter "github.com/go-critic/go-critic/linter"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/gocritic"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
)

func TestReportsDefaultCheckers(t *testing.T) {
	got := analyzertest.Run(t, gocritic.Analyzer, "example.com/p", map[string]string{
		"p.go": `package p

func f(x int) int {
	x = x + 1
	return x
}
`,
	})
	if len(got) != 1 || got[0].Line != 4 || !strings.HasPrefix(got[0].Message, "assignOp:") {
		t.Fatalf("diagnostics = %+v, want one assignOp finding on line 4", got)
	}
}

func TestDefaultSetExcludesNonDefaultTags(t *testing.T) {
	for _, tag := range []string{
		gocriticlinter.ExperimentalTag,
		gocriticlinter.OpinionatedTag,
		gocriticlinter.PerformanceTag,
		gocriticlinter.SecurityTag,
	} {
		info := &gocriticlinter.CheckerInfo{Tags: []string{gocriticlinter.StyleTag, tag}}
		if gocritic.EnabledByDefault(info) {
			t.Errorf("checker tagged %s is enabled by default", tag)
		}
	}
	if !gocritic.EnabledByDefault(&gocriticlinter.CheckerInfo{Tags: []string{gocriticlinter.DiagnosticTag}}) {
		t.Error("diagnostic checker is not enabled by default")
	}
}
