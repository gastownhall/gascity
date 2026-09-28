package beadstest

import (
	"testing"
	"time"
)

// EnvBeadsTestMode is the environment variable bd's own metrics/spawn.go
// checks (inTestMode / shouldSpawnFlusher) to skip launching the detached
// send-metrics child that otherwise races t.TempDir's RemoveAll for
// $HOME/.beads/eventsData/eventkit.lock (gastownhall/beads#5032).
const EnvBeadsTestMode = "BEADS_TEST_MODE"

// RemoveRetryAttempts is a placeholder pending ga-etb0b3's GREEN step, which
// must widen it past the 10-attempt/50ms budget internal/doctor's original
// guard used — ga-aik16g's third occurrence found that budget still
// insufficient under fleet-load contention.
//
// TODO(ga-etb0b3): replace with the real, widened attempt budget.
const RemoveRetryAttempts = 0

// retryRemoveAll is a placeholder pending ga-etb0b3's GREEN step — it must
// retry remove(dir) until it succeeds or the attempt budget is exhausted,
// pausing delay between tries. Left as a single unconditional call, the RED
// tests fail at their own assertions (call count, returned error) rather
// than at compile time, so RED stays compatible with this repo's pre-commit
// typecheck gate (make lint-changed rejects any commit that doesn't build).
func retryRemoveAll(dir string, remove func(string) error, _ int, _ time.Duration) error {
	return remove(dir)
}

// GuardedTempDir is a placeholder pending ga-etb0b3's GREEN step — it must
// return a t.TempDir() whose removal is retried by retryRemoveAll before
// t.TempDir()'s own single-shot RemoveAll can race a lingering writer. Left
// as a bare t.TempDir(), it registers no retrying cleanup.
func GuardedTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// GuardedTempDirWith is a placeholder pending ga-etb0b3's GREEN step — see
// GuardedTempDir. Ordinary callers want GuardedTempDir; this variant exists
// so a test can observe that the retrying removal was actually registered.
func GuardedTempDirWith(t *testing.T, _ func(string) error) string {
	t.Helper()
	return t.TempDir()
}

// TestOwnedHome is a placeholder pending ga-etb0b3's GREEN step — it must pin
// HOME to a fresh GuardedTempDir for the duration of the test and return it.
// Left as a bare TempDir with HOME untouched.
func TestOwnedHome(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// BdSubprocessEnv is a placeholder pending ga-etb0b3's GREEN step — it must
// default EnvBeadsTestMode to "1" in the returned map while letting any
// caller-supplied override for that key win. Left as an identity passthrough,
// it never adds the default.
func BdSubprocessEnv(overrides map[string]string) map[string]string {
	return overrides
}
