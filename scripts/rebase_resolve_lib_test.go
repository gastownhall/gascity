package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
