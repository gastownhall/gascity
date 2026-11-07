package beadstest

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// EnvBeadsTestMode is the environment variable bd's own metrics/spawn.go
// checks (inTestMode / shouldSpawnFlusher) to skip launching the detached
// send-metrics child that otherwise races t.TempDir's RemoveAll for
// $HOME/.beads/eventsData/eventkit.lock (gastownhall/beads#5032). The pinned
// bd (v1.3.1) honors it; the retrying removal below stays as the
// backstop for a bd that does not.
//
// bd's storage layer reads the same flag as a hard guard, so it is not free for
// a workspace bound to a Dolt server (bd init --server-port): with it set to
// "1", bd resolves that workspace to the 127.0.0.1:1 sentinel instead of the
// port the workspace recorded. Runners for such workspaces must override it to
// "0" (BdSubprocessEnv lets a caller's override win).
const EnvBeadsTestMode = "BEADS_TEST_MODE"

const (
	// RemoveRetryAttempts is the retry budget for retryRemoveAll. It is wider
	// than internal/doctor's original 10-attempt/50ms (500ms) guard: ga-aik16g
	// recurred a third time under fleet-load contention with that budget, so
	// this trades a longer worst-case (only ever paid when a removal is
	// actually still contended) for headroom the narrower guard lacked.
	RemoveRetryAttempts = 30
	removeRetryDelay    = 100 * time.Millisecond
)

// retryRemoveAll calls remove(dir) until it reports success or the attempt
// budget runs out, pausing delay between tries but not after the last one.
// It returns nil once a removal succeeds, and otherwise the final failure so
// the caller can report the give-up rather than discard it.
func retryRemoveAll(dir string, remove func(string) error, attempts int, delay time.Duration) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		lastErr = remove(dir)
		if lastErr == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(delay)
		}
	}
	return lastErr
}

// retryRemoveAllForTest retries remove briefly to absorb a lingering
// embedded-dolt/eventkit background writer that can hold files open a
// few dozen ms to a few hundred ms past the owning bd subprocess's apparent
// exit — which otherwise races t.TempDir()'s single-shot RemoveAll cleanup
// with an intermittent "directory not empty" error. It logs rather than
// fails on a final give-up, so TempDir's own best-effort cleanup still gets
// the last word while a future red run can still tell an insufficient guard
// from a missing one.
func retryRemoveAllForTest(t testing.TB, dir string, remove func(string) error) {
	t.Helper()
	if err := retryRemoveAll(dir, remove, RemoveRetryAttempts, removeRetryDelay); err != nil {
		t.Logf("guarded removal of %s exhausted %d attempts: %v", dir, RemoveRetryAttempts, err)
	}
}

// GuardedTempDir returns a t.TempDir() whose removal is retried by
// retryRemoveAllForTest. Registering the cleanup after t.TempDir() has
// registered its own means LIFO ordering runs the retrying removal first,
// leaving TempDir's single-shot RemoveAll nothing to trip over. Every temp
// dir a real bd subprocess writes into needs this.
func GuardedTempDir(t testing.TB) string {
	t.Helper()
	return GuardedTempDirWith(t, os.RemoveAll)
}

// GuardedTempDirWith is GuardedTempDir with the removal call injected. The
// registration is the whole point of the helper and yet is invisible to a
// dir-is-gone assertion, because t.TempDir() removes an idle dir on its own;
// injecting the removal is what lets a test observe that the cleanup was
// registered at all. Ordinary callers want GuardedTempDir.
func GuardedTempDirWith(t testing.TB, remove func(string) error) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { retryRemoveAllForTest(t, dir, remove) })
	return dir
}

// TestOwnedHome pins HOME to a fresh guarded temp dir for the duration of the
// test and returns it. bd's config precedence falls through, as a last
// resort, to $HOME/.beads/config.yaml, so only a test-owned HOME keeps a
// machine-level dolt.shared-server setting out of the bd subprocesses these
// tests spawn. bd then writes $HOME/.beads/ itself, which is why that dir
// needs the same retrying removal as the working dir.
func TestOwnedHome(t testing.TB) string {
	t.Helper()
	home := GuardedTempDir(t)
	t.Setenv("HOME", home)
	return home
}

// baitFiles are the files of the bait workspace GuardedBdWorkspaceDir plants:
// project files and no database. bd adopts a directory like this: `bd init`
// below it initializes into it, and `bd config` below it runs against it.
// Nothing in it is valid for use, so a bd that adopts it leaves files beside
// these, which is the escape the end-of-test check reports.
var baitFiles = map[string]string{
	"config.yaml":   "# bait workspace planted by beadstest.GuardedBdWorkspaceDir\n",
	"metadata.json": `{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"bait","project_id":"11111111-2222-3333-4444-555555555555"}`,
}

// plantBait writes the bait workspace at root/.beads and returns its path.
func plantBait(t testing.TB, root string) string {
	t.Helper()
	baitDir := filepath.Join(root, ".beads")
	if err := os.Mkdir(baitDir, 0o700); err != nil {
		t.Fatalf("plant bait workspace: %v", err)
	}
	for name, body := range baitFiles {
		if err := os.WriteFile(filepath.Join(baitDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("plant bait workspace: %v", err)
		}
	}
	return baitDir
}

// baitDisturbance reports what bd left in the bait workspace at baitDir, or ""
// when it holds exactly the planted files with their planted contents. A bd
// command that adopted the bait writes its own files beside them (a database,
// version and gate files) or rewrites a planted file in place.
func baitDisturbance(baitDir string) string {
	entries, err := os.ReadDir(baitDir)
	if err != nil {
		return fmt.Sprintf("could not be read back: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := slices.Sorted(maps.Keys(baitFiles))
	if !slices.Equal(names, want) {
		return fmt.Sprintf("holds %v, planted %v", names, want)
	}
	for _, name := range want {
		got, err := os.ReadFile(filepath.Join(baitDir, name))
		if err != nil {
			return fmt.Sprintf("could not read back planted %s: %v", name, err)
		}
		if string(got) != baitFiles[name] {
			return fmt.Sprintf("rewrote planted %s", name)
		}
	}
	return ""
}

// markGitRoot makes dir the root of a git repository without running git. bd
// bounds its workspace walk at the git root it gets from git, and git accepts
// a HEAD, an objects dir and a refs dir as a repository.
func markGitRoot(t testing.TB, dir string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	for _, sub := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(gitDir, sub), 0o700); err != nil {
			t.Fatalf("mark %s as a git root: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("mark %s as a git root: %v", dir, err)
	}
}

// GuardedBdWorkspaceDir returns a dir for a real bd subprocess to `bd init`
// and run in, isolated from every directory above it. Use it, not
// GuardedTempDir, for any dir bd will treat as a workspace.
//
// bd finds its workspace by walking up from the working directory toward / for
// a .beads that holds project files, and the git root is the only thing that
// bounds the walk. A temp dir that is not its own git root therefore inherits
// whatever .beads sits above it. An ancestor with project files but no
// database is adopted: `bd init` in the temp dir exits 0 having initialized
// the ANCESTOR, and later bd commands there run against it. An ancestor that
// is a full workspace makes `bd init` abort as already initialized and lets
// `bd config` succeed through it. Both shapes were seen on one host, where a
// stray /var/tmp/.beads failed every real-bd test below it (ga-l7otw9).
//
// The returned dir is its own git root, which stops the walk at the dir. It is
// also the child of a bait workspace (project files, no database): bd reaching
// the bait is a failure of that isolation, so it fails the caller in every run
// and not only on a polluted host. The bait is checked when the test ends,
// and a bd that wrote into it is reported with what it left there.
func GuardedBdWorkspaceDir(t testing.TB) string {
	t.Helper()
	return guardedBdWorkspaceDirWith(t, t.Errorf)
}

// guardedBdWorkspaceDirWith is GuardedBdWorkspaceDir with the escape report
// injected. Failing a test from its own cleanup is invisible to an assertion,
// so injecting the report is what lets a test observe that the check ran at
// all. Ordinary callers want GuardedBdWorkspaceDir.
func guardedBdWorkspaceDirWith(t testing.TB, errorf func(format string, args ...any)) string {
	t.Helper()
	root := GuardedTempDir(t)
	baitDir := plantBait(t, root)
	dir := filepath.Join(root, "workspace")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create bd workspace dir: %v", err)
	}
	markGitRoot(t, dir)
	t.Cleanup(func() {
		if left := baitDisturbance(baitDir); left != "" {
			errorf("a bd command escaped %s and reached the ancestor workspace %s, which %s", dir, baitDir, left)
		}
	})
	return dir
}

// BdSubprocessEnv builds an env map for a real bd subprocess, defaulting
// EnvBeadsTestMode to "1" while letting any caller-supplied override for that
// key win — the default is applied first and overrides are layered on top,
// never the reverse. A runner for a workspace bound to a Dolt server must
// override the default to "0"; see EnvBeadsTestMode.
func BdSubprocessEnv(overrides map[string]string) map[string]string {
	env := map[string]string{EnvBeadsTestMode: "1"}
	for k, v := range overrides {
		env[k] = v
	}
	return env
}
