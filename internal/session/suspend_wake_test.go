package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
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
			mgr := NewManagerWithOptions(store, sp)
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
			held := got.Metadata["held_until"] == wantHeld && got.Metadata["sleep_intent"] == string(SleepReasonUserHold)
			if held != tc.held || got.Metadata["state"] != string(StateSuspended) {
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
	err := NewStore(beads.SessionStore{Store: store}).OperatorSuspend(b.ID, time.Now(), nil)
	var illegal *IllegalTransitionError
	if !errors.As(err, &illegal) {
		t.Fatalf("OperatorSuspend on a closed row = %v, want an illegal transition", err)
	}
	if got, _ := store.Get(b.ID); got.Metadata["held_until"] != "" || got.Metadata["sleep_intent"] != "" {
		t.Fatalf("closed row was written: held_until=%q sleep_intent=%q", got.Metadata["held_until"], got.Metadata["sleep_intent"])
	}
}

// TestCompleteDrainPatchKeepsTheUserHoldIntent: a drain an operator's
// suspend began completes without clearing sleep_intent=user-hold, so its
// held_until never reads as a heartbeat hold; any other intent is cleared.
func TestCompleteDrainPatchKeepsTheUserHoldIntent(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for intent, wantKept := range map[string]bool{
		"user-hold":         true,
		"idle-stop-pending": false,
		"wait-hold":         false,
		"":                  false,
	} {
		for _, reason := range []string{"user-hold", "idle"} {
			patch := CompleteDrainPatch(Info{SleepIntent: intent}, now, reason)
			v, written := patch["sleep_intent"]
			if kept := !written; kept != wantKept || (written && v != "") {
				t.Errorf("intent %q, reason %q: sleep_intent written=%v value=%q, want kept=%v", intent, reason, written, v, wantKept)
			}
		}
	}
}
