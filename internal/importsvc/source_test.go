package importsvc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/git"
)

// TestLsRemoteHeadArgsHardened proves the remote HEAD probe is hardened against
// redirect-based SSRF: the SSRF host fence alone is not sufficient because git
// can follow a redirect off the fenced host, so the probe must carry the
// untrusted-remote git config overrides ahead of `ls-remote <url> HEAD`.
func TestLsRemoteHeadArgsHardened(t *testing.T) {
	const url = "https://github.com/example/tools.git"
	args := lsRemoteHeadArgs(url)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c http.followRedirects=false") {
		t.Errorf("HEAD probe args do not disable redirect following: %v", args)
	}
	if !strings.Contains(joined, "-c protocol.allow=never") {
		t.Errorf("HEAD probe args do not constrain transports: %v", args)
	}

	// The hardening flags must lead the subcommand, and the tail must be the
	// ls-remote HEAD probe against the given URL.
	hardening := git.UntrustedRemoteGitConfigArgs()
	if len(args) < len(hardening)+3 {
		t.Fatalf("args too short: %v", args)
	}
	tail := args[len(hardening):]
	wantTail := []string{"ls-remote", url, "HEAD"}
	for i, w := range wantTail {
		if tail[i] != w {
			t.Fatalf("tail[%d] = %q, want %q; full args %v", i, tail[i], w, args)
		}
	}
}

// TestDefaultHeadCommitRedactsUserinfo proves the remote-HEAD resolve error
// never echoes a userinfo token embedded in the source. GIT_ALLOW_PROTOCOL=file
// makes git fail the https probe instantly (offline, exit 128) so the resolve
// error path runs without a network round-trip.
func TestDefaultHeadCommitRedactsUserinfo(t *testing.T) {
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")

	_, err := defaultHeadCommit("", "https://user:ghp_secret@github.com/example/repo")
	if err == nil {
		t.Fatalf("expected the offline https probe to fail")
	}
	if strings.Contains(err.Error(), "ghp_secret") {
		t.Fatalf("resolve error leaked the userinfo token: %v", err)
	}
}

// TestCanonicalizeLocalGitImportSource_AcceptsRelativeTargetDir pins the
// ga-iawy13.7 canonical-path-at-ingest migration:
// canonicalizeLocalGitImportSource must succeed when targetDir is a valid
// relative path. The pre-migration bare EvalSymlinks call preserves
// relative inputs unresolved (Go's EvalSymlinks does not force an absolute
// result), which then makes the following filepath.Rel(repoRoot,
// resolvedTarget) call fail outright because repoRoot (from `git rev-parse
// --show-toplevel`) is always absolute while resolvedTarget stays relative;
// pathutil.NormalizePathForCompare always resolves to an absolute path
// first, fixing that. Every real caller today (resolveImportAddPath)
// already produces an absolute targetDir, so this closes a latent
// robustness gap rather than an observed production bug.
func TestCanonicalizeLocalGitImportSource_AcceptsRelativeTargetDir(t *testing.T) {
	repoRoot := initGitRepoForTest(t)
	sub := filepath.Join(repoRoot, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(repoRoot)

	canonical, ok, err := canonicalizeLocalGitImportSource("sub")
	if err != nil {
		t.Fatalf("canonicalizeLocalGitImportSource(%q) error = %v, want success for a valid relative path", "sub", err)
	}
	if !ok {
		t.Fatalf("canonicalizeLocalGitImportSource(%q) ok = false, want true", "sub")
	}
	const wantSuffix = "//sub"
	if !strings.HasSuffix(canonical, wantSuffix) {
		t.Errorf("canonicalizeLocalGitImportSource(%q) = %q, want suffix %q", "sub", canonical, wantSuffix)
	}
}

// initGitRepoForTest creates a minimal git repository in a temp directory.
func initGitRepoForTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitForTest(t, dir, "init")
	runGitForTest(t, dir, "config", "user.email", "test@test.com")
	runGitForTest(t, dir, "config", "user.name", "Test")
	return dir
}

func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = git.SanitizedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
}
