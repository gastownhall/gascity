//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
)

func TestStatusReadsBoundedDiagnosticsWithoutMutatingStorage(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(4, 2, testInstallationID, testSpoolGeneration))
	root := mustOpenMutableRoot(t, home)
	event := fixedEvent()
	event.InstallationID = testInstallationID
	event.ReleaseVersion = "1.0.0"
	event.OccurredHourUTC = "2026-07-12T00:00:00Z"
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	writeDiagnosticStatusToRoot(t, root, diagnosticStatus{
		droppedEvents:            3,
		lastUploadAttemptHourUTC: "2026-07-12T01:00:00Z",
		lastUploadSuccessHourUTC: "2026-07-12T00:00:00Z",
		lastErrorClass:           DiagnosticErrorNetworkTimeout,
	})
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{
		attemptToken: testSpawnTokenOne,
		attemptedAt:  time.Date(2026, time.July, 12, 1, 30, 0, 0, time.UTC),
	})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(event.EventID))
	eventTime := time.Date(2026, time.July, 12, 0, 30, 0, 0, time.UTC)
	if err := os.Chtimes(eventPath, eventTime, eventTime); err != nil {
		t.Fatal(err)
	}
	readRoot, err := openStorageRootReadOnly(home)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _, scanErr := scanOldestQueuedEventReadOnly(readRoot, defaultSpoolWorkBudget())
	if closeErr := readRoot.Close(); scanErr != nil || closeErr != nil {
		t.Fatalf("read-only diagnostic scan = %v, close = %v", scanErr, closeErr)
	}

	deps := defaultTestServiceDependencies(home, 2)
	deps.now = func() time.Time { return time.Date(2026, time.July, 12, 2, 0, 0, 0, time.UTC) }
	status := mustOpenTestService(t, deps).Status(context.Background())
	if !status.QueueDiagnosticsAvailable || status.QueueEvents != 1 || status.QueueBytes != uint64(len(data)) {
		t.Fatalf("queue diagnostics = %#v", status)
	}
	if !status.OldestQueuedEventPresent || status.OldestQueuedEventAge != 90*time.Minute {
		t.Fatalf("oldest event diagnostics = present:%v age:%s", status.OldestQueuedEventPresent, status.OldestQueuedEventAge)
	}
	if !status.StatusDiagnosticsAvailable || status.DroppedEvents != 3 ||
		status.LastUploadAttemptHourUTC != "2026-07-12T01:00:00Z" ||
		status.LastUploadSuccessHourUTC != "2026-07-12T00:00:00Z" ||
		status.LastErrorClass != DiagnosticErrorNetworkTimeout {
		t.Fatalf("bounded status diagnostics = %#v", status)
	}
	if !status.SpawnThrottlePresent || status.SpawnThrottleAge != 30*time.Minute {
		t.Fatalf("spawn diagnostics = present:%v age:%s", status.SpawnThrottlePresent, status.SpawnThrottleAge)
	}
	if _, err := os.Stat(eventPath); err != nil {
		t.Fatalf("Status mutated queued event: %v", err)
	}
}

func TestStatusQueueScanHonorsReadAndDirectoryBudgets(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(4, 2, testInstallationID, testSpoolGeneration))
	root := mustOpenMutableRoot(t, home)
	event := fixedEvent()
	event.InstallationID = testInstallationID
	event.ReleaseVersion = "1.0.0"
	event.OccurredHourUTC = "2026-07-12T00:00:00Z"
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		budget func() spoolWorkBudget
	}{
		{
			name: "read bytes",
			budget: func() spoolWorkBudget {
				budget := defaultSpoolWorkBudget()
				budget.maxReadBytes = maximumEventBytes
				return budget
			},
		},
		{
			name: "physical directories",
			budget: func() spoolWorkBudget {
				budget := defaultSpoolWorkBudget()
				budget.maxDirectories = spoolTraversalDirectoryEnvelope
				return budget
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			readRoot, err := openStorageRootReadOnly(home)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, _, scanErr := scanOldestQueuedEventReadOnly(readRoot, test.budget())
			closeErr := readRoot.Close()
			if scanErr == nil {
				t.Fatal("diagnostic scan exceeded its budget without failing closed")
			}
			if closeErr != nil {
				t.Fatalf("close diagnostic root: %v", closeErr)
			}
		})
	}
}

func TestStatusQueueScanSurfacesTraversalErrorAfterPartialDiagnostics(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(4, 2, testInstallationID, testSpoolGeneration))
	root := mustOpenMutableRoot(t, home)
	event := fixedEvent()
	event.InstallationID = testInstallationID
	event.ReleaseVersion = "1.0.0"
	event.OccurredHourUTC = "2026-07-12T00:00:00Z"
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected diagnostic traversal failure")
	enumerations := 0
	readRoot, err := openStorageRootReadOnlyWithHooks(home, storageTestHooks{
		beforeStep: func(step storageStep) error {
			if step != storageStepEnumerate {
				return nil
			}
			enumerations++
			if enumerations == 3 {
				return injected
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldest, present, events, bytesRead, scanErr := scanOldestQueuedEventReadOnly(readRoot, defaultSpoolWorkBudget())
	if closeErr := readRoot.Close(); closeErr != nil {
		t.Fatalf("close diagnostic root: %v", closeErr)
	}
	if !errors.Is(scanErr, injected) {
		t.Fatalf("diagnostic scan = oldest:%s present:%t events:%d bytes:%d err:%v, want injected traversal error",
			oldest, present, events, bytesRead, scanErr)
	}
}

func TestStatusMissingRootIsKnownEmptyAndDoesNotCreate(t *testing.T) {
	home := inspectStorageTestHome(t, false)
	if _, err := os.Lstat(home.Root()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("metrics root exists before Status: %v", err)
	}
	status := mustOpenTestService(t, defaultTestServiceDependencies(home, 2)).Status(context.Background())
	if !status.QueueDiagnosticsAvailable || !status.StatusDiagnosticsAvailable ||
		status.QueueEvents != 0 || status.QueueBytes != 0 || status.OldestQueuedEventPresent ||
		status.SpawnThrottlePresent {
		t.Fatalf("missing-root diagnostics = %#v", status)
	}
	if _, err := os.Lstat(home.Root()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Status created metrics root: %v", err)
	}
}

func TestStatusFailsEachDiagnosticProjectionClosed(t *testing.T) {
	tests := map[string]struct {
		prepare func(*testing.T, *storageRoot, gchome.ProductUsageHome)
		check   func(Status) bool
	}{
		"corrupt status": {
			prepare: func(t *testing.T, root *storageRoot, _ gchome.ProductUsageHome) {
				if err := root.writeFileAtomic(statusFileName, []byte("last_error_class = \"/secret/path\"\n")); err != nil {
					t.Fatal(err)
				}
			},
			check: func(status Status) bool { return !status.StatusDiagnosticsAvailable && status.LastErrorClass == "" },
		},
		"quota absent with queue": {
			prepare: func(t *testing.T, root *storageRoot, _ gchome.ProductUsageHome) {
				event := fixedEvent()
				event.InstallationID = testInstallationID
				event.ReleaseVersion = "1.0.0"
				writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
			},
			check: func(status Status) bool { return !status.QueueDiagnosticsAvailable && status.QueueEvents == 0 },
		},
		"corrupt throttle": {
			prepare: func(t *testing.T, root *storageRoot, _ gchome.ProductUsageHome) {
				if err := root.writeFileAtomic(spawnThrottleFileName, []byte("attempt_token = \"secret\"\n")); err != nil {
					t.Fatal(err)
				}
			},
			check: func(status Status) bool { return !status.SpawnThrottlePresent && status.SpawnThrottleAge == 0 },
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, enabledState(4, 2, testInstallationID, testSpoolGeneration))
			root := mustOpenMutableRoot(t, home)
			test.prepare(t, root, home)
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			status := mustOpenTestService(t, defaultTestServiceDependencies(home, 2)).Status(context.Background())
			if !test.check(status) {
				t.Fatalf("unexpected status = %#v", status)
			}
		})
	}
}

func writeDiagnosticStatusToRoot(t *testing.T, root *storageRoot, status diagnosticStatus) {
	t.Helper()
	data, err := encodeDiagnosticStatus(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.writeFileAtomic(statusFileName, data); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeAndCleanProofRequireDiagnosticStatusAbsent(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "valid", body: mustEncodeDiagnosticStatus(t, diagnosticStatus{droppedEvents: 2})},
		{name: "corrupt", body: []byte("status_schema = [\n")},
		{name: "oversized", body: bytes.Repeat([]byte("x"), maximumDiagnosticStatusBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, disabledState(7, 2, cleanupDisable))
			root := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home.Root(), statusFileName), test.body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := proveCleanMetricsTree(root, defaultSpoolWorkBudget()); !errors.Is(err, ErrStateChangedConcurrently) {
				t.Fatalf("clean proof with status = %v, want state changed", err)
			}
			result, purgeErr := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
			if purgeErr != nil || !result.complete {
				t.Fatalf("purge status = %+v, %v", result, purgeErr)
			}
			if _, err := os.Lstat(filepath.Join(home.Root(), statusFileName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("purge left status: %v", err)
			}
			if err := proveCleanMetricsTree(root, defaultSpoolWorkBudget()); err != nil {
				t.Fatalf("clean proof after status purge: %v", err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func mustEncodeDiagnosticStatus(t *testing.T, status diagnosticStatus) []byte {
	t.Helper()
	data, err := encodeDiagnosticStatus(status)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPurgeDiagnosticStatusPreservesReplacementAtDeleteBoundary(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, disabledState(7, 2, cleanupDisable))
	plain := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(plain, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	writeDiagnosticStatusToRoot(t, plain, diagnosticStatus{droppedEvents: 1})
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home.Root(), statusFileName)
	replacement := diagnosticStatus{droppedEvents: 2, lastErrorClass: DiagnosticErrorStorageFailure}
	replaced := false
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeMutation: func(step storageStep, observed string) {
			if replaced || step != storageStepDelete || observed != path {
				return
			}
			replaced = true
			if removeErr := os.Remove(path); removeErr != nil {
				t.Fatal(removeErr)
			}
			if writeErr := os.WriteFile(path, mustEncodeDiagnosticStatus(t, replacement), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, purgeErr := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
	if closeErr := root.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !replaced || purgeErr == nil || result.complete {
		t.Fatalf("replacement-boundary purge = %+v err=%v replaced=%v", result, purgeErr, replaced)
	}
	if got := readDiagnosticStatusFixture(t, home); got != replacement {
		t.Fatalf("replacement status = %#v, want %#v", got, replacement)
	}
}

func TestCleanSpoolProofAllowsOnlyValidOwnedStatusForPauseResume(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(7, 2, testInstallationID, "")
	paused.PausedThroughMetricsEpoch = 2
	writeStateFixture(t, home, paused)
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	writeDiagnosticStatusToRoot(t, root, diagnosticStatus{lastErrorClass: DiagnosticErrorServerPaused})
	if err := proveCleanMetricsTree(root, defaultSpoolWorkBudget()); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("ordinary off proof accepted status: %v", err)
	}
	if err := proveCleanMetricsTreeAllowDiagnosticStatus(root, defaultSpoolWorkBudget()); err != nil {
		t.Fatalf("pause-resume proof rejected valid status: %v", err)
	}
	if err := root.writeFileAtomic(statusFileName, []byte("status_schema = [\n")); err != nil {
		t.Fatal(err)
	}
	if err := proveCleanMetricsTreeAllowDiagnosticStatus(root, defaultSpoolWorkBudget()); err == nil {
		t.Fatal("pause-resume proof accepted corrupt status")
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}
