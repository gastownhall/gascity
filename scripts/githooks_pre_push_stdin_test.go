package scripts_test

import (
	"fmt"
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
	workerEnv *prePushWorkerEnv // set by withFakeBazel
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
		// The developer's own mode choice must not leak into the fixture.
		"GC_PREPUSH_SUITE=",
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
	for _, rel := range []string{".githooks/pre-push", ".githooks/lib/beads-chain.sh", ".githooks/lib/push-suite.sh"} {
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
	cmd := testCommand("git", args...)
	cmd.Dir = f.repo
	cmd.Env = f.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (f *prePushFixture) gitOut(t *testing.T, args ...string) string {
	t.Helper()
	cmd := testCommand("git", args...)
	cmd.Dir = f.repo
	cmd.Env = f.env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// run invokes the fixture's pre-push hook with the given ref lines on stdin,
// as a push to the named remote origin.
func (f *prePushFixture) run(t *testing.T, stdin string) (int, string) {
	t.Helper()
	return f.runAs(t, "origin", "git@example.com:fixture.git", stdin)
}

// runAs invokes the hook with git's two arguments: the remote's name (the URL
// as typed, for a push to a URL) and its URL.
func (f *prePushFixture) runAs(t *testing.T, remote, url, stdin string) (int, string) {
	t.Helper()
	cmd := testCommand(filepath.Join(f.repo, ".githooks", "pre-push"), remote, url)
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

func TestPrePushRunsSuiteForLargeGoDiff(t *testing.T) {
	f := newPrePushFixture(t)
	// Exceed a pipe buffer so an early-exiting grep cannot appear safe just
	// because git managed to finish writing a small list before it exited.
	for i := 0; i < 1024; i++ {
		name := fmt.Sprintf("%04d-%s.go", i, strings.Repeat("x", 180))
		if err := os.WriteFile(filepath.Join(f.repo, name), []byte("package fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.git(t, "add", "-A")
	f.git(t, "commit", "-q", "--no-verify", "-m", "large Go change")
	head := f.gitOut(t, "rev-parse", "HEAD")
	code, out := f.run(t, "refs/heads/main "+head+" refs/heads/main "+f.commitOld+"\n")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("large Go diff skipped push-time suite: %q\n%s", got, out)
	}
}

// TestPrePushRefusesUnfetchedRemoteTip covers the everyday way to reach the
// scan without the remote's tip: someone else pushed and this clone never
// fetched. git still runs the hook for a non-forced push and reports "fetch
// first" only after the hook returns, so the hook's refusal is all the user
// sees and must name the fetch that fixes it. The fetch names the ref, which a
// plain `git fetch origin` can skip.
func TestPrePushRefusesUnfetchedRemoteTip(t *testing.T) {
	f := newPrePushFixture(t)
	unfetched := strings.Repeat("1", 40)
	code, out := f.run(t, "refs/heads/main "+f.commitNew+" refs/heads/main "+unfetched+"\n")
	if code != 1 {
		t.Fatalf("pre-push exit = %d, want 1 for an unfetched remote tip\n%s", code, out)
	}
	for _, want := range []string{"refs/heads/main is at " + unfetched + " on origin", "run 'git fetch origin refs/heads/main' and push again"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pre-push refusal lacks %q\n%s", want, out)
		}
	}
	// The remedy must be the only refusal: falling through to the diff would
	// add git's `bad object` and a second, generic refusal.
	for _, unwanted := range []string{"cannot inspect", "bad object"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("pre-push went on to diff an unfetched remote tip (output has %q)\n%s", unwanted, out)
		}
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran over an unfetched remote tip: %q", got)
	}
}

// TestPrePushRefusalStripsPushURLCredentials covers a push to a URL, which git
// hands the hook as typed: the refusal must not echo a credential in it. An
// http(s) user name can be the token itself, so that whole userinfo goes;
// elsewhere only the password does, keeping the user an ssh fetch needs.
func TestPrePushRefusalStripsPushURLCredentials(t *testing.T) {
	const secret = "s3cr3t-token"
	f := newPrePushFixture(t)
	unfetched := strings.Repeat("1", 40)
	for _, tc := range []struct {
		name, url, want string
	}{
		{"https user and password", "https://user:" + secret + "@example.invalid/repo.git", "https://example.invalid/repo.git"},
		{"https token as user name", "https://" + secret + "@example.invalid/repo.git", "https://example.invalid/repo.git"},
		{"https token with placeholder password", "https://" + secret + ":x-oauth-basic@example.invalid/repo.git", "https://example.invalid/repo.git"},
		{"http password containing at sign", "http://user:" + secret + "@part@example.invalid/repo.git", "http://example.invalid/repo.git"},
		{"ssh password keeps user", "ssh://git:" + secret + "@example.invalid/repo.git", "ssh://git@example.invalid/repo.git"},
		{"ssh password containing at sign", "ssh://git:" + secret + "@part@example.invalid/repo.git", "ssh://git@example.invalid/repo.git"},
		{"ssh user and port unchanged", "ssh://git@example.invalid:2222/repo.git", "ssh://git@example.invalid:2222/repo.git"},
		{"https port unchanged", "https://example.invalid:8443/repo.git", "https://example.invalid:8443/repo.git"},
		{"scp-style unchanged", "git@example.invalid:o/repo.git", "git@example.invalid:o/repo.git"},
		{"local path unchanged", "/srv/git/repo.git", "/srv/git/repo.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := f.runAs(t, tc.url, tc.url, "refs/heads/main "+f.commitNew+" refs/heads/main "+unfetched+"\n")
			if code != 1 {
				t.Fatalf("pre-push exit = %d, want 1 for an unfetched remote tip\n%s", code, out)
			}
			if strings.Contains(out, secret) {
				t.Fatalf("pre-push refusal leaked the push URL's credential\n%s", out)
			}
			for _, want := range []string{" on " + tc.want + ", which", "run 'git fetch " + tc.want + " refs/heads/main' and push again"} {
				if !strings.Contains(out, want) {
					t.Fatalf("pre-push refusal lacks %q\n%s", want, out)
				}
			}
		})
	}
}

// TestPrePushDiffsAgainstPresentNonCommitRemoteTip pins the presence check: a
// remote ref may name a tree this clone already has. A `^{commit}` peel fails
// on it and would send the user to fetch something no fetch can change.
func TestPrePushDiffsAgainstPresentNonCommitRemoteTip(t *testing.T) {
	f := newPrePushFixture(t)
	tree := f.gitOut(t, "rev-parse", f.commitOld+"^{tree}")
	code, out := f.run(t, "refs/tags/snapshot "+f.commitNew+" refs/tags/snapshot "+tree+"\n")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0 for a present tree tip\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("Go change over a tree tip skipped the push-time suite: %q\n%s", got, out)
	}
}

// TestPrePushFailsWhenGoDiffCannotBeRead keeps every other diff failure fail
// closed with the hook's own message. The remote tip is present here, so the
// failure is the comparison itself, not a missing fetch.
func TestPrePushFailsWhenGoDiffCannotBeRead(t *testing.T) {
	f := newPrePushFixture(t)
	missing := strings.Repeat("2", 40)
	code, out := f.run(t, "refs/heads/main "+missing+" refs/heads/main "+f.commitOld+"\n")
	if code != 1 {
		t.Fatalf("pre-push exit = %d, want 1 for an unreadable Go diff\n%s", code, out)
	}
	if want := "pre-push: cannot inspect Go changes for " + f.commitOld + ".." + missing; !strings.Contains(out, want) {
		t.Fatalf("pre-push refusal lacks %q\n%s", want, out)
	}
	if strings.Contains(out, "git fetch") {
		t.Fatalf("pre-push blamed a missing fetch for a present remote tip\n%s", out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran despite an unreadable comparison: %q", got)
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
