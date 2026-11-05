package beadstest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRetryRemoveAllRetriesUntilRemovalSucceeds(t *testing.T) {
	calls := 0
	err := retryRemoveAll(t.TempDir(), func(string) error {
		calls++
		if calls < 3 {
			return errors.New("directory not empty")
		}
		return nil
	}, RemoveRetryAttempts, 0)
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

// TestGuardedTempDirRegistersTheRetryingRemoval pins the wiring between the two
// halves the tests above cover separately: that GuardedTempDirWith registers
// the retrying removal on the dir it hands back. Deleting that registration
// drives calls to 0 and turns this red, which no dir-is-gone assertion can do,
// since t.TempDir() removes an idle dir on its own. The injected remove
// deliberately never removes anything: TempDir's own cleanup still clears the
// dir. That the GuardedTempDir wrapper every real caller uses reaches this
// seam with a real removal is pinned separately by
// TestGuardedTempDirRemovalRunsBeforeTempDirsOwnCleanup.
func TestGuardedTempDirRegistersTheRetryingRemoval(t *testing.T) {
	calls := 0
	t.Run("guarded", func(t *testing.T) {
		GuardedTempDirWith(t, func(string) error {
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
// the contract — the returned dir is test-scoped and gone once the owning test
// finishes. The retry half is covered by the retryRemoveAll tests above, since
// a single RemoveAll of an idle dir succeeds on the first attempt; the wiring
// between the two halves is covered by
// TestGuardedTempDirRegistersTheRetryingRemoval for the seam and by
// TestGuardedTempDirRemovalRunsBeforeTempDirsOwnCleanup for the wrapper.
func TestGuardedTempDirRemovesItsDirWhenTheTestEnds(t *testing.T) {
	var dir string
	t.Run("guarded", func(t *testing.T) {
		dir = GuardedTempDir(t)
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
		home = TestOwnedHome(t)
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

// TestGuardedTempDirRemovalRunsBeforeTempDirsOwnCleanup pins the wrapper half:
// that GuardedTempDir itself reaches the seam with a real removal. It
// sandwiches a probe cleanup between TempDir's base RemoveAll (registered by
// the deliberate first t.TempDir() call) and GuardedTempDir's retry cleanup,
// so LIFO runs retry -> probe -> base and the probe observes whether the
// guarded removal ran. Bypassing the seam (return t.TempDir()) or injecting an
// inert remove both leave the dir standing and turn this red; no other test
// here catches either shape.
func TestGuardedTempDirRemovalRunsBeforeTempDirsOwnCleanup(t *testing.T) {
	var removedBeforeBase bool
	t.Run("guarded", func(t *testing.T) {
		_ = t.TempDir() // first TempDir call: pins the base RemoveAll below ours
		var dir string
		t.Cleanup(func() {
			_, err := os.Stat(dir)
			removedBeforeBase = os.IsNotExist(err)
		})
		dir = GuardedTempDir(t)
	})
	if !removedBeforeBase {
		t.Fatal("GuardedTempDir's cleanup did not remove the dir before TempDir's own cleanup ran")
	}
}

// TestBdSubprocessEnvSetsBeadsTestMode pins that every bd-subprocess env map
// built through this helper carries BEADS_TEST_MODE=1 — the flag bd's own
// metrics/spawn.go checks (inTestMode/shouldSpawnFlusher) to skip launching
// the detached send-metrics child that otherwise races t.TempDir's RemoveAll
// for $HOME/.beads/eventsData/eventkit.lock (gastownhall/beads#5032).
func TestBdSubprocessEnvSetsBeadsTestMode(t *testing.T) {
	env := BdSubprocessEnv(map[string]string{"BEADS_DIR": "/tmp/example/.beads"})
	if got := env[EnvBeadsTestMode]; got != "1" {
		t.Fatalf("BdSubprocessEnv()[%q] = %q, want \"1\"", EnvBeadsTestMode, got)
	}
	if got := env["BEADS_DIR"]; got != "/tmp/example/.beads" {
		t.Fatalf("BdSubprocessEnv() dropped caller override BEADS_DIR = %q", got)
	}
}

// TestBdSubprocessEnvOverrideCanDisableTestMode pins that an explicit caller
// override still wins over the default — BdSubprocessEnv sets the default
// first and layers overrides on top, not the reverse — so a future call site
// that must exercise bd without test-mode relaxations is not stuck.
func TestBdSubprocessEnvOverrideCanDisableTestMode(t *testing.T) {
	env := BdSubprocessEnv(map[string]string{EnvBeadsTestMode: "0"})
	if got := env[EnvBeadsTestMode]; got != "0" {
		t.Fatalf("BdSubprocessEnv() override = %q, want caller's \"0\" to win over the default", got)
	}
}
