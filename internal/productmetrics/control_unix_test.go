//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/testutil"
)

type purgeCallResult struct {
	result PurgeResult
	err    error
}

type uploadCallResult struct {
	result uploadRunResult
	err    error
}

func TestDisableAndPurgeInvalidAbsentAndUnavailableState(t *testing.T) {
	t.Run("nil service", func(t *testing.T) {
		var service *Service
		result, err := service.DisableAndPurge(context.Background())
		requirePurgeErrorClass(t, err, PurgeErrorInvalidRequest)
		if result.Outcome != PurgeFailed || result.DisabledDurable {
			t.Fatalf("nil-service result = %+v", result)
		}
	})
	t.Run("nil context", func(t *testing.T) {
		home := newMetricsTestHome(t)
		service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
		result, err := service.DisableAndPurge(nil) //nolint:staticcheck // A nil context is the contract under test.
		requirePurgeErrorClass(t, err, PurgeErrorInvalidRequest)
		if result.Outcome != PurgeFailed || result.DisabledDurable {
			t.Fatalf("nil-context result = %+v", result)
		}
	})
	t.Run("absent state", func(t *testing.T) {
		home := newMetricsTestHome(t)
		service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
		result, err := service.DisableAndPurge(context.Background())
		if err != nil || result.Outcome != PurgeCompleted || !result.DisabledDurable ||
			result.IncompletePhase != PurgeIncompleteNone || result.ManualCleanupRequired || result.ManualCleanupReason != PurgeManualCleanupNone {
			t.Fatalf("absent-state result = %+v err=%v", result, err)
		}
		requireCleanDisabledTree(t, home)
	})
	t.Run("unavailable home", func(t *testing.T) {
		home := newMetricsTestHome(t)
		deps := defaultTestServiceDependencies(home, 2)
		deps.homeErr = errors.New("injected unavailable home")
		deps.homeReason = ReasonHomeUnstable
		service := mustOpenTestService(t, deps)
		result, err := service.DisableAndPurge(context.Background())
		requirePurgeErrorClass(t, err, PurgeErrorStorage)
		if result.Outcome != PurgeFailed || result.DisabledDurable {
			t.Fatalf("unavailable-home result = %+v", result)
		}
	})
}

func TestDisableAndPurgeCommitsBeforeUploaderWaitWithoutHoldingStateLock(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	uploader, err := root.acquireLock(context.Background(), uploaderLockName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = uploader.Release() }()
	defer func() { _ = root.Close() }()
	service.deps.disableUploaderWait = 2 * time.Second
	attempts := make(chan struct{}, 1)
	service.deps.beforeDisableUploaderLock = func() { attempts <- struct{}{} }

	call := startDisableAndPurge(t, service)
	receiveUploaderAttempt(t, attempts)
	pending := waitForMetricsState(t, home, func(state persistedState) bool {
		return state.Preference == preferenceDisabled && state.CleanupKind == cleanupDisable &&
			state.InstallationID == "" && state.SpoolGeneration == ""
	})
	if pending.PausedThroughMetricsEpoch != 0 {
		t.Fatalf("disable barrier retained pause epoch: %#v", pending)
	}

	probeRoot := mustOpenMutableRoot(t, home)
	probeContext, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	probe, probeErr := probeRoot.acquireLock(probeContext, stateLockName)
	cancel()
	if probeErr != nil {
		t.Fatalf("off held state lock while waiting for uploader: %v", probeErr)
	}
	if err := probe.Release(); err != nil {
		t.Fatal(err)
	}
	if err := probeRoot.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case early := <-call:
		t.Fatalf("off stopped waiting before the two-second uploader bound: %+v err=%v", early.result, early.err)
	default:
	}

	outcome := receivePurgeCall(t, call)
	requirePurgeErrorClass(t, outcome.err, PurgeErrorUploaderQuiescence)
	if outcome.result.Outcome != PurgeCleanupPending || !outcome.result.DisabledDurable {
		t.Fatalf("uploader timeout result = %+v", outcome.result)
	}
	if outcome.result.IncompletePhase != PurgeIncompleteUploaderQuiescence ||
		outcome.result.ManualCleanupRequired || outcome.result.ManualCleanupReason != PurgeManualCleanupNone {
		t.Fatalf("uploader timeout guidance = %+v", outcome.result)
	}
	if after := readStateFixture(t, home); after != pending {
		t.Fatalf("uploader timeout changed disable owner:\nbefore=%#v\nafter=%#v", pending, after)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
}

func TestDisableAndPurgeBoundsInitialAndPostUploaderStateLocks(t *testing.T) {
	t.Run("initial state lock", func(t *testing.T) {
		home, service, _ := newRecordServiceFixture(t, testEventIDThree)
		before := readStateFixture(t, home)
		root := mustOpenMutableRoot(t, home)
		locked, err := root.acquireLock(context.Background(), stateLockName)
		if err != nil {
			t.Fatal(err)
		}
		service.deps.disableStateWait = 100 * time.Millisecond
		result, err := service.DisableAndPurge(context.Background())
		requirePurgeErrorClass(t, err, PurgeErrorDisableWrite)
		if result.Outcome != PurgeFailed || result.DisabledDurable {
			t.Fatalf("initial state timeout = %+v err=%v", result, err)
		}
		if result.IncompletePhase != PurgeIncompleteDisableWrite || result.ManualCleanupRequired ||
			result.ManualCleanupReason != PurgeManualCleanupNone {
			t.Fatalf("initial state timeout guidance = %+v", result)
		}
		if after := readStateFixture(t, home); after != before {
			t.Fatalf("initial state timeout mutated state:\nbefore=%#v\nafter=%#v", before, after)
		}
		if closeErr := errors.Join(locked.Release(), root.Close()); closeErr != nil {
			t.Fatal(closeErr)
		}
	})

	t.Run("post-uploader state lock", func(t *testing.T) {
		home, service, _ := newRecordServiceFixture(t, testEventIDThree)
		service.deps.disableStateWait = 100 * time.Millisecond
		atUploader := make(chan struct{})
		releaseUploaderAttempt := make(chan struct{})
		service.deps.beforeDisableUploaderLock = func() {
			close(atUploader)
			<-releaseUploaderAttempt
		}
		call := startDisableAndPurge(t, service)
		select {
		case <-atUploader:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("off did not reach uploader phase")
		}
		pending := readStateFixture(t, home)
		if pending.Preference != preferenceDisabled || pending.CleanupKind != cleanupDisable {
			t.Fatalf("post-uploader contention state = %#v", pending)
		}
		root := mustOpenMutableRoot(t, home)
		locked, err := root.acquireLock(context.Background(), stateLockName)
		if err != nil {
			t.Fatal(err)
		}
		close(releaseUploaderAttempt)
		outcome := receivePurgeCall(t, call)
		requirePurgeErrorClass(t, outcome.err, PurgeErrorStorage)
		if outcome.result.Outcome != PurgeCleanupPending || !outcome.result.DisabledDurable {
			t.Fatalf("post-uploader state timeout = %+v err=%v", outcome.result, outcome.err)
		}
		if outcome.result.IncompletePhase != PurgeIncompleteLocalCleanup || outcome.result.ManualCleanupRequired ||
			outcome.result.ManualCleanupReason != PurgeManualCleanupNone {
			t.Fatalf("post-uploader state timeout guidance = %+v", outcome.result)
		}
		if after := readStateFixture(t, home); after != pending {
			t.Fatalf("post-uploader timeout changed owner:\nbefore=%#v\nafter=%#v", pending, after)
		}
		if closeErr := errors.Join(locked.Release(), root.Close()); closeErr != nil {
			t.Fatal(closeErr)
		}
	})
}

func TestDisableAndPurgeBlockedUploaderConvergesAndCleanDisabledCrossesBarrier(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	rootTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa14)))
	writeJournaledRootTempCrashFixture(t, root, filepath.Base(rootTemp), []byte(testInstallationID), 0)
	uploader, err := root.acquireLock(context.Background(), uploaderLockName)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.disableUploaderWait = testutil.GoroutineRaceTimeout
	call := startDisableAndPurge(t, service)
	pending := waitForMetricsState(t, home, func(state persistedState) bool {
		return state.Preference == preferenceDisabled && state.CleanupKind == cleanupDisable
	})
	assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
	if err := uploader.Release(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	outcome := receivePurgeCall(t, call)
	if outcome.err != nil || outcome.result.Outcome != PurgeCompleted || !outcome.result.DisabledDurable || outcome.result.RemovedEvents != 1 ||
		outcome.result.RemovedBytes != uint64(len(data)) {
		t.Fatalf("blocked-uploader success = %+v err=%v", outcome.result, outcome.err)
	}
	clean := requireCleanDisabledTree(t, home)
	if clean.StateGeneration == pending.StateGeneration || clean.CleanupEpoch != pending.CleanupEpoch {
		t.Fatalf("cleanup completion did not install exact successor: pending=%#v clean=%#v", pending, clean)
	}
	if _, err := os.Lstat(rootTemp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("successful off retained root identity temp: %v", err)
	}

	barrierRoot := mustOpenMutableRoot(t, home)
	barrier, err := barrierRoot.acquireLock(context.Background(), uploaderLockName)
	if err != nil {
		t.Fatal(err)
	}
	second := startDisableAndPurge(t, service)
	freshOwner := waitForMetricsState(t, home, func(state persistedState) bool {
		return state.Preference == preferenceDisabled && state.CleanupKind == cleanupDisable &&
			(state.StateGeneration != clean.StateGeneration || state.CleanupEpoch != clean.CleanupEpoch)
	})
	select {
	case early := <-second:
		t.Fatalf("already-clean off bypassed uploader barrier: %+v err=%v", early.result, early.err)
	default:
	}
	if err := barrier.Release(); err != nil {
		t.Fatal(err)
	}
	if err := barrierRoot.Close(); err != nil {
		t.Fatal(err)
	}
	already := receivePurgeCall(t, second)
	if already.err != nil || already.result.Outcome != PurgeAlreadyDisabled || !already.result.DisabledDurable {
		t.Fatalf("already-disabled barrier result = %+v err=%v", already.result, already.err)
	}
	final := requireCleanDisabledTree(t, home)
	if final.StateGeneration == freshOwner.StateGeneration {
		t.Fatalf("already-disabled cleanup owner was not completed: owner=%#v final=%#v", freshOwner, final)
	}
}

func TestDisableAndPurgeIncompleteBudgetRetainsOwnerAndLaterRetryReusesIt(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	service.deps.disableCleanupBudget = spoolWorkBudget{
		maxEntries: 1, maxDirectories: 1, maxReadBytes: 1, maxNameBytes: maximumStorageNameBytes,
	}
	first, err := service.DisableAndPurge(context.Background())
	requirePurgeErrorClass(t, err, PurgeErrorCleanupIncomplete)
	if first.Outcome != PurgeCleanupPending || !first.DisabledDurable {
		t.Fatalf("tiny-budget off = %+v", first)
	}
	owner := readStateFixture(t, home)
	if owner.Preference != preferenceDisabled || owner.CleanupKind != cleanupDisable {
		t.Fatalf("tiny-budget off lost cleanup owner: %#v", owner)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)

	service.deps.disableCleanupBudget = spoolWorkBudget{}
	barrierRoot := mustOpenMutableRoot(t, home)
	barrier, err := barrierRoot.acquireLock(context.Background(), uploaderLockName)
	if err != nil {
		t.Fatal(err)
	}
	attempts := make(chan struct{}, 1)
	service.deps.beforeDisableUploaderLock = func() { attempts <- struct{}{} }
	retry := startDisableAndPurge(t, service)
	receiveUploaderAttempt(t, attempts)
	if current := readStateFixture(t, home); current != owner {
		t.Fatalf("retry replaced existing cleanup owner while waiting:\nowner=%#v\ncurrent=%#v", owner, current)
	}
	if err := barrier.Release(); err != nil {
		t.Fatal(err)
	}
	if err := barrierRoot.Close(); err != nil {
		t.Fatal(err)
	}
	second := receivePurgeCall(t, retry)
	if second.err != nil || second.result.Outcome != PurgeCompleted || !second.result.DisabledDurable || second.result.RemovedEvents != 1 {
		t.Fatalf("cleanup retry = %+v err=%v", second.result, second.err)
	}
	requireCleanDisabledTree(t, home)
}

func TestDisableAndPurgeUnknownRootResidueStaysPendingUntilManualRemoval(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	unknownFile := filepath.Join(home.Root(), "user-notes")
	unknownDirectory := filepath.Join(home.Root(), "user-directory")
	nested := filepath.Join(unknownDirectory, "nested", "keep")
	if err := os.WriteFile(unknownFile, []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(nested), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := service.DisableAndPurge(context.Background())
	requirePurgeErrorClass(t, err, PurgeErrorCleanupIncomplete)
	if first.Outcome != PurgeCleanupPending || !first.DisabledDurable ||
		first.RemovedEvents != 0 || first.RemovedBytes != 0 {
		t.Fatalf("unknown-residue off = %+v err=%v", first, err)
	}
	if first.IncompletePhase != PurgeIncompleteLocalCleanup || !first.ManualCleanupRequired ||
		first.ManualCleanupReason != PurgeManualCleanupUnrecognizedRootEntry {
		t.Fatalf("unknown-residue guidance = %+v", first)
	}
	owner := readStateFixture(t, home)
	if owner.Preference != preferenceDisabled || owner.CleanupKind != cleanupDisable {
		t.Fatalf("unknown-residue off lost cleanup owner: %#v", owner)
	}
	for path, want := range map[string]string{unknownFile: "notes", nested: "keep"} {
		if data, readErr := os.ReadFile(path); readErr != nil || string(data) != want {
			t.Fatalf("unknown residue %q changed: data=%q err=%v", path, data, readErr)
		}
	}

	second, secondErr := service.DisableAndPurge(context.Background())
	requirePurgeErrorClass(t, secondErr, PurgeErrorCleanupIncomplete)
	if second.Outcome != PurgeCleanupPending || !second.DisabledDurable ||
		second.RemovedEvents != 0 || second.RemovedBytes != 0 {
		t.Fatalf("unknown-residue retry = %+v err=%v", second, secondErr)
	}
	if second.IncompletePhase != PurgeIncompleteLocalCleanup || !second.ManualCleanupRequired ||
		second.ManualCleanupReason != PurgeManualCleanupUnrecognizedRootEntry {
		t.Fatalf("unknown-residue retry guidance = %+v", second)
	}
	if retryOwner := readStateFixture(t, home); retryOwner != owner {
		t.Fatalf("unknown-residue retry replaced owner:\nfirst=%#v\nretry=%#v", owner, retryOwner)
	}
	if err := os.Remove(unknownFile); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(unknownDirectory); err != nil {
		t.Fatal(err)
	}
	completed, completeErr := service.DisableAndPurge(context.Background())
	if completeErr != nil || completed.Outcome != PurgeCompleted || !completed.DisabledDurable {
		t.Fatalf("manual-residue-removal retry = %+v err=%v", completed, completeErr)
	}
	requireCleanDisabledTree(t, home)
}

func TestDisableAndPurgeUnsafeKnownControlShapeRequiresManualCleanup(t *testing.T) {
	tests := []struct {
		name        string
		controlName string
		shape       string
	}{
		{name: "status symlink", controlName: statusFileName, shape: "symlink"},
		{name: "status directory", controlName: statusFileName, shape: "directory"},
		{name: "status hardlink", controlName: statusFileName, shape: "hardlink"},
		{name: "spawn throttle symlink", controlName: spawnThrottleFileName, shape: "symlink"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, service, _ := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}

			path := filepath.Join(home.Root(), test.controlName)
			const sentinel = "outside fixed-control cleanup authority"
			var verifyPreserved func()
			verifyExternalTarget := func() {}
			removeUnsafeEntry := func() error { return os.Remove(path) }
			switch test.shape {
			case "symlink":
				target := filepath.Join(filepath.Dir(home.Root()), strings.ReplaceAll(test.controlName, ".", "-")+"-outside")
				if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				verifyExternalTarget = func() {
					if data, err := os.ReadFile(target); err != nil || string(data) != sentinel {
						t.Fatalf("symlink target changed: data=%q err=%v", data, err)
					}
				}
				verifyPreserved = func() {
					info, err := os.Lstat(path)
					if err != nil || info.Mode()&os.ModeSymlink == 0 {
						t.Fatalf("unsafe control symlink changed: info=%v err=%v", info, err)
					}
					verifyExternalTarget()
				}
			case "directory":
				nested := filepath.Join(path, "nested", "keep")
				if err := os.MkdirAll(filepath.Dir(nested), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(nested, []byte(sentinel), 0o600); err != nil {
					t.Fatal(err)
				}
				verifyPreserved = func() {
					if data, err := os.ReadFile(nested); err != nil || string(data) != sentinel {
						t.Fatalf("unsafe control directory changed: data=%q err=%v", data, err)
					}
				}
				removeUnsafeEntry = func() error { return os.RemoveAll(path) }
			case "hardlink":
				target := filepath.Join(filepath.Dir(home.Root()), "status-hardlink-outside")
				if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(target, path); err != nil {
					t.Fatal(err)
				}
				verifyExternalTarget = func() {
					if data, err := os.ReadFile(target); err != nil || string(data) != sentinel {
						t.Fatalf("hardlink target changed: data=%q err=%v", data, err)
					}
				}
				verifyPreserved = func() {
					entryInfo, entryErr := os.Stat(path)
					targetInfo, targetErr := os.Stat(target)
					if entryErr != nil || targetErr != nil || !os.SameFile(entryInfo, targetInfo) {
						t.Fatalf("unsafe control hardlink changed: entry=%v target=%v entryErr=%v targetErr=%v",
							entryInfo, targetInfo, entryErr, targetErr)
					}
					verifyExternalTarget()
				}
			default:
				t.Fatalf("unknown unsafe control shape %q", test.shape)
			}

			first, err := service.DisableAndPurge(context.Background())
			requirePurgeErrorClass(t, err, PurgeErrorCleanupIncomplete)
			if first.Outcome != PurgeCleanupPending || !first.DisabledDurable ||
				first.RemovedEvents != 0 || first.RemovedBytes != 0 {
				t.Fatalf("unsafe %s %s off = %+v err=%v", test.controlName, test.shape, first, err)
			}
			verifyPreserved()
			if first.IncompletePhase != PurgeIncompleteLocalCleanup || !first.ManualCleanupRequired ||
				first.ManualCleanupReason != PurgeManualCleanupUnrecognizedRootEntry {
				t.Fatalf("unsafe %s %s guidance = %+v", test.controlName, test.shape, first)
			}
			owner := readStateFixture(t, home)
			if owner.Preference != preferenceDisabled || owner.CleanupKind != cleanupDisable {
				t.Fatalf("unsafe %s %s off lost cleanup owner: %#v", test.controlName, test.shape, owner)
			}

			second, secondErr := service.DisableAndPurge(context.Background())
			requirePurgeErrorClass(t, secondErr, PurgeErrorCleanupIncomplete)
			if second.Outcome != PurgeCleanupPending || !second.DisabledDurable ||
				second.IncompletePhase != PurgeIncompleteLocalCleanup || !second.ManualCleanupRequired ||
				second.ManualCleanupReason != PurgeManualCleanupUnrecognizedRootEntry {
				t.Fatalf("unsafe %s %s retry guidance = %+v err=%v", test.controlName, test.shape, second, secondErr)
			}
			if retryOwner := readStateFixture(t, home); retryOwner != owner {
				t.Fatalf("unsafe %s %s retry replaced owner:\nfirst=%#v\nretry=%#v",
					test.controlName, test.shape, owner, retryOwner)
			}
			verifyPreserved()

			if err := removeUnsafeEntry(); err != nil {
				t.Fatal(err)
			}
			verifyExternalTarget()
			completed, completeErr := service.DisableAndPurge(context.Background())
			if completeErr != nil || completed.Outcome != PurgeCompleted || !completed.DisabledDurable {
				t.Fatalf("unsafe %s %s manual-removal retry = %+v err=%v",
					test.controlName, test.shape, completed, completeErr)
			}
			verifyExternalTarget()
			if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("manual removal left unsafe %s %s: %v", test.controlName, test.shape, err)
			}
			requireCleanDisabledTree(t, home)
		})
	}
}

func TestDisableAndPurgeTransientDiagnosticStatusFailureRemainsRetryable(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	writeDiagnosticStatusToRoot(t, root, diagnosticStatus{droppedEvents: 2})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	statusPath := filepath.Join(home.Root(), statusFileName)
	injected := errors.New("injected transient diagnostic-status metadata failure")
	injectedOnce := false
	deps := defaultTestServiceDependencies(home, 2)
	deps.storageHooks.beforeMetadataAttempt = func(path string) error {
		if path == statusPath && !injectedOnce {
			injectedOnce = true
			return injected
		}
		return nil
	}
	service := mustOpenTestService(t, deps)

	first, err := service.DisableAndPurge(context.Background())
	requirePurgeErrorClass(t, err, PurgeErrorCleanupIncomplete)
	if !injectedOnce || !errors.Is(err, injected) || first.Outcome != PurgeCleanupPending || !first.DisabledDurable ||
		first.IncompletePhase != PurgeIncompleteLocalCleanup || first.ManualCleanupRequired ||
		first.ManualCleanupReason != PurgeManualCleanupNone {
		t.Fatalf("transient diagnostic-status failure = injected:%v result:%+v err=%v", injectedOnce, first, err)
	}
	owner := readStateFixture(t, home)
	if _, err := os.Lstat(statusPath); err != nil {
		t.Fatalf("transient diagnostic-status failure removed the safe record: %v", err)
	}

	retry, retryErr := service.DisableAndPurge(context.Background())
	if retryErr != nil || retry.Outcome != PurgeCompleted || !retry.DisabledDurable {
		t.Fatalf("transient diagnostic-status retry = %+v err=%v", retry, retryErr)
	}
	if retryOwner := readStateFixture(t, home); retryOwner == owner || retryOwner.CleanupKind != cleanupNone {
		t.Fatalf("transient diagnostic-status retry did not complete owner:\nfirst=%#v\nretry=%#v", owner, retryOwner)
	}
	if _, err := os.Lstat(statusPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("transient diagnostic-status retry left the safe record: %v", err)
	}
	requireCleanDisabledTree(t, home)
}

func TestDisableAndPurgeIntentWithMappedTempRequiresManualCleanup(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	backend, ok := root.backend.(*unixStorageDirectory)
	if !ok {
		t.Fatal("root-temp journal test requires Unix storage")
	}
	journal, err := backend.openRootTempJournal()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xc01))
	marker, err := createRootTempJournalMarker(backend, journal, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(marker.close(), journal.close()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.Root(), name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	result, purgeErr := service.DisableAndPurge(context.Background())
	requirePurgeErrorClass(t, purgeErr, PurgeErrorCleanupIncomplete)
	if result.Outcome != PurgeCleanupPending || !result.DisabledDurable ||
		result.IncompletePhase != PurgeIncompleteLocalCleanup || !result.ManualCleanupRequired ||
		result.ManualCleanupReason != PurgeManualCleanupUnsettledRootTempJournal {
		t.Fatalf("INTENT manual cleanup guidance = %+v err=%v", result, purgeErr)
	}
}

func TestDisableAndPurgeJournalPreemptsOverBudgetUnknownRootStarvation(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	priorQuota := spoolQuota{Events: 1, Bytes: 1}
	if err := persistSpoolQuota(root, priorQuota); err != nil {
		t.Fatal(err)
	}
	journaledTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xcff)))
	writeJournaledRootTempCrashFixture(t, root, filepath.Base(journaledTemp), []byte("journal authority"), 0)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	const unknownCount = 64
	unknownPaths := make([]string, 0, unknownCount)
	for index := 0; index < unknownCount; index++ {
		path := filepath.Join(home.Root(), fmt.Sprintf(".aaa-preserved-unknown-%03d", index))
		if err := os.WriteFile(path, []byte("unknown"), 0o600); err != nil {
			t.Fatal(err)
		}
		unknownPaths = append(unknownPaths, path)
	}
	budget := defaultSpoolWorkBudget()
	budget.maxEntries = spoolFixedEntryEnvelope + 32
	service.deps.disableCleanupBudget = budget

	first, err := service.DisableAndPurge(context.Background())
	requirePurgeErrorClass(t, err, PurgeErrorCleanupIncomplete)
	if first.Outcome != PurgeCleanupPending || !first.DisabledDurable ||
		first.RemovedEvents != 0 || first.RemovedBytes != 0 {
		t.Fatalf("root-budget off = %+v err=%v", first, err)
	}
	owner := readStateFixture(t, home)
	if owner.Preference != preferenceDisabled || owner.CleanupKind != cleanupDisable {
		t.Fatalf("root-budget off lost owner: %#v", owner)
	}
	if _, err := os.Lstat(journaledTemp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("over-budget unknown entries starved journaled temp replay: %v", err)
	}
	for _, path := range unknownPaths {
		if data, err := os.ReadFile(path); err != nil || string(data) != "unknown" {
			t.Fatalf("bounded root proof changed preserved unknown %q: data=%q err=%v", path, data, err)
		}
	}
	if quota := readQuotaFixture(t, home); quota != priorQuota {
		t.Fatalf("root-budget pending cleanup changed quota: %+v", quota)
	}

	for _, path := range unknownPaths {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	service.deps.disableCleanupBudget = spoolWorkBudget{}
	retry, retryErr := service.DisableAndPurge(context.Background())
	if retryErr != nil || retry.Outcome != PurgeCompleted || !retry.DisabledDurable ||
		retry.RemovedEvents != 0 || retry.RemovedBytes != 0 {
		t.Fatalf("root-budget retry = %+v err=%v", retry, retryErr)
	}
	requireCleanDisabledTree(t, home)
}

func TestDisableAndPurgeCanonicalRootTempUnlinkUncertainty(t *testing.T) {
	for _, uncertainty := range []string{"not-applied", "applied-sync-pending"} {
		t.Run(uncertainty, func(t *testing.T) {
			home, service, _ := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			canonicalTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xb01)))
			writeJournaledRootTempCrashFixture(t, root, filepath.Base(canonicalTemp), []byte("identity"), 0)
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected canonical root temp unlink uncertainty")
			armed := false
			failed := false
			service.deps.storageHooks.beforeMutation = func(step storageStep, path string) {
				if step == storageStepDelete && path == canonicalTemp {
					armed = true
				}
			}
			service.deps.storageHooks.beforeStep = func(step storageStep) error {
				if !armed || failed {
					return nil
				}
				if uncertainty == "not-applied" && step == storageStepDelete {
					failed = true
					return injected
				}
				if uncertainty == "applied-sync-pending" && step == storageStepDirectorySync {
					failed = true
					return injected
				}
				return nil
			}

			first, err := service.DisableAndPurge(context.Background())
			requirePurgeErrorClass(t, err, PurgeErrorCleanupIncomplete)
			if !errors.Is(err, injected) || first.Outcome != PurgeCleanupPending || !first.DisabledDurable ||
				first.RemovedEvents != 0 || first.RemovedBytes != 0 {
				t.Fatalf("%s canonical-temp off = %+v err=%v", uncertainty, first, err)
			}
			owner := readStateFixture(t, home)
			if owner.Preference != preferenceDisabled || owner.CleanupKind != cleanupDisable {
				t.Fatalf("%s canonical-temp off lost owner: %#v", uncertainty, owner)
			}
			_, statErr := os.Lstat(canonicalTemp)
			if uncertainty == "not-applied" && statErr != nil {
				t.Fatalf("not-applied unlink removed canonical temp: %v", statErr)
			}
			if uncertainty == "applied-sync-pending" && !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("applied unlink retained canonical temp: %v", statErr)
			}

			retry, retryErr := service.DisableAndPurge(context.Background())
			if retryErr != nil || retry.Outcome != PurgeCompleted || !retry.DisabledDurable {
				t.Fatalf("%s canonical-temp retry = %+v err=%v", uncertainty, retry, retryErr)
			}
			requireCleanDisabledTree(t, home)
		})
	}
}

func TestDisableAndPurgeCanonicalRootTempRevalidatesBeforeUnlink(t *testing.T) {
	for _, drift := range []string{"replacement", "mode", "link-count"} {
		t.Run(drift, func(t *testing.T) {
			home, service, _ := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			canonicalTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xb02)))
			writeJournaledRootTempCrashFixture(t, root, filepath.Base(canonicalTemp), []byte("original"), 0)
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			displaced := canonicalTemp + "-displaced"
			alias := canonicalTemp + "-alias"
			drifted := false
			service.deps.storageHooks.beforeMutation = func(step storageStep, path string) {
				if drifted || step != storageStepDelete || path != canonicalTemp {
					return
				}
				drifted = true
				switch drift {
				case "replacement":
					if err := os.Rename(canonicalTemp, displaced); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(canonicalTemp, []byte("replacement"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "mode":
					if err := os.Chmod(canonicalTemp, 0o644); err != nil {
						t.Fatal(err)
					}
				case "link-count":
					if err := os.Link(canonicalTemp, alias); err != nil {
						t.Fatal(err)
					}
				}
			}

			result, err := service.DisableAndPurge(context.Background())
			requirePurgeErrorClass(t, err, PurgeErrorCleanupIncomplete)
			if !drifted || result.Outcome != PurgeCleanupPending || !result.DisabledDurable ||
				result.RemovedEvents != 0 || result.RemovedBytes != 0 {
				t.Fatalf("%s canonical-temp drift = drifted:%v result:%+v err:%v", drift, drifted, result, err)
			}
			want := "original"
			if drift == "replacement" {
				want = "replacement"
			}
			if data, readErr := os.ReadFile(canonicalTemp); readErr != nil || string(data) != want {
				t.Fatalf("%s canonical-temp replacement changed: data=%q err=%v", drift, data, readErr)
			}
			owner := readStateFixture(t, home)
			if owner.Preference != preferenceDisabled || owner.CleanupKind != cleanupDisable {
				t.Fatalf("%s canonical-temp drift lost owner: %#v", drift, owner)
			}
		})
	}
}

func TestTwoConcurrentDisableAndPurgeCallsReuseOwnerAndCrossUploaderBarrier(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	barrier, err := root.acquireLock(context.Background(), uploaderLockName)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.disableUploaderWait = testutil.GoroutineRaceTimeout
	attempts := make(chan struct{}, 2)
	service.deps.beforeDisableUploaderLock = func() { attempts <- struct{}{} }

	firstCall := startDisableAndPurge(t, service)
	receiveUploaderAttempt(t, attempts)
	owner := waitForMetricsState(t, home, func(state persistedState) bool {
		return state.Preference == preferenceDisabled && state.CleanupKind == cleanupDisable
	})
	secondCall := startDisableAndPurge(t, service)
	receiveUploaderAttempt(t, attempts)
	if current := readStateFixture(t, home); current != owner {
		t.Fatalf("concurrent off replaced shared cleanup owner:\nowner=%#v\ncurrent=%#v", owner, current)
	}
	if err := barrier.Release(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	first := receivePurgeCall(t, firstCall)
	second := receivePurgeCall(t, secondCall)
	for index, outcome := range []purgeCallResult{first, second} {
		if outcome.err != nil || outcome.result.Outcome != PurgeCompleted || !outcome.result.DisabledDurable {
			t.Fatalf("concurrent off %d = %+v err=%v", index, outcome.result, outcome.err)
		}
	}
	if first.result.RemovedEvents+second.result.RemovedEvents != 1 ||
		first.result.RemovedBytes+second.result.RemovedBytes != uint64(len(data)) {
		t.Fatalf("concurrent removal totals = (%d, %d)",
			first.result.RemovedEvents+second.result.RemovedEvents,
			first.result.RemovedBytes+second.result.RemovedBytes)
	}
	requireCleanDisabledTree(t, home)
}

func TestConcurrentOffPendingObserverAcceptsPeerCompletionBeforeInitialStateLock(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	serviceA := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	serviceA.deps.disableCleanupBudget = spoolWorkBudget{
		maxEntries: 1, maxDirectories: 1, maxReadBytes: 1, maxNameBytes: maximumStorageNameBytes,
	}
	first, firstErr := serviceA.DisableAndPurge(context.Background())
	requirePurgeErrorClass(t, firstErr, PurgeErrorCleanupIncomplete)
	if first.Outcome != PurgeCleanupPending || !first.DisabledDurable {
		t.Fatalf("establish pending owner = %+v err=%v", first, firstErr)
	}
	owner := readStateFixture(t, home)
	serviceA.deps.disableCleanupBudget = spoolWorkBudget{}

	depsB := defaultTestServiceDependencies(home, 2)
	enteredStateLock := make(chan struct{})
	releaseStateLock := make(chan struct{})
	var blockFirst sync.Once
	depsB.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepLock {
			blockFirst.Do(func() {
				close(enteredStateLock)
				<-releaseStateLock
			})
		}
		return nil
	}
	serviceB := mustOpenTestService(t, depsB)
	secondCall := startDisableAndPurge(t, serviceB)
	select {
	case <-enteredStateLock:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("second off did not pause before initial state lock")
	}
	if current := readStateFixture(t, home); current != owner {
		t.Fatalf("pending observer basis changed early:\nowner=%#v\ncurrent=%#v", owner, current)
	}
	peer, peerErr := serviceA.DisableAndPurge(context.Background())
	if peerErr != nil || peer.Outcome != PurgeCompleted || !peer.DisabledDurable {
		t.Fatalf("peer completion = %+v err=%v", peer, peerErr)
	}
	close(releaseStateLock)
	second := receivePurgeCall(t, secondCall)
	if second.err != nil || second.result.Outcome != PurgeCompleted || !second.result.DisabledDurable {
		t.Fatalf("pending observer after peer completion = %+v err=%v", second.result, second.err)
	}
	requireCleanDisabledTree(t, home)
}

func TestConcurrentOffPendingObserverDoesNotClaimDurabilityAfterPeerEnable(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	serviceA := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	serviceA.deps.disableCleanupBudget = spoolWorkBudget{
		maxEntries: 1, maxDirectories: 1, maxReadBytes: 1, maxNameBytes: maximumStorageNameBytes,
	}
	first, firstErr := serviceA.DisableAndPurge(context.Background())
	requirePurgeErrorClass(t, firstErr, PurgeErrorCleanupIncomplete)
	if first.Outcome != PurgeCleanupPending || !first.DisabledDurable {
		t.Fatalf("establish pending owner = %+v err=%v", first, firstErr)
	}
	serviceA.deps.disableCleanupBudget = spoolWorkBudget{}

	depsB := defaultTestServiceDependencies(home, 2)
	enteredStateLock := make(chan struct{})
	releaseStateLock := make(chan struct{})
	var blockFirst sync.Once
	depsB.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepLock {
			blockFirst.Do(func() {
				close(enteredStateLock)
				<-releaseStateLock
			})
		}
		return nil
	}
	serviceB := mustOpenTestService(t, depsB)
	secondCall := startDisableAndPurge(t, serviceB)
	select {
	case <-enteredStateLock:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("second off did not pause before initial state lock")
	}
	peer, peerErr := serviceA.DisableAndPurge(context.Background())
	if peerErr != nil || peer.Outcome != PurgeCompleted {
		t.Fatalf("peer completion = %+v err=%v", peer, peerErr)
	}
	serviceC := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	if err := serviceC.Enable(context.Background(), noticeInvocation(), io.Discard); err != nil {
		t.Fatalf("enable after peer completion: %v", err)
	}
	if state := readStateFixture(t, home); state.Preference != preferenceEnabled || state.CleanupKind != cleanupNone {
		t.Fatalf("peer enable state = %#v", state)
	}
	close(releaseStateLock)
	second := receivePurgeCall(t, secondCall)
	requirePurgeErrorClass(t, second.err, PurgeErrorStateChanged)
	if second.result.DisabledDurable || second.result.Outcome != PurgeCleanupPending {
		t.Fatalf("stale pending observer claimed current opt-out: %+v err=%v", second.result, second.err)
	}
}

func TestConcurrentOffNonPendingCASLoserIsStateConflictWithoutDurabilityClaim(t *testing.T) {
	for _, initial := range []struct {
		name  string
		state persistedState
	}{
		{name: "enabled", state: enabledState(7, 2, testInstallationID, testSpoolGeneration)},
		{name: "clean-disabled", state: disabledState(7, 2, cleanupNone)},
	} {
		t.Run(initial.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, initial.state)
			root := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			depsB := defaultTestServiceDependencies(home, 2)
			enteredStateLock := make(chan struct{})
			releaseStateLock := make(chan struct{})
			var blockFirst sync.Once
			depsB.storageHooks.beforeStep = func(step storageStep) error {
				if step == storageStepLock {
					blockFirst.Do(func() {
						close(enteredStateLock)
						<-releaseStateLock
					})
				}
				return nil
			}
			serviceB := mustOpenTestService(t, depsB)
			loserCall := startDisableAndPurge(t, serviceB)
			select {
			case <-enteredStateLock:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("CAS loser did not pause before initial state lock")
			}
			serviceA := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
			winner, winnerErr := serviceA.DisableAndPurge(context.Background())
			if winnerErr != nil || !winner.DisabledDurable ||
				(winner.Outcome != PurgeCompleted && winner.Outcome != PurgeAlreadyDisabled) {
				t.Fatalf("concurrent winner = %+v err=%v", winner, winnerErr)
			}
			winnerState := readStateFixture(t, home)
			close(releaseStateLock)
			loser := receivePurgeCall(t, loserCall)
			requirePurgeErrorClass(t, loser.err, PurgeErrorStateChanged)
			if loser.result.Outcome != PurgeFailed || loser.result.DisabledDurable {
				t.Fatalf("non-pending CAS loser = %+v err=%v", loser.result, loser.err)
			}
			if after := readStateFixture(t, home); after != winnerState {
				t.Fatalf("CAS loser changed winner state:\nwinner=%#v\nafter=%#v", winnerState, after)
			}
		})
	}
}

func TestDisableAndPurgeMakesBlockedUploadResponseStaleWithoutSettlement(t *testing.T) {
	for _, test := range []struct {
		name string
		kind uploadResponseKind
	}{
		{name: "accepted", kind: uploadResponseAccepted},
		{name: "retry", kind: uploadResponseRetry},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, service, _ := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
			if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			sendStarted := make(chan struct{})
			releaseResponse := make(chan struct{})
			uploadDone := make(chan uploadCallResult, 1)
			go func() {
				upload, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
					now: func() time.Time { return testRecordHour },
					start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
						close(sendStarted)
						<-releaseResponse
						return uploadResponse{kind: test.kind}, nil
					}),
				})
				uploadDone <- uploadCallResult{result: upload, err: err}
			}()
			select {
			case <-sendStarted:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("upload did not enter sender")
			}

			offAtBarrier := make(chan struct{})
			releaseOff := make(chan struct{})
			service.deps.beforeDisableUploaderLock = func() {
				close(offAtBarrier)
				<-releaseOff
			}
			offDone := startDisableAndPurge(t, service)
			select {
			case <-offAtBarrier:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("off did not reach uploader barrier")
			}
			state := readStateFixture(t, home)
			if state.Preference != preferenceDisabled || state.CleanupKind != cleanupDisable {
				t.Fatalf("off barrier state = %#v", state)
			}
			close(releaseResponse)
			var upload uploadCallResult
			select {
			case upload = <-uploadDone:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("timed out waiting for stale upload")
			}
			if !errors.Is(upload.err, ErrStateChangedConcurrently) || upload.result.outcome != uploadRunStale || upload.result.events != 1 {
				t.Fatalf("stale upload result = %+v err=%v", upload.result, upload.err)
			}
			assertSpoolFileLocation(t, home, inflightDirectoryName, event.EventID)
			close(releaseOff)
			off := receivePurgeCall(t, offDone)
			if off.err != nil || off.result.Outcome != PurgeCompleted || !off.result.DisabledDurable || off.result.RemovedEvents != 1 {
				t.Fatalf("off after stale response = %+v err=%v", off.result, off.err)
			}
			requireCleanDisabledTree(t, home)
		})
	}
}

func TestDisableAndPurgeFailureAndRecoveryMatrix(t *testing.T) {
	t.Run("disable write not applied", func(t *testing.T) {
		home, service, _ := newRecordServiceFixture(t, testEventIDThree)
		before := readStateFixture(t, home)
		injected := errors.New("injected disable rename failure")
		service.deps.storageHooks.beforeStep = func(step storageStep) error {
			if step == storageStepRename {
				return injected
			}
			return nil
		}
		result, err := service.DisableAndPurge(context.Background())
		requirePurgeErrorClass(t, err, PurgeErrorDisableWrite)
		if result.Outcome != PurgeFailed || result.DisabledDurable || !errors.Is(err, injected) {
			t.Fatalf("not-applied disable = %+v err=%v", result, err)
		}
		if after := readStateFixture(t, home); after != before {
			t.Fatalf("failed disable changed state:\nbefore=%#v\nafter=%#v", before, after)
		}
	})

	t.Run("disable applied sync pending", func(t *testing.T) {
		home, service, _ := newRecordServiceFixture(t, testEventIDThree)
		root := mustOpenMutableRoot(t, home)
		event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
		data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
		if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
		renamed := false
		injected := errors.New("injected disable parent-sync failure")
		service.deps.storageHooks.beforeStep = func(step storageStep) error {
			if step == storageStepRename {
				renamed = true
			}
			if renamed && step == storageStepDirectorySync {
				return injected
			}
			return nil
		}
		result, err := service.DisableAndPurge(context.Background())
		requirePurgeErrorClass(t, err, PurgeErrorDisableWrite)
		if result.Outcome != PurgeCleanupPending || result.DisabledDurable || !errors.Is(err, errStateAppliedSyncPending) {
			t.Fatalf("sync-pending disable = %+v err=%v", result, err)
		}
		state := readStateFixture(t, home)
		if state.Preference != preferenceDisabled || state.CleanupKind != cleanupDisable {
			t.Fatalf("sync-pending disable state = %#v", state)
		}
		assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
		service.deps.storageHooks = storageTestHooks{}
		retry, retryErr := service.DisableAndPurge(context.Background())
		if retryErr != nil || retry.Outcome != PurgeCompleted {
			t.Fatalf("sync-pending retry = %+v err=%v", retry, retryErr)
		}
	})

	t.Run("corrupt safe recovery", func(t *testing.T) {
		home := newMetricsTestHome(t)
		writeRawConfigFixture(t, home, []byte("state_schema = [\n"))
		service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
		result, err := service.DisableAndPurge(context.Background())
		if err != nil || result.Outcome != PurgeCompleted || !result.DisabledDurable || !result.RecoveredState {
			t.Fatalf("corrupt recovery = %+v err=%v", result, err)
		}
		requireCleanDisabledTree(t, home)
	})

	t.Run("unsafe root", func(t *testing.T) {
		home := newMetricsTestHome(t)
		target := t.TempDir()
		sentinel := filepath.Join(target, "keep")
		if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, home.Root()); err != nil {
			t.Fatal(err)
		}
		service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
		result, err := service.DisableAndPurge(context.Background())
		requirePurgeErrorClass(t, err, PurgeErrorStorage)
		if result.Outcome != PurgeFailed || result.DisabledDurable {
			t.Fatalf("unsafe-root result = %+v", result)
		}
		if data, readErr := os.ReadFile(sentinel); readErr != nil || string(data) != "keep" {
			t.Fatalf("unsafe-root target changed: data=%q err=%v", data, readErr)
		}
	})
}

func TestDisableAndPurgeCleanupCompletionFailureRequiresRetry(t *testing.T) {
	for _, failure := range []string{"not-applied", "applied-sync-pending"} {
		t.Run(failure, func(t *testing.T) {
			home, service, _ := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			configRenames := 0
			completionRename := false
			injected := errors.New("injected cleanup-completion failure")
			service.deps.storageHooks.beforeMutation = func(step storageStep, path string) {
				if step == storageStepRename && filepath.Base(path) == configFileName {
					configRenames++
					completionRename = configRenames == 2
				}
			}
			service.deps.storageHooks.beforeStep = func(step storageStep) error {
				if !completionRename {
					return nil
				}
				if failure == "not-applied" && step == storageStepRename {
					return injected
				}
				if failure == "applied-sync-pending" && step == storageStepDirectorySync {
					return injected
				}
				return nil
			}

			result, err := service.DisableAndPurge(context.Background())
			requirePurgeErrorClass(t, err, PurgeErrorCleanupIncomplete)
			if result.Outcome != PurgeCleanupPending || !result.DisabledDurable || !errors.Is(err, injected) {
				t.Fatalf("completion failure = %+v err=%v", result, err)
			}
			if result.IncompletePhase != PurgeIncompleteFinalProof || result.ManualCleanupRequired ||
				result.ManualCleanupReason != PurgeManualCleanupNone {
				t.Fatalf("completion failure guidance = %+v", result)
			}
			visible := readStateFixture(t, home)
			if failure == "not-applied" && visible.CleanupKind != cleanupDisable {
				t.Fatalf("not-applied completion lost owner: %#v", visible)
			}
			if failure == "applied-sync-pending" && visible.CleanupKind != cleanupNone {
				t.Fatalf("sync-pending completion did not leave visible successor: %#v", visible)
			}

			service.deps.storageHooks = storageTestHooks{}
			retry, retryErr := service.DisableAndPurge(context.Background())
			if retryErr != nil || !retry.DisabledDurable ||
				(retry.Outcome != PurgeCompleted && retry.Outcome != PurgeAlreadyDisabled) {
				t.Fatalf("completion retry = %+v err=%v", retry, retryErr)
			}
			requireCleanDisabledTree(t, home)
		})
	}
}

func TestDisableAndPurgeFilesystemFailureKeepsExactOwnerForRetry(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected spool unlink failure")
	armed := false
	failed := false
	service.deps.storageHooks.beforeMutation = func(step storageStep, path string) {
		if !failed && (step == storageStepDelete || step == storageStepUnlink || step == storageStepRmdir) &&
			(strings.Contains(path, queueDirectoryName) || strings.Contains(path, inflightDirectoryName)) {
			armed = true
		}
	}
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if armed && !failed && (step == storageStepDelete || step == storageStepUnlink || step == storageStepRmdir) {
			failed = true
			return injected
		}
		return nil
	}
	result, err := service.DisableAndPurge(context.Background())
	requirePurgeErrorClass(t, err, PurgeErrorCleanupIncomplete)
	if !failed || !errors.Is(err, injected) || result.Outcome != PurgeCleanupPending || !result.DisabledDurable {
		t.Fatalf("filesystem-failed off = %+v failed=%v err=%v", result, failed, err)
	}
	owner := readStateFixture(t, home)
	if owner.Preference != preferenceDisabled || owner.CleanupKind != cleanupDisable {
		t.Fatalf("filesystem failure lost cleanup owner: %#v", owner)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)

	service.deps.storageHooks = storageTestHooks{}
	retry, retryErr := service.DisableAndPurge(context.Background())
	if retryErr != nil || retry.Outcome != PurgeCompleted || !retry.DisabledDurable || retry.RemovedEvents != 1 {
		t.Fatalf("filesystem retry = %+v err=%v", retry, retryErr)
	}
	requireCleanDisabledTree(t, home)
}

func TestDisableAndPurgeExactTokenConflictAndPeerCleanRecovery(t *testing.T) {
	for _, test := range []struct {
		name      string
		peer      string
		wantClass PurgeErrorClass
		want      PurgeOutcome
	}{
		{
			name:      "same numeric ABA is a conflict",
			peer:      "aba",
			wantClass: PurgeErrorStateChanged,
			want:      PurgeCleanupPending,
		},
		{
			name:      "clean config with residual files is a conflict",
			peer:      "residual-successor",
			wantClass: PurgeErrorStateChanged,
			want:      PurgeCleanupPending,
		},
		{
			name: "real peer full cleanup successor is success",
			peer: "full-clean-successor",
			want: PurgeCompleted,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, service, _ := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
			if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
				t.Fatal(err)
			}
			barrier, err := root.acquireLock(context.Background(), uploaderLockName)
			if err != nil {
				t.Fatal(err)
			}
			service.deps.disableUploaderWait = testutil.GoroutineRaceTimeout
			call := startDisableAndPurge(t, service)
			owner := waitForMetricsState(t, home, func(state persistedState) bool {
				return state.Preference == preferenceDisabled && state.CleanupKind == cleanupDisable
			})
			switch test.peer {
			case "aba":
				writeStateFixture(t, home, owner)
			case "residual-successor":
				successor := owner
				successor.CleanupKind = cleanupNone
				successor.StateGeneration++
				writeStateFixture(t, home, successor)
			case "full-clean-successor":
				loaded := loadStateFromDirectory(root)
				if loaded.err != nil || !loaded.present {
					t.Fatalf("load peer cleanup owner: %v", loaded.err)
				}
				token := cleanupTokenFromLoaded(&loaded)
				stateLock, lockErr := service.lockState(context.Background(), root)
				if lockErr != nil {
					t.Fatal(lockErr)
				}
				if prepareErr := service.prepareCleanupLocked(stateLock, token); prepareErr != nil {
					t.Fatal(prepareErr)
				}
				purged, purgeErr := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
				if purgeErr != nil || !purged.complete {
					t.Fatalf("peer purge = %+v err=%v", purged, purgeErr)
				}
				if completeErr := service.completeCleanupLocked(stateLock, token); completeErr != nil {
					t.Fatal(completeErr)
				}
				if closeErr := errors.Join(stateLock.Close(), token.Close()); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			if err := barrier.Release(); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			outcome := receivePurgeCall(t, call)
			if test.wantClass != "" {
				requirePurgeErrorClass(t, outcome.err, test.wantClass)
			} else if outcome.err != nil {
				t.Fatalf("peer-clean recovery error: %v", outcome.err)
			}
			if outcome.result.Outcome != test.want || !outcome.result.DisabledDurable {
				t.Fatalf("exact-token result = %+v err=%v", outcome.result, outcome.err)
			}
			if test.want == PurgeCompleted {
				requireCleanDisabledTree(t, home)
			} else {
				assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
			}
		})
	}
}

func TestDisableAndPurgeNeverCrossesUploaderBarrierOnReplacementRoot(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	barrier, err := service.lockUploader(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := errors.Join(barrier.Close(), root.Close()); closeErr != nil {
			t.Errorf("close original-root barrier: %v", closeErr)
		}
	}()

	replacementRoot := home.Root() + ".replacement"
	displacedRoot := home.Root() + ".displaced"
	if err := os.Mkdir(replacementRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	zeroQuota, err := encodeSpoolQuota(spoolQuota{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacementRoot, quotaFileName), zeroQuota, 0o600); err != nil {
		t.Fatal(err)
	}

	var swapped atomic.Bool
	var swapErr error
	service.deps.disableUploaderWait = 100 * time.Millisecond
	service.deps.storageHooks.afterAtomicWrite = func(path string, outcome storageWriteState) {
		if path != filepath.Join(home.Root(), configFileName) || outcome != storageWriteAppliedDurable ||
			!swapped.CompareAndSwap(false, true) {
			return
		}
		ownerData, err := os.ReadFile(path)
		if err != nil {
			swapErr = err
			return
		}
		owner, err := decodePersistedState(ownerData)
		if err != nil {
			swapErr = err
			return
		}
		successorData, err := encodePersistedState(cleanupSuccessorState(owner))
		if err != nil {
			swapErr = err
			return
		}
		if err := os.WriteFile(filepath.Join(replacementRoot, configFileName), successorData, 0o600); err != nil {
			swapErr = err
			return
		}
		if err := os.Rename(home.Root(), displacedRoot); err != nil {
			swapErr = err
			return
		}
		swapErr = os.Rename(replacementRoot, home.Root())
	}

	result, offErr := service.DisableAndPurge(context.Background())
	if swapErr != nil || !swapped.Load() {
		t.Fatalf("replace metrics root after durable disable: swapped=%v err=%v", swapped.Load(), swapErr)
	}
	requirePurgeErrorClass(t, offErr, PurgeErrorUploaderQuiescence)
	if result.Outcome != PurgeCleanupPending || !result.DisabledDurable {
		t.Fatalf("replacement-root off = %+v err=%v", result, offErr)
	}
	if got, err := os.ReadFile(filepath.Join(displacedRoot, queueDirectoryName, testSpoolGeneration, eventFileName(event.EventID))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("original root event after quiescence timeout = %q err=%v", got, err)
	}
}

func TestDisableAndPurgeBindsInitialObservationToRetainedRoot(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	initial := readStateFixture(t, home)
	token, err := service.beginDisable(context.Background(), stateVersionFrom(initial))
	if err != nil {
		t.Fatalf("establish original-root cleanup owner: %v", err)
	}
	if err := token.Close(); err != nil {
		t.Fatalf("close original-root cleanup token: %v", err)
	}
	ownerA := readStateFixture(t, home)
	if ownerA.Preference != preferenceDisabled || ownerA.CleanupKind != cleanupDisable {
		t.Fatalf("original-root cleanup owner = %#v", ownerA)
	}

	replacementRoot := home.Root() + ".replacement"
	displacedRoot := home.Root() + ".displaced"
	if err := os.Mkdir(replacementRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	cleanB := cleanupSuccessorState(ownerA)
	cleanData, err := encodePersistedState(cleanB)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacementRoot, configFileName), cleanData, 0o600); err != nil {
		t.Fatal(err)
	}
	zeroQuota, err := encodeSpoolQuota(spoolQuota{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacementRoot, quotaFileName), zeroQuota, 0o600); err != nil {
		t.Fatal(err)
	}

	var swapped atomic.Bool
	var swapErr error
	service.deps.storageHooks.beforeDirectoryOpen = func(path string) error {
		if path != home.Root() || !swapped.CompareAndSwap(false, true) {
			return nil
		}
		if err := os.Rename(home.Root(), displacedRoot); err != nil {
			swapErr = err
			return nil
		}
		swapErr = os.Rename(replacementRoot, home.Root())
		return nil
	}
	var transitions []persistedState
	service.deps.storageHooks.afterAtomicWrite = func(path string, outcome storageWriteState) {
		if path != filepath.Join(home.Root(), configFileName) || outcome != storageWriteAppliedDurable {
			return
		}
		encoded, err := os.ReadFile(path)
		if err != nil {
			swapErr = errors.Join(swapErr, err)
			return
		}
		state, err := decodePersistedState(encoded)
		if err != nil {
			swapErr = errors.Join(swapErr, err)
			return
		}
		transitions = append(transitions, state)
	}

	result, offErr := service.DisableAndPurge(context.Background())
	if swapErr != nil || !swapped.Load() {
		t.Fatalf("swap pending root A for clean successor B: swapped=%v err=%v", swapped.Load(), swapErr)
	}
	if offErr != nil || result.Outcome != PurgeAlreadyDisabled || !result.DisabledDurable ||
		result.RemovedEvents != 0 || result.RemovedBytes != 0 {
		t.Fatalf("replacement-root disable = %+v err=%v, want fresh already-disabled handshake", result, offErr)
	}
	if len(transitions) != 2 {
		t.Fatalf("replacement-root state transitions = %#v, want fresh owner then completion", transitions)
	}
	ownerB := transitions[0]
	if ownerB.Preference != preferenceDisabled || ownerB.CleanupKind != cleanupDisable ||
		ownerB.CounterNamespace != cleanB.CounterNamespace || ownerB.StateGeneration != cleanB.StateGeneration+1 ||
		ownerB.CleanupEpoch != cleanB.CleanupEpoch+1 {
		t.Fatalf("replacement root reused stale A authority: A=%#v clean-B=%#v owner-B=%#v", ownerA, cleanB, ownerB)
	}
	wantFinalB := cleanupSuccessorState(ownerB)
	if transitions[1] != wantFinalB {
		t.Fatalf("replacement-root completion = %#v, want %#v", transitions[1], wantFinalB)
	}
	if finalB := readStateFixture(t, home); finalB != wantFinalB {
		t.Fatalf("replacement-root final state = %#v, want %#v", finalB, wantFinalB)
	}

	ownerAData, err := os.ReadFile(filepath.Join(displacedRoot, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	finalA, err := decodePersistedState(ownerAData)
	if err != nil {
		t.Fatal(err)
	}
	if finalA != ownerA {
		t.Fatalf("replacement-root operation changed displaced A:\nbefore=%#v\nafter=%#v", ownerA, finalA)
	}
	if got, err := os.ReadFile(filepath.Join(displacedRoot, queueDirectoryName, testSpoolGeneration, eventFileName(event.EventID))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("displaced A event changed: data=%q err=%v", got, err)
	}
}

func TestDisableAndPurgeRejectsUnprovenPeerSuccessor(t *testing.T) {
	for _, test := range []struct {
		name      string
		change    func(*persistedState)
		failSync  bool
		crossTree bool
		wantClass PurgeErrorClass
	}{
		{
			name: "changed full-state field",
			change: func(state *persistedState) {
				state.RequiredNoticeVersion++
			},
			wantClass: PurgeErrorStateChanged,
		},
		{
			name:      "peer successor root sync failure",
			failSync:  true,
			wantClass: PurgeErrorStorage,
		},
		{
			name:      "peer successor cross-device tree",
			crossTree: true,
			wantClass: PurgeErrorStorage,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
			root := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			queuePath := filepath.Join(home.Root(), queueDirectoryName)
			if test.crossTree {
				if err := os.Mkdir(queuePath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			barrier, err := root.acquireLock(context.Background(), uploaderLockName)
			if err != nil {
				t.Fatal(err)
			}
			var armed atomic.Bool
			injected := errors.New("injected peer-successor root sync failure")
			deps := defaultTestServiceDependencies(home, 2)
			crossDeviceOpens := 0
			deps.storageHooks.metadata = func(path string, metadata storageMetadata) storageMetadata {
				if armed.Load() && test.crossTree && path == queuePath {
					metadata.dev ^= 1 << 63
				}
				return metadata
			}
			deps.storageHooks.beforeDirectoryOpen = func(path string) error {
				if armed.Load() && test.crossTree && path == queuePath {
					crossDeviceOpens++
				}
				return nil
			}
			deps.storageHooks.beforeStep = func(step storageStep) error {
				if armed.Load() && test.failSync && step == storageStepDirectorySync {
					return injected
				}
				return nil
			}
			deps.disableUploaderWait = testutil.GoroutineRaceTimeout
			service := mustOpenTestService(t, deps)
			call := startDisableAndPurge(t, service)
			owner := waitForMetricsState(t, home, func(state persistedState) bool {
				return state.Preference == preferenceDisabled && state.CleanupKind == cleanupDisable
			})
			successor := cleanupSuccessorState(owner)
			if test.change != nil {
				test.change(&successor)
			}
			writeStateFixture(t, home, successor)
			armed.Store(test.failSync || test.crossTree)
			if err := barrier.Release(); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			outcome := receivePurgeCall(t, call)
			requirePurgeErrorClass(t, outcome.err, test.wantClass)
			if outcome.result.Outcome != PurgeCleanupPending || !outcome.result.DisabledDurable {
				t.Fatalf("unproven peer successor = %+v err=%v", outcome.result, outcome.err)
			}
			if after := readStateFixture(t, home); after != successor {
				t.Fatalf("unproven peer successor mutated state:\nwant=%#v\nafter=%#v", successor, after)
			}
			if test.failSync && !errors.Is(outcome.err, injected) {
				t.Fatalf("peer sync error lost cause: %v", outcome.err)
			}
			if test.crossTree && (!errors.Is(outcome.err, syscall.EXDEV) || crossDeviceOpens != 0) {
				t.Fatalf("peer proof crossed filesystem boundary: opens=%d err=%v", crossDeviceOpens, outcome.err)
			}
		})
	}
}

func TestDisableAndPurgeRejectsPeerSuccessorReplacedDuringCleanProof(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	barrier, err := root.acquireLock(context.Background(), uploaderLockName)
	if err != nil {
		t.Fatal(err)
	}

	var armed, replaced atomic.Bool
	var replacement persistedState
	var replacementData []byte
	var replaceErr error
	replacementTemp := filepath.Join(home.Root(), ".peer-successor-replacement")
	configPath := filepath.Join(home.Root(), configFileName)
	deps := defaultTestServiceDependencies(home, 2)
	deps.disableUploaderWait = testutil.GoroutineRaceTimeout
	deps.storageHooks.beforeStep = func(step storageStep) error {
		if step != storageStepEnumerate || !armed.Load() || !replaced.CompareAndSwap(false, true) {
			return nil
		}
		if err := os.WriteFile(replacementTemp, replacementData, 0o600); err != nil {
			replaceErr = err
			return nil
		}
		replaceErr = os.Rename(replacementTemp, configPath)
		return nil
	}
	service := mustOpenTestService(t, deps)
	call := startDisableAndPurge(t, service)
	owner := waitForMetricsState(t, home, func(state persistedState) bool {
		return state.Preference == preferenceDisabled && state.CleanupKind == cleanupDisable
	})
	successor := cleanupSuccessorState(owner)
	writeStateFixture(t, home, successor)
	replacement = successor
	replacement.RequiredNoticeVersion++
	replacementData, err = encodePersistedState(replacement)
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if err := barrier.Release(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	outcome := receivePurgeCall(t, call)
	if replaceErr != nil {
		t.Fatalf("replace peer successor during proof: %v", replaceErr)
	}
	if !replaced.Load() {
		t.Fatal("peer successor was not replaced during the clean-tree proof")
	}
	requirePurgeErrorClass(t, outcome.err, PurgeErrorStateChanged)
	if outcome.result.Outcome != PurgeCleanupPending || !outcome.result.DisabledDurable {
		t.Fatalf("post-proof peer replacement = %+v err=%v", outcome.result, outcome.err)
	}
	if after := readStateFixture(t, home); after != replacement {
		t.Fatalf("post-proof peer replacement was mutated:\nwant=%#v\nafter=%#v", replacement, after)
	}
}

func TestDisableAndPurgeAcceptsExactPeerCleanupSuccessorAcrossCounterRollover(t *testing.T) {
	for _, test := range []struct {
		name  string
		state persistedState
	}{
		{name: "state generation", state: disabledState(maximumStateCounter-1, 1, cleanupDisable)},
		{name: "cleanup epoch", state: disabledState(7, maximumStateCounter-1, cleanupDisable)},
		{name: "terminal namespace", state: func() persistedState {
			state := disabledState(maximumStateCounter-1, 1, cleanupDisable)
			state.CounterNamespace = terminalCounterNamespace
			return state
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, test.state)
			root := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			barrier, err := root.acquireLock(context.Background(), uploaderLockName)
			if err != nil {
				t.Fatal(err)
			}
			deps := defaultTestServiceDependencies(home, 1)
			deps.notice.version = 1
			deps.disableUploaderWait = testutil.GoroutineRaceTimeout
			service := mustOpenTestService(t, deps)
			attempts := make(chan struct{}, 1)
			service.deps.beforeDisableUploaderLock = func() { attempts <- struct{}{} }
			call := startDisableAndPurge(t, service)
			receiveUploaderAttempt(t, attempts)
			owner := waitForMetricsState(t, home, func(state persistedState) bool {
				return state.Preference == preferenceDisabled && state.CleanupKind == cleanupDisable
			})
			if owner != test.state {
				t.Fatalf("off replaced terminal cleanup owner:\nwant=%#v\ngot=%#v", test.state, owner)
			}
			loaded := loadStateFromDirectory(root)
			if loaded.err != nil || !loaded.present {
				t.Fatalf("load peer owner: %v", loaded.err)
			}
			token := cleanupTokenFromLoaded(&loaded)
			stateLock, lockErr := service.lockState(context.Background(), root)
			if lockErr != nil {
				t.Fatal(lockErr)
			}
			if prepareErr := service.prepareCleanupLocked(stateLock, token); prepareErr != nil {
				t.Fatal(prepareErr)
			}
			purged, purgeErr := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
			if purgeErr != nil || !purged.complete {
				t.Fatalf("peer rollover purge = %+v err=%v", purged, purgeErr)
			}
			if completeErr := service.completeCleanupLocked(stateLock, token); completeErr != nil {
				t.Fatal(completeErr)
			}
			if closeErr := errors.Join(stateLock.Close(), token.Close()); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err := barrier.Release(); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			outcome := receivePurgeCall(t, call)
			if outcome.err != nil || outcome.result.Outcome != PurgeCompleted || !outcome.result.DisabledDurable {
				t.Fatalf("peer rollover off = %+v err=%v", outcome.result, outcome.err)
			}
			if clean := requireCleanDisabledTree(t, home); clean != cleanupSuccessorState(test.state) {
				t.Fatalf("peer rollover successor = %#v, want %#v", clean, cleanupSuccessorState(test.state))
			}
		})
	}
}

func TestPeerCleanProofDoesNotDrainLiveJournaledRootTemp(t *testing.T) {
	home := newMetricsTestHome(t)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xd31))
	writeJournaledRootTempCrashFixture(t, root, name, []byte("live peer temp"), 0)
	tempPath := filepath.Join(home.Root(), name)
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)

	err := proveCleanMetricsTree(root, defaultSpoolWorkBudget())
	if !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("peer proof with live journaled temp = %v, want state-changed", err)
	}
	for _, path := range []string{tempPath, markerPath} {
		if _, statErr := os.Lstat(path); statErr != nil {
			t.Fatalf("read-only peer proof mutated %q: %v", path, statErr)
		}
	}
}

func TestPeerCleanProofTreatsFutureControlFilesAsUnrecognizedResidue(t *testing.T) {
	for _, name := range []string{"status.toml", "spawn-throttle"} {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, disabledState(7, 2, cleanupNone))
			plainRoot := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(plainRoot, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home.Root(), name)
			if err := os.WriteFile(path, []byte("future owner residue"), 0o644); err != nil {
				t.Fatal(err)
			}
			mutations := 0
			root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
				beforeMutation: func(_ storageStep, observed string) {
					if observed == path {
						mutations++
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			proofErr := proveCleanMetricsTree(root, defaultSpoolWorkBudget())
			if !errors.Is(proofErr, ErrStateChangedConcurrently) {
				t.Fatalf("peer proof accepted future control residue: %v", proofErr)
			}
			if mutations != 0 {
				t.Fatalf("peer proof attempted %d future-control mutations", mutations)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "future owner residue" {
				t.Fatalf("peer proof changed future control residue: data=%q err=%v", data, err)
			}
		})
	}
}

func TestPeerCleanProofPreservesFutureControlResidueShapesWithoutDescent(t *testing.T) {
	for _, name := range []string{"status.toml", "spawn-throttle"} {
		for _, shape := range []string{"regular", "symlink", "cross-device"} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				home := newMetricsTestHome(t)
				writeStateFixture(t, home, disabledState(7, 2, cleanupNone))
				plainRoot := mustOpenMutableRoot(t, home)
				if err := persistSpoolQuota(plainRoot, spoolQuota{}); err != nil {
					t.Fatal(err)
				}
				if err := plainRoot.Close(); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(home.Root(), name)
				want := "future owner residue"
				if shape == "symlink" {
					target := filepath.Join(t.TempDir(), "sentinel")
					if err := os.WriteFile(target, []byte(want), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
					t.Fatal(err)
				}
				mutations := 0
				opens := 0
				root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
					metadata: func(observed string, metadata storageMetadata) storageMetadata {
						if shape == "cross-device" && observed == path {
							metadata.dev ^= 1 << 63
						}
						return metadata
					},
					beforeDirectoryOpen: func(observed string) error {
						if observed == path {
							opens++
						}
						return nil
					},
					beforeMutation: func(_ storageStep, observed string) {
						if observed == path {
							mutations++
						}
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()
				proofErr := proveCleanMetricsTree(root, defaultSpoolWorkBudget())
				if !errors.Is(proofErr, ErrStateChangedConcurrently) || mutations != 0 || opens != 0 {
					t.Fatalf("peer future-control %s proof = mutations:%d opens:%d err:%v",
						shape, mutations, opens, proofErr)
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != want {
					t.Fatalf("peer future-control %s residue changed: data=%q err=%v", shape, data, err)
				}
				if shape == "symlink" {
					if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
						t.Fatalf("peer future-control symlink was replaced: info=%v err=%v", info, err)
					}
				}
			})
		}
	}
}

func TestPeerCleanProofRejectsMarkerReplacementAfterTempAbsence(t *testing.T) {
	home := newMetricsTestHome(t)
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xd32))
	tempPath := filepath.Join(home.Root(), name)
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	displacedMarker := markerPath + ".displaced"
	tempLookups := 0
	var swapErr error
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeMetadataAttempt: func(path string) error {
			if path != tempPath {
				return nil
			}
			tempLookups++
			if tempLookups != 2 {
				return nil
			}
			if err := os.Rename(markerPath, displacedMarker); err != nil {
				swapErr = err
				return nil
			}
			swapErr = os.WriteFile(markerPath, []byte("replacement marker"), 0o600)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	backend, ok := root.backend.(*unixStorageDirectory)
	if !ok {
		t.Fatal("root-temp journal test requires Unix storage")
	}
	journal, err := backend.openRootTempJournal()
	if err != nil {
		t.Fatal(err)
	}
	marker, err := createRootTempJournalMarker(backend, journal, name)
	if err != nil {
		_ = journal.close()
		t.Fatal(err)
	}
	if err := marker.close(); err != nil {
		t.Fatal(err)
	}

	err = proveCleanMetricsTree(root, defaultSpoolWorkBudget())
	if swapErr != nil {
		t.Fatalf("replace journal marker fixture: %v", swapErr)
	}
	if tempLookups < 2 {
		t.Fatalf("peer proof made %d mapped-temp lookups, want at least 2", tempLookups)
	}
	if !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("peer proof with replaced marker = %v, want state-changed", err)
	}
	if data, readErr := os.ReadFile(markerPath); readErr != nil || string(data) != "replacement marker" {
		t.Fatalf("read-only peer proof changed replacement marker: data=%q err=%v", data, readErr)
	}
}

func TestDisableAndPurgeSurfacesBarrierCloseFailures(t *testing.T) {
	for _, target := range []controlCloseTarget{controlCloseFinalState, controlCloseUploader, controlCloseRoot} {
		t.Run(string(target), func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
			deps := defaultTestServiceDependencies(home, 2)
			injected := errors.New("injected control close failure")
			deps.controlCloseError = func(observed controlCloseTarget) error {
				if observed == target {
					return injected
				}
				return nil
			}
			service := mustOpenTestService(t, deps)
			result, err := service.DisableAndPurge(context.Background())
			requirePurgeErrorClass(t, err, PurgeErrorStorage)
			if result.Outcome == PurgeCompleted || result.Outcome == PurgeAlreadyDisabled || !result.DisabledDurable || !errors.Is(err, injected) {
				t.Fatalf("close failure reported success: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestDisableAndPurgeFinalConfigMarkerRetirementFailureDoesNotExitZero(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(home.Root(), configFileName)
	journalPath := filepath.Join(home.Root(), rootTempJournalDirectoryName)
	injected := errors.New("injected final config marker retirement failure")
	proofInjected := errors.New("injected final config journal proof failure")
	finalConfigDurable := false
	markerDeleteArmed := false
	markerDeleteAttempts := 0
	service.deps.storageHooks.afterAtomicWrite = func(path string, state storageWriteState) {
		if path != configPath || state != storageWriteAppliedDurable {
			return
		}
		if cleanDisabledState(readStateFixture(t, home)) {
			finalConfigDurable = true
		}
	}
	service.deps.storageHooks.beforeMutation = func(step storageStep, path string) {
		if finalConfigDurable && step == storageStepDelete && filepath.Dir(path) == journalPath {
			markerDeleteArmed = true
			markerDeleteAttempts++
		}
	}
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if markerDeleteArmed && step == storageStepDelete {
			markerDeleteArmed = false
			return injected
		}
		if finalConfigDurable && markerDeleteAttempts > 0 && step == storageStepEnumerate {
			return proofInjected
		}
		return nil
	}

	result, err := service.DisableAndPurge(context.Background())
	requirePurgeErrorClass(t, err, PurgeErrorCleanupIncomplete)
	if result.Outcome != PurgeCleanupPending || !result.DisabledDurable || !finalConfigDurable || markerDeleteAttempts == 0 ||
		!errors.Is(err, proofInjected) {
		t.Fatalf("unsettled final-config journal reported success: result=%+v durable=%v deletes=%d err=%v",
			result, finalConfigDurable, markerDeleteAttempts, err)
	}
	entries, readErr := os.ReadDir(journalPath)
	if readErr != nil || len(entries) == 0 {
		t.Fatalf("failed final-config marker retirement left no retry evidence: entries=%v err=%v", entries, readErr)
	}
}

func TestDisableAndPurgeFinalConfigMarkerRetirementFailureSucceedsAfterDurableAbsenceProof(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(home.Root(), configFileName)
	journalPath := filepath.Join(home.Root(), rootTempJournalDirectoryName)
	injected := errors.New("injected final config marker retirement failure")
	finalConfigDurable := false
	markerDeleteArmed := false
	markerDeleteAttempts := 0
	service.deps.storageHooks.afterAtomicWrite = func(path string, state storageWriteState) {
		if path == configPath && state == storageWriteAppliedDurable && cleanDisabledState(readStateFixture(t, home)) {
			finalConfigDurable = true
		}
	}
	service.deps.storageHooks.beforeMutation = func(step storageStep, path string) {
		if finalConfigDurable && step == storageStepDelete && filepath.Dir(path) == journalPath {
			markerDeleteArmed = true
			markerDeleteAttempts++
		}
	}
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if markerDeleteArmed && step == storageStepDelete {
			markerDeleteArmed = false
			return injected
		}
		return nil
	}

	result, err := service.DisableAndPurge(context.Background())
	if err != nil || result.Outcome != PurgeCompleted || !result.DisabledDurable || !finalConfigDurable || markerDeleteAttempts == 0 {
		t.Fatalf("durably absent final-config temp was not accepted: result=%+v durable=%v deletes=%d err=%v",
			result, finalConfigDurable, markerDeleteAttempts, err)
	}
	entries, readErr := os.ReadDir(journalPath)
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("successful final-config proof evidence = entries:%v err:%v", entries, readErr)
	}
	name := entries[0].Name()
	markerData, readErr := os.ReadFile(filepath.Join(journalPath, name))
	evidence, decodeErr := decodeRootTempJournalMarker(name, markerData)
	if readErr != nil || decodeErr != nil || evidence.state != rootTempJournalMarkerBound {
		t.Fatalf("retained final-config marker = evidence:%+v readErr:%v decodeErr:%v", evidence, readErr, decodeErr)
	}
	if _, statErr := os.Lstat(filepath.Join(home.Root(), name)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("successful final-config proof retained mapped temp %q: %v", name, statErr)
	}
	if clean := requireCleanDisabledTree(t, home); !cleanDisabledState(clean) {
		t.Fatalf("successful final-config proof state = %#v", clean)
	}
}

func startDisableAndPurge(t *testing.T, service *Service) <-chan purgeCallResult {
	t.Helper()
	result := make(chan purgeCallResult, 1)
	go func() {
		purge, err := service.DisableAndPurge(context.Background())
		result <- purgeCallResult{result: purge, err: err}
	}()
	return result
}

func receivePurgeCall(t *testing.T, call <-chan purgeCallResult) purgeCallResult {
	t.Helper()
	select {
	case result := <-call:
		return result
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for DisableAndPurge")
		return purgeCallResult{}
	}
}

func receiveUploaderAttempt(t *testing.T, attempts <-chan struct{}) {
	t.Helper()
	select {
	case <-attempts:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for uploader-lock attempt")
	}
}

func waitForMetricsState(t *testing.T, home gchome.ProductUsageHome, predicate func(persistedState) bool) persistedState {
	t.Helper()
	deadline := time.Now().Add(testutil.GoroutineRaceTimeout)
	for time.Now().Before(deadline) {
		loaded := readStateReadOnlyFixture(home)
		if loaded.err == nil && loaded.present && predicate(loaded.state) {
			state := loaded.state
			_ = loaded.Close()
			return state
		}
		_ = loaded.Close()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for product-metrics state")
	return persistedState{}
}

func readStateReadOnlyFixture(home gchome.ProductUsageHome) loadedState {
	root, err := openStorageRootReadOnly(home)
	if err != nil {
		return loadedState{err: err}
	}
	loaded := loadStateFromDirectory(root)
	if closeErr := root.Close(); closeErr != nil && loaded.err == nil {
		loaded.err = closeErr
	}
	return loaded
}

func requirePurgeErrorClass(t *testing.T, err error, want PurgeErrorClass) {
	t.Helper()
	var classified *PurgeError
	if !errors.As(err, &classified) || classified.Class != want {
		t.Fatalf("purge error = %v, want class %q", err, want)
	}
}

func requireCleanDisabledTree(t *testing.T, home gchome.ProductUsageHome) persistedState {
	t.Helper()
	state := readStateFixture(t, home)
	if state.Preference != preferenceDisabled || state.CleanupKind != cleanupNone || state.InstallationID != "" ||
		state.SpoolGeneration != "" || state.PausedThroughMetricsEpoch != 0 {
		t.Fatalf("not clean disabled: %#v", state)
	}
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if quota := readQuotaFromRoot(t, root); quota != (spoolQuota{}) {
		t.Fatalf("clean-disabled quota = %+v", quota)
	}
	for _, name := range []string{queueDirectoryName, inflightDirectoryName} {
		entry, err := root.lookupEntry(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || entry.metadata.kind != storageEntryDirectory {
			t.Fatalf("clean-disabled tree %q is unsafe: entry=%+v err=%v", name, entry, err)
		}
		entries, readErr := os.ReadDir(filepath.Join(home.Root(), name))
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("clean-disabled tree %q is not empty: entries=%d err=%v", name, len(entries), readErr)
		}
	}
	for _, name := range []string{spoolControlDirectoryName, retiredControlDirectoryName, fallbackRelocationCursorName} {
		if _, err := root.lookupEntry(name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("clean-disabled residual %q: %v", name, err)
		}
	}
	return state
}
