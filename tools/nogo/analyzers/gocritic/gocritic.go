// Package gocritic exposes go-critic as a nogo analyzer named "gocritic" with
// golangci-lint's default checker set: every checker that is not tagged
// experimental, opinionated, performance or security. go-critic's own
// checkers/analyzer package registers as "ruleguard" and enables security
// checks, so it does not match golangci-lint's behavior or its
// //nolint:gocritic directives.
package gocritic

import (
	"fmt"
	"go/ast"
	"go/types"
	"go/version"
	"runtime"
	"strings"
	"sync"

	"github.com/go-critic/go-critic/checkers"
	gocritic "github.com/go-critic/go-critic/linter"
	"golang.org/x/tools/go/analysis"
)

// Analyzer runs go-critic's default (stable, non-opinionated) checkers.
var Analyzer = &analysis.Analyzer{
	Name: "gocritic",
	Doc:  "go-critic diagnostics with golangci-lint's default checker set",
	Run:  run,
}

var (
	initOnce sync.Once
	initErr  error
	sizes    = types.SizesFor("gc", runtime.GOARCH)
)

// EnabledByDefault reports whether golangci-lint enables a checker by default.
func EnabledByDefault(info *gocritic.CheckerInfo) bool {
	return !info.HasTag(gocritic.ExperimentalTag) &&
		!info.HasTag(gocritic.OpinionatedTag) &&
		!info.HasTag(gocritic.PerformanceTag) &&
		!info.HasTag(gocritic.SecurityTag)
}

func run(pass *analysis.Pass) (any, error) {
	initOnce.Do(func() { initErr = checkers.InitEmbeddedRules() })
	if initErr != nil {
		return nil, fmt.Errorf("gocritic: loading embedded rules: %w", initErr)
	}

	ctx := gocritic.NewContext(pass.Fset, sizes)
	ver, err := gocritic.ParseGoVersion(version.Lang(pass.Pkg.GoVersion()))
	if err != nil {
		return nil, fmt.Errorf("gocritic: package Go version: %w", err)
	}
	ctx.GoVersion = ver
	var enabled []*gocritic.Checker
	needFileInfo := false
	for _, info := range gocritic.GetCheckersInfo() {
		if !EnabledByDefault(info) {
			continue
		}
		c, err := gocritic.NewChecker(ctx, info)
		if err != nil {
			return nil, fmt.Errorf("gocritic: creating %s: %w", info.Name, err)
		}
		enabled = append(enabled, c)
		if strings.EqualFold(info.Name, "importShadow") {
			needFileInfo = true
		}
	}
	ctx.SetPackageInfo(pass.TypesInfo, pass.Pkg)

	for _, f := range pass.Files {
		if needFileInfo {
			ctx.SetFileInfo(f.Name.Name, f)
		}
		report(pass, f, enabled)
	}
	return nil, nil
}

func report(pass *analysis.Pass, f *ast.File, enabled []*gocritic.Checker) {
	for _, c := range enabled {
		for _, warn := range c.Check(f) {
			diag := analysis.Diagnostic{
				Pos:      warn.Pos,
				Category: c.Info.Name,
				Message:  fmt.Sprintf("%s: %s", c.Info.Name, warn.Text),
			}
			if warn.HasQuickFix() {
				diag.SuggestedFixes = []analysis.SuggestedFix{{
					Message: "suggested replacement",
					TextEdits: []analysis.TextEdit{{
						Pos:     warn.Suggestion.From,
						End:     warn.Suggestion.To,
						NewText: warn.Suggestion.Replacement,
					}},
				}}
			}
			pass.Report(diag)
		}
	}
}
