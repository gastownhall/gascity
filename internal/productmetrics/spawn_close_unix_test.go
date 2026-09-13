//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSpawnUploaderDoesNotStartAfterPreflightConfigLeaseCloseFailure(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	deps := spawnTestDependencies(
		home,
		func() time.Time { return testRecordHour },
		func() (string, error) { return testSpawnTokenOne, nil },
	)
	closedConfigLease := false
	var injectionErr error
	deps.storageHooks.afterRead = func(path string, _, read int, readErr error) {
		if closedConfigLease || filepath.Base(path) != configFileName || read != 0 || readErr != nil {
			return
		}
		closedConfigLease = true
		injectionErr = closeOpenFileMatchingPath(path)
	}
	service := mustOpenTestService(t, deps)
	started := false
	err := service.spawnUploader(context.Background(), spawnDependencies{
		executable: func() (string, error) { return "/opt/gascity/bin/gc", nil },
		environ:    func() []string { return []string{"HOME=/home/alice"} },
		start: func(privateUploaderProcessSpec) (func() error, error) {
			started = true
			return func() error { return nil }, nil
		},
	})
	if !closedConfigLease || injectionErr != nil {
		t.Fatalf("close retained preflight config lease: attempted=%v err=%v", closedConfigLease, injectionErr)
	}
	if !errors.Is(err, unix.EBADF) || started {
		t.Fatalf("preflight config lease close failure = err:%v started:%v, want EBADF and no Start", err, started)
	}
}

func TestSpawnUploaderDoesNotStartAfterLockedEligibilityConfigLeaseCloseFailure(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	deps := spawnTestDependencies(
		home,
		func() time.Time { return testRecordHour },
		func() (string, error) { return testSpawnTokenOne, nil },
	)
	configEOFReads := 0
	closedConfigLease := false
	var injectionErr error
	deps.storageHooks.afterRead = func(path string, _, read int, readErr error) {
		if filepath.Base(path) != configFileName || read != 0 || readErr != nil {
			return
		}
		configEOFReads++
		if configEOFReads != 2 {
			return
		}
		closedConfigLease = true
		injectionErr = closeOpenFileMatchingPath(path)
	}
	service := mustOpenTestService(t, deps)
	started := false
	err := service.spawnUploader(context.Background(), spawnDependencies{
		executable: func() (string, error) { return "/opt/gascity/bin/gc", nil },
		environ:    func() []string { return []string{"HOME=/home/alice"} },
		start: func(privateUploaderProcessSpec) (func() error, error) {
			started = true
			return func() error { return nil }, nil
		},
	})
	if configEOFReads != 2 || !closedConfigLease || injectionErr != nil {
		t.Fatalf("close retained locked config lease: EOF reads=%d attempted=%v err=%v", configEOFReads, closedConfigLease, injectionErr)
	}
	if !errors.Is(err, unix.EBADF) || started {
		t.Fatalf("locked config lease close failure = err:%v started:%v, want EBADF and no Start", err, started)
	}
}

func TestRunPrivateUploaderStopsBeforeTransportFactoryAfterPreflightConfigLeaseCloseFailure(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	deps := spawnTestDependencies(
		home,
		func() time.Time { return testRecordHour },
		func() (string, error) { return testSpawnTokenOne, nil },
	)
	deps.getenv = func(name string) string {
		if name == privateUploaderMarkerEnvironment {
			return privateUploaderMarkerValue
		}
		return ""
	}
	closedConfigLease := false
	var injectionErr error
	deps.storageHooks.afterRead = func(path string, _, read int, readErr error) {
		if closedConfigLease || filepath.Base(path) != configFileName || read != 0 || readErr != nil {
			return
		}
		closedConfigLease = true
		injectionErr = closeOpenFileMatchingPath(path)
	}
	factoryCalls := 0
	startCalls := 0
	deps.privateUploaderStart = nil
	deps.privateUploaderStartFactory = func() (uploadStartFunc, error) {
		factoryCalls++
		return func(context.Context, preparedUploadBatch, uint64) (uploadWaitFunc, error) {
			startCalls++
			return func() (uploadResponse, error) { return uploadResponse{}, nil }, nil
		}, nil
	}
	service := mustOpenTestService(t, deps)
	err := service.RunPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne})
	if !closedConfigLease || injectionErr != nil {
		t.Fatalf("close retained private-uploader preflight config lease: attempted=%v err=%v", closedConfigLease, injectionErr)
	}
	if !errors.Is(err, unix.EBADF) || factoryCalls != 0 || startCalls != 0 {
		t.Fatalf("private-uploader preflight close failure = err:%v factory:%d starts:%d, want EBADF/0/0",
			err, factoryCalls, startCalls)
	}
}
