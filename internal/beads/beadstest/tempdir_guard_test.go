package beadstest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// TestGuardedBdWorkspaceDirIsItsOwnGitRoot pins the isolation itself: bd bounds
// its upward workspace walk at the git root git reports, so a dir holding a
// HEAD, an objects dir and a refs dir of its own is where the walk stops.
// Dropping markGitRoot from GuardedBdWorkspaceDir turns this red, and with it
// every real-bd test that relies on the bait below being out of reach.
func TestGuardedBdWorkspaceDirIsItsOwnGitRoot(t *testing.T) {
	t.Run("workspace", func(t *testing.T) {
		dir := GuardedBdWorkspaceDir(t)
		head, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
		if err != nil {
			t.Fatalf("workspace dir %s is not a git root: %v", dir, err)
		}
		if got, want := string(head), "ref: refs/heads/main\n"; got != want {
			t.Fatalf(".git/HEAD = %q, want %q", got, want)
		}
		for _, sub := range []string{"objects", "refs"} {
			info, err := os.Stat(filepath.Join(dir, ".git", sub))
			if err != nil || !info.IsDir() {
				t.Fatalf(".git/%s is not a directory (stat err=%v)", sub, err)
			}
		}
	})
}

// TestGuardedBdWorkspaceDirSitsBelowABaitWorkspace pins the bait: project files
// that make bd adopt the parent as a workspace, and no database behind them.
func TestGuardedBdWorkspaceDirSitsBelowABaitWorkspace(t *testing.T) {
	t.Run("workspace", func(t *testing.T) {
		bait := filepath.Join(filepath.Dir(GuardedBdWorkspaceDir(t)), ".beads")
		config, err := os.ReadFile(filepath.Join(bait, "config.yaml"))
		if err != nil || len(config) == 0 {
			t.Fatalf("bait config.yaml must exist and be non-empty: len=%d err=%v", len(config), err)
		}
		raw, err := os.ReadFile(filepath.Join(bait, "metadata.json"))
		if err != nil {
			t.Fatalf("bait metadata.json: %v", err)
		}
		var meta struct {
			DoltDatabase string `json:"dolt_database"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil || meta.DoltDatabase == "" {
			t.Fatalf("bait metadata.json must name a dolt_database: %q (err=%v)", raw, err)
		}
		if _, err := os.Stat(filepath.Join(bait, "embeddeddolt")); !os.IsNotExist(err) {
			t.Fatalf("bait must have no database behind it, but stat of embeddeddolt returned err=%v", err)
		}
		if left := baitDisturbance(bait); left != "" {
			t.Fatalf("a freshly planted bait reports it %s", left)
		}
	})
}

func TestBaitDisturbanceNamesWhatBdLeftBehind(t *testing.T) {
	bait := plantBait(t, t.TempDir())
	if got := baitDisturbance(bait); got != "" {
		t.Fatalf("an untouched bait reports %q, want nothing", got)
	}

	if err := os.Mkdir(filepath.Join(bait, "embeddeddolt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := baitDisturbance(bait); !strings.Contains(got, "embeddeddolt") {
		t.Fatalf("a bait with a database beside the planted files reports %q, want it to name embeddeddolt", got)
	}

	lost := plantBait(t, t.TempDir())
	if err := os.Remove(filepath.Join(lost, "metadata.json")); err != nil {
		t.Fatal(err)
	}
	if got := baitDisturbance(lost); got == "" {
		t.Fatal("a bait that lost a planted file reports nothing, want the difference")
	}

	rewritten := plantBait(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(rewritten, "config.yaml"), []byte("issue-prefix: escaped\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := baitDisturbance(rewritten); !strings.Contains(got, "config.yaml") {
		t.Fatalf("a bait whose planted config.yaml was rewritten in place reports %q, want it to name config.yaml", got)
	}

	if got := baitDisturbance(filepath.Join(bait, "gone")); got == "" {
		t.Fatal("an unreadable bait reports nothing, want the read failure")
	}
}

// TestGuardedBdWorkspaceDirReportsAnEscapeWhenTheTestEnds pins the wiring
// between the check and the helper that the test above covers separately:
// that guardedBdWorkspaceDirWith registers it. Failing a test from its own
// cleanup is invisible to an assertion, so the report is injected. Deleting the
// registration leaves the first report list empty and turns this red.
func TestGuardedBdWorkspaceDirReportsAnEscapeWhenTheTestEnds(t *testing.T) {
	var reports []string
	record := func(format string, args ...any) { reports = append(reports, fmt.Sprintf(format, args...)) }

	t.Run("escaped", func(t *testing.T) {
		dir := guardedBdWorkspaceDirWith(t, record)
		// What a bd that adopted the bait leaves there: its database, beside
		// the files that were planted.
		if err := os.Mkdir(filepath.Join(filepath.Dir(dir), ".beads", "embeddeddolt"), 0o700); err != nil {
			t.Fatal(err)
		}
	})
	if len(reports) != 1 || !strings.Contains(reports[0], "embeddeddolt") {
		t.Fatalf("escape reports = %q, want exactly one naming embeddeddolt", reports)
	}

	reports = nil
	t.Run("contained", func(t *testing.T) { guardedBdWorkspaceDirWith(t, record) })
	if len(reports) != 0 {
		t.Fatalf("a bd that never left its workspace was reported as escaping: %q", reports)
	}
}
