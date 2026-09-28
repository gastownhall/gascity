//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package storebinding

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// migrationGuardHangBudget bounds the helper-readiness wait below — no assertion depends on
// how long it takes, so this is a hang detector, not a latency assertion, and raising it does
// not slow a passing run because the wait returns the instant "ready" arrives.
// testutil.ExecRaceTimeout (10s) is a floor, not a target (TESTING.md "Floors, ceilings, and
// inputs"): the helper re-execs the test binary and must clear Go runtime + package init, flag
// parse and test enumeration, and an AcquireMigrationGuard call before it can print its ready
// line. The sibling storebinding/sqlite fence helper follows the same re-exec-then-signal shape
// and was measured taking up to 3.02s at 48 concurrent children under memory pressure (ga-ap7lpd),
// which is what motivated its own hang budget (ga-sptey3); this one has no SQLite open/schema
// work in the path, so it is expected to be cheaper, not more expensive. Mirrors the precedent in
// cmd/gc/hangbudget_test.go (hangBudget = 6 * testutil.GoroutineRaceTimeout); 6x ExecRaceTimeout
// is 60s, well under the 20m gate package timeout.
const migrationGuardHangBudget = 6 * testutil.ExecRaceTimeout

func TestAcquireMigrationGuardClaimsAreRefCountedAndBoundToCityGeneration(t *testing.T) {
	directory := testMigrationGuardDirectory(t)
	guard, err := AcquireMigrationGuard(context.Background(), directory, Generation(7))
	if err != nil {
		t.Fatalf("AcquireMigrationGuard(): %v", err)
	}
	first, err := guard.claim(context.Background())
	if err != nil {
		t.Fatalf("first Claim(): %v", err)
	}
	second, err := guard.claim(context.Background())
	if err != nil {
		t.Fatalf("second Claim(): %v", err)
	}
	identity, err := first.Identity()
	if err != nil {
		t.Fatalf("first claim Identity(): %v", err)
	}
	if identity.Directory() != directory || identity.Generation() != Generation(7) {
		t.Fatalf("claim identity = directory:%q generation:%d, want directory:%q generation:%d", identity.Directory(), identity.Generation(), directory, Generation(7))
	}
	if err := guard.Release(); !errors.Is(err, ErrMigrationGuardClaimsHeld) {
		t.Fatalf("Release() error = %v, want ErrMigrationGuardClaimsHeld", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("first claim Release(): %v", err)
	}
	if err := guard.Release(); !errors.Is(err, ErrMigrationGuardClaimsHeld) {
		t.Fatalf("Release() after one claim error = %v, want ErrMigrationGuardClaimsHeld", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("second claim Release(): %v", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("Release() after claims: %v", err)
	}
	if _, err := first.Identity(); !errors.Is(err, ErrInvalidMigrationGuardClaim) {
		t.Fatalf("released claim Identity() error = %v, want ErrInvalidMigrationGuardClaim", err)
	}
	if _, err := guard.claim(context.Background()); !errors.Is(err, ErrMigrationGuardReleased) {
		t.Fatalf("claim() after Release error = %v, want ErrMigrationGuardReleased", err)
	}
}

func TestMigrationGuardFileReleaserRetriesOnlyPendingStages(t *testing.T) {
	t.Run("unlock succeeds before close fails", func(t *testing.T) {
		closeFailure := errors.New("close failure")
		unlockCalls := 0
		closeCalls := 0
		releaser := newMigrationGuardFileReleaserWithOperations(
			func() error {
				unlockCalls++
				return nil
			},
			func() error {
				closeCalls++
				if closeCalls == 1 {
					return closeFailure
				}
				return nil
			},
		)

		if err := releaser.release(); !errors.Is(err, closeFailure) {
			t.Fatalf("first release error = %v, want close failure", err)
		}
		if err := releaser.release(); err != nil {
			t.Fatalf("retry release: %v", err)
		}
		if unlockCalls != 1 {
			t.Fatalf("unlock calls = %d, want 1", unlockCalls)
		}
		if closeCalls != 2 {
			t.Fatalf("close calls = %d, want 2", closeCalls)
		}
	})

	t.Run("close succeeds after unlock fails", func(t *testing.T) {
		unlockFailure := errors.New("unlock failure")
		unlockCalls := 0
		closeCalls := 0
		releaser := newMigrationGuardFileReleaserWithOperations(
			func() error {
				unlockCalls++
				return unlockFailure
			},
			func() error {
				closeCalls++
				return nil
			},
		)

		if err := releaser.release(); !errors.Is(err, unlockFailure) {
			t.Fatalf("first release error = %v, want unlock failure", err)
		}
		if err := releaser.release(); err != nil {
			t.Fatalf("retry release: %v", err)
		}
		if unlockCalls != 1 {
			t.Fatalf("unlock calls = %d after successful close, want 1", unlockCalls)
		}
		if closeCalls != 1 {
			t.Fatalf("close calls = %d after successful close, want 1", closeCalls)
		}
	})
}

func TestRejectedMigrationGuardCleanupRetainsRetryOwnershipBeforeFlock(t *testing.T) {
	rejected := errors.New("reject before lock acquisition")
	cleanupFailure := errors.New("close rejected directory")
	closeCalls := 0
	_, err := closeRejectedMigrationGuard(rejected, migrationGuardReleaserFunc(func() error {
		closeCalls++
		if closeCalls == 1 {
			return cleanupFailure
		}
		return nil
	}))
	var cleanup *RejectedMigrationGuardCleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("rejected cleanup error = %T, want *RejectedMigrationGuardCleanupError", err)
	}
	if !errors.Is(err, rejected) || !errors.Is(err, cleanupFailure) {
		t.Fatalf("rejected cleanup error = %v, want rejection and cleanup failure", err)
	}
	if err := cleanup.RetryCleanup(); err != nil {
		t.Fatalf("RetryCleanup(): %v", err)
	}
	if closeCalls != 2 {
		t.Fatalf("close calls = %d, want 2", closeCalls)
	}
}

func TestAcquireMigrationGuardReturnsCleanupCapableGuardAfterCanceledAcquisitionCleanupFails(t *testing.T) {
	directory := testMigrationGuardDirectory(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	unlockFailure := errors.New("unlock after canceled acquisition")
	closeFailure := errors.New("close after canceled acquisition")
	unlockCalls := 0
	closeCalls := 0
	guard, err := acquireMigrationGuardWithReleaser(ctx, directory, Generation(1), func(*os.File) migrationGuardReleaser {
		cancel()
		return newMigrationGuardFileReleaserWithOperations(
			func() error {
				unlockCalls++
				if unlockCalls == 1 {
					return unlockFailure
				}
				return nil
			},
			func() error {
				closeCalls++
				if closeCalls == 1 {
					return closeFailure
				}
				return nil
			},
		)
	})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, unlockFailure) || !errors.Is(err, closeFailure) {
		t.Fatalf("acquire error = %v, want cancellation, unlock failure, and close failure", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard.Release() retry: %v", err)
	}
	if unlockCalls != 2 || closeCalls != 2 {
		t.Fatalf("retry calls = unlock:%d close:%d, want 2 each", unlockCalls, closeCalls)
	}
}

func TestAcquireWriterFenceRejectsReplacedMigrationGuardDirectoryBeforeProviderMutation(t *testing.T) {
	directory := testMigrationGuardDirectory(t)
	guard, err := AcquireMigrationGuard(context.Background(), directory, Generation(6))
	if err != nil {
		t.Fatalf("AcquireMigrationGuard(): %v", err)
	}
	originalClaim, err := guard.claim(context.Background())
	if err != nil {
		t.Fatalf("guard.Claim(): %v", err)
	}
	originalIdentity, err := originalClaim.Identity()
	if err != nil {
		t.Fatalf("original claim Identity(): %v", err)
	}

	if err := os.Rename(directory, filepath.Join(filepath.Dir(directory), ".gc-replaced")); err != nil {
		t.Fatalf("replace guarded directory: %v", err)
	}
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatalf("create replacement city directory: %v", err)
	}
	replacement, err := AcquireMigrationGuard(context.Background(), directory, Generation(6))
	if err != nil {
		t.Fatalf("AcquireMigrationGuard() replacement directory: %v", err)
	}
	replacementClaim, err := replacement.claim(context.Background())
	if err != nil {
		t.Fatalf("replacement Claim(): %v", err)
	}
	replacementIdentity, err := replacementClaim.Identity()
	if err != nil {
		t.Fatalf("replacement claim Identity(): %v", err)
	}
	if originalIdentity.Equal(replacementIdentity) {
		t.Fatal("guard identities remained equal after replacing the guarded .gc directory")
	}
	if originalClaim.Held() {
		t.Fatal("original claim remained held after replacing its guarded .gc directory")
	}
	if err := replacementClaim.Release(); err != nil {
		t.Fatalf("replacement claim Release(): %v", err)
	}
	if err := replacement.Release(); err != nil {
		t.Fatalf("replacement guard Release(): %v", err)
	}

	provider := &recordingProvider{fence: &recordingFence{target: testFenceTarget(t), role: FenceRoleSource, generation: Generation(6), held: true}}
	_, err = AcquireWriterFence(context.Background(), guard, provider, FenceRequest{
		Target:             testFenceTarget(t),
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if !errors.Is(err, ErrMigrationGuardIdentityChanged) {
		t.Fatalf("AcquireWriterFence() after city directory replacement error = %v, want ErrMigrationGuardIdentityChanged", err)
	}
	if provider.mutations != 0 {
		t.Fatalf("AcquireWriterFence() called provider %d times after city directory replacement", provider.mutations)
	}
	if err := originalClaim.Release(); err != nil {
		t.Fatalf("original claim Release(): %v", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard Release(): %v", err)
	}
}

func TestRejectedWriterFenceCleanupReleasesOwnedClaimAfterDirectoryReplacement(t *testing.T) {
	directory := testMigrationGuardDirectory(t)
	guard, err := AcquireMigrationGuard(context.Background(), directory, Generation(6))
	if err != nil {
		t.Fatalf("AcquireMigrationGuard(): %v", err)
	}
	target := testFenceTarget(t)
	acquireErr := errors.New("provider returned partial fence")
	firstReleaseErr := errors.New("first fence cleanup failed")
	fence := &cleanupOwnershipFence{
		target:        target,
		role:          FenceRoleSource,
		generation:    Generation(6),
		held:          true,
		releaseErrors: []error{firstReleaseErr, nil},
	}
	_, err = AcquireWriterFence(context.Background(), guard, &partialFenceAcquirer{fence: fence, err: acquireErr}, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{ComponentID("graph")},
		Role:               FenceRoleSource,
	})
	var cleanup *RejectedWriterFenceCleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("AcquireWriterFence() error = %T, want *RejectedWriterFenceCleanupError", err)
	}

	if err := os.Rename(directory, filepath.Join(filepath.Dir(directory), ".gc-replaced-during-cleanup")); err != nil {
		t.Fatalf("replace guarded directory: %v", err)
	}
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatalf("create replacement city directory: %v", err)
	}
	if err := cleanup.RetryCleanup(context.Background()); err != nil {
		t.Fatalf("RetryCleanup(): %v", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard.Release() after retry cleanup: %v", err)
	}
}

func TestAcquireMigrationGuardExcludesSecondProcessAndRecoversAfterSIGKILL(t *testing.T) {
	directory := testMigrationGuardDirectory(t)
	command := exec.Command(os.Args[0], "-test.run=^TestMigrationGuardHelperProcess$", "--", directory)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin pipe: %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout pipe: %v", err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start migration guard helper: %v", err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		_ = stdin.Close()
	})

	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() {
			ready <- fmt.Errorf("helper did not report readiness: %w", scanner.Err())
			return
		}
		if scanner.Text() != "ready" {
			ready <- fmt.Errorf("helper readiness = %q, want ready", scanner.Text())
			return
		}
		ready <- nil
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(migrationGuardHangBudget):
		t.Fatal("helper did not acquire its migration guard")
	}
	if err := command.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("helper exited before busy-lock probe: %v", err)
	}

	manifest := filepath.Join(directory, "migration-manifest.json")
	if err := os.WriteFile(manifest, []byte("old manifest"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	replacement := manifest + ".replacement"
	if err := os.WriteFile(replacement, []byte("replacement manifest"), 0o600); err != nil {
		t.Fatalf("write replacement manifest: %v", err)
	}
	if err := os.Rename(replacement, manifest); err != nil {
		t.Fatalf("replace manifest: %v", err)
	}
	if _, err := AcquireMigrationGuard(context.Background(), directory, Generation(9)); !errors.Is(err, ErrMigrationGuardBusy) {
		t.Fatalf("AcquireMigrationGuard() while helper holds stable directory lock error = %v, want ErrMigrationGuardBusy", err)
	}

	if err := command.Process.Kill(); err != nil {
		t.Fatalf("kill migration guard helper: %v", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("migration guard helper exited cleanly after SIGKILL")
	}
	waited = true

	guard, err := AcquireMigrationGuard(context.Background(), directory, Generation(9))
	if err != nil {
		t.Fatalf("AcquireMigrationGuard() after helper crash: %v", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("Release() after helper crash: %v", err)
	}
}

func TestMigrationGuardHelperProcess(t *testing.T) {
	directory, helper := migrationGuardHelperDirectory(os.Args)
	if !helper {
		return
	}
	guard, err := AcquireMigrationGuard(context.Background(), directory, Generation(9))
	if err != nil {
		t.Fatalf("helper AcquireMigrationGuard(): %v", err)
	}
	defer func() { _ = guard.Release() }()
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatalf("helper report readiness: %v", err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("helper wait for parent stdin: %v", err)
	}
}

func migrationGuardHelperDirectory(args []string) (string, bool) {
	for index, argument := range args {
		if argument != "--" || index+1 >= len(args) {
			continue
		}
		return args[index+1], true
	}
	return "", false
}

// cleanupOwnershipFence models a partial provider fence whose successful inner
// release does not own the outer city-guard claim. The generic rejection path
// must release that still-owned claim itself.
type cleanupOwnershipFence struct {
	target        FenceTarget
	role          FenceRole
	generation    Generation
	held          bool
	releaseErrors []error
}

func (f *cleanupOwnershipFence) Target() FenceTarget    { return f.target.Clone() }
func (f *cleanupOwnershipFence) Role() FenceRole        { return f.role }
func (f *cleanupOwnershipFence) Generation() Generation { return f.generation }
func (f *cleanupOwnershipFence) CoveredComponents() []ComponentID {
	components := make([]ComponentID, len(f.target.Components))
	for index, component := range f.target.Components {
		components[index] = component.ID
	}
	return components
}
func (f *cleanupOwnershipFence) Held(context.Context) (bool, error) { return f.held, nil }
func (f *cleanupOwnershipFence) Release(context.Context) error {
	f.held = false
	if len(f.releaseErrors) == 0 {
		return nil
	}
	err := f.releaseErrors[0]
	f.releaseErrors = f.releaseErrors[1:]
	return err
}
