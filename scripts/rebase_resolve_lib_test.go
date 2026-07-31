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

// TestRebaseResolveLibDefinesOwnershipGuardUnderAmbientZsh guards against a
// regression where sourcing rebase-resolve-lib.sh from a non-bash ambient
// shell silently fails to load its sibling push-ownership-guard.sh, leaving
// assert_bead_still_claimed undefined. rebase-resolve-lib.sh is always
// SOURCED (". scripts/rebase-resolve-lib.sh"), never executed via its own
// bash shebang — see formulas/mol-deployer-gate.formula.toml and
// prompts/deployer.md Guardrails — so whatever shell the deployer's
// interactive session runs (zsh, in this fork) becomes the shell that
// parses it. A self-location trick that only works under bash
// (${BASH_SOURCE[0]}) silently expands empty under zsh, so dirname resolves
// to "." instead of the script's real directory (ga-ql4bmm).
func TestRebaseResolveLibDefinesOwnershipGuardUnderAmbientZsh(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not installed")
	}
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "rebase-resolve-lib.sh")

	cmd := exec.Command("zsh", "-c", `. "$REBASE_LIB" && typeset -f assert_bead_still_claimed >/dev/null`)
	// cwd = repo root, matching the real invocation: both
	// mol-deployer-gate.formula.toml and prompts/deployer.md.tmpl source this
	// file via the repo-root-relative path ". scripts/rebase-resolve-lib.sh".
	// A cwd-independent fix must work here too, not just from scripts/ itself.
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "REBASE_LIB="+lib)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rebase-resolve-lib.sh sourced under zsh did not define assert_bead_still_claimed from push-ownership-guard.sh: %v\n%s", err, out)
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
