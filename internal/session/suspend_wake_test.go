package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

// wakeRow is an active session row carrying a pending explicit wake.
func wakeRow(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "worker", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{
		"session_name":      "s-wake",
		"template":          "worker",
		"provider":          "claude",
		"state":             string(StateActive),
		"wake_request":      string(WakeCauseExplicit),
		"wake_requested_at": "2026-10-08T11:00:00Z",
	}})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	return b
}

// TestOperatorSuspendClearsPendingWake is D7 rule 3: `gc session suspend`
// (its fallback and the API both reach Manager.Suspend) supersedes a pending
// wake in its own write; the city stop sweep leaves it for the next start.
func TestOperatorSuspendClearsPendingWake(t *testing.T) {
	for name, tc := range map[string]struct {
		suspend func(m *Manager, id string) error
		cleared bool
	}{
		"operator": {suspend: (*Manager).Suspend, cleared: true},
		"shutdown": {suspend: (*Manager).SuspendForShutdown},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := NewManagerWithOptions(store, sp, WithCityPath(t.TempDir()))
			b := wakeRow(t, store)
			if err := sp.Start(context.Background(), "s-wake", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			if err := tc.suspend(mgr, b.ID); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			got, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata["state"] != string(StateSuspended) {
				t.Fatalf("state = %q, want suspended", got.Metadata["state"])
			}
			if cleared := got.Metadata["wake_request"] == "" && got.Metadata["wake_requested_at"] == ""; cleared != tc.cleared {
				t.Fatalf("wake request %q/%q, want cleared=%v", got.Metadata["wake_request"], got.Metadata["wake_requested_at"], tc.cleared)
			}
		})
	}
}

// TestOperatorSuspendHoldsTheRow is mc-esqo7: every operator suspend writes
// OperatorSuspendPatch's user hold, so legacy's assigned-work wake cannot undo
// it. Manager.Suspend (the CLI fallback and the API) holds an active row and a
// row the shutdown sweep left suspended without the hold; the sweep itself
// holds nothing.
func TestOperatorSuspendHoldsTheRow(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	wantHeld := now.Add(IndefiniteHoldDuration).Format(time.RFC3339)
	for name, tc := range map[string]struct {
		state   State
		suspend func(m *Manager, id string) error
		held    bool
	}{
		"operator":                    {state: StateActive, suspend: (*Manager).Suspend, held: true},
		"operator on sweep-suspended": {state: StateSuspended, suspend: (*Manager).Suspend, held: true},
		"shutdown":                    {state: StateActive, suspend: (*Manager).SuspendForShutdown},
		"chat idle":                   {state: StateActive, suspend: idleSuspend},
		"chat idle on suspended":      {state: StateSuspended, suspend: idleSuspend},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			mgr := NewManagerWithOptions(store, runtime.NewFake(), WithClock(&clock.Fake{Time: now}))
			b := wakeRow(t, store)
			if err := store.SetMetadata(b.ID, "state", string(tc.state)); err != nil {
				t.Fatal(err)
			}
			if err := tc.suspend(mgr, b.ID); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			got, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			held := got.Metadata["held_until"] == wantHeld && got.Metadata["sleep_intent"] == string(SleepReasonUserHold) &&
				got.Metadata["suspended_at"] == now.Format(time.RFC3339)
			if got.Metadata["held_until"] != "" && !held || held != tc.held || got.Metadata["state"] != string(StateSuspended) {
				t.Fatalf("state=%q held_until=%q sleep_intent=%q, want suspended held=%v", got.Metadata["state"], got.Metadata["held_until"], got.Metadata["sleep_intent"], tc.held)
			}
		})
	}
}

// TestOperatorSuspendRefusesARowClosedSinceRead: the suspend write is
// decided from a fresh read, so a row closed under it gets no hold.
func TestOperatorSuspendRefusesARowClosedSinceRead(t *testing.T) {
	store := beads.NewMemStore()
	b := wakeRow(t, store)
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	err := NewStore(beads.SessionStore{Store: store}).OperatorSuspend(b.ID, time.Now())
	var illegal *IllegalTransitionError
	if !errors.As(err, &illegal) {
		t.Fatalf("OperatorSuspend on a closed row = %v, want an illegal transition", err)
	}
	if got, _ := store.Get(b.ID); got.Metadata["held_until"] != "" || got.Metadata["sleep_intent"] != "" {
		t.Fatalf("closed row was written: held_until=%q sleep_intent=%q", got.Metadata["held_until"], got.Metadata["sleep_intent"])
	}
}

func idleSuspend(m *Manager, id string) error { return m.SuspendIdle(context.Background(), id) }

// TestChatIdleSuspendHoldsNothing: the chat idle auto-suspend clears a
// pending wake (D7) but writes no hold, so the session resumes on its next
// wake reason; it keeps the operator's transition rules.
func TestChatIdleSuspendHoldsNothing(t *testing.T) {
	store := beads.NewMemStore()
	mgr := NewManagerWithOptions(store, runtime.NewFake())
	b := wakeRow(t, store)
	if err := mgr.SuspendIdle(context.Background(), b.ID); err != nil {
		t.Fatalf("SuspendIdle: %v", err)
	}
	got, _ := store.Get(b.ID)
	if got.Metadata["state"] != string(StateSuspended) || got.Metadata["held_until"] != "" || got.Metadata["sleep_intent"] != "" || got.Metadata["wake_request"] != "" {
		t.Fatalf("state=%q held_until=%q sleep_intent=%q wake_request=%q, want suspended, unheld, wake cleared",
			got.Metadata["state"], got.Metadata["held_until"], got.Metadata["sleep_intent"], got.Metadata["wake_request"])
	}
	draining := wakeRow(t, store)
	if err := store.SetMetadata(draining.ID, "state", string(StateDraining)); err != nil {
		t.Fatal(err)
	}
	var illegal *IllegalTransitionError
	if err := mgr.SuspendIdle(context.Background(), draining.ID); !errors.As(err, &illegal) {
		t.Fatalf("SuspendIdle of a draining row = %v, want the operator's illegal transition", err)
	}
}

// stopSeesProvider records the row the first Stop finds, can fail the stop,
// and can run a concurrent write during it.
type stopSeesProvider struct {
	*runtime.Fake
	store  beads.Store
	id     string
	seen   map[string]string
	fail   bool
	during func()
}

func (p *stopSeesProvider) Stop(name string) error {
	if p.seen == nil {
		got, _ := p.store.Get(p.id)
		p.seen = got.Metadata
		if p.during != nil {
			p.during()
		}
	}
	if p.fail {
		return errors.New("stop failed")
	}
	return p.Fake.Stop(name)
}

// TestOperatorSuspendHoldsBeforeTheStop: the hold is written before the
// runtime is torn down, so a tick between the two drains the row rather than
// restarting it; a failed stop restores the row, which never reads suspended
// over a live runtime.
func TestOperatorSuspendHoldsBeforeTheStop(t *testing.T) {
	for _, fail := range []bool{false, true} {
		store := beads.NewMemStore()
		b := wakeRow(t, store)
		sp := &stopSeesProvider{Fake: runtime.NewFake(), store: store, id: b.ID, fail: fail}
		if err := sp.Start(context.Background(), "s-wake", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		err := NewManagerWithOptions(store, sp).Suspend(b.ID)
		if sp.seen["sleep_intent"] != string(SleepReasonUserHold) || sp.seen["held_until"] == "" {
			t.Fatalf("fail=%v: the stop found sleep_intent=%q held_until=%q, want the hold already written", fail, sp.seen["sleep_intent"], sp.seen["held_until"])
		}
		got, _ := store.Get(b.ID)
		if restored := got.Metadata["state"] == string(StateActive) && got.Metadata["held_until"] == "" &&
			got.Metadata["wake_request"] == string(WakeCauseExplicit); (err != nil) != fail || restored != fail {
			t.Fatalf("fail=%v: err=%v state=%q held_until=%q wake_request=%q", fail, err, got.Metadata["state"], got.Metadata["held_until"], got.Metadata["wake_request"])
		}
	}
}

// TestFailedStopRollbackLeavesANewerWriteAlone: the failed-stop rollback is
// fenced and conditional on this suspend's hold and stamp, so a write that
// replaced them during the stop (a wake, a newer suspend) survives it.
func TestFailedStopRollbackLeavesANewerWriteAlone(t *testing.T) {
	store := beads.NewMemStore()
	b := wakeRow(t, store)
	sp := &stopSeesProvider{Fake: runtime.NewFake(), store: store, id: b.ID, fail: true}
	sp.during = func() {
		if err := store.SetMetadataBatch(b.ID, map[string]string{"held_until": "", "sleep_intent": "", "suspended_at": "", "state": string(StateAsleep)}); err != nil {
			t.Error(err)
		}
	}
	if err := sp.Start(context.Background(), "s-wake", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := NewManagerWithOptions(store, sp).Suspend(b.ID); err == nil {
		t.Fatal("Suspend with a failing stop succeeded")
	}
	if got, _ := store.Get(b.ID); got.Metadata["state"] != string(StateAsleep) || got.Metadata["wake_request"] != "" {
		t.Fatalf("state=%q wake_request=%q, want the newer write kept (no rollback over it)", got.Metadata["state"], got.Metadata["wake_request"])
	}
}

// everyGetWrites lets a concurrent writer win every fence: it writes an
// unrelated key after each read.
type everyGetWrites struct {
	beads.Store
	n int
}

func (s *everyGetWrites) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	s.n++
	_ = s.SetMetadata(id, "detached_at", fmt.Sprint(s.n))
	return b, err
}

func (s *everyGetWrites) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// TestApplyKeepingUserHoldReportsContention: every CAS lost is an error, not
// a silent no-op; a closed row writes nothing with no error.
func TestApplyKeepingUserHoldReportsContention(t *testing.T) {
	mem := beads.NewMemStore()
	b := wakeRow(t, mem)
	stamped := stampedMemStore(t, gate.Auto, mem)
	if _, err := NewStore(beads.SessionStore{Store: &everyGetWrites{Store: stamped}}).ApplyKeepingUserHold(b.ID, SleepPatch(time.Now(), "idle")); err == nil {
		t.Fatal("ApplyKeepingUserHold under a writer winning every fence returned no error")
	}
	if err := mem.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	if written, err := NewStore(beads.SessionStore{Store: mem}).ApplyKeepingUserHold(b.ID, SleepPatch(time.Now(), "idle")); written != nil || err != nil {
		t.Fatalf("ApplyKeepingUserHold on a closed row = %v, %v; want nil, nil", written, err)
	}
}

// TestKeepUserHold: a sleep decided from an older read keeps the operator's
// user-hold intent a fresh read shows; any other intent is cleared as the
// patch says.
func TestKeepUserHold(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for intent, wantKept := range map[string]bool{"user-hold": true, "idle-stop-pending": false, "wait-hold": false, "": false} {
		patch := SleepPatch(now, "idle")
		got := KeepUserHold(Info{SleepIntent: intent}, patch)
		if kept := got["sleep_intent"] == "user-hold"; kept != wantKept || (!kept && got["sleep_intent"] != "") {
			t.Errorf("intent %q: sleep_intent written %q, want kept=%v", intent, got["sleep_intent"], wantKept)
		}
		if _, ok := patch["sleep_intent"]; !ok {
			t.Errorf("intent %q: KeepUserHold mutated its input", intent)
		}
	}
}

// TestApplyKeepingUserHoldReadsTheRowFresh: the decision comes from the
// store's row, not the caller's snapshot: a suspend that landed after the
// snapshot keeps its intent through a drain completion.
func TestApplyKeepingUserHoldReadsTheRowFresh(t *testing.T) {
	store := beads.NewMemStore()
	b := wakeRow(t, store)
	front := NewStore(beads.SessionStore{Store: store})
	if err := front.OperatorSuspend(b.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	written, err := front.ApplyKeepingUserHold(b.ID, CompleteDrainPatch(time.Now(), "user-hold", false))
	if err != nil || written == nil {
		t.Fatalf("ApplyKeepingUserHold = %v, %v", written, err)
	}
	if got, _ := store.Get(b.ID); got.Metadata["sleep_intent"] != string(SleepReasonUserHold) || got.Metadata["state"] != string(StateAsleep) {
		t.Fatalf("state=%q sleep_intent=%q, want asleep with the user hold kept", got.Metadata["state"], got.Metadata["sleep_intent"])
	}
}
