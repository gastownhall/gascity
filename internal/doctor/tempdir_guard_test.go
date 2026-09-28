package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	removeRetryAttempts = 10
	removeRetryDelay    = 50 * time.Millisecond
)

// retryRemoveAllForTest retries remove briefly to absorb a lingering
// embedded-dolt background writer that can hold files open a few dozen ms
// past the owning bd subprocess's apparent exit — which otherwise races
// t.TempDir()'s single-shot RemoveAll cleanup with an intermittent
// "directory not empty" error. It logs rather than fails on a final give-up,
// so TempDir's own best-effort cleanup still gets the last word while a
// future red run can still tell an insufficient guard from a missing one.
// cmd/gc dodges the same lingering-writer hazard by placement instead — see
// doltIdentityHomeDir (ga-7dgcg6).
func retryRemoveAllForTest(t *testing.T, dir string, remove func(string) error) {
	t.Helper()
	if err := retryRemoveAll(dir, remove, removeRetryAttempts, removeRetryDelay); err != nil {
		t.Logf("guarded removal of %s exhausted %d attempts: %v", dir, removeRetryAttempts, err)
	}
}

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

// guardedTempDir returns a t.TempDir() whose removal is retried by
// retryRemoveAllForTest. Registering the cleanup after t.TempDir() has
// registered its own means LIFO ordering runs the retrying removal first,
// leaving TempDir's single-shot RemoveAll nothing to trip over. Every temp
// dir a bd subprocess writes into needs this, so bead ga-531fk — where the
// dir pinned as HOME was the one missing the guard and failed a Mac CI run
// after its test body had already passed — cannot repeat: the guard now
// comes with the directory instead of being a second line to remember.
func guardedTempDir(t *testing.T) string {
	t.Helper()
	return guardedTempDirWith(t, os.RemoveAll)
}

// guardedTempDirWith is guardedTempDir with the removal call injected. The
// registration is the whole point of the helper and yet is invisible to a
// dir-is-gone assertion, because t.TempDir() removes an idle dir on its own;
// injecting the removal is what lets a test observe that the cleanup was
// registered at all. Ordinary callers want guardedTempDir.
func guardedTempDirWith(t *testing.T, remove func(string) error) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { retryRemoveAllForTest(t, dir, remove) })
	return dir
}

// testOwnedHome pins HOME to a fresh guarded temp dir for the duration of the
// test and returns it. bd's config precedence falls through, as a last
// resort, to $HOME/.beads/config.yaml, so only a test-owned HOME keeps a
// machine-level dolt.shared-server setting out of the bd subprocesses these
// tests spawn (ga-zxpfic). bd then writes $HOME/.beads/ itself, which is why
// that dir needs the same retrying removal as the working dir.
func testOwnedHome(t *testing.T) string {
	t.Helper()
	home := guardedTempDir(t)
	t.Setenv("HOME", home)
	return home
}

func TestRetryRemoveAllRetriesUntilRemovalSucceeds(t *testing.T) {
	calls := 0
	err := retryRemoveAll(t.TempDir(), func(string) error {
		calls++
		if calls < 3 {
			return errors.New("directory not empty")
		}
		return nil
	}, removeRetryAttempts, 0)
	if calls != 3 {
		t.Fatalf("remove called %d times, want 3 (two failures, then success)", calls)
	}
	if err != nil {
		t.Fatalf("retryRemoveAll returned %v, want nil once a removal succeeds", err)
	}
}

func TestRetryRemoveAllStopsAtItsAttemptBudget(t *testing.T) {
	calls := 0
	wantErr := errors.New("directory not empty")
	err := retryRemoveAll(t.TempDir(), func(string) error {
		calls++
		return wantErr
	}, 4, 0)
	if calls != 4 {
		t.Fatalf("remove called %d times, want 4 (the attempt budget)", calls)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("retryRemoveAll returned %v, want the final failure %v", err, wantErr)
	}
}

// TestGuardedTempDirRegistersTheRetryingRemoval pins the wiring between the
// two halves the tests below and above cover separately: that
// guardedTempDirWith registers the retrying removal on the dir it hands
// back. Deleting that registration — exactly the pre-ga-531fk shape — drives
// calls to 0 and turns this red, which no dir-is-gone assertion can do,
// since t.TempDir() removes an idle dir on its own. The injected remove
// deliberately never removes anything: TempDir's own cleanup still clears
// the dir. That the guardedTempDir wrapper every real caller uses reaches
// this seam with a real removal is pinned separately by
// TestGuardedTempDirRemovalRunsBeforeTempDirsOwnCleanup.
func TestGuardedTempDirRegistersTheRetryingRemoval(t *testing.T) {
	calls := 0
	t.Run("guarded", func(t *testing.T) {
		guardedTempDirWith(t, func(string) error {
			calls++
			if calls < 3 {
				return errors.New("directory not empty")
			}
			return nil
		})
	})
	if calls != 3 {
		t.Fatalf("registered remove called %d times, want 3 (two failures, then success)", calls)
	}
}

// TestGuardedTempDirRemovesItsDirWhenTheTestEnds pins the structural half of
// the contract — the returned dir is test-scoped and gone once the owning
// test finishes. The retry half is covered by the retryRemoveAll tests above,
// since a single RemoveAll of an idle dir succeeds on the first attempt; the
// wiring between the two halves is covered by
// TestGuardedTempDirRegistersTheRetryingRemoval for the seam and by
// TestGuardedTempDirRemovalRunsBeforeTempDirsOwnCleanup for the wrapper.
func TestGuardedTempDirRemovesItsDirWhenTheTestEnds(t *testing.T) {
	var dir string
	t.Run("guarded", func(t *testing.T) {
		dir = guardedTempDir(t)
		if err := os.WriteFile(filepath.Join(dir, "leftover"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Stat(%s) after the subtest returned err=%v, want the dir removed", dir, err)
	}
}

func TestTestOwnedHomePinsHOMEToAGuardedTempDir(t *testing.T) {
	var home string
	t.Run("pinned", func(t *testing.T) {
		home = testOwnedHome(t)
		if got := os.Getenv("HOME"); got != home {
			t.Fatalf("HOME = %q, want the test-owned dir %q", got, home)
		}
		if err := os.MkdirAll(filepath.Join(home, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("Stat(%s) after the subtest returned err=%v, want the test-owned HOME removed", home, err)
	}
}

// TestGuardedTempDirRemovalRunsBeforeTempDirsOwnCleanup pins the wrapper
// half: that guardedTempDir itself reaches the seam with a real removal. It
// sandwiches a probe cleanup between TempDir's base RemoveAll (registered by
// the deliberate first t.TempDir() call) and guardedTempDir's retry cleanup,
// so LIFO runs retry -> probe -> base and the probe observes whether the
// guarded removal ran. Bypassing the seam (return t.TempDir()) or injecting
// an inert remove both leave the dir standing and turn this red; no other
// test here catches either shape.
func TestGuardedTempDirRemovalRunsBeforeTempDirsOwnCleanup(t *testing.T) {
	var removedBeforeBase bool
	t.Run("guarded", func(t *testing.T) {
		_ = t.TempDir() // first TempDir call: pins the base RemoveAll below ours
		var dir string
		t.Cleanup(func() {
			_, err := os.Stat(dir)
			removedBeforeBase = os.IsNotExist(err)
		})
		dir = guardedTempDir(t)
	})
	if !removedBeforeBase {
		t.Fatal("guardedTempDir's cleanup did not remove the dir before TempDir's own cleanup ran")
	}
}
