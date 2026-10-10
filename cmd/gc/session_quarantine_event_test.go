package main

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
)

// quarantinedEvents returns the session.quarantined events rec captured.
func quarantinedEvents(rec *events.Fake) []events.Event {
	var out []events.Event
	for _, e := range rec.Events {
		if e.Type == events.SessionQuarantined {
			out = append(out, e)
		}
	}
	return out
}

// failWakes drives n consecutive wake failures through recordWakeFailure,
// re-reading the session from the store between failures the way the
// reconciler re-projects it each tick.
func failWakes(t *testing.T, store *testStore, session *beads.Bead, rec events.Recorder, clk clock.Clock, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		recordWakeFailure(seedSessionInfo(*session), sessionFrontDoor(store), clk, rec, "gascity/gc.worker")
		syncBeadFromStore(session, store)
	}
}

func TestRecordWakeFailureEmitsQuarantinedOnEntry(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	rec := events.NewFake()
	session := makeBead("b1", map[string]string{
		"agent_name":   "gascity/gc.worker",
		"session_name": "gascity--gc__worker",
	})

	failWakes(t, store, &session, rec, clk, defaultMaxWakeAttempts)

	if session.Metadata["quarantined_until"] == "" {
		t.Fatal("fixture must quarantine at max attempts")
	}
	got := quarantinedEvents(rec)
	if len(got) != 1 {
		t.Fatalf("session.quarantined events = %d, want 1: %+v", len(got), rec.Events)
	}
	if got[0].Subject != "gascity/gc.worker" {
		t.Errorf("Subject = %q, want %q", got[0].Subject, "gascity/gc.worker")
	}
	if got[0].Actor != "gc" {
		t.Errorf("Actor = %q, want gc", got[0].Actor)
	}
	if got[0].SessionID != "b1" {
		t.Errorf("SessionID = %q, want b1", got[0].SessionID)
	}

	// A further failure while the quarantine is still active is not a new
	// entry and must not announce one.
	failWakes(t, store, &session, rec, clk, 1)
	if got := quarantinedEvents(rec); len(got) != 1 {
		t.Fatalf("session.quarantined events after a failure inside quarantine = %d, want 1", len(got))
	}
}

func TestRecordWakeFailureBelowThresholdEmitsNothing(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	rec := events.NewFake()
	session := makeBead("b1", map[string]string{"agent_name": "gascity/gc.worker"})

	failWakes(t, store, &session, rec, clk, defaultMaxWakeAttempts-1)

	if session.Metadata["quarantined_until"] != "" {
		t.Fatal("fixture must not quarantine below threshold")
	}
	if got := quarantinedEvents(rec); len(got) != 0 {
		t.Fatalf("session.quarantined events = %+v, want none below threshold", got)
	}
}

func TestRecordWakeFailureNoEventWhenPersistFails(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	store.metadataBatchErr = errors.New("injected persist failure")
	rec := events.NewFake()
	session := makeBead("b1", map[string]string{
		"agent_name":    "gascity/gc.worker",
		"wake_attempts": "4", // one below threshold
	})

	recordWakeFailure(seedSessionInfo(session), sessionFrontDoor(store), clk, rec, "gascity/gc.worker")
	syncBeadFromStore(&session, store)

	if session.Metadata["quarantined_until"] != "" {
		t.Fatal("fixture must fail to persist the quarantine")
	}
	if got := quarantinedEvents(rec); len(got) != 0 {
		t.Fatalf("session.quarantined events = %+v, want none when the quarantine did not persist", got)
	}
}

func TestParallelWakeFailureEmitsQuarantined(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	rec := events.NewFake()
	session := makeBead("b1", map[string]string{
		"agent_name":   "gascity/gc.worker",
		"session_name": "gascity--gc__worker",
		"template":     "gascity/gc.worker",
	})
	tp := TemplateParams{SessionName: "gascity--gc__worker", TemplateName: "gascity/gc.worker", Command: "true"}

	// One failure past the threshold: the extra failure lands while the
	// session is already quarantined and must not announce a second entry.
	for i := 0; i < defaultMaxWakeAttempts+1; i++ {
		result := startResult{
			prepared: preparedStart{candidate: startCandidate{info: seedSessionInfo(session), tp: tp}},
			err:      errors.New("start op failed"),
			outcome:  TraceOutcomeProviderError,
			started:  now,
			finished: now,
		}
		commitStartFailure(result, sessionFrontDoor(store), clk, rec, 0, io.Discard, nil)
		syncBeadFromStore(&session, store)
	}

	if session.Metadata["quarantined_until"] == "" {
		t.Fatal("fixture must quarantine at max attempts")
	}
	got := quarantinedEvents(rec)
	if len(got) != 1 {
		t.Fatalf("session.quarantined events = %d, want 1: %+v", len(got), rec.Events)
	}
	if got[0].Subject != tp.DisplayName() {
		t.Errorf("Subject = %q, want %q (the session.woke subject)", got[0].Subject, tp.DisplayName())
	}
	if got[0].SessionID != "b1" {
		t.Errorf("SessionID = %q, want b1", got[0].SessionID)
	}
}

// TestCheckStabilityRapidExitsEmitQuarantined drives the reconciler's
// rapid-exit path (checkStability -> recordWakeFailure) to the wake-attempt
// threshold and checks that the recorder it is handed sees the quarantine.
// Subject falls back to the template when the bead has no agent_name.
func TestCheckStabilityRapidExitsEmitQuarantined(t *testing.T) {
	for _, tc := range []struct {
		name        string
		meta        map[string]string
		wantSubject string
	}{
		{"agent name", map[string]string{"agent_name": "gascity/gc.worker", "template": "gascity/gc.worker-tmpl"}, "gascity/gc.worker"},
		{"template fallback", map[string]string{"template": "gascity/gc.worker-tmpl"}, "gascity/gc.worker-tmpl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
			clk := &clock.Fake{Time: now}
			store := newTestStore()
			dt := newDrainTracker()
			rec := events.NewFake()
			session := makeBead("b1", tc.meta)

			for i := 0; i < defaultMaxWakeAttempts; i++ {
				// checkStability clears last_woke_at on each rapid exit; every
				// iteration is a fresh wake that died inside the window.
				session.Metadata["last_woke_at"] = now.Add(-10 * time.Second).Format(time.RFC3339)
				if _, stab := checkStability(seedSessionInfo(session), nil, false, dt, sessionFrontDoor(store), clk, rec, nil); !stab {
					t.Fatalf("exit %d: rapid exit should report stability failure", i+1)
				}
				syncBeadFromStore(&session, store)
			}

			if session.Metadata["quarantined_until"] == "" {
				t.Fatal("fixture must quarantine at max attempts")
			}
			got := quarantinedEvents(rec)
			if len(got) != 1 {
				t.Fatalf("session.quarantined events = %d, want 1: %+v", len(got), rec.Events)
			}
			if got[0].Subject != tc.wantSubject {
				t.Errorf("Subject = %q, want %q", got[0].Subject, tc.wantSubject)
			}
			if got[0].SessionID != "b1" {
				t.Errorf("SessionID = %q, want b1", got[0].SessionID)
			}
		})
	}
}
