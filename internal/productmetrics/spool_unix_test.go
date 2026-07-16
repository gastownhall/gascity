//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/testutil"
	"golang.org/x/sys/unix"
)

const (
	testEventIDOne   = "8c4f4128-a6e8-4f66-bd1b-1fcf1298b124"
	testEventIDTwo   = "123e4567-e89b-42d3-a456-426614174000"
	testEventIDThree = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

var testRecordHour = time.Date(2026, 7, 11, 2, 0, 0, 0, time.UTC)

func unixStatDevice(stat unix.Stat_t) uint64 {
	return uint64(stat.Dev) //nolint:unconvert // Stat_t.Dev differs between Linux and Darwin.
}

func unixStatInode(stat unix.Stat_t) uint64 {
	return uint64(stat.Ino) //nolint:unconvert // Normalize platform-specific inode fields for comparisons.
}

func TestLoadSpoolQuotaDistinguishesAbsentFromDurableZero(t *testing.T) {
	home := newMetricsTestHome(t)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	quota, present, err := loadSpoolQuota(root)
	if err != nil || present || quota != (spoolQuota{}) {
		t.Fatalf("absent quota = (%+v, %v, %v)", quota, present, err)
	}
	if err := persistInitialSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	quota, present, err = loadSpoolQuota(root)
	if err != nil || !present || quota != (spoolQuota{}) {
		t.Fatalf("durable zero quota = (%+v, %v, %v)", quota, present, err)
	}
}

func TestRecordOnceWritesOneImmutableEventAndConservativeQuotaWithoutScanning(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDOne)
	deps.now = func() time.Time { return testRecordHour }
	var enumerations int
	deps.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepEnumerate {
			enumerations++
			return errors.New("foreground enqueue attempted a scan")
		}
		return nil
	}
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	t.Cleanup(func() { _ = permit.Close() })

	if got := service.RecordOnce(permit, CommandHelp); got != RecordStored {
		t.Fatalf("RecordOnce = %v, want stored", got)
	}
	if enumerations != 0 {
		t.Fatalf("foreground enqueue enumerated %d entries", enumerations)
	}
	eventPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(testEventIDOne))
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatalf("read queued event: %v", err)
	}
	want := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	wantBytes, err := EncodeEvent(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(wantBytes) {
		t.Fatalf("queued bytes = %s, want %s", data, wantBytes)
	}
	info, err := os.Stat(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("event mode = %04o", info.Mode().Perm())
	}
	quota := readQuotaFixture(t, home)
	if quota != (spoolQuota{Events: 1, Bytes: uint64(len(data))}) {
		t.Fatalf("quota = %+v", quota)
	}

	if got := service.RecordOnce(permit, CommandVersion); got != RecordDropped {
		t.Fatalf("second RecordOnce = %v, want dropped", got)
	}
	entries, err := os.ReadDir(filepath.Dir(eventPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("event files after second attempt = %d", len(entries))
	}
}

func TestRecordOnceMissingQuotaRequiresExactEmptySpoolProof(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, gchome.ProductUsageHome) string
	}{
		{
			name: "queue directory",
			setup: func(t *testing.T, home gchome.ProductUsageHome) string {
				path := filepath.Join(home.Root(), queueDirectoryName)
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "queue leaf",
			setup: func(t *testing.T, home gchome.ProductUsageHome) string {
				path := filepath.Join(home.Root(), queueDirectoryName)
				if err := os.WriteFile(path, []byte("retained"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "inflight directory",
			setup: func(t *testing.T, home gchome.ProductUsageHome) string {
				path := filepath.Join(home.Root(), inflightDirectoryName)
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "inflight symlink",
			setup: func(t *testing.T, home gchome.ProductUsageHome) string {
				path := filepath.Join(home.Root(), inflightDirectoryName)
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "surviving quota staging temp",
			setup: func(t *testing.T, home gchome.ProductUsageHome) string {
				path := filepath.Join(home.Root(), ".pm-control")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, ".pm-tmp-crash"), []byte("partial quota"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "queue byte cap already overrun",
			setup: func(t *testing.T, home gchome.ProductUsageHome) string {
				path := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
				payload := filepath.Join(path, eventFileName(testEventIDTwo))
				if err := os.WriteFile(payload, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(payload, int64(maximumSpoolBytes+1)); err != nil {
					t.Fatal(err)
				}
				return payload
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, service, permit := newRecordServiceFixture(t, testEventIDOne)
			retainedPath := test.setup(t, home)
			var enumerations int
			service.deps.storageHooks.beforeStep = func(step storageStep) error {
				if step == storageStepEnumerate {
					enumerations++
				}
				return nil
			}

			if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
				t.Fatalf("RecordOnce = %v, want dropped", got)
			}
			if enumerations != 0 {
				t.Fatalf("missing-quota proof enumerated %d entries", enumerations)
			}
			if _, err := os.Lstat(filepath.Join(home.Root(), quotaFileName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("missing-quota rejection rewrote quota: %v", err)
			}
			if _, err := os.Lstat(retainedPath); err != nil {
				t.Fatalf("missing-quota rejection changed retained evidence: %v", err)
			}
		})
	}
}

func TestRecordOnceFreshQuotaBootstrapNeverReplacesDestinationRace(t *testing.T) {
	t.Run("clean first install", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		if got := service.RecordOnce(permit, CommandHelp); got != RecordStored {
			t.Fatalf("RecordOnce = %v, want stored", got)
		}
		if got := readQuotaFixture(t, home); got.Events != 1 || got.Bytes == 0 {
			t.Fatalf("fresh bootstrap quota = %+v", got)
		}
	})

	t.Run("conflicting destination appears before install", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		competing := spoolQuota{Events: maximumSpoolEvents, Bytes: 1}
		competingData, err := encodeSpoolQuota(competing)
		if err != nil {
			t.Fatal(err)
		}
		renames := 0
		service.deps.storageHooks.beforeStep = func(step storageStep) error {
			if step != storageStepRename {
				return nil
			}
			renames++
			if renames == 2 {
				if err := os.WriteFile(filepath.Join(home.Root(), quotaFileName), competingData, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}

		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("RecordOnce = %v, want dropped", got)
		}
		if got := readQuotaFixture(t, home); got != competing {
			t.Fatalf("fresh install replaced racing quota with %+v", got)
		}
		assertNoQueuedEvents(t, home)
	})

	t.Run("exact expected destination replays", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		eventData, err := EncodeEvent(testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp))
		if err != nil {
			t.Fatal(err)
		}
		expected := spoolQuota{Events: 1, Bytes: uint64(len(eventData))}
		expectedData, err := encodeSpoolQuota(expected)
		if err != nil {
			t.Fatal(err)
		}
		renames := 0
		service.deps.storageHooks.beforeStep = func(step storageStep) error {
			if step == storageStepRename {
				renames++
				if renames == 2 {
					if err := os.WriteFile(filepath.Join(home.Root(), quotaFileName), expectedData, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			return nil
		}
		if got := service.RecordOnce(permit, CommandHelp); got != RecordStored {
			t.Fatalf("RecordOnce = %v, want stored replay", got)
		}
		if got := readQuotaFixture(t, home); got != expected {
			t.Fatalf("replayed quota = %+v, want %+v", got, expected)
		}
		if _, err := os.Lstat(filepath.Join(home.Root(), spoolControlDirectoryName)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("successful replay left quota staging control: %v", err)
		}
	})
}

func TestRecordOnceDecisionWindowGatesNoReplaceConflictReplay(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	eventData, err := EncodeEvent(testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp))
	if err != nil {
		t.Fatal(err)
	}
	expected := spoolQuota{Events: 1, Bytes: uint64(len(eventData))}
	expectedData, err := encodeSpoolQuota(expected)
	if err != nil {
		t.Fatal(err)
	}
	current := testRecordHour
	service.deps.now = func() time.Time { return current }
	renames := 0
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepRename {
			renames++
			if renames == 2 {
				if err := os.WriteFile(filepath.Join(home.Root(), quotaFileName), expectedData, 0o600); err != nil {
					t.Fatal(err)
				}
				// Model the no-replace syscall observing the racing destination.
				// From that attempted mutation onward, replay classification,
				// parent sync, and staging cleanup are a clock-free durability tail.
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
				return unix.EEXIST
			}
		}
		return nil
	}
	if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
		t.Fatalf("RecordOnce = %v, want dropped", got)
	}
	if got := readQuotaFixture(t, home); got != expected {
		t.Fatalf("conflict replay changed racing quota to %+v", got)
	}
	controlPath := filepath.Join(home.Root(), spoolControlDirectoryName)
	if _, err := os.Lstat(controlPath); err != nil {
		t.Fatalf("expired post-attempt replay lost conservative control evidence: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(controlPath, quotaStagingFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expired post-attempt replay did not finish staging cleanup: %v", err)
	}
	assertNoQueuedEvents(t, home)
}

func TestRecordOnceFirstAttemptWinsEvenWhenTheAttemptFails(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	if got := service.RecordOnce(permit, CommandID(65535)); got != RecordDropped {
		t.Fatalf("invalid first attempt = %v", got)
	}
	if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
		t.Fatalf("second attempt = %v", got)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), quotaFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed first attempt created quota: %v", err)
	}
}

func TestRecordOnceConcurrentAttemptsHaveOneFirstAttemptWinner(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	start := make(chan struct{})
	results := make(chan RecordResult, 2)
	var wait sync.WaitGroup
	for _, command := range []CommandID{CommandHelp, CommandVersion} {
		wait.Add(1)
		go func(command CommandID) {
			defer wait.Done()
			<-start
			results <- service.RecordOnce(permit, command)
		}(command)
	}
	close(start)
	wait.Wait()
	close(results)
	stored := 0
	for result := range results {
		if result == RecordStored {
			stored++
		}
	}
	if stored != 1 {
		t.Fatalf("stored attempts = %d, want 1", stored)
	}
	if got := readQuotaFixture(t, home); got.Events != 1 {
		t.Fatalf("concurrent quota = %+v", got)
	}
}

func TestRecordOnceDropsExactRecordAndGenerationStalePermits(t *testing.T) {
	t.Run("record incarnation replaced", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		replacement := enabledState(7, 2, testInstallationID, testSpoolGeneration)
		writeStateFixture(t, home, replacement)
		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("RecordOnce = %v", got)
		}
		assertNoQueuedEvents(t, home)
	})

	t.Run("spool generation changed", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		replacement := enabledState(8, 2, testInstallationID, "22222222-2222-4222-8222-222222222222")
		writeStateFixture(t, home, replacement)
		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("RecordOnce = %v", got)
		}
		assertNoQueuedEvents(t, home)
	})
}

func TestRecordOnceDropsOutOfRetentionInvocationHoursBeforeReservation(t *testing.T) {
	for name, occurred := range map[string]time.Time{
		"expired": testRecordHour.Add(-(maximumEventAgeHours + 1) * time.Hour),
		"future":  testRecordHour.Add(time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
			deps := defaultTestServiceDependencies(home, 2)
			deps.newUUID = uuidSequence(t, testEventIDOne)
			deps.now = func() time.Time { return testRecordHour }
			service := mustOpenTestService(t, deps)
			permit := service.RecordingPermit(recordableInvocationAt(occurred))
			t.Cleanup(func() { _ = permit.Close() })
			if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
				t.Fatalf("RecordOnce = %v", got)
			}
			if _, err := os.Lstat(filepath.Join(home.Root(), quotaFileName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("out-of-window event reserved quota: %v", err)
			}
		})
	}
}

func TestRecordOnceDropsAtQuotaCapsAndNeverScansToMakeRoom(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{Events: maximumSpoolEvents, Bytes: 1}); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	var scans int
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepEnumerate {
			scans++
		}
		return nil
	}
	if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
		t.Fatalf("RecordOnce = %v", got)
	}
	if scans != 0 {
		t.Fatalf("quota-full foreground path scanned %d times", scans)
	}
	assertNoQueuedEvents(t, home)
	if got := readQuotaFixture(t, home); got != (spoolQuota{Events: maximumSpoolEvents, Bytes: 1}) {
		t.Fatalf("quota changed on cap drop: %+v", got)
	}
}

func TestRecordOnceChecksDecisionBudgetBeforeUncancellableStorage(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDOne)
	start := testRecordHour
	var mu sync.Mutex
	times := []time.Time{start, start.Add(defaultRecordDecisionBudget + time.Nanosecond)}
	deps.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if len(times) == 0 {
			return start.Add(defaultRecordDecisionBudget + time.Second)
		}
		value := times[0]
		times = times[1:]
		return value
	}
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(start))
	t.Cleanup(func() { _ = permit.Close() })
	if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
		t.Fatalf("RecordOnce = %v", got)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), quotaFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("spent decision budget started quota write: %v", err)
	}
}

func TestRecordOnceRechecksDecisionBudgetAfterStateLockBeforeConfigRead(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDOne)
	current := testRecordHour
	var mu sync.Mutex
	deps.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	configReads := 0
	deps.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepLock {
			mu.Lock()
			current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			mu.Unlock()
		}
		return nil
	}
	deps.storageHooks.metadata = func(path string, metadata storageMetadata) storageMetadata {
		if filepath.Base(path) == configFileName {
			configReads++
		}
		return metadata
	}
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	t.Cleanup(func() { _ = permit.Close() })

	if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
		t.Fatalf("RecordOnce = %v", got)
	}
	if configReads != 0 {
		t.Fatalf("expired post-lock decision window began config I/O: %d metadata reads", configReads)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), quotaFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expired post-lock decision window began quota I/O: %v", err)
	}
}

func TestRecordOnceSpentBudgetAfterReservationLeavesOnlySafeOvercount(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDOne)
	start := testRecordHour
	current := start
	deps.now = func() time.Time { return current }
	deps.beforeRecordOperation = func(operation recordOperation) {
		if operation == recordOperationQueueOpen {
			current = start.Add(defaultRecordDecisionBudget + time.Nanosecond)
		}
	}
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(start))
	t.Cleanup(func() { _ = permit.Close() })
	if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
		t.Fatalf("RecordOnce = %v", got)
	}
	assertNoQueuedEvents(t, home)
	quota := readQuotaFixture(t, home)
	if quota.Events != 1 || quota.Bytes == 0 {
		t.Fatalf("post-reservation quota = %+v", quota)
	}
}

func TestRecordOnceDoesNotWriteAfterQuotaDirectorySyncIsUncertain(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDOne)
	deps.now = func() time.Time { return testRecordHour }
	quotaRenameSeen := false
	deps.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepRename {
			quotaRenameSeen = true
			return nil
		}
		if step == storageStepDirectorySync && quotaRenameSeen {
			return errors.New("injected quota parent sync failure")
		}
		return nil
	}
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	t.Cleanup(func() { _ = permit.Close() })
	if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
		t.Fatalf("RecordOnce = %v", got)
	}
	assertNoQueuedEvents(t, home)
}

func TestRecordOnceDecisionWindowGatesEveryForegroundQuotaBoundary(t *testing.T) {
	t.Run("quota read", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		root := mustOpenMutableRoot(t, home)
		if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
			t.Fatal(err)
		}
		_ = root.Close()
		current := testRecordHour
		service.deps.now = func() time.Time { return current }
		service.deps.beforeRecordOperation = func(operation recordOperation) {
			if operation == recordOperationQuotaRead {
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			}
		}
		quotaOpens := 0
		service.deps.storageHooks.afterFileOpen = func(path string) {
			if filepath.Base(path) == quotaFileName {
				quotaOpens++
			}
		}
		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("RecordOnce = %v, want dropped", got)
		}
		if quotaOpens != 0 {
			t.Fatalf("expired quota-read boundary opened quota %d times", quotaOpens)
		}
	})

	t.Run("present quota read before control lookup", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		root := mustOpenMutableRoot(t, home)
		if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
			t.Fatal(err)
		}
		_ = root.Close()
		current := testRecordHour
		service.deps.now = func() time.Time { return current }
		service.deps.beforeRecordOperation = func(operation recordOperation) {
			if operation == recordLookupOperation(spoolControlDirectoryName) {
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			}
		}
		quotaOpens := 0
		lookups := 0
		service.deps.storageHooks.afterFileOpen = func(path string) {
			if filepath.Base(path) == quotaFileName {
				quotaOpens++
			}
		}
		service.deps.storageHooks.beforeStep = func(step storageStep) error {
			if step == storageStepEntryStat {
				lookups++
			}
			return nil
		}
		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("RecordOnce = %v, want dropped", got)
		}
		if quotaOpens != 1 || lookups != 0 {
			t.Fatalf("post-quota boundary = quota opens:%d control lookups:%d, want 1/0", quotaOpens, lookups)
		}
	})

	lookupNames := []string{queueDirectoryName, inflightDirectoryName, spoolControlDirectoryName, retiredControlDirectoryName}
	for index, expireName := range lookupNames {
		t.Run("lookup "+expireName, func(t *testing.T) {
			home, service, permit := newRecordServiceFixture(t, testEventIDOne)
			current := testRecordHour
			service.deps.now = func() time.Time { return current }
			service.deps.beforeRecordOperation = func(operation recordOperation) {
				if operation == recordLookupOperation(expireName) {
					current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
				}
			}
			lookups := 0
			service.deps.storageHooks.beforeStep = func(step storageStep) error {
				if step == storageStepEntryStat {
					lookups++
				}
				return nil
			}
			if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
				t.Fatalf("RecordOnce = %v, want dropped", got)
			}
			if lookups != index {
				t.Fatalf("expired lookup %q began %d exact lookups, want %d prior lookups only", expireName, lookups, index)
			}
			if _, err := os.Lstat(filepath.Join(home.Root(), quotaFileName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("expired lookup %q persisted quota: %v", expireName, err)
			}
		})
	}

	for _, test := range []struct {
		operation      recordOperation
		wantQuota      bool
		wantQuotaWrite int
	}{
		{operation: recordOperationControlOpen},
		{operation: recordOperationQuotaStage},
		{operation: recordOperationQuotaInstall, wantQuotaWrite: 1},
		{operation: recordOperationControlClean, wantQuota: true, wantQuotaWrite: 1},
		{operation: recordOperationControlRemove, wantQuota: true, wantQuotaWrite: 1},
		{operation: recordOperationQueueOpen, wantQuota: true, wantQuotaWrite: 1},
		{operation: recordOperationGenerationOpen, wantQuota: true, wantQuotaWrite: 1},
		{operation: recordOperationEventWrite, wantQuota: true, wantQuotaWrite: 1},
	} {
		t.Run(string(test.operation), func(t *testing.T) {
			home, service, permit := newRecordServiceFixture(t, testEventIDOne)
			current := testRecordHour
			service.deps.now = func() time.Time { return current }
			service.deps.beforeRecordOperation = func(operation recordOperation) {
				if operation == test.operation {
					current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
				}
			}
			quotaWrites := 0
			service.deps.storageHooks.beforeStep = func(step storageStep) error {
				if step == storageStepWrite {
					quotaWrites++
				}
				return nil
			}
			if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
				t.Fatalf("RecordOnce = %v, want dropped", got)
			}
			if quotaWrites != test.wantQuotaWrite {
				t.Fatalf("expired %s boundary performed %d quota writes, want %d", test.operation, quotaWrites, test.wantQuotaWrite)
			}
			_, quotaErr := os.Lstat(filepath.Join(home.Root(), quotaFileName))
			if test.wantQuota && quotaErr != nil {
				t.Fatalf("expired %s boundary lost conservative quota: %v", test.operation, quotaErr)
			}
			if !test.wantQuota && !errors.Is(quotaErr, fs.ErrNotExist) {
				t.Fatalf("expired %s boundary installed quota: %v", test.operation, quotaErr)
			}
			eventPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(testEventIDOne))
			if _, err := os.Lstat(eventPath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("expired %s boundary wrote an event: %v", test.operation, err)
			}
		})
	}
}

func TestRecordOnceRejectsActiveOrRetiredControlEvidenceWithPresentQuota(t *testing.T) {
	for _, controlName := range []string{spoolControlDirectoryName, retiredControlDirectoryName} {
		for _, shape := range []string{"directory", "file", "symlink"} {
			t.Run(controlName+"/"+shape, func(t *testing.T) {
				home, service, permit := newRecordServiceFixture(t, testEventIDOne)
				root := mustOpenMutableRoot(t, home)
				queued := testSpoolEvent(testEventIDTwo, "1.0.0", testRecordHour, CommandVersion)
				queuedBytes := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, queued)
				low := spoolQuota{Events: 1, Bytes: 1}
				if err := persistSpoolQuota(root, low); err != nil {
					t.Fatal(err)
				}
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
				controlPath := filepath.Join(home.Root(), controlName)
				switch shape {
				case "directory":
					if err := os.Mkdir(controlPath, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(controlPath, "crash-evidence"), []byte("retained"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "file":
					if err := os.WriteFile(controlPath, []byte("retained"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					sentinel := filepath.Join(t.TempDir(), "sentinel")
					if err := os.WriteFile(sentinel, []byte("retained"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(sentinel, controlPath); err != nil {
						t.Fatal(err)
					}
				}
				quotaPath := filepath.Join(home.Root(), quotaFileName)
				quotaBefore, err := os.ReadFile(quotaPath)
				if err != nil {
					t.Fatal(err)
				}
				enumerations := 0
				service.deps.storageHooks.beforeStep = func(step storageStep) error {
					if step == storageStepEnumerate {
						enumerations++
					}
					return nil
				}

				if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
					t.Fatalf("RecordOnce = %v, want dropped", got)
				}
				if got := readQuotaFixture(t, home); got != low {
					t.Fatalf("control-evidence rejection changed quota to %+v", got)
				}
				quotaAfter, err := os.ReadFile(quotaPath)
				if err != nil || string(quotaAfter) != string(quotaBefore) {
					t.Fatalf("control-evidence rejection changed quota bytes: before=%q after=%q err=%v", quotaBefore, quotaAfter, err)
				}
				if enumerations != 0 {
					t.Fatalf("control-evidence rejection enumerated %d entries", enumerations)
				}
				if _, err := os.Lstat(controlPath); err != nil {
					t.Fatalf("control evidence was not preserved: %v", err)
				}
				queuedPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(queued.EventID))
				if got, err := os.ReadFile(queuedPath); err != nil || string(got) != string(queuedBytes) {
					t.Fatalf("undercounted queued event changed: got=%q err=%v", got, err)
				}
			})
		}
	}
}

func TestRecordOnceRejectsFallbackCursorEvidenceWithPresentLowQuota(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	root := mustOpenMutableRoot(t, home)
	queued := testSpoolEvent(testEventIDTwo, "1.0.0", testRecordHour, CommandVersion)
	queuedBytes := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, queued)
	low := spoolQuota{Events: 1, Bytes: 1}
	if err := persistSpoolQuota(root, low); err != nil {
		t.Fatal(err)
	}
	cursorData, err := encodeRelocationCursor(relocationCursor{Next: maximumRelocationSlots})
	if err != nil {
		t.Fatal(err)
	}
	if err := root.writeFileAtomic(fallbackRelocationCursorName, cursorData); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(home.Root(), quotaFileName)
	quotaBefore, err := os.ReadFile(quotaPath)
	if err != nil {
		t.Fatal(err)
	}
	cursorPath := filepath.Join(home.Root(), fallbackRelocationCursorName)
	cursorBefore, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatal(err)
	}
	enumerations := 0
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepEnumerate {
			enumerations++
		}
		return nil
	}

	if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
		t.Fatalf("RecordOnce = %v, want fallback-cursor drop", got)
	}
	_ = permit.Close()
	quotaAfter, quotaErr := os.ReadFile(quotaPath)
	cursorAfter, cursorErr := os.ReadFile(cursorPath)
	queuedPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(queued.EventID))
	queuedAfter, queuedErr := os.ReadFile(queuedPath)
	if enumerations != 0 || quotaErr != nil || string(quotaAfter) != string(quotaBefore) ||
		cursorErr != nil || string(cursorAfter) != string(cursorBefore) ||
		queuedErr != nil || string(queuedAfter) != string(queuedBytes) {
		t.Fatalf("fallback-cursor rejection = enumerations:%d quota:%q/%q err:%v cursor:%q/%q err:%v queued:%q/%q err:%v",
			enumerations, quotaBefore, quotaAfter, quotaErr, cursorBefore, cursorAfter, cursorErr, queuedBytes, queuedAfter, queuedErr)
	}
}

func TestClaimRestoreDeletePreservesOldestOrderAndQuotaDurability(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()

	oldHour := testRecordHour.Add(-2 * time.Hour)
	newHour := testRecordHour.Add(-time.Hour)
	oldEvent := testSpoolEvent(testEventIDOne, "1.0.0", oldHour, CommandHelp)
	newEvent := testSpoolEvent(testEventIDTwo, "1.0.0", newHour, CommandVersion)
	oldBytes := writeSpoolEventFixture(t, root, inflightDirectoryName, testSpoolGeneration, oldEvent)
	newBytes := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, newEvent)
	oldMTime := time.Unix(100, 123)
	newMTime := time.Unix(200, 456)
	setSpoolMTime(t, home, inflightDirectoryName, oldEvent.EventID, oldMTime)
	setSpoolMTime(t, home, queueDirectoryName, newEvent.EventID, newMTime)
	if err := persistSpoolQuota(root, spoolQuota{Events: 2, Bytes: uint64(len(oldBytes) + len(newBytes))}); err != nil {
		t.Fatal(err)
	}

	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatalf("claimSpoolBatch: %v", err)
	}
	if len(claim.records) != 2 || claim.records[0].event.EventID != testEventIDOne || claim.records[1].event.EventID != testEventIDTwo {
		t.Fatalf("claim order = %+v", claim.records)
	}
	assertSpoolFileLocation(t, home, inflightDirectoryName, testEventIDOne)
	assertSpoolFileLocation(t, home, inflightDirectoryName, testEventIDTwo)

	if err := restoreSpoolClaim(root, claim); err != nil {
		t.Fatalf("restoreSpoolClaim: %v", err)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, testEventIDOne)
	if got := spoolMTime(t, home, queueDirectoryName, testSpoolGeneration, testEventIDOne); !got.Equal(oldMTime) {
		t.Fatalf("restored mtime = %v, want %v", got, oldMTime)
	}

	claim, err = claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if err := deleteSpoolClaim(root, claim); err != nil {
		t.Fatalf("deleteSpoolClaim: %v", err)
	}
	if quota := readQuotaFromRoot(t, root); quota != (spoolQuota{}) {
		t.Fatalf("quota after delete = %+v", quota)
	}
}

func TestRestoreSpoolClaimMismatchedDestinationCollisionPreservesBothFiles(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(root, wantQuota); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil || len(claim.records) != 1 {
		t.Fatalf("claim collision fixture = records:%d err:%v", len(claim.records), err)
	}
	queuePath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	inflightPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	blocker := []byte("different queue occupant")
	if err := os.WriteFile(queuePath, blocker, 0o600); err != nil {
		t.Fatal(err)
	}

	restoreErr := restoreSpoolClaim(root, claim)
	if restoreErr == nil {
		t.Fatal("mismatched restore collision was treated as an exact duplicate")
	}
	if got, err := os.ReadFile(inflightPath); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("mismatched restore collision changed inflight claim: data=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(queuePath); err != nil || !bytes.Equal(got, blocker) {
		t.Fatalf("mismatched restore collision changed queue blocker: data=%q err=%v", got, err)
	}
	if got := readQuotaFromRoot(t, root); got != wantQuota {
		t.Fatalf("mismatched restore collision changed quota: got=%+v want=%+v", got, wantQuota)
	}
}

func TestRestoreSpoolClaimExactDestinationCollisionRetiresOnlyInflightDuplicate(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(root, wantQuota); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil || len(claim.records) != 1 {
		t.Fatalf("claim exact-collision fixture = records:%d err:%v", len(claim.records), err)
	}
	queuePath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	inflightPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	if err := os.WriteFile(queuePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreSpoolClaim(root, claim); err != nil {
		t.Fatalf("restore exact duplicate: %v", err)
	}
	if got, err := os.ReadFile(queuePath); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("exact restore collision changed queue copy: data=%q err=%v", got, err)
	}
	if _, err := os.Lstat(inflightPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("exact restore collision retained inflight duplicate: %v", err)
	}
	if got := readQuotaFromRoot(t, root); got != wantQuota {
		t.Fatalf("exact restore collision changed quota: got=%+v want=%+v", got, wantQuota)
	}
}

func TestRestoreSpoolClaimExactCollisionInstallsClaimedSourceBeforeRetiringDestination(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	name := eventFileName(testEventIDOne)
	queuePath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, name)
	inflightPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, name)
	var armed atomic.Bool
	queueCompletedReads := 0
	rewritten := false
	var rewriteErr error
	var replacement []byte
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		afterRead: func(path string, _, read int, readErr error) {
			if !armed.Load() || path != queuePath || read != 0 || readErr != nil {
				return
			}
			queueCompletedReads++
			if queueCompletedReads != 2 || rewritten {
				return
			}
			rewritten = true
			rewriteErr = os.WriteFile(queuePath, replacement, 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	replacement = bytes.Repeat([]byte{'x'}, len(data))
	wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(root, wantQuota); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil || len(claim.records) != 1 {
		t.Fatalf("claim exact-collision rewrite fixture = records:%d err:%v", len(claim.records), err)
	}
	if err := os.WriteFile(queuePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)

	restoreErr := restoreSpoolClaim(root, claim)
	if restoreErr != nil || rewriteErr != nil || !rewritten {
		t.Fatalf("restore with destination rewrite = restored:%v rewritten:%v rewriteErr:%v", restoreErr, rewritten, rewriteErr)
	}
	queueData, queueErr := os.ReadFile(queuePath)
	var queueStat unix.Stat_t
	statErr := unix.Lstat(queuePath, &queueStat)
	if queueErr != nil || statErr != nil || !bytes.Equal(queueData, data) ||
		(recordIncarnation{dev: unixStatDevice(queueStat), ino: unixStatInode(queueStat)}) != claim.records[0].incarnation {
		t.Fatalf("restored queue lacks claimed source authority: data=%q readErr=%v statErr=%v incarnation=%+v want=%+v",
			queueData, queueErr, statErr,
			recordIncarnation{dev: unixStatDevice(queueStat), ino: unixStatInode(queueStat)}, claim.records[0].incarnation)
	}
	if _, err := os.Lstat(inflightPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("durable source-authoritative restore retained displaced destination: %v", err)
	}
	if got := readQuotaFromRoot(t, root); got != wantQuota {
		t.Fatalf("source-authoritative restore changed quota: got=%+v want=%+v", got, wantQuota)
	}
}

func TestRestoreSpoolClaimExchangeUncertaintyPreservesBothAuthoritiesAndQuota(t *testing.T) {
	for _, test := range []struct {
		name           string
		replace        string
		beforeErr      error
		postErr        error
		failParentSync bool
		wantSwapped    bool
	}{
		{name: "unsupported", beforeErr: unix.ENOSYS},
		{name: "not-applied", beforeErr: unix.EIO},
		{name: "source-identity-replacement", replace: "source"},
		{name: "destination-identity-replacement", replace: "destination"},
		{name: "post-exchange-failure", postErr: unix.EIO, wantSwapped: true},
		{name: "parent-sync-pending", failParentSync: true, wantSwapped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, _, permit := newRecordServiceFixture(t, testEventIDThree)
			name := eventFileName(testEventIDOne)
			queuePath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, name)
			inflightPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, name)
			var armed atomic.Bool
			injected := false
			exchangeApplied := false
			var injectErr error
			hooks := storageTestHooks{
				beforeExchange: func() error {
					if !armed.Load() {
						return nil
					}
					injected = true
					if test.replace == "" {
						return test.beforeErr
					}
					path := inflightPath
					if test.replace == "destination" {
						path = queuePath
					}
					if err := os.Rename(path, path+".displaced"); err != nil {
						injectErr = err
						return err
					}
					injectErr = os.WriteFile(path, []byte("replacement"), 0o600)
					return injectErr
				},
				afterExchange: func() error {
					if !armed.Load() {
						return nil
					}
					injected = true
					exchangeApplied = true
					return test.postErr
				},
				beforeStep: func(step storageStep) error {
					if armed.Load() && exchangeApplied && test.failParentSync && step == storageStepDirectorySync {
						injected = true
						return unix.EIO
					}
					return nil
				},
			}
			root, err := openStorageRootMutableWithHooks(home, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
			wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
			if err := persistSpoolQuota(root, wantQuota); err != nil {
				t.Fatal(err)
			}
			claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
			if err != nil || len(claim.records) != 1 {
				t.Fatalf("claim exchange-uncertainty fixture = records:%d err:%v", len(claim.records), err)
			}
			if err := os.WriteFile(queuePath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			var destinationStat unix.Stat_t
			if err := unix.Lstat(queuePath, &destinationStat); err != nil {
				t.Fatal(err)
			}
			destination := recordIncarnation{dev: unixStatDevice(destinationStat), ino: unixStatInode(destinationStat)}
			armed.Store(true)

			restoreErr := restoreSpoolClaim(root, claim)
			if restoreErr == nil || !injected || injectErr != nil {
				t.Fatalf("uncertain exchange = err:%v injected:%v injectErr:%v", restoreErr, injected, injectErr)
			}
			if got := readQuotaFromRoot(t, root); got != wantQuota {
				t.Fatalf("uncertain exchange changed quota: got=%+v want=%+v", got, wantQuota)
			}

			found := make(map[recordIncarnation]string)
			for _, path := range []string{queuePath, inflightPath, queuePath + ".displaced", inflightPath + ".displaced"} {
				var stat unix.Stat_t
				if err := unix.Lstat(path, &stat); err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						continue
					}
					t.Fatal(err)
				}
				found[recordIncarnation{dev: unixStatDevice(stat), ino: unixStatInode(stat)}] = path
			}
			if found[claim.records[0].incarnation] == "" || found[destination] == "" {
				t.Fatalf("uncertain exchange lost authority: found=%v source=%+v destination=%+v",
					found, claim.records[0].incarnation, destination)
			}
			if test.wantSwapped {
				var queueStat, inflightStat unix.Stat_t
				queueErr := unix.Lstat(queuePath, &queueStat)
				inflightErr := unix.Lstat(inflightPath, &inflightStat)
				if queueErr != nil || inflightErr != nil ||
					(recordIncarnation{dev: unixStatDevice(queueStat), ino: unixStatInode(queueStat)}) != claim.records[0].incarnation ||
					(recordIncarnation{dev: unixStatDevice(inflightStat), ino: unixStatInode(inflightStat)}) != destination {
					t.Fatalf("post-application evidence = queue:%+v/%v inflight:%+v/%v", queueStat, queueErr, inflightStat, inflightErr)
				}
			}
		})
	}
}

func TestRestoreSpoolClaimDestinationChangeAtDeleteBoundaryPreservesInflightSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(string) error
	}{
		{name: "unlink", change: os.Remove},
		{name: "truncate", change: func(path string) error { return os.WriteFile(path, []byte("changed"), 0o600) }},
		{name: "replace", change: func(path string) error {
			if err := os.Rename(path, path+".displaced"); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("replacement"), 0o600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, _, permit := newRecordServiceFixture(t, testEventIDThree)
			name := eventFileName(testEventIDOne)
			queuePath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, name)
			inflightPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, name)
			var armed atomic.Bool
			var changed atomic.Bool
			var changeErr error
			root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
				beforeMutation: func(step storageStep, path string) {
					if step != storageStepDelete || path != inflightPath || !armed.Load() ||
						!changed.CompareAndSwap(false, true) {
						return
					}
					changeErr = test.change(queuePath)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
			if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
				t.Fatal(err)
			}
			claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
			if err != nil || len(claim.records) != 1 {
				t.Fatalf("claim destination-change fixture = records:%d err:%v", len(claim.records), err)
			}
			if err := os.WriteFile(queuePath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			armed.Store(true)
			restoreErr := restoreSpoolClaim(root, claim)
			if changeErr != nil || !changed.Load() {
				t.Fatalf("destination change = changed:%v err:%v", changed.Load(), changeErr)
			}
			if restoreErr == nil {
				t.Fatal("destination change at source-delete boundary authorized retirement")
			}
			if got, err := os.ReadFile(inflightPath); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("destination change removed inflight source: data=%q err=%v", got, err)
			}
		})
	}
}

func TestRestoreSpoolClaimCrossDeviceDestinationNeverAuthorizesInflightDeletion(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	name := eventFileName(testEventIDOne)
	queuePath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, name)
	inflightPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, name)
	var armed atomic.Bool
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		metadata: func(path string, metadata storageMetadata) storageMetadata {
			if armed.Load() && path == queuePath {
				metadata.dev ^= 1 << 63
			}
			return metadata
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil || len(claim.records) != 1 {
		t.Fatalf("claim cross-device fixture = records:%d err:%v", len(claim.records), err)
	}
	if err := os.WriteFile(queuePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	restoreErr := restoreSpoolClaim(root, claim)
	if restoreErr == nil || !errors.Is(restoreErr, syscall.EXDEV) {
		t.Fatalf("cross-device destination restore error = %v, want EXDEV", restoreErr)
	}
	if got, err := os.ReadFile(inflightPath); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cross-device destination removed inflight source: data=%q err=%v", got, err)
	}
}

func TestClaimSpoolBatchRejectsCrossDeviceGenerationAtDirectPostSweepReopen(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	plainRoot := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, plainRoot, queueDirectoryName, testSpoolGeneration, event)
	wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(plainRoot, wantQuota); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	name := eventFileName(event.EventID)
	eventPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, name)
	generationPath := filepath.Dir(eventPath)
	inflightPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, name)
	var afterSweep atomic.Bool
	postSweepGenerationOpens := 0
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		afterRead: func(path string, _, read int, readErr error) {
			if path == eventPath && read == 0 && readErr == nil {
				afterSweep.Store(true)
			}
		},
		metadata: func(path string, metadata storageMetadata) storageMetadata {
			if afterSweep.Load() && path == generationPath {
				metadata.dev ^= 1 << 63
			}
			return metadata
		},
		beforeDirectoryOpen: func(path string) error {
			if afterSweep.Load() && path == generationPath {
				postSweepGenerationOpens++
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	claim, claimErr := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if !afterSweep.Load() || !errors.Is(claimErr, unix.EXDEV) || len(claim.records) != 0 || postSweepGenerationOpens != 0 {
		t.Fatalf("post-sweep cross-device reopen = armed:%v opens:%d records:%d err:%v",
			afterSweep.Load(), postSweepGenerationOpens, len(claim.records), claimErr)
	}
	if got, err := os.ReadFile(eventPath); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("post-sweep cross-device reopen changed queue source: data=%q err=%v", got, err)
	}
	if _, err := os.Lstat(inflightPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("post-sweep cross-device reopen moved source below boundary: %v", err)
	}
	if got := readQuotaFromRoot(t, root); got != wantQuota {
		t.Fatalf("post-sweep cross-device reopen changed quota: got=%+v want=%+v", got, wantQuota)
	}
}

func TestSpoolSweepRejectsCrossDeviceEventBeforeOpeningIt(t *testing.T) {
	for _, tree := range []string{queueDirectoryName, inflightDirectoryName} {
		t.Run(tree, func(t *testing.T) {
			home, _, permit := newRecordServiceFixture(t, testEventIDThree)
			plainRoot := mustOpenMutableRoot(t, home)
			event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, plainRoot, tree, testSpoolGeneration, event)
			wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
			if err := persistSpoolQuota(plainRoot, wantQuota); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}
			eventPath := filepath.Join(home.Root(), tree, testSpoolGeneration, eventFileName(event.EventID))
			fileOpens := 0
			root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
				metadata: func(path string, metadata storageMetadata) storageMetadata {
					if path == eventPath {
						metadata.dev ^= 1 << 63
					}
					return metadata
				},
				afterFileOpen: func(path string) {
					if path == eventPath {
						fileOpens++
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()

			result, sweepErr := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
			if !errors.Is(sweepErr, unix.EXDEV) || result.complete || fileOpens != 0 {
				t.Fatalf("cross-device %s event sweep = complete:%v opens:%d err:%v", tree, result.complete, fileOpens, sweepErr)
			}
			if got, err := os.ReadFile(eventPath); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("cross-device %s event changed source: data=%q err=%v", tree, got, err)
			}
			if got := readQuotaFromRoot(t, root); got.Events < wantQuota.Events || got.Bytes < wantQuota.Bytes {
				t.Fatalf("cross-device %s event undercounted quota: got=%+v minimum=%+v", tree, got, wantQuota)
			}
		})
	}
}

func TestRestoreSpoolClaimByteIdenticalInflightReplacementIsNotOriginalAuthority(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(root, wantQuota); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil || len(claim.records) != 1 {
		t.Fatalf("claim replacement fixture = records:%d err:%v", len(claim.records), err)
	}
	queuePath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	inflightPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	displacedPath := inflightPath + ".displaced"
	if err := os.Rename(inflightPath, displacedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inflightPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(queuePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	restoreErr := restoreSpoolClaim(root, claim)
	if restoreErr == nil {
		t.Fatal("byte-identical inflight replacement inherited original claim authority")
	}
	for _, path := range []string{queuePath, inflightPath, displacedPath} {
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("replacement collision changed %q: data=%q err=%v", path, got, err)
		}
	}
	if got := readQuotaFromRoot(t, root); got != wantQuota {
		t.Fatalf("replacement collision changed quota: got=%+v want=%+v", got, wantQuota)
	}
}

func TestClaimSpoolBatchLateMismatchedRestoreCollisionPreservesInflightSource(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	plainRoot := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, plainRoot, inflightDirectoryName, testSpoolGeneration, event)
	wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(plainRoot, wantQuota); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	name := eventFileName(event.EventID)
	queuePath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, name)
	inflightPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, name)
	blocker := []byte("late mismatched queue occupant")
	injected := false
	var injectErr error
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeMutation: func(step storageStep, sourceName string) {
			if injected || step != storageStepRename || sourceName != name {
				return
			}
			injected = true
			injectErr = os.WriteFile(queuePath, blocker, 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	claim, claimErr := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if injectErr != nil || !injected {
		t.Fatalf("inject late restore collision: injected=%v err=%v", injected, injectErr)
	}
	if claimErr == nil || len(claim.records) != 0 {
		t.Fatalf("late mismatched restore collision = claim:%+v err:%v", claim, claimErr)
	}
	if got, err := os.ReadFile(inflightPath); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("late collision changed inflight source: data=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(queuePath); err != nil || !bytes.Equal(got, blocker) {
		t.Fatalf("late collision changed queue blocker: data=%q err=%v", got, err)
	}
	if got := readQuotaFromRoot(t, root); got != wantQuota {
		t.Fatalf("late collision changed quota: got=%+v want=%+v", got, wantQuota)
	}
}

func TestPrepareSpoolClaimReportsByteCountMismatchWithoutNilWrapArtifact(t *testing.T) {
	_, _, permit := newRecordServiceFixture(t, testEventIDThree)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data, err := EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	claim := spoolClaim{
		generation: permit.spoolGeneration,
		records: []spoolRecord{{
			generation: permit.spoolGeneration,
			name:       eventFileName(event.EventID),
			event:      event,
			bytes:      uint64(len(data) + 1),
		}},
	}
	_, prepareErr := prepareSpoolClaimForUpload(claim, permit)
	if prepareErr == nil || !strings.Contains(prepareErr.Error(), "byte count mismatch") ||
		strings.Contains(prepareErr.Error(), "%!w(<nil>)") {
		t.Fatalf("claimed byte mismatch error = %v", prepareErr)
	}
}

func TestReconcileTreatsSecondFileWithSameEventIDAsPoison(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	writeSpoolEventFixture(t, root, inflightDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 2, Bytes: uint64(2 * len(data))}); err != nil {
		t.Fatal(err)
	}
	result, err := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if result.complete {
		t.Fatal("duplicate-removal pass certified exact quota")
	}
	requireMutationFreeReconcile(t, root, testCurrentSpoolPolicy(), testRecordHour)
	assertSpoolFileLocation(t, home, queueDirectoryName, testEventIDOne)
	if _, err := os.Lstat(filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, eventFileName(testEventIDOne))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("duplicate inflight event remains: %v", err)
	}
	if got := readQuotaFromRoot(t, root); got != (spoolQuota{Events: 1, Bytes: uint64(len(data))}) {
		t.Fatalf("deduplicated quota = %+v", got)
	}
}

func TestReconcileDuplicateIDKeepsOldestFileAcrossQueueAndInflight(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	writeSpoolEventFixture(t, root, inflightDirectoryName, testSpoolGeneration, event)
	setSpoolMTime(t, home, queueDirectoryName, event.EventID, time.Unix(200, 0))
	setSpoolMTime(t, home, inflightDirectoryName, event.EventID, time.Unix(100, 0))
	if err := persistSpoolQuota(root, spoolQuota{Events: 2, Bytes: uint64(2 * len(data))}); err != nil {
		t.Fatal(err)
	}
	result, err := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if result.complete {
		t.Fatal("older-duplicate selection pass certified exact quota")
	}
	requireMutationFreeReconcile(t, root, testCurrentSpoolPolicy(), testRecordHour)
	assertSpoolFileLocation(t, home, inflightDirectoryName, testEventIDOne)
	if _, err := os.Lstat(filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(testEventIDOne))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("newer duplicate queue event remains: %v", err)
	}
}

func TestSettledClaimCannotDeleteARestoredEventOrReleaseQuotaTwice(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	eventOne := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	eventTwo := testSpoolEvent(testEventIDTwo, "1.0.0", testRecordHour, CommandHelp)
	bytesOne := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, eventOne)
	bytesTwo := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, eventTwo)
	if err := persistSpoolQuota(root, spoolQuota{Events: 2, Bytes: uint64(len(bytesOne) + len(bytesTwo))}); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.records) != 2 {
		t.Fatalf("claim records = %d", len(claim.records))
	}
	if err := restoreSpoolClaim(root, claim); err != nil {
		t.Fatal(err)
	}
	if err := deleteSpoolClaim(root, claim); err == nil {
		t.Fatal("settled restored claim was accepted for deletion")
	}
	wantQuota := spoolQuota{Events: 2, Bytes: uint64(len(bytesOne) + len(bytesTwo))}
	if got := readQuotaFromRoot(t, root); got != wantQuota {
		t.Fatalf("restored-then-deleted quota = %+v, want %+v", got, wantQuota)
	}

	claim, err = claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteSpoolClaim(root, claim); err != nil {
		t.Fatal(err)
	}
	if err := deleteSpoolClaim(root, claim); err == nil {
		t.Fatal("settled deleted claim was accepted a second time")
	}
	if got := readQuotaFromRoot(t, root); got != (spoolQuota{}) {
		t.Fatalf("double delete quota = %+v", got)
	}
}

func TestClaimDeleteSyncFailureCannotUndercountAndReconciliationRepairs(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	deleteStarted := false
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepDelete {
			deleteStarted = true
		}
		if step == storageStepDirectorySync && deleteStarted {
			return errors.New("injected claimed-delete parent sync failure")
		}
		return nil
	}}
	uncertainRoot, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteSpoolClaim(uncertainRoot, claim); err == nil {
		t.Fatal("delete with uncertain parent sync unexpectedly succeeded")
	}
	_ = uncertainRoot.Close()
	if got := readQuotaFixture(t, home); got != (spoolQuota{Events: 1, Bytes: uint64(len(data))}) {
		t.Fatalf("uncertain delete lowered quota: %+v", got)
	}

	repairRoot := mustOpenMutableRoot(t, home)
	defer func() { _ = repairRoot.Close() }()
	result, err := reconcileSpool(repairRoot, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if !result.complete || readQuotaFromRoot(t, repairRoot) != (spoolQuota{}) {
		t.Fatalf("reconciliation did not repair conservative delete: %+v", result)
	}
}

func TestClaimDeleteIdentityDisappearanceCannotReleaseQuota(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(root, wantQuota); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	victimPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	displacedPath := victimPath + ".displaced"
	var injectedErr error
	swapped := false
	hooks := storageTestHooks{beforeMutation: func(step storageStep, _ string) {
		if swapped || step != storageStepDelete {
			return
		}
		swapped = true
		injectedErr = os.Rename(victimPath, displacedPath)
	}}
	uncertainRoot, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	deleteErr := deleteSpoolClaim(uncertainRoot, claim)
	_ = uncertainRoot.Close()
	displacedData, displacedErr := os.ReadFile(displacedPath)
	gotQuota := readQuotaFixture(t, home)
	if injectedErr != nil || !swapped || !errors.Is(deleteErr, errStorageEntryChanged) ||
		errors.Is(deleteErr, fs.ErrNotExist) || gotQuota != wantQuota ||
		displacedErr != nil || !bytes.Equal(displacedData, data) {
		t.Fatalf("identity disappearance settlement = swapped:%v injected:%v delete:%v quota:%+v displaced:%q displacedErr:%v",
			swapped, injectedErr, deleteErr, gotQuota, displacedData, displacedErr)
	}
}

func TestClaimDeleteByteIdenticalReplacementCannotReleaseQuota(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(root, wantQuota); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	victimPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	displacedPath := victimPath + ".displaced"
	var injectedErr error
	swapped := false
	hooks := storageTestHooks{afterRead: func(path string, _, read int, _ error) {
		if swapped || path != victimPath || read == 0 {
			return
		}
		swapped = true
		injectedErr = os.Rename(victimPath, displacedPath)
		if injectedErr == nil {
			injectedErr = os.WriteFile(victimPath, data, 0o600)
		}
	}}
	uncertainRoot, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	deleteErr := deleteSpoolClaim(uncertainRoot, claim)
	_ = uncertainRoot.Close()
	victimData, victimErr := os.ReadFile(victimPath)
	displacedData, displacedErr := os.ReadFile(displacedPath)
	gotQuota := readQuotaFixture(t, home)
	if injectedErr != nil || !swapped || !errors.Is(deleteErr, errStorageEntryChanged) ||
		errors.Is(deleteErr, fs.ErrNotExist) || gotQuota != wantQuota ||
		victimErr != nil || !bytes.Equal(victimData, data) ||
		displacedErr != nil || !bytes.Equal(displacedData, data) {
		t.Fatalf("byte-identical replacement settlement = swapped:%v injected:%v delete:%v quota:%+v victim:%q victimErr:%v displaced:%q displacedErr:%v",
			swapped, injectedErr, deleteErr, gotQuota, victimData, victimErr, displacedData, displacedErr)
	}
}

func TestClaimDeleteFinalGateSwapCannotReleaseQuota(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(root, wantQuota); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	victimPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	displacedPath := victimPath + ".displaced"
	var injectedErr error
	deleteStarted := false
	finalMetadataObserved := false
	swapped := false
	postSwapSyncs := 0
	hooks := storageTestHooks{
		decisionGate: func() bool {
			if finalMetadataObserved && !swapped {
				swapped = true
				injectedErr = os.Rename(victimPath, displacedPath)
				if injectedErr == nil {
					injectedErr = os.WriteFile(victimPath, data, 0o600)
				}
			}
			return true
		},
		beforeMutation: func(step storageStep, _ string) {
			if step == storageStepDelete {
				deleteStarted = true
			}
		},
		metadata: func(path string, metadata storageMetadata) storageMetadata {
			if deleteStarted && path == victimPath {
				finalMetadataObserved = true
			}
			return metadata
		},
		beforeStep: func(step storageStep) error {
			if swapped && step == storageStepDirectorySync {
				postSwapSyncs++
			}
			return nil
		},
	}
	uncertainRoot, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	deleteErr := deleteSpoolClaim(uncertainRoot, claim)
	_ = uncertainRoot.Close()
	victimData, victimErr := os.ReadFile(victimPath)
	displacedData, displacedErr := os.ReadFile(displacedPath)
	gotQuota := readQuotaFixture(t, home)
	if injectedErr != nil || !deleteStarted || !finalMetadataObserved || !swapped ||
		!errors.Is(deleteErr, errStorageEntryChanged) || errors.Is(deleteErr, fs.ErrNotExist) ||
		gotQuota != wantQuota || postSwapSyncs != 0 || victimErr != nil || !bytes.Equal(victimData, data) ||
		displacedErr != nil || !bytes.Equal(displacedData, data) {
		t.Fatalf("final-gate replacement settlement = deleteStarted:%v finalMetadata:%v swapped:%v injected:%v delete:%v quota:%+v syncs:%d victim:%q victimErr:%v displaced:%q displacedErr:%v",
			deleteStarted, finalMetadataObserved, swapped, injectedErr, deleteErr, gotQuota, postSwapSyncs,
			victimData, victimErr, displacedData, displacedErr)
	}
}

func TestClaimDeleteMissingFileSyncFailureCannotReleaseQuota(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	wantQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(root, wantQuota); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	victimPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	if err := os.Remove(victimPath); err != nil {
		t.Fatal(err)
	}

	armed := false
	failedSync := false
	hooks := storageTestHooks{
		beforeMetadataAttempt: func(path string) error {
			if path == victimPath {
				armed = true
			}
			return nil
		},
		beforeStep: func(step storageStep) error {
			if armed && !failedSync && step == storageStepDirectorySync {
				failedSync = true
				return errors.New("injected missing-claim parent sync failure")
			}
			return nil
		},
	}
	uncertainRoot, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	deleteErr := deleteSpoolClaim(uncertainRoot, claim)
	_ = uncertainRoot.Close()
	gotQuota := readQuotaFixture(t, home)
	if deleteErr == nil || !failedSync || gotQuota != wantQuota {
		t.Fatalf("missing claim sync failure = err:%v failedSync:%v quota:%+v", deleteErr, failedSync, gotQuota)
	}
}

func TestMissingInflightSettlementRequiresDurableQueueDisposition(t *testing.T) {
	tests := []struct {
		name           string
		setupQueue     func(*testing.T, string, string, []byte) storageTestHooks
		wantDeleteErr  bool
		wantReleased   bool
		wantReconciled bool
	}{
		{name: "durably absent", wantReleased: true},
		{name: "exact restored", wantReconciled: true, setupQueue: func(t *testing.T, _, eventPath string, data []byte) storageTestHooks {
			if err := os.WriteFile(eventPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			return storageTestHooks{}
		}},
		{name: "queue open error", wantDeleteErr: true, setupQueue: func(t *testing.T, generationPath, _ string, _ []byte) storageTestHooks {
			if err := os.Remove(generationPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(generationPath, []byte("not-a-directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			return storageTestHooks{}
		}},
		{name: "queue metadata validation error", wantDeleteErr: true, wantReconciled: true, setupQueue: func(t *testing.T, _, eventPath string, data []byte) storageTestHooks {
			if err := os.WriteFile(eventPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			injected := false
			return storageTestHooks{beforeMetadataAttempt: func(path string) error {
				if !injected && path == eventPath {
					injected = true
					return errors.New("injected queue read metadata failure")
				}
				return nil
			}}
		}},
		{name: "event absence sync error", wantDeleteErr: true, setupQueue: func(t *testing.T, _, eventPath string, _ []byte) storageTestHooks {
			armed := false
			failed := false
			t.Cleanup(func() {
				if !failed {
					t.Error("event absence proof never reached its parent sync")
				}
			})
			return storageTestHooks{
				beforeMetadataAttempt: func(path string) error {
					if path == eventPath {
						armed = true
					}
					return nil
				},
				beforeStep: func(step storageStep) error {
					if armed && step == storageStepDirectorySync {
						failed = true
						return errors.New("injected queue event absence sync failure")
					}
					return nil
				},
			}
		}},
		{name: "durably absent generation", wantReleased: true, setupQueue: func(t *testing.T, generationPath, _ string, _ []byte) storageTestHooks {
			if err := os.Remove(generationPath); err != nil {
				t.Fatal(err)
			}
			return storageTestHooks{}
		}},
		{name: "generation absence sync error", wantDeleteErr: true, setupQueue: func(t *testing.T, generationPath, _ string, _ []byte) storageTestHooks {
			if err := os.Remove(generationPath); err != nil {
				t.Fatal(err)
			}
			armed := false
			failed := false
			t.Cleanup(func() {
				if !failed {
					t.Error("generation absence proof never reached its parent sync")
				}
			})
			return storageTestHooks{
				afterDirectoryAttempt: func(path string, err error) {
					if path == generationPath && errors.Is(err, fs.ErrNotExist) {
						armed = true
					}
				},
				beforeStep: func(step storageStep) error {
					if armed && step == storageStepDirectorySync {
						failed = true
						return errors.New("injected queue generation absence sync failure")
					}
					return nil
				},
			}
		}},
		{name: "durably absent queue", wantReleased: true, setupQueue: func(t *testing.T, generationPath, _ string, _ []byte) storageTestHooks {
			queuePath := filepath.Dir(generationPath)
			if err := os.Remove(generationPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(queuePath); err != nil {
				t.Fatal(err)
			}
			return storageTestHooks{}
		}},
		{name: "queue absence sync error", wantDeleteErr: true, setupQueue: func(t *testing.T, generationPath, _ string, _ []byte) storageTestHooks {
			queuePath := filepath.Dir(generationPath)
			if err := os.Remove(generationPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(queuePath); err != nil {
				t.Fatal(err)
			}
			armed := false
			failed := false
			t.Cleanup(func() {
				if !failed {
					t.Error("queue-root absence proof never reached its parent sync")
				}
			})
			return storageTestHooks{
				afterDirectoryAttempt: func(path string, err error) {
					if path == queuePath && errors.Is(err, fs.ErrNotExist) {
						armed = true
					}
				},
				beforeStep: func(step storageStep) error {
					if armed && step == storageStepDirectorySync {
						failed = true
						return errors.New("injected queue root absence sync failure")
					}
					return nil
				},
			}
		}},
		{name: "event reappears after absence sync", wantDeleteErr: true, wantReconciled: true, setupQueue: func(t *testing.T, _, eventPath string, data []byte) storageTestHooks {
			metadataAttempts := 0
			reappeared := false
			t.Cleanup(func() {
				if !reappeared {
					t.Error("event was not recreated at the post-sync recheck")
				}
			})
			return storageTestHooks{beforeMetadataAttempt: func(path string) error {
				if path != eventPath {
					return nil
				}
				metadataAttempts++
				if metadataAttempts == 2 {
					if err := os.WriteFile(eventPath, data, 0o600); err != nil {
						return err
					}
					reappeared = true
				}
				return nil
			}}
		}},
		{name: "changed malformed file", wantDeleteErr: true, setupQueue: func(t *testing.T, _, eventPath string, _ []byte) storageTestHooks {
			if err := os.WriteFile(eventPath, []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
			return storageTestHooks{}
		}},
		{name: "unsafe symlink", wantDeleteErr: true, setupQueue: func(t *testing.T, _, eventPath string, _ []byte) storageTestHooks {
			sentinel := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(sentinel, eventPath); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside" {
					t.Errorf("queue settlement changed outside sentinel: data=%q err=%v", data, err)
				}
			})
			return storageTestHooks{}
		}},
		{name: "unreadable file", wantDeleteErr: true, setupQueue: func(t *testing.T, _, eventPath string, data []byte) storageTestHooks {
			if err := os.WriteFile(eventPath, data, 0o000); err != nil {
				t.Fatal(err)
			}
			return storageTestHooks{}
		}},
		{name: "directory at event name", wantDeleteErr: true, setupQueue: func(t *testing.T, _, eventPath string, _ []byte) storageTestHooks {
			if err := os.Mkdir(eventPath, 0o700); err != nil {
				t.Fatal(err)
			}
			return storageTestHooks{}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, _, permit := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
			initialQuota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
			if err := persistSpoolQuota(root, initialQuota); err != nil {
				t.Fatal(err)
			}
			claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
			if err != nil || len(claim.records) != 1 {
				t.Fatalf("claim fixture = records:%d err:%v", len(claim.records), err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			inflightPath := filepath.Join(home.Root(), inflightDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
			if err := os.Remove(inflightPath); err != nil {
				t.Fatal(err)
			}
			queueGenerationPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
			queueEventPath := filepath.Join(queueGenerationPath, eventFileName(event.EventID))
			hooks := storageTestHooks{}
			if test.setupQueue != nil {
				hooks = test.setupQueue(t, queueGenerationPath, queueEventPath, data)
			}
			settlementRoot, err := openStorageRootMutableWithHooks(home, hooks)
			if err != nil {
				t.Fatal(err)
			}
			deleteErr := deleteSpoolClaim(settlementRoot, claim)
			closeErr := settlementRoot.Close()
			gotQuota := readQuotaFixture(t, home)
			wantQuota := initialQuota
			if test.wantReleased {
				wantQuota = spoolQuota{}
			}
			if (deleteErr != nil) != test.wantDeleteErr || closeErr != nil || gotQuota != wantQuota {
				t.Fatalf("missing-inflight settlement = err:%v wantErr:%v close:%v quota:%+v want:%+v",
					deleteErr, test.wantDeleteErr, closeErr, gotQuota, wantQuota)
			}

			result := spoolSweepResult{}
			for attempts := 0; attempts < 32 && !result.complete; attempts++ {
				repairRoot := mustOpenMutableRoot(t, home)
				result, err = reconcileSpool(repairRoot, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
				closeErr = repairRoot.Close()
				if err != nil || closeErr != nil {
					t.Fatal(errors.Join(err, closeErr))
				}
			}
			wantReconciled := spoolQuota{}
			if test.wantReconciled {
				wantReconciled = initialQuota
			}
			if !result.complete || result.quota != wantReconciled {
				t.Fatalf("missing-inflight reconciliation = %+v, want quota %+v", result, wantReconciled)
			}
		})
	}
}

func TestClaimEnforcesBatchCountAndEncodedRequestLimit(t *testing.T) {
	home := newMetricsTestHome(t)
	longRelease := "1.0.0-" + strings.Repeat("a", 3000)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	deps.release.releaseVersion = longRelease
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	t.Cleanup(func() { _ = permit.Close() })
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	quota := spoolQuota{}
	for index := 0; index < maximumBatchEvents+5; index++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1, index+1)
		event := testSpoolEvent(id, longRelease, testRecordHour, CommandHelp)
		data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
		quota.Events++
		quota.Bytes += uint64(len(data))
	}
	if err := persistSpoolQuota(root, quota); err != nil {
		t.Fatal(err)
	}
	claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	batchBytes, err := EncodeBatch(Batch{SchemaVersion: SchemaVersionV1, Events: claim.events()})
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.records) == 0 || len(claim.records) >= maximumBatchEvents {
		t.Fatalf("request cap selected %d records", len(claim.records))
	}
	if len(batchBytes) > maximumRequestBytes {
		t.Fatalf("encoded claim = %d bytes", len(batchBytes))
	}
}

func TestReconcileSpoolDeletesPoisonExpiredAndNonCurrentGenerationsWithoutReadingSparseFiles(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	valid := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	validBytes := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, valid)
	expired := testSpoolEvent(testEventIDTwo, "1.0.0", testRecordHour.Add(-(maximumEventAgeHours+1)*time.Hour), CommandHelp)
	writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, expired)
	current := mustOpenSpoolGeneration(t, root, queueDirectoryName, testSpoolGeneration)
	if err := current.writeFileAtomicNoReplace(eventFileName(testEventIDThree), []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	mismatchID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	mismatch := testSpoolEvent("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "1.0.0", testRecordHour, CommandHelp)
	writeNamedSpoolFixture(t, current, eventFileName(mismatchID), mismatch)
	sparseName := eventFileName("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	sparsePath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, sparseName)
	if err := os.WriteFile(sparsePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(sparsePath, 1<<30); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"))
	if err := os.Symlink(filepath.Join(home.Root(), configFileName), symlinkPath); err != nil {
		t.Fatal(err)
	}
	oversizedName := strings.Repeat("x", maximumStorageNameBytes+1)
	if err := os.WriteFile(filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, oversizedName), []byte("poison"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "poison"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = current.Close()

	oldGeneration := "99999999-9999-4999-8999-999999999999"
	oldEvent := testSpoolEvent("ffffffff-ffff-4fff-8fff-ffffffffffff", "1.0.0", testRecordHour, CommandHelp)
	writeSpoolEventFixture(t, root, queueDirectoryName, oldGeneration, oldEvent)
	if err := persistSpoolQuota(root, spoolQuota{Events: 100, Bytes: maximumSpoolBytes}); err != nil {
		t.Fatal(err)
	}

	result, err := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatalf("reconcileSpool: %v", err)
	}
	if result.complete {
		t.Fatal("poison-cleanup pass certified exact quota")
	}
	if result.usage.readBytes >= 1<<30 {
		t.Fatalf("sparse poison charged declared bytes: %+v", result.usage)
	}
	requireMutationFreeReconcile(t, root, testCurrentSpoolPolicy(), testRecordHour)
	if quota := readQuotaFromRoot(t, root); quota != (spoolQuota{Events: 1, Bytes: uint64(len(validBytes))}) {
		t.Fatalf("reconciled quota = %+v", quota)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, testEventIDOne)
	for _, path := range []string{
		filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(testEventIDTwo)),
		filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(testEventIDThree)),
		filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(mismatchID)),
		sparsePath, symlinkPath,
		filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, oversizedName), nested,
		filepath.Join(home.Root(), queueDirectoryName, oldGeneration),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("poison path remains %q: %v", path, err)
		}
	}
}

func TestReconcileMutationPassCannotCertifyQuotaAndMutationFreePassConverges(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()

	want := spoolQuota{}
	for index := 0; index < 80; index++ {
		tree := queueDirectoryName
		if index%2 != 0 {
			tree = inflightDirectoryName
		}
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1000, index+1000)
		data := writeSpoolEventFixture(t, root, tree, testSpoolGeneration,
			testSpoolEvent(id, "1.0.0", testRecordHour, CommandHelp))
		want.Events++
		want.Bytes += uint64(len(data))

		directory := mustOpenSpoolGeneration(t, root, tree, testSpoolGeneration)
		if err := directory.writeFileAtomicNoReplace(fmt.Sprintf("poison-%03d", index), []byte("not-json")); err != nil {
			t.Fatal(err)
		}
		_ = directory.Close()
		if index%10 == 0 {
			nested := filepath.Join(home.Root(), tree, testSpoolGeneration, fmt.Sprintf("nested-%03d", index))
			if err := os.Mkdir(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(nested, "poison"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for index := 0; index < 6; index++ {
		generation := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+2000, index+2000)
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+3000, index+3000)
		writeSpoolEventFixture(t, root, queueDirectoryName, generation,
			testSpoolEvent(id, "1.0.0", testRecordHour, CommandHelp))
	}
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	if err := persistSpoolQuota(root, markers); err != nil {
		t.Fatal(err)
	}

	result, err := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if result.complete {
		t.Fatal("tree-mutating reconcile pass certified exact quota")
	}
	if got := readQuotaFromRoot(t, root); got != markers {
		t.Fatalf("tree-mutating reconcile lowered conservative quota to %+v", got)
	}

	for attempts := 0; attempts < 8 && !result.complete; attempts++ {
		result, err = reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
		if err != nil {
			t.Fatal(err)
		}
		if !result.complete {
			if got := readQuotaFromRoot(t, root); got != markers {
				t.Fatalf("additional tree-mutating reconcile lowered quota to %+v", got)
			}
		}
	}
	if !result.complete {
		t.Fatal("mutation-free bounded reconcile did not converge")
	}
	if got := readQuotaFromRoot(t, root); got != want {
		t.Fatalf("mutation-free reconcile quota = %+v, want %+v", got, want)
	}
}

func TestReconcileSpoolUnlinksSymlinkedKnownTreeWithoutFollowingIt(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	target := filepath.Join(filepath.Dir(home.Root()), "outside-tree")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home.Root(), queueDirectoryName)); err != nil {
		t.Fatal(err)
	}
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	result, err := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if result.complete {
		t.Fatal("symlink-removal pass certified exact quota")
	}
	requireMutationFreeReconcile(t, root, testCurrentSpoolPolicy(), testRecordHour)
	if _, err := os.Lstat(filepath.Join(home.Root(), queueDirectoryName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("symlinked queue remains: %v", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatalf("cleanup followed symlink: data=%q err=%v", data, err)
	}
}

func TestReconcileSpoolEnforcesExactFiveThousandEventBoundaryOldestFirst(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	directoryPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
	if err := os.MkdirAll(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	quota := spoolQuota{}
	oldestID := ""
	for index := 0; index < int(maximumEnumerationEvents); index++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1, index+1)
		if index == 0 {
			oldestID = id
		}
		data, err := EncodeEvent(testSpoolEvent(id, "1.0.0", testRecordHour, CommandHelp))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directoryPath, eventFileName(id)), data, 0o600); err != nil {
			t.Fatal(err)
		}
		quota.Events++
		quota.Bytes += uint64(len(data))
	}
	oldTime := time.Unix(1, 0)
	if err := os.Chtimes(filepath.Join(directoryPath, eventFileName(oldestID)), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := persistSpoolQuota(root, quota); err != nil {
		t.Fatal(err)
	}
	result, err := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if result.complete {
		t.Fatal("count-pruning pass certified exact quota")
	}
	requireMutationFreeReconcile(t, root, testCurrentSpoolPolicy(), testRecordHour)
	if got := readQuotaFromRoot(t, root); got.Events != maximumSpoolEvents || got.Bytes >= quota.Bytes {
		t.Fatalf("boundary quota = %+v, original %+v", got, quota)
	}
	if _, err := os.Lstat(filepath.Join(directoryPath, eventFileName(oldestID))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("oldest overflow event remains: %v", err)
	}
}

func TestReconcileSpoolEnforcesFourMiBByteBoundaryOldestFirst(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	directoryPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
	if err := os.MkdirAll(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	longRelease := "1.0.0-" + strings.Repeat("a", 3400)
	quota := spoolQuota{}
	oldestID := ""
	for index := 0; quota.Bytes <= maximumSpoolBytes; index++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1, index+1)
		if index == 0 {
			oldestID = id
		}
		data, err := EncodeEvent(testSpoolEvent(id, longRelease, testRecordHour, CommandHelp))
		if err != nil {
			t.Fatal(err)
		}
		if uint64(len(data)) > maximumEventBytes {
			t.Fatalf("byte-boundary fixture is %d bytes", len(data))
		}
		if err := os.WriteFile(filepath.Join(directoryPath, eventFileName(id)), data, 0o600); err != nil {
			t.Fatal(err)
		}
		quota.Events++
		quota.Bytes += uint64(len(data))
	}
	oldTime := time.Unix(1, 0)
	if err := os.Chtimes(filepath.Join(directoryPath, eventFileName(oldestID)), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := persistSpoolQuota(root, spoolQuota{Events: quota.Events, Bytes: maximumQuotaByteMarker}); err != nil {
		t.Fatal(err)
	}
	result, err := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if result.complete {
		t.Fatal("byte-pruning pass certified exact quota")
	}
	requireMutationFreeReconcile(t, root, testCurrentSpoolPolicy(), testRecordHour)
	if got := readQuotaFromRoot(t, root); got.Bytes > maximumSpoolBytes || got.Events >= quota.Events {
		t.Fatalf("byte-boundary quota = %+v, original %+v", got, quota)
	}
	if _, err := os.Lstat(filepath.Join(directoryPath, eventFileName(oldestID))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("oldest byte-overflow event remains: %v", err)
	}
}

func TestPurgeSpoolUsesOneGlobalBudgetAndConvergesAcrossCalls(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	quota := spoolQuota{}
	for index := 0; index < 8; index++ {
		generation := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+100, index+100)
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+200, index+200)
		event := testSpoolEvent(id, "1.0.0", testRecordHour, CommandHelp)
		data := writeSpoolEventFixture(t, root, queueDirectoryName, generation, event)
		quota.Events++
		quota.Bytes += uint64(len(data))
	}
	if err := persistSpoolQuota(root, quota); err != nil {
		t.Fatal(err)
	}
	budget := spoolWorkBudget{maxEntries: 3, maxDirectories: 3, maxReadBytes: 1, maxNameBytes: 1024}
	result, err := purgeSpool(root, budget)
	if err != nil {
		t.Fatal(err)
	}
	if result.complete {
		t.Fatal("tiny root-global budget falsely reported complete")
	}
	if result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories || result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes {
		t.Fatalf("cleanup exceeded budget: result=%+v budget=%+v", result.usage, budget)
	}
	if got := readQuotaFromRoot(t, root); got != quota {
		t.Fatalf("incomplete purge reset/lowered quota: %+v", got)
	}

	removedEvents := result.removedEvents
	removedBytes := result.removedBytes
	for attempts := 0; attempts < 20 && !result.complete; attempts++ {
		result, err = purgeSpool(root, defaultSpoolWorkBudget())
		if err != nil {
			t.Fatal(err)
		}
		removedEvents += result.removedEvents
		removedBytes += result.removedBytes
	}
	if !result.complete {
		t.Fatal("bounded repeated purge did not converge")
	}
	if got := readQuotaFromRoot(t, root); got != (spoolQuota{}) {
		t.Fatalf("complete purge quota = %+v", got)
	}
	if removedEvents != quota.Events || removedBytes != quota.Bytes {
		t.Fatalf("summed removal = (%d, %d), want (%d, %d)", removedEvents, removedBytes, quota.Events, quota.Bytes)
	}
}

func TestPurgeSpoolWithinBudgetConvergesWithOneAggregateMeter(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	quota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(root, quota); err != nil {
		t.Fatal(err)
	}
	rootTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa11)))
	writeJournaledRootTempCrashFixture(t, root, filepath.Base(rootTemp), []byte("identity-bearing temp"), 0)

	budget := defaultSpoolWorkBudget()
	result, err := purgeSpoolWithinBudget(root, budget)
	if err != nil || !result.complete {
		t.Fatalf("aggregate bounded purge = %+v err=%v", result, err)
	}
	if result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories ||
		result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes ||
		result.eventEntries > maximumEnumerationEvents {
		t.Fatalf("aggregate bounded purge exceeded one budget: result=%+v budget=%+v", result, budget)
	}
	if result.usage.entries < 2*spoolFixedEntryEnvelope ||
		result.usage.nameBytes < 2*spoolFixedNameEnvelope ||
		result.usage.readBytes < 2*spoolFixedReadEnvelope {
		t.Fatalf("multi-pass purge did not reserve fixed work per pass: %+v", result.usage)
	}
	if result.removedEvents != quota.Events || result.removedBytes != quota.Bytes {
		t.Fatalf("aggregate removal = (%d, %d), want (%d, %d)",
			result.removedEvents, result.removedBytes, quota.Events, quota.Bytes)
	}
	if _, err := os.Lstat(rootTemp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("aggregate bounded purge left root temp: %v", err)
	}
	if got := readQuotaFromRoot(t, root); got != (spoolQuota{}) {
		t.Fatalf("aggregate bounded purge quota = %+v", got)
	}
}

func TestPurgeSpoolWithinBudgetKeepsCumulativeEventCapAcrossPasses(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	directoryPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
	if err := os.MkdirAll(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := uint64(0); index < maximumEnumerationEvents+1; index++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1, index+1)
		if err := os.WriteFile(filepath.Join(directoryPath, eventFileName(id)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	quota := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	if err := persistSpoolQuota(root, quota); err != nil {
		t.Fatal(err)
	}
	budget := spoolWorkBudget{
		maxEntries:     maximumCleanupEntries * 4,
		maxDirectories: maximumCleanupDirectories,
		maxReadBytes:   maximumCleanupReadBytes * 4,
		maxNameBytes:   maximumCleanupNameBytes * 4,
	}
	result, err := purgeSpoolWithinBudget(root, budget)
	if err != nil {
		t.Fatal(err)
	}
	if result.complete || result.eventEntries > maximumEnumerationEvents {
		t.Fatalf("event-capped aggregate purge = %+v", result)
	}
	if result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories ||
		result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes {
		t.Fatalf("event-capped aggregate purge exceeded one budget: result=%+v budget=%+v", result, budget)
	}
	entries, readErr := os.ReadDir(directoryPath)
	if readErr != nil || len(entries) == 0 {
		t.Fatalf("event-capped purge did not retain unproven work: entries=%d err=%v", len(entries), readErr)
	}
}

func TestPurgeSpoolTreatsFutureControlFilesAsUnrecognizedResidue(t *testing.T) {
	for _, name := range []string{"status.toml"} {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, disabledState(7, 2, cleanupDisable))
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
			result, purgeErr := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
			if purgeErr == nil || result.complete {
				t.Fatalf("future control residue was certified clean: result=%+v err=%v", result, purgeErr)
			}
			if mutations != 0 {
				t.Fatalf("future control residue received %d mutation attempts", mutations)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "future owner residue" {
				t.Fatalf("future control residue changed: data=%q err=%v", data, err)
			}
		})
	}
}

func TestPurgeSpoolPreservesFutureControlResidueShapesWithoutDescent(t *testing.T) {
	for _, name := range []string{"status.toml"} {
		for _, shape := range []string{"hardlink", "symlink", "cross-device"} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				home := newMetricsTestHome(t)
				writeStateFixture(t, home, disabledState(7, 2, cleanupDisable))
				plainRoot := mustOpenMutableRoot(t, home)
				if err := persistSpoolQuota(plainRoot, spoolQuota{}); err != nil {
					t.Fatal(err)
				}
				if err := plainRoot.Close(); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(home.Root(), name)
				want := "future owner residue"
				if shape == "symlink" || shape == "hardlink" {
					target := filepath.Join(t.TempDir(), "sentinel")
					if err := os.WriteFile(target, []byte(want), 0o600); err != nil {
						t.Fatal(err)
					}
					if shape == "symlink" {
						if err := os.Symlink(target, path); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Link(target, path); err != nil {
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
				result, purgeErr := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
				if purgeErr == nil || result.complete || mutations != 0 || opens != 0 {
					t.Fatalf("future control %s residue = result:%+v mutations:%d opens:%d err:%v",
						shape, result, mutations, opens, purgeErr)
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != want {
					t.Fatalf("future control %s residue changed: data=%q err=%v", shape, data, err)
				}
				if shape == "symlink" {
					if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
						t.Fatalf("future control symlink was replaced: info=%v err=%v", info, err)
					}
				}
			})
		}
	}
}

func TestPurgeSpawnThrottleRemovesSafeCorruptAndOversizedRecords(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "corrupt", body: []byte("throttle_schema = [\n")},
		{name: "oversized", body: bytes.Repeat([]byte("x"), maximumSpawnThrottleBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, disabledState(7, 2, cleanupDisable))
			plainRoot := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(plainRoot, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home.Root(), spawnThrottleFileName)
			if err := os.WriteFile(path, test.body, 0o600); err != nil {
				t.Fatal(err)
			}

			root := mustOpenMutableRoot(t, home)
			defer func() { _ = root.Close() }()
			result, err := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
			if err != nil || !result.complete {
				t.Fatalf("purge safe %s spawn throttle = result:%+v err:%v", test.name, result, err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("purge retained safe %s spawn throttle: %v", test.name, err)
			}
			if err := proveCleanMetricsTree(root, defaultSpoolWorkBudget()); err != nil {
				t.Fatalf("clean proof after %s spawn-throttle purge: %v", test.name, err)
			}
		})
	}
}

func TestPurgeSpawnThrottlePreservesUnsafeFilesystemShapes(t *testing.T) {
	for _, shape := range []string{"symlink", "cross-device"} {
		t.Run(shape, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, disabledState(7, 2, cleanupDisable))
			plainRoot := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(plainRoot, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}

			path := filepath.Join(home.Root(), spawnThrottleFileName)
			const sentinel = "outside spawn-throttle authority"
			var target string
			if shape == "symlink" {
				target = filepath.Join(t.TempDir(), "outside-sentinel")
				if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(sentinel), 0o600); err != nil {
				t.Fatal(err)
			}

			mutations := 0
			root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
				metadata: func(observed string, metadata storageMetadata) storageMetadata {
					if shape == "cross-device" && observed == path {
						metadata.dev ^= 1 << 63
					}
					return metadata
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
			result, purgeErr := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
			if purgeErr == nil || result.complete || mutations != 0 {
				t.Fatalf("purge unsafe %s spawn throttle = result:%+v mutations:%d err:%v",
					shape, result, mutations, purgeErr)
			}
			if shape == "cross-device" && !errors.Is(purgeErr, syscall.EXDEV) {
				t.Fatalf("cross-device spawn-throttle purge error = %v, want EXDEV", purgeErr)
			}
			if shape == "symlink" {
				if info, err := os.Lstat(path); err != nil || info.Mode()&fs.ModeSymlink == 0 {
					t.Fatalf("unsafe spawn-throttle symlink changed: info=%v err=%v", info, err)
				}
				if data, err := os.ReadFile(target); err != nil || string(data) != sentinel {
					t.Fatalf("spawn-throttle purge followed symlink: data=%q err=%v", data, err)
				}
			} else if data, err := os.ReadFile(path); err != nil || string(data) != sentinel {
				t.Fatalf("cross-device spawn throttle changed: data=%q err=%v", data, err)
			}
		})
	}
}

func TestRootTempJournalMalformedCanonicalMarkerIsNonAuthorizing(t *testing.T) {
	home := newMetricsTestHome(t)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	backend, ok := root.backend.(*unixStorageDirectory)
	if !ok {
		t.Fatal("root-temp journal test requires Unix storage")
	}
	journal, err := backend.openRootTempJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}

	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa91))
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	if err := os.WriteFile(markerPath, []byte("malformed non-empty marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(home.Root(), name)
	if err := os.WriteFile(tempPath, []byte("mapped root temp"), 0o600); err != nil {
		t.Fatal(err)
	}

	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if !errors.Is(state.operation, errUnsettledRootTempJournal) || state.mutated {
		t.Fatalf("malformed canonical marker authorized mutation: mutated=%v err=%v", state.mutated, state.operation)
	}
	if data, err := os.ReadFile(tempPath); err != nil || string(data) != "mapped root temp" {
		t.Fatalf("malformed canonical marker changed mapped temp: data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(markerPath); err != nil || string(data) != "malformed non-empty marker" {
		t.Fatalf("malformed canonical marker changed: data=%q err=%v", data, err)
	}
}

func TestRootTempJournalCrossDeviceMarkerEvidenceCannotAuthorizeMappedTempMutation(t *testing.T) {
	home := newMetricsTestHome(t)
	plainRoot := mustOpenMutableRoot(t, home)
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa93))
	writeJournaledRootTempCrashFixture(t, plainRoot, name, []byte("mapped root temp"), 0)
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	tempPath := filepath.Join(home.Root(), name)
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	tempMutations := 0
	markerOpens := 0
	markerReads := 0
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		metadata: func(path string, metadata storageMetadata) storageMetadata {
			if path == markerPath {
				metadata.dev ^= 1 << 63
			}
			return metadata
		},
		beforeMutation: func(_ storageStep, path string) {
			if path == tempPath {
				tempMutations++
			}
		},
		afterFileOpen: func(path string) {
			if path == markerPath {
				markerOpens++
			}
		},
		beforeRead: func(path string) {
			if path == markerPath {
				markerReads++
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if !errors.Is(state.operation, errUnsettledRootTempJournal) || state.mutated || tempMutations != 0 ||
		markerOpens != 0 || markerReads != 0 {
		t.Fatalf("cross-device marker authorized work: mutated=%v temp-mutations=%d opens=%d reads=%d err=%v",
			state.mutated, tempMutations, markerOpens, markerReads, state.operation)
	}
	if data, err := os.ReadFile(tempPath); err != nil || string(data) != "mapped root temp" {
		t.Fatalf("cross-device marker changed mapped temp: data=%q err=%v", data, err)
	}
	if _, err := os.Lstat(markerPath); err != nil {
		t.Fatalf("cross-device marker evidence was removed: %v", err)
	}
}

func TestRootTempJournalCrossDeviceMappedTempIsRejectedBeforeOpenOrMutation(t *testing.T) {
	home := newMetricsTestHome(t)
	plainRoot := mustOpenMutableRoot(t, home)
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa98))
	writeJournaledRootTempCrashFixture(t, plainRoot, name, []byte("bound temp"), 0)
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(home.Root(), name)
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	tempOpens := 0
	tempReads := 0
	tempMutations := 0
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		metadata: func(path string, metadata storageMetadata) storageMetadata {
			if path == tempPath {
				metadata.dev ^= 1 << 63
			}
			return metadata
		},
		afterFileOpen: func(path string) {
			if path == tempPath {
				tempOpens++
			}
		},
		beforeRead: func(path string) {
			if path == tempPath {
				tempReads++
			}
		},
		beforeMutation: func(_ storageStep, path string) {
			if path == tempPath {
				tempMutations++
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if !errors.Is(state.operation, errUnsettledRootTempJournal) || state.mutated ||
		tempOpens != 0 || tempReads != 0 || tempMutations != 0 {
		t.Fatalf("cross-device mapped temp work = mutated:%v opens:%d reads:%d mutations:%d err:%v",
			state.mutated, tempOpens, tempReads, tempMutations, state.operation)
	}
	if data, err := os.ReadFile(tempPath); err != nil || string(data) != "bound temp" {
		t.Fatalf("cross-device mapped temp changed: data=%q err=%v", data, err)
	}
	if _, err := os.Lstat(markerPath); err != nil {
		t.Fatalf("cross-device mapped temp marker removed: %v", err)
	}
}

func TestRootTempJournalIntentWithMappedTempRemainsPendingWithoutMutation(t *testing.T) {
	home := newMetricsTestHome(t)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	backend, ok := root.backend.(*unixStorageDirectory)
	if !ok {
		t.Fatal("root-temp journal test requires Unix storage")
	}
	journal, err := backend.openRootTempJournal()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa94))
	marker, err := createRootTempJournalMarker(backend, journal, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := marker.close(); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(home.Root(), name)
	if err := os.WriteFile(tempPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if !errors.Is(state.operation, errUnsettledRootTempJournal) || state.mutated {
		t.Fatalf("intent marker authorized mapped-temp mutation: mutated=%v err=%v", state.mutated, state.operation)
	}
	for _, path := range []string{tempPath, markerPath} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("intent replay removed %q: %v", path, err)
		}
	}
}

func TestRootTempJournalBoundIdentityMismatchPreservesBothFiles(t *testing.T) {
	home := newMetricsTestHome(t)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa95))
	writeJournaledRootTempCrashFixture(t, root, name, []byte("original bound temp"), 0)
	tempPath := filepath.Join(home.Root(), name)
	displacedPath := tempPath + ".displaced"
	if err := os.Rename(tempPath, displacedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempPath, []byte("replacement temp"), 0o600); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if !errors.Is(state.operation, errUnsettledRootTempJournal) || state.mutated {
		t.Fatalf("mismatched binding authorized mutation: mutated=%v err=%v", state.mutated, state.operation)
	}
	for path, want := range map[string]string{
		tempPath:      "replacement temp",
		displacedPath: "original bound temp",
	} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Fatalf("mismatched binding changed %q: data=%q err=%v", path, data, err)
		}
	}
	if _, err := os.Lstat(markerPath); err != nil {
		t.Fatalf("mismatched binding removed marker: %v", err)
	}
}

func TestRootTempJournalRevalidatesMarkerAfterDeleteHookBeforeTempUnlink(t *testing.T) {
	home := newMetricsTestHome(t)
	plainRoot := mustOpenMutableRoot(t, home)
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa97))
	writeJournaledRootTempCrashFixture(t, plainRoot, name, []byte("bound temp"), 0)
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(home.Root(), name)
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	displacedMarker := markerPath + ".displaced"
	swapped := false
	var swapErr error
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeMutation: func(step storageStep, path string) {
			if swapped || step != storageStepDelete || path != tempPath {
				return
			}
			swapped = true
			if err := os.Rename(markerPath, displacedMarker); err != nil {
				swapErr = err
				return
			}
			swapErr = os.WriteFile(markerPath, nil, 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if swapErr != nil || !swapped {
		t.Fatalf("swap marker before temp unlink: swapped=%v err=%v", swapped, swapErr)
	}
	if !errors.Is(state.operation, errUnsettledRootTempJournal) || state.mutated {
		t.Fatalf("post-hook marker replacement authorized temp unlink: mutated=%v err=%v", state.mutated, state.operation)
	}
	if data, err := os.ReadFile(tempPath); err != nil || string(data) != "bound temp" {
		t.Fatalf("post-hook marker replacement changed temp: data=%q err=%v", data, err)
	}
	for _, path := range []string{markerPath, displacedMarker} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("post-hook marker evidence %q was removed: %v", path, err)
		}
	}
}

func TestRootTempJournalFinalTempIdentityCheckFollowsMarkerGuardRead(t *testing.T) {
	home := newMetricsTestHome(t)
	plainRoot := mustOpenMutableRoot(t, home)
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa9a))
	writeJournaledRootTempCrashFixture(t, plainRoot, name, []byte("bound temp"), 0)
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(home.Root(), name)
	displacedTemp := tempPath + ".displaced"
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	armed := false
	swapped := false
	var swapErr error
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeMutation: func(step storageStep, path string) {
			if step == storageStepDelete && path == tempPath {
				armed = true
			}
		},
		beforeRead: func(path string) {
			if !armed || swapped || path != markerPath {
				return
			}
			swapped = true
			if err := os.Rename(tempPath, displacedTemp); err != nil {
				swapErr = err
				return
			}
			swapErr = os.WriteFile(tempPath, []byte("replacement temp"), 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if swapErr != nil || !swapped {
		t.Fatalf("swap temp during marker guard read: swapped=%v err=%v", swapped, swapErr)
	}
	if state.mutated || state.operation == nil {
		t.Fatalf("guard-read temp swap authorized unlink: mutated=%v err=%v", state.mutated, state.operation)
	}
	for path, want := range map[string]string{tempPath: "replacement temp", displacedTemp: "bound temp"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Fatalf("guard-read temp swap changed %q: data=%q err=%v", path, data, err)
		}
	}
}

func TestRootTempJournalMarkerRetirementRevalidatesContentAfterDeleteHook(t *testing.T) {
	home := newMetricsTestHome(t)
	plainRoot := mustOpenMutableRoot(t, home)
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa9b))
	writeJournaledRootTempCrashFixture(t, plainRoot, name, nil, 0)
	tempPath := filepath.Join(home.Root(), name)
	if err := os.Remove(tempPath); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.syncDirectory(); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	truncated := false
	var truncateErr error
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeMutation: func(step storageStep, path string) {
			if truncated || step != storageStepDelete || path != markerPath {
				return
			}
			truncated = true
			truncateErr = os.Truncate(markerPath, 0)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if truncateErr != nil || !truncated {
		t.Fatalf("truncate marker at retirement: truncated=%v err=%v", truncated, truncateErr)
	}
	if state.mutated || state.operation == nil {
		t.Fatalf("truncated marker was retired: mutated=%v err=%v", state.mutated, state.operation)
	}
	if data, err := os.ReadFile(markerPath); err != nil || len(data) != 0 {
		t.Fatalf("truncated marker was not preserved: data=%x err=%v", data, err)
	}
}

func TestRootTempJournalMarkerRetirementRequiresDurableTempAbsenceAfterHook(t *testing.T) {
	home := newMetricsTestHome(t)
	plainRoot := mustOpenMutableRoot(t, home)
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa9c))
	writeJournaledRootTempCrashFixture(t, plainRoot, name, nil, 0)
	tempPath := filepath.Join(home.Root(), name)
	if err := os.Remove(tempPath); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.syncDirectory(); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	created := false
	var createErr error
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeMutation: func(step storageStep, path string) {
			if created || step != storageStepDelete || path != markerPath {
				return
			}
			created = true
			createErr = os.WriteFile(tempPath, []byte("late temp"), 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if createErr != nil || !created {
		t.Fatalf("create temp at marker retirement: created=%v err=%v", created, createErr)
	}
	if state.mutated || state.operation == nil {
		t.Fatalf("marker retired over late temp: mutated=%v err=%v", state.mutated, state.operation)
	}
	if data, err := os.ReadFile(tempPath); err != nil || string(data) != "late temp" {
		t.Fatalf("late temp changed: data=%q err=%v", data, err)
	}
	if _, err := os.Lstat(markerPath); err != nil {
		t.Fatalf("marker removed over late temp: %v", err)
	}
}

func TestRootTempJournalFixedProofAcceptsExactlyMaximumMarkers(t *testing.T) {
	for _, count := range []int{maximumStorageTempAttempts, maximumStorageTempAttempts + 1} {
		t.Run(fmt.Sprintf("markers-%d", count), func(t *testing.T) {
			home := newMetricsTestHome(t)
			root := mustOpenMutableRoot(t, home)
			defer func() { _ = root.Close() }()
			backend, ok := root.backend.(*unixStorageDirectory)
			if !ok {
				t.Fatal("root-temp journal test requires Unix storage")
			}
			journal, err := backend.openRootTempJournal()
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.close(); err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(home.Root(), rootTempJournalDirectoryName)
			var journalStat unix.Stat_t
			if err := unix.Stat(journalPath, &journalStat); err != nil {
				t.Fatal(err)
			}
			device := unixStatDevice(journalStat)
			for index := 0; index < count; index++ {
				name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xb00+index))
				data, err := encodeBoundRootTempJournalMarker(name, recordIncarnation{dev: device, ino: uint64(index + 1)})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(journalPath, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			meter := newSpoolWorkMeter(defaultSpoolWorkBudget())
			meter.physicalDirectories = true
			proofErr := proveRootTempJournalReadOnlyWithMeter(root, meter, true)
			if count == maximumStorageTempAttempts {
				if proofErr != nil || meter.exhausted || !meter.rootTempJournalSentinel {
					t.Fatalf("exact marker bound was rejected: exhausted=%v sentinel=%v err=%v",
						meter.exhausted, meter.rootTempJournalSentinel, proofErr)
				}
				return
			}
			if !errors.Is(proofErr, errUnsettledRootTempJournal) || !meter.exhausted {
				t.Fatalf("overflow marker was accepted: exhausted=%v err=%v", meter.exhausted, proofErr)
			}
		})
	}
}

func TestPurgeSpoolCapsAggregateRootTempMarkerRetirementAtMaximum(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, disabledState(7, 2, cleanupDisable))
	root := mustOpenMutableRoot(t, home)
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
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(home.Root(), rootTempJournalDirectoryName)
	var journalStat unix.Stat_t
	if err := unix.Stat(journalPath, &journalStat); err != nil {
		t.Fatal(err)
	}
	device := unixStatDevice(journalStat)
	for index := 0; index < maximumStorageTempAttempts+1; index++ {
		name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xc00+index))
		data, err := encodeBoundRootTempJournalMarker(name, recordIncarnation{dev: device, ino: uint64(index + 1)})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(journalPath, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	budget := spoolWorkBudget{
		maxEntries: 1_000_000, maxDirectories: 100_000,
		maxReadBytes: 100_000_000, maxNameBytes: 100_000_000,
	}
	result, purgeErr := purgeSpoolWithinBudget(root, budget)
	if result.complete {
		t.Fatalf("aggregate purge drained more than %d markers: err=%v", maximumStorageTempAttempts, purgeErr)
	}
	entries, err := os.ReadDir(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("aggregate marker retirement left %d entries, want exactly one overflow sentinel", len(entries))
	}
}

func TestRootTempJournalOversizedMarkerChargesOnlyMaximumPlusOnePhysicalBytes(t *testing.T) {
	home := newMetricsTestHome(t)
	plainRoot := mustOpenMutableRoot(t, home)
	backend, ok := plainRoot.backend.(*unixStorageDirectory)
	if !ok {
		t.Fatal("root-temp journal test requires Unix storage")
	}
	journal, err := backend.openRootTempJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa96))
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	if err := os.WriteFile(markerPath, bytes.Repeat([]byte{'x'}, maximumRootTempJournalMarkerBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	grew := false
	physicalReadBytes := 0
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeRead: func(path string) {
			if grew || path != markerPath {
				return
			}
			grew = true
			file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if openErr != nil {
				t.Fatal(openErr)
			}
			_, writeErr := file.Write([]byte{'x'})
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("grow marker: write=%v close=%v", writeErr, closeErr)
			}
		},
		afterRead: func(path string, _, read int, _ error) {
			if path == markerPath {
				physicalReadBytes += read
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	meter := newSpoolWorkMeter(defaultSpoolWorkBudget())
	meter.physicalDirectories = true
	proofErr := proveRootTempJournalReadOnlyWithMeter(root, meter, false)
	if !errors.Is(proofErr, errUnsettledRootTempJournal) || !grew {
		t.Fatalf("oversized marker proof = grew:%v err:%v", grew, proofErr)
	}
	if physicalReadBytes != rootTempJournalMarkerReadLimit || meter.usage.readBytes != uint64(rootTempJournalMarkerReadLimit) {
		t.Fatalf("oversized marker read = physical:%d metered:%d want:%d",
			physicalReadBytes, meter.usage.readBytes, rootTempJournalMarkerReadLimit)
	}
	if info, err := os.Lstat(markerPath); err != nil || info.Size() != int64(rootTempJournalMarkerReadLimit) {
		t.Fatalf("oversized marker changed: info=%v err=%v", info, err)
	}
}

func TestRootTempJournalStatKnownOversizedSparseMarkerReadsNoBytes(t *testing.T) {
	home := newMetricsTestHome(t)
	plainRoot := mustOpenMutableRoot(t, home)
	backend, ok := plainRoot.backend.(*unixStorageDirectory)
	if !ok {
		t.Fatal("root-temp journal test requires Unix storage")
	}
	journal, err := backend.openRootTempJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa99))
	markerPath := filepath.Join(home.Root(), rootTempJournalDirectoryName, name)
	file, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	truncateErr := file.Truncate(8 << 30)
	closeErr := file.Close()
	if truncateErr != nil || closeErr != nil {
		t.Fatalf("create sparse marker: truncate=%v close=%v", truncateErr, closeErr)
	}
	reads := 0
	physicalReadBytes := 0
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeRead: func(path string) {
			if path == markerPath {
				reads++
			}
		},
		afterRead: func(path string, _, read int, _ error) {
			if path == markerPath {
				physicalReadBytes += read
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	meter := newSpoolWorkMeter(defaultSpoolWorkBudget())
	meter.physicalDirectories = true
	proofErr := proveRootTempJournalReadOnlyWithMeter(root, meter, false)
	if !errors.Is(proofErr, errUnsettledRootTempJournal) || reads != 0 || physicalReadBytes != 0 {
		t.Fatalf("sparse oversized marker proof = reads:%d bytes:%d err:%v", reads, physicalReadBytes, proofErr)
	}
	if info, err := os.Lstat(markerPath); err != nil || info.Size() != 8<<30 {
		t.Fatalf("sparse marker changed: info=%v err=%v", info, err)
	}
}

func TestRootTempJournalDoesNotCertifyReplacedNamedDirectory(t *testing.T) {
	home := newMetricsTestHome(t)
	journalPath := filepath.Join(home.Root(), rootTempJournalDirectoryName)
	replacedPath := filepath.Join(home.Root(), ".replaced-root-temp-journal")
	replacementMarker := filepath.Join(journalPath, fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa92)))
	swapped := false
	var swapErr error
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeStep: func(step storageStep) error {
			if step != storageStepEnumerate || swapped {
				return nil
			}
			swapped = true
			if err := os.Rename(journalPath, replacedPath); err != nil {
				swapErr = err
				return nil
			}
			if err := os.Mkdir(journalPath, 0o700); err != nil {
				swapErr = err
				return nil
			}
			swapErr = os.WriteFile(replacementMarker, nil, 0o600)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	backend, ok := root.backend.(*unixStorageDirectory)
	if !ok {
		t.Fatal("root-temp journal test requires Unix storage")
	}
	journal, err := backend.openRootTempJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}

	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if swapErr != nil {
		t.Fatalf("replace named journal fixture: %v", swapErr)
	}
	if !swapped {
		t.Fatal("journal path replacement was not injected")
	}
	if state.journalSettled {
		t.Fatal("unlinked old journal descriptor certified the replacement named journal")
	}
	if _, err := os.Lstat(replacementMarker); err != nil {
		t.Fatalf("replacement journal marker was mutated through stale authority: %v", err)
	}
}

func TestPurgeExactRootCleanupDeletesOnlyCanonicalAtomicTempRegularFiles(t *testing.T) {
	home := newMetricsTestHome(t)
	plainRoot := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(plainRoot, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	canonicalRegular := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xabc)))
	canonicalSparse := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xabd)))
	fixtureRoot := mustOpenMutableRoot(t, home)
	writeJournaledRootTempCrashFixture(t, fixtureRoot, filepath.Base(canonicalRegular),
		[]byte("installation_id = \""+testInstallationID+"\"\n"), 0)
	writeJournaledRootTempCrashFixture(t, fixtureRoot, filepath.Base(canonicalSparse), nil, 8<<30)
	if err := fixtureRoot.Close(); err != nil {
		t.Fatal(err)
	}

	invalidRegulars := []string{
		"notes.txt",
		".pm-tmp-crashed-config",
		".pm-tmp-01-2",
		".pm-tmp-1-02",
		".pm-tmp-1-A",
		".pm-tmp-0-1",
		".pm-tmp-1-0",
		".pm-tmp-1-2-extra",
		".pm-tmp-10000000000000000-1",
	}
	for _, name := range invalidRegulars {
		if err := os.WriteFile(filepath.Join(home.Root(), name), []byte("user-owned residue"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	canonicalLater := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xac1)))
	fixtureRoot = mustOpenMutableRoot(t, home)
	writeJournaledRootTempCrashFixture(t, fixtureRoot, filepath.Base(canonicalLater), []byte("later identity temp"), 0)
	if err := fixtureRoot.Close(); err != nil {
		t.Fatal(err)
	}
	laxTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xac2)))
	if err := os.WriteFile(laxTemp, []byte("lax user file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(laxTemp, 0o644); err != nil {
		t.Fatal(err)
	}
	linkedTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xac3)))
	linkedAlias := filepath.Join(home.Root(), "hard-link-alias")
	if err := os.WriteFile(linkedTemp, []byte("linked user file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(linkedTemp, linkedAlias); err != nil {
		t.Fatal(err)
	}
	wrongOwnerTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xac4)))
	if err := os.WriteFile(wrongOwnerTemp, []byte("wrong-owner user file"), 0o600); err != nil {
		t.Fatal(err)
	}

	directoryTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xabe)))
	if err := os.MkdirAll(filepath.Join(directoryTemp, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	nestedSparse := filepath.Join(directoryTemp, "nested", "poison")
	if err := os.WriteFile(nestedSparse, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(nestedSparse, 8<<30); err != nil {
		t.Fatal(err)
	}
	symlinkTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xabf)))
	symlinkTarget := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(symlinkTarget, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(symlinkTarget, symlinkTemp); err != nil {
		t.Fatal(err)
	}
	fifoTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xac0)))
	if err := unix.Mkfifo(fifoTemp, 0o600); err != nil {
		t.Fatal(err)
	}

	var tempReadBytes, poisonDirectoryOpens, nestedMetadataAttempts int
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		metadata: func(path string, metadata storageMetadata) storageMetadata {
			if path == wrongOwnerTemp {
				metadata.uid++
			}
			return metadata
		},
		beforeDirectoryOpen: func(path string) error {
			if path == directoryTemp || strings.HasPrefix(path, directoryTemp+string(os.PathSeparator)) {
				poisonDirectoryOpens++
			}
			return nil
		},
		beforeMetadataAttempt: func(path string) error {
			if strings.HasPrefix(path, directoryTemp+string(os.PathSeparator)) {
				nestedMetadataAttempts++
			}
			return nil
		},
		afterRead: func(path string, _ int, read int, _ error) {
			if path == canonicalRegular || path == canonicalSparse || path == canonicalLater ||
				path == laxTemp || path == linkedTemp || path == wrongOwnerTemp ||
				strings.HasPrefix(path, directoryTemp+string(os.PathSeparator)) {
				tempReadBytes += read
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	budget := defaultSpoolWorkBudget()
	result, err := purgeSpoolWithinBudget(root, budget)
	if err == nil || result.complete {
		t.Fatalf("unrecognized root residue was certified clean: result=%+v err=%v", result, err)
	}
	if result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories ||
		result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes {
		t.Fatalf("root-temp purge exceeded shared budget: result=%+v budget=%+v", result.usage, budget)
	}
	if tempReadBytes != 0 {
		t.Fatalf("root-temp purge physically read %d poison bytes", tempReadBytes)
	}
	if poisonDirectoryOpens != 0 || nestedMetadataAttempts != 0 {
		t.Fatalf("root-temp purge descended into preserved directory: opens=%d nested-metadata=%d",
			poisonDirectoryOpens, nestedMetadataAttempts)
	}
	for _, path := range []string{canonicalRegular, canonicalSparse, canonicalLater} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("canonical regular root temp %q remains: %v", filepath.Base(path), err)
		}
	}
	for _, name := range invalidRegulars {
		if data, err := os.ReadFile(filepath.Join(home.Root(), name)); err != nil || string(data) != "user-owned residue" {
			t.Fatalf("unrecognized root file %q changed: data=%q err=%v", name, data, err)
		}
	}
	for path, want := range map[string]string{
		laxTemp:        "lax user file",
		linkedTemp:     "linked user file",
		linkedAlias:    "linked user file",
		wrongOwnerTemp: "wrong-owner user file",
	} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Fatalf("metadata-invalid root file %q changed: data=%q err=%v", filepath.Base(path), data, err)
		}
	}
	for _, path := range []string{directoryTemp, nestedSparse, symlinkTemp, fifoTemp} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("preserved root entry %q is missing: %v", filepath.Base(path), err)
		}
	}
	if data, err := os.ReadFile(symlinkTarget); err != nil || string(data) != "outside" {
		t.Fatalf("root-temp purge followed symlink: data=%q err=%v", data, err)
	}
}

func TestExactRootTempUnlinkSyncFailureRequiresLaterRootSync(t *testing.T) {
	home := newMetricsTestHome(t)
	canonicalTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xac5)))
	injected := errors.New("injected root sync failure after canonical temp unlink")
	unlinkStarted := false
	failedSync := false
	awaitingRecovery := false
	recoverySyncs := 0
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeMutation: func(step storageStep, path string) {
			if (step == storageStepDelete || step == storageStepUnlink) && path == canonicalTemp {
				unlinkStarted = true
			}
		},
		beforeStep: func(step storageStep) error {
			if unlinkStarted && !failedSync && step == storageStepDirectorySync {
				failedSync = true
				awaitingRecovery = true
				return injected
			}
			if awaitingRecovery && step == storageStepDirectorySync {
				recoverySyncs++
				awaitingRecovery = false
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	writeJournaledRootTempCrashFixture(t, root, filepath.Base(canonicalTemp), []byte("identity"), 0)
	first := &spoolSweepState{
		root: root, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()), failClosedArmed: true,
	}
	first.cleanupRootTempJournal()
	if !failedSync || !errors.Is(first.operation, injected) {
		t.Fatalf("canonical temp unlink sync failure = failed:%v err:%v", failedSync, first.operation)
	}
	if _, err := os.Lstat(canonicalTemp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("applied canonical temp unlink remains: %v", err)
	}

	second := &spoolSweepState{
		root: root, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()), failClosedArmed: true,
	}
	second.cleanupRootTempJournal()
	if second.operation != nil || recoverySyncs == 0 || awaitingRecovery {
		t.Fatalf("canonical temp absence replay = recovery-syncs:%d awaiting:%v err:%v",
			recoverySyncs, awaitingRecovery, second.operation)
	}
}

func TestPurgeSpoolPreservesCrossDeviceDescendantsAtEveryDepth(t *testing.T) {
	for _, boundary := range []string{
		"queue", "inflight", "generation", "nested", "active-control", "retired-control",
	} {
		t.Run(boundary, func(t *testing.T) {
			home := newMetricsTestHome(t)
			plainRoot := mustOpenMutableRoot(t, home)
			priorQuota := spoolQuota{Events: 1, Bytes: 4}
			if err := persistSpoolQuota(plainRoot, priorQuota); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}
			generationPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
			boundaryPath := filepath.Join(generationPath, "mounted")
			switch boundary {
			case "queue":
				boundaryPath = filepath.Join(home.Root(), queueDirectoryName)
			case "inflight":
				boundaryPath = filepath.Join(home.Root(), inflightDirectoryName)
			case "generation":
				boundaryPath = generationPath
			case "active-control":
				boundaryPath = filepath.Join(home.Root(), spoolControlDirectoryName)
			case "retired-control":
				boundaryPath = filepath.Join(home.Root(), retiredControlDirectoryName)
			}
			sentinelPath := filepath.Join(boundaryPath, "inside", "keep")
			if err := os.MkdirAll(filepath.Dir(sentinelPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sentinelPath, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			boundaryOpens := 0
			boundaryMutations := 0
			boundaryReadBytes := 0
			root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
				metadata: func(path string, metadata storageMetadata) storageMetadata {
					if path == boundaryPath {
						metadata.dev ^= 1 << 63
					}
					return metadata
				},
				beforeDirectoryOpen: func(path string) error {
					if path == boundaryPath || strings.HasPrefix(path, boundaryPath+string(os.PathSeparator)) {
						boundaryOpens++
					}
					return nil
				},
				beforeMutation: func(_ storageStep, path string) {
					if path == boundaryPath || strings.HasPrefix(path, boundaryPath+string(os.PathSeparator)) {
						boundaryMutations++
					}
				},
				beforeExchange: func() error {
					boundaryMutations++
					return nil
				},
				afterRead: func(path string, _ int, read int, _ error) {
					if path == boundaryPath || strings.HasPrefix(path, boundaryPath+string(os.PathSeparator)) {
						boundaryReadBytes += read
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			result, purgeErr := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
			if !errors.Is(purgeErr, syscall.EXDEV) || result.complete ||
				result.removedEvents != 0 || result.removedBytes != 0 {
				t.Fatalf("cross-device %s purge = result:%+v err:%v", boundary, result, purgeErr)
			}
			if boundaryOpens != 0 || boundaryMutations != 0 || boundaryReadBytes != 0 {
				t.Fatalf("cross-device %s activity = opens:%d mutations:%d read-bytes:%d",
					boundary, boundaryOpens, boundaryMutations, boundaryReadBytes)
			}
			if data, err := os.ReadFile(sentinelPath); err != nil || string(data) != "keep" {
				t.Fatalf("cross-device %s sentinel changed: data=%q err=%v", boundary, data, err)
			}
			requireIncompletePurgeFailClosedEvidence(t, root, priorQuota)
		})
	}
}

func TestPurgeExactRootProofReservesDirectoryTraversalEnvelope(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	result := spoolSweepResult{}
	for attempts := 0; attempts < 8 && !result.complete; attempts++ {
		var err error
		result, err = purgeSpool(root, defaultSpoolWorkBudget())
		if err != nil {
			t.Fatal(err)
		}
	}
	if !result.complete {
		t.Fatal("fixture did not reach an exact clean-root proof")
	}

	budget := defaultSpoolWorkBudget()
	budget.maxDirectories = spoolFixedDirectoryReserve + 1
	result, err := purgeSpool(root, budget)
	if err != nil {
		t.Fatalf("directory-envelope exhaustion became an operation error: %v", err)
	}
	if result.complete {
		t.Fatal("exact-root proof ignored the directory traversal envelope")
	}
	if result.usage.directories > budget.maxDirectories {
		t.Fatalf("exact-root proof used %d directories, budget %d", result.usage.directories, budget.maxDirectories)
	}
}

func TestPurgeLaxTopLevelSpoolTreesUsesOneMeterAndRequiresMutationFreeZeroProof(t *testing.T) {
	for _, treeName := range []string{queueDirectoryName, inflightDirectoryName} {
		for _, shape := range []string{"empty", "deep"} {
			t.Run(treeName+"/"+shape, func(t *testing.T) {
				home, _, _ := newRecordServiceFixture(t, testEventIDThree)
				plainRoot := mustOpenMutableRoot(t, home)
				quota := spoolQuota{Events: 1, Bytes: 1}
				if err := persistSpoolQuota(plainRoot, quota); err != nil {
					t.Fatal(err)
				}
				if err := plainRoot.Close(); err != nil {
					t.Fatal(err)
				}

				treePath := filepath.Join(home.Root(), treeName)
				if err := os.Mkdir(treePath, 0o700); err != nil {
					t.Fatal(err)
				}
				if shape == "deep" {
					path := treePath
					for depth := 0; depth < 9; depth++ {
						path = filepath.Join(path, fmt.Sprintf("d%02d", depth))
						if err := os.Mkdir(path, 0o700); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(filepath.Join(path, "payload"), []byte("x"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Chmod(treePath, 0o755); err != nil {
					t.Fatal(err)
				}

				namespaceMutations := 0
				hooks := storageTestHooks{beforeStep: func(step storageStep) error {
					if step == storageStepRename || step == storageStepUnlink || step == storageStepRmdir {
						namespaceMutations++
					}
					return nil
				}}
				root, err := openStorageRootMutableWithHooks(home, hooks)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()
				budget := spoolWorkBudget{
					maxEntries:     spoolFixedEntryEnvelope + 32,
					maxDirectories: spoolMinimumDirectoryProgress,
					maxReadBytes:   spoolFixedReadEnvelope + 3*maximumControlFileBytes,
					maxNameBytes:   spoolFixedNameEnvelope + 4096,
				}

				result := spoolSweepResult{}
				sawMutation := false
				for attempts := 0; attempts < 128 && !result.complete; attempts++ {
					namespaceMutations = 0
					result, err = purgeSpool(root, budget)
					if err != nil {
						t.Fatal(err)
					}
					if result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories ||
						result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes {
						t.Fatalf("lax-tree cleanup exceeded one global budget: result=%+v budget=%+v", result.usage, budget)
					}
					if namespaceMutations > 0 && !result.complete {
						sawMutation = true
						if result.complete {
							t.Fatal("lax-tree mutation pass certified an empty spool")
						}
						requireIncompletePurgeFailClosedEvidence(t, root, quota)
					}
				}
				if !sawMutation || !result.complete {
					t.Fatalf("lax-tree cleanup did not converge through a mutation-free pass: %+v", result)
				}
				if got := readQuotaFromRoot(t, root); got != (spoolQuota{}) {
					t.Fatalf("mutation-free lax-tree quota = %+v", got)
				}
				if _, err := os.Lstat(treePath); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("lax top-level tree remains: %v", err)
				}
			})
		}
	}
}

func TestPurgeMalformedSpoolControlConvergesWithoutFollowingEntries(t *testing.T) {
	for _, shape := range []string{"leaf", "symlink", "fifo", "lax-empty", "lax-deep"} {
		t.Run(shape, func(t *testing.T) {
			home, _, _ := newRecordServiceFixture(t, testEventIDThree)
			plainRoot := mustOpenMutableRoot(t, home)
			quota := spoolQuota{Events: 1, Bytes: 1}
			if err := persistSpoolQuota(plainRoot, quota); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}
			controlPath := filepath.Join(home.Root(), spoolControlDirectoryName)
			switch shape {
			case "leaf":
				if err := os.WriteFile(controlPath, []byte("malformed"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), controlPath); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(controlPath, 0o600); err != nil {
					t.Fatal(err)
				}
			case "lax-empty", "lax-deep":
				if err := os.Mkdir(controlPath, 0o700); err != nil {
					t.Fatal(err)
				}
				if shape == "lax-deep" {
					path := controlPath
					for depth := 0; depth < 9; depth++ {
						path = filepath.Join(path, fmt.Sprintf("d%02d", depth))
						if err := os.Mkdir(path, 0o700); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(filepath.Join(path, "payload"), []byte("x"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Chmod(controlPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			root := mustOpenMutableRoot(t, home)
			defer func() { _ = root.Close() }()
			budget := spoolWorkBudget{
				maxEntries: spoolFixedEntryEnvelope + 64, maxDirectories: spoolMinimumDirectoryProgress,
				maxReadBytes: spoolFixedReadEnvelope + 3*maximumControlFileBytes,
				maxNameBytes: spoolFixedNameEnvelope + 8192,
			}
			result := spoolSweepResult{}
			for attempts := 0; attempts < 128 && !result.complete; attempts++ {
				var err error
				result, err = purgeSpool(root, budget)
				if err != nil {
					t.Fatalf("purge attempt %d: %v", attempts+1, err)
				}
				if !result.complete {
					requireIncompletePurgeFailClosedEvidence(t, root, quota)
				}
			}
			if !result.complete || readQuotaFromRoot(t, root) != (spoolQuota{}) {
				t.Fatalf("malformed control did not converge: %+v", result)
			}
			for _, name := range []string{spoolControlDirectoryName, retiredControlDirectoryName} {
				if _, err := root.lookupEntry(name); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("control entry %q remains: %v", name, err)
				}
			}
		})
	}
}

func TestControlCleanupDoesNotReportUserEventRemoval(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	retired := filepath.Join(home.Root(), retiredControlDirectoryName)
	if err := os.MkdirAll(filepath.Join(retired, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"looks-like-event.json": []byte("control"),
		"nested/quota.next":     []byte("staging"),
	} {
		if err := os.WriteFile(filepath.Join(retired, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()

	result := spoolSweepResult{}
	for attempts := 0; attempts < 16 && !result.complete; attempts++ {
		var err error
		result, err = purgeSpool(root, defaultSpoolWorkBudget())
		if err != nil {
			t.Fatal(err)
		}
		if result.removedEvents != 0 || result.removedBytes != 0 {
			t.Fatalf("control cleanup reported user removals: events=%d bytes=%d", result.removedEvents, result.removedBytes)
		}
	}
	if !result.complete {
		t.Fatalf("control cleanup did not converge: %+v", result)
	}
}

func TestRetiredControlMetadataDoesNotConsumeEventCapOrRemovalCounters(t *testing.T) {
	home, _, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	eventBytes := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(eventBytes))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(home.Root(), retiredControlDirectoryName)
	if err := os.MkdirAll(filepath.Join(retired, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < int(maximumEnumerationEvents)+1; index++ {
		name := fmt.Sprintf("control-%04d", index)
		if err := os.WriteFile(filepath.Join(retired, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(retired, "looks-like-event.json"), []byte("control"), 0o600); err != nil {
		t.Fatal(err)
	}
	sparse := filepath.Join(retired, "sparse")
	if err := os.WriteFile(sparse, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(sparse, int64(maximumSpoolBytes+1)); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(retired, "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retired, "nested", "quota.next"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}

	root = mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	totalEvents := uint64(0)
	totalBytes := uint64(0)
	complete := false
	for attempts := 0; attempts < 12 && !complete; attempts++ {
		_, retiredErr := os.Lstat(retired)
		retiredPresent := retiredErr == nil
		if retiredErr != nil && !errors.Is(retiredErr, fs.ErrNotExist) {
			t.Fatal(retiredErr)
		}
		state := runSpoolSweep(root, spoolPolicy{}, time.Time{}, defaultSpoolWorkBudget(), true)
		if retiredPresent && state.meter.eventEntries != 0 {
			t.Fatalf("retired-control priority pass consumed %d event slots", state.meter.eventEntries)
		}
		result, err := state.finish()
		if err != nil {
			t.Fatal(err)
		}
		totalEvents += result.removedEvents
		totalBytes += result.removedBytes
		complete = result.complete
	}
	if !complete {
		t.Fatal("retired-control cleanup did not converge")
	}
	if totalEvents != 1 || totalBytes != uint64(len(eventBytes)) {
		t.Fatalf("reported removals = events:%d bytes:%d, want only queue event (%d bytes)", totalEvents, totalBytes, len(eventBytes))
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside" {
		t.Fatalf("control symlink target changed: data=%q err=%v", data, err)
	}
	_ = permit.Close()
}

func TestSweepDoesNotStartFixedQuotaPersistenceWithoutSharedBudget(t *testing.T) {
	encodedZero, err := encodeSpoolQuota(spoolQuota{})
	if err != nil {
		t.Fatal(err)
	}
	base := spoolWorkBudget{
		maxEntries: maximumCleanupEntries, maxDirectories: 8,
		maxReadBytes: maximumCleanupReadBytes, maxNameBytes: maximumCleanupNameBytes,
	}
	for _, test := range []struct {
		name   string
		budget spoolWorkBudget
	}{
		{name: "entries", budget: func() spoolWorkBudget {
			budget := base
			budget.maxEntries = 2
			return budget
		}()},
		{name: "names", budget: func() spoolWorkBudget {
			budget := base
			budget.maxNameBytes = uint64(len(spoolControlDirectoryName) + len(retiredControlDirectoryName))
			return budget
		}()},
		{name: "read-write bytes", budget: func() spoolWorkBudget {
			budget := base
			budget.maxReadBytes = uint64(len(encodedZero) - 1)
			return budget
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			ensureMetricsRoot(t, home)
			writes := 0
			root, err := openStorageRootMutableWithHooks(home, storageTestHooks{beforeStep: func(step storageStep) error {
				if step == storageStepWrite {
					writes++
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			result, sweepErr := purgeSpool(root, test.budget)
			if writes != 0 {
				t.Fatalf("insufficient %s budget started %d real quota writes: result=%+v err=%v", test.name, writes, result, sweepErr)
			}
			if result.complete {
				t.Fatalf("insufficient %s budget certified completion: result=%+v err=%v", test.name, result, sweepErr)
			}
		})
	}
}

func TestFixedQuotaEnvelopeCoversSixtyThreeTempCollisionsAtSharedCaps(t *testing.T) {
	home := newMetricsTestHome(t)
	ensureMetricsRoot(t, home)
	controlPath := filepath.Join(home.Root(), spoolControlDirectoryName)
	if err := os.Mkdir(controlPath, 0o700); err != nil {
		t.Fatal(err)
	}
	firstSequence := storageTempSequence.Load() + 1
	for offset := uint64(0); offset < maximumStorageTempAttempts-1; offset++ {
		name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), firstSequence+offset)
		if err := os.WriteFile(filepath.Join(controlPath, name), []byte("collision"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tempAttempts := 0
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeTempFileCreate: func(string) { tempAttempts++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	budget := defaultSpoolWorkBudget()
	meter := newSpoolWorkMeter(budget)
	meter.physicalDirectories = true
	meter.usage = spoolWorkUsage{
		entries:   budget.maxEntries - spoolFixedEntryEnvelope,
		readBytes: budget.maxReadBytes - spoolFixedReadEnvelope,
		nameBytes: budget.maxNameBytes - spoolFixedNameEnvelope,
	}
	state := &spoolSweepState{
		root: root, purgeAll: true, traversed: true, meter: meter,
		restoreDirectoryOpenHooks: root.installDirectoryOpenHooks(
			meter.beforePhysicalDirectoryOpen, meter.afterPhysicalDirectoryOpen,
		),
	}
	result, err := state.finish()
	if err != nil || !result.complete {
		t.Fatalf("fixed-envelope finish = complete:%v err:%v usage:%+v", result.complete, err, result.usage)
	}
	if tempAttempts != maximumStorageTempAttempts {
		t.Fatalf("temporary-file attempts = %d, want %d after 63 collisions", tempAttempts, maximumStorageTempAttempts)
	}
	if result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories ||
		result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes {
		t.Fatalf("fixed persistence exceeded shared caps: usage=%+v budget=%+v", result.usage, budget)
	}
}

func TestSpoolSweepCapsSuccessfulPhysicalDirectoryOpenatCalls(t *testing.T) {
	const helperModeEnv = "GC_PM_DIRECTORY_OPEN_HELPER"
	if mode := os.Getenv(helperModeEnv); mode != "" {
		home, err := gchome.InspectProductUsageHome(gchome.ResolveReadOnly())
		if err != nil {
			t.Fatal(err)
		}
		root, err := openStorageRootMutable(home)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "sweep" {
			_, _ = purgeSpool(root, defaultSpoolWorkBudget())
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("strace physical-open accounting is Linux-only")
	}
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Skip("strace is unavailable")
	}

	home := newMetricsTestHome(t)
	ensureMetricsRoot(t, home)
	path := filepath.Join(home.Root(), queueDirectoryName)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for depth := 0; depth < 300; depth++ {
		path = filepath.Join(path, fmt.Sprintf("d%03d", depth))
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	traceCount := func(mode string) int {
		t.Helper()
		trace := filepath.Join(t.TempDir(), mode+".trace")
		command := exec.Command(strace, "-qq", "-f", "-e", "trace=openat", "-o", trace,
			os.Args[0], "-test.run=^TestSpoolSweepCapsSuccessfulPhysicalDirectoryOpenatCalls$", "-test.count=1")
		command.Env = make([]string, 0, len(os.Environ())+2)
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "GC_HOME=") && !strings.HasPrefix(value, helperModeEnv+"=") {
				command.Env = append(command.Env, value)
			}
		}
		command.Env = append(command.Env, helperModeEnv+"="+mode, "GC_HOME="+home.Home().Path(), "GC_TESTENV_PASSTHROUGH=GC_HOME")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("trace %s helper: %v\n%s", mode, err, output)
		}
		data, err := os.ReadFile(trace)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "openat(") && strings.Contains(line, "O_DIRECTORY") && !strings.Contains(line, "= -1") {
				count++
			}
		}
		return count
	}

	baseline := traceCount("baseline")
	sweep := traceCount("sweep")
	physicalSweepOpens := sweep - baseline
	if physicalSweepOpens < 0 || physicalSweepOpens > int(maximumCleanupDirectories) {
		t.Fatalf("successful post-root O_DIRECTORY openat calls = %d (sweep=%d baseline=%d), want <= %d",
			physicalSweepOpens, sweep, baseline, maximumCleanupDirectories)
	}
}

func TestSpoolSweepMakesProgressWithOneOrTwoOrdinaryDirectorySlotsLeft(t *testing.T) {
	for _, remaining := range []uint64{1, 2} {
		t.Run(fmt.Sprintf("remaining-%d", remaining), func(t *testing.T) {
			home, _, _ := newRecordServiceFixture(t, testEventIDThree)
			path := filepath.Join(home.Root(), queueDirectoryName, "generation", "child")
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "payload"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			physicalOpens := 0
			mutations := 0
			root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
				afterDirectoryOpen: func(string) { physicalOpens++ },
				beforeStep: func(step storageStep) error {
					if step == storageStepRename || step == storageStepUnlink || step == storageStepRmdir {
						mutations++
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			physicalOpens = 0
			budget := defaultSpoolWorkBudget()
			// Journal and top-level tree traversals consume two opens each. Leave
			// exactly one or two ordinary slots before the first child descent.
			budget.maxDirectories = spoolFixedDirectoryReserve + 2*spoolTraversalDirectoryEnvelope + remaining
			result, err := purgeSpool(root, budget)
			if err != nil {
				t.Fatal(err)
			}
			if result.complete || mutations == 0 {
				t.Fatalf("boundary sweep made no bounded namespace progress: result=%+v mutations=%d", result, mutations)
			}
			if uint64(physicalOpens) > budget.maxDirectories {
				t.Fatalf("physical opens = %d, budget=%d", physicalOpens, budget.maxDirectories)
			}
		})
	}
}

func TestPurgeMutationPassCannotCertifyEmptyAndMutationFreePassConverges(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	quota := spoolQuota{}
	for index := 0; index < 96; index++ {
		tree := queueDirectoryName
		if index%2 != 0 {
			tree = inflightDirectoryName
		}
		generation := fmt.Sprintf("%08x-0000-4000-8000-%012x", index%8+4000, index%8+4000)
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+5000, index+5000)
		data := writeSpoolEventFixture(t, root, tree, generation,
			testSpoolEvent(id, "1.0.0", testRecordHour, CommandHelp))
		quota.Events++
		quota.Bytes += uint64(len(data))
		if index%12 == 0 {
			nested := filepath.Join(home.Root(), tree, generation, fmt.Sprintf("nested-%03d", index))
			if err := os.Mkdir(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(nested, "poison"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := persistSpoolQuota(root, quota); err != nil {
		t.Fatal(err)
	}

	result, err := purgeSpool(root, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if result.complete {
		t.Fatal("tree-mutating purge pass certified an empty spool")
	}
	requireIncompletePurgeFailClosedEvidence(t, root, quota)

	for attempts := 0; attempts < 64 && !result.complete; attempts++ {
		result, err = purgeSpool(root, defaultSpoolWorkBudget())
		if err != nil {
			t.Fatal(err)
		}
		if !result.complete {
			requireIncompletePurgeFailClosedEvidence(t, root, quota)
		}
	}
	if !result.complete {
		t.Fatal("mutation-free bounded purge did not converge")
	}
	if got := readQuotaFromRoot(t, root); got != (spoolQuota{}) {
		t.Fatalf("mutation-free purge quota = %+v", got)
	}
}

func TestPurgeMalformedKnownTreeDoesNotEnumerateOrStarveBehindRootSiblings(t *testing.T) {
	for _, malformed := range []string{"leaf", "symlink"} {
		t.Run(malformed, func(t *testing.T) {
			home, _, _ := newRecordServiceFixture(t, testEventIDThree)
			plainRoot := mustOpenMutableRoot(t, home)
			event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, plainRoot, inflightDirectoryName, testSpoolGeneration, event)
			quota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
			if err := persistSpoolQuota(plainRoot, quota); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}

			firstSibling := filepath.Join(home.Root(), "unrelated-00000")
			lastSibling := ""
			for index := 0; index < int(maximumCleanupEntries)+1; index++ {
				lastSibling = filepath.Join(home.Root(), fmt.Sprintf("unrelated-%05d", index))
				if err := os.WriteFile(lastSibling, []byte("control"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			queuePath := filepath.Join(home.Root(), queueDirectoryName)
			outsideMarker := ""
			switch malformed {
			case "leaf":
				if err := os.WriteFile(queuePath, []byte("malformed"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				outside := t.TempDir()
				outsideMarker = filepath.Join(outside, "keep")
				if err := os.WriteFile(outsideMarker, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, queuePath); err != nil {
					t.Fatal(err)
				}
			}

			lastMetadataPath := ""
			rootEnumerations := 0
			hooks := storageTestHooks{
				metadata: func(path string, metadata storageMetadata) storageMetadata {
					lastMetadataPath = path
					return metadata
				},
				beforeStep: func(step storageStep) error {
					if step == storageStepEnumerate && lastMetadataPath == home.Root() {
						rootEnumerations++
					}
					return nil
				},
			}
			root, err := openStorageRootMutableWithHooks(home, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()

			result, err := purgeSpool(root, defaultSpoolWorkBudget())
			if err != nil {
				t.Fatal(err)
			}
			ordinaryNameBytes := uint64(len(quotaFileName) + len(spoolControlDirectoryName) + len(retiredControlDirectoryName) + len(fallbackRelocationCursorName) +
				len(queueDirectoryName) + len(inflightDirectoryName) + len(testSpoolGeneration) + len(eventFileName(testEventIDOne)))
			wantEntries := spoolFixedEntryEnvelope + 8
			wantNameBytes := spoolFixedNameEnvelope + ordinaryNameBytes
			if result.usage.entries != wantEntries || result.usage.readBytes != spoolFixedReadEnvelope || result.usage.nameBytes != wantNameBytes {
				t.Fatalf("fixed queue/inflight lookups plus inflight traversal usage = %+v, want %d entries/%d read bytes/%d name bytes", result.usage, wantEntries, spoolFixedReadEnvelope, wantNameBytes)
			}
			if result.complete {
				t.Fatal("tree-mutating malformed-tree purge certified an empty spool")
			}
			if rootEnumerations != 0 {
				t.Fatalf("fixed malformed-tree cleanup enumerated the root %d times before making known-tree progress", rootEnumerations)
			}
			requireIncompletePurgeFailClosedEvidence(t, root, quota)
			var cleanupErr error
			for attempts := 0; attempts < 64; attempts++ {
				result, err = purgeSpool(root, defaultSpoolWorkBudget())
				cleanupErr = err
				if errors.Is(cleanupErr, errUnrecognizedMetricsRootEntry) {
					break
				}
				if cleanupErr != nil {
					t.Fatal(cleanupErr)
				}
				budget := defaultSpoolWorkBudget()
				if result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories ||
					result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes {
					t.Fatalf("purge usage exceeded its root-global budget: %+v", result.usage)
				}
			}
			if !errors.Is(cleanupErr, errUnrecognizedMetricsRootEntry) || result.complete {
				t.Fatalf("unknown root siblings did not keep exact cleanup pending: result=%+v err=%v", result, cleanupErr)
			}
			if rootEnumerations == 0 {
				t.Fatal("final exact-root proof never enumerated the root")
			}
			requireIncompletePurgeFailClosedEvidence(t, root, quota)
			if _, err := os.Lstat(queuePath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("malformed queue remains: %v", err)
			}
			for _, sibling := range []string{firstSibling, lastSibling} {
				if data, err := os.ReadFile(sibling); err != nil || string(data) != "control" {
					t.Fatalf("unknown root entry %q changed: data=%q err=%v", sibling, data, err)
				}
			}
			if outsideMarker != "" {
				if data, err := os.ReadFile(outsideMarker); err != nil || string(data) != "keep" {
					t.Fatalf("malformed-tree cleanup followed symlink: data=%q err=%v", data, err)
				}
			}
		})
	}
}

func TestExactChildLookupCleanupRejectsInodeReplacement(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	queuePath := filepath.Join(home.Root(), queueDirectoryName)
	if err := os.WriteFile(queuePath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	entry, err := root.lookupEntry(queueDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(queuePath, queuePath+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(queuePath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.unlinkEnumeratedEntry(entry); !errors.Is(err, errStorageEntryChanged) {
		t.Fatalf("replacement cleanup error = %v, want %v", err, errStorageEntryChanged)
	}
	if data, err := os.ReadFile(queuePath); err != nil || string(data) != "replacement" {
		t.Fatalf("replacement queue was changed: data=%q err=%v", data, err)
	}
}

func TestPurgeSpoolReportsSaturatingMetadataSizedRemovalWithoutReadingContent(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	generation := "99999999-9999-4999-8999-999999999999"
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	valid := writeSpoolEventFixture(t, root, queueDirectoryName, generation, event)
	directory := mustOpenSpoolGeneration(t, root, queueDirectoryName, generation)
	malformed := []byte("malformed")
	if err := directory.writeFileAtomicNoReplace("malformed", malformed); err != nil {
		t.Fatal(err)
	}
	_ = directory.Close()
	sparseSize := int64(1 << 30)
	sparsePath := filepath.Join(home.Root(), queueDirectoryName, generation, eventFileName(testEventIDTwo))
	if err := os.WriteFile(sparsePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(sparsePath, sparseSize); err != nil {
		t.Fatal(err)
	}
	if err := persistSpoolQuota(root, spoolQuota{Events: 3, Bytes: maximumQuotaByteMarker}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	eventReadCalls := 0
	eventReadBytes := 0
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{afterRead: func(path string, _, read int, _ error) {
		relative, relativeErr := filepath.Rel(home.Root(), path)
		if relativeErr != nil {
			return
		}
		if relative == queueDirectoryName || strings.HasPrefix(relative, queueDirectoryName+string(filepath.Separator)) ||
			relative == inflightDirectoryName || strings.HasPrefix(relative, inflightDirectoryName+string(filepath.Separator)) {
			eventReadCalls++
			eventReadBytes += read
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	result, err := purgeSpool(root, defaultSpoolWorkBudget())
	if err != nil {
		t.Fatal(err)
	}
	if result.complete || result.removedEvents != 3 || result.removedBytes != uint64(len(valid)+len(malformed))+uint64(sparseSize) {
		t.Fatalf("purge result = %+v", result)
	}
	if result.usage.readBytes < spoolFixedReadEnvelope {
		t.Fatalf("purge did not retain its fixed read envelope: %+v", result.usage)
	}
	if eventReadCalls != 0 || eventReadBytes != 0 {
		t.Fatalf("purge physically read event content: calls=%d bytes=%d usage=%+v", eventReadCalls, eventReadBytes, result.usage)
	}
	if got := readQuotaFromRoot(t, root); got != (spoolQuota{Events: 3, Bytes: maximumQuotaByteMarker}) {
		t.Fatalf("mutating purge changed durable quota: %+v", got)
	}
	for attempts := 0; attempts < 8 && !result.complete; attempts++ {
		result, err = purgeSpool(root, defaultSpoolWorkBudget())
		if err != nil {
			t.Fatalf("cleanup-tail attempt %d: %v", attempts+1, err)
		}
		if result.removedEvents != 0 || result.removedBytes != 0 {
			t.Fatalf("cleanup-tail reported event removal: %+v", result)
		}
	}
	if !result.complete {
		t.Fatalf("mutation-free purge result = %+v, err=%v", result, err)
	}

	state := &spoolSweepState{removedEvents: math.MaxUint64, removedBytes: math.MaxUint64}
	state.noteRemoved(math.MaxInt64)
	if state.removedEvents != math.MaxUint64 || state.removedBytes != math.MaxUint64 {
		t.Fatalf("removed counters wrapped: events=%d bytes=%d", state.removedEvents, state.removedBytes)
	}
}

func TestPurgeSpoolConvergesWhenMalformedNestingExceedsDirectoryBudget(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	generation := "99999999-9999-4999-8999-999999999999"
	path := filepath.Join(home.Root(), queueDirectoryName, generation)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 9; index++ {
		path = filepath.Join(path, fmt.Sprintf("nested-%02d", index))
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "poison"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: 1}); err != nil {
		t.Fatal(err)
	}
	budget := spoolWorkBudget{
		maxEntries: spoolFixedEntryEnvelope + 100,
		// Journal proof, top-level tree traversal, and one nested-child step
		// each consume a two-open traversal envelope.
		maxDirectories: spoolMinimumDirectoryProgress,
		maxReadBytes:   spoolFixedReadEnvelope + 1, maxNameBytes: spoolFixedNameEnvelope + 4096,
	}
	result := spoolSweepResult{}
	var err error
	for attempts := 0; attempts < 128 && !result.complete; attempts++ {
		result, err = purgeSpool(root, budget)
		if err != nil {
			t.Fatalf("purge attempt %d: %v", attempts+1, err)
		}
		if !result.complete {
			requireIncompletePurgeFailClosedEvidence(t, root, spoolQuota{Events: 1, Bytes: 1})
		}
	}
	if !result.complete {
		t.Fatalf("malformed deep nesting made no bounded cleanup progress: result=%+v", result)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), queueDirectoryName, generation)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deep generation remains: %v", err)
	}
}

func TestPurgeQuarantineCollisionChainMakesBoundedMonotonicProgress(t *testing.T) {
	for _, kind := range []string{"leaf", "empty-directory", "lax-empty-directory", "deep-directory", "lax-deep-directory", "directory-chain"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newQuarantineCollisionFixture(t, kind)
			defer func() { _ = fixture.root.Close() }()
			tiny := spoolWorkBudget{
				maxEntries: maximumCleanupEntries, maxDirectories: 2,
				maxReadBytes: maximumCleanupReadBytes, maxNameBytes: maximumCleanupNameBytes,
			}

			fixture.quarantinePass(t, tiny)
			switch kind {
			case "leaf", "empty-directory", "lax-empty-directory":
				requireMissingStorageEntry(t, fixture.tree, fixture.sourceCanonical)
				requireStorageEntryIncarnation(t, fixture.generation, fixture.sourceName, fixture.source)
			case "deep-directory", "lax-deep-directory", "directory-chain":
				requireStorageEntryIncarnation(t, fixture.tree, fixture.sourceCanonical, fixture.source)
				requireStorageEntryIncarnation(t, fixture.generation, fixture.sourceName, fixture.blocker)
			}

			fixture.quarantinePass(t, tiny)
			switch kind {
			case "leaf", "empty-directory", "lax-empty-directory":
				requireStorageEntryIncarnation(t, fixture.tree, fixture.sourceCanonical, fixture.source)
				requireMissingStorageEntry(t, fixture.generation, fixture.sourceName)
			case "deep-directory", "lax-deep-directory":
				requireStorageEntryIncarnation(t, fixture.tree, fixture.blockerCanonical, fixture.blocker)
				requireMissingStorageEntry(t, fixture.generation, fixture.sourceName)
			case "directory-chain":
				requireStorageEntryIncarnation(t, fixture.tree, fixture.blockerCanonical, fixture.blocker)
				requireStorageEntryIncarnation(t, fixture.generation, fixture.sourceName, fixture.tail)
			}

			if kind == "directory-chain" {
				fixture.quarantinePass(t, tiny)
				requireStorageEntryIncarnation(t, fixture.tree, fixture.tailCanonical, fixture.tail)
				requireMissingStorageEntry(t, fixture.generation, fixture.sourceName)
			}

			result := spoolSweepResult{}
			for attempts := 0; attempts < 20 && !result.complete; attempts++ {
				result = fixture.purgePass(t, defaultSpoolWorkBudget())
			}
			if !result.complete {
				t.Fatal("bounded collision cleanup did not converge")
			}
			if got := readQuotaFromRoot(t, fixture.root); got != (spoolQuota{}) {
				t.Fatalf("converged collision purge quota = %+v", got)
			}
		})
	}
}

func TestUnsupportedDirectoryExchangeFallsBackToDurableCollisionProgress(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "ENOSYS", err: unix.ENOSYS},
		{name: "ENOTSUP", err: unix.ENOTSUP},
		{name: "EOPNOTSUPP", err: unix.EOPNOTSUPP},
		{name: "EXDEV", err: unix.EXDEV},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newQuarantineCollisionFixture(t, "directory-chain")
			defer func() { _ = fixture.root.Close() }()
			hooks := fixture.unsupportedExchangeHooks(test.err)
			fixture.installHooks(t, hooks)
			tiny := spoolWorkBudget{
				maxEntries: maximumCleanupEntries, maxDirectories: 2,
				maxReadBytes: maximumCleanupReadBytes, maxNameBytes: maximumCleanupNameBytes,
			}

			fixture.unsupportedQuarantinePass(t, tiny)
			fixture.reopen(t, hooks)
			fixture.unsupportedQuarantinePass(t, tiny)

			result := spoolSweepResult{}
			for attempts := 0; attempts < 24 && !result.complete; attempts++ {
				result = fixture.purgePass(t, defaultSpoolWorkBudget())
			}
			if !result.complete || readQuotaFromRoot(t, fixture.root) != (spoolQuota{}) {
				t.Fatalf("unsupported-exchange collision cleanup did not converge: %+v", result)
			}
		})
	}
}

func TestUnsupportedDirectoryExchangeFallsBackThroughAncestorCollision(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "ENOSYS", err: unix.ENOSYS},
		{name: "ENOTSUP", err: unix.ENOTSUP},
		{name: "EOPNOTSUPP", err: unix.EOPNOTSUPP},
		{name: "EXDEV", err: unix.EXDEV},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, _, _ := newRecordServiceFixture(t, testEventIDThree)
			treePath := filepath.Join(home.Root(), queueDirectoryName)
			temporaryAncestor := filepath.Join(treePath, "!ancestor")
			sourceParentPath := filepath.Join(temporaryAncestor, "inside")
			sourceName := "!source"
			sourcePath := filepath.Join(sourceParentPath, sourceName)
			if err := os.MkdirAll(sourcePath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sourcePath, "payload"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			plainRoot := mustOpenMutableRoot(t, home)
			plainTree, err := plainRoot.openDir([]string{queueDirectoryName}, false)
			if err != nil {
				t.Fatal(err)
			}
			plainSourceParent, err := plainTree.openDir([]string{"!ancestor", "inside"}, false)
			if err != nil {
				t.Fatal(err)
			}
			sourceEntry, err := plainSourceParent.lookupEntry(sourceName)
			if err != nil {
				t.Fatal(err)
			}
			sourceCanonical := fmt.Sprintf(".orphan-%x-%x", sourceEntry.metadata.dev, sourceEntry.metadata.ino)
			if err := os.Rename(temporaryAncestor, filepath.Join(treePath, sourceCanonical)); err != nil {
				t.Fatal(err)
			}
			_ = plainSourceParent.Close()
			blockerEntry, err := plainTree.lookupEntry(sourceCanonical)
			if err != nil {
				t.Fatal(err)
			}
			blockerCanonical := fmt.Sprintf(".orphan-%x-%x", blockerEntry.metadata.dev, blockerEntry.metadata.ino)
			tailPath := filepath.Join(treePath, blockerCanonical)
			if err := os.Mkdir(tailPath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tailPath, "payload"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			quota := spoolQuota{Events: 1, Bytes: 1}
			if err := persistSpoolQuota(plainRoot, quota); err != nil {
				t.Fatal(err)
			}
			_ = plainTree.Close()
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}

			namespaceMutations := 0
			hooks := storageTestHooks{
				beforeStep: func(step storageStep) error {
					if step == storageStepRename || step == storageStepUnlink || step == storageStepRmdir {
						namespaceMutations++
					}
					return nil
				},
				beforeExchange: func() error { return test.err },
			}
			root, err := openStorageRootMutableWithHooks(home, hooks)
			if err != nil {
				t.Fatal(err)
			}
			tree, err := root.openDir([]string{queueDirectoryName}, false)
			if err != nil {
				t.Fatal(err)
			}
			sourceParent, err := tree.openDir([]string{sourceCanonical, "inside"}, false)
			if err != nil {
				t.Fatal(err)
			}
			current, err := sourceParent.lookupEntry(sourceName)
			if err != nil {
				t.Fatal(err)
			}
			budget := spoolWorkBudget{
				maxEntries: maximumCleanupEntries, maxDirectories: 2,
				maxReadBytes: maximumCleanupReadBytes, maxNameBytes: maximumCleanupNameBytes,
			}
			meter := newSpoolWorkMeter(budget)
			meter.usage = spoolWorkUsage{entries: 2, directories: budget.maxDirectories - 1, nameBytes: uint64(len(sourceCanonical) + len(sourceName))}
			meter.exhausted = true
			state := &spoolSweepState{root: root, purgeAll: true, meter: meter}
			state.quarantineDirectory(sourceParent, tree, current, true, true)
			if state.operation != nil {
				t.Fatal(state.operation)
			}
			if !state.mutated || namespaceMutations == 0 {
				t.Fatal("unsupported ancestor collision made no durable progress")
			}
			if meter.usage.entries > budget.maxEntries || meter.usage.directories > budget.maxDirectories ||
				meter.usage.readBytes > budget.maxReadBytes || meter.usage.nameBytes > budget.maxNameBytes {
				t.Fatalf("unsupported ancestor fallback exceeded budget: result=%+v budget=%+v", meter.usage, budget)
			}
			_ = sourceParent.Close()
			_ = tree.Close()
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}

			root, err = openStorageRootMutableWithHooks(home, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			result := spoolSweepResult{}
			for attempts := 0; attempts < 24 && !result.complete; attempts++ {
				result, err = purgeSpool(root, defaultSpoolWorkBudget())
				if err != nil {
					t.Fatal(err)
				}
			}
			if !result.complete || readQuotaFromRoot(t, root) != (spoolQuota{}) {
				t.Fatalf("unsupported ancestor collision did not converge after reopen: %+v", result)
			}
		})
	}
}

func TestRelocationCursorCrashReplaySkipsReservedSlotsAndReconverges(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	defer func() { _ = fixture.root.Close() }()
	hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	fixture.installHooks(t, hooks)
	budget := spoolWorkBudget{
		maxEntries: maximumCleanupEntries, maxDirectories: 2,
		maxReadBytes: maximumCleanupReadBytes, maxNameBytes: maximumCleanupNameBytes,
	}
	entry, err := fixture.generation.lookupEntry(fixture.sourceName)
	if err != nil {
		t.Fatal(err)
	}
	meter := exhaustedCollisionMeter(budget, fixture.sourceName)
	state := &spoolSweepState{
		root: fixture.root, purgeAll: true, meter: meter,
		afterRelocationReservation: func() error { return errors.New("simulated crash after durable high-water reservation") },
	}
	state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
	if state.operation == nil || !state.mutated {
		t.Fatalf("post-reservation crash state = mutated:%v err:%v", state.mutated, state.operation)
	}
	if state.relocationQuotaMarked || state.durableQuotaMarker {
		fixture.quota = spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	}
	if cursor := readRelocationCursorFromRoot(t, fixture.root); cursor.Next != maximumRelocationSlots {
		t.Fatalf("durable cursor after simulated crash = %+v", cursor)
	}
	requireStorageEntryIncarnation(t, fixture.generation, fixture.sourceName, fixture.source)
	requireStorageEntryIncarnation(t, fixture.tree, fixture.sourceCanonical, fixture.blocker)

	fixture.reopen(t, hooks)
	fixture.unsupportedQuarantinePass(t, budget)
	requireMissingStorageEntry(t, fixture.tree, relocationCandidateName(0))
	requireStorageEntryIncarnation(t, fixture.tree, relocationCandidateName(maximumRelocationSlots), fixture.blocker)
	fixture.reopen(t, hooks)
	fixture.unsupportedQuarantinePass(t, budget)

	result := spoolSweepResult{}
	for attempts := 0; attempts < 24 && !result.complete; attempts++ {
		result = fixture.purgePass(t, defaultSpoolWorkBudget())
	}
	if !result.complete || readQuotaFromRoot(t, fixture.root) != (spoolQuota{}) {
		t.Fatalf("post-reservation crash replay did not converge: %+v", result)
	}
}

func TestRelocationCursorReservesExactlyEightSlotsOrNone(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	defer func() { _ = fixture.root.Close() }()
	hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	fixture.installHooks(t, hooks)
	entry, err := fixture.generation.lookupEntry(fixture.sourceName)
	if err != nil {
		t.Fatal(err)
	}
	budget := defaultSpoolWorkBudget()
	budget.maxEntries = 13 // Six prior charges leave seven candidate slots.
	state := &spoolSweepState{
		root: fixture.root, purgeAll: true,
		meter: exhaustedCollisionMeter(budget, fixture.sourceName),
	}
	state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
	if state.operation == nil {
		t.Fatal("partial relocation block unexpectedly succeeded")
	}
	requireStorageEntryIncarnation(t, fixture.generation, fixture.sourceName, fixture.source)
	requireStorageEntryIncarnation(t, fixture.tree, fixture.sourceCanonical, fixture.blocker)
	control, err := fixture.root.openDir([]string{spoolControlDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Close() }()
	if _, err := control.lookupEntry(relocationCursorFileName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("partial high-water block was persisted: %v", err)
	}
}

func TestFallbackRelocationSyncUncertaintyReplaysFromExactNamespace(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	defer func() { _ = fixture.root.Close() }()
	exchangeAttempted := false
	renamesAfterExchange := 0
	failSync := true
	hooks := storageTestHooks{
		beforeExchange: func() error {
			exchangeAttempted = true
			return unix.ENOSYS
		},
		beforeStep: func(step storageStep) error {
			if step == storageStepRename || step == storageStepUnlink || step == storageStepRmdir {
				fixture.namespaceMutations++
			}
			if exchangeAttempted && step == storageStepRename {
				renamesAfterExchange++
			}
			if failSync && step == storageStepDirectorySync && renamesAfterExchange >= 2 {
				failSync = false
				return errors.New("simulated crash at fallback parent sync")
			}
			return nil
		},
	}
	fixture.installHooks(t, hooks)
	budget := spoolWorkBudget{
		maxEntries: maximumCleanupEntries, maxDirectories: 2,
		maxReadBytes: maximumCleanupReadBytes, maxNameBytes: maximumCleanupNameBytes,
	}
	entry, err := fixture.generation.lookupEntry(fixture.sourceName)
	if err != nil {
		t.Fatal(err)
	}
	state := &spoolSweepState{root: fixture.root, purgeAll: true, meter: exhaustedCollisionMeter(budget, fixture.sourceName)}
	state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
	if state.operation == nil || !state.mutated || failSync {
		t.Fatalf("fallback sync uncertainty = mutated:%v failPending:%v err:%v", state.mutated, failSync, state.operation)
	}
	if state.relocationQuotaMarked || state.durableQuotaMarker {
		fixture.quota = spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	}
	requireMissingStorageEntry(t, fixture.tree, fixture.sourceCanonical)
	requireStorageEntryIncarnation(t, fixture.tree, relocationCandidateName(0), fixture.blocker)

	cleanHooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	fixture.reopen(t, cleanHooks)
	fixture.unsupportedQuarantinePass(t, budget)
	result := spoolSweepResult{}
	for attempts := 0; attempts < 24 && !result.complete; attempts++ {
		result = fixture.purgePass(t, defaultSpoolWorkBudget())
	}
	if !result.complete || readQuotaFromRoot(t, fixture.root) != (spoolQuota{}) {
		t.Fatalf("fallback sync-uncertainty replay did not converge: %+v", result)
	}
}

func TestFallbackControlHandleConsumesReservedDirectorySlot(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	defer func() { _ = fixture.root.Close() }()
	controlOpens := 0
	hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	hooks.afterComponentOpen = func(path string) {
		if filepath.Base(path) == spoolControlDirectoryName {
			controlOpens++
		}
	}
	fixture.installHooks(t, hooks)
	budget := spoolWorkBudget{
		maxEntries: maximumCleanupEntries, maxDirectories: 2,
		maxReadBytes: maximumCleanupReadBytes, maxNameBytes: maximumCleanupNameBytes,
	}
	entry, err := fixture.generation.lookupEntry(fixture.sourceName)
	if err != nil {
		t.Fatal(err)
	}
	state := &spoolSweepState{root: fixture.root, purgeAll: true, meter: exhaustedCollisionMeter(budget, fixture.sourceName)}
	state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
	if state.operation != nil {
		t.Fatal(state.operation)
	}
	if controlOpens != 1 || state.meter.usage.directories != budget.maxDirectories {
		t.Fatalf("fallback control usage = opens:%d meter:%+v budget:%+v", controlOpens, state.meter.usage, budget)
	}
}

func TestRelocationCursorCorruptionAndExhaustionRetiresThenReconverges(t *testing.T) {
	for _, test := range []struct {
		name string
		data func(*testing.T) []byte
	}{
		{name: "corrupt", data: func(*testing.T) []byte { return []byte("not = [toml") }},
		{name: "exhausted", data: func(t *testing.T) []byte {
			data, err := encodeRelocationCursor(relocationCursor{Next: maximumRelocationSequence})
			if err != nil {
				t.Fatal(err)
			}
			return data
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newQuarantineCollisionFixture(t, "directory-chain")
			defer func() { _ = fixture.root.Close() }()
			writeRelocationCursorFixture(t, fixture.root, test.data(t))
			hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
			fixture.installHooks(t, hooks)
			budget := defaultSpoolWorkBudget()
			entry, err := fixture.generation.lookupEntry(fixture.sourceName)
			if err != nil {
				t.Fatal(err)
			}
			state := &spoolSweepState{root: fixture.root, purgeAll: true, meter: exhaustedCollisionMeter(budget, fixture.sourceName)}
			state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
			if state.operation != nil || !state.mutated {
				t.Fatalf("cursor retirement = mutated:%v err:%v", state.mutated, state.operation)
			}
			requireStorageEntryIncarnation(t, fixture.generation, fixture.sourceName, fixture.source)
			requireStorageEntryIncarnation(t, fixture.tree, fixture.sourceCanonical, fixture.blocker)
			if _, err := fixture.root.lookupEntry(spoolControlDirectoryName); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("active control survived retirement: %v", err)
			}
			if _, err := fixture.root.lookupEntry(retiredControlDirectoryName); err != nil {
				t.Fatalf("retired control evidence is absent: %v", err)
			}
			if state.relocationQuotaMarked || state.durableQuotaMarker {
				fixture.quota = spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
			}
			fixture.reopen(t, hooks)
			fixture.unsupportedQuarantinePass(t, budget)
			fixture.reopen(t, hooks)
			fixture.unsupportedQuarantinePass(t, budget)
			result := spoolSweepResult{}
			for attempts := 0; attempts < 24 && !result.complete; attempts++ {
				result = fixture.purgePass(t, defaultSpoolWorkBudget())
			}
			if !result.complete || readQuotaFromRoot(t, fixture.root) != (spoolQuota{}) {
				t.Fatalf("retired cursor did not reconverge: %+v", result)
			}
		})
	}
}

func TestRelocationCursorSurvivesConservativeQuotaRewriteUntilEmptyProof(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	defer func() { _ = fixture.root.Close() }()
	hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	fixture.installHooks(t, hooks)
	budget := defaultSpoolWorkBudget()
	fixture.unsupportedQuarantinePass(t, budget)
	wantCursor := readRelocationCursorFromRoot(t, fixture.root)
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	if err := persistSpoolQuota(fixture.root, markers); err != nil {
		t.Fatal(err)
	}
	if got := readRelocationCursorFromRoot(t, fixture.root); got != wantCursor {
		t.Fatalf("conservative quota rewrite changed cursor from %+v to %+v", wantCursor, got)
	}
	fixture.quota = markers
	fixture.reopen(t, hooks)
	fixture.unsupportedQuarantinePass(t, budget)
	result := spoolSweepResult{}
	for attempts := 0; attempts < 24 && !result.complete; attempts++ {
		result = fixture.purgePass(t, defaultSpoolWorkBudget())
	}
	if !result.complete || readQuotaFromRoot(t, fixture.root) != (spoolQuota{}) {
		t.Fatalf("cursor-preserving cleanup did not converge: %+v", result)
	}
	if _, err := fixture.root.lookupEntry(spoolControlDirectoryName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("relocation control survived mutation-free proof: %v", err)
	}
}

func TestUnsupportedExchangePoisonedFixedControlConverges(t *testing.T) {
	for _, test := range []struct {
		name             string
		forceQuotaAbsent bool
		setup            func(*testing.T, string) string
	}{
		{
			name: "nonempty relocation directory",
			setup: func(t *testing.T, control string) string {
				path := filepath.Join(control, relocationCursorFileName, "deep")
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "payload"), []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				return ""
			},
		},
		{
			name: "lax relocation directory",
			setup: func(t *testing.T, control string) string {
				path := filepath.Join(control, relocationCursorFileName)
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "payload"), []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				return ""
			},
		},
		{
			name: "symlinked relocation cursor",
			setup: func(t *testing.T, control string) string {
				sentinel := filepath.Join(t.TempDir(), "sentinel")
				if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, filepath.Join(control, relocationCursorFileName)); err != nil {
					t.Fatal(err)
				}
				return sentinel
			},
		},
		{
			name: "nonempty quota staging directory", forceQuotaAbsent: true,
			setup: func(t *testing.T, control string) string {
				path := filepath.Join(control, quotaStagingFileName, "deep")
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "payload"), []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				return ""
			},
		},
		{
			name: "symlinked quota staging", forceQuotaAbsent: true,
			setup: func(t *testing.T, control string) string {
				sentinel := filepath.Join(t.TempDir(), "sentinel")
				if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, filepath.Join(control, quotaStagingFileName)); err != nil {
					t.Fatal(err)
				}
				return sentinel
			},
		},
		{
			name: "FIFO quota staging", forceQuotaAbsent: true,
			setup: func(t *testing.T, control string) string {
				if err := unix.Mkfifo(filepath.Join(control, quotaStagingFileName), 0o600); err != nil {
					t.Fatal(err)
				}
				return ""
			},
		},
		{
			name: "oversized quota staging", forceQuotaAbsent: true,
			setup: func(t *testing.T, control string) string {
				path := filepath.Join(control, quotaStagingFileName)
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, maximumQuotaBytes+1); err != nil {
					t.Fatal(err)
				}
				return ""
			},
		},
		{
			name: "lax active control",
			setup: func(t *testing.T, control string) string {
				if err := os.Chmod(control, 0o755); err != nil {
					t.Fatal(err)
				}
				return ""
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newQuarantineCollisionFixture(t, "directory-chain")
			_ = fixture.generation.Close()
			_ = fixture.tree.Close()
			_ = fixture.root.Close()

			controlPath := filepath.Join(fixture.home.Root(), spoolControlDirectoryName)
			if err := os.Mkdir(controlPath, 0o700); err != nil {
				t.Fatal(err)
			}
			sentinel := test.setup(t, controlPath)
			if test.forceQuotaAbsent {
				if err := os.Remove(filepath.Join(fixture.home.Root(), quotaFileName)); err != nil {
					t.Fatal(err)
				}
			}

			hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
			root, err := openStorageRootMutableWithHooks(fixture.home, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()

			result := spoolSweepResult{}
			var lastErr error
			for attempts := 0; attempts < 40 && !result.complete; attempts++ {
				result, lastErr = purgeSpool(root, defaultSpoolWorkBudget())
			}
			if !result.complete || lastErr != nil {
				t.Fatalf("poisoned control did not converge: result=%+v err=%v", result, lastErr)
			}
			if got := readQuotaFromRoot(t, root); got != (spoolQuota{}) {
				t.Fatalf("converged poisoned-control quota = %+v", got)
			}
			for _, name := range []string{spoolControlDirectoryName, retiredControlDirectoryName} {
				if _, err := root.lookupEntry(name); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("control namespace %q remains: %v", name, err)
				}
			}
			if sentinel != "" {
				data, err := os.ReadFile(sentinel)
				if err != nil || string(data) != "outside" {
					t.Fatalf("outside sentinel changed: data=%q err=%v", data, err)
				}
			}
		})
	}
}

func TestUnsupportedExchangeRetiresPoisonedActiveControlBeforeRetry(t *testing.T) {
	for _, test := range []struct {
		name             string
		forceQuotaAbsent bool
		setup            func(*testing.T, string)
	}{
		{
			name: "nonempty relocation directory",
			setup: func(t *testing.T, control string) {
				path := filepath.Join(control, relocationCursorFileName, "deep")
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "payload"), []byte("poison"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlinked quota staging", forceQuotaAbsent: true,
			setup: func(t *testing.T, control string) {
				sentinel := filepath.Join(t.TempDir(), "sentinel")
				if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, filepath.Join(control, quotaStagingFileName)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lax active control",
			setup: func(t *testing.T, control string) {
				if err := os.Chmod(control, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newQuarantineCollisionFixture(t, "directory-chain")
			defer func() { _ = fixture.root.Close() }()
			controlPath := filepath.Join(fixture.home.Root(), spoolControlDirectoryName)
			if err := os.Mkdir(controlPath, 0o700); err != nil {
				t.Fatal(err)
			}
			test.setup(t, controlPath)
			poisonedControl, err := os.Stat(controlPath)
			if err != nil {
				t.Fatal(err)
			}
			if test.forceQuotaAbsent {
				if err := os.Remove(filepath.Join(fixture.home.Root(), quotaFileName)); err != nil {
					t.Fatal(err)
				}
			}

			wroteInsidePoisonedControl := false
			hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
			priorStep := hooks.beforeStep
			hooks.beforeStep = func(step storageStep) error {
				if err := priorStep(step); err != nil {
					return err
				}
				if step == storageStepWrite {
					if active, err := os.Stat(controlPath); err == nil && os.SameFile(active, poisonedControl) {
						wroteInsidePoisonedControl = true
					}
				}
				return nil
			}
			fixture.installHooks(t, hooks)
			entry, err := fixture.generation.lookupEntry(fixture.sourceName)
			if err != nil {
				t.Fatal(err)
			}
			state := &spoolSweepState{
				root: fixture.root, purgeAll: true,
				meter: exhaustedCollisionMeter(defaultSpoolWorkBudget(), fixture.sourceName),
			}
			state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
			if state.operation != nil || !state.mutated {
				t.Fatalf("poisoned active-control retirement = mutated:%v err:%v", state.mutated, state.operation)
			}
			if wroteInsidePoisonedControl {
				t.Fatal("recovery wrote inside poisoned active control before retirement")
			}
			requireStorageEntryIncarnation(t, fixture.generation, fixture.sourceName, fixture.source)
			if _, err := fixture.root.lookupEntry(spoolControlDirectoryName); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("poisoned active control remains: %v", err)
			}
			if _, err := fixture.root.lookupEntry(retiredControlDirectoryName); err != nil {
				t.Fatalf("retired control evidence is absent: %v", err)
			}
		})
	}
}

func TestUnsupportedExchangeConvergesWithPoisonedActiveAndRetiredControl(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	active := filepath.Join(fixture.home.Root(), spoolControlDirectoryName)
	if err := os.MkdirAll(filepath.Join(active, relocationCursorFileName, "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, relocationCursorFileName, "deep", "payload"), []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(fixture.home.Root(), retiredControlDirectoryName)
	if err := os.MkdirAll(filepath.Join(retired, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retired, "deep", "payload"), []byte("retired"), 0o600); err != nil {
		t.Fatal(err)
	}
	hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	fixture.installHooks(t, hooks)
	entry, err := fixture.generation.lookupEntry(fixture.sourceName)
	if err != nil {
		t.Fatal(err)
	}
	state := &spoolSweepState{
		root: fixture.root, purgeAll: true,
		meter: exhaustedCollisionMeter(defaultSpoolWorkBudget(), fixture.sourceName),
	}
	state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
	if state.operation != nil {
		t.Fatalf("preexisting retired control blocked its own bounded cleanup: %v", state.operation)
	}
	_ = fixture.generation.Close()
	_ = fixture.tree.Close()
	_ = fixture.root.Close()

	root, err := openStorageRootMutableWithHooks(fixture.home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	result := spoolSweepResult{}
	var lastErr error
	for attempts := 0; attempts < 48 && !result.complete; attempts++ {
		result, lastErr = purgeSpool(root, defaultSpoolWorkBudget())
		if !result.complete {
			requireIncompletePurgeFailClosedEvidence(t, root, fixture.quota)
		}
	}
	if !result.complete || lastErr != nil {
		t.Fatalf("dual poisoned controls did not converge: result=%+v err=%v", result, lastErr)
	}
	if got := readQuotaFromRoot(t, root); got != (spoolQuota{}) {
		t.Fatalf("converged dual-control quota = %+v", got)
	}
}

func TestAncestorQuarantineCollisionRelocatesBlockerAndConverges(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	treePath := filepath.Join(home.Root(), queueDirectoryName)
	temporaryAncestor := filepath.Join(treePath, "!ancestor")
	sourceParentPath := filepath.Join(temporaryAncestor, "inside")
	sourceName := "!source"
	sourcePath := filepath.Join(sourceParentPath, sourceName)
	if err := os.MkdirAll(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "payload"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	plainRoot := mustOpenMutableRoot(t, home)
	plainTree, err := plainRoot.openDir([]string{queueDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	plainSourceParent, err := plainTree.openDir([]string{"!ancestor", "inside"}, false)
	if err != nil {
		t.Fatal(err)
	}
	sourceEntry, err := plainSourceParent.lookupEntry(sourceName)
	if err != nil {
		t.Fatal(err)
	}
	source := recordIncarnation{dev: sourceEntry.metadata.dev, ino: sourceEntry.metadata.ino}
	sourceCanonical := fmt.Sprintf(".orphan-%x-%x", source.dev, source.ino)
	if err := os.Rename(temporaryAncestor, filepath.Join(treePath, sourceCanonical)); err != nil {
		t.Fatal(err)
	}
	_ = plainSourceParent.Close()
	blockerEntry, err := plainTree.lookupEntry(sourceCanonical)
	if err != nil {
		t.Fatal(err)
	}
	blocker := recordIncarnation{dev: blockerEntry.metadata.dev, ino: blockerEntry.metadata.ino}
	blockerCanonical := fmt.Sprintf(".orphan-%x-%x", blocker.dev, blocker.ino)
	tailPath := filepath.Join(treePath, blockerCanonical)
	if err := os.Mkdir(tailPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tailPath, "payload"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tailEntry, err := plainTree.lookupEntry(blockerCanonical)
	if err != nil {
		t.Fatal(err)
	}
	tail := recordIncarnation{dev: tailEntry.metadata.dev, ino: tailEntry.metadata.ino}
	tailCanonical := fmt.Sprintf(".orphan-%x-%x", tail.dev, tail.ino)
	quota := spoolQuota{Events: 1, Bytes: 1}
	if err := persistSpoolQuota(plainRoot, quota); err != nil {
		t.Fatal(err)
	}
	_ = plainTree.Close()
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	namespaceMutations := 0
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepRename || step == storageStepUnlink || step == storageStepRmdir {
			namespaceMutations++
		}
		return nil
	}}
	root, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	tree, err := root.openDir([]string{queueDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	sourceParent, err := tree.openDir([]string{sourceCanonical, "inside"}, false)
	if err != nil {
		t.Fatal(err)
	}
	tiny := spoolWorkBudget{
		maxEntries: maximumCleanupEntries, maxDirectories: 2,
		maxReadBytes: maximumCleanupReadBytes, maxNameBytes: maximumCleanupNameBytes,
	}

	namespaceMutations = 0
	runDirectQuarantinePass(t, root, sourceParent, tree, sourceName, quota, tiny, 2, &namespaceMutations)
	requireStorageEntryIncarnation(t, tree, blockerCanonical, blocker)
	requireStorageEntryIncarnation(t, tree, sourceCanonical, tail)
	_ = sourceParent.Close()
	sourceParent, err = tree.openDir([]string{blockerCanonical, "inside"}, false)
	if err != nil {
		t.Fatal(err)
	}

	namespaceMutations = 0
	runDirectQuarantinePass(t, root, sourceParent, tree, sourceName, quota, tiny, 1, &namespaceMutations)
	requireStorageEntryIncarnation(t, tree, sourceCanonical, source)
	requireStorageEntryIncarnation(t, sourceParent, sourceName, tail)

	namespaceMutations = 0
	runDirectQuarantinePass(t, root, sourceParent, tree, sourceName, quota, tiny, 1, &namespaceMutations)
	requireStorageEntryIncarnation(t, tree, tailCanonical, tail)
	requireMissingStorageEntry(t, sourceParent, sourceName)
	_ = sourceParent.Close()

	result := spoolSweepResult{}
	for attempts := 0; attempts < 20 && !result.complete; attempts++ {
		namespaceMutations = 0
		result, err = purgeSpool(root, defaultSpoolWorkBudget())
		if err != nil {
			t.Fatal(err)
		}
		if !result.complete {
			if namespaceMutations == 0 {
				t.Fatal("incomplete ancestor-collision purge made no namespace progress")
			}
			requireIncompletePurgeFailClosedEvidence(t, root, quota)
		}
	}
	if !result.complete || readQuotaFromRoot(t, root) != (spoolQuota{}) {
		t.Fatalf("ancestor-collision cleanup did not converge: %+v", result)
	}
}

type quarantineCollisionFixture struct {
	home               gchome.ProductUsageHome
	root               *storageRoot
	tree               *storageDir
	generation         *storageDir
	sourceName         string
	sourceCanonical    string
	blockerCanonical   string
	tailCanonical      string
	source             recordIncarnation
	blocker            recordIncarnation
	tail               recordIncarnation
	quota              spoolQuota
	namespaceMutations int
}

func newQuarantineCollisionFixture(t *testing.T, kind string) *quarantineCollisionFixture {
	t.Helper()
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	treePath := filepath.Join(home.Root(), queueDirectoryName)
	generationName := "!source-generation"
	sourceName := "!source"
	generationPath := filepath.Join(treePath, generationName)
	sourcePath := filepath.Join(generationPath, sourceName)
	if err := os.MkdirAll(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "payload"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	plainRoot := mustOpenMutableRoot(t, home)
	plainTree, err := plainRoot.openDir([]string{queueDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	plainGeneration, err := plainTree.openDir([]string{generationName}, false)
	if err != nil {
		t.Fatal(err)
	}
	sourceEntry, err := plainGeneration.lookupEntry(sourceName)
	if err != nil {
		t.Fatal(err)
	}
	sourceCanonical := fmt.Sprintf(".orphan-%x-%x", sourceEntry.metadata.dev, sourceEntry.metadata.ino)
	blockerPath := filepath.Join(treePath, sourceCanonical)
	switch kind {
	case "leaf":
		err = os.WriteFile(blockerPath, []byte("collision"), 0o600)
	case "empty-directory", "lax-empty-directory", "deep-directory", "lax-deep-directory", "directory-chain":
		err = os.Mkdir(blockerPath, 0o700)
	default:
		t.Fatalf("unknown collision kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	if kind == "deep-directory" || kind == "lax-deep-directory" {
		path := blockerPath
		for depth := 0; depth < 513; depth++ {
			path = filepath.Join(path, fmt.Sprintf("d%03d", depth))
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(path, "payload"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "lax-empty-directory" || kind == "lax-deep-directory" {
		if err := os.Chmod(blockerPath, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "directory-chain" {
		if err := os.WriteFile(filepath.Join(blockerPath, "payload"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	blockerEntry, err := plainTree.lookupEntry(sourceCanonical)
	if err != nil {
		t.Fatal(err)
	}
	blockerCanonical := fmt.Sprintf(".orphan-%x-%x", blockerEntry.metadata.dev, blockerEntry.metadata.ino)
	var tailEntry storageEntry
	if kind == "directory-chain" {
		tailPath := filepath.Join(treePath, blockerCanonical)
		if err := os.Mkdir(tailPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tailPath, "payload"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		tailEntry, err = plainTree.lookupEntry(blockerCanonical)
		if err != nil {
			t.Fatal(err)
		}
	}
	quota := spoolQuota{Events: 1, Bytes: 1}
	if err := persistSpoolQuota(plainRoot, quota); err != nil {
		t.Fatal(err)
	}
	_ = plainGeneration.Close()
	_ = plainTree.Close()
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	fixture := &quarantineCollisionFixture{
		home:       home,
		sourceName: sourceName, sourceCanonical: sourceCanonical, blockerCanonical: blockerCanonical,
		source:  recordIncarnation{dev: sourceEntry.metadata.dev, ino: sourceEntry.metadata.ino},
		blocker: recordIncarnation{dev: blockerEntry.metadata.dev, ino: blockerEntry.metadata.ino},
		tail:    recordIncarnation{dev: tailEntry.metadata.dev, ino: tailEntry.metadata.ino},
		quota:   quota,
	}
	if tailEntry.name != "" {
		fixture.tailCanonical = fmt.Sprintf(".orphan-%x-%x", tailEntry.metadata.dev, tailEntry.metadata.ino)
	}
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepRename || step == storageStepUnlink || step == storageStepRmdir {
			fixture.namespaceMutations++
		}
		return nil
	}}
	fixture.root, err = openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	fixture.tree, err = fixture.root.openDir([]string{queueDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	fixture.generation, err = fixture.tree.openDir([]string{generationName}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = fixture.generation.Close()
		_ = fixture.tree.Close()
	})
	return fixture
}

func (fixture *quarantineCollisionFixture) unsupportedExchangeHooks(injected error) storageTestHooks {
	return storageTestHooks{
		beforeStep: func(step storageStep) error {
			if step == storageStepRename || step == storageStepUnlink || step == storageStepRmdir {
				fixture.namespaceMutations++
			}
			return nil
		},
		beforeExchange: func() error { return injected },
	}
}

func (fixture *quarantineCollisionFixture) installHooks(t *testing.T, hooks storageTestHooks) {
	t.Helper()
	for _, directory := range []*storageDir{fixture.root.storageDir, fixture.tree, fixture.generation} {
		backend, ok := directory.backend.(*unixStorageDirectory)
		if !ok {
			t.Fatalf("storage backend = %T, want unix", directory.backend)
		}
		backend.hooks = hooks
	}
}

func (fixture *quarantineCollisionFixture) reopen(t *testing.T, hooks storageTestHooks) {
	t.Helper()
	_ = fixture.generation.Close()
	_ = fixture.tree.Close()
	_ = fixture.root.Close()
	var err error
	fixture.root, err = openStorageRootMutableWithHooks(fixture.home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	fixture.tree, err = fixture.root.openDir([]string{queueDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	fixture.generation, err = fixture.tree.openDir([]string{"!source-generation"}, false)
	if err != nil {
		t.Fatal(err)
	}
}

func (fixture *quarantineCollisionFixture) unsupportedQuarantinePass(t *testing.T, budget spoolWorkBudget) {
	t.Helper()
	fixture.namespaceMutations = 0
	entry, err := fixture.generation.lookupEntry(fixture.sourceName)
	if err != nil {
		t.Fatal(err)
	}
	meter := exhaustedCollisionMeter(budget, fixture.sourceName)
	state := &spoolSweepState{root: fixture.root, purgeAll: true, meter: meter}
	state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
	if state.operation != nil {
		t.Fatal(state.operation)
	}
	if !state.mutated {
		t.Fatal("unsupported exchange made no durable cursor or namespace progress")
	}
	if meter.usage.entries > budget.maxEntries || meter.usage.directories > budget.maxDirectories ||
		meter.usage.readBytes > budget.maxReadBytes || meter.usage.nameBytes > budget.maxNameBytes {
		t.Fatalf("unsupported fallback exceeded budget: result=%+v budget=%+v", meter.usage, budget)
	}
	if state.relocationQuotaMarked || state.durableQuotaMarker {
		fixture.quota = spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	}
	if got := readQuotaFromRoot(t, fixture.root); got != fixture.quota {
		t.Fatalf("unsupported fallback changed conservative quota to %+v", got)
	}
}

func exhaustedCollisionMeter(budget spoolWorkBudget, sourceName string) *spoolWorkMeter {
	meter := newSpoolWorkMeter(budget)
	meter.usage = spoolWorkUsage{
		entries: 2, directories: budget.maxDirectories - 1,
		nameBytes: uint64(len("!source-generation") + len(sourceName)),
	}
	meter.exhausted = true
	return meter
}

func readRelocationCursorFromRoot(t *testing.T, root *storageRoot) relocationCursor {
	t.Helper()
	control, err := root.openDir([]string{spoolControlDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Close() }()
	data, err := control.readFile(relocationCursorFileName, maximumRelocationBytes)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodeRelocationCursor(data)
	if err != nil {
		t.Fatal(err)
	}
	return cursor
}

func readFallbackRelocationCursorFromRoot(t *testing.T, root *storageRoot) relocationCursor {
	t.Helper()
	data, err := root.readFile(fallbackRelocationCursorName, maximumRelocationBytes)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodeRelocationCursor(data)
	if err != nil {
		t.Fatal(err)
	}
	return cursor
}

func writeRelocationCursorFixture(t *testing.T, root *storageRoot, data []byte) {
	t.Helper()
	control, err := root.openDir([]string{spoolControlDirectoryName}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Close() }()
	if err := control.writeFileAtomic(relocationCursorFileName, data); err != nil {
		t.Fatal(err)
	}
}

func (fixture *quarantineCollisionFixture) purgePass(t *testing.T, budget spoolWorkBudget) spoolSweepResult {
	t.Helper()
	fixture.namespaceMutations = 0
	result, err := purgeSpool(fixture.root, budget)
	if err != nil {
		t.Fatal(err)
	}
	if result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories ||
		result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes {
		t.Fatalf("collision purge exceeded budget: result=%+v budget=%+v", result.usage, budget)
	}
	if !result.complete {
		if fixture.namespaceMutations == 0 {
			t.Fatal("incomplete collision pass made no namespace progress")
		}
		requireIncompletePurgeFailClosedEvidence(t, fixture.root, fixture.quota)
	}
	return result
}

func (fixture *quarantineCollisionFixture) quarantinePass(t *testing.T, budget spoolWorkBudget) {
	t.Helper()
	fixture.namespaceMutations = 0
	runDirectQuarantinePass(t, fixture.root, fixture.generation, fixture.tree, fixture.sourceName,
		fixture.quota, budget, 1, &fixture.namespaceMutations)
}

func runDirectQuarantinePass(t *testing.T, root *storageRoot, sourceParent, quarantineRoot *storageDir, sourceName string,
	quota spoolQuota, budget spoolWorkBudget, wantFixedLookups int, namespaceMutations *int,
) {
	t.Helper()
	entry, err := sourceParent.lookupEntry(sourceName)
	if err != nil {
		t.Fatal(err)
	}
	baseNameBytes := uint64(len("!source-generation") + len(sourceName))
	meter := newSpoolWorkMeter(budget)
	meter.usage = spoolWorkUsage{entries: 2, directories: budget.maxDirectories, nameBytes: baseNameBytes}
	meter.exhausted = true
	state := &spoolSweepState{root: root, purgeAll: true, meter: meter}
	state.quarantineDirectory(sourceParent, quarantineRoot, entry, true, true)
	if state.operation != nil {
		t.Fatal(state.operation)
	}
	if !state.mutated || *namespaceMutations == 0 {
		t.Fatal("collision pass made no durable namespace progress")
	}
	wantEntries := uint64(2 + 2 + wantFixedLookups)
	if meter.usage.entries != wantEntries {
		t.Fatalf("collision lookup usage = %+v, want two fail-closed control lookups plus %d exact target charges", meter.usage, wantFixedLookups)
	}
	if meter.usage.entries > budget.maxEntries || meter.usage.directories > budget.maxDirectories ||
		meter.usage.readBytes > budget.maxReadBytes || meter.usage.nameBytes > budget.maxNameBytes {
		t.Fatalf("collision pass exceeded budget: result=%+v budget=%+v", meter.usage, budget)
	}
	if got := readQuotaFromRoot(t, root); got != quota {
		t.Fatalf("collision pass changed quota to %+v", got)
	}
}

func requireStorageEntryIncarnation(t *testing.T, directory *storageDir, name string, want recordIncarnation) {
	t.Helper()
	entry, err := directory.lookupEntry(name)
	if err != nil {
		t.Fatalf("lookup %q: %v", name, err)
	}
	got := recordIncarnation{dev: entry.metadata.dev, ino: entry.metadata.ino}
	if got != want {
		t.Fatalf("entry %q incarnation = %+v, want %+v", name, got, want)
	}
}

func requireMissingStorageEntry(t *testing.T, directory *storageDir, name string) {
	t.Helper()
	if _, err := directory.lookupEntry(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("entry %q remains: %v", name, err)
	}
}

func TestMalformedSubtreeQuarantineRejectsEnumeratedInodeReplacement(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	tree, err := root.openDir([]string{queueDirectoryName}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	badPath := filepath.Join(home.Root(), queueDirectoryName, "bad")
	if err := os.Mkdir(badPath, 0o700); err != nil {
		t.Fatal(err)
	}
	iterator, err := tree.iterateEntries()
	if err != nil {
		t.Fatal(err)
	}
	entry, err := iterator.Next()
	if err != nil {
		t.Fatal(err)
	}
	_ = iterator.Close()
	if err := os.Rename(badPath, badPath+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(badPath, 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := tree.renameEnumeratedDirectory(entry, tree, ".orphan-test")
	if !errors.Is(err, errStorageEntryChanged) || result.state != storageRenameNotApplied {
		t.Fatalf("replacement rename = (%v, %v)", result.state, err)
	}
	if _, err := os.Stat(badPath); err != nil {
		t.Fatalf("replacement directory was touched: %v", err)
	}
}

func TestMalformedSubtreeQuarantineReportsAppliedButUnsyncedRename(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	plainRoot := mustOpenMutableRoot(t, home)
	plainTree, err := plainRoot.openDir([]string{queueDirectoryName}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home.Root(), queueDirectoryName, "bad"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = plainTree.Close()
	_ = plainRoot.Close()

	renameStarted := false
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepRename {
			renameStarted = true
		}
		if step == storageStepDirectorySync && renameStarted {
			return errors.New("injected quarantine parent sync failure")
		}
		return nil
	}}
	root, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	tree, err := root.openDir([]string{queueDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	iterator, err := tree.iterateEntries()
	if err != nil {
		t.Fatal(err)
	}
	entry, err := iterator.Next()
	if err != nil {
		t.Fatal(err)
	}
	_ = iterator.Close()
	result, err := tree.renameEnumeratedDirectory(entry, tree, ".orphan-test")
	if err == nil || result.state != storageRenameAppliedSyncPending {
		t.Fatalf("unsynced rename = (%v, %v)", result.state, err)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), queueDirectoryName, "bad")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("applied rename left source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home.Root(), queueDirectoryName, ".orphan-test")); err != nil {
		t.Fatalf("applied rename lacks target: %v", err)
	}
}

func TestDirectoryExchangeRejectsMutationBoundaryReplacementOfEitherEndpoint(t *testing.T) {
	for _, replacedEndpoint := range []string{"source", "target"} {
		t.Run(replacedEndpoint, func(t *testing.T) {
			var replacementPath, displacedPath string
			replaced := false
			hooks := storageTestHooks{beforeExchange: func() error {
				if replaced {
					return nil
				}
				replaced = true
				if err := os.Rename(replacementPath, displacedPath); err != nil {
					return err
				}
				return os.WriteFile(replacementPath, []byte("replacement"), 0o600)
			}}
			home, sourceParent, targetParent, sourceEntry, targetEntry := newDirectoryExchangeFixture(t, hooks)
			defer func() {
				_ = sourceParent.Close()
				_ = targetParent.Close()
			}()
			if replacedEndpoint == "source" {
				replacementPath = filepath.Join(home.Root(), queueDirectoryName, "generation", "source")
			} else {
				replacementPath = filepath.Join(home.Root(), queueDirectoryName, "target")
			}
			displacedPath = replacementPath + "-enumerated"
			result, exchangeErr := sourceParent.exchangeEnumeratedEntries(sourceEntry, targetParent, targetEntry)
			replacement, replacementErr := os.ReadFile(replacementPath)
			displaced, displacedErr := os.Stat(displacedPath)
			if !replaced || !errors.Is(exchangeErr, errStorageEntryChanged) || result.state != storageRenameNotApplied ||
				replacementErr != nil || string(replacement) != "replacement" || displacedErr != nil || !displaced.IsDir() {
				t.Fatalf("exchange %s boundary replacement = replaced:%v state:%v err:%v replacement:%q/%v displaced:%v/%v",
					replacedEndpoint, replaced, result.state, exchangeErr, replacement, replacementErr, displaced, displacedErr)
			}
		})
	}
}

func TestDirectoryExchangeMandatoryPostcheckRejectsEitherEndpointReplacement(t *testing.T) {
	for _, replacedEndpoint := range []string{"source", "target"} {
		t.Run(replacedEndpoint, func(t *testing.T) {
			var replacementPath, displacedPath string
			replaced := false
			hooks := storageTestHooks{afterExchange: func() error {
				if replaced {
					return nil
				}
				replaced = true
				if err := os.Rename(replacementPath, displacedPath); err != nil {
					return err
				}
				return os.WriteFile(replacementPath, []byte("replacement"), 0o600)
			}}
			home, sourceParent, targetParent, sourceEntry, targetEntry := newDirectoryExchangeFixture(t, hooks)
			defer func() {
				_ = sourceParent.Close()
				_ = targetParent.Close()
			}()
			if replacedEndpoint == "source" {
				replacementPath = filepath.Join(home.Root(), queueDirectoryName, "generation", "source")
			} else {
				replacementPath = filepath.Join(home.Root(), queueDirectoryName, "target")
			}
			displacedPath = replacementPath + "-swapped"
			result, exchangeErr := sourceParent.exchangeEnumeratedEntries(sourceEntry, targetParent, targetEntry)
			replacement, replacementErr := os.ReadFile(replacementPath)
			displaced, displacedErr := os.Lstat(displacedPath)
			if !replaced || !errors.Is(exchangeErr, errStorageEntryChanged) || result.state != storageRenameAppliedSyncPending ||
				replacementErr != nil || string(replacement) != "replacement" || displacedErr != nil || !displaced.IsDir() {
				t.Fatalf("exchange %s postcheck replacement = replaced:%v state:%v err:%v replacement:%q/%v displaced:%v/%v",
					replacedEndpoint, replaced, result.state, exchangeErr, replacement, replacementErr, displaced, displacedErr)
			}
		})
	}
}

func TestDirectoryExchangePostApplicationErrorsAreAlwaysAppliedSyncPending(t *testing.T) {
	for _, direction := range []string{"forward", "reverse"} {
		for _, injected := range []error{unix.EIO, unix.EINVAL, unix.ENOSYS, unix.EXDEV} {
			t.Run(direction+" "+injected.Error(), func(t *testing.T) {
				afterCalls := 0
				renameAttempts := 0
				parentSyncs := 0
				hooks := storageTestHooks{
					afterExchange: func() error {
						afterCalls++
						return injected
					},
					beforeStep: func(step storageStep) error {
						switch step {
						case storageStepRename:
							renameAttempts++
						case storageStepDirectorySync:
							parentSyncs++
						}
						return nil
					},
				}
				_, sourceParent, targetParent, sourceEntry, targetEntry := newDirectoryExchangeFixture(t, hooks)
				defer func() {
					_ = sourceParent.Close()
					_ = targetParent.Close()
				}()
				afterCalls = 0
				renameAttempts = 0
				parentSyncs = 0
				caller, source, target, targetDirectory := sourceParent, sourceEntry, targetEntry, targetParent
				if direction == "reverse" {
					caller, source, target, targetDirectory = targetParent, targetEntry, sourceEntry, sourceParent
				}
				result, exchangeErr := caller.exchangeEnumeratedEntries(source, targetDirectory, target)
				if !errors.Is(exchangeErr, injected) || result.state != storageRenameAppliedSyncPending ||
					afterCalls != 1 || renameAttempts != 1 || parentSyncs != 2 {
					t.Fatalf("post-application %s %v = state:%v err:%v after:%d renames:%d syncs:%d",
						direction, injected, result.state, exchangeErr, afterCalls, renameAttempts, parentSyncs)
				}
				requireStorageEntryIncarnation(t, targetParent, targetEntry.name,
					recordIncarnation{dev: sourceEntry.metadata.dev, ino: sourceEntry.metadata.ino})
				requireStorageEntryIncarnation(t, sourceParent, sourceEntry.name,
					recordIncarnation{dev: targetEntry.metadata.dev, ino: targetEntry.metadata.ino})
			})
		}
	}
}

func TestDirectoryExchangeRevalidatesBothEntriesAndReportsSyncUncertainty(t *testing.T) {
	for _, replaced := range []string{"source", "target"} {
		for _, replacement := range []string{"file", "symlink", "fifo"} {
			t.Run("rejects "+replacement+" replacement of "+replaced, func(t *testing.T) {
				home, sourceParent, targetParent, sourceEntry, targetEntry := newDirectoryExchangeFixture(t, storageTestHooks{})
				defer func() {
					_ = sourceParent.Close()
					_ = targetParent.Close()
				}()
				var path string
				if replaced == "source" {
					path = filepath.Join(home.Root(), queueDirectoryName, "generation", "source")
				} else {
					path = filepath.Join(home.Root(), queueDirectoryName, "target")
				}
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				var wantType fs.FileMode
				switch replacement {
				case "file":
					if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					wantType = os.ModeSymlink
					if err := os.Symlink(t.TempDir(), path); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					wantType = os.ModeNamedPipe
					if err := unix.Mkfifo(path, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				result, err := sourceParent.exchangeEnumeratedEntries(sourceEntry, targetParent, targetEntry)
				if !errors.Is(err, errStorageEntryChanged) || result.state != storageRenameNotApplied {
					t.Fatalf("replacement exchange = (%v, %v)", result.state, err)
				}
				info, err := os.Lstat(path)
				if err != nil || info.Mode().Type() != wantType {
					t.Fatalf("replacement was changed: mode=%v err=%v", infoMode(info), err)
				}
			})
		}
	}

	for _, failSync := range []int{1, 2} {
		t.Run(fmt.Sprintf("applied but parent sync %d uncertain", failSync), func(t *testing.T) {
			exchanged := false
			syncCalls := 0
			hooks := storageTestHooks{beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					exchanged = true
				}
				if exchanged && step == storageStepDirectorySync {
					syncCalls++
					if syncCalls == failSync {
						return errors.New("injected exchange parent sync failure")
					}
				}
				return nil
			}}
			home, sourceParent, targetParent, sourceEntry, targetEntry := newDirectoryExchangeFixture(t, hooks)
			result, err := sourceParent.exchangeEnumeratedEntries(sourceEntry, targetParent, targetEntry)
			if err == nil || result.state != storageRenameAppliedSyncPending {
				t.Fatalf("uncertain exchange = (%v, %v)", result.state, err)
			}
			if syncCalls != 2 {
				t.Fatalf("exchange parent sync calls = %d, want 2", syncCalls)
			}
			requireStorageEntryIncarnation(t, targetParent, targetEntry.name,
				recordIncarnation{dev: sourceEntry.metadata.dev, ino: sourceEntry.metadata.ino})
			requireStorageEntryIncarnation(t, sourceParent, sourceEntry.name,
				recordIncarnation{dev: targetEntry.metadata.dev, ino: targetEntry.metadata.ino})
			_ = sourceParent.Close()
			_ = targetParent.Close()
			reopened := mustOpenMutableRoot(t, home)
			defer func() { _ = reopened.Close() }()
			purge := spoolSweepResult{}
			for attempts := 0; attempts < 8 && !purge.complete; attempts++ {
				purge, err = purgeSpool(reopened, defaultSpoolWorkBudget())
				if err != nil {
					t.Fatal(err)
				}
			}
			if !purge.complete {
				t.Fatal("reopened cleanup did not converge after uncertain exchange sync")
			}
		})
	}

	t.Run("pre-syscall EINTR retries only after unchanged proof", func(t *testing.T) {
		attempts := 0
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if step == storageStepRename {
				attempts++
				if attempts == 1 {
					return unix.EINTR
				}
			}
			return nil
		}}
		_, sourceParent, targetParent, sourceEntry, targetEntry := newDirectoryExchangeFixture(t, hooks)
		result, err := sourceParent.exchangeEnumeratedEntries(sourceEntry, targetParent, targetEntry)
		if err != nil || result.state != storageRenameAppliedDurable || attempts != 2 {
			t.Fatalf("pre-syscall EINTR exchange = (%v, %v), attempts=%d", result.state, err, attempts)
		}
	})

	t.Run("applied then EINTR does not exchange twice", func(t *testing.T) {
		postCalls := 0
		hooks := storageTestHooks{afterExchange: func() error {
			postCalls++
			if postCalls == 1 {
				return unix.EINTR
			}
			return nil
		}}
		_, sourceParent, targetParent, sourceEntry, targetEntry := newDirectoryExchangeFixture(t, hooks)
		result, err := sourceParent.exchangeEnumeratedEntries(sourceEntry, targetParent, targetEntry)
		if err != nil || result.state != storageRenameAppliedDurable || postCalls != 1 {
			t.Fatalf("post-application EINTR exchange = (%v, %v), postCalls=%d", result.state, err, postCalls)
		}
		requireStorageEntryIncarnation(t, targetParent, targetEntry.name,
			recordIncarnation{dev: sourceEntry.metadata.dev, ino: sourceEntry.metadata.ino})
		requireStorageEntryIncarnation(t, sourceParent, sourceEntry.name,
			recordIncarnation{dev: targetEntry.metadata.dev, ino: targetEntry.metadata.ino})
	})

	t.Run("ambiguous EINTR syncs both parents", func(t *testing.T) {
		var targetPath string
		syncCalls := 0
		var injectedErr error
		hooks := storageTestHooks{
			afterExchange: func() error {
				moved := targetPath + ".moved"
				if err := os.Rename(targetPath, moved); err != nil {
					injectedErr = err
					return err
				}
				if err := os.Mkdir(targetPath, 0o700); err != nil {
					injectedErr = err
					return err
				}
				if err := os.WriteFile(filepath.Join(targetPath, "payload"), []byte("replacement"), 0o600); err != nil {
					injectedErr = err
					return err
				}
				return unix.EINTR
			},
			beforeStep: func(step storageStep) error {
				if step == storageStepDirectorySync {
					syncCalls++
				}
				return nil
			},
		}
		home, sourceParent, targetParent, sourceEntry, targetEntry := newDirectoryExchangeFixture(t, hooks)
		targetPath = filepath.Join(home.Root(), queueDirectoryName, "target")
		syncCalls = 0
		result, err := sourceParent.exchangeEnumeratedEntries(sourceEntry, targetParent, targetEntry)
		if injectedErr != nil || err == nil || result.state != storageRenameAppliedSyncPending || syncCalls != 2 {
			t.Fatalf("ambiguous exchange EINTR = result:%v err:%v injected:%v syncs:%d",
				result.state, err, injectedErr, syncCalls)
		}
		if data, readErr := os.ReadFile(filepath.Join(targetPath, "payload")); readErr != nil || string(data) != "replacement" {
			t.Fatalf("ambiguous exchange changed replacement: data=%q err=%v", data, readErr)
		}
	})

	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "ENOSYS", err: unix.ENOSYS},
		{name: "ENOTSUP", err: unix.ENOTSUP},
		{name: "EOPNOTSUPP", err: unix.EOPNOTSUPP},
		{name: "EINVAL", err: unix.EINVAL},
		{name: "EXDEV", err: unix.EXDEV},
	} {
		t.Run("unsupported "+test.name+" is typed not-applied", func(t *testing.T) {
			hooks := storageTestHooks{beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					return test.err
				}
				return nil
			}}
			_, sourceParent, targetParent, sourceEntry, targetEntry := newDirectoryExchangeFixture(t, hooks)
			result, err := sourceParent.exchangeEnumeratedEntries(sourceEntry, targetParent, targetEntry)
			if !errors.Is(err, errStorageExchangeUnsupported) || result.state != storageRenameNotApplied {
				t.Fatalf("unsupported exchange = (%v, %v)", result.state, err)
			}
			requireStorageEntryIncarnation(t, sourceParent, sourceEntry.name,
				recordIncarnation{dev: sourceEntry.metadata.dev, ino: sourceEntry.metadata.ino})
			requireStorageEntryIncarnation(t, targetParent, targetEntry.name,
				recordIncarnation{dev: targetEntry.metadata.dev, ino: targetEntry.metadata.ino})
		})
	}
}

func TestEntryExchangeAtomicallySwapsMixedKinds(t *testing.T) {
	postCalls := 0
	hooks := storageTestHooks{afterExchange: func() error {
		postCalls++
		return unix.EINTR
	}}
	home, sourceParent, targetParent, _, targetEntry := newDirectoryExchangeFixture(t, hooks)
	sourcePath := filepath.Join(home.Root(), queueDirectoryName, "generation", "source")
	if err := os.RemoveAll(sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("mixed-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceEntry, err := sourceParent.lookupEntry("source")
	if err != nil {
		t.Fatal(err)
	}
	result, exchangeErr := sourceParent.exchangeEnumeratedEntries(sourceEntry, targetParent, targetEntry)
	if exchangeErr != nil || result.state != storageRenameAppliedDurable || postCalls != 1 {
		t.Fatalf("mixed-kind exchange = result:%v err:%v postCalls:%d", result.state, exchangeErr, postCalls)
	}
	requireStorageEntryIncarnation(t, targetParent, targetEntry.name,
		recordIncarnation{dev: sourceEntry.metadata.dev, ino: sourceEntry.metadata.ino})
	requireStorageEntryIncarnation(t, sourceParent, sourceEntry.name,
		recordIncarnation{dev: targetEntry.metadata.dev, ino: targetEntry.metadata.ino})
	data, readErr := os.ReadFile(filepath.Join(home.Root(), queueDirectoryName, "target"))
	sourceInfo, sourceErr := os.Lstat(sourcePath)
	if readErr != nil || string(data) != "mixed-file" || sourceErr != nil || !sourceInfo.IsDir() {
		t.Fatalf("mixed-kind exchange contents = data:%q readErr:%v source:%v sourceErr:%v",
			data, readErr, sourceInfo, sourceErr)
	}
}

func TestDirectoryExchangeSameParentSyncsOnceAndRejectsSameEntry(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	treePath := filepath.Join(home.Root(), queueDirectoryName)
	for _, name := range []string{"source", "target"} {
		path := filepath.Join(treePath, name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "payload"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exchanged := false
	syncCalls := 0
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepRename {
			exchanged = true
		}
		if exchanged && step == storageStepDirectorySync {
			syncCalls++
		}
		return nil
	}}
	root, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	tree, err := root.openDir([]string{queueDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	source, err := tree.lookupEntry("source")
	if err != nil {
		t.Fatal(err)
	}
	target, err := tree.lookupEntry("target")
	if err != nil {
		t.Fatal(err)
	}
	result, err := tree.exchangeEnumeratedEntries(source, tree, target)
	if err != nil || result.state != storageRenameAppliedDurable || syncCalls != 1 {
		t.Fatalf("same-parent exchange = (%v, %v), syncCalls=%d", result.state, err, syncCalls)
	}
	requireStorageEntryIncarnation(t, tree, target.name,
		recordIncarnation{dev: source.metadata.dev, ino: source.metadata.ino})

	current, err := tree.lookupEntry("source")
	if err != nil {
		t.Fatal(err)
	}
	result, err = tree.exchangeEnumeratedEntries(current, tree, current)
	if err == nil || result.state != storageRenameNotApplied {
		t.Fatalf("same-entry exchange = (%v, %v)", result.state, err)
	}
	requireStorageEntryIncarnation(t, tree, current.name,
		recordIncarnation{dev: current.metadata.dev, ino: current.metadata.ino})
}

func infoMode(info fs.FileInfo) fs.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode()
}

func newDirectoryExchangeFixture(t *testing.T, hooks storageTestHooks) (gchome.ProductUsageHome, *storageDir, *storageDir, storageEntry, storageEntry) {
	t.Helper()
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	sourcePath := filepath.Join(home.Root(), queueDirectoryName, "generation", "source")
	targetPath := filepath.Join(home.Root(), queueDirectoryName, "target")
	if err := os.MkdirAll(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(targetPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sourcePath, targetPath} {
		if err := os.WriteFile(filepath.Join(path, "payload"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tree, err := root.openDir([]string{queueDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	sourceParent, err := tree.openDir([]string{"generation"}, false)
	if err != nil {
		t.Fatal(err)
	}
	sourceEntry, err := sourceParent.lookupEntry("source")
	if err != nil {
		t.Fatal(err)
	}
	targetEntry, err := tree.lookupEntry("target")
	if err != nil {
		t.Fatal(err)
	}
	return home, sourceParent, tree, sourceEntry, targetEntry
}

func TestEventRetentionArithmeticFailsClosedAtBoundaries(t *testing.T) {
	now := testRecordHour
	if !eventWithinRetention(now.Add(-maximumEventAgeHours*time.Hour), now) {
		t.Fatal("exact seven-day boundary rejected")
	}
	for _, occurred := range []time.Time{
		now.Add(-(maximumEventAgeHours*time.Hour + time.Hour)),
		now.Add(time.Hour),
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC),
	} {
		if eventWithinRetention(occurred, now) {
			t.Errorf("out-of-window occurrence %v accepted", occurred)
		}
	}
}

func TestReconcileIncompleteMutationRetainsFailClosedQuotaEvidence(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	nonCurrentGeneration := "99999999-9999-4999-8999-999999999999"
	nested := filepath.Join(home.Root(), queueDirectoryName, nonCurrentGeneration, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(nested, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plainRoot := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(plainRoot, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	physicalOpens := 0
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		afterDirectoryOpen: func(string) { physicalOpens++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	physicalOpens = 0
	budget := defaultSpoolWorkBudget()
	budget.maxDirectories = 6
	result, reconcileErr := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, budget)
	quota, quotaPresent, quotaErr := loadSpoolQuota(root)
	_, activeErr := root.lookupEntry(spoolControlDirectoryName)
	_, retiredErr := root.lookupEntry(retiredControlDirectoryName)
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	failClosedEvidence := quotaErr == nil && quotaPresent && quota == markers ||
		activeErr == nil || retiredErr == nil
	if physicalOpens > int(budget.maxDirectories) {
		t.Fatalf("physical directory opens = %d, budget = %d", physicalOpens, budget.maxDirectories)
	}
	retainedSibling := false
	if err := filepath.WalkDir(home.Root(), func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == "first" || entry.Name() == "second" {
			retainedSibling = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !retainedSibling {
		t.Fatal("reconcile fixture did not retain an unvisited sibling")
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := mustOpenMutableRoot(t, home)
	reopenedQuota, reopenedPresent, reopenedQuotaErr := loadSpoolQuota(reopened)
	_, reopenedActiveErr := reopened.lookupEntry(spoolControlDirectoryName)
	_, reopenedRetiredErr := reopened.lookupEntry(retiredControlDirectoryName)
	reopenedEvidence := reopenedQuotaErr == nil && reopenedPresent && reopenedQuota == markers ||
		reopenedActiveErr == nil || reopenedRetiredErr == nil
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	recordResult := service.RecordOnce(permit, CommandHelp)
	eventPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(testEventIDThree))
	_, eventErr := os.Lstat(eventPath)
	if reconcileErr != nil || result.complete || !failClosedEvidence || !reopenedEvidence ||
		recordResult != RecordDropped || !errors.Is(eventErr, fs.ErrNotExist) {
		t.Fatalf("incomplete reconcile lost fail-closed evidence: complete=%v err=%v quota=%+v present=%v quotaErr=%v activeErr=%v retiredErr=%v reopenedQuota=%+v reopenedPresent=%v reopenedQuotaErr=%v reopenedActiveErr=%v reopenedRetiredErr=%v opens=%d subsequentRecord=%v eventErr=%v",
			result.complete, reconcileErr, quota, quotaPresent, quotaErr, activeErr, retiredErr,
			reopenedQuota, reopenedPresent, reopenedQuotaErr, reopenedActiveErr, reopenedRetiredErr,
			physicalOpens, recordResult, eventErr)
	}
}

func TestRecordOnceDecisionExpiryStopsTempCollisionRetries(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	current := testRecordHour
	service.deps.now = func() time.Time { return current }
	generationPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
	seeded := false
	tempAttempts := 0
	service.deps.storageHooks.beforeTempFileCreate = func(path string) {
		if filepath.Dir(path) != generationPath {
			return
		}
		tempAttempts++
		if seeded {
			return
		}
		seeded = true
		firstSequence := storageTempSequence.Load()
		for offset := uint64(0); offset < maximumStorageTempAttempts-1; offset++ {
			name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), firstSequence+offset)
			if err := os.WriteFile(filepath.Join(generationPath, name), []byte("collision"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
	}

	result := service.RecordOnce(permit, CommandHelp)
	eventPath := filepath.Join(generationPath, eventFileName(testEventIDOne))
	_, eventErr := os.Lstat(eventPath)
	if result != RecordDropped || !errors.Is(eventErr, fs.ErrNotExist) || tempAttempts != 1 {
		t.Fatalf("expired temp retry continued foreground work: result=%v eventErr=%v attempts=%d", result, eventErr, tempAttempts)
	}
	quota := readQuotaFixture(t, home)
	if quota.Events != 1 || quota.Bytes == 0 {
		t.Fatalf("expired post-reservation attempt lost conservative quota: %+v", quota)
	}
}

func TestCleanupOnlyCurrentTreeNeverProducesRecords(t *testing.T) {
	for _, treeName := range []string{queueDirectoryName, inflightDirectoryName} {
		for _, laxAt := range []string{"tree", "generation"} {
			t.Run(treeName+"/"+laxAt, func(t *testing.T) {
				home, _, permit := newRecordServiceFixture(t, testEventIDThree)
				plainRoot := mustOpenMutableRoot(t, home)
				event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
				eventBytes := writeSpoolEventFixture(t, plainRoot, treeName, testSpoolGeneration, event)
				if err := persistSpoolQuota(plainRoot, spoolQuota{Events: 1, Bytes: uint64(len(eventBytes))}); err != nil {
					t.Fatal(err)
				}
				if err := plainRoot.Close(); err != nil {
					t.Fatal(err)
				}
				taintedPath := filepath.Join(home.Root(), treeName)
				if laxAt == "generation" {
					taintedPath = filepath.Join(taintedPath, testSpoolGeneration)
				}
				if err := os.Chmod(taintedPath, 0o755); err != nil {
					t.Fatal(err)
				}

				eventOpens := 0
				eventRenames := 0
				root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
					afterFileOpen: func(path string) {
						if filepath.Ext(path) == eventFileSuffix {
							eventOpens++
						}
					},
					afterRename: func(source, target string, state storageRenameState) {
						if state != storageRenameNotApplied &&
							(filepath.Ext(source) == eventFileSuffix || filepath.Ext(target) == eventFileSuffix) {
							eventRenames++
						}
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()
				result := spoolSweepResult{}
				for attempts := 0; attempts < 16 && !result.complete; attempts++ {
					state := runSpoolSweep(root, policyFromPermit(permit), testRecordHour, defaultSpoolWorkBudget(), false)
					if len(state.records) != 0 {
						t.Fatalf("cleanup-only pass retained %d upload records", len(state.records))
					}
					result, err = state.finish()
					if err != nil {
						t.Fatal(err)
					}
				}
				if !result.complete {
					t.Fatalf("cleanup-only tree did not converge: %+v", result)
				}
				claim, err := claimSpoolBatch(root, permit, testRecordHour, defaultSpoolWorkBudget())
				if err != nil {
					t.Fatal(err)
				}
				if len(claim.records) != 0 || eventOpens != 0 || eventRenames != 0 {
					t.Fatalf("cleanup-only data escaped: claim=%d eventOpens=%d eventRenames=%d",
						len(claim.records), eventOpens, eventRenames)
				}
				if quota := readQuotaFromRoot(t, root); quota != (spoolQuota{}) {
					t.Fatalf("cleanup-only converged quota = %+v", quota)
				}
			})
		}
	}
}

func TestCapBoundaryUnsupportedExchangeRetainsRelocationCapacity(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	defer func() { _ = fixture.root.Close() }()
	physicalOpens := 0
	hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	hooks.afterDirectoryOpen = func(string) { physicalOpens++ }
	fixture.reopen(t, hooks)
	physicalOpens = 0
	fixture.namespaceMutations = 0
	budget := defaultSpoolWorkBudget()
	// Model four already-consumed ordinary opens (tree+iterator and
	// generation+iterator), leaving the fifth, fixed slot as the only
	// physical capacity available for durable relocation state.
	budget.maxDirectories = 5
	meter := newSpoolWorkMeter(budget)
	meter.physicalDirectories = true
	meter.usage.directories = meter.ordinaryDirectoryLimit()
	restore := fixture.root.installDirectoryOpenHooks(meter.beforePhysicalDirectoryOpen, meter.afterPhysicalDirectoryOpen)
	entry, err := fixture.generation.lookupEntry(fixture.sourceName)
	if err != nil {
		t.Fatal(err)
	}
	state := &spoolSweepState{root: fixture.root, purgeAll: true, meter: meter}
	state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
	restore()
	passOpens := physicalOpens
	if state.operation != nil || !state.mutated {
		t.Fatalf("cap-boundary fallback = mutated:%v err:%v usage:%+v opens:%d",
			state.mutated, state.operation, meter.usage, passOpens)
	}
	if meter.usage.directories > budget.maxDirectories || passOpens > 1 {
		t.Fatalf("cap-boundary fallback opened %d new directories, usage=%+v budget=%+v", passOpens, meter.usage, budget)
	}
	requireMissingStorageEntry(t, fixture.tree, fixture.sourceCanonical)
	requireStorageEntryIncarnation(t, fixture.tree, relocationCandidateName(0), fixture.blocker)
	if cursor := readRelocationCursorFromRoot(t, fixture.root); cursor.Next != maximumRelocationSlots {
		t.Fatalf("cap-boundary relocation cursor = %+v", cursor)
	}
	if state.retainedControl != nil {
		_ = state.retainedControl.Close()
		state.retainedControl = nil
	}
}

func TestGrowingEventReadChargesMaximumPlusOneAndStopsNextOpen(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	plainRoot := mustOpenMutableRoot(t, home)
	first := testSpoolEvent(testEventIDTwo, "1.0.0", testRecordHour, CommandHelp)
	second := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandVersion)
	firstBytes := writeSpoolEventFixture(t, plainRoot, queueDirectoryName, testSpoolGeneration, first)
	secondBytes := writeSpoolEventFixture(t, plainRoot, queueDirectoryName, testSpoolGeneration, second)
	if err := persistSpoolQuota(plainRoot, spoolQuota{
		Events: 2, Bytes: uint64(len(firstBytes) + len(secondBytes)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(first.EventID))
	secondPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(second.EventID))
	eventBytes := map[string][]byte{firstPath: firstBytes, secondPath: secondBytes}
	eventOpens := map[string]int{firstPath: 0, secondPath: 0}
	grownPath := ""
	type readObservation struct {
		requested int
		read      int
		err       error
	}
	var grownReads []readObservation
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeRead: func(path string) {
			if _, isEvent := eventBytes[path]; !isEvent || grownPath != "" {
				return
			}
			grownPath = path
			// This seam runs after opened-FD and named-entry validation but
			// before the first read syscall. Grow whichever canonical event
			// the filesystem enumerated first, avoiding any Readdir ordering
			// assumption.
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			growth := int(3*maximumEventBytes) - len(eventBytes[path])
			if growth <= 0 {
				t.Fatalf("event fixture is already %d bytes", len(eventBytes[path]))
			}
			_, writeErr := file.Write([]byte(strings.Repeat("x", growth)))
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("grow event after final validation: write=%v close=%v", writeErr, closeErr)
			}
		},
		afterFileOpen: func(path string) {
			if _, isEvent := eventOpens[path]; isEvent {
				eventOpens[path]++
			}
		},
		afterRead: func(path string, requested, read int, err error) {
			if path == grownPath {
				grownReads = append(grownReads, readObservation{requested: requested, read: read, err: err})
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	budget := defaultSpoolWorkBudget()
	budget.maxReadBytes = spoolFixedReadEnvelope + maximumEventBytes + 1
	state := runSpoolSweep(root, testCurrentSpoolPolicy(), testRecordHour, budget, false)
	physicalReadUsage := state.meter.usage.readBytes
	if state.meter.fixedEnvelopeClaimed {
		physicalReadUsage -= spoolFixedReadEnvelope
	}
	records := len(state.records)
	result, sweepErr := state.finish()
	if sweepErr != nil {
		t.Fatal(sweepErr)
	}
	wantReads := []readObservation{{requested: int(maximumEventBytes), read: int(maximumEventBytes)}, {requested: 1, read: 1}}
	readShapeOK := len(grownReads) == len(wantReads)
	if readShapeOK {
		for index := range wantReads {
			if grownReads[index].requested != wantReads[index].requested || grownReads[index].read != wantReads[index].read ||
				grownReads[index].err != nil {
				readShapeOK = false
			}
		}
	}
	totalEventOpens := eventOpens[firstPath] + eventOpens[secondPath]
	if grownPath == "" || physicalReadUsage != maximumEventBytes+1 || totalEventOpens != 1 || eventOpens[grownPath] != 1 || records != 0 || !readShapeOK {
		t.Fatalf("growing read accounting = grown:%q usage:%d opens:%v records:%d reads:%+v result:%+v",
			grownPath, physicalReadUsage, eventOpens, records, grownReads, result)
	}
	if physicalReadUsage > budget.maxReadBytes-spoolFixedReadEnvelope {
		t.Fatalf("physical event reads exceeded ordinary budget: usage=%d budget=%d",
			physicalReadUsage, budget.maxReadBytes-spoolFixedReadEnvelope)
	}
}

func TestNoReplaceAppliedThenEINTRClassifiesWithoutRetry(t *testing.T) {
	t.Run("rename", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		sourcePath := filepath.Join(inspection.Root(), "event.json")
		targetPath := filepath.Join(inspection.Root(), inflightDirectoryName, "event.json")
		armed := false
		attempts := 0
		syncs := 0
		var injectedErr error
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if armed && step == storageStepRename {
				attempts++
				if attempts == 1 {
					injectedErr = os.Rename(sourcePath, targetPath)
					if injectedErr != nil {
						return injectedErr
					}
					return unix.EINTR
				}
			}
			if armed && step == storageStepDirectorySync {
				syncs++
			}
			return nil
		}}
		root, target := openRenameTestDirectories(t, inspection, hooks)
		if err := root.writeFileAtomic("event.json", []byte("source")); err != nil {
			t.Fatal(err)
		}
		sourceBefore, err := os.Stat(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		armed = true
		result, renameErr := root.renameFile("event.json", target, "event.json")
		targetAfter, targetErr := os.Stat(targetPath)
		_, sourceErr := os.Lstat(sourcePath)
		if injectedErr != nil || renameErr != nil || result.state != storageRenameAppliedDurable || attempts != 1 || syncs != 2 ||
			targetErr != nil || !os.SameFile(sourceBefore, targetAfter) || !errors.Is(sourceErr, fs.ErrNotExist) {
			t.Fatalf("applied-then-EINTR rename = result:%v err:%v injected:%v attempts:%d syncs:%d targetErr:%v sourceErr:%v",
				result.state, renameErr, injectedErr, attempts, syncs, targetErr, sourceErr)
		}
	})

	t.Run("atomic no-replace install", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		targetPath := filepath.Join(inspection.Root(), "event.json")
		var tempPath string
		var tempBefore os.FileInfo
		attempts := 0
		syncs := 0
		var injectedErr error
		hooks := storageTestHooks{
			beforeTempFileCreate: func(path string) { tempPath = path },
			beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					attempts++
					if attempts == 1 {
						var err error
						tempBefore, err = os.Stat(tempPath)
						if err != nil {
							injectedErr = err
							return err
						}
						injectedErr = os.Rename(tempPath, targetPath)
						if injectedErr != nil {
							return injectedErr
						}
						return unix.EINTR
					}
				}
				if attempts > 0 && step == storageStepDirectorySync {
					syncs++
				}
				return nil
			},
		}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		writeErr := root.writeFileAtomicNoReplace("event.json", []byte("source"))
		targetAfter, targetErr := os.Stat(targetPath)
		_, tempErr := os.Lstat(tempPath)
		entry, entryErr := root.lookupEntry("event.json")
		if injectedErr != nil || writeErr != nil || attempts != 1 || syncs != 4 || targetErr != nil ||
			tempBefore == nil || !os.SameFile(tempBefore, targetAfter) || !errors.Is(tempErr, fs.ErrNotExist) ||
			entryErr != nil || entry.metadata.nlink != 1 {
			t.Fatalf("applied-then-EINTR no-replace rename = err:%v injected:%v attempts:%d syncs:%d targetErr:%v tempErr:%v entry:%+v entryErr:%v",
				writeErr, injectedErr, attempts, syncs, targetErr, tempErr, entry.metadata, entryErr)
		}
	})

	t.Run("unchanged rename retries while budget remains", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		armed := false
		attempts := 0
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if armed && step == storageStepRename {
				attempts++
				if attempts == 1 {
					return unix.EINTR
				}
			}
			return nil
		}}
		root, target := openRenameTestDirectories(t, inspection, hooks)
		if err := root.writeFileAtomic("event.json", []byte("source")); err != nil {
			t.Fatal(err)
		}
		armed = true
		result, err := root.renameFile("event.json", target, "event.json")
		if err != nil || result.state != storageRenameAppliedDurable || attempts != 2 {
			t.Fatalf("unchanged EINTR rename retry = (%v, %v), attempts=%d", result.state, err, attempts)
		}
	})

	t.Run("unchanged no-replace retries while budget remains", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		attempts := 0
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if step == storageStepRename {
				attempts++
				if attempts == 1 {
					return unix.EINTR
				}
			}
			return nil
		}}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := root.writeFileAtomicNoReplace("event.json", []byte("source")); err != nil || attempts != 2 {
			t.Fatalf("unchanged EINTR no-replace retry = err:%v attempts:%d", err, attempts)
		}
	})

	t.Run("ambiguous rename syncs both parents", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		sourcePath := filepath.Join(inspection.Root(), "event.json")
		targetPath := filepath.Join(inspection.Root(), inflightDirectoryName, "event.json")
		armed := false
		attempts := 0
		syncs := 0
		var injectedErr error
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if armed && step == storageStepRename {
				attempts++
				if attempts == 1 {
					replacementPath := targetPath + ".replacement"
					if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
						injectedErr = err
						return err
					}
					if err := os.Rename(sourcePath, targetPath); err != nil {
						injectedErr = err
						return err
					}
					injectedErr = os.Rename(replacementPath, targetPath)
					if injectedErr != nil {
						return injectedErr
					}
					return unix.EINTR
				}
			}
			if armed && step == storageStepDirectorySync {
				syncs++
			}
			return nil
		}}
		root, target := openRenameTestDirectories(t, inspection, hooks)
		if err := root.writeFileAtomic("event.json", []byte("source")); err != nil {
			t.Fatal(err)
		}
		armed = true
		result, err := root.renameFile("event.json", target, "event.json")
		if injectedErr != nil || err == nil || result.state != storageRenameAppliedSyncPending || attempts != 1 || syncs != 2 {
			t.Fatalf("ambiguous EINTR rename = (%v, %v), injected=%v attempts=%d syncs=%d",
				result.state, err, injectedErr, attempts, syncs)
		}
		if data, err := os.ReadFile(targetPath); err != nil || string(data) != "replacement" {
			t.Fatalf("ambiguous rename changed replacement: data=%q err=%v", data, err)
		}
	})

	t.Run("ambiguous no-replace syncs parent", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		targetPath := filepath.Join(inspection.Root(), "event.json")
		var tempPath string
		attempts := 0
		syncs := 0
		var injectedErr error
		hooks := storageTestHooks{
			beforeTempFileCreate: func(path string) { tempPath = path },
			beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					attempts++
					if attempts == 1 {
						replacementPath := targetPath + ".replacement"
						if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
							injectedErr = err
							return err
						}
						if err := os.Rename(tempPath, targetPath); err != nil {
							injectedErr = err
							return err
						}
						injectedErr = os.Rename(replacementPath, targetPath)
						if injectedErr != nil {
							return injectedErr
						}
						return unix.EINTR
					}
				}
				if attempts > 0 && step == storageStepDirectorySync {
					syncs++
				}
				return nil
			},
		}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		backend, ok := root.backend.(*unixStorageDirectory)
		if !ok {
			t.Fatalf("storage backend = %T", root.backend)
		}
		result, writeErr := backend.writeFileAtomically("event.json", []byte("source"), true)
		if injectedErr != nil || writeErr == nil || result.state != storageWriteAppliedSyncPending || attempts != 1 || syncs == 0 {
			t.Fatalf("ambiguous EINTR no-replace rename = (%v, %v), injected=%v attempts=%d syncs=%d",
				result.state, writeErr, injectedErr, attempts, syncs)
		}
		if data, err := os.ReadFile(targetPath); err != nil || string(data) != "replacement" {
			t.Fatalf("ambiguous no-replace changed replacement: data=%q err=%v", data, err)
		}
	})

	t.Run("ambiguous no-replace syncs parent when temp is absent", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		targetPath := filepath.Join(inspection.Root(), "event.json")
		var tempPath string
		attempts := 0
		syncs := 0
		var injectedErr error
		hooks := storageTestHooks{
			beforeTempFileCreate: func(path string) { tempPath = path },
			beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					attempts++
					if attempts == 1 {
						replacementPath := targetPath + ".replacement"
						if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
							injectedErr = err
							return err
						}
						if err := os.Rename(tempPath, targetPath); err != nil {
							injectedErr = err
							return err
						}
						injectedErr = os.Rename(replacementPath, targetPath)
						if injectedErr != nil {
							return injectedErr
						}
						return unix.EINTR
					}
				}
				if attempts > 0 && step == storageStepDirectorySync {
					syncs++
				}
				return nil
			},
		}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		backend, ok := root.backend.(*unixStorageDirectory)
		if !ok {
			t.Fatalf("storage backend = %T", root.backend)
		}
		result, writeErr := backend.writeFileAtomically("event.json", []byte("source"), true)
		if injectedErr != nil || writeErr == nil || result.state != storageWriteAppliedSyncPending || attempts != 1 || syncs != 1 {
			t.Fatalf("ambiguous absent-temp no-replace rename = result:%v err:%v injected:%v attempts:%d syncs:%d",
				result.state, writeErr, injectedErr, attempts, syncs)
		}
		if data, err := os.ReadFile(targetPath); err != nil || string(data) != "replacement" {
			t.Fatalf("ambiguous absent-temp no-replace changed replacement: data=%q err=%v", data, err)
		}
	})
}

func TestStorageReplaceRenameDecisionGateCoversEveryCallsite(t *testing.T) {
	t.Run("replaceFile", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		sourcePath := filepath.Join(inspection.Root(), "source")
		targetPath := filepath.Join(inspection.Root(), inflightDirectoryName, "target")
		if err := os.Mkdir(filepath.Dir(targetPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(targetPath, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		sourceBefore, err := os.Lstat(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		targetBefore, err := os.Lstat(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		allowed := true
		armed := false
		preMutation := 0
		hooks := storageTestHooks{
			decisionGate: func() bool { return allowed },
			beforeMutation: func(step storageStep, _ string) {
				if armed && step == storageStepRename {
					preMutation++
					allowed = false
				}
			},
		}
		root, target := openRenameTestDirectories(t, inspection, hooks)
		armed = true
		result, replaceErr := root.replaceFile("source", target, "target")
		sourceAfter, sourceErr := os.Lstat(sourcePath)
		targetAfter, targetErr := os.Lstat(targetPath)
		targetData, readErr := os.ReadFile(targetPath)
		if !errors.Is(replaceErr, errRecordDecisionWindowExpired) || result.state != storageRenameNotApplied ||
			preMutation != 1 || sourceErr != nil || targetErr != nil || readErr != nil ||
			!os.SameFile(sourceBefore, sourceAfter) || !os.SameFile(targetBefore, targetAfter) || string(targetData) != "target" {
			t.Fatalf("expired replaceFile = result:%v err:%v preMutation:%d sourceErr:%v targetErr:%v readErr:%v targetData:%q",
				result.state, replaceErr, preMutation, sourceErr, targetErr, readErr, targetData)
		}
	})

	t.Run("atomic replace", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		targetPath := filepath.Join(inspection.Root(), "target")
		if err := os.WriteFile(targetPath, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		targetBefore, err := os.Lstat(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		allowed := true
		preMutation := 0
		hooks := storageTestHooks{
			decisionGate: func() bool { return allowed },
			beforeMutation: func(step storageStep, _ string) {
				if step == storageStepRename {
					preMutation++
					allowed = false
				}
			},
		}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		result, writeErr := root.writeFileAtomicOutcome("target", []byte("replacement"))
		targetAfter, targetErr := os.Lstat(targetPath)
		targetData, readErr := os.ReadFile(targetPath)
		if !errors.Is(writeErr, errRecordDecisionWindowExpired) || result.state != storageWriteNotApplied ||
			preMutation != 1 || targetErr != nil || readErr != nil || !os.SameFile(targetBefore, targetAfter) || string(targetData) != "target" {
			t.Fatalf("expired atomic replace = result:%v err:%v preMutation:%d targetErr:%v readErr:%v targetData:%q",
				result.state, writeErr, preMutation, targetErr, readErr, targetData)
		}
	})

	t.Run("replaceFile unchanged EINTR expires before retry", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		sourcePath := filepath.Join(inspection.Root(), "source")
		targetPath := filepath.Join(inspection.Root(), inflightDirectoryName, "target")
		if err := os.Mkdir(filepath.Dir(targetPath), 0o700); err != nil {
			t.Fatal(err)
		}
		for path, data := range map[string]string{sourcePath: "source", targetPath: "target"} {
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		allowed := true
		attempts := 0
		hooks := storageTestHooks{
			decisionGate: func() bool { return allowed },
			beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					attempts++
					if attempts == 1 {
						allowed = false
						return unix.EINTR
					}
				}
				return nil
			},
		}
		root, target := openRenameTestDirectories(t, inspection, hooks)
		result, err := root.replaceFile("source", target, "target")
		sourceData, sourceErr := os.ReadFile(sourcePath)
		targetData, targetErr := os.ReadFile(targetPath)
		if !errors.Is(err, errRecordDecisionWindowExpired) || result.state != storageRenameNotApplied || attempts != 1 ||
			sourceErr != nil || targetErr != nil || string(sourceData) != "source" || string(targetData) != "target" {
			t.Fatalf("expired replaceFile retry = result:%v err:%v attempts:%d source:%q/%v target:%q/%v",
				result.state, err, attempts, sourceData, sourceErr, targetData, targetErr)
		}
	})

	t.Run("atomic replace unchanged EINTR expires before retry", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		targetPath := filepath.Join(inspection.Root(), "target")
		if err := os.WriteFile(targetPath, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		allowed := true
		attempts := 0
		hooks := storageTestHooks{
			decisionGate: func() bool { return allowed },
			beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					attempts++
					if attempts == 1 {
						allowed = false
						return unix.EINTR
					}
				}
				return nil
			},
		}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		result, writeErr := root.writeFileAtomicOutcome("target", []byte("replacement"))
		data, readErr := os.ReadFile(targetPath)
		if !errors.Is(writeErr, errRecordDecisionWindowExpired) || result.state != storageWriteNotApplied || attempts != 1 ||
			readErr != nil || string(data) != "target" {
			t.Fatalf("expired atomic-replace retry = result:%v err:%v attempts:%d data:%q readErr:%v",
				result.state, writeErr, attempts, data, readErr)
		}
	})
}

func TestStorageReplaceRenameEINTRClassificationCoversEveryCallsite(t *testing.T) {
	t.Run("replaceFile unchanged retries", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		if err := os.Mkdir(filepath.Join(inspection.Root(), inflightDirectoryName), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inspection.Root(), "source"), []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inspection.Root(), inflightDirectoryName, "target"), []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		attempts := 0
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if step == storageStepRename {
				attempts++
				if attempts == 1 {
					return unix.EINTR
				}
			}
			return nil
		}}
		root, target := openRenameTestDirectories(t, inspection, hooks)
		result, err := root.replaceFile("source", target, "target")
		if err != nil || result.state != storageRenameAppliedDurable || attempts != 2 {
			t.Fatalf("unchanged replaceFile EINTR = result:%v err:%v attempts:%d", result.state, err, attempts)
		}
	})

	t.Run("atomic replace unchanged retries", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		if err := os.WriteFile(filepath.Join(inspection.Root(), "target"), []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		attempts := 0
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if step == storageStepRename {
				attempts++
				if attempts == 1 {
					return unix.EINTR
				}
			}
			return nil
		}}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		result, writeErr := root.writeFileAtomicOutcome("target", []byte("replacement"))
		if writeErr != nil || result.state != storageWriteAppliedDurable || attempts != 2 {
			t.Fatalf("unchanged atomic-replace EINTR = result:%v err:%v attempts:%d", result.state, writeErr, attempts)
		}
	})

	t.Run("replaceFile applied then EINTR", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		sourcePath := filepath.Join(inspection.Root(), "source")
		targetPath := filepath.Join(inspection.Root(), inflightDirectoryName, "target")
		if err := os.Mkdir(filepath.Dir(targetPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(targetPath, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		sourceBefore, err := os.Stat(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		attempts := 0
		syncs := 0
		var injectedErr error
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if step == storageStepRename {
				attempts++
				if attempts == 1 {
					injectedErr = os.Rename(sourcePath, targetPath)
					if injectedErr != nil {
						return injectedErr
					}
					return unix.EINTR
				}
			}
			if attempts > 0 && step == storageStepDirectorySync {
				syncs++
			}
			return nil
		}}
		root, target := openRenameTestDirectories(t, inspection, hooks)
		result, replaceErr := root.replaceFile("source", target, "target")
		targetAfter, targetErr := os.Stat(targetPath)
		_, sourceErr := os.Lstat(sourcePath)
		if injectedErr != nil || replaceErr != nil || result.state != storageRenameAppliedDurable || attempts != 1 || syncs != 2 ||
			targetErr != nil || !os.SameFile(sourceBefore, targetAfter) || !errors.Is(sourceErr, fs.ErrNotExist) {
			t.Fatalf("applied replaceFile EINTR = result:%v err:%v injected:%v attempts:%d syncs:%d targetErr:%v sourceErr:%v",
				result.state, replaceErr, injectedErr, attempts, syncs, targetErr, sourceErr)
		}
	})

	t.Run("atomic replace applied then EINTR", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		targetPath := filepath.Join(inspection.Root(), "target")
		if err := os.WriteFile(targetPath, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		var tempPath string
		attempts := 0
		syncs := 0
		var injectedErr error
		hooks := storageTestHooks{
			beforeTempFileCreate: func(path string) { tempPath = path },
			beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					attempts++
					if attempts == 1 {
						injectedErr = os.Rename(tempPath, targetPath)
						if injectedErr != nil {
							return injectedErr
						}
						return unix.EINTR
					}
				}
				if attempts > 0 && step == storageStepDirectorySync {
					syncs++
				}
				return nil
			},
		}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		result, writeErr := root.writeFileAtomicOutcome("target", []byte("replacement"))
		data, readErr := os.ReadFile(targetPath)
		_, tempErr := os.Lstat(tempPath)
		if injectedErr != nil || writeErr != nil || result.state != storageWriteAppliedDurable || attempts != 1 || syncs != 4 ||
			readErr != nil || string(data) != "replacement" || !errors.Is(tempErr, fs.ErrNotExist) {
			t.Fatalf("applied atomic-replace EINTR = result:%v err:%v injected:%v attempts:%d syncs:%d data:%q readErr:%v tempErr:%v",
				result.state, writeErr, injectedErr, attempts, syncs, data, readErr, tempErr)
		}
	})

	t.Run("replaceFile ambiguous syncs both parents", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		sourcePath := filepath.Join(inspection.Root(), "source")
		targetPath := filepath.Join(inspection.Root(), inflightDirectoryName, "target")
		replacementPath := targetPath + ".replacement"
		if err := os.Mkdir(filepath.Dir(targetPath), 0o700); err != nil {
			t.Fatal(err)
		}
		for path, data := range map[string]string{sourcePath: "source", targetPath: "target", replacementPath: "replacement"} {
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		attempts := 0
		syncs := 0
		var injectedErr error
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if step == storageStepRename {
				attempts++
				if attempts == 1 {
					if err := os.Rename(sourcePath, targetPath); err != nil {
						injectedErr = err
						return err
					}
					injectedErr = os.Rename(replacementPath, targetPath)
					if injectedErr != nil {
						return injectedErr
					}
					return unix.EINTR
				}
			}
			if attempts > 0 && step == storageStepDirectorySync {
				syncs++
			}
			return nil
		}}
		root, target := openRenameTestDirectories(t, inspection, hooks)
		result, replaceErr := root.replaceFile("source", target, "target")
		data, readErr := os.ReadFile(targetPath)
		if injectedErr != nil || replaceErr == nil || result.state != storageRenameAppliedSyncPending || attempts != 1 || syncs != 2 ||
			readErr != nil || string(data) != "replacement" {
			t.Fatalf("ambiguous replaceFile EINTR = result:%v err:%v injected:%v attempts:%d syncs:%d data:%q readErr:%v",
				result.state, replaceErr, injectedErr, attempts, syncs, data, readErr)
		}
	})

	t.Run("atomic replace ambiguous syncs parent", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		targetPath := filepath.Join(inspection.Root(), "target")
		replacementPath := targetPath + ".replacement"
		if err := os.WriteFile(targetPath, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		var tempPath string
		attempts := 0
		syncs := 0
		var injectedErr error
		hooks := storageTestHooks{
			beforeTempFileCreate: func(path string) { tempPath = path },
			beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					attempts++
					if attempts == 1 {
						if err := os.Rename(tempPath, targetPath); err != nil {
							injectedErr = err
							return err
						}
						injectedErr = os.Rename(replacementPath, targetPath)
						if injectedErr != nil {
							return injectedErr
						}
						return unix.EINTR
					}
				}
				if attempts > 0 && step == storageStepDirectorySync {
					syncs++
				}
				return nil
			},
		}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		result, writeErr := root.writeFileAtomicOutcome("target", []byte("source"))
		data, readErr := os.ReadFile(targetPath)
		if injectedErr != nil || writeErr == nil || result.state != storageWriteAppliedSyncPending || attempts != 1 || syncs == 0 ||
			readErr != nil || string(data) != "replacement" {
			t.Fatalf("ambiguous atomic-replace EINTR = result:%v err:%v injected:%v attempts:%d syncs:%d data:%q readErr:%v",
				result.state, writeErr, injectedErr, attempts, syncs, data, readErr)
		}
	})
}

func TestRecordOnceDecisionExpiryStopsQuotaStageTempCollisionRetries(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	current := testRecordHour
	service.deps.now = func() time.Time { return current }
	controlPath := filepath.Join(home.Root(), spoolControlDirectoryName)
	seeded := false
	tempAttempts := 0
	collisions := make(map[string]os.FileInfo)
	service.deps.storageHooks.beforeTempFileCreate = func(path string) {
		if filepath.Dir(path) != controlPath {
			return
		}
		tempAttempts++
		if seeded {
			return
		}
		seeded = true
		firstSequence := storageTempSequence.Load()
		for offset := uint64(0); offset < maximumStorageTempAttempts-1; offset++ {
			name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), firstSequence+offset)
			collisionPath := filepath.Join(controlPath, name)
			if err := os.WriteFile(collisionPath, []byte("collision:"+name), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(collisionPath)
			if err != nil {
				t.Fatal(err)
			}
			collisions[collisionPath] = info
		}
		current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
	}

	result := service.RecordOnce(permit, CommandHelp)
	if result != RecordDropped || tempAttempts != 1 || len(collisions) != maximumStorageTempAttempts-1 {
		t.Fatalf("expired quota-stage collision retry = result:%v attempts:%d collisions:%d",
			result, tempAttempts, len(collisions))
	}
	for path, before := range collisions {
		after, err := os.Lstat(path)
		data, readErr := os.ReadFile(path)
		if err != nil || readErr != nil || !os.SameFile(before, after) || string(data) != "collision:"+filepath.Base(path) {
			t.Fatalf("quota-stage collision changed %q: same=%v statErr=%v data=%q readErr=%v",
				path, err == nil && os.SameFile(before, after), err, data, readErr)
		}
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), quotaFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expired quota-stage retry installed quota: %v", err)
	}
	if _, err := os.Lstat(controlPath); err != nil {
		t.Fatalf("expired quota-stage retry lost fail-closed control evidence: %v", err)
	}
	assertNoQueuedEvents(t, home)
}

func TestRecordOnceDecisionGatesStorageSafeBoundaries(t *testing.T) {
	t.Run("root components before state lock", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		nowCalls := 0
		service.deps.now = func() time.Time {
			nowCalls++
			if nowCalls > 2 {
				return testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			}
			return testRecordHour
		}
		directoryOpens := 0
		service.deps.storageHooks.afterDirectoryOpen = func(string) { directoryOpens++ }
		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("RecordOnce = %v", got)
		}
		if directoryOpens != 0 {
			t.Fatalf("expired root-open boundary completed %d component opens", directoryOpens)
		}
		if _, err := os.Lstat(filepath.Join(home.Root(), stateLockName)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("expired root-open boundary reached state lock: %v", err)
		}
	})

	t.Run("config validation after file open", func(t *testing.T) {
		_, service, permit := newRecordServiceFixture(t, testEventIDOne)
		current := testRecordHour
		service.deps.now = func() time.Time { return current }
		configOpened := false
		configValidations := 0
		service.deps.storageHooks.afterFileOpen = func(path string) {
			if filepath.Base(path) == configFileName {
				configOpened = true
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			}
		}
		service.deps.storageHooks.metadata = func(path string, metadata storageMetadata) storageMetadata {
			if configOpened && filepath.Base(path) == configFileName {
				configValidations++
			}
			return metadata
		}
		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("RecordOnce = %v", got)
		}
		if !configOpened || configValidations != 0 {
			t.Fatalf("expired post-open config boundary = opened:%v validations:%d", configOpened, configValidations)
		}
	})

	t.Run("ENOENT before directory creation", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		current := testRecordHour
		service.deps.now = func() time.Time { return current }
		queueOpenAttempts := 0
		service.deps.storageHooks.beforeDirectoryOpen = func(path string) error {
			if path == filepath.Join(home.Root(), queueDirectoryName) {
				queueOpenAttempts++
			}
			return nil
		}
		service.deps.storageHooks.afterDirectoryAttempt = func(path string, err error) {
			if path == filepath.Join(home.Root(), queueDirectoryName) && errors.Is(err, fs.ErrNotExist) {
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			}
		}
		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("RecordOnce = %v", got)
		}
		if queueOpenAttempts != 1 {
			t.Fatalf("expired ENOENT-to-Mkdir boundary attempted queue open %d times", queueOpenAttempts)
		}
		if _, err := os.Lstat(filepath.Join(home.Root(), queueDirectoryName)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("expired ENOENT-to-Mkdir boundary created queue: %v", err)
		}
		quota := readQuotaFixture(t, home)
		if quota.Events != 1 || quota.Bytes == 0 {
			t.Fatalf("expired directory boundary lost conservative quota: %+v", quota)
		}
	})

	t.Run("between root components", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		current := testRecordHour
		service.deps.now = func() time.Time { return current }
		componentOpens := 0
		service.deps.storageHooks.afterDirectoryOpen = func(path string) {
			if path == "/" {
				return
			}
			componentOpens++
			if componentOpens == 1 {
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			}
		}
		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("RecordOnce = %v", got)
		}
		if componentOpens != 1 {
			t.Fatalf("expired between-component boundary opened %d components", componentOpens)
		}
		if _, err := os.Lstat(filepath.Join(home.Root(), stateLockName)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("expired between-component boundary reached state lock: %v", err)
		}
	})

	t.Run("after final validation before read", func(t *testing.T) {
		_, service, permit := newRecordServiceFixture(t, testEventIDOne)
		current := testRecordHour
		service.deps.now = func() time.Time { return current }
		beforeRead := 0
		readSyscalls := 0
		service.deps.storageHooks.beforeRead = func(path string) {
			if filepath.Base(path) == configFileName {
				beforeRead++
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			}
		}
		service.deps.storageHooks.afterRead = func(path string, _, _ int, _ error) {
			if filepath.Base(path) == configFileName {
				readSyscalls++
			}
		}
		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("RecordOnce = %v", got)
		}
		if beforeRead != 1 || readSyscalls != 0 {
			t.Fatalf("expired final-validation boundary = beforeRead:%d syscalls:%d", beforeRead, readSyscalls)
		}
	})
}

func TestStorageDecisionExpiryAfterRevalidationPreventsMutation(t *testing.T) {
	for _, operation := range []string{"rename", "remove"} {
		t.Run(operation, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			allowed := true
			armed := false
			preMutation := 0
			hooks := storageTestHooks{
				decisionGate: func() bool { return allowed },
				beforeMutation: func(_ storageStep, _ string) {
					if armed {
						preMutation++
						allowed = false
					}
				},
			}
			root, err := openStorageRootMutableWithHooks(inspection, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			sourcePath := filepath.Join(inspection.Root(), "source")
			switch operation {
			case "rename":
				if err := os.Mkdir(sourcePath, 0o700); err != nil {
					t.Fatal(err)
				}
			case "remove":
				if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			entry, err := root.lookupEntry("source")
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			armed = true
			if operation == "rename" {
				result, err := root.renameEnumeratedDirectory(entry, root.storageDir, "target")
				if !errors.Is(err, errRecordDecisionWindowExpired) || result.state != storageRenameNotApplied {
					t.Fatalf("expired post-revalidation rename = (%v, %v)", result.state, err)
				}
				if _, err := os.Lstat(filepath.Join(inspection.Root(), "target")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("expired rename created target: %v", err)
				}
			} else if err := root.unlinkEnumeratedEntry(entry); !errors.Is(err, errRecordDecisionWindowExpired) {
				t.Fatalf("expired post-revalidation remove = %v", err)
			}
			after, err := os.Lstat(sourcePath)
			if err != nil || !os.SameFile(before, after) || preMutation != 1 {
				t.Fatalf("expired %s changed source: same=%v err=%v preMutation=%d", operation,
					err == nil && os.SameFile(before, after), err, preMutation)
			}
		})
	}
}

func TestRecordOnceDecisionExpiryStopsUnchangedNoReplaceRetry(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	current := testRecordHour
	service.deps.now = func() time.Time { return current }
	armed := false
	renameAttempts := 0
	service.deps.storageHooks.afterAtomicWrite = func(path string, state storageWriteState) {
		if filepath.Base(path) == quotaStagingFileName && state == storageWriteAppliedDurable {
			armed = true
		}
	}
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if armed && step == storageStepRename {
			renameAttempts++
			if renameAttempts == 1 {
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
				return unix.EINTR
			}
		}
		return nil
	}
	if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
		t.Fatalf("RecordOnce = %v", got)
	}
	if renameAttempts != 1 {
		t.Fatalf("expired unchanged no-replace rename made %d attempts", renameAttempts)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), quotaFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expired unchanged no-replace retry installed quota: %v", err)
	}
	stagingPath := filepath.Join(home.Root(), spoolControlDirectoryName, quotaStagingFileName)
	if _, err := os.Lstat(stagingPath); err != nil {
		t.Fatalf("expired unchanged no-replace retry lost staging evidence: %v", err)
	}
	assertNoQueuedEvents(t, home)
}

func TestRecordOnceDecisionExpiryDuringNoReplacePrestatePreventsInstall(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	current := testRecordHour
	service.deps.now = func() time.Time { return current }
	generationPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
	eventPath := filepath.Join(generationPath, eventFileName(testEventIDOne))
	var eventTempPath string
	expired := false
	renameSteps := 0
	service.deps.storageHooks.beforeTempFileCreate = func(path string) {
		if filepath.Dir(path) == generationPath {
			eventTempPath = path
		}
	}
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepRename && eventTempPath != "" && !expired {
			renameSteps++
			expired = true
			current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
		}
		return nil
	}
	result := service.RecordOnce(permit, CommandHelp)
	_ = permit.Close()
	_, eventErr := os.Lstat(eventPath)
	entries, readDirErr := os.ReadDir(generationPath)
	temporaryNames := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pm-tmp-") {
			temporaryNames++
		}
	}
	quota := readQuotaFixture(t, home)
	if result != RecordDropped || !expired || renameSteps != 1 || eventTempPath == "" ||
		!errors.Is(eventErr, fs.ErrNotExist) || readDirErr != nil || temporaryNames != 0 ||
		quota.Events != 1 || quota.Bytes == 0 {
		t.Fatalf("expired no-replace prestate = result:%v expired:%v renames:%d temp:%q eventErr:%v readDirErr:%v temps:%d quota:%+v",
			result, expired, renameSteps, eventTempPath, eventErr, readDirErr, temporaryNames, quota)
	}
}

func TestDirectoryOpenatInstrumentationHasSingleStructuralGateway(t *testing.T) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "storage_unix.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	direct := make(map[string]int)
	openatSelectors := 0
	openatCalls := 0
	gatewayBefore := false
	gatewayAfter := false
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok {
				if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "unix" && selector.Sel.Name == "Openat" {
					openatSelectors++
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok {
				if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "unix" && selector.Sel.Name == "Openat" {
					openatCalls++
					direct[function.Name.Name]++
					for _, argument := range call.Args {
						if identifier, ok := argument.(*ast.Ident); ok && identifier.Name == "unixDirectoryOpenFlags" {
							if function.Name.Name != "openDirectoryAt" {
								t.Errorf("directory Openat found outside gateway in %s", function.Name.Name)
							}
						}
					}
				}
				if function.Name.Name == "openDirectoryAt" && selector.Sel.Name == "openingDirectory" {
					gatewayBefore = true
				}
				if function.Name.Name == "openDirectoryAt" && selector.Sel.Name == "openedDirectory" {
					gatewayAfter = true
				}
			}
			return true
		})
	}
	wantDirect := map[string]int{"openDirectoryAt": 1, "openFileAt": 1, "openFileAtGated": 1}
	if fmt.Sprint(direct) != fmt.Sprint(wantDirect) || openatSelectors != openatCalls || !gatewayBefore || !gatewayAfter {
		t.Fatalf("Openat instrumentation gateways = direct:%v selectors:%d calls:%d before:%v after:%v; want %v and one fully instrumented directory gateway",
			direct, openatSelectors, openatCalls, gatewayBefore, gatewayAfter, wantDirect)
	}
}

func TestDualControlCleanupPreemptsDeepTreeStarvation(t *testing.T) {
	for _, activeShape := range []string{"directory", "leaf", "fifo", "symlink"} {
		t.Run(activeShape, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
			plainRoot := mustOpenMutableRoot(t, home)
			quota := spoolQuota{Events: 1, Bytes: 1}
			if err := persistSpoolQuota(plainRoot, quota); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}

			sentinel := filepath.Join(t.TempDir(), "outside-sentinel")
			if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			activePath := filepath.Join(home.Root(), spoolControlDirectoryName)
			switch activeShape {
			case "directory":
				if err := os.Mkdir(activePath, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(activePath, "evidence"), []byte("active"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "leaf":
				if err := os.WriteFile(activePath, []byte("active"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(activePath, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(sentinel, activePath); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown active-control shape %q", activeShape)
			}
			activeBefore, err := os.Lstat(activePath)
			if err != nil {
				t.Fatal(err)
			}

			retiredPath := filepath.Join(home.Root(), retiredControlDirectoryName)
			retiredDeep := retiredPath
			for depth := 0; depth < 12; depth++ {
				retiredDeep = filepath.Join(retiredDeep, fmt.Sprintf("r%02d", depth))
			}
			if err := os.MkdirAll(retiredDeep, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(sentinel, filepath.Join(retiredDeep, "outside-link")); err != nil {
				t.Fatal(err)
			}
			queuePath := filepath.Join(home.Root(), queueDirectoryName, "99999999-9999-4999-8999-999999999999")
			queueDeep := queuePath
			for depth := 0; depth < 12; depth++ {
				queueDeep = filepath.Join(queueDeep, fmt.Sprintf("q%02d", depth))
			}
			if err := os.MkdirAll(queueDeep, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(queueDeep, "payload"), []byte("event-tree"), 0o600); err != nil {
				t.Fatal(err)
			}

			snapshot := func(path string) string {
				t.Helper()
				var entries []string
				err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					relative, err := filepath.Rel(path, current)
					if err != nil {
						return err
					}
					entries = append(entries, fmt.Sprintf("%s:%s", relative, entry.Type()))
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return strings.Join(entries, "\n")
			}
			retiredBefore := snapshot(retiredPath)
			queueBefore := snapshot(queuePath)
			physicalOpens := 0
			root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
				afterDirectoryOpen: func(string) { physicalOpens++ },
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			physicalOpens = 0
			budget := defaultSpoolWorkBudget()
			budget.maxDirectories = 4

			result, err := purgeSpool(root, budget)
			if err != nil || result.complete {
				t.Fatalf("dual-control first pass = complete:%v err:%v usage:%+v", result.complete, err, result.usage)
			}
			if uint64(physicalOpens) > budget.maxDirectories {
				t.Fatalf("dual-control first pass opened %d directories, budget=%d", physicalOpens, budget.maxDirectories)
			}
			if got := snapshot(retiredPath); got == retiredBefore {
				t.Fatal("deep event traversal starved retired-control cleanup")
			}
			if got := snapshot(queuePath); got != queueBefore {
				t.Fatal("event traversal ran before dual-control recovery made progress")
			}
			activeAfter, err := os.Lstat(activePath)
			if err != nil || !os.SameFile(activeBefore, activeAfter) {
				t.Fatalf("active fail-closed evidence changed: before=%v after=%v err=%v", activeBefore, activeAfter, err)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside" {
				t.Fatalf("outside sentinel changed: data=%q err=%v", data, err)
			}
			deps := defaultTestServiceDependencies(home, 2)
			deps.newUUID = uuidSequence(t, testEventIDThree)
			deps.now = func() time.Time { return testRecordHour }
			service := mustOpenTestService(t, deps)
			permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
			if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
				t.Fatalf("incomplete dual-control RecordOnce = %v, want dropped", got)
			}
			_ = permit.Close()
			eventPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(testEventIDThree))
			if _, err := os.Lstat(eventPath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("incomplete dual-control pass admitted an event: %v", err)
			}
		})
	}
}

func TestSpoolDeepPurgeConvergesUnderLowFileDescriptorLimit(t *testing.T) {
	const helperEnvironment = "GC_PRODUCTMETRICS_LOW_NOFILE_HELPER"
	if os.Getenv(helperEnvironment) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 4*testutil.ExecRaceTimeout)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSpoolDeepPurgeConvergesUnderLowFileDescriptorLimit$")
		command.Env = append(os.Environ(), helperEnvironment+"=1")
		output, err := command.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("low-NOFILE purge helper timed out: %v\n%s", ctx.Err(), output)
		}
		if err != nil {
			t.Fatalf("low-NOFILE purge helper failed: %v\n%s", err, output)
		}
		return
	}

	limit := unix.Rlimit{Cur: 128, Max: 128}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("set low RLIMIT_NOFILE: %v", err)
	}
	var observed unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &observed); err != nil || observed.Cur != limit.Cur {
		t.Fatalf("low RLIMIT_NOFILE = %+v, err=%v", observed, err)
	}

	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	deep := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	for depth := 0; depth < 300; depth++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%03d", depth))
		if err := os.Mkdir(deep, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(deep, "payload"), []byte("deep"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "outside-sentinel")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(deep, "outside-link")); err != nil {
		t.Fatal(err)
	}
	plainRoot := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(plainRoot, spoolQuota{Events: 1, Bytes: 4}); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	result := spoolSweepResult{}
	for attempt := 0; attempt < 128 && !result.complete; attempt++ {
		root := mustOpenMutableRoot(t, home)
		before := fallbackProgressFingerprint(t, home.Root())
		var purgeErr error
		result, purgeErr = purgeSpool(root, defaultSpoolWorkBudget())
		after := fallbackProgressFingerprint(t, home.Root())
		strongEvidence := result.complete || hasStrongFailClosedSpoolEvidence(root)
		closeErr := root.Close()
		if purgeErr != nil || closeErr != nil || result.usage.entries > maximumCleanupEntries ||
			result.usage.directories > 32 || result.usage.readBytes > maximumCleanupReadBytes ||
			result.usage.nameBytes > maximumCleanupNameBytes || !strongEvidence || !result.complete && before == after {
			t.Fatalf("low-NOFILE purge attempt %d = complete:%v err:%v close:%v progressed:%v evidence:%v usage:%+v",
				attempt+1, result.complete, purgeErr, closeErr, before != after, strongEvidence, result.usage)
		}
	}
	if !result.complete || result.quota != (spoolQuota{}) {
		t.Fatalf("low-NOFILE deep purge did not converge: %+v", result)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside" {
		t.Fatalf("low-NOFILE deep purge changed outside sentinel: data=%q err=%v", data, err)
	}
}

func TestSpoolNestedPurgeConvergesAtMinimumDirectoryBudget(t *testing.T) {
	const helperEnvironment = "GC_PRODUCTMETRICS_MIN_NOFILE_HELPER"
	if os.Getenv(helperEnvironment) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 4*testutil.ExecRaceTimeout)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSpoolNestedPurgeConvergesAtMinimumDirectoryBudget$")
		command.Env = append(os.Environ(), helperEnvironment+"=1")
		defer func() {
			for _, inherited := range command.ExtraFiles {
				if err := inherited.Close(); err != nil {
					t.Errorf("close inherited descriptor fixture: %v", err)
				}
			}
		}()
		for range 2 {
			inherited, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatalf("open inherited descriptor fixture: %v", err)
			}
			command.ExtraFiles = append(command.ExtraFiles, inherited)
		}
		output, err := command.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("minimum-directory-budget purge helper timed out: %v\n%s", ctx.Err(), output)
		}
		if err != nil {
			t.Fatalf("minimum-directory-budget purge helper failed: %v\n%s", err, output)
		}
		return
	}

	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	deep := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, "nested", "nonempty")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "payload"), []byte("event-tree"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "outside-sentinel")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(deep, "outside-link")); err != nil {
		t.Fatal(err)
	}
	plainRoot := mustOpenMutableRoot(t, home)
	initialQuota := spoolQuota{Events: 1, Bytes: uint64(len("event-tree"))}
	if err := persistSpoolQuota(plainRoot, initialQuota); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("read RLIMIT_NOFILE: %v", err)
	}
	// The fallback limit still selects the minimum directory budget because
	// 32/4 is below spoolMinimumDirectoryProgress, while leaving headroom for
	// descriptors owned by the Go test harness.
	const softLimit = spoolFallbackDirectoryLimit
	if limit.Max < softLimit {
		t.Skipf("RLIMIT_NOFILE hard limit %d is below regression limit", limit.Max)
	}
	limit.Cur = softLimit
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("set minimum-directory-budget RLIMIT_NOFILE: %v", err)
	}
	var observed unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &observed); err != nil || observed.Cur != limit.Cur || observed.Max != limit.Max {
		t.Fatalf("minimum-directory-budget RLIMIT_NOFILE = %+v, err=%v, want %+v", observed, err, limit)
	}

	result := spoolSweepResult{}
	for attempt := 0; attempt < 32 && !result.complete; attempt++ {
		root := mustOpenMutableRoot(t, home)
		before := fallbackProgressFingerprint(t, home.Root())
		var purgeErr error
		result, purgeErr = purgeSpool(root, defaultSpoolWorkBudget())
		after := fallbackProgressFingerprint(t, home.Root())
		strongEvidence := result.complete || hasStrongFailClosedSpoolEvidence(root)
		closeErr := root.Close()
		if purgeErr != nil || closeErr != nil || result.usage.directories > spoolMinimumDirectoryProgress ||
			!strongEvidence || !result.complete && before == after {
			t.Fatalf("minimum-directory-budget purge attempt %d = complete:%v err:%v close:%v progressed:%v evidence:%v usage:%+v",
				attempt+1, result.complete, purgeErr, closeErr, before != after, strongEvidence, result.usage)
		}
	}
	if !result.complete || result.quota != (spoolQuota{}) {
		t.Fatalf("minimum-directory-budget nested purge did not converge: %+v", result)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside" {
		t.Fatalf("minimum-directory-budget nested purge changed outside sentinel: data=%q err=%v", data, err)
	}
}

func TestSpoolDirectoryBudgetDerivedFromSoftLimit(t *testing.T) {
	tests := []struct {
		name      string
		requested uint64
		softLimit uint64
		want      uint64
	}{
		{name: "zero descriptors still attempts the progress envelope", requested: maximumCleanupDirectories, softLimit: 0, want: spoolMinimumDirectoryProgress},
		{name: "sixteen descriptors", requested: maximumCleanupDirectories, softLimit: 16, want: spoolMinimumDirectoryProgress},
		{name: "twenty descriptors", requested: maximumCleanupDirectories, softLimit: 20, want: spoolMinimumDirectoryProgress},
		{name: "one hundred twenty eight descriptors", requested: maximumCleanupDirectories, softLimit: 128, want: 32},
		{name: "large limit retains caller cap", requested: maximumCleanupDirectories, softLimit: 4096, want: maximumCleanupDirectories},
		{name: "explicit smaller budget is not raised", requested: 4, softLimit: 16, want: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := spoolDirectoryBudgetForSoftLimit(test.requested, test.softLimit); got != test.want {
				t.Fatalf("directory budget for requested=%d soft=%d = %d, want %d",
					test.requested, test.softLimit, got, test.want)
			}
		})
	}
}

func TestSpoolPurgePreservesDescriptorExhaustionError(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	queuePath := filepath.Join(home.Root(), queueDirectoryName)
	deep := filepath.Join(queuePath, testSpoolGeneration, "nested")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "payload"), []byte("event-tree"), 0o600); err != nil {
		t.Fatal(err)
	}
	plainRoot := mustOpenMutableRoot(t, home)
	initialQuota := spoolQuota{Events: 1, Bytes: uint64(len("event-tree"))}
	if err := persistSpoolQuota(plainRoot, initialQuota); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeDirectoryOpen: func(path string) error {
			if path == queuePath {
				return unix.EMFILE
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, purgeErr := purgeSpool(root, defaultSpoolWorkBudget())
	strongEvidence := hasStrongFailClosedSpoolEvidence(root)
	closeErr := root.Close()
	if !errors.Is(purgeErr, unix.EMFILE) || result.complete || closeErr != nil || !strongEvidence ||
		readQuotaFixture(t, home) != initialQuota {
		t.Fatalf("descriptor-exhausted purge = result:%+v err:%v close:%v evidence:%v quota:%+v",
			result, purgeErr, closeErr, strongEvidence, readQuotaFixture(t, home))
	}
}

func TestLoneRetiredDeepControlPreemptsEventTraversalAtENOSYSCap(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	plainRoot := mustOpenMutableRoot(t, home)
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	if err := persistSpoolQuota(plainRoot, markers); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.Root(), stateLockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	retiredPath := filepath.Join(home.Root(), retiredControlDirectoryName)
	if err := os.Mkdir(retiredPath, 0o700); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(retiredPath, "temporary")
	if err := os.Mkdir(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	deepRetired := temporary
	for depth := 0; depth < 12; depth++ {
		deepRetired = filepath.Join(deepRetired, fmt.Sprintf("r%02d", depth))
		if err := os.Mkdir(deepRetired, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(deepRetired, "payload"), []byte("retired"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Lstat(temporary, &stat); err != nil {
		t.Fatal(err)
	}
	canonical := fmt.Sprintf(".orphan-%x-%x", unixStatDevice(stat), unixStatInode(stat))
	if err := os.Rename(temporary, filepath.Join(retiredPath, canonical)); err != nil {
		t.Fatal(err)
	}
	firstNestedPath := filepath.Join(retiredPath, canonical, "r00")
	if err := unix.Lstat(firstNestedPath, &stat); err != nil {
		t.Fatal(err)
	}
	firstNestedStat := stat
	firstNestedCanonical := fmt.Sprintf(".orphan-%x-%x", unixStatDevice(stat), unixStatInode(stat))

	queuePath := filepath.Join(home.Root(), queueDirectoryName, "99999999-9999-4999-8999-999999999999")
	deepQueue := queuePath
	for depth := 0; depth < 12; depth++ {
		deepQueue = filepath.Join(deepQueue, fmt.Sprintf("q%02d", depth))
	}
	if err := os.MkdirAll(deepQueue, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deepQueue, "payload"), []byte("event-tree"), 0o600); err != nil {
		t.Fatal(err)
	}

	exchangeAttempts := 0
	physicalOpens := 0
	blockerInjected := false
	currentAttempt := -1
	injectionAttempt := -1
	var blockerInjectionErr error
	var postInjectionFingerprint string
	var sourcePostInjectionFingerprint string
	var blockerPath string
	var blockerStat unix.Stat_t
	var parkingOccupantPath string
	var parkingOccupantTargetPath string
	var parkingOccupantStat unix.Stat_t
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeMutation: func(step storageStep, sourceName string) {
			if blockerInjected || step != storageStepRename || sourceName != "r00" {
				return
			}
			blockerInjected = true
			blockerPath = filepath.Join(retiredPath, firstNestedCanonical)
			blockerInjectionErr = os.Mkdir(blockerPath, 0o700)
			if blockerInjectionErr == nil {
				blockerInjectionErr = os.WriteFile(filepath.Join(blockerPath, "payload"), []byte("blocker"), 0o600)
			}
			if blockerInjectionErr == nil {
				blockerInjectionErr = unix.Lstat(blockerPath, &blockerStat)
			}
			blockerCanonical := fmt.Sprintf(".orphan-%x-%x", unixStatDevice(blockerStat), unixStatInode(blockerStat))
			parkingOccupantPath = filepath.Join(retiredPath, blockerCanonical)
			if blockerInjectionErr == nil {
				blockerInjectionErr = os.Mkdir(parkingOccupantPath, 0o700)
			}
			if blockerInjectionErr == nil {
				blockerInjectionErr = os.WriteFile(filepath.Join(parkingOccupantPath, "payload"), []byte("parking-occupant"), 0o600)
			}
			if blockerInjectionErr == nil {
				blockerInjectionErr = unix.Lstat(parkingOccupantPath, &parkingOccupantStat)
			}
			parkingOccupantCanonical := fmt.Sprintf(".orphan-%x-%x", unixStatDevice(parkingOccupantStat), unixStatInode(parkingOccupantStat))
			parkingOccupantTargetPath = filepath.Join(retiredPath, parkingOccupantCanonical)
			if blockerInjectionErr == nil {
				injectionAttempt = currentAttempt
				postInjectionFingerprint = filesystemStateFingerprint(t, home.Root())
				sourcePostInjectionFingerprint = filesystemStateFingerprint(t, firstNestedPath)
			}
		},
		beforeExchange: func() error {
			exchangeAttempts++
			return unix.ENOSYS
		},
		afterDirectoryOpen: func(string) { physicalOpens++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	budget := defaultSpoolWorkBudget()
	budget.maxDirectories = 3
	for attempt := 0; attempt < 3; attempt++ {
		currentAttempt = attempt
		before := filesystemStateFingerprint(t, home.Root())
		queueBefore := filesystemStateFingerprint(t, filepath.Join(home.Root(), queueDirectoryName))
		physicalOpens = 0
		result, purgeErr := purgeSpool(root, budget)
		after := filesystemStateFingerprint(t, home.Root())
		queueAfter := filesystemStateFingerprint(t, filepath.Join(home.Root(), queueDirectoryName))
		progressBaseline := before
		if injectionAttempt == attempt {
			progressBaseline = postInjectionFingerprint
			var moved unix.Stat_t
			_, oldErr := os.Lstat(parkingOccupantPath)
			newErr := unix.Lstat(parkingOccupantTargetPath, &moved)
			payload, payloadErr := os.ReadFile(filepath.Join(parkingOccupantTargetPath, "payload"))
			var sourceAfter unix.Stat_t
			sourceErr := unix.Lstat(firstNestedPath, &sourceAfter)
			var blockerAfter unix.Stat_t
			blockerErr := unix.Lstat(blockerPath, &blockerAfter)
			blockerPayload, blockerPayloadErr := os.ReadFile(filepath.Join(blockerPath, "payload"))
			if !errors.Is(oldErr, fs.ErrNotExist) || newErr != nil ||
				unixStatDevice(moved) != unixStatDevice(parkingOccupantStat) || unixStatInode(moved) != unixStatInode(parkingOccupantStat) ||
				payloadErr != nil || string(payload) != "parking-occupant" || sourceErr != nil ||
				unixStatDevice(sourceAfter) != unixStatDevice(firstNestedStat) || unixStatInode(sourceAfter) != unixStatInode(firstNestedStat) ||
				filesystemStateFingerprint(t, firstNestedPath) != sourcePostInjectionFingerprint || blockerErr != nil ||
				unixStatDevice(blockerAfter) != unixStatDevice(blockerStat) || unixStatInode(blockerAfter) != unixStatInode(blockerStat) ||
				blockerPayloadErr != nil || string(blockerPayload) != "blocker" {
				t.Fatalf("lone retired parking-chain progress = old:%v new:%v same:%v payload:%q payloadErr:%v sourceErr:%v sourceSame:%v blockerErr:%v blockerSame:%v blockerPayload:%q blockerPayloadErr:%v",
					oldErr, newErr,
					newErr == nil && unixStatDevice(moved) == unixStatDevice(parkingOccupantStat) && unixStatInode(moved) == unixStatInode(parkingOccupantStat),
					payload, payloadErr, sourceErr,
					sourceErr == nil && unixStatDevice(sourceAfter) == unixStatDevice(firstNestedStat) && unixStatInode(sourceAfter) == unixStatInode(firstNestedStat),
					blockerErr,
					blockerErr == nil && unixStatDevice(blockerAfter) == unixStatDevice(blockerStat) && unixStatInode(blockerAfter) == unixStatInode(blockerStat),
					blockerPayload, blockerPayloadErr)
			}
		}
		if purgeErr != nil || result.complete || progressBaseline == after || queueBefore != queueAfter ||
			uint64(physicalOpens) > budget.maxDirectories || !hasStrongFailClosedSpoolEvidence(root) {
			t.Fatalf("lone retired pass %d = complete:%v err:%v progressed:%v queueChanged:%v opens:%d usage:%+v evidence:%v",
				attempt+1, result.complete, purgeErr, progressBaseline != after, queueBefore != queueAfter,
				physicalOpens, result.usage, hasStrongFailClosedSpoolEvidence(root))
		}
		if _, activeErr := root.lookupEntry(spoolControlDirectoryName); !errors.Is(activeErr, fs.ErrNotExist) {
			t.Fatalf("lone retired pass %d created active control: %v", attempt+1, activeErr)
		}
		deps := defaultTestServiceDependencies(home, 2)
		deps.newUUID = uuidSequence(t, testEventIDThree)
		deps.now = func() time.Time { return testRecordHour }
		service := mustOpenTestService(t, deps)
		permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
		if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
			t.Fatalf("lone retired pass %d RecordOnce = %v, want dropped", attempt+1, got)
		}
		_ = permit.Close()
	}
	if !blockerInjected || blockerInjectionErr != nil || exchangeAttempts == 0 {
		t.Fatalf("lone retired cap-boundary collision = injected:%v injectionErr:%v exchanges:%d",
			blockerInjected, blockerInjectionErr, exchangeAttempts)
	}
}

func TestNestedRetiredControlAncestorCollisionRotatesBlockerWithoutExchange(t *testing.T) {
	home := newMetricsTestHome(t)
	plainRoot := mustOpenMutableRoot(t, home)
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	if err := persistSpoolQuota(plainRoot, markers); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	retiredPath := filepath.Join(home.Root(), retiredControlDirectoryName)
	temporary := filepath.Join(retiredPath, "temporary")
	sourcePath := filepath.Join(temporary, "nested")
	if err := os.MkdirAll(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "payload"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	var sourceStat unix.Stat_t
	if err := unix.Lstat(sourcePath, &sourceStat); err != nil {
		t.Fatal(err)
	}
	sourceCanonical := fmt.Sprintf(".orphan-%x-%x", unixStatDevice(sourceStat), unixStatInode(sourceStat))
	ancestorPath := filepath.Join(retiredPath, sourceCanonical)
	if err := os.Rename(temporary, ancestorPath); err != nil {
		t.Fatal(err)
	}

	exchangeAttempts := 0
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{beforeExchange: func() error {
		exchangeAttempts++
		return unix.ENOSYS
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	retired, err := root.openDir([]string{retiredControlDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = retired.Close() }()
	ancestor, err := retired.lookupEntry(sourceCanonical)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := retired.openEnumeratedCleanupDirectory(ancestor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Close() }()
	source, err := parent.lookupEntry("nested")
	if err != nil {
		t.Fatal(err)
	}
	ancestorCanonical := fmt.Sprintf(".orphan-%x-%x", ancestor.metadata.dev, ancestor.metadata.ino)
	rotatedPath := filepath.Join(retiredPath, ancestorCanonical)
	before := filesystemStateFingerprint(t, home.Root())
	state := &spoolSweepState{
		root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()), failClosedArmed: true,
	}
	progressed, liftErr := state.liftNestedRetiredControlDirectory(parent, retired, source, sourceCanonical)
	after := filesystemStateFingerprint(t, home.Root())
	var rotated unix.Stat_t
	rotatedErr := unix.Lstat(rotatedPath, &rotated)
	_, oldErr := os.Lstat(ancestorPath)
	payload, payloadErr := os.ReadFile(filepath.Join(rotatedPath, "nested", "payload"))
	if liftErr != nil || !progressed || !state.mutated || before == after || exchangeAttempts != 0 ||
		!errors.Is(oldErr, fs.ErrNotExist) || rotatedErr != nil ||
		unixStatDevice(rotated) != ancestor.metadata.dev || unixStatInode(rotated) != ancestor.metadata.ino ||
		payloadErr != nil || string(payload) != "source" {
		t.Fatalf("ancestor collision = progressed:%v mutated:%v err:%v changed:%v exchanges:%d old:%v rotated:%v same:%v payload:%q payloadErr:%v",
			progressed, state.mutated, liftErr, before != after, exchangeAttempts, oldErr, rotatedErr,
			rotatedErr == nil && unixStatDevice(rotated) == ancestor.metadata.dev && unixStatInode(rotated) == ancestor.metadata.ino,
			payload, payloadErr)
	}
}

func TestRetiredControlCollisionRotationBreaksCanonicalTerminalGraphs(t *testing.T) {
	tests := []struct {
		name            string
		nodes           int
		saturateBreaker bool
		finalNames      func([]string) []string
	}{
		{
			name:  "self-canonical terminal",
			nodes: 1,
			finalNames: func(canonical []string) []string {
				return []string{canonical[0]}
			},
		},
		{
			name:  "two-cycle",
			nodes: 2,
			finalNames: func(canonical []string) []string {
				return []string{canonical[1], canonical[0]}
			},
		},
		{
			name:  "long canonical chain",
			nodes: maximumRelocationSlots + 1,
			finalNames: func(canonical []string) []string {
				names := make([]string, len(canonical))
				names[0] = "chain-start"
				for index := 1; index < len(names); index++ {
					names[index] = canonical[index-1]
				}
				return names
			},
		},
		{
			name: "full graph-breaker block", nodes: 1, saturateBreaker: true,
			finalNames: func(canonical []string) []string {
				return []string{canonical[0]}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			plainRoot := mustOpenMutableRoot(t, home)
			markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
			if err := persistSpoolQuota(plainRoot, markers); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}
			retiredPath := filepath.Join(home.Root(), retiredControlDirectoryName)
			if err := os.Mkdir(retiredPath, 0o700); err != nil {
				t.Fatal(err)
			}
			canonical := make([]string, test.nodes)
			temporary := make([]string, test.nodes)
			metadata := make([]unix.Stat_t, test.nodes)
			for index := 0; index < test.nodes; index++ {
				temporary[index] = filepath.Join(retiredPath, fmt.Sprintf("temporary-%02d", index))
				if err := os.Mkdir(temporary[index], 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(temporary[index], "payload"), []byte(fmt.Sprintf("node-%02d", index)), 0o600); err != nil {
					t.Fatal(err)
				}
				var stat unix.Stat_t
				if err := unix.Lstat(temporary[index], &stat); err != nil {
					t.Fatal(err)
				}
				metadata[index] = stat
				canonical[index] = fmt.Sprintf(".orphan-%x-%x", unixStatDevice(stat), unixStatInode(stat))
			}
			finalNames := test.finalNames(canonical)
			for index := range temporary {
				if err := os.Rename(temporary[index], filepath.Join(retiredPath, finalNames[index])); err != nil {
					t.Fatal(err)
				}
			}
			breakerOccupants := 0
			if test.saturateBreaker {
				span := maximumRelocationSequence - uint64(maximumRelocationSlots)
				start := (unixStatDevice(metadata[0]) ^ unixStatInode(metadata[0])*0x9e3779b97f4a7c15) % (span + 1)
				for offset := 0; offset < maximumRelocationSlots; offset++ {
					path := filepath.Join(retiredPath, relocationCandidateName(start+uint64(offset)))
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(path, "payload"), []byte(fmt.Sprintf("breaker-%02d", offset)), 0o600); err != nil {
						t.Fatal(err)
					}
					breakerOccupants++
				}
			}

			root := mustOpenMutableRoot(t, home)
			defer func() { _ = root.Close() }()
			retired, err := root.openDir([]string{retiredControlDirectoryName}, false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = retired.Close() }()
			blocker, err := retired.lookupEntry(finalNames[0])
			if err != nil {
				t.Fatal(err)
			}
			before := filesystemStateFingerprint(t, retiredPath)
			state := &spoolSweepState{
				root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()), failClosedArmed: true,
			}
			progressed, rotateErr := state.rotateRetiredControlCollision(retired, blocker)
			after := filesystemStateFingerprint(t, retiredPath)
			payloads := make(map[string]bool)
			walkErr := filepath.WalkDir(home.Root(), func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() || entry.Name() != "payload" {
					return err
				}
				data, readErr := os.ReadFile(path)
				if readErr == nil {
					payloads[string(data)] = true
				}
				return readErr
			})
			if rotateErr != nil || !progressed || !state.mutated || before == after || walkErr != nil || len(payloads) != test.nodes+breakerOccupants {
				t.Fatalf("canonical graph = progressed:%v mutated:%v err:%v changed:%v payloads:%v walkErr:%v",
					progressed, state.mutated, rotateErr, before != after, payloads, walkErr)
			}
			for index := 0; index < test.nodes; index++ {
				if !payloads[fmt.Sprintf("node-%02d", index)] {
					t.Fatalf("canonical graph lost node %d: %v", index, payloads)
				}
			}
			for index := 0; index < breakerOccupants; index++ {
				if !payloads[fmt.Sprintf("breaker-%02d", index)] {
					t.Fatalf("graph breaker lost occupant %d: %v", index, payloads)
				}
			}
			active, activeErr := root.lookupEntry(spoolControlDirectoryName)
			if test.saturateBreaker {
				if activeErr != nil || active.metadata.dev != blocker.metadata.dev || active.metadata.ino != blocker.metadata.ino {
					t.Fatalf("graph breaker promotion = active:%+v err:%v blocker:%+v", active, activeErr, blocker)
				}
			} else if !errors.Is(activeErr, fs.ErrNotExist) {
				t.Fatalf("ordinary graph breaker created active control: %+v err:%v", active, activeErr)
			}
		})
	}
}

func TestRetiredControlFullBreakerWithActiveControlMakesRepeatedProgress(t *testing.T) {
	shapes := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "file",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("active-file"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "empty directory",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "nonempty directory",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "payload"), []byte("active-directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			plainRoot := mustOpenMutableRoot(t, home)
			markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
			if err := persistSpoolQuota(plainRoot, markers); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}
			retiredPath := filepath.Join(home.Root(), retiredControlDirectoryName)
			if err := os.Mkdir(retiredPath, 0o700); err != nil {
				t.Fatal(err)
			}
			blockerPath := filepath.Join(retiredPath, "temporary")
			if err := os.Mkdir(blockerPath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(blockerPath, "payload"), []byte("retired-blocker"), 0o600); err != nil {
				t.Fatal(err)
			}
			var blockerStat unix.Stat_t
			if err := unix.Lstat(blockerPath, &blockerStat); err != nil {
				t.Fatal(err)
			}
			blockerName := fmt.Sprintf(".orphan-%x-%x", unixStatDevice(blockerStat), unixStatInode(blockerStat))
			if err := os.Rename(blockerPath, filepath.Join(retiredPath, blockerName)); err != nil {
				t.Fatal(err)
			}
			span := maximumRelocationSequence - uint64(maximumRelocationSlots)
			start := (unixStatDevice(blockerStat) ^ unixStatInode(blockerStat)*0x9e3779b97f4a7c15) % (span + 1)
			for offset := 0; offset < maximumRelocationSlots; offset++ {
				path := filepath.Join(retiredPath, relocationCandidateName(start+uint64(offset)))
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "payload"), []byte(fmt.Sprintf("candidate-%02d", offset)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			activePath := filepath.Join(home.Root(), spoolControlDirectoryName)
			shape.setup(t, activePath)
			var activeStat unix.Stat_t
			if err := unix.Lstat(activePath, &activeStat); err != nil {
				t.Fatal(err)
			}

			physicalOpens := 0
			hooks := storageTestHooks{afterDirectoryOpen: func(string) {
				physicalOpens++
			}}
			root, err := openStorageRootMutableWithHooks(home, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			retired, err := root.openDir([]string{retiredControlDirectoryName}, false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = retired.Close() }()
			blocker, err := retired.lookupEntry(blockerName)
			if err != nil {
				t.Fatal(err)
			}
			state := &spoolSweepState{
				root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()), failClosedArmed: true,
			}
			before := filesystemStateFingerprint(t, home.Root())
			physicalOpens = 0
			progressed, rotateErr := state.rotateRetiredControlCollision(retired, blocker)
			after := filesystemStateFingerprint(t, home.Root())
			opensDuringEscape := physicalOpens
			if rotateErr != nil || !progressed || !state.mutated || before == after || opensDuringEscape != 0 ||
				!hasStrongFailClosedSpoolEvidence(root) {
				t.Fatalf("active %s escape = progressed:%v mutated:%v err:%v changed:%v opens:%d evidence:%v",
					shape.name, progressed, state.mutated, rotateErr, before != after,
					opensDuringEscape, hasStrongFailClosedSpoolEvidence(root))
			}
			foundActiveInode := false
			payloads := make(map[string]bool)
			walkErr := filepath.WalkDir(home.Root(), func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				var stat unix.Stat_t
				if lstatErr := unix.Lstat(path, &stat); lstatErr != nil {
					return lstatErr
				}
				if unixStatDevice(stat) == unixStatDevice(activeStat) && unixStatInode(stat) == unixStatInode(activeStat) {
					foundActiveInode = true
				}
				if entry.IsDir() {
					return nil
				}
				data, readErr := os.ReadFile(path)
				if readErr == nil {
					payloads[string(data)] = true
				}
				return readErr
			})
			if walkErr != nil || !foundActiveInode || !payloads["retired-blocker"] ||
				(shape.name == "nonempty directory" && !payloads["active-directory"]) {
				t.Fatalf("active %s preservation = foundInode:%v payloads:%v err:%v", shape.name, foundActiveInode, payloads, walkErr)
			}
			for index := 0; index < maximumRelocationSlots; index++ {
				if !payloads[fmt.Sprintf("candidate-%02d", index)] {
					t.Fatalf("active %s lost saturated candidate %d: %v", shape.name, index, payloads)
				}
			}
			if shape.name == "file" {
				dataFound := false
				_ = filepath.WalkDir(home.Root(), func(path string, entry fs.DirEntry, err error) error {
					if err == nil && !entry.IsDir() {
						data, readErr := os.ReadFile(path)
						dataFound = dataFound || readErr == nil && string(data) == "active-file"
					}
					return err
				})
				if !dataFound {
					t.Fatal("active file payload was lost")
				}
			}
			if err := retired.Close(); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			root, err = openStorageRootMutableWithHooks(home, hooks)
			if err != nil {
				t.Fatal(err)
			}

			budget := defaultSpoolWorkBudget()
			budget.maxDirectories = 3
			for attempt := 0; attempt < 2; attempt++ {
				before = filesystemStateFingerprint(t, home.Root())
				physicalOpens = 0
				result, purgeErr := purgeSpool(root, budget)
				after = filesystemStateFingerprint(t, home.Root())
				opensDuringPurge := physicalOpens
				if purgeErr != nil || !result.complete && before == after || uint64(opensDuringPurge) > budget.maxDirectories ||
					result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories ||
					result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes ||
					!result.complete && !hasStrongFailClosedSpoolEvidence(root) {
					t.Fatalf("active %s purge %d = complete:%v err:%v changed:%v opens:%d usage:%+v evidence:%v",
						shape.name, attempt+1, result.complete, purgeErr, before != after, opensDuringPurge,
						result.usage, hasStrongFailClosedSpoolEvidence(root))
				}
				if result.complete {
					break
				}
			}
		})
	}
}

func TestDualControlFullBreakerExchangeMakesBoundedPurgeProgress(t *testing.T) {
	shapes := []struct {
		name              string
		alwaysUnsupported bool
		setup             func(*testing.T, string)
	}{
		{
			name: "file",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("active-file"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "empty directory",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "nonempty directory",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "payload"), []byte("active-directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unsupported file", alwaysUnsupported: true,
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("active-file"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unsupported nonempty directory", alwaysUnsupported: true,
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "payload"), []byte("active-directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
			plainRoot := mustOpenMutableRoot(t, home)
			markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
			if err := persistSpoolQuota(plainRoot, markers); err != nil {
				t.Fatal(err)
			}
			if err := plainRoot.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home.Root(), stateLockName), nil, 0o600); err != nil {
				t.Fatal(err)
			}

			retiredPath := filepath.Join(home.Root(), retiredControlDirectoryName)
			temporary := filepath.Join(retiredPath, "temporary")
			sourcePath := filepath.Join(temporary, "r00")
			if err := os.MkdirAll(filepath.Join(sourcePath, "deep"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sourcePath, "deep", "payload"), []byte("source-tree"), 0o600); err != nil {
				t.Fatal(err)
			}
			var topStat unix.Stat_t
			if err := unix.Lstat(temporary, &topStat); err != nil {
				t.Fatal(err)
			}
			topCanonical := fmt.Sprintf(".orphan-%x-%x", unixStatDevice(topStat), unixStatInode(topStat))
			if err := os.Rename(temporary, filepath.Join(retiredPath, topCanonical)); err != nil {
				t.Fatal(err)
			}
			sourcePath = filepath.Join(retiredPath, topCanonical, "r00")
			var sourceStat unix.Stat_t
			if err := unix.Lstat(sourcePath, &sourceStat); err != nil {
				t.Fatal(err)
			}
			sourceCanonical := fmt.Sprintf(".orphan-%x-%x", unixStatDevice(sourceStat), unixStatInode(sourceStat))

			queuePath := filepath.Join(home.Root(), queueDirectoryName, "99999999-9999-4999-8999-999999999999", "deep")
			if err := os.MkdirAll(queuePath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(queuePath, "payload"), []byte("queue-tree"), 0o600); err != nil {
				t.Fatal(err)
			}
			activePath := filepath.Join(home.Root(), spoolControlDirectoryName)
			shape.setup(t, activePath)
			var activeStat unix.Stat_t
			if err := unix.Lstat(activePath, &activeStat); err != nil {
				t.Fatal(err)
			}

			injected := false
			var injectionErr error
			var postInjectionFingerprint string
			injectedInodes := make(map[recordIncarnation]bool)
			injectedPayloads := make(map[string]bool)
			cursorBlocksSaturated := 0
			var cursorHookErr error
			var terminal recordIncarnation
			var terminalName string
			exchangeAttempts := 0
			physicalOpens := 0
			hooks := storageTestHooks{
				beforeMutation: func(step storageStep, sourceName string) {
					if injected || step != storageStepRename || sourceName != "r00" {
						return
					}
					injected = true
					temporaryNames := make([]string, maximumRelocationSlots+1)
					metadata := make([]unix.Stat_t, maximumRelocationSlots+1)
					canonical := make([]string, maximumRelocationSlots+1)
					for index := range temporaryNames {
						temporaryNames[index] = filepath.Join(retiredPath, fmt.Sprintf("injected-%02d", index))
						if injectionErr == nil {
							injectionErr = os.Mkdir(temporaryNames[index], 0o700)
						}
						payload := fmt.Sprintf("chain-%02d", index)
						if injectionErr == nil {
							injectionErr = os.WriteFile(filepath.Join(temporaryNames[index], "payload"), []byte(payload), 0o600)
						}
						if injectionErr == nil {
							injectionErr = unix.Lstat(temporaryNames[index], &metadata[index])
						}
						canonical[index] = fmt.Sprintf(".orphan-%x-%x", unixStatDevice(metadata[index]), unixStatInode(metadata[index]))
						injectedInodes[recordIncarnation{dev: unixStatDevice(metadata[index]), ino: unixStatInode(metadata[index])}] = true
						injectedPayloads[payload] = true
					}
					finalNames := make([]string, len(temporaryNames))
					finalNames[0] = sourceCanonical
					for index := 1; index < len(finalNames); index++ {
						finalNames[index] = canonical[index-1]
					}
					for index := range temporaryNames {
						if injectionErr == nil {
							injectionErr = os.Rename(temporaryNames[index], filepath.Join(retiredPath, finalNames[index]))
						}
					}
					terminal = recordIncarnation{dev: unixStatDevice(metadata[len(metadata)-1]), ino: unixStatInode(metadata[len(metadata)-1])}
					terminalName = finalNames[len(finalNames)-1]
					span := maximumRelocationSequence - uint64(maximumRelocationSlots)
					start := (terminal.dev ^ terminal.ino*0x9e3779b97f4a7c15) % (span + 1)
					for offset := 0; offset < maximumRelocationSlots; offset++ {
						path := filepath.Join(retiredPath, relocationCandidateName(start+uint64(offset)))
						if injectionErr == nil {
							injectionErr = os.Mkdir(path, 0o700)
						}
						payload := fmt.Sprintf("breaker-%02d", offset)
						if injectionErr == nil {
							injectionErr = os.WriteFile(filepath.Join(path, "payload"), []byte(payload), 0o600)
						}
						var stat unix.Stat_t
						if injectionErr == nil {
							injectionErr = unix.Lstat(path, &stat)
						}
						injectedInodes[recordIncarnation{dev: unixStatDevice(stat), ino: unixStatInode(stat)}] = true
						injectedPayloads[payload] = true
					}
					if shape.alwaysUnsupported {
						activeCanonical := fmt.Sprintf(".orphan-%x-%x", unixStatDevice(activeStat), unixStatInode(activeStat))
						activeStart := (unixStatDevice(activeStat) ^ unixStatInode(activeStat)*0x9e3779b97f4a7c15) % (span + 1)
						for index := 0; index <= maximumRelocationSlots; index++ {
							name := activeCanonical
							if index > 0 {
								name = relocationCandidateName(activeStart + uint64(index-1))
							}
							path := filepath.Join(retiredPath, name)
							if injectionErr == nil {
								injectionErr = os.Mkdir(path, 0o700)
							}
							payload := fmt.Sprintf("active-slot-%02d", index)
							if injectionErr == nil {
								injectionErr = os.WriteFile(filepath.Join(path, "payload"), []byte(payload), 0o600)
							}
							var stat unix.Stat_t
							if injectionErr == nil {
								injectionErr = unix.Lstat(path, &stat)
							}
							injectedInodes[recordIncarnation{dev: unixStatDevice(stat), ino: unixStatInode(stat)}] = true
							injectedPayloads[payload] = true
						}
					}
					if injectionErr == nil {
						postInjectionFingerprint = filesystemStateFingerprint(t, home.Root())
					}
				},
				afterAtomicWrite: func(path string, state storageWriteState) {
					if !shape.alwaysUnsupported || filepath.Base(path) != fallbackRelocationCursorName ||
						state != storageWriteAppliedDurable || cursorBlocksSaturated >= 2 || cursorHookErr != nil {
						return
					}
					data, err := os.ReadFile(path)
					if err != nil {
						cursorHookErr = err
						return
					}
					cursor, err := decodeRelocationCursor(data)
					if err != nil || cursor.Next < uint64(maximumRelocationSlots) {
						cursorHookErr = errors.Join(err, errors.New("cursor reservation did not contain a complete block"))
						return
					}
					var cursorStat unix.Stat_t
					if err := unix.Lstat(path, &cursorStat); err != nil {
						cursorHookErr = err
						return
					}
					start := cursor.Next - uint64(maximumRelocationSlots)
					for offset := 0; offset < maximumRelocationSlots; offset++ {
						name := fallbackRelocationCandidateName(recordIncarnation{
							dev: unixStatDevice(cursorStat), ino: unixStatInode(cursorStat),
						}, start+uint64(offset))
						candidatePath := filepath.Join(retiredPath, name)
						if err := os.Mkdir(candidatePath, 0o700); err != nil {
							cursorHookErr = err
							return
						}
						payload := fmt.Sprintf("cursor-block-%02d-%02d", cursorBlocksSaturated, offset)
						if err := os.WriteFile(filepath.Join(candidatePath, "payload"), []byte(payload), 0o600); err != nil {
							cursorHookErr = err
							return
						}
						var stat unix.Stat_t
						if err := unix.Lstat(candidatePath, &stat); err != nil {
							cursorHookErr = err
							return
						}
						injectedInodes[recordIncarnation{dev: unixStatDevice(stat), ino: unixStatInode(stat)}] = true
						injectedPayloads[payload] = true
					}
					cursorBlocksSaturated++
				},
				beforeExchange: func() error {
					exchangeAttempts++
					if shape.alwaysUnsupported || exchangeAttempts == 1 {
						return unix.ENOSYS
					}
					return nil
				},
				afterDirectoryOpen: func(string) { physicalOpens++ },
			}

			budget := defaultSpoolWorkBudget()
			budget.maxDirectories = 4
			root, err := openStorageRootMutableWithHooks(home, hooks)
			if err != nil {
				t.Fatal(err)
			}
			queueBefore := filesystemStateFingerprintIfPresent(t, filepath.Join(home.Root(), queueDirectoryName))
			physicalOpens = 0
			result, purgeErr := purgeSpool(root, budget)
			after := filesystemStateFingerprint(t, home.Root())
			queueAfter := filesystemStateFingerprintIfPresent(t, filepath.Join(home.Root(), queueDirectoryName))
			opensDuring := physicalOpens
			active, activeErr := root.lookupEntry(spoolControlDirectoryName)
			var retiredTerminal unix.Stat_t
			retiredTerminalErr := unix.Lstat(filepath.Join(retiredPath, terminalName), &retiredTerminal)
			escapeStateOK := activeErr == nil && retiredTerminalErr == nil
			if shape.alwaysUnsupported {
				cursorData, cursorErr := os.ReadFile(filepath.Join(home.Root(), fallbackRelocationCursorName))
				cursor, decodeErr := decodeRelocationCursor(cursorData)
				escapeStateOK = escapeStateOK && cursorHookErr == nil && cursorErr == nil && decodeErr == nil &&
					cursorBlocksSaturated == 1 && cursor.Next >= uint64(maximumRelocationSlots) &&
					active.metadata.dev == unixStatDevice(activeStat) && active.metadata.ino == unixStatInode(activeStat) &&
					unixStatDevice(retiredTerminal) == terminal.dev && unixStatInode(retiredTerminal) == terminal.ino
			} else {
				escapeStateOK = escapeStateOK && active.metadata.dev == terminal.dev && active.metadata.ino == terminal.ino &&
					unixStatDevice(retiredTerminal) == unixStatDevice(activeStat) && unixStatInode(retiredTerminal) == unixStatInode(activeStat)
			}
			if injectionErr != nil || !injected || purgeErr != nil || result.complete ||
				postInjectionFingerprint == "" || postInjectionFingerprint == after || queueBefore != queueAfter ||
				exchangeAttempts < 2 || uint64(opensDuring) > budget.maxDirectories ||
				result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories ||
				result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes ||
				!escapeStateOK || !hasStrongFailClosedSpoolEvidence(root) {
				t.Fatalf("dual-control %s escape = injected:%v injectionErr:%v complete:%v purgeErr:%v progressed:%v queueChanged:%v exchanges:%d opens:%d usage:%+v active:%+v activeErr:%v terminalErr:%v evidence:%v",
					shape.name, injected, injectionErr, result.complete, purgeErr, postInjectionFingerprint != after,
					queueBefore != queueAfter, exchangeAttempts, opensDuring, result.usage, active, activeErr,
					retiredTerminalErr, hasStrongFailClosedSpoolEvidence(root))
			}
			foundInodes := make(map[recordIncarnation]bool)
			foundPayloads := make(map[string]bool)
			walkErr := filepath.WalkDir(home.Root(), func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				var stat unix.Stat_t
				if lstatErr := unix.Lstat(path, &stat); lstatErr != nil {
					return lstatErr
				}
				foundInodes[recordIncarnation{dev: unixStatDevice(stat), ino: unixStatInode(stat)}] = true
				if entry.IsDir() {
					return nil
				}
				data, readErr := os.ReadFile(path)
				if readErr == nil {
					foundPayloads[string(data)] = true
				}
				return readErr
			})
			sourceIncarnation := recordIncarnation{dev: unixStatDevice(sourceStat), ino: unixStatInode(sourceStat)}
			if walkErr != nil || !foundInodes[recordIncarnation{dev: unixStatDevice(activeStat), ino: unixStatInode(activeStat)}] ||
				!foundInodes[sourceIncarnation] {
				t.Fatalf("dual-control %s preservation = walkErr:%v activeFound:%v", shape.name, walkErr,
					foundInodes[recordIncarnation{dev: unixStatDevice(activeStat), ino: unixStatInode(activeStat)}])
			}
			for incarnation := range injectedInodes {
				if !foundInodes[incarnation] {
					t.Fatalf("dual-control %s lost inode %+v", shape.name, incarnation)
				}
			}
			for payload := range injectedPayloads {
				if !foundPayloads[payload] {
					t.Fatalf("dual-control %s lost payload %q: %v", shape.name, payload, foundPayloads)
				}
			}
			for _, payload := range []string{"source-tree", "queue-tree"} {
				if !foundPayloads[payload] {
					t.Fatalf("dual-control %s lost fixture payload %q: %v", shape.name, payload, foundPayloads)
				}
			}
			if shape.name == "file" && !foundPayloads["active-file"] ||
				shape.name == "nonempty directory" && !foundPayloads["active-directory"] {
				t.Fatalf("dual-control %s lost active payload: %v", shape.name, foundPayloads)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}

			if shape.alwaysUnsupported {
				queueBefore = filesystemStateFingerprintIfPresent(t, filepath.Join(home.Root(), queueDirectoryName))
				deps := defaultTestServiceDependencies(home, 2)
				deps.newUUID = uuidSequence(t, testEventIDThree)
				deps.now = func() time.Time { return testRecordHour }
				service := mustOpenTestService(t, deps)
				permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
				if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
					t.Fatalf("dual-control %s foreground fallback barrier RecordOnce = %v", shape.name, got)
				}
				_ = permit.Close()
				queueAfter = filesystemStateFingerprintIfPresent(t, filepath.Join(home.Root(), queueDirectoryName))
				if queueBefore != queueAfter {
					t.Fatalf("dual-control %s foreground fallback barrier changed queue", shape.name)
				}
			}

			result = spoolSweepResult{}
			for attempt := 0; attempt < 128 && !result.complete; attempt++ {
				root, err = openStorageRootMutableWithHooks(home, hooks)
				if err != nil {
					t.Fatal(err)
				}
				before := fallbackProgressFingerprint(t, home.Root())
				queueBefore = filesystemStateFingerprintIfPresent(t, filepath.Join(home.Root(), queueDirectoryName))
				_, activeBeforeErr := os.Lstat(filepath.Join(home.Root(), spoolControlDirectoryName))
				_, retiredBeforeErr := os.Lstat(filepath.Join(home.Root(), retiredControlDirectoryName))
				fallbackInProgress := activeBeforeErr == nil && retiredBeforeErr == nil
				passBudget := defaultSpoolWorkBudget()
				if fallbackInProgress {
					passBudget.maxDirectories = spoolMinimumDirectoryProgress
				}
				physicalOpens = 0
				result, purgeErr = purgeSpool(root, passBudget)
				after = fallbackProgressFingerprint(t, home.Root())
				queueAfter = filesystemStateFingerprintIfPresent(t, filepath.Join(home.Root(), queueDirectoryName))
				opensDuring = physicalOpens
				if purgeErr != nil || !result.complete && before == after || fallbackInProgress && queueBefore != queueAfter ||
					uint64(opensDuring) > passBudget.maxDirectories || result.usage.entries > passBudget.maxEntries ||
					result.usage.directories > passBudget.maxDirectories || result.usage.readBytes > passBudget.maxReadBytes ||
					result.usage.nameBytes > passBudget.maxNameBytes || !result.complete && !hasStrongFailClosedSpoolEvidence(root) {
					t.Fatalf("dual-control %s follow-up %d = complete:%v err:%v progressed:%v fallback:%v queueChanged:%v opens:%d usage:%+v evidence:%v",
						shape.name, attempt+1, result.complete, purgeErr, before != after, fallbackInProgress,
						queueBefore != queueAfter, opensDuring, result.usage, hasStrongFailClosedSpoolEvidence(root))
				}
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if !result.complete {
				t.Fatalf("dual-control %s did not converge within bounded passes: %+v", shape.name, result)
			}
			root = mustOpenMutableRoot(t, home)
			if quota := readQuotaFromRoot(t, root); quota != (spoolQuota{}) {
				t.Fatalf("dual-control %s final quota = %+v", shape.name, quota)
			}
			for _, name := range []string{spoolControlDirectoryName, retiredControlDirectoryName, fallbackRelocationCursorName} {
				if _, lookupErr := root.lookupEntry(name); !errors.Is(lookupErr, fs.ErrNotExist) {
					t.Fatalf("dual-control %s final control %q remains: %v", shape.name, name, lookupErr)
				}
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFallbackRelocationCursorCorruptionExhaustionAndCrashReplayReserveDisjointBlocks(t *testing.T) {
	for _, test := range []struct {
		name string
		data func(*testing.T) []byte
	}{
		{name: "corrupt", data: func(*testing.T) []byte { return []byte("not = [toml") }},
		{name: "exhausted", data: func(t *testing.T) []byte {
			data, err := encodeRelocationCursor(relocationCursor{Next: maximumRelocationSequence})
			if err != nil {
				t.Fatal(err)
			}
			return data
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			root := mustOpenMutableRoot(t, home)
			if err := root.writeFileAtomic(fallbackRelocationCursorName, test.data(t)); err != nil {
				t.Fatal(err)
			}
			original, err := root.lookupEntry(fallbackRelocationCursorName)
			if err != nil {
				t.Fatal(err)
			}

			meter := newSpoolWorkMeter(defaultSpoolWorkBudget())
			meter.physicalDirectories = true
			state := &spoolSweepState{root: root, purgeAll: true, meter: meter}
			first, err := state.reserveFallbackRelocationBlock()
			if err != nil || !state.mutated || !state.failClosedArmed || first.slots != maximumRelocationSlots {
				t.Fatalf("first fallback reservation = %+v mutated:%v armed:%v err:%v", first, state.mutated, state.failClosedArmed, err)
			}
			if want := fallbackRelocationRecoveryStart(original); first.start != want {
				t.Fatalf("fallback recovery start = %d, want inode-derived %d", first.start, want)
			}
			firstCursor := readFallbackRelocationCursorFromRoot(t, root)
			if firstCursor.Next != first.start+uint64(maximumRelocationSlots) {
				t.Fatalf("first fallback cursor = %+v, reservation=%+v", firstCursor, first)
			}
			firstNames := make(map[string]bool, maximumRelocationSlots)
			for offset := 0; offset < maximumRelocationSlots; offset++ {
				firstNames[fallbackRelocationCandidateName(first.cursor, first.start+uint64(offset))] = true
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}

			root = mustOpenMutableRoot(t, home)
			defer func() { _ = root.Close() }()
			meter = newSpoolWorkMeter(defaultSpoolWorkBudget())
			meter.physicalDirectories = true
			replay := &spoolSweepState{root: root, purgeAll: true, meter: meter}
			second, err := replay.reserveFallbackRelocationBlock()
			if err != nil || !replay.mutated || second.slots != maximumRelocationSlots || second.cursor == first.cursor {
				t.Fatalf("replayed fallback reservation = %+v first:%+v mutated:%v err:%v", second, first, replay.mutated, err)
			}
			if firstCursor.Next <= maximumRelocationSequence-uint64(maximumRelocationSlots) && second.start != firstCursor.Next {
				t.Fatalf("replayed fallback start = %d, want skipped high-water %d", second.start, firstCursor.Next)
			}
			for offset := 0; offset < maximumRelocationSlots; offset++ {
				name := fallbackRelocationCandidateName(second.cursor, second.start+uint64(offset))
				if firstNames[name] {
					t.Fatalf("replayed fallback reused candidate %q", name)
				}
			}
		})
	}
}

func TestFallbackRelocationCursorReservesCompleteLogicalBlockOrNothing(t *testing.T) {
	home := newMetricsTestHome(t)
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	worstCaseName := fallbackRelocationCandidateName(recordIncarnation{dev: math.MaxUint64, ino: math.MaxUint64}, maximumRelocationSequence)
	budget := spoolWorkBudget{
		// The cursor lookup consumes one entry. Eight remaining entries can
		// name the candidates but cannot also revalidate the installed cursor.
		maxEntries: 9, maxDirectories: 1, maxReadBytes: maximumCleanupReadBytes,
		maxNameBytes: uint64(len(fallbackRelocationCursorName)) + 8*uint64(len(worstCaseName)),
	}
	state := &spoolSweepState{root: root, purgeAll: true, meter: newSpoolWorkMeter(budget)}
	reservation, err := state.reserveFallbackRelocationBlock()
	if err == nil || state.mutated || reservation != (fallbackRelocationReservation{}) {
		t.Fatalf("partial logical fallback reservation = %+v mutated:%v err:%v", reservation, state.mutated, err)
	}
	if _, lookupErr := root.lookupEntry(fallbackRelocationCursorName); !errors.Is(lookupErr, fs.ErrNotExist) {
		t.Fatalf("partial logical fallback reservation persisted a cursor: %v", lookupErr)
	}
}

func TestFallbackRelocationParksExactAnyKindActiveEntry(t *testing.T) {
	for _, shape := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "file", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("active-file"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "nonempty directory", setup: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "payload"), []byte("active-directory"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			root := mustOpenMutableRoot(t, home)
			activePath := filepath.Join(home.Root(), spoolControlDirectoryName)
			var symlinkSentinel string
			if shape.name == "symlink" {
				symlinkSentinel = filepath.Join(t.TempDir(), "outside-sentinel")
				if err := os.WriteFile(symlinkSentinel, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(symlinkSentinel, activePath); err != nil {
					t.Fatal(err)
				}
			} else {
				shape.setup(t, activePath)
			}
			active, err := root.lookupEntry(spoolControlDirectoryName)
			if err != nil {
				t.Fatal(err)
			}
			retired, err := root.openDir([]string{retiredControlDirectoryName}, true)
			if err != nil {
				t.Fatal(err)
			}
			meter := newSpoolWorkMeter(defaultSpoolWorkBudget())
			meter.physicalDirectories = true
			state := &spoolSweepState{root: root, purgeAll: true, meter: meter, failClosedArmed: true}
			progressed, parkErr := state.parkActiveControlInRetired(retired, active)
			if !progressed || parkErr != nil || !state.mutated {
				t.Fatalf("park %s = progressed:%v mutated:%v err:%v", shape.name, progressed, state.mutated, parkErr)
			}
			if _, err := root.lookupEntry(spoolControlDirectoryName); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("park %s left active name: %v", shape.name, err)
			}
			cursorEntry, err := root.lookupEntry(fallbackRelocationCursorName)
			if err != nil {
				t.Fatal(err)
			}
			cursor := readFallbackRelocationCursorFromRoot(t, root)
			if cursor.Next != maximumRelocationSlots {
				t.Fatalf("park %s cursor = %+v", shape.name, cursor)
			}
			targetName := fallbackRelocationCandidateName(recordIncarnation{
				dev: cursorEntry.metadata.dev, ino: cursorEntry.metadata.ino,
			}, 0)
			parked, err := retired.lookupEntry(targetName)
			if err != nil || parked.metadata.dev != active.metadata.dev || parked.metadata.ino != active.metadata.ino ||
				parked.metadata.kind != active.metadata.kind {
				t.Fatalf("park %s target = %+v active=%+v err:%v", shape.name, parked, active, err)
			}
			switch shape.name {
			case "file":
				data, err := os.ReadFile(filepath.Join(home.Root(), retiredControlDirectoryName, targetName))
				if err != nil || string(data) != "active-file" {
					t.Fatalf("parked file payload = %q err:%v", data, err)
				}
			case "nonempty directory":
				data, err := os.ReadFile(filepath.Join(home.Root(), retiredControlDirectoryName, targetName, "payload"))
				if err != nil || string(data) != "active-directory" {
					t.Fatalf("parked directory payload = %q err:%v", data, err)
				}
			case "symlink":
				parkedPath := filepath.Join(home.Root(), retiredControlDirectoryName, targetName)
				link, err := os.Readlink(parkedPath)
				data, readErr := os.ReadFile(symlinkSentinel)
				if err != nil || link != symlinkSentinel || readErr != nil || string(data) != "outside" {
					t.Fatalf("parked symlink = %q err:%v sentinel=%q readErr:%v", link, err, data, readErr)
				}
			}
			if err := retired.Close(); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFallbackRelocationReservationCrashReplaySkipsReservedBlockAndConverges(t *testing.T) {
	home := newMetricsTestHome(t)
	root := mustOpenMutableRoot(t, home)
	activePath := filepath.Join(home.Root(), spoolControlDirectoryName)
	if err := os.WriteFile(activePath, []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	active, err := root.lookupEntry(spoolControlDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := root.openDir([]string{retiredControlDirectoryName}, true)
	if err != nil {
		t.Fatal(err)
	}
	meter := newSpoolWorkMeter(defaultSpoolWorkBudget())
	meter.physicalDirectories = true
	injected := false
	state := &spoolSweepState{
		root: root, purgeAll: true, meter: meter, failClosedArmed: true,
		afterRelocationReservation: func() error {
			injected = true
			return errors.New("simulated crash after durable fallback reservation")
		},
	}
	progressed, parkErr := state.parkActiveControlInRetired(retired, active)
	if !injected || !progressed || parkErr == nil || !state.mutated {
		t.Fatalf("fallback reservation crash = injected:%v progressed:%v mutated:%v err:%v", injected, progressed, state.mutated, parkErr)
	}
	firstCursorEntry, err := root.lookupEntry(fallbackRelocationCursorName)
	if err != nil {
		t.Fatal(err)
	}
	firstCursor := readFallbackRelocationCursorFromRoot(t, root)
	if firstCursor.Next != maximumRelocationSlots {
		t.Fatalf("fallback reservation crash cursor = %+v", firstCursor)
	}
	if current, err := root.lookupEntry(spoolControlDirectoryName); err != nil ||
		current.metadata.dev != active.metadata.dev || current.metadata.ino != active.metadata.ino {
		t.Fatalf("fallback reservation crash moved active entry: current=%+v err:%v", current, err)
	}
	firstCursorIncarnation := recordIncarnation{dev: firstCursorEntry.metadata.dev, ino: firstCursorEntry.metadata.ino}
	if err := retired.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	root = mustOpenMutableRoot(t, home)
	retired, err = root.openDir([]string{retiredControlDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	active, err = root.lookupEntry(spoolControlDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	meter = newSpoolWorkMeter(defaultSpoolWorkBudget())
	meter.physicalDirectories = true
	replay := &spoolSweepState{root: root, purgeAll: true, meter: meter, failClosedArmed: true}
	progressed, parkErr = replay.parkActiveControlInRetired(retired, active)
	if !progressed || parkErr != nil || !replay.mutated {
		t.Fatalf("fallback reservation replay = progressed:%v mutated:%v err:%v", progressed, replay.mutated, parkErr)
	}
	secondCursorEntry, err := root.lookupEntry(fallbackRelocationCursorName)
	if err != nil {
		t.Fatal(err)
	}
	secondCursor := readFallbackRelocationCursorFromRoot(t, root)
	secondCursorIncarnation := recordIncarnation{dev: secondCursorEntry.metadata.dev, ino: secondCursorEntry.metadata.ino}
	if secondCursor.Next != 2*maximumRelocationSlots || secondCursorIncarnation == firstCursorIncarnation {
		t.Fatalf("fallback reservation replay cursor = %+v incarnation=%+v first=%+v", secondCursor, secondCursorIncarnation, firstCursorIncarnation)
	}
	secondTarget := fallbackRelocationCandidateName(secondCursorIncarnation, maximumRelocationSlots)
	parked, err := retired.lookupEntry(secondTarget)
	if err != nil || parked.metadata.dev != active.metadata.dev || parked.metadata.ino != active.metadata.ino {
		t.Fatalf("fallback reservation replay target = %+v active=%+v err:%v", parked, active, err)
	}
	for sequence := uint64(0); sequence < maximumRelocationSlots; sequence++ {
		if _, err := retired.lookupEntry(fallbackRelocationCandidateName(firstCursorIncarnation, sequence)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("fallback replay populated previously reserved sequence %d: %v", sequence, err)
		}
	}
	if err := retired.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	result := spoolSweepResult{}
	for attempts := 0; attempts < 32 && !result.complete; attempts++ {
		root = mustOpenMutableRoot(t, home)
		result, err = purgeSpool(root, defaultSpoolWorkBudget())
		closeErr := root.Close()
		if err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
	}
	if !result.complete || result.quota != (spoolQuota{}) {
		t.Fatalf("fallback reservation replay did not converge: %+v", result)
	}
	root = mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	for _, name := range []string{spoolControlDirectoryName, retiredControlDirectoryName, fallbackRelocationCursorName} {
		if _, err := root.lookupEntry(name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("fallback reservation replay left %q: %v", name, err)
		}
	}
}

func TestFallbackCursorCleanupCrashWindowKeepsForegroundFailClosed(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	if got := service.RecordOnce(permit, CommandHelp); got != RecordStored {
		t.Fatalf("fixture RecordOnce = %v", got)
	}
	_ = permit.Close()
	originalQueue := filesystemStateFingerprint(t, filepath.Join(home.Root(), queueDirectoryName))
	originalQuota := readQuotaFixture(t, home)

	plainRoot := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuotaDirect(plainRoot, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	cursorData, err := encodeRelocationCursor(relocationCursor{Next: maximumRelocationSlots})
	if err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.writeFileAtomic(fallbackRelocationCursorName, cursorData); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	cursorMutationStarted := false
	cursorParentSynced := false
	failQuotaStageSync := false
	injected := false
	hooks := storageTestHooks{
		beforeMutation: func(step storageStep, path string) {
			if step == storageStepUnlink && filepath.Base(path) == fallbackRelocationCursorName {
				cursorMutationStarted = true
			}
		},
		beforeTempFileCreate: func(path string) {
			if cursorParentSynced && filepath.Base(filepath.Dir(path)) == spoolControlDirectoryName {
				failQuotaStageSync = true
			}
		},
		beforeStep: func(step storageStep) error {
			if cursorMutationStarted && !cursorParentSynced && step == storageStepDirectorySync {
				cursorParentSynced = true
				return nil
			}
			if failQuotaStageSync && !injected && step == storageStepFileSync {
				injected = true
				return errors.New("simulated crash during post-cursor quota persistence")
			}
			return nil
		},
	}
	root, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	result, reconcileErr := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
	if !cursorMutationStarted || !cursorParentSynced || !injected || reconcileErr == nil || result.complete {
		t.Fatalf("fallback cursor cleanup crash = mutation:%v parentSynced:%v injected:%v complete:%v err:%v",
			cursorMutationStarted, cursorParentSynced, injected, result.complete, reconcileErr)
	}
	if _, err := root.lookupEntry(fallbackRelocationCursorName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fallback cursor survived applied cleanup: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	root = mustOpenMutableRoot(t, home)
	if !hasStrongFailClosedSpoolEvidence(root) {
		t.Fatal("cursor cleanup crash reopened without durable fail-closed evidence")
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDTwo)
	deps.now = func() time.Time { return testRecordHour }
	afterCrashService := mustOpenTestService(t, deps)
	afterCrashPermit := afterCrashService.RecordingPermit(recordableInvocationAt(testRecordHour))
	if got := afterCrashService.RecordOnce(afterCrashPermit, CommandVersion); got != RecordDropped {
		t.Fatalf("post-cursor-cleanup crash RecordOnce = %v", got)
	}
	_ = afterCrashPermit.Close()
	if queue := filesystemStateFingerprint(t, filepath.Join(home.Root(), queueDirectoryName)); queue != originalQueue {
		t.Fatal("post-cursor-cleanup crash foreground attempt changed queue")
	}

	result = spoolSweepResult{}
	for attempts := 0; attempts < 32 && !result.complete; attempts++ {
		root = mustOpenMutableRoot(t, home)
		result, err = reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
		closeErr := root.Close()
		if err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
	}
	if !result.complete || result.quota != originalQuota {
		t.Fatalf("fallback cursor cleanup crash did not reconcile exact quota: result=%+v want=%+v", result, originalQuota)
	}
}

func TestDualControlCollisionPassMakesMonotonicProgress(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	defer func() { _ = fixture.root.Close() }()
	activePath := filepath.Join(fixture.home.Root(), spoolControlDirectoryName)
	activeDeep := filepath.Join(activePath, relocationCursorFileName, "deep")
	if err := os.MkdirAll(activeDeep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(activeDeep, "payload"), []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	activeBefore, err := os.Lstat(activePath)
	if err != nil {
		t.Fatal(err)
	}
	retiredPath := filepath.Join(fixture.home.Root(), retiredControlDirectoryName)
	if err := os.Mkdir(retiredPath, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "outside-sentinel")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinelBefore, err := os.Lstat(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(retiredPath, "outside-link")); err != nil {
		t.Fatal(err)
	}
	retiredBefore, err := os.ReadDir(retiredPath)
	if err != nil {
		t.Fatal(err)
	}
	eventTreeBefore := filesystemStateFingerprint(t, filepath.Join(fixture.home.Root(), queueDirectoryName))
	hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	fixture.installHooks(t, hooks)
	entry, err := fixture.generation.lookupEntry(fixture.sourceName)
	if err != nil {
		t.Fatal(err)
	}
	budget := defaultSpoolWorkBudget()
	meter := exhaustedCollisionMeter(budget, fixture.sourceName)
	state := &spoolSweepState{root: fixture.root, purgeAll: true, meter: meter}
	state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
	retiredAfter, retiredErr := os.ReadDir(retiredPath)
	activeAfter, activeErr := os.Lstat(activePath)
	eventTreeAfter := filesystemStateFingerprint(t, filepath.Join(fixture.home.Root(), queueDirectoryName))
	if state.operation != nil || !state.mutated || retiredErr != nil || len(retiredAfter) >= len(retiredBefore) ||
		activeErr != nil || !os.SameFile(activeBefore, activeAfter) || eventTreeAfter != eventTreeBefore {
		t.Fatalf("dual-control ranked progress = mutated:%v err:%v retired:%d->%d retiredErr:%v activeSame:%v activeErr:%v eventTreeChanged:%v usage:%+v",
			state.mutated, state.operation, len(retiredBefore), len(retiredAfter), retiredErr,
			activeErr == nil && os.SameFile(activeBefore, activeAfter), activeErr, eventTreeAfter != eventTreeBefore, meter.usage)
	}
	if meter.usage.entries > budget.maxEntries || meter.usage.directories > budget.maxDirectories ||
		meter.usage.readBytes > budget.maxReadBytes || meter.usage.nameBytes > budget.maxNameBytes {
		t.Fatalf("dual-control collision exceeded budget: usage=%+v budget=%+v", meter.usage, budget)
	}
	deps := defaultTestServiceDependencies(fixture.home, 2)
	deps.newUUID = uuidSequence(t, testEventIDThree)
	deps.now = func() time.Time { return testRecordHour }
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
		t.Fatalf("deferred dual-control RecordOnce = %v", got)
	}
	_ = permit.Close()
	sentinelAfter, sentinelErr := os.Lstat(sentinel)
	data, readErr := os.ReadFile(sentinel)
	if sentinelErr != nil || readErr != nil || string(data) != "outside" || !os.SameFile(sentinelBefore, sentinelAfter) ||
		sentinelBefore.Mode() != sentinelAfter.Mode() {
		t.Fatalf("dual-control cleanup changed outside sentinel: same=%v beforeMode=%v after=%v data=%q readErr=%v",
			sentinelErr == nil && os.SameFile(sentinelBefore, sentinelAfter), sentinelBefore.Mode(), sentinelAfter, data, readErr)
	}
	if state.retainedControl != nil {
		_ = state.retainedControl.Close()
	}
}

func TestUnsafeQuotaShapesConvergeWithoutFollowingEntries(t *testing.T) {
	type quotaShape struct {
		name            string
		unsafe          bool
		safeReplaceable bool
		setup           func(*testing.T, string, string)
	}
	shapes := []quotaShape{
		{
			name: "symlink", unsafe: true,
			setup: func(t *testing.T, quotaPath, sentinel string) {
				if err := os.Symlink(sentinel, quotaPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fifo", unsafe: true,
			setup: func(t *testing.T, quotaPath, _ string) {
				if err := unix.Mkfifo(quotaPath, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "private-empty-directory", unsafe: true,
			setup: func(t *testing.T, quotaPath, _ string) {
				if err := os.Mkdir(quotaPath, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "private-deep-directory", unsafe: true,
			setup: func(t *testing.T, quotaPath, sentinel string) {
				deep := filepath.Join(quotaPath, "deep", "deeper")
				if err := os.MkdirAll(deep, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, filepath.Join(deep, "outside-link")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "nonempty-lax-directory", unsafe: true,
			setup: func(t *testing.T, quotaPath, sentinel string) {
				deep := filepath.Join(quotaPath, "deep", "deeper")
				if err := os.MkdirAll(deep, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, filepath.Join(deep, "outside-link")); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(quotaPath, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hard-link", unsafe: true,
			setup: func(t *testing.T, quotaPath, sentinel string) {
				if err := os.Link(sentinel, quotaPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lax-regular", unsafe: true,
			setup: func(t *testing.T, quotaPath, _ string) {
				if err := os.WriteFile(quotaPath, []byte("lax"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(quotaPath, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt-safe-regular", safeReplaceable: true,
			setup: func(t *testing.T, quotaPath, _ string) {
				if err := os.WriteFile(quotaPath, []byte("not = [toml"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized-safe-regular", safeReplaceable: true,
			setup: func(t *testing.T, quotaPath, _ string) {
				if err := os.WriteFile(quotaPath, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(quotaPath, maximumQuotaBytes+1); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	operations := []struct {
		name string
		run  func(*storageRoot, spoolWorkBudget) (spoolSweepResult, error)
	}{
		{name: "purge", run: purgeSpool},
		{name: "reconcile", run: func(root *storageRoot, budget spoolWorkBudget) (spoolSweepResult, error) {
			return reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, budget)
		}},
	}

	for _, shape := range shapes {
		for _, operation := range operations {
			t.Run(shape.name+"/"+operation.name, func(t *testing.T) {
				home := newMetricsTestHome(t)
				writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
				sentinel := filepath.Join(t.TempDir(), "outside-sentinel")
				if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				sentinelBefore, err := os.Lstat(sentinel)
				if err != nil {
					t.Fatal(err)
				}
				quotaPath := filepath.Join(home.Root(), quotaFileName)
				shape.setup(t, quotaPath, sentinel)
				// Foreground probes legitimately create the stable advisory-lock
				// inode. Seed it before the monotonic baseline so the fingerprint
				// measures only quota/control/event cleanup progress.
				if err := os.WriteFile(filepath.Join(home.Root(), stateLockName), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				quotaReplacements := 0
				unsafeQuotaReads := 0
				unsafePhase := shape.unsafe
				noteQuotaRead := func(path string, _, read int, _ error) {
					if path == quotaPath && unsafePhase {
						unsafeQuotaReads += read
					}
				}
				root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
					afterRead: noteQuotaRead,
					afterRename: func(_, target string, state storageRenameState) {
						if target == quotaPath && state != storageRenameNotApplied {
							quotaReplacements++
						}
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()
				budget := spoolWorkBudget{
					maxEntries: spoolFixedEntryEnvelope + 64, maxDirectories: spoolMinimumDirectoryProgress,
					maxReadBytes: spoolFixedReadEnvelope + 3*maximumControlFileBytes,
					maxNameBytes: spoolFixedNameEnvelope + 8192,
				}
				result := spoolSweepResult{}
				attempts := 0
				previousFingerprint := filesystemStateFingerprint(t, home.Root())
				for ; attempts < 32 && !result.complete; attempts++ {
					result, err = operation.run(root, budget)
					if err != nil {
						t.Fatalf("attempt %d: %v", attempts+1, err)
					}
					if result.usage.entries > budget.maxEntries || result.usage.directories > budget.maxDirectories ||
						result.usage.readBytes > budget.maxReadBytes || result.usage.nameBytes > budget.maxNameBytes {
						t.Fatalf("attempt %d exceeded budget: usage=%+v budget=%+v", attempts+1, result.usage, budget)
					}
					if entry, lookupErr := root.lookupEntry(quotaFileName); lookupErr == nil &&
						entry.metadata.mode&unix.S_IFMT == unix.S_IFREG && entry.metadata.nlink == 1 &&
						privateFilePermissions(entry.metadata.mode) {
						unsafePhase = false
					}
					if !result.complete {
						currentFingerprint := filesystemStateFingerprint(t, home.Root())
						if currentFingerprint == previousFingerprint {
							t.Fatalf("attempt %d made no monotonic filesystem progress", attempts+1)
						}
						previousFingerprint = currentFingerprint
						deps := defaultTestServiceDependencies(home, 2)
						deps.newUUID = uuidSequence(t, testEventIDThree)
						deps.now = func() time.Time { return testRecordHour }
						deps.storageHooks.afterRead = noteQuotaRead
						service := mustOpenTestService(t, deps)
						permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
						if got := service.RecordOnce(permit, CommandHelp); got != RecordDropped {
							t.Fatalf("attempt %d fail-closed RecordOnce = %v", attempts+1, got)
						}
						_ = permit.Close()
						if afterProbe := filesystemStateFingerprint(t, home.Root()); afterProbe != currentFingerprint {
							t.Fatalf("attempt %d foreground probe changed deferred cleanup state", attempts+1)
						}
						eventPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(testEventIDThree))
						if _, err := os.Lstat(eventPath); !errors.Is(err, fs.ErrNotExist) {
							t.Fatalf("attempt %d foreground probe queued an event: %v", attempts+1, err)
						}
					}
				}
				if !result.complete {
					t.Fatalf("unsafe quota shape did not converge after %d attempts: %+v", attempts, result)
				}
				if shape.safeReplaceable && attempts != 1 {
					t.Fatalf("safe invalid quota needed %d attempts, want one atomic replacement", attempts)
				}
				if quotaReplacements == 0 {
					t.Fatal("convergence never installed a replacement quota")
				}
				if shape.unsafe && unsafeQuotaReads != 0 {
					t.Fatalf("unsafe quota content was physically read: %d bytes", unsafeQuotaReads)
				}
				after, err := os.Lstat(quotaPath)
				if err != nil || !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 {
					t.Fatalf("final quota shape = after:%v err:%v", after, err)
				}
				quotaEntry, err := root.lookupEntry(quotaFileName)
				if err != nil || quotaEntry.metadata.nlink != 1 {
					t.Fatalf("final quota link metadata = %+v, err=%v", quotaEntry.metadata, err)
				}
				quota, present, err := loadSpoolQuota(root)
				if err != nil || !present || quota != (spoolQuota{}) {
					t.Fatalf("final quota = (%+v, %v, %v)", quota, present, err)
				}
				for _, controlName := range []string{spoolControlDirectoryName, retiredControlDirectoryName} {
					if _, err := root.lookupEntry(controlName); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("control namespace %q remains: %v", controlName, err)
					}
				}
				sentinelAfter, sentinelErr := os.Lstat(sentinel)
				data, readErr := os.ReadFile(sentinel)
				var sentinelStat unix.Stat_t
				statErr := unix.Lstat(sentinel, &sentinelStat)
				if sentinelErr != nil || readErr != nil || statErr != nil || string(data) != "outside" ||
					!os.SameFile(sentinelBefore, sentinelAfter) || sentinelBefore.Mode() != sentinelAfter.Mode() || sentinelStat.Nlink != 1 {
					t.Fatalf("outside sentinel changed: same=%v beforeMode=%v after=%v data=%q readErr=%v statErr=%v nlink=%d",
						sentinelErr == nil && os.SameFile(sentinelBefore, sentinelAfter), sentinelBefore.Mode(), sentinelAfter, data, readErr, statErr, sentinelStat.Nlink)
				}
			})
		}
	}
}

func TestEventInstallCrashWindowCannotLeaveTwoNamesForOneReservation(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDOne)
	generationPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
	targetPath := filepath.Join(generationPath, eventFileName(testEventIDOne))
	var eventTempPath string
	crashInjected := false
	service.deps.storageHooks.beforeTempFileCreate = func(path string) {
		if filepath.Dir(path) == generationPath {
			eventTempPath = path
		}
	}
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if step != storageStepRename || eventTempPath == "" || crashInjected {
			return nil
		}
		if err := os.Rename(eventTempPath, targetPath); err != nil {
			return err
		}
		if err := os.Chmod(generationPath, 0o500); err != nil {
			return err
		}
		crashInjected = true
		return unix.EINTR
	}
	t.Cleanup(func() { _ = os.Chmod(generationPath, 0o700) })
	firstResult := service.RecordOnce(permit, CommandHelp)
	if err := os.Chmod(generationPath, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, readDirErr := os.ReadDir(generationPath)
	quota := readQuotaFixture(t, home)
	duplicateNames := countDuplicateStorageNames(t, entries)

	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDTwo)
	deps.now = func() time.Time { return testRecordHour }
	secondService := mustOpenTestService(t, deps)
	secondPermit := secondService.RecordingPermit(recordableInvocationAt(testRecordHour))
	secondResult := secondService.RecordOnce(secondPermit, CommandVersion)
	_ = secondPermit.Close()
	finalEntries, finalReadDirErr := os.ReadDir(generationPath)
	finalQuota := readQuotaFixture(t, home)
	finalDuplicates := countDuplicateStorageNames(t, finalEntries)
	temporaryNames := 0
	for _, entry := range finalEntries {
		if strings.HasPrefix(entry.Name(), ".pm-tmp-") {
			temporaryNames++
		}
	}
	if !crashInjected || firstResult != RecordStored || secondResult != RecordStored ||
		readDirErr != nil || duplicateNames > 1 || len(entries) > int(quota.Events) ||
		finalReadDirErr != nil || finalDuplicates > 1 || temporaryNames != 0 || len(finalEntries) > int(finalQuota.Events) {
		t.Fatalf("event install crash window = injected:%v first:%v readDirErr:%v entries:%v duplicateNames:%d quota:%+v second:%v finalReadDirErr:%v finalEntries:%v finalDuplicates:%d temps:%d finalQuota:%+v temp:%q target:%q",
			crashInjected, firstResult, readDirErr, entries, duplicateNames, quota, secondResult,
			finalReadDirErr, finalEntries, finalDuplicates, temporaryNames, finalQuota, eventTempPath, targetPath)
	}
}

func countDuplicateStorageNames(t *testing.T, entries []os.DirEntry) int {
	t.Helper()
	maximumLinks := 0
	for _, entry := range entries {
		leftInfo, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		links := 0
		for right := range entries {
			rightInfo, err := entries[right].Info()
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(leftInfo, rightInfo) {
				links++
			}
		}
		if links > maximumLinks {
			maximumLinks = links
		}
	}
	return maximumLinks
}

func TestLoneRetiredControlCannotBeRemovedWithoutReplacementEvidence(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	defer func() { _ = fixture.root.Close() }()
	retiredPath := filepath.Join(fixture.home.Root(), retiredControlDirectoryName)
	if err := os.WriteFile(retiredPath, []byte("retired-evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	fixture.installHooks(t, hooks)
	entry, err := fixture.generation.lookupEntry(fixture.sourceName)
	if err != nil {
		t.Fatal(err)
	}
	meter := exhaustedCollisionMeter(defaultSpoolWorkBudget(), fixture.sourceName)
	state := &spoolSweepState{root: fixture.root, purgeAll: true, meter: meter}
	state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
	if state.retainedControl != nil {
		_ = state.retainedControl.Close()
		state.retainedControl = nil
	}
	quota, present, quotaErr := loadSpoolQuota(fixture.root)
	_, activeErr := fixture.root.lookupEntry(spoolControlDirectoryName)
	_, retiredErr := fixture.root.lookupEntry(retiredControlDirectoryName)
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	strongEvidence := quotaErr == nil && present && quota == markers || activeErr == nil || retiredErr == nil

	deps := defaultTestServiceDependencies(fixture.home, 2)
	deps.newUUID = uuidSequence(t, testEventIDThree)
	deps.now = func() time.Time { return testRecordHour }
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	recordResult := service.RecordOnce(permit, CommandHelp)
	_ = permit.Close()
	if state.operation != nil || !state.mutated || !strongEvidence || recordResult != RecordDropped {
		t.Fatalf("lone retired recovery = mutated:%v err:%v quota:%+v present:%v quotaErr:%v activeErr:%v retiredErr:%v record:%v",
			state.mutated, state.operation, quota, present, quotaErr, activeErr, retiredErr, recordResult)
	}
}

func TestLoneRetiredDirectoryCannotBeRemovedWithoutReplacementEvidence(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	defer func() { _ = fixture.root.Close() }()
	retiredPath := filepath.Join(fixture.home.Root(), retiredControlDirectoryName)
	if err := os.Mkdir(retiredPath, 0o700); err != nil {
		t.Fatal(err)
	}
	hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	fixture.installHooks(t, hooks)
	entry, err := fixture.generation.lookupEntry(fixture.sourceName)
	if err != nil {
		t.Fatal(err)
	}
	meter := exhaustedCollisionMeter(defaultSpoolWorkBudget(), fixture.sourceName)
	state := &spoolSweepState{root: fixture.root, purgeAll: true, meter: meter}
	state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
	if state.retainedControl != nil {
		_ = state.retainedControl.Close()
		state.retainedControl = nil
	}
	strongEvidence := hasStrongFailClosedSpoolEvidence(fixture.root)
	deps := defaultTestServiceDependencies(fixture.home, 2)
	deps.newUUID = uuidSequence(t, testEventIDThree)
	deps.now = func() time.Time { return testRecordHour }
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	recordResult := service.RecordOnce(permit, CommandHelp)
	_ = permit.Close()
	if state.operation != nil || !state.mutated || !strongEvidence || recordResult != RecordDropped {
		t.Fatalf("lone retired directory recovery = mutated:%v err:%v strongEvidence:%v record:%v",
			state.mutated, state.operation, strongEvidence, recordResult)
	}
}

func TestUnsafeLoneActiveControlMakesExhaustedUnsupportedProgress(t *testing.T) {
	for _, shape := range []string{"file", "fifo", "symlink"} {
		t.Run(shape, func(t *testing.T) {
			fixture := newQuarantineCollisionFixture(t, "directory-chain")
			defer func() { _ = fixture.root.Close() }()
			activePath := filepath.Join(fixture.home.Root(), spoolControlDirectoryName)
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(sentinel, []byte("sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			sentinelBefore, err := os.Lstat(sentinel)
			if err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "file":
				err = os.WriteFile(activePath, []byte("unsafe-active"), 0o600)
			case "fifo":
				err = unix.Mkfifo(activePath, 0o600)
			case "symlink":
				err = os.Symlink(sentinel, activePath)
			}
			if err != nil {
				t.Fatal(err)
			}
			hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
			fixture.installHooks(t, hooks)
			entry, err := fixture.generation.lookupEntry(fixture.sourceName)
			if err != nil {
				t.Fatal(err)
			}
			before := filesystemStateFingerprint(t, fixture.home.Root())
			meter := exhaustedCollisionMeter(defaultSpoolWorkBudget(), fixture.sourceName)
			state := &spoolSweepState{root: fixture.root, purgeAll: true, meter: meter}
			state.quarantineDirectory(fixture.generation, fixture.tree, entry, true, true)
			if state.retainedControl != nil {
				_ = state.retainedControl.Close()
				state.retainedControl = nil
			}
			after := filesystemStateFingerprint(t, fixture.home.Root())

			deps := defaultTestServiceDependencies(fixture.home, 2)
			deps.newUUID = uuidSequence(t, testEventIDThree)
			deps.now = func() time.Time { return testRecordHour }
			service := mustOpenTestService(t, deps)
			permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
			recordResult := service.RecordOnce(permit, CommandHelp)
			_ = permit.Close()
			converged := false
			interleavedStored := false
			var convergenceErr error
			for attempt := 0; attempt < 128; attempt++ {
				result, purgeErr := purgeSpool(fixture.root, defaultSpoolWorkBudget())
				if purgeErr != nil {
					convergenceErr = purgeErr
					break
				}
				if result.complete {
					converged = true
					break
				}
				if !hasStrongFailClosedSpoolEvidence(fixture.root) {
					convergenceErr = errors.New("incomplete unsafe-active purge lost its durable barrier")
					break
				}
				attemptDeps := defaultTestServiceDependencies(fixture.home, 2)
				attemptDeps.newUUID = uuidSequence(t, testEventIDThree)
				attemptDeps.now = func() time.Time { return testRecordHour }
				attemptService := mustOpenTestService(t, attemptDeps)
				attemptPermit := attemptService.RecordingPermit(recordableInvocationAt(testRecordHour))
				if attemptService.RecordOnce(attemptPermit, CommandHelp) != RecordDropped {
					interleavedStored = true
				}
				_ = attemptPermit.Close()
			}
			sentinelAfter, sentinelErr := os.Lstat(sentinel)
			data, readErr := os.ReadFile(sentinel)
			if state.operation != nil || !state.mutated || before == after || recordResult != RecordDropped ||
				!converged || convergenceErr != nil || interleavedStored ||
				sentinelErr != nil || readErr != nil || string(data) != "sentinel" || !os.SameFile(sentinelBefore, sentinelAfter) {
				t.Fatalf("unsafe active %s boundary = mutated:%v err:%v changed:%v record:%v converged:%v convergenceErr:%v interleavedStored:%v sentinelSame:%v sentinelErr:%v data:%q readErr:%v",
					shape, state.mutated, state.operation, before != after, recordResult,
					converged, convergenceErr, interleavedStored,
					sentinelErr == nil && os.SameFile(sentinelBefore, sentinelAfter), sentinelErr, data, readErr)
			}
		})
	}
}

func TestSelfCanonicalRetiredChildMakesRealProgressAtCollisionBoundary(t *testing.T) {
	fixture := newQuarantineCollisionFixture(t, "directory-chain")
	defer func() { _ = fixture.root.Close() }()
	activePath := filepath.Join(fixture.home.Root(), spoolControlDirectoryName)
	if err := os.Mkdir(activePath, 0o700); err != nil {
		t.Fatal(err)
	}
	retiredPath := filepath.Join(fixture.home.Root(), retiredControlDirectoryName)
	if err := os.Mkdir(retiredPath, 0o700); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(retiredPath, "temporary")
	if err := os.Mkdir(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temporary, "payload"), []byte("retired"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Lstat(temporary, &stat); err != nil {
		t.Fatal(err)
	}
	canonical := fmt.Sprintf(".orphan-%x-%x", unixStatDevice(stat), unixStatInode(stat))
	if err := os.Rename(temporary, filepath.Join(retiredPath, canonical)); err != nil {
		t.Fatal(err)
	}
	hooks := fixture.unsupportedExchangeHooks(unix.ENOSYS)
	physicalOpens := 0
	hooks.afterDirectoryOpen = func(string) { physicalOpens++ }
	fixture.installHooks(t, hooks)
	for attempt := 0; attempt < 3; attempt++ {
		before := filesystemStateFingerprint(t, fixture.home.Root())
		physicalOpens = 0
		budget := defaultSpoolWorkBudget()
		budget.maxDirectories = 3
		state := runSpoolSweep(fixture.root, spoolPolicy{}, time.Time{}, budget, true)
		result, sweepErr := state.finish()
		if sweepErr != nil {
			t.Fatalf("self-canonical retired attempt %d: %v", attempt+1, sweepErr)
		}
		after := filesystemStateFingerprint(t, fixture.home.Root())
		deps := defaultTestServiceDependencies(fixture.home, 2)
		deps.newUUID = uuidSequence(t, testEventIDThree)
		deps.now = func() time.Time { return testRecordHour }
		service := mustOpenTestService(t, deps)
		permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
		recordResult := service.RecordOnce(permit, CommandHelp)
		_ = permit.Close()
		if result.complete || !state.mutated || before == after || physicalOpens > int(budget.maxDirectories) || recordResult != RecordDropped {
			t.Fatalf("self-canonical retired attempt %d = complete:%v mutated:%v fingerprintChanged:%v opens:%d record:%v usage:%+v",
				attempt+1, result.complete, state.mutated, before != after, physicalOpens, recordResult, result.usage)
		}
	}
}

func TestReadExhaustionDeletionArmsEvidenceBeforeUnlinkFailure(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	plainRoot := mustOpenMutableRoot(t, home)
	totalBytes := uint64(0)
	for index, eventID := range []string{testEventIDOne, testEventIDTwo, testEventIDThree} {
		data := writeSpoolEventFixture(t, plainRoot, queueDirectoryName, testSpoolGeneration,
			testSpoolEvent(eventID, "1.0.0", testRecordHour, CommandID(index+1)))
		totalBytes += uint64(len(data))
	}
	lowQuota := spoolQuota{Events: 1, Bytes: totalBytes / 3}
	if err := persistSpoolQuota(plainRoot, lowQuota); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	unlinked := false
	failedSync := false
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepUnlink {
			unlinked = true
		}
		if unlinked && !failedSync && step == storageStepDirectorySync {
			failedSync = true
			return errors.New("simulated crash after overflow unlink")
		}
		return nil
	}}
	root, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	budget := defaultSpoolWorkBudget()
	budget.maxReadBytes = spoolFixedReadEnvelope + maximumEventBytes
	state := runSpoolSweep(root, testCurrentSpoolPolicy(), testRecordHour, budget, false)
	remaining, readDirErr := os.ReadDir(filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration))
	quota, present, quotaErr := loadSpoolQuota(root)
	_, activeErr := root.lookupEntry(spoolControlDirectoryName)
	_, retiredErr := root.lookupEntry(retiredControlDirectoryName)
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	strongEvidence := quotaErr == nil && present && quota == markers || activeErr == nil || retiredErr == nil

	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	deps.now = func() time.Time { return testRecordHour }
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	recordResult := service.RecordOnce(permit, CommandHelp)
	_ = permit.Close()
	if !unlinked || !failedSync || state.operation == nil || readDirErr != nil || len(remaining) < 2 || !strongEvidence || recordResult != RecordDropped {
		t.Fatalf("read-exhaustion crash = unlinked:%v failedSync:%v err:%v remaining:%d readDirErr:%v quota:%+v present:%v quotaErr:%v activeErr:%v retiredErr:%v record:%v",
			unlinked, failedSync, state.operation, len(remaining), readDirErr, quota, present, quotaErr, activeErr, retiredErr, recordResult)
	}
}

func TestReopenedCleanupGenerationMustRecoverySyncBeforeExactQuotaCertification(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	plainRoot := mustOpenMutableRoot(t, home)
	expired := testSpoolEvent(testEventIDOne, "1.0.0",
		testRecordHour.Add(-(maximumEventAgeHours+1)*time.Hour), CommandHelp)
	eventBytes := writeSpoolEventFixture(t, plainRoot, queueDirectoryName, testSpoolGeneration, expired)
	initialQuota := spoolQuota{Events: 1, Bytes: uint64(len(eventBytes))}
	if err := persistSpoolQuota(plainRoot, initialQuota); err != nil {
		t.Fatal(err)
	}
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	unlinked := false
	failedParentSync := false
	firstRoot, err := openStorageRootMutableWithHooks(home, storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepUnlink {
			unlinked = true
		}
		if unlinked && !failedParentSync && step == storageStepDirectorySync {
			failedParentSync = true
			return errors.New("simulated crash after applied event unlink")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	firstResult, firstErr := reconcileSpool(firstRoot, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
	if !unlinked || !failedParentSync || firstErr == nil || firstResult.complete {
		t.Fatalf("initial uncertain event unlink = unlinked:%v failedSync:%v complete:%v err:%v",
			unlinked, failedParentSync, firstResult.complete, firstErr)
	}
	if err := firstRoot.Close(); err != nil {
		t.Fatal(err)
	}

	generationPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration)
	keepRecoverySyncFailing := true
	waitingForRecoverySync := false
	recoverySyncAttempts := 0
	hooks := storageTestHooks{
		afterDirectoryOpen: func(path string) {
			if path == generationPath {
				waitingForRecoverySync = true
			}
		},
		beforeStep: func(step storageStep) error {
			if waitingForRecoverySync && step == storageStepDirectorySync {
				waitingForRecoverySync = false
				recoverySyncAttempts++
				if keepRecoverySyncFailing {
					return errors.New("retained generation recovery sync remains unavailable")
				}
			}
			if waitingForRecoverySync && step == storageStepEnumerate {
				// Without a recovery sync, enumeration would trust uncertain
				// contents. Clear the arm so later unrelated syncs cannot make
				// this regression pass accidentally.
				waitingForRecoverySync = false
			}
			return nil
		},
	}
	for attempt := 0; attempt < 3; attempt++ {
		root, openErr := openStorageRootMutableWithHooks(home, hooks)
		if openErr != nil {
			t.Fatal(openErr)
		}
		beforeAttempts := recoverySyncAttempts
		result, reconcileErr := reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
		quota, present, quotaErr := loadSpoolQuota(root)
		strongEvidence := hasStrongFailClosedSpoolEvidence(root)
		closeErr := root.Close()
		if reconcileErr == nil || result.complete || recoverySyncAttempts != beforeAttempts+1 || quotaErr != nil || !present ||
			quota.Events < initialQuota.Events || quota.Bytes < initialQuota.Bytes || !strongEvidence || closeErr != nil {
			t.Fatalf("reopened uncertain generation attempt %d = complete:%v err:%v syncs:%d->%d quota:%+v present:%v quotaErr:%v evidence:%v close:%v",
				attempt+1, result.complete, reconcileErr, beforeAttempts, recoverySyncAttempts, quota, present, quotaErr, strongEvidence, closeErr)
		}
	}

	keepRecoverySyncFailing = false
	result := spoolSweepResult{}
	for attempts := 0; attempts < 32 && !result.complete; attempts++ {
		root, openErr := openStorageRootMutableWithHooks(home, hooks)
		if openErr != nil {
			t.Fatal(openErr)
		}
		result, err = reconcileSpool(root, testCurrentSpoolPolicy(), testRecordHour, defaultSpoolWorkBudget())
		closeErr := root.Close()
		if err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
	}
	if !result.complete || result.quota != (spoolQuota{}) || recoverySyncAttempts < 4 {
		t.Fatalf("recovery-synced generation did not converge: result=%+v syncAttempts=%d", result, recoverySyncAttempts)
	}
}

func TestMetadataAtEINTRRetryStructurallyRechecksDecisionGate(t *testing.T) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "storage_unix.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	metadataLoops := 0
	gatedLoops := 0
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "metadataAt" || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			loop, ok := node.(*ast.ForStmt)
			if !ok {
				return true
			}
			fstatPosition := token.NoPos
			gatePosition := token.NoPos
			ast.Inspect(loop.Body, func(loopNode ast.Node) bool {
				call, ok := loopNode.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if selector.Sel.Name == "Fstatat" {
					fstatPosition = call.Pos()
				}
				if selector.Sel.Name == "canStartStorageWork" {
					gatePosition = call.Pos()
				}
				return true
			})
			if fstatPosition != token.NoPos {
				metadataLoops++
				if gatePosition != token.NoPos && gatePosition < fstatPosition {
					gatedLoops++
				}
			}
			return false
		})
	}
	if metadataLoops != 1 || gatedLoops != metadataLoops {
		t.Fatalf("metadataAt Fstatat retry loops = %d, decision-gated before every attempt = %d", metadataLoops, gatedLoops)
	}

	directory := t.TempDir()
	path := filepath.Join(directory, "event.json")
	if err := os.WriteFile(path, []byte("event"), 0o600); err != nil {
		t.Fatal(err)
	}
	directoryFD, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(directoryFD) }()
	allowed := true
	attempts := 0
	hooks := storageTestHooks{
		decisionGate: func() bool { return allowed },
		beforeMetadataAttempt: func(string) error {
			attempts++
			allowed = false
			return unix.EINTR
		},
	}
	_, err = metadataAt(directoryFD, "event.json", path, hooks)
	if !errors.Is(err, errRecordDecisionWindowExpired) || attempts != 1 {
		t.Fatalf("metadataAt expired retry = attempts:%d err:%v", attempts, err)
	}
}

func TestPostMutationRenameClassificationAndSyncStayClockFree(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	sourcePath := filepath.Join(inspection.Root(), "event.json")
	targetPath := filepath.Join(inspection.Root(), inflightDirectoryName, "event.json")
	allowed := true
	armed := false
	attempts := 0
	syncs := 0
	var injectedErr error
	hooks := storageTestHooks{
		decisionGate: func() bool { return allowed },
		beforeStep: func(step storageStep) error {
			if armed && step == storageStepRename {
				attempts++
				if attempts == 1 {
					injectedErr = os.Rename(sourcePath, targetPath)
					if injectedErr != nil {
						return injectedErr
					}
					allowed = false
					return unix.EINTR
				}
			}
			if armed && step == storageStepDirectorySync {
				syncs++
			}
			return nil
		},
	}
	root, target := openRenameTestDirectories(t, inspection, hooks)
	if err := root.writeFileAtomic("event.json", []byte("source")); err != nil {
		t.Fatal(err)
	}
	armed = true
	result, err := root.renameFile("event.json", target, "event.json")
	data, readErr := os.ReadFile(targetPath)
	if injectedErr != nil || err != nil || result.state != storageRenameAppliedDurable || attempts != 1 || syncs != 2 ||
		readErr != nil || string(data) != "source" {
		t.Fatalf("expired post-mutation classification = result:%v err:%v injected:%v attempts:%d syncs:%d data:%q readErr:%v",
			result.state, err, injectedErr, attempts, syncs, data, readErr)
	}
}

func TestDirectoryOpenExpiryStopsSafeValidationAndRecoverySync(t *testing.T) {
	t.Run("existing product root", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		current := testRecordHour
		service.deps.now = func() time.Time { return current }
		expired := false
		validationAfterExpiry := 0
		recoverySyncsAfterExpiry := 0
		service.deps.storageHooks.afterDirectoryOpen = func(path string) {
			if path == home.Root() && !expired {
				expired = true
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			}
		}
		service.deps.storageHooks.metadata = func(_ string, metadata storageMetadata) storageMetadata {
			if expired {
				validationAfterExpiry++
			}
			return metadata
		}
		service.deps.storageHooks.beforeStep = func(step storageStep) error {
			if expired && step == storageStepDirectorySync {
				recoverySyncsAfterExpiry++
			}
			return nil
		}
		result := service.RecordOnce(permit, CommandHelp)
		if result != RecordDropped || !expired || validationAfterExpiry != 0 || recoverySyncsAfterExpiry != 0 {
			t.Fatalf("root post-open expiry = result:%v expired:%v validations:%d recoverySyncs:%d",
				result, expired, validationAfterExpiry, recoverySyncsAfterExpiry)
		}
	})

	t.Run("existing descendant", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		plainRoot := mustOpenMutableRoot(t, home)
		if err := persistSpoolQuota(plainRoot, spoolQuota{}); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(home.Root(), queueDirectoryName), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := plainRoot.Close(); err != nil {
			t.Fatal(err)
		}
		current := testRecordHour
		service.deps.now = func() time.Time { return current }
		target := filepath.Join(home.Root(), queueDirectoryName)
		expired := false
		validationAfterExpiry := 0
		recoverySyncsAfterExpiry := 0
		service.deps.storageHooks.afterDirectoryOpen = func(path string) {
			if path == target && !expired {
				expired = true
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			}
		}
		service.deps.storageHooks.metadata = func(_ string, metadata storageMetadata) storageMetadata {
			if expired {
				validationAfterExpiry++
			}
			return metadata
		}
		service.deps.storageHooks.beforeStep = func(step storageStep) error {
			if expired && step == storageStepDirectorySync {
				recoverySyncsAfterExpiry++
			}
			return nil
		}
		result := service.RecordOnce(permit, CommandHelp)
		if result != RecordDropped || !expired || validationAfterExpiry != 0 || recoverySyncsAfterExpiry != 0 {
			t.Fatalf("descendant post-open expiry = result:%v expired:%v validations:%d recoverySyncs:%d",
				result, expired, validationAfterExpiry, recoverySyncsAfterExpiry)
		}
	})

	t.Run("created descendant finishes durability", func(t *testing.T) {
		home, service, permit := newRecordServiceFixture(t, testEventIDOne)
		plainRoot := mustOpenMutableRoot(t, home)
		if err := persistSpoolQuota(plainRoot, spoolQuota{}); err != nil {
			t.Fatal(err)
		}
		if err := plainRoot.Close(); err != nil {
			t.Fatal(err)
		}
		current := testRecordHour
		service.deps.now = func() time.Time { return current }
		target := filepath.Join(home.Root(), queueDirectoryName)
		expired := false
		validationAfterMutation := 0
		durabilitySyncs := 0
		service.deps.storageHooks.afterDirectoryOpen = func(path string) {
			if path == target && !expired {
				expired = true
				current = testRecordHour.Add(defaultRecordDecisionBudget + time.Nanosecond)
			}
		}
		service.deps.storageHooks.metadata = func(_ string, metadata storageMetadata) storageMetadata {
			if expired {
				validationAfterMutation++
			}
			return metadata
		}
		service.deps.storageHooks.beforeStep = func(step storageStep) error {
			if expired && step == storageStepDirectorySync {
				durabilitySyncs++
			}
			return nil
		}
		result := service.RecordOnce(permit, CommandHelp)
		queueInfo, statErr := os.Stat(target)
		if result != RecordDropped || !expired || validationAfterMutation == 0 || durabilitySyncs < 2 ||
			statErr != nil || !queueInfo.IsDir() || queueInfo.Mode().Perm() != 0o700 {
			t.Fatalf("created descendant durability = result:%v expired:%v validations:%d syncs:%d info:%v statErr:%v",
				result, expired, validationAfterMutation, durabilitySyncs, queueInfo, statErr)
		}
	})
}

func TestFixedControlReadEnvelopeIncludesLimitProbeAndInventoriesCallsites(t *testing.T) {
	wantEntryEnvelope := uint64(9*maximumStorageTempAttempts + 32*maximumRelocationSlots + 64 + 1 + 2 + 2)
	if spoolFixedEntryEnvelope != wantEntryEnvelope ||
		spoolFixedNameEnvelope != wantEntryEnvelope*maximumStorageNameBytes {
		t.Fatalf("fixed entry/name envelopes = %d/%d, want %d/%d including journal sentinel, spawn throttle, and diagnostic status",
			spoolFixedEntryEnvelope, spoolFixedNameEnvelope,
			wantEntryEnvelope, wantEntryEnvelope*maximumStorageNameBytes)
	}
	wantReadEnvelope := uint64(3*maximumQuotaBytes+4*maximumRelocationBytes+4) +
		3*maximumStorageTempAttempts*rootTempJournalMarkerReadLimit +
		maximumSpawnThrottleBytes + maximumDiagnosticStatusBytes + 2
	if spoolFixedReadEnvelope != wantReadEnvelope {
		t.Fatalf("fixed read envelope = %d, want %d including marker and control limit probes", spoolFixedReadEnvelope, wantReadEnvelope)
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "spool.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	limitReads := map[string]int{}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "chargeFixedEntry", "chargeFixedRead", "chargeFixedDirectory", "chargeFixedTraversalDirectory", "availableFixedSlots", "claimFixedWorkEnvelope":
			calls[selector.Sel.Name]++
		}
		if selector.Sel.Name != "chargeFixedRead" || len(call.Args) != 1 {
			return true
		}
		expression := call.Args[0]
		binary, ok := expression.(*ast.BinaryExpr)
		if !ok || binary.Op != token.ADD {
			return true
		}
		identifier, ok := binary.X.(*ast.Ident)
		literal, literalOK := binary.Y.(*ast.BasicLit)
		if ok && literalOK && literal.Value == "1" &&
			(identifier.Name == "maximumQuotaBytes" || identifier.Name == "maximumRelocationBytes" ||
				identifier.Name == "maximumDiagnosticStatusBytes") {
			limitReads[identifier.Name]++
		}
		return true
	})
	wantCalls := map[string]int{
		"chargeFixedEntry": 28, "chargeFixedRead": 11, "chargeFixedDirectory": 7, "chargeFixedTraversalDirectory": 2,
		"availableFixedSlots": 2, "claimFixedWorkEnvelope": 5,
	}
	if fmt.Sprint(calls) != fmt.Sprint(wantCalls) || limitReads["maximumQuotaBytes"] != 2 ||
		limitReads["maximumRelocationBytes"] != 2 || limitReads["maximumDiagnosticStatusBytes"] != 1 {
		t.Fatalf("fixed work callsite inventory = calls:%v limitReads:%v, want calls:%v quota probes:2 relocation probes:2 status probes:1",
			calls, limitReads, wantCalls)
	}
}

func TestFixedControlReadsCannotExceedPhysicalFiveMiBAllowance(t *testing.T) {
	home, _, _ := newRecordServiceFixture(t, testEventIDThree)
	plainRoot := mustOpenMutableRoot(t, home)
	control, err := plainRoot.openDir([]string{spoolControlDirectoryName}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.Root(), quotaFileName), bytesOfLength(int(maximumQuotaBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.Root(), spoolControlDirectoryName, relocationCursorFileName), bytesOfLength(int(maximumRelocationBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.Root(), fallbackRelocationCursorName), bytesOfLength(int(maximumRelocationBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = control.Close()
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}
	physicalReadBytes := uint64(0)
	growNext := map[string]bool{}
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeRead: func(path string) {
			if !growNext[path] {
				return
			}
			growNext[path] = false
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.Write([]byte("x"))
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("grow fixed control read: write=%v close=%v", writeErr, closeErr)
			}
		},
		afterRead: func(_ string, _, read int, _ error) {
			if read > 0 {
				physicalReadBytes += uint64(read)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	control, err = root.openDir([]string{spoolControlDirectoryName}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Close() }()
	quotaPath := filepath.Join(home.Root(), quotaFileName)
	cursorPath := filepath.Join(home.Root(), spoolControlDirectoryName, relocationCursorFileName)
	fallbackCursorPath := filepath.Join(home.Root(), fallbackRelocationCursorName)
	for _, read := range []struct {
		directory *storageDir
		name      string
		path      string
		maximum   uint64
	}{
		{directory: root.storageDir, name: quotaFileName, path: quotaPath, maximum: maximumQuotaBytes},
		{directory: root.storageDir, name: quotaFileName, path: quotaPath, maximum: maximumQuotaBytes},
		{directory: control, name: relocationCursorFileName, path: cursorPath, maximum: maximumRelocationBytes},
		{directory: root.storageDir, name: fallbackRelocationCursorName, path: fallbackCursorPath, maximum: maximumRelocationBytes},
	} {
		if err := os.WriteFile(read.path, bytesOfLength(int(read.maximum)), 0o600); err != nil {
			t.Fatal(err)
		}
		growNext[read.path] = true
		if _, err := read.directory.readFile(read.name, int64(read.maximum)); err == nil {
			t.Fatalf("growing fixed read %q unexpectedly fit its limit", read.path)
		}
	}
	budget := defaultSpoolWorkBudget()
	meter := newSpoolWorkMeter(budget)
	meter.physicalDirectories = true
	ordinaryAllowance := meter.ordinaryReadLimit()
	meter.usage.readBytes = ordinaryAllowance
	for _, bytes := range []uint64{maximumQuotaBytes + 1, maximumQuotaBytes + 1, maximumRelocationBytes + 1, maximumRelocationBytes + 1} {
		if !meter.chargeFixedRead(bytes) {
			t.Fatalf("fixed read reservation %d rejected: usage=%+v budget=%+v", bytes, meter.usage, budget)
		}
	}
	fixedWriteBytes := uint64(maximumQuotaBytes + 2*maximumRelocationBytes)
	physicalTotal := ordinaryAllowance + physicalReadBytes + fixedWriteBytes
	if physicalReadBytes != 2*(maximumQuotaBytes+1)+2*(maximumRelocationBytes+1) || physicalTotal > budget.maxReadBytes {
		t.Fatalf("fixed physical reads = %d total with ordinary allowance and fixed writes = %d, cap = %d, meter=%+v",
			physicalReadBytes, physicalTotal, budget.maxReadBytes, meter.usage)
	}
}

func bytesOfLength(length int) []byte {
	return []byte(strings.Repeat("x", length))
}

func hasStrongFailClosedSpoolEvidence(root *storageRoot) bool {
	quota, present, quotaErr := loadSpoolQuota(root)
	_, activeErr := root.lookupEntry(spoolControlDirectoryName)
	_, retiredErr := root.lookupEntry(retiredControlDirectoryName)
	_, fallbackCursorErr := root.lookupEntry(fallbackRelocationCursorName)
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	return quotaErr == nil && present && quota == markers || activeErr == nil || retiredErr == nil || fallbackCursorErr == nil
}

func filesystemStateFingerprint(t *testing.T, root string) string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("%s|%v|%d|%d", relative, info.Mode(), info.Size(), info.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(entries, "\n")
}

func filesystemStateFingerprintIfPresent(t *testing.T, root string) string {
	t.Helper()
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return "<missing>"
	} else if err != nil {
		t.Fatal(err)
	}
	return filesystemStateFingerprint(t, root)
}

func fallbackProgressFingerprint(t *testing.T, root string) string {
	t.Helper()
	fingerprint := filesystemStateFingerprint(t, root)
	cursorPath := filepath.Join(root, fallbackRelocationCursorName)
	var stat unix.Stat_t
	if err := unix.Lstat(cursorPath, &stat); errors.Is(err, fs.ErrNotExist) {
		return fingerprint + "\nfallback-cursor|missing"
	} else if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s\nfallback-cursor|%x|%x|%x", fingerprint, unixStatDevice(stat), unixStatInode(stat), data)
}

func FuzzEventFileName(f *testing.F) {
	for _, seed := range []string{eventFileName(testEventIDOne), testEventIDOne, "../event.json", strings.Repeat("x", 129), ".pm-tmp-1-2"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		id, ok := eventIDFromFileName(name)
		if !ok {
			return
		}
		if !validCanonicalUUIDv4(id) || eventFileName(id) != name || len(name) > maximumStorageNameBytes {
			t.Fatalf("accepted non-canonical event name %q -> %q", name, id)
		}
	})
}

func BenchmarkRecordOnceEnqueue(b *testing.B) {
	home := newMetricsBenchmarkHome(b)
	writeBenchmarkState(b, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	b.ReportAllocs()
	b.ResetTimer()
	stored := 0
	for index := 0; index < b.N; index++ {
		b.StopTimer()
		if index > 0 && index%1000 == 0 {
			root := mustOpenBenchmarkRoot(b, home)
			result := spoolSweepResult{}
			var err error
			for attempts := 0; attempts < 8 && !result.complete; attempts++ {
				result, err = purgeSpool(root, defaultSpoolWorkBudget())
				if err != nil {
					break
				}
			}
			closeErr := root.Close()
			if err != nil || closeErr != nil || !result.complete {
				b.Fatalf("benchmark purge: complete=%v err=%v close=%v", result.complete, err, closeErr)
			}
		}
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", index%1000+1, index%1000+1)
		deps := defaultTestServiceDependencies(home, 2)
		deps.newUUID = func() (string, error) { return id, nil }
		deps.now = func() time.Time { return testRecordHour }
		service, err := openWithDependencies(deps)
		if err != nil {
			b.Fatal(err)
		}
		permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
		if !permit.Valid() {
			b.Fatal("benchmark permit is invalid")
		}
		b.StartTimer()
		result := service.RecordOnce(permit, CommandHelp)
		b.StopTimer()
		if err := permit.Close(); err != nil {
			b.Fatal(err)
		}
		if result != RecordStored {
			b.Fatalf("RecordOnce = %v", result)
		}
		stored++
	}
	if b.N > 0 {
		b.ReportMetric(float64(stored)/float64(b.N), "stored/op")
	}
}

func newMetricsBenchmarkHome(b *testing.B) gchome.ProductUsageHome {
	b.Helper()
	trustedTempRoot := "/tmp"
	if runtime.GOOS == "darwin" {
		trustedTempRoot = "/private/tmp"
	}
	b.Setenv("GOTMPDIR", trustedTempRoot)
	b.Setenv("TMPDIR", trustedTempRoot)
	privateAncestor := b.TempDir()
	if err := os.Chmod(privateAncestor, 0o700); err != nil {
		b.Fatal(err)
	}
	homePath := filepath.Join(privateAncestor, ".gc")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		b.Fatal(err)
	}
	b.Setenv("GC_HOME", homePath)
	home, err := gchome.InspectProductUsageHome(gchome.ResolveReadOnly())
	if err != nil {
		b.Fatal(err)
	}
	return home
}

func writeBenchmarkState(b *testing.B, home gchome.ProductUsageHome, state persistedState) {
	b.Helper()
	data, err := encodePersistedState(state)
	if err != nil {
		b.Fatal(err)
	}
	root := mustOpenBenchmarkRoot(b, home)
	writeErr := root.writeFileAtomic(configFileName, data)
	closeErr := root.Close()
	if writeErr != nil || closeErr != nil {
		b.Fatalf("write benchmark state: write=%v close=%v", writeErr, closeErr)
	}
}

func mustOpenBenchmarkRoot(b *testing.B, home gchome.ProductUsageHome) *storageRoot {
	b.Helper()
	root, err := openStorageRootMutable(home)
	if err != nil {
		b.Fatal(err)
	}
	return root
}

func newRecordServiceFixture(t *testing.T, eventID string) (gchome.ProductUsageHome, *Service, RecordingPermit) {
	t.Helper()
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, eventID)
	deps.now = func() time.Time { return testRecordHour }
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	if !permit.Valid() {
		t.Fatal("recording permit is invalid")
	}
	t.Cleanup(func() { _ = permit.Close() })
	return home, service, permit
}

func recordableInvocationAt(hour time.Time) InvocationContext {
	return InvocationContext{Recordable: true, OccurredHourUTC: hour.UTC().Format(time.RFC3339)}
}

func testCurrentSpoolPolicy() spoolPolicy {
	return spoolPolicy{generation: testSpoolGeneration, installationID: testInstallationID}
}

func requireMutationFreeReconcile(t *testing.T, root *storageRoot, policy spoolPolicy, now time.Time) {
	t.Helper()
	result := spoolSweepResult{}
	for attempts := 0; attempts < 8 && !result.complete; attempts++ {
		var err error
		result, err = reconcileSpool(root, policy, now, defaultSpoolWorkBudget())
		if err != nil {
			t.Fatal(err)
		}
	}
	if !result.complete {
		t.Fatal("bounded reconciliation never reached a mutation-free proof pass")
	}
}

func testSpoolEvent(eventID, release string, occurred time.Time, command CommandID) Event {
	os := OSLinux
	if runtime.GOOS == "darwin" {
		os = OSDarwin
	}
	return Event{
		EventID: eventID, InstallationID: testInstallationID, App: AppGasCity,
		ReleaseVersion: release, OS: os, OccurredHourUTC: occurred.UTC().Format(time.RFC3339), CommandID: command,
	}
}

func mustOpenMutableRoot(t *testing.T, home gchome.ProductUsageHome) *storageRoot {
	t.Helper()
	root, err := openStorageRootMutable(home)
	if err != nil {
		t.Fatalf("open mutable root: %v", err)
	}
	return root
}

func writeJournaledRootTempCrashFixture(t *testing.T, root *storageRoot, name string, data []byte, sparseSize int64) {
	t.Helper()
	backend, ok := root.backend.(*unixStorageDirectory)
	if !ok {
		t.Fatal("journaled root-temp fixture requires Unix storage")
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
	rootFD, err := backend.duplicateFD()
	if err != nil {
		_ = marker.close()
		t.Fatal(err)
	}
	tempFD, tempMetadata, err := createPrivateTempFileNamed(rootFD, backend.path, backend.euid, backend.hooks, name)
	if err != nil {
		_ = unix.Close(rootFD)
		_ = marker.close()
		t.Fatal(err)
	}
	if err = syncDirectoryFD(rootFD, backend.hooks); err == nil {
		err = marker.bindTemp(tempMetadata)
	}
	if len(data) != 0 {
		if err == nil {
			err = writeAllFD(tempFD, data, backend.hooks)
		}
	}
	if err == nil && sparseSize > 0 {
		err = unix.Ftruncate(tempFD, sparseSize)
	}
	if err == nil {
		err = syncFileFD(tempFD, backend.hooks)
	}
	closeErr := unix.Close(tempFD)
	syncErr := syncDirectoryFD(rootFD, backend.hooks)
	rootCloseErr := unix.Close(rootFD)
	markerCloseErr := marker.close()
	if err := errors.Join(err, closeErr, syncErr, rootCloseErr, markerCloseErr); err != nil {
		t.Fatal(err)
	}
}

func mustOpenSpoolGeneration(t *testing.T, root *storageRoot, tree, generation string) *storageDir {
	t.Helper()
	directory, err := root.openDir([]string{tree, generation}, true)
	if err != nil {
		t.Fatalf("open spool generation: %v", err)
	}
	return directory
}

func writeSpoolEventFixture(t *testing.T, root *storageRoot, tree, generation string, event Event) []byte {
	t.Helper()
	directory := mustOpenSpoolGeneration(t, root, tree, generation)
	defer func() { _ = directory.Close() }()
	data, err := EncodeEvent(event)
	if err != nil {
		t.Fatalf("encode event fixture: %v", err)
	}
	if err := directory.writeFileAtomicNoReplace(eventFileName(event.EventID), data); err != nil {
		t.Fatalf("write event fixture: %v", err)
	}
	return data
}

func writeNamedSpoolFixture(t *testing.T, directory *storageDir, name string, event Event) []byte {
	t.Helper()
	data, err := EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.writeFileAtomicNoReplace(name, data); err != nil {
		t.Fatal(err)
	}
	return data
}

func readQuotaFixture(t *testing.T, home gchome.ProductUsageHome) spoolQuota {
	t.Helper()
	root, err := openStorageRootReadOnly(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	return readQuotaFromRoot(t, root)
}

func readQuotaFromRoot(t *testing.T, root *storageRoot) spoolQuota {
	t.Helper()
	quota, present, err := loadSpoolQuota(root)
	if err != nil {
		t.Fatalf("load quota: %v", err)
	}
	if !present {
		t.Fatal("load quota: absent")
	}
	return quota
}

func requireIncompletePurgeFailClosedEvidence(t *testing.T, root *storageRoot, prior spoolQuota) {
	t.Helper()
	quota, present, quotaErr := loadSpoolQuota(root)
	_, activeErr := root.lookupEntry(spoolControlDirectoryName)
	_, retiredErr := root.lookupEntry(retiredControlDirectoryName)
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	retainedQuota := quotaErr == nil && present && quota == prior
	overflowMarkers := quotaErr == nil && present && quota == markers
	if retainedQuota || overflowMarkers || activeErr == nil || retiredErr == nil {
		return
	}
	t.Fatalf("incomplete purge lost fail-closed evidence: quota=%+v present=%v quotaErr=%v activeErr=%v retiredErr=%v prior=%+v markers=%+v",
		quota, present, quotaErr, activeErr, retiredErr, prior, markers)
}

func setSpoolMTime(t *testing.T, home gchome.ProductUsageHome, tree, eventID string, value time.Time) {
	t.Helper()
	path := filepath.Join(home.Root(), tree, testSpoolGeneration, eventFileName(eventID))
	if err := os.Chtimes(path, value, value); err != nil {
		t.Fatal(err)
	}
}

func spoolMTime(t *testing.T, home gchome.ProductUsageHome, tree, generation, eventID string) time.Time {
	t.Helper()
	info, err := os.Stat(filepath.Join(home.Root(), tree, generation, eventFileName(eventID)))
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func assertSpoolFileLocation(t *testing.T, home gchome.ProductUsageHome, tree, eventID string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(home.Root(), tree, testSpoolGeneration, eventFileName(eventID))); err != nil {
		t.Fatalf("event %s is not in %s: %v", eventID, tree, err)
	}
}

func assertNoQueuedEvents(t *testing.T, home gchome.ProductUsageHome) {
	t.Helper()
	queue := filepath.Join(home.Root(), queueDirectoryName)
	entries, err := os.ReadDir(queue)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("queue contains entries: %v", entries)
	}
}
