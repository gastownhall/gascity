// Package revive exposes github.com/mgechev/revive as a nogo analyzer.
// revive is a lint framework with no analysis.Analyzer; this mirrors
// golangci-lint's wrapper: revive's default configuration and the package's
// Go language version.
//
// golangci-lint excludes revive findings by path and message text, which nogo's
// per-analyzer exclude_files cannot express, so the -exclude flag carries those
// rules: entries separated by ";", each "<path regexp>::<text regexp>", where
// the text is "<rule>: <failure>" as golangci-lint prints it.
package revive

import (
	"fmt"
	"go/version"
	"os"
	"regexp"
	"strings"

	goversion "github.com/hashicorp/go-version"
	reviveconfig "github.com/mgechev/revive/config"
	"github.com/mgechev/revive/lint"
	"golang.org/x/tools/go/analysis"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/position"
)

// Analyzer runs revive's default rules.
var Analyzer = &analysis.Analyzer{
	Name: "revive",
	Doc:  "revive's default rule set (golint successor)",
	Run:  run,
}

var excludeFlag string

func init() {
	Analyzer.Flags.StringVar(&excludeFlag, "exclude", "",
		`";"-separated "<path regexp>::<text regexp>" findings to drop`)
}

// Exclusion drops findings whose file path and text both match.
type Exclusion struct {
	Path *regexp.Regexp
	Text *regexp.Regexp
}

// ParseExclusions parses the -exclude flag value.
func ParseExclusions(value string) ([]Exclusion, error) {
	var out []Exclusion
	for _, entry := range strings.Split(value, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		pathRE, textRE, ok := strings.Cut(entry, "::")
		if !ok {
			return nil, fmt.Errorf("revive exclusion %q: want <path regexp>::<text regexp>", entry)
		}
		p, err := regexp.Compile(pathRE)
		if err != nil {
			return nil, fmt.Errorf("revive exclusion %q path: %w", entry, err)
		}
		t, err := regexp.Compile(textRE)
		if err != nil {
			return nil, fmt.Errorf("revive exclusion %q text: %w", entry, err)
		}
		out = append(out, Exclusion{Path: p, Text: t})
	}
	return out, nil
}

func excluded(exclusions []Exclusion, path, text string) bool {
	for _, e := range exclusions {
		if e.Path.MatchString(path) && e.Text.MatchString(text) {
			return true
		}
	}
	return false
}

// newConfig returns revive's default configuration (its default rule set at
// confidence 0.8, what golangci-lint uses when no rules are configured) for
// the given Go language version.
func newConfig(goVersion string) (*lint.Config, []lint.Rule, error) {
	conf, err := reviveconfig.GetConfig("")
	if err != nil {
		return nil, nil, fmt.Errorf("revive default config: %w", err)
	}
	if goVersion != "" {
		v, err := goversion.NewVersion(goVersion)
		if err != nil {
			return nil, nil, fmt.Errorf("revive go version %q: %w", goVersion, err)
		}
		conf.GoVersion = v
	}
	rules, err := reviveconfig.GetLintingRules(conf, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("revive rules: %w", err)
	}
	return conf, rules, nil
}

func run(pass *analysis.Pass) (any, error) {
	exclusions, err := ParseExclusions(excludeFlag)
	if err != nil {
		return nil, err
	}
	conf, rules, err := newConfig(strings.TrimPrefix(version.Lang(pass.Pkg.GoVersion()), "go"))
	if err != nil {
		return nil, err
	}

	var files []string
	for _, f := range pass.Files {
		name := pass.Fset.File(f.Pos()).Name()
		// cgo-generated sources are not the files developers edit.
		if strings.HasSuffix(name, ".go") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		return nil, nil
	}

	linter := lint.New(os.ReadFile, 0)
	failures, err := linter.Lint([][]string{files}, rules, *conf)
	if err != nil {
		return nil, err
	}
	idx := position.NewIndex(pass)
	var internalErr error
	for failure := range failures {
		if failure.IsInternal() {
			if internalErr == nil {
				internalErr = fmt.Errorf("revive: %s", failure.Failure)
			}
			continue
		}
		if failure.Confidence < conf.Confidence {
			continue
		}
		text := fmt.Sprintf("%s: %s", failure.RuleName, failure.Failure)
		if excluded(exclusions, failure.Position.Start.Filename, text) {
			continue
		}
		pos, ok := idx.Pos(failure.Position.Start)
		if !ok {
			continue
		}
		pass.Report(analysis.Diagnostic{Pos: pos, Category: failure.RuleName, Message: text})
	}
	return nil, internalErr
}
