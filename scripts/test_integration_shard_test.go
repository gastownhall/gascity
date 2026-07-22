package scripts_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestCmdGCIntegrationShardRunsOnlyIntegrationManifest(t *testing.T) {
	repo := repoRoot(t)
	manifest := parseCmdGCIntegrationManifest(t, filepath.Join(repo, "scripts", "test-integration-shard"))
	fixture := newIntegrationShardFixture(t)

	out, err := fixture.run(t)
	if err != nil {
		t.Fatalf("test-integration-shard failed: %v\n%s", err, out)
	}

	captured, err := os.ReadFile(fixture.capturePath)
	if err != nil {
		t.Fatalf("read captured go invocation: %v", err)
	}
	encodedInvocations := strings.TrimSuffix(string(captured), "\x00\x00")
	invocations := strings.Split(encodedInvocations, "\x00\x00")
	if len(invocations) != 1 {
		t.Fatalf("go test invoked %d times, want only the final tagged manifest command:\n%s", len(invocations), captured)
	}
	got := strings.Split(invocations[0], "\x00")
	want := []string{
		"test",
		"-tags", "integration",
		"-timeout", "17s",
		"./cmd/gc",
		"-run", "^(" + strings.Join(manifest, "|") + ")$",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("go test argv = %q, want exact tagged manifest command %q", got, want)
	}
}

func TestCmdGCIntegrationManifestMatchesTaggedDeclarations(t *testing.T) {
	repo := repoRoot(t)
	manifest := parseCmdGCIntegrationManifest(t, filepath.Join(repo, "scripts", "test-integration-shard"))
	declared := discoverCmdGCIntegrationTests(t, filepath.Join(repo, "cmd", "gc"))

	if drift := cmdGCIntegrationManifestDrift(manifest, declared); len(drift) != 0 {
		t.Fatalf("cmd/gc integration manifest drift:\n%s\nupdate cmd_gc_integration_tests in scripts/test-integration-shard", strings.Join(drift, "\n"))
	}
}

func TestCmdGCIntegrationManifestDriftDiagnosesBothDirections(t *testing.T) {
	manifest := []string{"TestKept", "TestStale", "TestStale"}
	declared := []string{"TestKept", "TestNew"}
	want := []string{
		"duplicate cmd/gc integration manifest entry: TestStale",
		"unassigned cmd/gc integration test: TestNew",
		"cmd/gc integration manifest entry is not integration-only: TestStale",
	}

	if got := cmdGCIntegrationManifestDrift(manifest, declared); !slices.Equal(got, want) {
		t.Fatalf("manifest drift diagnostics = %q, want %q", got, want)
	}
}

func TestCmdGCIntegrationDiscoveryUsesCanonicalLinuxPlatform(t *testing.T) {
	context := canonicalCmdGCIntegrationBuildContext()
	if context.GOOS != "linux" || context.GOARCH != "amd64" {
		t.Fatalf("cmd/gc integration build target = %s/%s, want linux/amd64", context.GOOS, context.GOARCH)
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

	if got, want := discoverCmdGCIntegrationTests(t, dir), []string{"TestLinux"}; !slices.Equal(got, want) {
		t.Fatalf("canonical linux/amd64 integration tests = %q, want %q", got, want)
	}
}

func TestCmdGCIntegrationDiscoveryDistinguishesTestMainHarness(t *testing.T) {
	t.Run("ordinary test", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "ordinary_integration_test.go"), `//go:build integration

package fixture

import "testing"

func TestMain(*testing.T) {}
`)

		if got, want := discoverCmdGCIntegrationTests(t, dir), []string{"TestMain"}; !slices.Equal(got, want) {
			t.Fatalf("integration tests = %q, want ordinary TestMain included as %q", got, want)
		}
	})

	t.Run("test harness", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "harness_integration_test.go"), `//go:build integration

package fixture

import "testing"

func TestMain(m *testing.M) {}
func TestOrdinary(t *testing.T) {}
`)

		if got, want := discoverCmdGCIntegrationTests(t, dir), []string{"TestOrdinary"}; !slices.Equal(got, want) {
			t.Fatalf("integration tests = %q, want harness excluded and ordinary test %q", got, want)
		}
	})
}

func parseCmdGCIntegrationManifest(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cmd/gc integration manifest: %v", err)
	}

	const declaration = "cmd_gc_integration_tests=("
	inManifest := false
	var tests []string
	for _, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(rawLine)
		if !inManifest {
			if line == declaration {
				inManifest = true
			}
			continue
		}
		if line == ")" {
			if len(tests) == 0 {
				t.Fatal("cmd_gc_integration_tests is empty")
			}
			return tests
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 1 {
			t.Fatalf("unsupported cmd_gc_integration_tests entry %q", rawLine)
		}
		testName := strings.Trim(fields[0], "'\"")
		if !isGoTestName(testName) {
			t.Fatalf("invalid cmd_gc_integration_tests entry %q", testName)
		}
		tests = append(tests, testName)
	}
	if !inManifest {
		t.Fatalf("%s not found in %s", declaration, path)
	}
	t.Fatalf("unterminated %s in %s", declaration, path)
	return nil
}

func discoverCmdGCIntegrationTests(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cmd/gc directory: %v", err)
	}

	withoutIntegration := canonicalCmdGCIntegrationBuildContext()
	withIntegration := withoutIntegration
	withIntegration.BuildTags = []string{"integration"}

	fileSet := token.NewFileSet()
	var tests []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		tagged, err := withIntegration.MatchFile(dir, name)
		if err != nil {
			t.Fatalf("match tagged cmd/gc file %s: %v", name, err)
		}
		untagged, err := withoutIntegration.MatchFile(dir, name)
		if err != nil {
			t.Fatalf("match untagged cmd/gc file %s: %v", name, err)
		}
		if !tagged || untagged {
			continue
		}

		path := filepath.Join(dir, name)
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse integration-only cmd/gc file %s: %v", name, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && isGoTestName(function.Name.Name) && isGoTestFunc(function, "T") {
				tests = append(tests, function.Name.Name)
			}
		}
	}
	slices.Sort(tests)
	return tests
}

func canonicalCmdGCIntegrationBuildContext() build.Context {
	context := build.Default
	context.GOOS = "linux"
	context.GOARCH = "amd64"
	context.Compiler = "gc"
	context.CgoEnabled = true
	context.BuildTags = nil
	context.ToolTags = nil
	return context
}

func isGoTestName(name string) bool {
	if !strings.HasPrefix(name, "Test") {
		return false
	}
	runeAfterPrefix, _ := utf8.DecodeRuneInString(strings.TrimPrefix(name, "Test"))
	return !unicode.IsLower(runeAfterPrefix)
}

func isGoTestFunc(function *ast.FuncDecl, parameterType string) bool {
	if function.Type.TypeParams != nil && len(function.Type.TypeParams.List) != 0 {
		return false
	}
	if function.Type.Results != nil && len(function.Type.Results.List) != 0 {
		return false
	}
	if function.Type.Params == nil || len(function.Type.Params.List) != 1 {
		return false
	}
	parameter := function.Type.Params.List[0]
	if len(parameter.Names) > 1 {
		return false
	}
	pointer, ok := parameter.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch target := pointer.X.(type) {
	case *ast.Ident:
		return target.Name == parameterType
	case *ast.SelectorExpr:
		return target.Sel.Name == parameterType
	default:
		return false
	}
}

func cmdGCIntegrationManifestDrift(manifest, declared []string) []string {
	manifestSet := make(map[string]struct{}, len(manifest))
	var duplicates []string
	for _, testName := range manifest {
		if _, exists := manifestSet[testName]; exists {
			if !slices.Contains(duplicates, testName) {
				duplicates = append(duplicates, testName)
			}
			continue
		}
		manifestSet[testName] = struct{}{}
	}
	slices.Sort(duplicates)
	declaredSet := make(map[string]struct{}, len(declared))
	for _, testName := range declared {
		declaredSet[testName] = struct{}{}
	}

	var unassigned []string
	for testName := range declaredSet {
		if _, ok := manifestSet[testName]; !ok {
			unassigned = append(unassigned, testName)
		}
	}
	slices.Sort(unassigned)
	var stale []string
	for testName := range manifestSet {
		if _, ok := declaredSet[testName]; !ok {
			stale = append(stale, testName)
		}
	}
	slices.Sort(stale)

	drift := make([]string, 0, len(duplicates)+len(unassigned)+len(stale))
	for _, testName := range duplicates {
		drift = append(drift, "duplicate cmd/gc integration manifest entry: "+testName)
	}
	for _, testName := range unassigned {
		drift = append(drift, "unassigned cmd/gc integration test: "+testName)
	}
	for _, testName := range stale {
		drift = append(drift, "cmd/gc integration manifest entry is not integration-only: "+testName)
	}
	return drift
}

type integrationShardFixture struct {
	binDir      string
	homeDir     string
	capturePath string
}

func newIntegrationShardFixture(t *testing.T) integrationShardFixture {
	t.Helper()

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	capturePath := filepath.Join(tmp, "go-test.capture")

	writeExecutable(t, filepath.Join(binDir, "go"), `#!/usr/bin/env bash
set -euo pipefail

capture_path=`+shellQuote(capturePath)+`

case "$1" in
  env)
    case "$2" in
      GOPATH) echo /tmp/fake-gopath ;;
      GOCACHE) echo /tmp/fake-gocache ;;
      GOMODCACHE) echo /tmp/fake-gomodcache ;;
      GOTMPDIR) echo "" ;;
      GOROOT) echo /tmp/fake-goroot ;;
      *) echo "unexpected go env key: $2" >&2; exit 1 ;;
    esac
    ;;
  test)
    for arg in "$@"; do
      if [[ "$arg" == "-list" ]]; then
        echo "go test -list must not run" >&2
        exit 97
      fi
    done
    printf '%s\0' "$@" >> "$capture_path"
    printf '\0' >> "$capture_path"
    ;;
  *)
    echo "unexpected go command: $*" >&2
    exit 1
    ;;
esac
`)

	return integrationShardFixture{
		binDir:      binDir,
		homeDir:     filepath.Join(tmp, "home"),
		capturePath: capturePath,
	}
}

func (f integrationShardFixture) run(t *testing.T) ([]byte, error) {
	t.Helper()
	repo := repoRoot(t)
	cmd := exec.Command(
		filepath.Join(repo, "scripts", "test-integration-shard"),
		"packages-cmd-gc-integration",
	)
	cmd.Dir = repo
	cmd.Env = []string{
		"PATH=" + f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + f.homeDir,
		"GC_TEST_NO_SLICE=1",
		"SYS_USR_CGO_FALLBACK=0",
		"GO_TEST_TIMEOUT=17s",
	}
	return cmd.CombinedOutput()
}
