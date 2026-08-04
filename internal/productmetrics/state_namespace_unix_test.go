//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStaleEnableCannotRegainAuthorityAcrossFreshCounterNamespace(t *testing.T) {
	home := newMetricsTestHome(t)
	initial := disabledState(2, 1, cleanupNone)
	initial.RequiredNoticeVersion = 2
	initial.AcceptedNoticeVersion = 2
	writeStateFixture(t, home, initial)
	precreateStateLock(t, home)

	reachedLockAttempt := make(chan struct{})
	releaseLockAttempt := make(chan struct{})
	var once sync.Once
	var entropyCalls atomic.Int64
	enableDeps := defaultTestServiceDependencies(home, 2)
	enableDeps.newUUID = func() (string, error) {
		entropyCalls.Add(1)
		return "abababab-abab-4bab-8bab-abababababab", nil
	}
	enableDeps.storageHooks = storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepLock {
			once.Do(func() {
				close(reachedLockAttempt)
				<-releaseLockAttempt
			})
		}
		return nil
	}}
	staleEnable := mustOpenTestService(t, enableDeps)
	result := make(chan error, 1)
	go func() {
		result <- staleEnable.Enable(context.Background(), noticeInvocation(), io.Discard)
	}()
	select {
	case <-reachedLockAttempt:
	case <-time.After(10 * time.Second):
		t.Fatal("stale enable did not reach its pre-lock barrier")
	}

	late := enabledState(maximumStateCounter-1, 2, testInstallationID, testSpoolGeneration)
	late.CleanupEpoch = maximumStateCounter - 1
	writeStateFixture(t, home, late)
	off := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	token, err := off.beginDisable(context.Background(), stateVersionFrom(late))
	if err != nil {
		t.Fatalf("terminal-adjacent beginDisable: %v", err)
	}
	if err := off.completeCleanup(context.Background(), token); err != nil {
		t.Fatalf("terminal-adjacent completeCleanup: %v", err)
	}
	finalDisabled := readStateFixture(t, home)
	if finalDisabled.CounterNamespace == initial.CounterNamespace {
		t.Fatalf("counter recovery reused namespace %d", finalDisabled.CounterNamespace)
	}
	withoutNamespace := finalDisabled
	withoutNamespace.CounterNamespace = initial.CounterNamespace
	if withoutNamespace != initial {
		t.Fatalf("test did not reproduce the numeric/state ABA across namespaces:\ninitial=%#v\nfinal=%#v", initial, finalDisabled)
	}

	close(releaseLockAttempt)
	select {
	case err := <-result:
		if !errors.Is(err, ErrStateChangedConcurrently) {
			t.Fatalf("stale pre-disable Enable error = %v, want ErrStateChangedConcurrently", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stale enable did not finish")
	}
	if entropyCalls.Load() != 0 {
		t.Fatalf("stale enable regained authority and requested entropy %d times", entropyCalls.Load())
	}
	if got := readStateFixture(t, home); got != finalDisabled {
		t.Fatalf("stale enable crossed completed opt-out after counter reset:\nwant=%#v\ngot=%#v", finalDisabled, got)
	}
}

func TestStaleCleanupTokenCannotRegainAuthorityAcrossFreshCounterNamespace(t *testing.T) {
	home := newMetricsTestHome(t)
	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	oldToken, err := service.beginDisable(context.Background(), stateVersion{})
	if err != nil {
		t.Fatalf("create old disable barrier: %v", err)
	}
	if err := service.completeCleanup(context.Background(), oldToken); err != nil {
		t.Fatalf("complete old disable barrier: %v", err)
	}
	if err := service.Enable(context.Background(), noticeInvocation(), io.Discard); err != nil {
		t.Fatalf("enable after old cleanup: %v", err)
	}

	late := enabledState(maximumStateCounter-1, 2, testInstallationID, testSpoolGeneration)
	late.CleanupEpoch = maximumStateCounter - 1
	writeStateFixture(t, home, late)
	newToken, err := service.beginDisable(context.Background(), stateVersionFrom(late))
	if err != nil {
		t.Fatalf("create fresh-namespace barrier: %v", err)
	}
	if newToken.counterNamespace == oldToken.counterNamespace {
		t.Fatalf("counter recovery reused cleanup-token namespace: old=%#v new=%#v", oldToken, newToken)
	}
	if newToken.stateGeneration != oldToken.stateGeneration || newToken.cleanupEpoch != oldToken.cleanupEpoch || newToken.kind != oldToken.kind {
		t.Fatalf("test did not reproduce the numeric cleanup-token ABA: old=%#v new=%#v", oldToken, newToken)
	}

	if err := service.completeCleanup(context.Background(), oldToken); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("stale old-namespace cleanup token error = %v, want ErrStateChangedConcurrently", err)
	}
	state := readStateFixture(t, home)
	if state.CleanupKind != cleanupDisable || state.CounterNamespace != newToken.counterNamespace {
		t.Fatalf("stale old-namespace token cleared the new cleanup barrier: %#v", state)
	}
}

func TestTerminalAdjacentNoticeBumpPersistsFloorAgainstOlderBinary(t *testing.T) {
	home := newMetricsTestHome(t)
	terminalAdjacent := enabledState(maximumStateCounter-1, 1, testInstallationID, testSpoolGeneration)
	writeStateFixture(t, home, terminalAdjacent)

	newRelease := defaultTestServiceDependencies(home, 1)
	newRelease.notice.version = 2
	if permit := mustOpenTestService(t, newRelease).RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatalf("new-notice invocation received permit: %#v", permit)
	}
	after := readStateFixture(t, home)
	if after.CounterNamespace <= terminalAdjacent.CounterNamespace || after.StateGeneration != 1 ||
		after.RequiredNoticeVersion < 2 || after.SpoolGeneration != "" || after.InstallationID != terminalAdjacent.InstallationID {
		t.Errorf("notice floor was not durably invalidated in a fresh inactive namespace: %#v", after)
	}

	oldRelease := defaultTestServiceDependencies(home, 1)
	oldRelease.notice.version = 1
	if permit := mustOpenTestService(t, oldRelease).RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatalf("older-notice binary remained recordable after the newer binary observed the stale notice: %#v", permit)
	}
}

func TestTerminalAdjacentNoticeBumpReissuesPauseCleanupOwnershipInFreshNamespace(t *testing.T) {
	for _, order := range []string{"notice-first", "cleanup-first"} {
		t.Run(order, func(t *testing.T) {
			home := newMetricsTestHome(t)
			paused := enabledState(maximumStateCounter-1, 1, testInstallationID, "")
			paused.CleanupKind = cleanupPause
			paused.CleanupEpoch = maximumStateCounter - 1
			paused.PausedThroughMetricsEpoch = 1
			writeStateFixture(t, home, paused)
			oldToken := leasedCleanupTokenFixture(t, home)

			newRelease := defaultTestServiceDependencies(home, 1)
			newRelease.notice.version = 2
			service := mustOpenTestService(t, newRelease)
			if order == "cleanup-first" {
				if err := service.completeCleanup(context.Background(), oldToken); err != nil {
					t.Fatalf("complete old cleanup before notice invalidation: %v", err)
				}
			}
			if permit := service.RecordingPermit(recordableInvocation()); permit.Valid() {
				t.Fatalf("new-notice invocation received permit: %#v", permit)
			}

			after := readStateFixture(t, home)
			if after.CounterNamespace <= paused.CounterNamespace || after.RequiredNoticeVersion != 2 ||
				after.SpoolGeneration != "" || after.InstallationID != paused.InstallationID {
				t.Fatalf("notice invalidation did not install the fresh inactive namespace: %#v", after)
			}
			if err := service.completeCleanup(context.Background(), oldToken); !errors.Is(err, ErrStateChangedConcurrently) {
				t.Fatalf("old pause cleanup token error = %v, want ErrStateChangedConcurrently", err)
			}
			if order == "notice-first" {
				if after.CleanupKind != cleanupPause {
					t.Fatalf("notice invalidation lost pause cleanup ownership: %#v", after)
				}
				freshToken := leasedCleanupTokenFixture(t, home)
				if err := service.completeCleanup(context.Background(), freshToken); err != nil {
					t.Fatalf("fresh-namespace pause cleanup was stranded: %v", err)
				}
			}

			oldRelease := defaultTestServiceDependencies(home, 1)
			oldRelease.notice.version = 1
			if permit := mustOpenTestService(t, oldRelease).RecordingPermit(recordableInvocation()); permit.Valid() {
				t.Fatalf("older-notice peer received permit after %s transition: %#v", order, permit)
			}
		})
	}
}

func TestTerminalCounterNamespaceIsDurableNonRecordableFallback(t *testing.T) {
	home := newMetricsTestHome(t)
	state := enabledState(maximumStateCounter-1, 2, testInstallationID, testSpoolGeneration)
	state.CounterNamespace = terminalCounterNamespace - 1
	state.CleanupEpoch = maximumStateCounter - 1
	writeStateFixture(t, home, state)
	deps := defaultTestServiceDependencies(home, 2)
	var entropyCalls atomic.Int64
	deps.newUUID = func() (string, error) {
		entropyCalls.Add(1)
		return "abababab-abab-4bab-8bab-abababababab", nil
	}
	service := mustOpenTestService(t, deps)

	token, err := service.beginDisable(context.Background(), stateVersionFrom(state))
	if err != nil {
		t.Fatalf("install terminal fallback disable: %v", err)
	}
	barrier := readStateFixture(t, home)
	if barrier.CounterNamespace != terminalCounterNamespace || barrier.Preference != preferenceDisabled ||
		barrier.CleanupKind != cleanupDisable || barrier.InstallationID != "" || barrier.SpoolGeneration != "" {
		t.Fatalf("terminal fallback barrier = %#v", barrier)
	}
	if err := service.completeCleanup(context.Background(), token); err != nil {
		t.Fatalf("complete terminal fallback cleanup: %v", err)
	}
	clean := readStateFixture(t, home)
	if clean.CounterNamespace != terminalCounterNamespace || clean.Preference != preferenceDisabled || clean.CleanupKind != cleanupNone {
		t.Fatalf("clean terminal fallback = %#v", clean)
	}
	if permit := service.RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatalf("terminal fallback issued permit: %#v", permit)
	}
	var output oneWriteBuffer
	if err := service.Enable(context.Background(), noticeInvocation(), &output); err == nil {
		t.Fatal("terminal fallback allowed explicit enable")
	}
	if output.String() != "" || entropyCalls.Load() != 0 {
		t.Fatalf("terminal fallback enable disclosed notice or requested entropy: output=%q entropy=%d", output.String(), entropyCalls.Load())
	}

	exhausted := clean
	exhausted.StateGeneration = maximumStateCounter - 1
	exhausted.CleanupKind = cleanupDisable
	exhausted.CleanupEpoch = 1
	writeStateFixture(t, home, exhausted)
	before := readConfigFixture(t, home)
	exhaustedToken := leasedCleanupTokenFixture(t, home)
	if err := service.completeCleanup(context.Background(), exhaustedToken); err != nil {
		t.Fatalf("terminal namespace could not complete exhausted cleanup by exact replacement: %v", err)
	}
	after := readStateFixture(t, home)
	if after.CounterNamespace != terminalCounterNamespace || after.Preference != preferenceDisabled ||
		after.CleanupKind != cleanupNone || after.StateGeneration != 1 || after.CleanupEpoch != 1 {
		t.Fatalf("terminal exact-replacement completion = %#v\nbefore:\n%s", after, before)
	}
}

func TestNamespaceParticipatesInPermitAndMutationCAS(t *testing.T) {
	home := newMetricsTestHome(t)
	oldState := enabledState(7, 2, testInstallationID, testSpoolGeneration)
	writeStateFixture(t, home, oldState)
	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	permit := service.RecordingPermit(recordableInvocation())
	if !permit.Valid() {
		t.Fatal("initial state did not issue permit")
	}

	newState := oldState
	newState.CounterNamespace++
	writeStateFixture(t, home, newState)
	if stateMatchesPermit(newState, permit) {
		t.Fatalf("old-namespace permit matched new-namespace state: permit=%#v state=%#v", permit, newState)
	}
	if _, err := service.applyPause(context.Background(), permit, 2); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("old-namespace pause permit error = %v, want ErrStateChangedConcurrently", err)
	}
	if _, err := service.beginDisable(context.Background(), stateVersionFrom(oldState)); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("old-namespace mutation CAS error = %v, want ErrStateChangedConcurrently", err)
	}
	if got := readStateFixture(t, home); got != newState {
		t.Fatalf("stale permit/CAS mutated new namespace:\nwant=%#v\ngot=%#v", newState, got)
	}
}

func TestTerminalNoticeInvalidationAndPauseCleanupBothWinnerOrders(t *testing.T) {
	for _, winner := range []string{"notice", "cleanup"} {
		t.Run(winner+"-wins", func(t *testing.T) {
			home := newMetricsTestHome(t)
			paused := enabledState(maximumStateCounter-1, 1, testInstallationID, "")
			paused.CleanupKind = cleanupPause
			paused.CleanupEpoch = maximumStateCounter - 1
			paused.PausedThroughMetricsEpoch = 1
			writeStateFixture(t, home, paused)
			precreateStateLock(t, home)
			oldToken := leasedCleanupTokenFixture(t, home)

			blocked := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			blockingHooks := storageTestHooks{beforeStep: func(step storageStep) error {
				if step == storageStepLock {
					once.Do(func() {
						close(blocked)
						<-release
					})
				}
				return nil
			}}

			newNoticeDeps := defaultTestServiceDependencies(home, 1)
			newNoticeDeps.notice.version = 2
			cleanupDeps := defaultTestServiceDependencies(home, 1)
			cleanupDeps.notice.version = 2
			if winner == "notice" {
				cleanupDeps.storageHooks = blockingHooks
			} else {
				newNoticeDeps.storageHooks = blockingHooks
			}
			newNotice := mustOpenTestService(t, newNoticeDeps)
			cleanup := mustOpenTestService(t, cleanupDeps)

			loserResult := make(chan error, 1)
			if winner == "notice" {
				go func() { loserResult <- cleanup.completeCleanup(context.Background(), oldToken) }()
			} else {
				go func() {
					permit := newNotice.RecordingPermit(recordableInvocation())
					if permit.Valid() {
						loserResult <- errors.New("notice-invalidating invocation received a permit")
						return
					}
					loserResult <- nil
				}()
			}
			select {
			case <-blocked:
			case <-time.After(10 * time.Second):
				t.Fatal("losing transition did not reach pre-lock barrier")
			}

			if winner == "notice" {
				if permit := newNotice.RecordingPermit(recordableInvocation()); permit.Valid() {
					t.Fatalf("notice winner received permit: %#v", permit)
				}
			} else if err := cleanup.completeCleanup(context.Background(), oldToken); err != nil {
				t.Fatalf("cleanup winner: %v", err)
			}
			close(release)
			select {
			case err := <-loserResult:
				if winner == "notice" && !errors.Is(err, ErrStateChangedConcurrently) {
					t.Fatalf("losing cleanup error = %v, want ErrStateChangedConcurrently", err)
				}
				if winner == "cleanup" && err != nil {
					t.Fatalf("losing invalidation invocation: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("losing transition did not finish")
			}

			// When cleanup won, the first invalidation lost its CAS internally.
			// A fresh observation must durably install the notice floor.
			if permit := newNotice.RecordingPermit(recordableInvocation()); permit.Valid() {
				t.Fatalf("fresh notice invalidation received permit: %#v", permit)
			}
			final := readStateFixture(t, home)
			if final.CounterNamespace <= paused.CounterNamespace || final.RequiredNoticeVersion != 2 || final.SpoolGeneration != "" {
				t.Fatalf("%s winner final state = %#v", winner, final)
			}
			oldNoticeDeps := defaultTestServiceDependencies(home, 1)
			oldNoticeDeps.notice.version = 1
			if permit := mustOpenTestService(t, oldNoticeDeps).RecordingPermit(recordableInvocation()); permit.Valid() {
				t.Fatalf("old-notice peer received permit after %s winner: %#v", winner, permit)
			}
		})
	}
}
