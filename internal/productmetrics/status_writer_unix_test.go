//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
)

func TestRecordOnceAuthorizedDropUpdatesBoundedDiagnostics(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	defer func() { _ = permit.Close() }()
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{Events: maximumSpoolEvents, Bytes: 1}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	if result := service.RecordOnce(permit, CommandHelp); result != RecordDropped {
		t.Fatalf("RecordOnce() = %v, want dropped", result)
	}
	status := readDiagnosticStatusFixture(t, home)
	if status.droppedEvents != 1 || status.lastErrorClass != DiagnosticErrorDiskFull {
		t.Fatalf("drop diagnostics = %#v", status)
	}
}

func TestRecordOnceFailClosedQuotaAndControlEvidenceSuppressDiagnostics(t *testing.T) {
	tests := []struct {
		name        string
		quota       spoolQuota
		residueName string
		residueDir  bool
	}{
		{
			name: "conservative event marker",
			quota: spoolQuota{
				Events: maximumQuotaEventMarker,
			},
		},
		{
			name: "conservative byte marker",
			quota: spoolQuota{
				Bytes: maximumQuotaByteMarker,
			},
		},
		{name: "active control", residueName: spoolControlDirectoryName, residueDir: true},
		{name: "retired control", residueName: retiredControlDirectoryName, residueDir: true},
		{name: "fallback cursor", residueName: fallbackRelocationCursorName},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, service, permit := newRecordServiceFixture(t, testEventIDThree)
			defer func() { _ = permit.Close() }()
			root := mustOpenMutableRoot(t, home)
			if err := persistSpoolQuota(root, test.quota); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			if test.residueName != "" {
				path := filepath.Join(home.Root(), test.residueName)
				var err error
				if test.residueDir {
					err = os.Mkdir(path, 0o700)
				} else {
					err = os.WriteFile(path, []byte("fail-closed residue"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}

			if result := service.RecordOnce(permit, CommandHelp); result != RecordDropped {
				t.Fatalf("RecordOnce() = %v, want dropped", result)
			}
			assertNoDiagnosticStatusFixture(t, home)
		})
	}
}

func TestRecordOnceQuotaPersistenceUncertaintySuppressesDiagnostics(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	defer func() { _ = permit.Close() }()
	installingQuota := false
	quotaRenameApplied := false
	syncFailureInjected := false
	service.deps.beforeRecordOperation = func(operation recordOperation) {
		if operation == recordOperationQuotaInstall {
			installingQuota = true
		}
	}
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if installingQuota && step == storageStepRename {
			quotaRenameApplied = true
		}
		if quotaRenameApplied && !syncFailureInjected && step == storageStepDirectorySync {
			syncFailureInjected = true
			return errors.New("injected quota parent-sync failure")
		}
		return nil
	}

	if result := service.RecordOnce(permit, CommandHelp); result != RecordDropped {
		t.Fatalf("RecordOnce() = %v, want dropped", result)
	}
	if !syncFailureInjected {
		t.Fatal("RecordOnce did not reach the applied quota parent-sync uncertainty")
	}
	quota := readQuotaFixture(t, home)
	if quota.Events != 1 || quota.Bytes == 0 {
		t.Fatalf("visible sync-pending quota = %+v, want one conservative reservation", quota)
	}
	if info, err := os.Stat(filepath.Join(home.Root(), spoolControlDirectoryName)); err != nil || !info.IsDir() {
		t.Fatalf("sync-pending quota did not retain active control: info=%v err=%v", info, err)
	}
	assertNoQueuedEvents(t, home)
	assertNoDiagnosticStatusFixture(t, home)
}

func TestRecordOncePostReservationQueueFailurePersistsDiagnostics(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	defer func() { _ = permit.Close() }()
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	queuePath := filepath.Join(home.Root(), queueDirectoryName)
	if err := os.WriteFile(queuePath, []byte("unsafe queue shape"), 0o600); err != nil {
		t.Fatal(err)
	}

	if result := service.RecordOnce(permit, CommandHelp); result != RecordDropped {
		t.Fatalf("RecordOnce() = %v, want dropped", result)
	}
	quota := readQuotaFixture(t, home)
	if quota.Events != 1 || quota.Bytes == 0 {
		t.Fatalf("post-reservation quota = %+v, want one conservative reservation", quota)
	}
	status := readDiagnosticStatusFixture(t, home)
	if status.droppedEvents != 1 || status.lastErrorClass != DiagnosticErrorStorageFailure {
		t.Fatalf("post-reservation failure diagnostics = %#v", status)
	}
	if data, err := os.ReadFile(queuePath); err != nil || string(data) != "unsafe queue shape" {
		t.Fatalf("unsafe queue shape changed: data=%q err=%v", data, err)
	}
}

func TestRecordOnceIneligibleDropDoesNotCreateDiagnostics(t *testing.T) {
	home := newMetricsTestHome(t)
	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	if result := service.RecordOnce(RecordingPermit{}, CommandHelp); result != RecordDropped {
		t.Fatalf("RecordOnce() = %v, want dropped", result)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), statusFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ineligible drop created status.toml: %v", err)
	}
}

func TestUploaderPersistsAttemptAndClosedSettlementDiagnostics(t *testing.T) {
	tests := map[string]struct {
		response    uploadResponse
		waitErr     error
		wantClass   DiagnosticErrorClass
		wantSuccess bool
	}{
		"accepted": {
			response:    uploadResponse{kind: uploadResponseAccepted, statusCode: 200},
			wantSuccess: true,
		},
		"duplicate": {
			response:    uploadResponse{kind: uploadResponseDuplicate, statusCode: 409},
			wantSuccess: true,
		},
		"server failure": {
			response:  uploadResponse{kind: uploadResponseRetry, statusCode: 503},
			wantClass: DiagnosticErrorServer5xx,
		},
		"network timeout": {
			response:  uploadResponse{kind: uploadResponseRetry, diagnosticError: DiagnosticErrorNetworkTimeout},
			waitErr:   context.DeadlineExceeded,
			wantClass: DiagnosticErrorNetworkTimeout,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home, service, permit := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
			if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}

			result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
				now: func() time.Time { return testRecordHour },
				start: func(context.Context, preparedUploadBatch, uint64) (uploadWaitFunc, error) {
					return func() (uploadResponse, error) { return test.response, test.waitErr }, nil
				},
			})
			if test.waitErr == nil && err != nil {
				t.Fatalf("uploadOneBatch: %v", err)
			}
			if test.waitErr != nil && !errors.Is(err, test.waitErr) {
				t.Fatalf("uploadOneBatch error = %v, want %v", err, test.waitErr)
			}
			if test.wantSuccess && result.outcome != uploadRunDeleted {
				t.Fatalf("successful upload result = %+v", result)
			}
			if !test.wantSuccess && result.outcome != uploadRunRestored {
				t.Fatalf("failed upload result = %+v", result)
			}
			status := readDiagnosticStatusFixture(t, home)
			if status.lastUploadAttemptHourUTC != testRecordHour.Format("2006-01-02T15:00:00Z") {
				t.Fatalf("attempt hour = %q", status.lastUploadAttemptHourUTC)
			}
			if test.wantSuccess {
				if status.lastUploadSuccessHourUTC != status.lastUploadAttemptHourUTC || status.lastErrorClass != "" {
					t.Fatalf("success diagnostics = %#v", status)
				}
			} else if status.lastUploadSuccessHourUTC != "" || status.lastErrorClass != test.wantClass {
				t.Fatalf("failure diagnostics = %#v", status)
			}
		})
	}
}

func readDiagnosticStatusFixture(t *testing.T, home gchome.ProductUsageHome) diagnosticStatus {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home.Root(), statusFileName))
	if err != nil {
		t.Fatal(err)
	}
	status, err := decodeDiagnosticStatus(data)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func assertNoDiagnosticStatusFixture(t *testing.T, home gchome.ProductUsageHome) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(home.Root(), statusFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unexpected status.toml: %v", err)
	}
}
