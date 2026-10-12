package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
		"operator": {suspend: operatorSuspend, cleared: true},
		"shutdown": {suspend: sweepSuspend},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := newTestManager(t, store, sp)
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
// the Suspend cell's user hold, so legacy's assigned-work wake cannot undo
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
		"operator":                    {state: StateActive, suspend: operatorSuspend, held: true},
		"operator on sweep-suspended": {state: StateSuspended, suspend: operatorSuspend, held: true},
		"shutdown":                    {state: StateActive, suspend: sweepSuspend},
		"chat idle":                   {state: StateActive, suspend: idleSuspend},
		"chat idle on suspended":      {state: StateSuspended, suspend: idleSuspend},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			mgr := newTestManager(t, store, runtime.NewFake(), WithClock(&clock.Fake{Time: now}))
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
	_, err := NewStore(beads.SessionStore{Store: store}).OperatorSuspend(b.ID, time.Now())
	var illegal *IllegalTransitionError
	if !errors.As(err, &illegal) {
		t.Fatalf("OperatorSuspend on a closed row = %v, want an illegal transition", err)
	}
	if got, _ := store.Get(b.ID); got.Metadata["held_until"] != "" || got.Metadata["sleep_intent"] != "" {
		t.Fatalf("closed row was written: held_until=%q sleep_intent=%q", got.Metadata["held_until"], got.Metadata["sleep_intent"])
	}
}

func idleSuspend(m *Manager, id string) error {
	return m.Suspend(context.Background(), testActor(m, ActorOperator), id, true)
}

// TestChatIdleSuspendHoldsNothing: the chat idle auto-suspend clears a
// pending wake (D7) but writes no hold, so the session resumes on its next
// wake reason; it keeps the operator's transition rules.
func TestChatIdleSuspendHoldsNothing(t *testing.T) {
	store := beads.NewMemStore()
	mgr := newTestManager(t, store, runtime.NewFake())
	b := wakeRow(t, store)
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), b.ID, true); err != nil {
		t.Fatalf("idle Suspend: %v", err)
	}
	got, _ := store.Get(b.ID)
	if got.Metadata["state"] != string(StateSuspended) || got.Metadata["held_until"] != "" || got.Metadata["sleep_intent"] != "" || got.Metadata["wake_request"] != "" {
		t.Fatalf("state=%q held_until=%q sleep_intent=%q wake_request=%q, want suspended, unheld, wake cleared",
			got.Metadata["state"], got.Metadata["held_until"], got.Metadata["sleep_intent"], got.Metadata["wake_request"])
	}
	if _, err := time.Parse(time.RFC3339, got.Metadata["suspended_at"]); err != nil {
		t.Fatalf("suspended_at = %q, want the SuspendSoft cell's stamp", got.Metadata["suspended_at"])
	}
	draining := wakeRow(t, store)
	if err := store.SetMetadata(draining.ID, "state", string(StateDraining)); err != nil {
		t.Fatal(err)
	}
	var illegal *IllegalTransitionError
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), draining.ID, true); !errors.As(err, &illegal) {
		t.Fatalf("idle Suspend of a draining row = %v, want the operator's illegal transition", err)
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
		err := operatorSuspend(newTestManager(t, store, sp), b.ID)
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
	if err := operatorSuspend(newTestManager(t, store, sp), b.ID); err == nil {
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

// TestApplySleepReportsContention: every CAS lost is an error, not
// a silent no-op; a closed row writes nothing with no error.
func TestApplySleepReportsContention(t *testing.T) {
	mem := beads.NewMemStore()
	b := wakeRow(t, mem)
	stamped := stampedMemStore(t, gate.Auto, mem)
	if _, err := NewStore(beads.SessionStore{Store: &everyGetWrites{Store: stamped}}).ApplySleep(b.ID, SleepPatch(time.Now(), "idle")); err == nil {
		t.Fatal("ApplySleep under a writer winning every fence returned no error")
	}
	if err := mem.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	if written, err := NewStore(beads.SessionStore{Store: mem}).ApplySleep(b.ID, SleepPatch(time.Now(), "idle")); written != nil || err != nil {
		t.Fatalf("ApplySleep on a closed row = %v, %v; want nil, nil", written, err)
	}
}

// TestApplySleepKeepsTheHolds: a sleep decided from an older read keeps the
// operator's user-hold intent a fresh read shows, and a wait's intent (D-J 3);
// only the idle stop's transient is cleared.
func TestApplySleepKeepsTheHolds(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for intent, want := range map[string]string{"user-hold": "user-hold", "idle-stop-pending": "", "wait-hold": "wait-hold", "": ""} {
		store := beads.NewMemStore()
		b := wakeRow(t, store)
		if err := store.SetMetadataBatch(b.ID, map[string]string{"sleep_intent": intent}); err != nil {
			t.Fatal(err)
		}
		patch := SleepPatch(now, "idle")
		if _, err := NewStore(beads.SessionStore{Store: store}).ApplySleep(b.ID, patch); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(b.ID)
		if got.Metadata["sleep_intent"] != want || got.Metadata["state"] != string(StateAsleep) {
			t.Errorf("intent %q: after the sleep state=%q sleep_intent=%q, want asleep with %q", intent, got.Metadata["state"], got.Metadata["sleep_intent"], want)
		}
		if !maps.Equal(patch, SleepPatch(now, "idle")) {
			t.Errorf("intent %q: ApplySleep mutated its input", intent)
		}
	}
}

// TestApplySleepReadsTheRowFresh: the decision comes from the
// store's row, not the caller's snapshot: a suspend that landed after the
// snapshot keeps its intent through a drain completion.
func TestApplySleepReadsTheRowFresh(t *testing.T) {
	store := beads.NewMemStore()
	b := wakeRow(t, store)
	front := NewStore(beads.SessionStore{Store: store})
	if _, err := front.OperatorSuspend(b.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	written, err := front.ApplySleep(b.ID, CompleteDrainPatch(time.Now(), "user-hold", false))
	if err != nil || written == nil {
		t.Fatalf("ApplySleep = %v, %v", written, err)
	}
	if got, _ := store.Get(b.ID); got.Metadata["sleep_intent"] != string(SleepReasonUserHold) || got.Metadata["state"] != string(StateAsleep) {
		t.Fatalf("state=%q sleep_intent=%q, want asleep with the user hold kept", got.Metadata["state"], got.Metadata["sleep_intent"])
	}
}

// TestOperatorWakeClearsASuspend is D-J 1: `gc session wake` on a row an
// operator suspended clears the whole hold (held_until, the user-hold intent,
// suspended_at) with the wake request, in one write.
func TestOperatorWakeClearsASuspend(t *testing.T) {
	store := beads.NewMemStore()
	b := wakeRow(t, store)
	front := NewStore(beads.SessionStore{Store: store})
	if _, err := front.OperatorSuspend(b.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if held, _ := store.Get(b.ID); held.Metadata["held_until"] == "" || held.Metadata["suspended_at"] == "" || held.Metadata["sleep_intent"] != string(SleepReasonUserHold) {
		t.Fatalf("suspend wrote %v, want the operator hold", held.Metadata)
	}
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	if _, err := front.WakeSession(b.ID, now, WakeOpts{}); err != nil {
		t.Fatal(err)
	}
	m := mustGet(t, store, b.ID).Metadata
	for _, k := range dispositionKeys {
		if m[k] != "" {
			t.Errorf("%s = %q after the wake, want cleared", k, m[k])
		}
	}
	if m["wake_request"] != string(WakeCauseExplicit) || m["wake_requested_at"] != now.Format(time.RFC3339) {
		t.Errorf("wake request %q at %q, want explicit at %s", m["wake_request"], m["wake_requested_at"], now.Format(time.RFC3339))
	}
	if d := DecodeDisposition(m); d.SuppressesWake(now) {
		t.Errorf("disposition %+v still suppresses a wake", d)
	}
}

// TestWaitEndKeepsAnOperatorHold: a wait begun and ended on a row an
// operator suspended moves only wait_hold; the user-hold intent and
// held_until stand throughout (the WaitBegin and WaitEnd cells).
func TestWaitEndKeepsAnOperatorHold(t *testing.T) {
	store := beads.NewMemStore()
	b := wakeRow(t, store)
	front := NewStore(beads.SessionStore{Store: store})
	if _, err := front.OperatorSuspend(b.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	held := mustGet(t, store, b.ID).Metadata
	if err := front.SetWaitHold(b.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	waiting := mustGet(t, store, b.ID).Metadata
	if err := front.SetWaitHold(b.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	ended := mustGet(t, store, b.ID).Metadata
	if waiting["wait_hold"] != "true" || ended["wait_hold"] != "" {
		t.Fatalf("wait_hold %q while waiting, %q after, want true then cleared", waiting["wait_hold"], ended["wait_hold"])
	}
	for _, k := range []string{"held_until", "sleep_intent", "suspended_at", "state"} {
		if waiting[k] != held[k] || ended[k] != held[k] {
			t.Errorf("%s: %q suspended, %q waiting, %q ended; want the operator's to stand", k, held[k], waiting[k], ended[k])
		}
	}
}

func mustGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestApplyLifecycleIsFencedAndMovesNoHold: a lifecycle write decided on an
// older read lands only on the read it decided (every CAS lost is an error),
// writes nothing to a closed row, and refuses a disposition key, which only
// a cell writes.
func TestApplyLifecycleIsFencedAndMovesNoHold(t *testing.T) {
	mem := beads.NewMemStore()
	b := wakeRow(t, mem)
	ack := AcknowledgeDrainPatch(time.Now(), false)
	stamped := stampedMemStore(t, gate.Auto, mem)
	if _, err := NewStore(beads.SessionStore{Store: &everyGetWrites{Store: stamped}}).ApplyLifecycle(b.ID, ack); err == nil {
		t.Fatal("ApplyLifecycle under a writer winning every fence returned no error")
	}
	front := NewStore(beads.SessionStore{Store: mem})
	if _, err := front.ApplyLifecycle(b.ID, MetadataPatch{"state": "asleep", "sleep_intent": ""}); err == nil {
		t.Fatal("ApplyLifecycle wrote a disposition key")
	}
	written, err := front.ApplyLifecycle(b.ID, ack)
	if err != nil || written["state"] != string(StateDrained) || mustGet(t, mem, b.ID).Metadata["state"] != string(StateDrained) {
		t.Fatalf("ApplyLifecycle = %v, %v; want the ack written", written, err)
	}
	if err := mem.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	before := mustGet(t, mem, b.ID).Metadata
	if written, err := front.ApplyLifecycle(b.ID, MetadataPatch{"state": "asleep"}); written != nil || err != nil || mustGet(t, mem, b.ID).Metadata["state"] != before["state"] {
		t.Fatalf("ApplyLifecycle on a closed row = %v, %v; want nil, nil and no write", written, err)
	}
}

// TestSetWaitHoldOnAClosedRow: a wait begun or ended on a closed row writes
// nothing and reports no error, as main's blind write succeeded there.
func TestSetWaitHoldOnAClosedRow(t *testing.T) {
	mem := beads.NewMemStore()
	b := wakeRow(t, mem)
	if err := mem.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	before := mustGet(t, mem, b.ID).Metadata
	front := NewStore(beads.SessionStore{Store: mem})
	for _, on := range []bool{true, false} {
		if err := front.SetWaitHold(b.ID, on, ""); err != nil {
			t.Errorf("SetWaitHold(on=%v) on a closed row = %v, want nil", on, err)
		}
	}
	if after := mustGet(t, mem, b.ID).Metadata; after["wait_hold"] != before["wait_hold"] || after["sleep_intent"] != before["sleep_intent"] {
		t.Errorf("closed row wait_hold %q -> %q, intent %q -> %q; want no write", before["wait_hold"], after["wait_hold"], before["sleep_intent"], after["sleep_intent"])
	}
}

// TestCloseClearsTheHolds: a closed row keeps no hold, intent or stamp: the
// close's clear is the Retire cell.
func TestCloseClearsTheHolds(t *testing.T) {
	store := beads.NewMemStore()
	mgr := newTestManager(t, store, runtime.NewFake())
	b := wakeRow(t, store)
	if err := store.SetMetadataBatch(b.ID, map[string]string{"sleep_intent": "user-hold", "held_until": dispT0.Format(time.RFC3339), "wait_hold": "true", "suspended_at": dispT0.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.CloseDetailed(context.Background(), testActor(mgr, ActorOperator), b.ID); err != nil {
		t.Fatalf("CloseDetailed: %v", err)
	}
	m := mustGet(t, store, b.ID).Metadata
	for _, k := range dispositionKeys {
		if m[k] != "" {
			t.Errorf("%s = %q after the close, want cleared", k, m[k])
		}
	}
}
