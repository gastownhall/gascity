package scripts_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const runtimeTmuxManifestRelativePath = "scripts/runtime-tmux-tests.manifest"

func TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory(t *testing.T) {
	repo := repoRoot(t)
	manifest := parseRuntimeTmuxManifest(t, filepath.Join(repo, runtimeTmuxManifestRelativePath))
	dir := filepath.Join(repo, "internal", "runtime", "tmux")
	declared := discoverRuntimeTmuxTests(t, dir, "linux", true)

	if drift := runtimeTmuxManifestDrift(manifest, declared); len(drift) != 0 {
		t.Fatalf("runtime-tmux manifest drift:\n%s\nupdate %s", strings.Join(drift, "\n"), runtimeTmuxManifestRelativePath)
	}
	if got, want := len(manifest), 326; got != want {
		t.Fatalf("runtime-tmux manifest contains %d tests, want %d", got, want)
	}

	untagged := discoverRuntimeTmuxTests(t, dir, "linux", false)
	if got, want := len(untagged), 218; got != want {
		t.Fatalf("runtime-tmux untagged inventory contains %d tests, want %d", got, want)
	}
	if got, want := len(declared)-len(untagged), 108; got != want {
		t.Fatalf("runtime-tmux integration-only inventory contains %d tests, want %d", got, want)
	}
}

func TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce(t *testing.T) {
	manifest := parseRuntimeTmuxManifest(t, filepath.Join(repoRoot(t), runtimeTmuxManifestRelativePath))
	wantShardCounts := []int{55, 55, 54, 54, 54, 54}
	seen := make(map[string]int, len(manifest))

	for shardIndex := 0; shardIndex < len(wantShardCounts); shardIndex++ {
		count := 0
		for index, testName := range manifest {
			if index%len(wantShardCounts) == shardIndex {
				seen[testName]++
				count++
			}
		}
		if count != wantShardCounts[shardIndex] {
			t.Errorf("runtime-tmux shard %d contains %d tests, want %d", shardIndex+1, count, wantShardCounts[shardIndex])
		}
	}

	for _, testName := range manifest {
		if seen[testName] != 1 {
			t.Errorf("runtime-tmux test %s assigned %d times, want exactly once", testName, seen[testName])
		}
	}
}

func TestRuntimeTmuxManifestDiscoveryUsesCanonicalLinuxPlatform(t *testing.T) {
	context := canonicalRuntimeTmuxBuildContext("linux", true)
	if context.GOOS != "linux" || context.GOARCH != "amd64" || !slices.Equal(context.BuildTags, []string{"integration"}) {
		t.Fatalf("runtime-tmux manifest build target = %s/%s tags %q, want linux/amd64 with integration", context.GOOS, context.GOARCH, context.BuildTags)
	}

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "linux_integration_test.go"), `//go:build integration && linux

package fixture

import "testing"

func TestLinux(t *testing.T) {}
`)
	writeTestFile(t, filepath.Join(dir, "darwin_integration_test.go"), `//go:build integration && darwin

package fixture

import "testing"

func TestDarwin(t *testing.T) {}
`)

	if got, want := discoverRuntimeTmuxTests(t, dir, "linux", true), []string{"TestLinux"}; !slices.Equal(got, want) {
		t.Fatalf("linux runtime-tmux inventory = %q, want %q", got, want)
	}
	if got, want := discoverRuntimeTmuxTests(t, dir, "darwin", true), []string{"TestDarwin"}; !slices.Equal(got, want) {
		t.Fatalf("darwin runtime-tmux inventory = %q, want %q", got, want)
	}
}

func TestRuntimeTmuxManifestDiscoveryDistinguishesTestMainHarness(t *testing.T) {
	t.Run("test harness", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "main_test.go"), `package fixture

import testpkg "testing"

func TestMain(m *testpkg.M) {}
func TestMainOrdinary(t *testpkg.T) {}
`)

		if got, want := discoverRuntimeTmuxTests(t, dir, "linux", true), []string{"TestMainOrdinary"}; !slices.Equal(got, want) {
			t.Fatalf("runtime-tmux tests = %q, want harness excluded and ordinary test %q", got, want)
		}
	})

	t.Run("ordinary TestMain", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "ordinary_test.go"), `package fixture

import . "testing"

func TestMain(t *T) {}
`)

		if got, want := discoverRuntimeTmuxTests(t, dir, "linux", true), []string{"TestMain"}; !slices.Equal(got, want) {
			t.Fatalf("runtime-tmux tests = %q, want ordinary TestMain included as %q", got, want)
		}
	})
}

func TestRuntimeTmuxManifestDriftDiagnostics(t *testing.T) {
	t.Run("stale and unassigned", func(t *testing.T) {
		manifest := []string{"TestKept", "TestStale"}
		declared := []string{"TestKept", "TestNew"}
		want := []string{
			"unassigned runtime-tmux test: TestNew",
			"stale runtime-tmux manifest entry: TestStale",
		}
		if got := runtimeTmuxManifestDrift(manifest, declared); !slices.Equal(got, want) {
			t.Fatalf("runtime-tmux drift diagnostics = %q, want %q", got, want)
		}
	})

	t.Run("reordered", func(t *testing.T) {
		manifest := []string{"TestBeta", "TestAlpha"}
		declared := []string{"TestAlpha", "TestBeta"}
		want := []string{
			"reordered runtime-tmux manifest entry 1: got TestBeta, want TestAlpha",
			"reordered runtime-tmux manifest entry 2: got TestAlpha, want TestBeta",
		}
		if got := runtimeTmuxManifestDrift(manifest, declared); !slices.Equal(got, want) {
			t.Fatalf("runtime-tmux reorder diagnostics = %q, want %q", got, want)
		}
	})
}

func parseRuntimeTmuxManifest(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read runtime-tmux manifest: %v", err)
	}

	var tests []string
	for lineNumber, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line != rawLine || !isSafeManifestTestName(line) {
			t.Fatalf("invalid runtime-tmux manifest entry on line %d: %q", lineNumber+1, rawLine)
		}
		tests = append(tests, line)
	}
	if len(tests) == 0 {
		t.Fatal("runtime-tmux manifest contains no tests")
	}
	return tests
}

func isSafeManifestTestName(name string) bool {
	if !isGoTestName(name) {
		return false
	}
	for _, character := range name {
		if character != '_' && (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
			return false
		}
	}
	return true
}

func discoverRuntimeTmuxTests(t *testing.T, dir, goos string, integration bool) []string {
	t.Helper()
	context := canonicalRuntimeTmuxBuildContext(goos, integration)
	pkg, err := context.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("load runtime-tmux package for %s/amd64 (integration=%t): %v", goos, integration, err)
	}

	testFiles := append(slices.Clone(pkg.TestGoFiles), pkg.XTestGoFiles...)
	fileSet := token.NewFileSet()
	var tests []string
	for _, name := range testFiles {
		path := filepath.Join(dir, name)
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse runtime-tmux test file %s: %v", name, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !isGoTestName(function.Name.Name) {
				continue
			}
			if testingParameterKind(parsed, function) == "T" {
				tests = append(tests, function.Name.Name)
			}
		}
	}
	return tests
}

func canonicalRuntimeTmuxBuildContext(goos string, integration bool) build.Context {
	context := build.Default
	context.GOOS = goos
	context.GOARCH = "amd64"
	context.Compiler = "gc"
	context.CgoEnabled = true
	context.BuildTags = nil
	if integration {
		context.BuildTags = []string{"integration"}
	}
	context.ToolTags = nil
	return context
}

func testingParameterKind(file *ast.File, function *ast.FuncDecl) string {
	if function.Type.TypeParams != nil && len(function.Type.TypeParams.List) != 0 {
		return ""
	}
	if function.Type.Results != nil && len(function.Type.Results.List) != 0 {
		return ""
	}
	if function.Type.Params == nil || len(function.Type.Params.List) != 1 {
		return ""
	}
	parameter := function.Type.Params.List[0]
	if len(parameter.Names) > 1 {
		return ""
	}
	pointer, ok := parameter.Type.(*ast.StarExpr)
	if !ok {
		return ""
	}

	testingAliases := make(map[string]struct{})
	dotImported := false
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || path != "testing" {
			continue
		}
		if imported.Name == nil {
			testingAliases["testing"] = struct{}{}
			continue
		}
		if imported.Name.Name == "." {
			dotImported = true
			continue
		}
		if imported.Name.Name != "_" {
			testingAliases[imported.Name.Name] = struct{}{}
		}
	}

	switch target := pointer.X.(type) {
	case *ast.Ident:
		if dotImported && (target.Name == "T" || target.Name == "M") {
			return target.Name
		}
	case *ast.SelectorExpr:
		qualifier, ok := target.X.(*ast.Ident)
		if !ok {
			return ""
		}
		if _, ok := testingAliases[qualifier.Name]; ok && (target.Sel.Name == "T" || target.Sel.Name == "M") {
			return target.Sel.Name
		}
	}
	return ""
}

func runtimeTmuxManifestDrift(manifest, declared []string) []string {
	manifestSet := make(map[string]struct{}, len(manifest))
	declaredSet := make(map[string]struct{}, len(declared))
	var drift []string
	for _, testName := range manifest {
		if _, duplicate := manifestSet[testName]; duplicate {
			drift = append(drift, "duplicate runtime-tmux manifest entry: "+testName)
		}
		manifestSet[testName] = struct{}{}
	}
	for _, testName := range declared {
		declaredSet[testName] = struct{}{}
		if _, assigned := manifestSet[testName]; !assigned {
			drift = append(drift, "unassigned runtime-tmux test: "+testName)
		}
	}
	for _, testName := range manifest {
		if _, exists := declaredSet[testName]; !exists {
			drift = append(drift, "stale runtime-tmux manifest entry: "+testName)
		}
	}
	if len(drift) == 0 && !slices.Equal(manifest, declared) {
		for index := range manifest {
			if manifest[index] != declared[index] {
				drift = append(drift, "reordered runtime-tmux manifest entry "+strconv.Itoa(index+1)+": got "+manifest[index]+", want "+declared[index])
			}
		}
	}
	return drift
}
