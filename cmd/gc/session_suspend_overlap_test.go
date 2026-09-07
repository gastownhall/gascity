package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Suspend can complete while a controller tick still holds its pre-suspend
// snapshot. That tick must not turn an intentional stop into continuity reset.
func TestSuspendPreservesConversationAgainstInflightReconcile(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := sessionpkg.NewManagerWithOptions(store, sp)
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{
		Template: "helper", Command: "codex", Provider: "codex", WorkDir: t.TempDir(),
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	clk := &clock.Fake{Time: now}
	if err := store.SetMetadataBatch(info.ID, map[string]string{
		"last_woke_at": now.Add(-99 * time.Second).Format(time.RFC3339),
		"session_key":  "retained-conversation", "started_config_hash": "configured-launch",
		"continuation_reset_pending": "", "wake_attempts": "0", "churn_count": "0",
	}); err != nil {
		t.Fatal(err)
	}
	// The reconciler lists metadata before the API's deliberate stop.
	stale, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatal(err)
	}
	stopped, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.SessionKey != "retained-conversation" || stopped.SleepReason != "user-hold" || stopped.LastWokeAt != "" {
		t.Fatal("Suspend itself did not preserve conversation and intentional-stop markers")
	}
	// Provider observation happens after Stop, while the tick still holds stale.
	alive := sp.IsRunning(info.SessionName)
	if alive {
		t.Fatal("runtime is still running after Suspend")
	}
	front := sessionFrontDoor(store)
	churned := false
	_, err = withCurrentSessionExit(stale, alive, front, func() error {
		patch, err := healStateWithRollbackInfo(stale, alive, true, front, clk, 0, true)
		if err != nil {
			return err
		}
		stale = stale.ApplyPatch(patch)
		_, churned = checkChurn(stale, nil, alive, newDrainTracker(), front, clk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	actual, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.SessionKey != "retained-conversation" || actual.StartedConfigHash != "configured-launch" || actual.ContinuationResetPending == "true" {
		t.Errorf("in-flight reconcile discarded suspended conversation: key=%q hash=%q reset=%q", actual.SessionKey, actual.StartedConfigHash, actual.ContinuationResetPending)
	}
	if churned || actual.ChurnCount != "0" {
		t.Errorf("in-flight reconcile counted intentional stop as churn: churned=%v count=%q", churned, actual.ChurnCount)
	}
}

func exitGuardFixture(t *testing.T, age time.Duration) (*beads.MemStore, *sessionpkg.Store, sessionpkg.Info, *clock.Fake) {
	t.Helper()
	now := time.Now().UTC()
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession}, Metadata: map[string]string{
		"state": "active", "session_name": "exit-guard", "provider": "codex",
		"session_key": "retained-conversation", "started_config_hash": "configured-launch",
		"last_woke_at": now.Add(-age).Format(time.RFC3339), "churn_count": "0", "wake_attempts": "0",
	}})
	if err != nil {
		t.Fatal(err)
	}
	front := sessionFrontDoor(store)
	info, err := front.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return store, front, info, &clock.Fake{Time: now}
}

func TestCurrentSessionExitPreservesUnexpectedDeathAccounting(t *testing.T) {
	for _, age := range []time.Duration{10 * time.Second, 99 * time.Second} {
		t.Run(age.String(), func(t *testing.T) {
			_, front, info, clk := exitGuardFixture(t, age)
			// The tick may have already applied its own metadata patch. This folded
			// snapshot must still compare equal to authoritative persisted state.
			var err error
			info, err = front.ApplyPatchInfo(info, sessionpkg.MetadataPatch{"detached_at": "2026-09-07T03:00:00Z"})
			if err != nil {
				t.Fatal(err)
			}
			crashed, churned := false, false
			applied, err := withCurrentSessionExit(info, false, front, func() error {
				patch, err := healStateWithRollbackInfo(info, false, true, front, clk, 0, true)
				if err != nil {
					return err
				}
				info = info.ApplyPatch(patch)
				info, crashed = checkStability(info, nil, false, newDrainTracker(), front, clk, nil)
				if !crashed {
					info, churned = checkChurn(info, nil, false, newDrainTracker(), front, clk)
				}
				return nil
			})
			if err != nil || !applied {
				t.Fatalf("fresh folded snapshot deferred: applied=%v err=%v", applied, err)
			}
			got, err := front.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.SessionKey != "" || got.StartedConfigHash != "" || got.ContinuationResetPending != "true" {
				t.Fatal("unexpected death did not reset continuity")
			}
			if age == 10*time.Second && (!crashed || got.WakeAttempts != 1) {
				t.Fatal("rapid crash accounting changed")
			}
			if age == 99*time.Second && (!churned || got.ChurnCount != "1") {
				t.Fatal("churn accounting changed")
			}
		})
	}
}

type exitReadFailureStore struct{ beads.Store }

func (s exitReadFailureStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("injected authoritative read failure")
}

func TestCurrentSessionExitDefersUnavailableAndClosedState(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{true: "closed", false: "read failure"}[closed], func(t *testing.T) {
			store, front, info, _ := exitGuardFixture(t, time.Minute)
			if closed {
				if err := store.Close(info.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				front = sessionFrontDoor(exitReadFailureStore{Store: store})
			}
			called := false
			applied, err := withCurrentSessionExit(info, false, front, func() error { called = true; return nil })
			if called || applied {
				t.Fatal("unavailable/closed session entered mutation region")
			}
			if !closed && err == nil {
				t.Fatal("authoritative read failure was swallowed")
			}
		})
	}
}

func TestCurrentSessionExitBypassesStaleCachingStore(t *testing.T) {
	store, _, info, _ := exitGuardFixture(t, time.Minute)
	cache := beads.NewCachingStore(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	front := sessionFrontDoor(cache)
	snapshot, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	mgr := sessionpkg.NewManagerWithOptions(store, runtime.NewFake())
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatal(err)
	}
	cached, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cached.SleepReason == "user-hold" {
		t.Fatal("negative control: snapshot cache unexpectedly refreshed")
	}
	called := false
	applied, err := withCurrentSessionExit(snapshot, false, front, func() error { called = true; return nil })
	if err != nil || called || applied {
		t.Fatalf("cached stale state reached mutation: called=%v applied=%v err=%v", called, applied, err)
	}
}

type suspendNoticeProvider struct {
	*runtime.Fake
	entered chan struct{}
}

func (p *suspendNoticeProvider) Stop(name string) error {
	close(p.entered)
	return p.Fake.Stop(name)
}

func TestCurrentSessionExitSerializesSuspendAcrossWholeDecision(t *testing.T) {
	store, front, info, _ := exitGuardFixture(t, time.Minute)
	provider := &suspendNoticeProvider{Fake: runtime.NewFake(), entered: make(chan struct{})}
	mgr := sessionpkg.NewManagerWithOptions(store, provider)
	begin, done := make(chan struct{}), make(chan error, 1)
	go func() { <-begin; done <- mgr.Suspend(info.ID) }()
	applied, err := withCurrentSessionExit(info, false, front, func() error {
		// Simulate heal and a later exit-accounting write. Suspend must not slip
		// between the two and have its intentional reason overwritten afterward.
		if err := front.ApplyPatch(info.ID, sessionpkg.MetadataPatch{"state": "asleep"}); err != nil {
			return err
		}
		close(begin)
		select {
		case <-provider.entered:
			t.Error("Suspend entered runtime Stop during guarded exit decision")
		case <-time.After(10 * time.Second):
		}
		return front.ApplyPatch(info.ID, sessionpkg.MetadataPatch{"sleep_reason": "runtime-missing"})
	})
	if err != nil || !applied {
		t.Fatalf("guard failed: applied=%v err=%v", applied, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Suspend did not complete after guard released")
	}
	current, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.SleepReason != "user-hold" || current.MetadataState != "suspended" {
		t.Fatal("later exit write overwrote Suspend")
	}
}

// The provider boundary models the HTTP Suspend completing after the tick's
// persisted snapshot, but before its runtime liveness observation.
type suspendOnObservationProvider struct {
	*runtime.Fake
	suspend func() error
	fired   bool
	err     error
}

func (p *suspendOnObservationProvider) IsRunning(name string) bool {
	if !p.fired {
		p.fired = true
		p.err = p.suspend()
	}
	return p.Fake.IsRunning(name)
}

func TestReconcileSessionBeadsDoesNotUndoConcurrentSuspend(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state": "awake", "provider": "codex", "session_origin": "manual",
		"session_key": "retained-conversation", "started_config_hash": "configured-launch",
		"last_woke_at":  env.clk.Now().Add(-99 * time.Second).Format(time.RFC3339),
		"wake_attempts": "0", "churn_count": "0",
	})
	sp := &suspendOnObservationProvider{Fake: env.sp}
	mgr := sessionpkg.NewManagerWithOptions(env.store, sp)
	sp.suspend = func() error { return mgr.Suspend(session.ID) }
	reconcileSessionBeads(context.Background(), []beads.Bead{session}, env.desiredState,
		map[string]bool{"worker": true}, env.cfg, sp, env.store, nil, nil, nil,
		env.dt, map[string]int{"worker": 1}, false, nil, "", nil, env.clk, env.rec, 0, 0,
		&env.stdout, &env.stderr, env.startOptions...)
	if !sp.fired || sp.err != nil {
		t.Fatalf("Suspend boundary not exercised: fired=%v err=%v", sp.fired, sp.err)
	}
	got := env.sessionInfo(session.ID)
	if got.SessionKey != "retained-conversation" || got.StartedConfigHash != "configured-launch" || got.ContinuationResetPending == "true" {
		t.Errorf("controller lost suspended conversation: key=%q hash=%q reset=%q", got.SessionKey, got.StartedConfigHash, got.ContinuationResetPending)
	}
	if got.ChurnCount != "0" || got.WakeAttempts != 0 || got.SleepReason != "user-hold" {
		t.Errorf("controller overwrote intentional exit: churn=%q wake=%d reason=%q", got.ChurnCount, got.WakeAttempts, got.SleepReason)
	}
	if env.sp.IsRunning("worker") {
		t.Error("controller woke the suspended runtime from stale snapshot")
	}
}
