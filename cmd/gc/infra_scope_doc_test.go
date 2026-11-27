package main

import (
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// infraScopeDocDeclaredOnce guards infraScopeDocDeclaredTests, filled by the
// one walk infraScopeDocTestDeclared performs per process.
var infraScopeDocDeclaredOnce sync.Once

// infraScopeDocDeclaredTests maps every test function name declared by a Go
// file in the repository.
var infraScopeDocDeclaredTests map[string]bool

// infraScopeDocRepoRoot resolves the repository root for the doc guards
// below: the bazel runfiles workspace when available, the caller-path
// arithmetic of plain `go test` otherwise.
func infraScopeDocRepoRoot(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return root
	}
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// TestInfraScopeDocsFollowTheCallerNotTheSession guards the documented shape
// of the city-infrastructure scoping rule (docs/reference/tmux-agent-slice.md
// § City infrastructure and RUNTIME-RPP-036): the rule is stated by the
// caller's cgroup — never by session identity, never as numbered conditions
// — and it names the callers that stay unscoped together with their remedy.
func TestInfraScopeDocsFollowTheCallerNotTheSession(t *testing.T) {
	root := infraScopeDocRepoRoot(t)

	docBytes, err := os.ReadFile(filepath.Join(root, "docs", "reference", "tmux-agent-slice.md"))
	if err != nil {
		t.Fatalf("reading tmux-agent-slice.md: %v", err)
	}
	doc := string(docBytes)
	for _, want := range []string{
		"whichever gc command autostarts the managed Dolt server",
		"only when the move suits the gc process's cgroup",
		"Where the gc process runs decides whether the move suits it",
		"| Where the gc process runs | The move suits it |",
		"runs where the operator placed it",
		"| Spawn | Wrapped when the move suits the caller |",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("tmux-agent-slice.md lost the caller-based rule %q", want)
		}
	}
	for _, stale := range []string{
		"only when both hold",
		"condition 2",
		"Condition 2",
		"Wrapped from a session",
		"runs inside an agent session",
		"no session cgroup to escape",
		"GC_SESSION_ID",
	} {
		if strings.Contains(doc, stale) {
			t.Errorf("tmux-agent-slice.md still states the retired session gate %q", stale)
		}
	}

	reqBytes, err := os.ReadFile(filepath.Join(root, "internal", "runtime", "REQUIREMENTS.md"))
	if err != nil {
		t.Fatalf("reading REQUIREMENTS.md: %v", err)
	}
	var row string
	for _, line := range strings.Split(string(reqBytes), "\n") {
		if strings.HasPrefix(line, "| RUNTIME-RPP-036 |") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("RUNTIME-RPP-036 row not found in REQUIREMENTS.md")
	}
	for _, stale := range []string{"GC_SESSION_ID", "carry no session identity", "run only for a caller in a session"} {
		if strings.Contains(row, stale) {
			t.Errorf("RUNTIME-RPP-036 still states the retired session gate %q", stale)
		}
	}
	if !strings.Contains(row, "run only for a caller whose cgroup the scope suits") {
		t.Error("RUNTIME-RPP-036 does not scope the probe to the caller's cgroup")
	}
	for _, name := range regexp.MustCompile(`Test[A-Za-z0-9_]+`).FindAllString(row, -1) {
		if !infraScopeDocTestDeclared(t, root, name) {
			t.Errorf("RUNTIME-RPP-036 cites test %s, which no Go source in the repository declares", name)
		}
	}

	for _, src := range []string{
		filepath.Join(root, "cmd", "gc", "infra_scope.go"),
		filepath.Join(root, "cmd", "gc", "beads_provider_lifecycle.go"),
	} {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading %s: %v", src, err)
		}
		if strings.Contains(string(data), "GC_SESSION_ID") {
			t.Errorf("%s still ties infrastructure scoping to GC_SESSION_ID", src)
		}
	}
}

// infraScopeDocTestDeclared reports whether a Go file under root declares
// the test function name. It caches one walk per process.
func infraScopeDocTestDeclared(t *testing.T, root, name string) bool {
	t.Helper()
	infraScopeDocDeclaredOnce.Do(func() {
		infraScopeDocDeclaredTests = map[string]bool{}
		declared := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil //nolint:nilerr // unreadable entries simply do not declare tests
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil //nolint:nilerr // same
			}
			for _, m := range declared.FindAllStringSubmatch(string(data), -1) {
				infraScopeDocDeclaredTests[m[1]] = true
			}
			return nil
		})
	})
	return infraScopeDocDeclaredTests[name]
}
