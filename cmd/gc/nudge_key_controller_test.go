package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type inputFencedNudgeProvider struct {
	*runtime.Fake
	fenced bool
}

func (p *inputFencedNudgeProvider) Nudge(name string, content []runtime.ContentBlock) error {
	if err := p.Fake.Nudge(name, content); err != nil {
		return err
	}
	if p.fenced {
		return runtime.ErrInputFenced
	}
	return nil
}

func (p *inputFencedNudgeProvider) NudgeNow(name string, content []runtime.ContentBlock) error {
	if err := p.Fake.NudgeNow(name, content); err != nil {
		return err
	}
	if p.fenced {
		return runtime.ErrInputFenced
	}
	return nil
}

func TestDiscoverDueExactNudgeSessionIDs_StableDeduplicatesDueItems(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	state := nudgequeue.State{
		Pending: []nudgequeue.Item{
			{ID: "first", SessionID: "gc-a", DeliverAfter: now.Add(-time.Second)},
			{ID: "future", SessionID: "gc-future", DeliverAfter: now.Add(time.Second)},
			{ID: "duplicate", SessionID: "gc-a", DeliverAfter: now.Add(-time.Second)},
			{ID: "unkeyed", DeliverAfter: now.Add(-time.Second)},
			{ID: "invalid", SessionID: " bad", DeliverAfter: now.Add(-time.Second)},
		},
		InFlight: []nudgequeue.Item{
			{ID: "expired", SessionID: "gc-b", LeaseUntil: now.Add(-time.Second)},
			{ID: "leased", SessionID: "gc-c", LeaseUntil: now.Add(time.Second)},
		},
	}

	got := discoverDueExactNudgeSessionIDs(state, now)
	want := []string{"gc-a", "gc-b"}
	if len(got) != len(want) {
		t.Fatalf("discoverDueExactNudgeSessionIDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("discoverDueExactNudgeSessionIDs() = %v, want %v", got, want)
		}
	}
}

func TestReconcileExactQueuedNudge_InputFenceParksWithoutRetryThenDeliversOnce(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := &inputFencedNudgeProvider{Fake: runtime.NewFake(), fenced: true}
	mgr := newSessionManagerWithConfig(dir, store.Store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	item := newQueuedNudgeWithOptions("worker", "copy-mode fenced", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: info.ID})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	if err := reconcileExactQueuedNudge(context.Background(), info.ID, exactQueuedNudgeParams{CityPath: dir, Config: &config.City{}, Provider: fake, SessionStore: store.Store, NudgeStore: store.Store}); err != nil {
		t.Fatalf("reconcileExactQueuedNudge while input fenced: %v", err)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState while input fenced: %v", err)
	}
	if len(state.Pending) != 1 || len(state.InFlight) != 0 || len(state.Dead) != 0 {
		t.Fatalf("input-fenced queue outcome pending/inflight/dead = %d/%d/%d, want 1/0/0", len(state.Pending), len(state.InFlight), len(state.Dead))
	}
	if got := state.Pending[0]; got.Attempts != 0 || !got.LastAttemptAt.IsZero() || got.LastError != "" || !got.ClaimedAt.IsZero() || !got.LeaseUntil.IsZero() {
		t.Fatalf("input-fenced item = %+v, want released without attempt, backoff, or error", got)
	}
	shadow, ok, err := nudgeFrontDoor(store).FindIncludingTerminal(item.ID)
	if err != nil {
		t.Fatalf("FindIncludingTerminal while input fenced: %v", err)
	}
	if !ok || !shadow.Open {
		t.Fatalf("input-fenced shadow = %+v, want open command", shadow)
	}
	refetched, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("refetch session while input fenced: %v", err)
	}
	if refetched.Metadata[session.MetadataLastNudgeDeliveredAt] != "" {
		t.Fatalf("session has %s while input fenced", session.MetadataLastNudgeDeliveredAt)
	}

	fake.fenced = false
	if err := reconcileExactQueuedNudge(context.Background(), info.ID, exactQueuedNudgeParams{CityPath: dir, Config: &config.City{}, Provider: fake, SessionStore: store.Store, NudgeStore: store.Store}); err != nil {
		t.Fatalf("reconcileExactQueuedNudge after input fence clears: %v", err)
	}
	if got := fake.CountCalls("Nudge", info.SessionName); got != 2 {
		t.Fatalf("Nudge calls after fence clears = %d, want 2 including the fenced attempt", got)
	}
	state, err = nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState after input fence clears: %v", err)
	}
	if len(state.Pending)+len(state.InFlight)+len(state.Dead) != 0 {
		t.Fatalf("queue not acknowledged after input fence clears: pending=%d in_flight=%d dead=%d", len(state.Pending), len(state.InFlight), len(state.Dead))
	}
	refetched, err = store.Get(info.ID)
	if err != nil {
		t.Fatalf("refetch session after input fence clears: %v", err)
	}
	if refetched.Metadata[session.MetadataLastNudgeDeliveredAt] == "" {
		t.Fatalf("session missing %s after delivery", session.MetadataLastNudgeDeliveredAt)
	}
}

func TestReconcileExactQueuedNudge_ParksWhileAttachedThenDeliversWhenDetached(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store.Store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.SetAttached(info.SessionName, true)
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	item := newQueuedNudgeWithOptions("worker", "exact work", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: info.ID})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	if err := reconcileExactQueuedNudge(context.Background(), info.ID, exactQueuedNudgeParams{CityPath: dir, Config: &config.City{}, Provider: fake, SessionStore: store.Store, NudgeStore: store.Store}); err != nil {
		t.Fatalf("reconcileExactQueuedNudge: %v", err)
	}
	if got := fake.CountCalls("Nudge", info.SessionName); got != 0 {
		t.Fatalf("Nudge calls while attached = %d, want 0", got)
	}
	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("attached queue outcome pending/inflight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	shadow, ok, err := nudgeFrontDoor(store).FindIncludingTerminal(item.ID)
	if err != nil {
		t.Fatalf("FindIncludingTerminal: %v", err)
	}
	if !ok || !shadow.Open {
		t.Fatalf("attached shadow = %+v, want open nonterminal command", shadow)
	}
	refetched, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("refetch session: %v", err)
	}
	if refetched.Metadata[session.MetadataLastNudgeDeliveredAt] != "" {
		t.Fatalf("session has %s while attached", session.MetadataLastNudgeDeliveredAt)
	}

	fake.SetAttached(info.SessionName, false)
	if err := reconcileExactQueuedNudge(context.Background(), info.ID, exactQueuedNudgeParams{CityPath: dir, Config: &config.City{}, Provider: fake, SessionStore: store.Store, NudgeStore: store.Store}); err != nil {
		t.Fatalf("reconcileExactQueuedNudge after detach: %v", err)
	}
	if got := fake.CountCalls("Nudge", info.SessionName); got != 1 {
		t.Fatalf("Nudge calls after detach = %d, want 1", got)
	}
	pending, inFlight, dead, err = listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges after detach: %v", err)
	}
	if len(pending)+len(inFlight)+len(dead) != 0 {
		t.Fatalf("queue not acknowledged after detach: pending=%d in_flight=%d dead=%d", len(pending), len(inFlight), len(dead))
	}
	shadow, ok, err = nudgeFrontDoor(store).FindIncludingTerminal(item.ID)
	if err != nil {
		t.Fatalf("FindIncludingTerminal after detach: %v", err)
	}
	if !ok || shadow.Open || shadow.State != "injected" || shadow.CommitBoundary != "provider-nudge-return" {
		t.Fatalf("terminal shadow = %+v, want closed injected provider-nudge-return", shadow)
	}
	refetched, err = store.Get(info.ID)
	if err != nil {
		t.Fatalf("refetch session after detach: %v", err)
	}
	if refetched.Metadata[session.MetadataLastNudgeDeliveredAt] == "" {
		t.Fatalf("session missing %s after detached exact delivery", session.MetadataLastNudgeDeliveredAt)
	}
}

func TestReconcileExactQueuedNudge_StaleContinuationRemainsFencedAtDelivery(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store.Store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	item := newQueuedNudgeWithOptions("worker", "stale", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: info.ID, ContinuationEpoch: "0"})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	if err := reconcileExactQueuedNudge(context.Background(), info.ID, exactQueuedNudgeParams{CityPath: dir, Config: &config.City{}, Provider: fake, SessionStore: store.Store, NudgeStore: store.Store}); err != nil {
		t.Fatalf("reconcileExactQueuedNudge: %v", err)
	}
	if got := fake.CountCalls("Nudge", info.SessionName); got != 0 {
		t.Fatalf("Nudge calls = %d, want 0 for stale continuation", got)
	}
	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 1 {
		t.Fatalf("stale continuation durable outcome pending/inflight/dead = %d/%d/%d, want 0/0/1", len(pending), len(inFlight), len(dead))
	}
}

func TestNudgeKeyController_CoalescesAndDrains(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan string, 2)
	var calls atomic.Int32
	controller, err := newNudgeKeyController(nudgeKeyControllerOptions{
		Workers: 1, MaxDistinct: 1, MaxRetries: 0,
		Reconcile: func(_ context.Context, id string) error {
			call := calls.Add(1)
			started <- struct{}{}
			if call == 1 {
				<-release
			}
			done <- id
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(controller.Stop)
	if err := controller.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if outcome, err := controller.Admit("gc-a"); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("first Admit = (%s, %v), want accepted", outcome, err)
	}
	<-started
	if outcome, err := controller.Admit("gc-a"); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("second Admit = (%s, %v), want coalesced", outcome, err)
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case got := <-done:
			if got != "gc-a" {
				t.Fatalf("reconciled %q, want gc-a", got)
			}
		case <-time.After(hangBudget):
			t.Fatalf("controller did not drain reconcile %d", i+1)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("reconcile calls = %d, want 2 so the in-flight admission is not lost", got)
	}
}

func TestNudgeKeyController_OverflowRequestsAudit(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	controller, err := newNudgeKeyController(nudgeKeyControllerOptions{
		Workers: 1, MaxDistinct: 1, MaxRetries: 0,
		Reconcile: func(context.Context, string) error { close(started); <-release; return nil },
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer controller.Stop()
	if err := controller.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := controller.Admit("gc-first"); err != nil {
		t.Fatalf("first Admit: %v", err)
	}
	<-started
	if outcome, err := controller.Admit("gc-overflow"); err != nil || outcome != sessionStartAdmissionOverflow {
		t.Fatalf("overflow Admit = (%s, %v), want overflow", outcome, err)
	}
	if !controller.TakeAuditRequest() {
		t.Fatal("overflow did not request an authoritative audit")
	}
	close(release)
}

func TestNudgeKeyController_RecoversFromPanic(t *testing.T) {
	completed := make(chan string, 1)
	controller, err := newNudgeKeyController(nudgeKeyControllerOptions{
		Workers: 1, MaxDistinct: 2, MaxRetries: 0,
		Reconcile: func(_ context.Context, id string) error {
			if id == "gc-panic" {
				panic("boom")
			}
			completed <- id
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer controller.Stop()
	if err := controller.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := controller.Admit("gc-panic"); err != nil {
		t.Fatalf("panic Admit: %v", err)
	}
	if _, err := controller.Admit("gc-next"); err != nil {
		t.Fatalf("next Admit: %v", err)
	}
	select {
	case got := <-completed:
		if got != "gc-next" {
			t.Fatalf("completed %q, want gc-next", got)
		}
	case <-time.After(hangBudget):
		t.Fatal("worker did not continue after panic")
	}
}
