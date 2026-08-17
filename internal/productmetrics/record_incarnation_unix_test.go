//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestConfigRecordLeaseDistinguishesAtomicReplacementAndCloses(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(4, 2, testInstallationID, testSpoolGeneration))
	root, err := openStorageRootMutable(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	oldData, oldLease, err := root.readFileLease(configFileName, maximumConfigBytes)
	if err != nil {
		t.Fatalf("open old record lease: %v", err)
	}
	if !oldLease.Valid() {
		t.Fatal("old record lease is not valid")
	}
	updated := enabledState(5, 2, testInstallationID, "33333333-3333-4333-8333-333333333333")
	updatedData, err := encodePersistedState(updated)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.writeFileAtomic(configFileName, updatedData); err != nil {
		t.Fatalf("replace config: %v", err)
	}
	newData, newLease, err := root.readFileLease(configFileName, maximumConfigBytes)
	if err != nil {
		t.Fatalf("open new record lease: %v", err)
	}
	if oldLease.Matches(newLease) {
		t.Fatalf("atomic replacement reused exact-record incarnation: old=%#v new=%#v", oldLease.incarnation(), newLease.incarnation())
	}
	if string(oldData) == string(newData) {
		t.Fatal("test replacement did not change config bytes")
	}
	if err := oldLease.Close(); err != nil {
		t.Fatalf("close old lease: %v", err)
	}
	if oldLease.Valid() {
		t.Fatal("closed old lease remained valid")
	}
	if err := oldLease.Close(); err != nil {
		t.Fatalf("idempotent old lease close: %v", err)
	}
	if err := newLease.Close(); err != nil {
		t.Fatalf("close new lease: %v", err)
	}
}

func TestReadOnlyStatusAndRejectedPermitCloseConfigRecordLeases(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stable /proc fd count is Linux-specific")
	}
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, disabledState(4, 0, cleanupNone))
	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	runtime.GC()
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("count process descriptors: %v", err)
	}
	for range 200 {
		_ = service.Status(context.Background())
		if permit := service.RecordingPermit(recordableInvocation()); permit.Valid() {
			t.Fatalf("disabled state issued permit: %#v", permit)
		}
	}
	runtime.GC()
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > len(before)+2 {
		t.Fatalf("read-only state projections leaked config leases: before=%d after=%d", len(before), len(after))
	}
}

func TestRecordingPermitCloseInvalidatesAllCopies(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(4, 2, testInstallationID, testSpoolGeneration))
	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	permit := service.RecordingPermit(recordableInvocation())
	copyOfPermit := permit
	if !permit.Valid() || !copyOfPermit.Valid() {
		t.Fatalf("fresh permit copies are not valid: permit=%#v copy=%#v", permit, copyOfPermit)
	}
	if err := permit.Close(); err != nil {
		t.Fatalf("close permit: %v", err)
	}
	if permit.Valid() || copyOfPermit.Valid() {
		t.Fatalf("closed shared permit lease remained valid: permit=%#v copy=%#v", permit, copyOfPermit)
	}
	if err := copyOfPermit.Close(); err != nil {
		t.Fatalf("idempotent copied-permit close: %v", err)
	}
}

func TestTerminalNamespaceOptOutDeletesRetainedIdentity(t *testing.T) {
	home := newMetricsTestHome(t)
	state := enabledState(maximumStateCounter-1, 2, testInstallationID, "")
	state.CounterNamespace = terminalCounterNamespace
	state.AcceptedNoticeVersion = 1
	writeStateFixture(t, home, state)

	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = func() (string, error) {
		t.Fatal("terminal fail-closed state requested activation entropy")
		return "", errors.New("unreachable")
	}
	service := mustOpenTestService(t, deps)
	status := service.Status(context.Background())
	if status.State != StateFailClosed || status.Reason != ReasonCounterNamespaceExhausted {
		t.Fatalf("terminal inactive state was not fail-closed: %#v", status)
	}
	if permit := service.RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatalf("terminal inactive state issued a recording permit: %#v", permit)
	}
	if err := service.Enable(context.Background(), noticeInvocation(), io.Discard); err == nil {
		t.Fatal("terminal inactive state allowed explicit activation")
	}
	token, err := service.beginDisable(context.Background(), stateVersionFrom(state))
	if err != nil {
		t.Fatalf("terminal inactive identity could not be durably opted out: %v", err)
	}
	disabled := readStateFixture(t, home)
	if disabled.Preference != preferenceDisabled || disabled.InstallationID != "" || disabled.SpoolGeneration != "" ||
		disabled.CleanupKind != cleanupDisable || !cleanupTokenMatchesState(token, disabled) {
		t.Fatalf("terminal opt-out barrier = %#v token=%#v", disabled, token)
	}
}

func TestCorruptRollbackCannotReviveStaleCleanupOwner(t *testing.T) {
	home := newMetricsTestHome(t)
	oldBarrier := disabledState(1, 1, cleanupDisable)
	oldBarrier.CounterNamespace = 2
	writeStateFixture(t, home, oldBarrier)
	oldToken := leasedCleanupTokenFixture(t, home)

	rolledBack := enabledState(9, 2, testInstallationID, testSpoolGeneration)
	rolledBack.CounterNamespace = 1
	raw := append(encodeUncheckedState(t, rolledBack), []byte("unknown = true\n")...)
	writeRawConfigFixture(t, home, raw)

	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	newToken, err := service.beginDisable(context.Background(), stateVersion{})
	if err != nil {
		t.Fatalf("recover corrupt state with opt-out barrier: %v", err)
	}
	if newToken.counterNamespace != oldToken.counterNamespace || newToken.stateGeneration != oldToken.stateGeneration ||
		newToken.cleanupEpoch != oldToken.cleanupEpoch || newToken.kind != oldToken.kind {
		t.Fatalf("test did not reproduce cleanup-owner ABA: old=%#v new=%#v", oldToken, newToken)
	}
	if oldToken.recordLease.Matches(newToken.recordLease) {
		t.Fatalf("corrupt recovery retained the stale exact record: old=%#v new=%#v", oldToken, newToken)
	}
	err = service.completeCleanup(context.Background(), oldToken)
	after := readStateFixture(t, home)
	if !errors.Is(err, ErrStateChangedConcurrently) || after.CleanupKind != cleanupDisable {
		t.Fatalf("stale cleanup owner crossed corrupt-state recovery: err=%v state=%#v", err, after)
	}
}

func TestCorruptRecoveryCannotReuseFinalNamespaceCleanupAuthority(t *testing.T) {
	home := newMetricsTestHome(t)
	late := enabledState(maximumStateCounter-1, 2, testInstallationID, testSpoolGeneration)
	late.CounterNamespace = terminalCounterNamespace - 1
	late.CleanupEpoch = maximumStateCounter - 1
	writeStateFixture(t, home, late)

	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	staleToken, err := service.beginDisable(context.Background(), stateVersionFrom(late))
	if err != nil {
		t.Fatalf("install final-namespace cleanup barrier: %v", err)
	}
	if staleToken.counterNamespace != terminalCounterNamespace || staleToken.stateGeneration != 1 ||
		staleToken.cleanupEpoch != 1 || staleToken.kind != cleanupDisable {
		t.Fatalf("unexpected final-namespace token: %#v", staleToken)
	}
	if err := service.completeCleanup(context.Background(), staleToken); err != nil {
		t.Fatalf("complete first final-namespace cleanup: %v", err)
	}

	writeRawConfigFixture(t, home, []byte("state_schema = [\n"))
	freshToken, err := service.beginDisable(context.Background(), stateVersion{})
	if err != nil {
		t.Fatalf("recover corrupt final-namespace state: %v", err)
	}
	if freshToken.counterNamespace != staleToken.counterNamespace || freshToken.stateGeneration != staleToken.stateGeneration ||
		freshToken.cleanupEpoch != staleToken.cleanupEpoch || freshToken.kind != staleToken.kind {
		t.Fatalf("test did not reproduce final-namespace numeric ABA: stale=%#v fresh=%#v", staleToken, freshToken)
	}
	if freshToken.recordLease.Matches(staleToken.recordLease) {
		t.Errorf("corrupt recovery reused final-namespace exact-record authority: stale=%#v fresh=%#v", staleToken, freshToken)
	}
	if err := service.completeCleanup(context.Background(), staleToken); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("stale final-namespace owner error = %v, want ErrStateChangedConcurrently", err)
	}
	if got := readStateFixture(t, home); got.CleanupKind != cleanupDisable {
		t.Fatalf("stale final-namespace owner cleared recovered cleanup: %#v", got)
	}
}

func TestCleanupWinnerCannotLeaveNoticeFloorResumeWindow(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(maximumStateCounter-1, 1, testInstallationID, "")
	paused.CleanupKind = cleanupPause
	paused.CleanupEpoch = maximumStateCounter - 1
	paused.PausedThroughMetricsEpoch = 1
	writeStateFixture(t, home, paused)
	precreateStateLock(t, home)
	oldCleanup := leasedCleanupTokenFixture(t, home)

	reachedLockAttempt := make(chan struct{})
	releaseLockAttempt := make(chan struct{})
	var once sync.Once
	newNoticeDeps := defaultTestServiceDependencies(home, 2)
	newNoticeDeps.notice.version = 2
	newNoticeDeps.storageHooks = storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepLock {
			once.Do(func() {
				close(reachedLockAttempt)
				<-releaseLockAttempt
			})
		}
		return nil
	}}
	newNotice := mustOpenTestService(t, newNoticeDeps)
	done := make(chan RecordingPermit, 1)
	go func() { done <- newNotice.RecordingPermit(recordableInvocation()) }()

	select {
	case <-reachedLockAttempt:
	case <-time.After(10 * time.Second):
		t.Fatal("notice invalidation did not reach its pre-lock barrier")
	}
	cleanupDeps := defaultTestServiceDependencies(home, 2)
	cleanupDeps.notice.version = 2
	if err := mustOpenTestService(t, cleanupDeps).completeCleanup(context.Background(), oldCleanup); err != nil {
		t.Fatalf("cleanup winner: %v", err)
	}
	close(releaseLockAttempt)
	select {
	case permit := <-done:
		if permit.Valid() {
			t.Fatalf("notice transition invocation received a permit: %#v", permit)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("losing notice invalidation did not finish")
	}

	between := readStateFixture(t, home)
	if between.RequiredNoticeVersion < 2 || between.CleanupKind != cleanupNone || between.SpoolGeneration != "" {
		t.Fatalf("cleanup-winner invalidation did not close the resume window under the same lock: %#v", between)
	}

	oldNoticeDeps := defaultTestServiceDependencies(home, 2)
	oldNoticeDeps.notice.version = 1
	oldNoticeDeps.newUUID = uuidSequence(t, "45454545-4545-4545-8545-454545454545")
	oldNotice := mustOpenTestService(t, oldNoticeDeps)
	if permit := oldNotice.RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatalf("resume transition itself received a permit: %#v", permit)
	}
	if permit := oldNotice.RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatalf("old-notice peer resumed and became recordable before the observed newer floor was durable: %#v", permit)
	}
}

func TestExplicitStaleEnableFailurePersistsNoticeFloorFirst(t *testing.T) {
	home := newMetricsTestHome(t)
	prior := enabledState(4, 1, testInstallationID, testSpoolGeneration)
	writeStateFixture(t, home, prior)
	precreateStateLock(t, home)

	deps := defaultTestServiceDependencies(home, 1)
	deps.notice.version = 2
	deps.newUUID = uuidSequence(t, "56565656-5656-4656-8656-565656565656")
	renames := 0
	deps.storageHooks = storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepRename {
			renames++
			if renames == 2 {
				return errors.New("injected activation install failure")
			}
		}
		return nil
	}}
	var output oneWriteBuffer
	if err := mustOpenTestService(t, deps).Enable(context.Background(), noticeInvocation(), &output); err == nil {
		t.Fatal("stale-notice Enable unexpectedly succeeded")
	}
	if output.String() != testNotice {
		t.Fatalf("explicit stale-notice output = %q", output.String())
	}
	if renames != 2 {
		t.Fatalf("stale Enable replacements = %d, want invalidation then activation", renames)
	}

	after := readStateFixture(t, home)
	if after.RequiredNoticeVersion < 2 || after.SpoolGeneration != "" {
		t.Errorf("explicit stale-notice failure left the superseded spool active: %#v", after)
	}
	oldNoticeDeps := defaultTestServiceDependencies(home, 1)
	oldNoticeDeps.notice.version = 1
	if permit := mustOpenTestService(t, oldNoticeDeps).RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatalf("old-notice peer remained recordable after explicit newer-notice observation: %#v", permit)
	}
}

func TestExplicitStaleEnableWriterFailureKeepsNoticeFloorInvalidated(t *testing.T) {
	tests := map[string]io.Writer{
		"short":  shortNoticeWriter{},
		"failed": failingNoticeWriter{},
	}
	for name, writer := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, enabledState(4, 1, testInstallationID, testSpoolGeneration))
			precreateStateLock(t, home)

			deps := defaultTestServiceDependencies(home, 1)
			deps.notice.version = 2
			entropyCalls := 0
			deps.newUUID = func() (string, error) {
				entropyCalls++
				return "", errors.New("unexpected entropy request")
			}
			if err := mustOpenTestService(t, deps).Enable(context.Background(), noticeInvocation(), writer); err == nil {
				t.Fatal("stale-notice Enable unexpectedly succeeded")
			}
			after := readStateFixture(t, home)
			if after.RequiredNoticeVersion != 2 || after.AcceptedNoticeVersion != 1 || after.SpoolGeneration != "" ||
				after.InstallationID != testInstallationID {
				t.Fatalf("writer failure did not retain the invalidation barrier: %#v", after)
			}
			if entropyCalls != 0 {
				t.Fatalf("writer failure requested entropy %d times", entropyCalls)
			}
			oldDeps := defaultTestServiceDependencies(home, 1)
			oldDeps.notice.version = 1
			if permit := mustOpenTestService(t, oldDeps).RecordingPermit(recordableInvocation()); permit.Valid() {
				t.Fatalf("old-notice peer remained recordable after %s writer failure: %#v", name, permit)
			}
		})
	}
}

func TestExplicitStaleEnableEntropyFailureKeepsNoticeFloorInvalidated(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(4, 1, testInstallationID, testSpoolGeneration))
	precreateStateLock(t, home)

	deps := defaultTestServiceDependencies(home, 1)
	deps.notice.version = 2
	deps.newUUID = func() (string, error) { return "", errors.New("injected entropy failure") }
	var output oneWriteBuffer
	if err := mustOpenTestService(t, deps).Enable(context.Background(), noticeInvocation(), &output); err == nil {
		t.Fatal("entropy-failed stale-notice Enable unexpectedly succeeded")
	}
	if output.String() != testNotice {
		t.Fatalf("entropy-failed stale-notice output = %q", output.String())
	}
	after := readStateFixture(t, home)
	if after.RequiredNoticeVersion != 2 || after.AcceptedNoticeVersion != 1 || after.SpoolGeneration != "" ||
		after.InstallationID != testInstallationID {
		t.Fatalf("entropy failure did not retain the invalidation barrier: %#v", after)
	}
}
