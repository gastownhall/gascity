package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRebaseResolveLibUsesBash3Syntax(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "scripts", "rebase-resolve-lib.sh")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rebase-resolve-lib.sh: %v", err)
	}

	bash4LowercaseExpansion := regexp.MustCompile(`\$\{[^}\n]*,,[^}\n]*\}`)
	if match := bash4LowercaseExpansion.Find(contents); match != nil {
		t.Fatalf("rebase-resolve-lib.sh must run under macOS Bash 3; found Bash 4 lowercase expansion %q", match)
	}
}

// ownershipGuardProbe sources rebase-resolve-lib.sh and proves that the
// sibling push-ownership-guard.sh it pulled in is the one from THIS tree.
// Provenance needs three checks, not one: assert_bead_still_claimed must
// exist (it is what attempt_bounded_self_rebase calls before its
// --force-with-lease push), one of the real guard's private helpers must
// exist alongside it, and the decoy marker planted by the unrelated-repo
// fixture must be absent. `typeset -f` is the spelling that works in both
// bash and zsh.
const ownershipGuardProbe = `
. "$REBASE_LIB" || { echo "SOURCE_FAILED"; exit 1; }
typeset -f assert_bead_still_claimed >/dev/null 2>&1 || { echo "GUARD_MISSING"; exit 1; }
typeset -f _pog_resolve_bead_id >/dev/null 2>&1 || { echo "GUARD_MISSING_HELPER"; exit 1; }
if [ -n "${RRL_DECOY_GUARD_LOADED:-}" ]; then echo "GUARD_WRONG_TREE"; exit 1; fi
echo "GUARD_OK"
`

// decoyGuard mimics push-ownership-guard.sh closely enough to be
// indistinguishable by function name alone, so the only thing that
// distinguishes it from the real guard is the marker it sets. It stands in
// for the genuinely dangerous case: an unrelated worktree that happens to
// have a scripts/push-ownership-guard.sh of its own.
const decoyGuard = `#!/usr/bin/env bash
RRL_DECOY_GUARD_LOADED=1
_pog_resolve_bead_id() { echo "decoy"; }
assert_bead_still_claimed() { return 0; }
`

// TestRebaseResolveLibDefinesOwnershipGuardAcrossShellsAndCwds guards
// against a regression where sourcing rebase-resolve-lib.sh silently fails
// to load its sibling push-ownership-guard.sh, leaving
// assert_bead_still_claimed undefined — or, worse, loads a DIFFERENT tree's
// copy of it. rebase-resolve-lib.sh is always SOURCED
// (". scripts/rebase-resolve-lib.sh"), never executed via its own bash
// shebang — see formulas/mol-deployer-gate.formula.toml and
// prompts/deployer.md Guardrails — so whatever shell the deployer's
// interactive session runs (zsh, in this fork) becomes the shell that parses
// it, and whatever cwd that session happens to be in becomes the cwd.
//
// Both axes have produced real bugs, in opposite directions, which is why
// this is a matrix and not a single case:
//
//   - A bash-only self-location trick (${BASH_SOURCE[0]}) expands empty
//     under zsh, so dirname resolves to "." instead of the script's real
//     directory (ga-ql4bmm). Caught by the zsh arms.
//   - A git-toplevel anchor ($(git rev-parse --show-toplevel)/scripts)
//     resolves from the CALLER's cwd, not the sourced file's location, so it
//     breaks outside any repo and silently cross-sources from an unrelated
//     repo that has its own scripts/ dir. Caught by the outside-any-repo and
//     unrelated-git-repo arms — under bash as well as zsh.
//
// Only the zsh arms skip when zsh is absent; the bash arms always run, so
// the self-location path stays covered on any runner image.
func TestRebaseResolveLibDefinesOwnershipGuardAcrossShellsAndCwds(t *testing.T) {
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "rebase-resolve-lib.sh")

	// An unrelated git repo carrying its own scripts/push-ownership-guard.sh.
	// A cwd-anchored resolver "succeeds" here — it finds a real file — which
	// makes this the worst case: a force-push gate sourced from a foreign
	// tree rather than an honest failure.
	unrelated := filepath.Join(t.TempDir(), "unrelated-repo")
	if err := os.MkdirAll(filepath.Join(unrelated, "scripts"), 0o755); err != nil {
		t.Fatalf("create unrelated repo tree: %v", err)
	}
	if out, err := exec.Command("git", "init", "-q", unrelated).CombinedOutput(); err != nil {
		t.Fatalf("git init unrelated repo: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "scripts", "push-ownership-guard.sh"), []byte(decoyGuard), 0o755); err != nil {
		t.Fatalf("write decoy guard: %v", err)
	}

	cwds := []struct {
		name string
		dir  string
	}{
		// The real invocation path: both mol-deployer-gate.formula.toml and
		// prompts/deployer.md.tmpl source this file as
		// ". scripts/rebase-resolve-lib.sh" from the repo root.
		{"repo-root", root},
		{"scripts-dir", filepath.Join(root, "scripts")},
		{"outside-any-repo", t.TempDir()},
		{"unrelated-git-repo", unrelated},
	}

	for _, shell := range []string{"bash", "zsh"} {
		for _, cwd := range cwds {
			t.Run(shell+"/"+cwd.name, func(t *testing.T) {
				if _, err := exec.LookPath(shell); err != nil {
					if shell == "zsh" {
						t.Skip("zsh not installed")
					}
					t.Fatalf("%s not installed: %v", shell, err)
				}

				cmd := exec.Command(shell, "-c", ownershipGuardProbe)
				cmd.Dir = cwd.dir
				cmd.Env = append(os.Environ(), "REBASE_LIB="+lib)

				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("sourcing rebase-resolve-lib.sh under %s from %s did not load this tree's push-ownership-guard.sh: %v\n%s",
						shell, cwd.name, err, out)
				}
				if !strings.Contains(string(out), "GUARD_OK") {
					t.Fatalf("expected GUARD_OK under %s from %s, got: %s", shell, cwd.name, out)
				}
			})
		}
	}
}

// TestRebaseResolveLibClassifierWorksUnderZsh guards against a regression
// where is_additive_keepboth_path and resolve_conflict_markers_in_file
// silently broke every external command they call (tr, mktemp, awk, mv)
// whenever sourced into a zsh caller. zsh ties the scalar $PATH to an array
// named `path`; both functions declared `local path="$1"`, which shadows
// that special parameter and blanks the command search path for the
// function body. Under bash this parameter name is inert, so the bug was
// invisible to any bash-only test — it only fires when the ambient sourcing
// shell is zsh, which is the deployer's actual interactive shell in this
// fork (ga-g1mlel). Unlike a fatal bash-only expansion, this failure is
// silent: is_additive_keepboth_path misclassifies (tr fails, so its
// lowercase comparison is against an empty string) and
// resolve_conflict_markers_in_file returns rc=2 (mktemp not found) instead
// of resolving — both fail SAFE (route to the builder) rather than loud,
// which is exactly why the bug went unnoticed.
func TestRebaseResolveLibClassifierWorksUnderZsh(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not installed")
	}
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "rebase-resolve-lib.sh")

	conflictFile := filepath.Join(t.TempDir(), "conflict.txt")
	fixture := "top\n<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> branch\nbottom\n"
	if err := os.WriteFile(conflictFile, []byte(fixture), 0o644); err != nil {
		t.Fatalf("write conflict fixture: %v", err)
	}

	// DOCS/Guide.TXT is a case-insensitive doc-file match: correct behavior
	// is rc=0. Pre-fix, $PATH is blanked inside the function so `tr` cannot
	// run, the lowercase comparison degrades to an empty string, and no case
	// pattern matches — the function wrongly returns 1.
	script := `
. "$REBASE_LIB"
is_additive_keepboth_path "DOCS/Guide.TXT" || { echo "CLASSIFY_FAILED"; exit 1; }
resolve_conflict_markers_in_file "$CONFLICT_FILE" 1 || { echo "RESOLVE_FAILED rc=$?"; exit 1; }
echo "OK"
`
	cmd := exec.Command("zsh", "-c", script)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"REBASE_LIB="+lib,
		"CONFLICT_FILE="+conflictFile,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("classifier functions failed under zsh (local path=... shadows $PATH, breaking tr/mktemp/awk/mv): %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected OK sentinel, got: %s", out)
	}

	got, err := os.ReadFile(conflictFile)
	if err != nil {
		t.Fatalf("read resolved conflict file: %v", err)
	}
	if strings.Contains(string(got), "<<<<<<<") || !strings.Contains(string(got), "ours") || !strings.Contains(string(got), "theirs") {
		t.Fatalf("conflict file not correctly resolved (expected keep-both union, no markers): %q", got)
	}
}

// TestRebaseResolveLib runs the shell self-test for
// scripts/rebase-resolve-lib.sh, the deployer's bounded self-rebase
// trivial-conflict classifier. It exercises the classifier against real
// temp git repos (identical/one-side-empty/additive-both hunks, real
// conflicts, structural conflicts) plus attempt_bounded_self_rebase's guard
// rails and --force-with-lease push behavior. Hermetic: temp git repos only,
// no network/gh/model calls.
func TestRebaseResolveLib(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-rebase-resolve.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-rebase-resolve.sh failed: %v\n%s", err, out)
	}
}
