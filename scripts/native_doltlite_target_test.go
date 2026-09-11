package scripts_test

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/doc"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestNativeDoltliteMakeTargetPolicyRejectsOverrides(t *testing.T) {
	const recipe = `$(TEST_ENV) CGO_ENABLED=0 go test -tags gascity_native_beads -run '^TestDoltlite' ./internal/beads -count=1`
	valid := "before:\n\ttrue\n\ntest-native-doltlite-beads:\n\t" + recipe + "\n\nafter:\n\ttrue\n"
	if err := validateNativeDoltliteMakefile(valid); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}

	for name, makefile := range map[string]string{
		"second invocation":            strings.Replace(valid, "\n\nafter:", "\n\tgo test ./internal/beads\n\nafter:", 1),
		"second run flag":              strings.Replace(valid, " -tags gascity_native_beads", " -run='^TestAbsent$' -tags gascity_native_beads", 1),
		"double dash run":              strings.Replace(valid, " -count=1", " -count=1 --run=^TestAbsent$", 1),
		"test binary run":              strings.Replace(valid, " -count=1", " -count=1 -test.run=^TestAbsent$", 1),
		"duplicate rule":               valid + "test-native-doltlite-beads: prerequisite\n\tgo test ./internal/beads\n",
		"multiple target rule":         valid + "alias test-native-doltlite-beads:\n\tgo test ./internal/beads\n",
		"blank separated invocation":   strings.Replace(valid, "\n\nafter:", "\n\n\tgo test ./internal/beads\n\nafter:", 1),
		"comment separated invocation": strings.Replace(valid, "\n\nafter:", "\n\n# still the same recipe\n\tgo test ./internal/beads\n\nafter:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateNativeDoltliteMakefile(makefile); err == nil {
				t.Fatal("override unexpectedly accepted")
			}
		})
	}
}

func TestNativeDoltliteDryRunPolicyRequiresOneExactCommand(t *testing.T) {
	const command = "env -i PATH=... CGO_ENABLED=0 go test -tags gascity_native_beads -run '^TestDoltlite' ./internal/beads -count=1"
	if err := validateNativeDoltliteDryRun(command + "\n"); err != nil {
		t.Fatalf("valid dry-run command rejected: %v", err)
	}
	if err := validateNativeDoltliteDryRun(command + "\ngo test ./internal/beads\n"); err == nil {
		t.Fatal("second expanded command unexpectedly accepted")
	}
}

func TestNativeDoltliteOwnerPolicyRejectsFuzzOwners(t *testing.T) {
	for _, name := range []string{"FuzzDoltliteReadStore"} {
		if !nativeDoltliteFuzzOwner(name) {
			t.Errorf("%s should require an explicit selector policy", name)
		}
	}
	for _, name := range []string{"TestDoltliteReadStore", "BenchmarkDoltliteReadStore", "Fuzzhelper", "ExampleDoltliteReadStore", "testHelper"} {
		if nativeDoltliteFuzzOwner(name) {
			t.Errorf("%s should not be classified as a fuzz owner", name)
		}
	}
}

func TestNativeDoltliteFilePolicyRejectsImplicitPlatformConstraints(t *testing.T) {
	dir := t.TempDir()
	const source = "//go:build gascity_native_beads\n\npackage beads\n"
	for name, wantError := range map[string]bool{
		"doltlite_portable_test.go":      false,
		"doltlite_linux_test.go":         true,
		"doltlite_windows_arm64_test.go": true,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			err := validateNativeDoltliteBuildContext(dir, name)
			if wantError && err == nil {
				t.Fatal("implicit platform constraint unexpectedly accepted")
			}
			if !wantError && err != nil {
				t.Fatalf("portable file rejected: %v", err)
			}
		})
	}
}

func TestNativeDoltliteConstraintIgnoresBlockCommentDirective(t *testing.T) {
	header := "/*\n//go:build gascity_native_beads\n*/\n"
	nativeOnly, err := nativeDoltliteTestConstraint(header)
	if err != nil {
		t.Fatalf("parse block-comment fixture: %v", err)
	}
	if nativeOnly {
		t.Fatal("directive-looking text inside a block comment must not tag the file")
	}
}

func validateNativeDoltliteMakefile(makefile string) error {
	const (
		targetName     = "test-native-doltlite-beads"
		targetLineText = targetName + ":"
		recipe         = `$(TEST_ENV) CGO_ENABLED=0 go test -tags gascity_native_beads -run '^TestDoltlite' ./internal/beads -count=1`
	)

	lines := strings.Split(makefile, "\n")
	targetLine := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 || !slices.Contains(strings.Fields(line[:colon]), targetName) {
			continue
		}
		if targetLine >= 0 || line != targetLineText {
			return fmt.Errorf("target must have exactly one declaration without prerequisites")
		}
		targetLine = i
	}
	if targetLine < 0 {
		return fmt.Errorf("target is missing")
	}
	if targetLine+2 >= len(lines) {
		return fmt.Errorf("target recipe is incomplete")
	}
	if got := lines[targetLine+1]; got != "\t"+recipe {
		return fmt.Errorf("recipe = %q, want exactly %q", got, recipe)
	}
	for _, line := range lines[targetLine+2:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(line, "\t") {
			return fmt.Errorf("target must contain exactly one recipe command")
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		break
	}
	return nil
}

func validateNativeDoltliteDryRun(output string) error {
	const suffix = ` CGO_ENABLED=0 go test -tags gascity_native_beads -run '^TestDoltlite' ./internal/beads -count=1`

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		return fmt.Errorf("expanded to %d commands, want exactly 1", len(lines))
	}
	if !strings.HasSuffix(lines[0], suffix) {
		return fmt.Errorf("expanded command does not end with %q", strings.TrimSpace(suffix))
	}
	return nil
}

func validateNativeDoltliteBuildContext(dir, name string) error {
	for _, target := range []struct {
		goos   string
		goarch string
	}{
		{goos: "linux", goarch: "amd64"},
		{goos: "windows", goarch: "arm64"},
	} {
		context := build.Default
		context.GOOS = target.goos
		context.GOARCH = target.goarch
		context.CgoEnabled = false
		context.BuildTags = []string{"gascity_native_beads"}
		matched, err := context.MatchFile(dir, name)
		if err != nil {
			return fmt.Errorf("match %s/%s build context: %w", target.goos, target.goarch, err)
		}
		if !matched {
			return fmt.Errorf("implicit platform constraint excludes %s/%s", target.goos, target.goarch)
		}
	}
	return nil
}

func assertNativeDoltliteBeadsSelectionMatchesTaggedOwners(t *testing.T, repoRoot string) {
	t.Helper()
	if _, err := nativeDoltliteTestConstraint("// +build gascity_native_beads\n"); err == nil {
		t.Error("legacy-only native test constraint unexpectedly passed")
	}

	beadsDir := filepath.Join(repoRoot, "internal", "beads")
	target := build.Default
	target.CgoEnabled = false
	target.BuildTags = []string{"gascity_native_beads"}
	if err := validateNativeDoltliteOwnerSelection(beadsDir, target); err != nil {
		t.Fatal(err)
	}
}

func validateNativeDoltliteOwnerSelection(beadsDir string, target build.Context) error {
	const selectedTestPrefix = "TestDoltlite"

	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return fmt.Errorf("read internal/beads: %w", err)
	}

	var (
		nativeOwners      []string
		unmatchedNative   []string
		selectedOrdinary  []string
		unsupportedNative []string
		alwaysRunOwners   []string
		policyErrors      []string
	)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(beadsDir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			policyErrors = append(policyErrors, fmt.Sprintf("read %s: %v", path, err))
			continue
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, content, parser.ParseComments)
		if err != nil {
			policyErrors = append(policyErrors, fmt.Sprintf("parse %s: %v", path, err))
			continue
		}
		header := string(content[:fileSet.Position(parsed.Package).Offset])
		nativeOnly, err := nativeDoltliteTestConstraint(header)
		if err != nil {
			policyErrors = append(policyErrors, fmt.Sprintf("%s: %v", entry.Name(), err))
			continue
		}
		if nativeOnly {
			if err := validateNativeDoltliteBuildContext(beadsDir, entry.Name()); err != nil {
				policyErrors = append(policyErrors, fmt.Sprintf("%s: %v", entry.Name(), err))
			}
		}
		included, err := target.MatchFile(beadsDir, entry.Name())
		if err != nil {
			policyErrors = append(policyErrors, fmt.Sprintf("match target build context for %s: %v", entry.Name(), err))
			continue
		}
		if !included {
			continue
		}
		if nativeOnly {
			for _, example := range doc.Examples(parsed) {
				if example.Output != "" || example.EmptyOutput {
					unsupportedNative = append(unsupportedNative, entry.Name()+":Example"+example.Name)
				}
			}
		}
		for _, declaration := range parsed.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			owner := entry.Name() + ":" + fn.Name.Name
			if fn.Name.Name == "TestMain" && goTestFunc(fn, "M") {
				alwaysRunOwners = append(alwaysRunOwners, owner)
				continue
			}
			if nativeOnly && nativeDoltliteFuzzOwner(fn.Name.Name) {
				unsupportedNative = append(unsupportedNative, owner)
				continue
			}
			if !goTestOwnerName(fn.Name.Name, "Test") {
				continue
			}
			selected := strings.HasPrefix(fn.Name.Name, selectedTestPrefix)
			switch {
			case nativeOnly:
				nativeOwners = append(nativeOwners, owner)
				if !selected {
					unmatchedNative = append(unmatchedNative, owner)
				}
			case selected:
				selectedOrdinary = append(selectedOrdinary, owner)
			}
		}
	}

	sort.Strings(nativeOwners)
	sort.Strings(unmatchedNative)
	sort.Strings(selectedOrdinary)
	sort.Strings(unsupportedNative)
	sort.Strings(alwaysRunOwners)
	if len(nativeOwners) == 0 {
		policyErrors = append(policyErrors, "no gascity_native_beads test owners found")
	}
	if len(unmatchedNative) != 0 {
		policyErrors = append(policyErrors, fmt.Sprintf("native DoltLite owners excluded by -run '^%s': %s", selectedTestPrefix, strings.Join(unmatchedNative, ", ")))
	}
	if len(selectedOrdinary) != 0 {
		policyErrors = append(policyErrors, fmt.Sprintf("ordinary internal/beads owners selected by -run '^%s': %s", selectedTestPrefix, strings.Join(selectedOrdinary, ", ")))
	}
	if len(unsupportedNative) != 0 {
		policyErrors = append(policyErrors, fmt.Sprintf("native DoltLite default-run owners need an explicit selector policy: %s", strings.Join(unsupportedNative, ", ")))
	}
	if len(alwaysRunOwners) != 0 {
		policyErrors = append(policyErrors, fmt.Sprintf("internal/beads TestMain runs regardless of -run selector: %s", strings.Join(alwaysRunOwners, ", ")))
	}
	if len(policyErrors) != 0 {
		return fmt.Errorf("native DoltLite target policy:\n%s", strings.Join(policyErrors, "\n"))
	}
	return nil
}

func nativeDoltliteFuzzOwner(name string) bool {
	return goTestOwnerName(name, "Fuzz")
}

func goTestOwnerName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(r)
}

// goTestFunc mirrors cmd/go's syntactic test-harness signature check.
func goTestFunc(fn *ast.FuncDecl, argument string) bool {
	if fn.Type.TypeParams != nil && len(fn.Type.TypeParams.List) != 0 {
		return false
	}
	if fn.Type.Results != nil && len(fn.Type.Results.List) != 0 {
		return false
	}
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) > 1 {
		return false
	}
	pointer, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch parameter := pointer.X.(type) {
	case *ast.Ident:
		return parameter.Name == argument
	case *ast.SelectorExpr:
		return parameter.Sel.Name == argument
	default:
		return false
	}
}

func nativeDoltliteTestConstraint(header string) (bool, error) {
	const nativeBuildTag = "gascity_native_beads"

	parsedHeader, err := parser.ParseFile(
		token.NewFileSet(),
		"native_doltlite_header.go",
		header+"\npackage beads\n",
		parser.ParseComments,
	)
	if err != nil {
		return false, fmt.Errorf("parse source header: %w", err)
	}

	var (
		goBuildExpr  constraint.Expr
		legacyNative bool
	)
	for _, group := range parsedHeader.Comments {
		for _, comment := range group.List {
			line := strings.TrimSpace(comment.Text)
			switch {
			case constraint.IsGoBuild(line):
				if goBuildExpr != nil {
					return false, fmt.Errorf("multiple //go:build constraints")
				}
				parsed, err := constraint.Parse(line)
				if err != nil {
					return false, fmt.Errorf("parse build constraint: %w", err)
				}
				goBuildExpr = parsed
			case constraint.IsPlusBuild(line):
				parsed, err := constraint.Parse(line)
				if err != nil {
					return false, fmt.Errorf("parse legacy build constraint: %w", err)
				}
				legacyNative = legacyNative || constraintContainsPositiveTag(parsed, nativeBuildTag, false)
			}
		}
	}
	if goBuildExpr == nil && legacyNative {
		return false, fmt.Errorf("legacy-only native test constraint; add //go:build %s", nativeBuildTag)
	}
	if tag, ok := goBuildExpr.(*constraint.TagExpr); ok && tag.Tag == nativeBuildTag {
		return true, nil
	}
	if constraintContainsPositiveTag(goBuildExpr, nativeBuildTag, false) {
		return false, fmt.Errorf("compound native test constraint; use only //go:build %s", nativeBuildTag)
	}
	return false, nil
}

func constraintContainsPositiveTag(expr constraint.Expr, want string, negated bool) bool {
	switch expr := expr.(type) {
	case *constraint.TagExpr:
		return expr.Tag == want && !negated
	case *constraint.NotExpr:
		return constraintContainsPositiveTag(expr.X, want, !negated)
	case *constraint.AndExpr:
		return constraintContainsPositiveTag(expr.X, want, negated) || constraintContainsPositiveTag(expr.Y, want, negated)
	case *constraint.OrExpr:
		return constraintContainsPositiveTag(expr.X, want, negated) || constraintContainsPositiveTag(expr.Y, want, negated)
	default:
		return false
	}
}

func TestNativeDoltliteOwnerPolicyRejectsRootPackageTestMain(t *testing.T) {
	for name, source := range map[string]string{
		"ordinary": `package beads

import "testing"

func TestMain(m *testing.M) {}
`,
		"ordinary unnamed parameter": `package beads

import "testing"

func TestMain(*testing.M) {}
`,
		"ordinary selector alias": `package beads

import testpkg "testing"

func TestMain(m *testpkg.M) {}
`,
		"native tagged": `//go:build gascity_native_beads

package beads

import "testing"

func TestMain(m *testing.M) {}
`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeNativeDoltliteOwnerFixture(t, dir)
			writeNativeDoltlitePolicyFixture(t, filepath.Join(dir, "testmain_test.go"), source)

			err := validateNativeDoltliteOwnerSelection(dir, nativeDoltlitePolicyTestContext())
			if err == nil || !strings.Contains(err.Error(), "TestMain runs regardless of -run selector") {
				t.Fatalf("TestMain policy error = %v, want always-run rejection", err)
			}
		})
	}
}

func TestNativeDoltliteOwnerPolicyUsesTargetBuildContext(t *testing.T) {
	tests := map[string]struct {
		name      string
		source    string
		wantError bool
	}{
		"target matching ordinary owner is rejected": {
			name: "ordinary_test.go",
			source: `package beads

import "testing"

func TestDoltliteOrdinaryLeak(t *testing.T) {}
`,
			wantError: true,
		},
		"integration owner is excluded": {
			name: "integration_test.go",
			source: `//go:build integration

package beads

import "testing"

func TestDoltliteIntegrationOnly(t *testing.T) {}
`,
		},
		"negative native tag is excluded": {
			name: "negative_test.go",
			source: `//go:build !gascity_native_beads

package beads

import "testing"

func TestDoltliteWithoutNative(t *testing.T) {}
`,
		},
		"other platform owner is excluded": {
			name: "ordinary_windows_test.go",
			source: `package beads

import "testing"

func TestDoltliteWindowsOnly(t *testing.T) {}
`,
		},
		"ordinary TestMain-shaped test is selector excluded": {
			name: "ordinary_test.go",
			source: `package beads

import "testing"

func TestMain(t *testing.T) {}
`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeNativeDoltliteOwnerFixture(t, dir)
			writeNativeDoltlitePolicyFixture(t, filepath.Join(dir, tt.name), tt.source)

			err := validateNativeDoltliteOwnerSelection(dir, nativeDoltlitePolicyTestContext())
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "ordinary internal/beads owners selected") {
					t.Fatalf("owner policy error = %v, want ordinary-owner rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("excluded owner affected native target policy: %v", err)
			}
		})
	}
}

func TestNativeDoltliteConstraintTreatsNegativeTagAsExcluded(t *testing.T) {
	nativeOnly, err := nativeDoltliteTestConstraint("//go:build !gascity_native_beads\n")
	if err != nil {
		t.Fatalf("negative native constraint rejected: %v", err)
	}
	if nativeOnly {
		t.Fatal("negative native constraint classified as native-only")
	}
}

func TestNativeDoltliteOwnerPolicyOnlyRejectsRunnableExamples(t *testing.T) {
	for name, tt := range map[string]struct {
		source    string
		wantError bool
	}{
		"documentation only": {
			source: `//go:build gascity_native_beads

package beads

func ExampleDoltliteDocumentation() {}
`,
		},
		"registered empty output": {
			source: `//go:build gascity_native_beads

package beads

func ExampleDoltliteRunnable() {
	// Output:
}
`,
			wantError: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeNativeDoltliteOwnerFixture(t, dir)
			writeNativeDoltlitePolicyFixture(t, filepath.Join(dir, "example_test.go"), tt.source)

			err := validateNativeDoltliteOwnerSelection(dir, nativeDoltlitePolicyTestContext())
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "ExampleDoltliteRunnable") {
					t.Fatalf("example policy error = %v, want runnable-example rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("documentation-only example treated as runnable: %v", err)
			}
		})
	}
}

func nativeDoltlitePolicyTestContext() build.Context {
	context := build.Default
	context.GOOS = "linux"
	context.GOARCH = "amd64"
	context.CgoEnabled = false
	context.BuildTags = []string{"gascity_native_beads"}
	return context
}

func writeNativeDoltliteOwnerFixture(t *testing.T, dir string) {
	t.Helper()
	writeNativeDoltlitePolicyFixture(t, filepath.Join(dir, "doltlite_test.go"), `//go:build gascity_native_beads

package beads

import "testing"

func TestDoltliteFixture(t *testing.T) {}
`)
}

func writeNativeDoltlitePolicyFixture(t *testing.T, path, source string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
