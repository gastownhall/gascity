package bootstrap

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// readCorePackAsset reads a file under internal/bootstrap/packs/core, the
// same way core_formula_report_test.go reaches the formula TOMLs.
func readCorePackAsset(t *testing.T, rel string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filename)
	if r := bazeltest.OverrideRoot(); r != "" {
		root = filepath.Join(r, "internal", "bootstrap")
	}
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// TestPoolWorkerPromptMentionsReusedSlotWorktrees is AC-8: implicit pool
// agents now run each instance in a per-slot git worktree that persists
// between sessions, so the pool-worker prompt must tell a worker its cwd may
// hold leftover work from an earlier session — check git state, and never
// discard work you did not create.
func TestPoolWorkerPromptMentionsReusedSlotWorktrees(t *testing.T) {
	prompt := readCorePackAsset(t, filepath.Join("packs", "core", "assets", "prompts", "pool-worker.template.md"))
	for _, phrase := range []string{
		"slot worktree",
		"git status",
		"git log",
		"never discard work you did not create",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("pool-worker prompt does not mention %q", phrase)
		}
	}
}

// TestMolDoWorkDescribesSlotBranchCommits is AC-8: the work formula's
// description claims there is no git branching or worktree isolation, which
// stopped being true — implicit pool instances commit to their own slot
// branch, not the rig root's branch.
func TestMolDoWorkDescribesSlotBranchCommits(t *testing.T) {
	formula := readCorePackAsset(t, filepath.Join("packs", "core", "formulas", "mol-do-work.toml"))
	if !strings.Contains(formula, "slot branch") {
		t.Errorf("mol-do-work does not mention the slot branch its workers commit to")
	}
	if strings.Contains(strings.ToLower(formula), "no worktree isolation") {
		t.Errorf("mol-do-work still claims there is no worktree isolation")
	}
	if strings.Contains(strings.ToLower(formula), "no git branching") {
		t.Errorf("mol-do-work still claims there is no git branching")
	}
}
