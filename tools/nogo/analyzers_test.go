package nogo_test

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gordonklaus/ineffassign/pkg/ineffassign"
	"github.com/kisielk/errcheck/errcheck"
	"golang.org/x/tools/go/analysis"
	"honnef.co/go/tools/analysis/lint"
	"honnef.co/go/tools/quickfix"
	"honnef.co/go/tools/simple"
	"honnef.co/go/tools/staticcheck"
	"honnef.co/go/tools/stylecheck"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/errorlint"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/gocritic"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/misspell"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/revive"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/runtimelease"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/testrelax"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/unconvert"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/unparam"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/unused"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/v2purity"
)

// readFile reads a file of this package, from runfiles under Bazel or from the
// package directory under `go test`.
func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}

// bzlList returns the quoted entries of the Starlark list assigned to name.
func bzlList(t *testing.T, bzl, name string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?ms)^` + name + ` = \[(.*?)\]`).FindStringSubmatch(bzl)
	if m == nil {
		t.Fatalf("analyzers.bzl has no list %s", name)
	}
	var out []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, q[1])
	}
	return out
}

// TestStaticcheckSetMatchesGolangciDefault pins the staticcheck check lists in
// analyzers.bzl to golangci-lint v2's default staticcheck configuration
// (checks = all minus ST1000/ST1003/ST1016/ST1020/ST1021/ST1022) for the
// honnef.co/go/tools version in go.mod, so an upgrade that adds or removes a
// check fails here instead of silently changing the gate.
func TestStaticcheckSetMatchesGolangciDefault(t *testing.T) {
	bzl := readFile(t, "analyzers.bzl")
	excluded := bzlList(t, bzl, "STATICCHECK_EXCLUDED")
	for name, analyzers := range map[string][]*lint.Analyzer{
		"STATICCHECK": staticcheck.Analyzers,
		"SIMPLE":      simple.Analyzers,
		"STYLECHECK":  stylecheck.Analyzers,
		"QUICKFIX":    quickfix.Analyzers,
	} {
		var want []string
		for _, a := range analyzers {
			check := strings.ToLower(a.Analyzer.Name)
			if !slices.Contains(excluded, check) {
				want = append(want, check)
			}
		}
		sort.Strings(want)
		got := bzlList(t, bzl, name)
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s in analyzers.bzl = %v\nwant %v", name, got, want)
		}
	}
}

func wrapped() []*analysis.Analyzer {
	return []*analysis.Analyzer{
		errorlint.Analyzer,
		gocritic.Analyzer,
		misspell.Analyzer,
		revive.Analyzer,
		unconvert.Analyzer,
		unparam.Analyzer,
		unused.Analyzer,
		runtimelease.Analyzer,
		v2purity.Analyzer,
		testrelax.Analyzer,
		errcheck.Analyzer,
		ineffassign.Analyzer,
	}
}

// TestAnalyzersValidate checks the non-staticcheck analyzers are well formed
// and carry the golangci-lint linter names that //nolint:<name> directives in
// the tree refer to (nogo matches //nolint names against Analyzer.Name).
func TestAnalyzersValidate(t *testing.T) {
	all := wrapped()
	if err := analysis.Validate(all); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range all {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	want := []string{"errcheck", "errorlint", "gocritic", "ineffassign", "misspell", "revive", "runtimelease", "testrelax", "unconvert", "unparam", "unused", "v2purity"}
	if !slices.Equal(names, want) {
		t.Errorf("analyzer names = %v, want %v", names, want)
	}
}

// TestConfigNamesKnownAnalyzers keeps config.json from configuring an analyzer
// the suite does not run (nogo would silently ignore the entry).
func TestConfigNamesKnownAnalyzers(t *testing.T) {
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFile(t, "config.json")), &cfg); err != nil {
		t.Fatalf("config.json: %v", err)
	}
	known := map[string]bool{"_base": true}
	for _, a := range wrapped() {
		known[a.Name] = true
	}
	for name := range cfg {
		if !known[name] {
			t.Errorf("config.json configures unknown analyzer %q", name)
		}
	}
}

// TestReviveExclusionsParse keeps the revive -exclude value in config.json
// well formed.
func TestReviveExclusionsParse(t *testing.T) {
	var cfg struct {
		Revive struct {
			AnalyzerFlags map[string]string `json:"analyzer_flags"`
		} `json:"revive"`
	}
	if err := json.Unmarshal([]byte(readFile(t, "config.json")), &cfg); err != nil {
		t.Fatalf("config.json: %v", err)
	}
	ex, err := revive.ParseExclusions(cfg.Revive.AnalyzerFlags["exclude"])
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) == 0 {
		t.Fatal("config.json revive exclude is empty")
	}
}
