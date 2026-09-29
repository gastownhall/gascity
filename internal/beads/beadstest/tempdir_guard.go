package beadstest

import (
	"os"
	"testing"
	"time"
)

// EnvBeadsTestMode is the environment variable bd's own metrics/spawn.go
// checks (inTestMode / shouldSpawnFlusher) to skip launching the detached
// send-metrics child that otherwise races t.TempDir's RemoveAll for
// $HOME/.beads/eventsData/eventkit.lock (gastownhall/beads#5032). The pinned
// bd (v1.3.1-rc.2) honors it; the retrying removal below stays as the
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
