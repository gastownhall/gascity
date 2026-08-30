package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prePushFixture is a throwaway git repo wired to run the real .githooks/pre-push
// against fake `bd` and `make` binaries.
type prePushFixture struct {
	repo      string
	binDir    string
	bdStdin   string
	makeRuns  string
	commitOld string
	commitNew string
	env       []string
}

// newPrePushFixture builds a repo with two commits — the second adds a Go file —
// so the hook's `git diff -- '*.go'` scan has something real to resolve.
func newPrePushFixture(t *testing.T) *prePushFixture {
	t.Helper()
	root := repoRoot(t)
	repo := t.TempDir()
	binDir := t.TempDir()
	recordDir := t.TempDir()

	f := &prePushFixture{
		repo:     repo,
		binDir:   binDir,
		bdStdin:  filepath.Join(recordDir, "bd-stdin"),
		makeRuns: filepath.Join(recordDir, "make-runs"),
	}
	f.env = append(os.Environ(),
		"PATH="+binDir+":/usr/bin:/bin",
		"GIT_CONFIG_GLOBAL="+filepath.Join(recordDir, "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(recordDir, "gitconfig-system"),
		"BD_STDIN_RECORD="+f.bdStdin,
		"MAKE_RECORD="+f.makeRuns,
	)

	// `bd` records exactly what the chain handed it on stdin.
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env sh
cat > "$BD_STDIN_RECORD"
`)
	// `make` records that the push-time suite was reached.
	writeExecutable(t, filepath.Join(binDir, "make"), `#!/usr/bin/env sh
printf '%s\n' "$*" >> "$MAKE_RECORD"
`)

	for _, dir := range []string{".githooks/lib", "scripts"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, rel := range []string{".githooks/pre-push", ".githooks/lib/beads-chain.sh"} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		writeExecutable(t, filepath.Join(repo, rel), string(body))
	}
	// The ownership guard talks to a live bead store; stub it so this test
	// isolates the hook's stdin composition.
	writeExecutable(t, filepath.Join(repo, "scripts", "push-ownership-guard.sh"), `#!/usr/bin/env bash
assert_bead_still_claimed() { return 0; }
`)

	f.git(t, "init", "-q", "-b", "main")
	f.git(t, "config", "user.email", "test@example.com")
	f.git(t, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	f.git(t, "add", "-A")
	f.git(t, "commit", "-q", "--no-verify", "-m", "base")
	f.commitOld = f.gitOut(t, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(repo, "touched.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatalf("write go file: %v", err)
	}
	f.git(t, "add", "-A")
	f.git(t, "commit", "-q", "--no-verify", "-m", "add go file")
	f.commitNew = f.gitOut(t, "rev-parse", "HEAD")

	return f
}

func (f *prePushFixture) git(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.repo
	cmd.Env = f.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (f *prePushFixture) gitOut(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.repo
	cmd.Env = f.env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// run invokes the fixture's pre-push hook with the given ref lines on stdin.
func (f *prePushFixture) run(t *testing.T, stdin string) (int, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(f.repo, ".githooks", "pre-push"), "origin", "git@example.com:fixture.git")
	cmd.Dir = f.repo
	cmd.Env = f.env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run pre-push: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), string(out)
}

func (f *prePushFixture) read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// TestPrePushReplaysRefListToBeadsAndSuiteScan pins the composition introduced
// with the beads chain: git delivers the ref list on stdin exactly once, and
// both consumers — the beads pre-push hook and the Go-change scan that gates
// the push-time suite — must see it. Reading it in only one place silently
// disables the other.
func TestPrePushReplaysRefListToBeadsAndSuiteScan(t *testing.T) {
	f := newPrePushFixture(t)
	refLine := "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}

	if got := f.read(t, f.bdStdin); got != refLine {
		t.Fatalf("beads received stdin %q, want %q", got, refLine)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite not reached (make invocations = %q); the Go-change scan lost its stdin", got)
	}
}

// TestPrePushSkipsSuiteForBranchDeletion keeps the pre-existing carve-out
// intact through the stdin rework: a deletion has nothing to test, but beads
// must still see the ref line.
func TestPrePushSkipsSuiteForBranchDeletion(t *testing.T) {
	f := newPrePushFixture(t)
	zero := strings.Repeat("0", 40)
	refLine := "(delete) " + zero + " refs/heads/gone " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}

	if got := f.read(t, f.bdStdin); got != refLine {
		t.Fatalf("beads received stdin %q, want %q", got, refLine)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran for a branch deletion: %q", got)
	}
}

// TestPrePushHandlesEmptyRefList guards the emptiness check around the replay:
// re-terminating an empty buffer would manufacture a blank ref line, which the
// scan would read as a real non-deletion push.
func TestPrePushHandlesEmptyRefList(t *testing.T) {
	f := newPrePushFixture(t)

	code, out := f.run(t, "")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0 for an empty ref list\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran with no refs pushed: %q", got)
	}
}

// TestPrePushPropagatesBeadsRejection keeps beads authoritative at push time:
// its rejection must abort before the suite runs.
func TestPrePushPropagatesBeadsRejection(t *testing.T) {
	f := newPrePushFixture(t)
	writeExecutable(t, filepath.Join(f.binDir, "bd"), `#!/usr/bin/env sh
cat > "$BD_STDIN_RECORD"
echo "beads rejected this push" >&2
exit 9
`)
	refLine := "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 9 {
		t.Fatalf("pre-push exit = %d, want 9 (beads rejection must abort the push)\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran after a beads rejection: %q", got)
	}
}
