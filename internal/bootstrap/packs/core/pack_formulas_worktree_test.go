package core

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The core formulas that provision a per-bead worktree must go through
// `gc worktree ensure|cleanup`, the transactional owner of workspace
// provisioning, instead of raw `git worktree add|remove`. A raw add publishes
// no ownership evidence, so the controller cannot verify the directory, and a
// raw remove (with its `rm -rf` fallback) can strand the registration or
// delete unmerged work. These tests pin the prompt text that carries that
// contract (ga-b7i4cn).

var worktreeProvisioningFormulas = []string{"mol-scoped-work.toml", "mol-polecat-commit.toml"}

// logicalLines joins shell backslash continuations so a multi-line command can
// be matched as the single command it is.
func logicalLines(text string) []string {
	return strings.Split(strings.ReplaceAll(text, "\\\n", " "), "\n")
}

// evidenceKeys is the full metadata set a bead must carry for the controller
// to treat its work dir as a managed worktree.
func evidenceKeys() []string {
	keys := []string{beadmeta.WorkDirMetadataKey, beadmeta.WorkBranchMetadataKey}
	return append(keys, beadmeta.WorktreeOwnershipEvidenceKeys...)
}

func TestWorkspaceSetupUsesGCWorktreeEnsure(t *testing.T) {
	for _, file := range worktreeProvisioningFormulas {
		t.Run(file, func(t *testing.T) {
			step := formulaStep(t, readFormula(t, file), "workspace-setup")

			if strings.Contains(step, "git worktree add") {
				t.Error("workspace-setup creates the worktree with raw `git worktree add`; use `gc worktree ensure`")
			}
			if !strings.Contains(step, "gc worktree ensure") {
				t.Fatal("workspace-setup must provision the worktree with `gc worktree ensure`")
			}
			if !strings.Contains(step, "--json") {
				t.Error("workspace-setup must read the ensure report with --json; the provenance is what gets published")
			}
			if strings.Contains(step, "--set-metadata work_dir=") {
				t.Error("workspace-setup publishes the legacy `work_dir` key; the managed key is gc.work_dir")
			}

			// `gc worktree ensure` publishes nothing itself. The controller
			// refuses a work_dir whose evidence is partial, so every key must land
			// in the one bd update: a crash between two updates would leave the
			// bead with a work dir and incomplete evidence.
			var publish string
			for _, line := range logicalLines(step) {
				if strings.Contains(line, "--set-metadata "+beadmeta.WorkDirMetadataKey+"=") {
					publish = line
					break
				}
			}
			if publish == "" {
				t.Fatalf("workspace-setup never publishes --set-metadata %s=", beadmeta.WorkDirMetadataKey)
			}
			for _, key := range evidenceKeys() {
				if !strings.Contains(publish, "--set-metadata "+key+"=") {
					t.Errorf("the bd update that publishes %s must also publish %s in the same command", beadmeta.WorkDirMetadataKey, key)
				}
			}
		})
	}
}

func TestMolScopedWorkCleanupUsesGovernedCleanup(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-scoped-work.toml"), "cleanup-worktree")
	assertGovernedCleanup(t, step)
}

// assertGovernedCleanup pins a cleanup step that delegates removal to
// `gc worktree cleanup` and only forgets the evidence once the worktree is
// really gone.
func assertGovernedCleanup(t *testing.T, step string) {
	t.Helper()
	for _, banned := range []string{"git worktree remove", "git -C \"$REPO\"", "rm -rf", "git worktree prune"} {
		if strings.Contains(step, banned) {
			t.Errorf("cleanup must not use %q; `gc worktree cleanup` is the only remover (no force mode, no recursive fallback)", banned)
		}
	}
	cleanupAt := strings.Index(step, "gc worktree cleanup")
	if cleanupAt < 0 {
		t.Fatal("cleanup must remove the worktree with `gc worktree cleanup`")
	}
	if !strings.Contains(step, "--attempt-id") {
		t.Error("cleanup must pass --attempt-id so a stale request cannot remove a workspace re-created at the same path")
	}
	if !strings.Contains(step, "--json") {
		t.Error("cleanup must read the structured report with --json")
	}
	// Safety refusals (dirty tree, unmerged commits) come back as
	// cleanup_pending. The step must handle that outcome explicitly instead of
	// treating the refusal as success or as a reason to delete anyway.
	for _, want := range []string{"cleanup_pending", "already_absent", ".removed"} {
		if !strings.Contains(step, want) {
			t.Errorf("cleanup must branch on the report's %q field", want)
		}
	}
	unsetAt := strings.Index(step, "--unset-metadata")
	if unsetAt < 0 {
		t.Fatal("cleanup must clear the worktree evidence once the worktree is gone")
	}
	if unsetAt < cleanupAt {
		t.Error("cleanup clears the evidence before `gc worktree cleanup` has run; a refused cleanup would lose the only record of the worktree")
	}
	var unset string
	for _, line := range logicalLines(step) {
		if strings.Contains(line, "--unset-metadata") {
			unset = line
			break
		}
	}
	for _, key := range evidenceKeys() {
		if !strings.Contains(unset, "--unset-metadata "+key) {
			t.Errorf("cleanup must unset %s together with the rest of the evidence", key)
		}
	}
}

func TestMolPolecatCommitWorktreeCleanupIsDAGTeardown(t *testing.T) {
	f := readFormula(t, "mol-polecat-commit.toml")
	byID := map[string]int{}
	for i, s := range f.Steps {
		byID[s.ID] = i
	}
	meta := func(id, key string) any {
		i, ok := byID[id]
		if !ok {
			t.Fatalf("mol-polecat-commit has no step %q", id)
		}
		return f.Steps[i].Metadata[key]
	}

	// Prose after a `git push` retry loop that `exit 1`s never runs on the
	// failure path, so the worktree leaks exactly when work is stranded. A
	// teardown step in the scope runs on pass and on fail.
	if got := meta("body", "gc.kind"); got != "scope" {
		t.Errorf("body gc.kind = %v, want scope", got)
	}
	if got := meta("body", "gc.scope_role"); got != "body" {
		t.Errorf("body gc.scope_role = %v, want body", got)
	}
	for _, id := range []string{"workspace-setup", "commit-and-push"} {
		if got := meta(id, "gc.scope_ref"); got != "body" {
			t.Errorf("%s gc.scope_ref = %v, want body", id, got)
		}
	}
	if got := meta("cleanup-worktree", "gc.kind"); got != "cleanup" {
		t.Errorf("cleanup-worktree gc.kind = %v, want cleanup", got)
	}
	if got := meta("cleanup-worktree", "gc.scope_ref"); got != "body" {
		t.Errorf("cleanup-worktree gc.scope_ref = %v, want body", got)
	}
	if got := meta("cleanup-worktree", "gc.scope_role"); got != "teardown" {
		t.Errorf("cleanup-worktree gc.scope_role = %v, want teardown", got)
	}
	needsBody := false
	for _, n := range f.Steps[byID["cleanup-worktree"]].Needs {
		needsBody = needsBody || n == "body"
	}
	if !needsBody {
		t.Error(`cleanup-worktree must need "body" so teardown waits for the scope to settle`)
	}

	assertGovernedCleanup(t, formulaStep(t, f, "cleanup-worktree"))

	commit := formulaStep(t, f, "commit-and-push")
	for _, banned := range []string{"git worktree remove", "rm -rf", "gc worktree cleanup", "--unset-metadata"} {
		if strings.Contains(commit, banned) {
			t.Errorf("commit-and-push still carries cleanup (%q); cleanup belongs to the cleanup-worktree teardown step", banned)
		}
	}
}
