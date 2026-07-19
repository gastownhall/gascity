//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCouncilStaleExplicitEnableCannotCrossCompletedDisable(t *testing.T) {
	for iteration := range 8 {
		t.Run(fmt.Sprintf("iteration-%02d", iteration), func(t *testing.T) {
			home := newMetricsTestHome(t)
			state := pendingState(4)
			state.RequiredNoticeVersion = 2
			writeStateFixture(t, home, state)
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
			enableService := mustOpenTestService(t, enableDeps)
			enableResult := make(chan error, 1)
			var output oneWriteBuffer
			go func() {
				enableResult <- enableService.Enable(context.Background(), noticeInvocation(), &output)
			}()

			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			select {
			case <-reachedLockAttempt:
			case <-deadline.C:
				t.Fatal("stale enable did not reach the pre-lock barrier")
			}

			offService := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
			token, err := offService.beginDisable(context.Background(), testStateVersion(4))
			if err != nil {
				t.Fatalf("beginDisable: %v", err)
			}
			if err := offService.completeCleanup(context.Background(), token); err != nil {
				t.Fatalf("completeCleanup: %v", err)
			}
			cleanDisabled := readStateFixture(t, home)
			if cleanDisabled.Preference != preferenceDisabled || cleanDisabled.CleanupKind != cleanupNone ||
				cleanDisabled.InstallationID != "" || cleanDisabled.SpoolGeneration != "" {
				t.Fatalf("off did not reach identity-free clean disabled state: %#v", cleanDisabled)
			}
			cleanDisabledBytes := readConfigFixture(t, home)

			close(releaseLockAttempt)
			select {
			case err := <-enableResult:
				if !errors.Is(err, ErrStateChangedConcurrently) {
					t.Fatalf("stale pre-disable Enable error = %v, want ErrStateChangedConcurrently", err)
				}
			case <-deadline.C:
				t.Fatal("stale enable did not finish")
			}
			if output.String() != "" {
				t.Fatalf("stale enable printed notice after completed off: %q", output.String())
			}
			if entropyCalls.Load() != 0 {
				t.Fatalf("stale enable requested entropy %d times", entropyCalls.Load())
			}
			if after := readConfigFixture(t, home); string(after) != string(cleanDisabledBytes) {
				t.Fatalf("stale enable mutated completed opt-out\nbefore:\n%s\nafter:\n%s", cleanDisabledBytes, after)
			}
			final := readStateFixture(t, home)
			if final.Preference != preferenceDisabled || final.CleanupKind != cleanupNone ||
				final.InstallationID != "" || final.SpoolGeneration != "" {
				t.Fatalf("stale enable crossed completed off barrier: %#v", final)
			}

			laterID := "edededed-eded-4ded-8ded-edededededed"
			laterSpool := "fefefefe-fefe-4efe-8efe-fefefefefefe"
			laterDeps := defaultTestServiceDependencies(home, 2)
			laterDeps.newUUID = uuidSequence(t, laterID, laterSpool)
			var laterOutput oneWriteBuffer
			if err := mustOpenTestService(t, laterDeps).Enable(context.Background(), noticeInvocation(), &laterOutput); err != nil {
				t.Fatalf("Enable beginning from final clean-disabled state: %v", err)
			}
			later := readStateFixture(t, home)
			if later.Preference != preferenceEnabled || later.InstallationID != laterID ||
				later.SpoolGeneration != laterSpool || laterOutput.String() != testNotice {
				t.Fatalf("later explicit enable = (%#v, %q), want fresh identity and spool", later, laterOutput.String())
			}
		})
	}
}

func TestExplicitEnableIsIdempotentWhenPeerWinsAfterObservation(t *testing.T) {
	home := newMetricsTestHome(t)
	state := pendingState(4)
	state.RequiredNoticeVersion = 2
	writeStateFixture(t, home, state)
	precreateStateLock(t, home)

	reachedFirstLockAttempt := make(chan struct{})
	releaseFirstLockAttempt := make(chan struct{})
	var firstBlocked atomic.Bool
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t,
		"acacacac-acac-4cac-8cac-acacacacacac",
		"bdbdbdbd-bdbd-4dbd-8dbd-bdbdbdbdbdbd",
	)
	deps.storageHooks = storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepLock && firstBlocked.CompareAndSwap(false, true) {
			close(reachedFirstLockAttempt)
			<-releaseFirstLockAttempt
		}
		return nil
	}}
	service := mustOpenTestService(t, deps)

	type enableResult struct {
		err  error
		text string
	}
	firstResult := make(chan enableResult, 1)
	go func() {
		var output oneWriteBuffer
		err := service.Enable(context.Background(), noticeInvocation(), &output)
		firstResult <- enableResult{err: err, text: output.String()}
	}()

	select {
	case <-reachedFirstLockAttempt:
	case <-time.After(10 * time.Second):
		t.Fatal("first enable did not reach the pre-lock barrier")
	}
	var peerOutput oneWriteBuffer
	if err := service.Enable(context.Background(), noticeInvocation(), &peerOutput); err != nil {
		t.Fatalf("peer Enable: %v", err)
	}
	close(releaseFirstLockAttempt)
	select {
	case result := <-firstResult:
		if result.err != nil {
			t.Fatalf("reloaded-enabled Enable error = %v", result.err)
		}
		if result.text != "" {
			t.Fatalf("reloaded-enabled Enable printed duplicate notice %q", result.text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first enable did not finish")
	}
	if peerOutput.String() != testNotice {
		t.Fatalf("winning peer notice = %q", peerOutput.String())
	}
	final := readStateFixture(t, home)
	if final.StateGeneration != 5 || final.Preference != preferenceEnabled ||
		final.InstallationID == "" || final.SpoolGeneration == "" {
		t.Fatalf("idempotent peer-winner state = %#v", final)
	}
}
