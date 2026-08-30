package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// beadsManagedGitHooks are the git hooks beads installs when its own installer
// claims core.hooksPath. Every one of them needs a .githooks counterpart, or
// pointing core.hooksPath back at .githooks (which `make setup` does, and which
// AGENTS.md requires) silently drops that beads hook.
var beadsManagedGitHooks = []string{
	"post-checkout",
	"post-merge",
	"pre-commit",
	"pre-push",
	"prepare-commit-msg",
}

// beadsChainPath is the single shared forwarder every .githooks hook calls.
const beadsChainPath = ".githooks/lib/beads-chain.sh"

// chainEnv builds a PATH-controlled environment for beads-chain.sh runs so the
// fake `bd` is the only one visible while the real coreutils stay reachable.
func chainEnv(t *testing.T, binDir string, extra ...string) []string {
	t.Helper()
	env := []string{
		"PATH=" + binDir + ":/usr/bin:/bin",
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}
	return append(env, extra...)
}

// runChain executes the beads chain forwarder and returns its exit code plus
// combined output.
func runChain(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, beadsChainPath), args...)
	cmd.Dir = root
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run %s %v: %v\n%s", beadsChainPath, args, err, out)
	}
	return exitErr.ExitCode(), string(out)
}

func asExitError(err error, target **exec.ExitError) bool {
	exitErr := &exec.ExitError{}
	ok := errors.As(err, &exitErr)
	if ok {
		*target = exitErr
	}
	return ok
}

// TestGitHooksCoverEveryBeadsManagedHook is the regression guard for when
// beads took over core.hooksPath: because .beads/hooks never chained onward,
// every gate in .githooks stopped running. .githooks is the single hooksPath
// owner now, so it must carry a chaining counterpart for each beads hook.
func TestGitHooksCoverEveryBeadsManagedHook(t *testing.T) {
	root := repoRoot(t)
	for _, hook := range beadsManagedGitHooks {
		t.Run(hook, func(t *testing.T) {
			path := filepath.Join(root, ".githooks", hook)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf(".githooks/%s missing: %v (pointing core.hooksPath at .githooks would drop the beads %s hook)", hook, err, hook)
			}
			if info.Mode().Perm()&0o111 == 0 {
				t.Fatalf(".githooks/%s mode = %o, want executable", hook, info.Mode().Perm())
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read .githooks/%s: %v", hook, err)
			}
			// Hooks invoke the forwarder through a quoted repo-root path, so
			// compare against a quote-stripped body rather than pinning one
			// spelling of the call.
			text := strings.ReplaceAll(string(body), `"`, "")
			if !strings.Contains(text, beadsChainPath) {
				t.Fatalf(".githooks/%s does not call %s; beads would stop running for this hook", hook, beadsChainPath)
			}
			if !strings.Contains(text, beadsChainPath+" "+hook) {
				t.Fatalf(".githooks/%s must forward its own hook name: want %q in body", hook, beadsChainPath+" "+hook)
			}
		})
	}
}

// TestBeadsChainForwardsHookNameAndArguments pins the contract the .beads/hooks
// integration block implemented: `bd hooks run <hook> "$@"` with BD_GIT_HOOK set.
func TestBeadsChainForwardsHookNameAndArguments(t *testing.T) {
	binDir := t.TempDir()
	record := filepath.Join(t.TempDir(), "argv")
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env sh
{
  printf 'args=%s\n' "$*"
  printf 'BD_GIT_HOOK=%s\n' "${BD_GIT_HOOK:-unset}"
} > "$CHAIN_RECORD"
`)

	code, out := runChain(t, chainEnv(t, binDir, "CHAIN_RECORD="+record), "prepare-commit-msg", ".git/COMMIT_EDITMSG", "message")
	if code != 0 {
		t.Fatalf("chain exit = %d, want 0\n%s", code, out)
	}

	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	want := "args=hooks run prepare-commit-msg .git/COMMIT_EDITMSG message\nBD_GIT_HOOK=1\n"
	if string(got) != want {
		t.Fatalf("bd invocation = %q, want %q", got, want)
	}
}

// TestBeadsChainPropagatesBeadsFailure keeps beads authoritative: a real hook
// rejection must still abort the commit/push.
func TestBeadsChainPropagatesBeadsFailure(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env sh
echo "beads rejected the change" >&2
exit 7
`)

	code, out := runChain(t, chainEnv(t, binDir), "pre-commit")
	if code != 7 {
		t.Fatalf("chain exit = %d, want 7 (beads failure must propagate)\n%s", code, out)
	}
}

// TestBeadsChainTreatsUninitializedDatabaseAsSuccess mirrors the beads exit-3
// carve-out: a clone without a beads database still commits.
func TestBeadsChainTreatsUninitializedDatabaseAsSuccess(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env sh
exit 3
`)

	code, out := runChain(t, chainEnv(t, binDir), "pre-commit")
	if code != 0 {
		t.Fatalf("chain exit = %d, want 0 for uninitialized database\n%s", code, out)
	}
	if !strings.Contains(out, "database not initialized") {
		t.Fatalf("chain output = %q, want the uninitialized-database notice", out)
	}
}

// TestBeadsChainTreatsTimeoutAsSuccess mirrors the beads BEADS_HOOK_TIMEOUT
// carve-out: a wedged bd must not wedge every commit in the repo.
func TestBeadsChainTreatsTimeoutAsSuccess(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env sh
sleep 30
`)

	code, out := runChain(t, chainEnv(t, binDir, "BEADS_HOOK_TIMEOUT=1"), "pre-commit")
	if code != 0 {
		t.Fatalf("chain exit = %d, want 0 on timeout\n%s", code, out)
	}
	if !strings.Contains(out, "timed out") {
		t.Fatalf("chain output = %q, want the timeout notice", out)
	}
}

// TestBeadsChainIsNoopWithoutBd keeps .githooks usable for contributors who do
// not have beads installed at all.
func TestBeadsChainIsNoopWithoutBd(t *testing.T) {
	// A PATH holding nothing but `sh` (which the shebang needs) guarantees bd is
	// unreachable regardless of where the host installed it.
	bdFreeBin := t.TempDir()
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("locate sh: %v", err)
	}
	if err := os.Symlink(shPath, filepath.Join(bdFreeBin, "sh")); err != nil {
		t.Fatalf("link sh: %v", err)
	}

	code, out := runChain(t, []string{
		"PATH=" + bdFreeBin,
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}, "pre-commit")
	if code != 0 {
		t.Fatalf("chain exit = %d, want 0 when bd is absent\n%s", code, out)
	}
}

// runHooksOwnerCheck runs scripts/check-githooks-owner.sh inside a throwaway git
// repo configured with the given core.hooksPath value.
func runHooksOwnerCheck(t *testing.T, hooksPath string) (int, string) {
	t.Helper()
	root := repoRoot(t)
	repo := t.TempDir()

	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "gitconfig-system"),
	)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.MkdirAll(filepath.Join(repo, ".githooks"), 0o755); err != nil {
		t.Fatalf("mkdir .githooks: %v", err)
	}
	if hooksPath != "" {
		git("config", "core.hooksPath", hooksPath)
	}

	cmd := exec.Command(filepath.Join(root, "scripts", "check-githooks-owner.sh"))
	cmd.Dir = repo
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run check-githooks-owner.sh: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), string(out)
}

// TestCheckGitHooksOwnerAcceptsGitHooks accepts both spellings a clone can hold:
// the relative path `make setup` writes and an absolute path pointing at the
// same directory.
func TestCheckGitHooksOwnerAcceptsGitHooks(t *testing.T) {
	for _, spelling := range []string{".githooks", "./.githooks"} {
		t.Run(spelling, func(t *testing.T) {
			code, out := runHooksOwnerCheck(t, spelling)
			if code != 0 {
				t.Fatalf("check exit = %d, want 0 for core.hooksPath=%s\n%s", code, spelling, out)
			}
		})
	}
}

// TestCheckGitHooksOwnerRejectsForeignHooksPath is the detector for when
// beads' installer repoints core.hooksPath at .beads/hooks: that takeover must
// be reported, because the bypassed .githooks gate cannot report its own absence.
func TestCheckGitHooksOwnerRejectsForeignHooksPath(t *testing.T) {
	for name, hooksPath := range map[string]string{
		"beads takeover": ".beads/hooks",
		"git default":    "",
	} {
		t.Run(name, func(t *testing.T) {
			code, out := runHooksOwnerCheck(t, hooksPath)
			if code == 0 {
				t.Fatalf("check exit = 0 for core.hooksPath=%q, want failure\n%s", hooksPath, out)
			}
			if !strings.Contains(out, "make setup") {
				t.Fatalf("check output = %q, want the `make setup` remediation", out)
			}
		})
	}
}

// TestMakeSetupInstallsAndVerifiesGitHooksOwner keeps the installer honest: it
// must claim core.hooksPath for .githooks and then assert the claim stuck.
func TestMakeSetupInstallsAndVerifiesGitHooksOwner(t *testing.T) {
	cmd := exec.Command("make", "-n", "setup")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n setup: %v\n%s", err, out)
	}
	recipe := string(out)
	if !strings.Contains(recipe, "git config core.hooksPath .githooks") {
		t.Fatalf("make setup no longer claims core.hooksPath for .githooks:\n%s", recipe)
	}
	if !strings.Contains(recipe, "check-githooks-owner.sh") {
		t.Fatalf("make setup does not verify the hooks owner after install:\n%s", recipe)
	}
}
