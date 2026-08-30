package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// wakeRefusedEvents returns the captured session.wake_refused events, in
// emission order -- the wake-refusal sibling of strandedEvents/unknownStateEvents.
func (c *capturingRecorder) wakeRefusedEvents() []events.Event {
	out := make([]events.Event, 0, len(c.events))
	for _, e := range c.events {
		if e.Type == events.SessionWakeRefused {
			out = append(out, e)
		}
	}
	return out
}

// newWakeRefusalSession seeds a session bead carrying a durable explicit wake
// request (wake_request=explicit) -- the shape a pre-start wake refusal sees,
// per gastownhall/gascity#5739 part 2 and ga-fxvdit. Mirrors
// newUnknownStateSession's verbatim-seed construction.
func newWakeRefusalSession(t *testing.T, name string) (beads.Store, string) {
	t.Helper()
	const id = "gc-wake-refused-1"
	_, mem := sessiontest.Store(t, beads.Bead{
		ID:     id,
		Title:  name,
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": name,
			"state":        "asleep",
			"wake_request": string(sessionpkg.WakeCauseExplicit),
		},
	})
	return mem, id
}

// wakeRefusalInfo re-projects the session bead's typed Info through the front
// door, modeling how the reconciler re-reads each session from the store on
// every tick -- mirrors unknownStateInfo.
func wakeRefusalInfo(t *testing.T, store beads.Store, id string) sessionpkg.Info {
	t.Helper()
	info, err := sessionFrontDoor(store).Get(id)
	if err != nil {
		t.Fatalf("front-door Get(%s): %v", id, err)
	}
	return info
}

// TestEmitSessionWakeRefused_HeldSessionNotQuarantinedAtThreshold is the
// regression guard on the HAZARD documented in ga-fxvdit, written first per
// the bead's own instruction: wake_attempts is threshold-bearing
// (defaultMaxWakeAttempts) and recordWakeFailure's normal path routes
// increments through WakeFailureAccrualPatch, which quarantines the session
// once the threshold is crossed. A pre-start refusal bump must NOT go
// through that path -- a merely-held session must never be self-quarantined
// just because its explicit wake keeps being refused.
//
// The session here is seeded already AT defaultMaxWakeAttempts, as if that
// many refusals/failures had accrued from unrelated history. One more
// refusal pushes the counter past the threshold; if the bump routed through
// the accrual path (the bug this guards against) that single call would
// quarantine the session. It must not.
func TestEmitSessionWakeRefused_HeldSessionNotQuarantinedAtThreshold(t *testing.T) {
	store, id := newWakeRefusalSession(t, "worker-held")
	if err := sessionFrontDoor(store).SetMarker(id, "wake_attempts", strconv.Itoa(defaultMaxWakeAttempts)); err != nil {
		t.Fatalf("seeding wake_attempts at threshold: %v", err)
	}
	info := wakeRefusalInfo(t, store, id)
	if info.WakeAttemptsMetadata != strconv.Itoa(defaultMaxWakeAttempts) {
		t.Fatalf("precondition: wake_attempts = %q, want %d", info.WakeAttemptsMetadata, defaultMaxWakeAttempts)
	}
	if info.QuarantinedUntil != "" {
		t.Fatalf("precondition: QuarantinedUntil = %q, want empty", info.QuarantinedUntil)
	}

	clk := &clock.Fake{Time: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	rec := &capturingRecorder{}
	var stderr bytes.Buffer

	emitSessionWakeRefused(store, info, nil, "gascity/worker", "held", rec, clk, &stderr)

	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	final := wakeRefusalInfo(t, store, id)
	wantAttempts := strconv.Itoa(defaultMaxWakeAttempts + 1)
	if final.WakeAttemptsMetadata != wantAttempts {
		t.Fatalf("wake_attempts after one more refusal = %q, want %q", final.WakeAttemptsMetadata, wantAttempts)
	}
	if final.QuarantinedUntil != "" {
		t.Fatalf("QuarantinedUntil = %q after crossing defaultMaxWakeAttempts=%d via a pre-start refusal, want empty: "+
			"the bump must be a direct marker write, never routed through WakeFailureAccrualPatch",
			final.QuarantinedUntil, defaultMaxWakeAttempts)
	}
}

// TestEmitSessionWakeRefused_SingleRefusalEmitsAndBumpsOnce is bead
// ga-fxvdit's Tests item 1: a refused explicit wake emits exactly one
// session.wake_refused with the correct reason, and leaves wake_attempts=1.
func TestEmitSessionWakeRefused_SingleRefusalEmitsAndBumpsOnce(t *testing.T) {
	store, id := newWakeRefusalSession(t, "worker-single")
	info := wakeRefusalInfo(t, store, id)

	clk := &clock.Fake{Time: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	rec := &capturingRecorder{}
	var stderr bytes.Buffer
	const template = "gascity/worker"

	emitSessionWakeRefused(store, info, nil, template, "held", rec, clk, &stderr)

	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	got := rec.wakeRefusedEvents()
	if len(got) != 1 {
		t.Fatalf("wakeRefusedEvents = %d, want 1", len(got))
	}
	evt := got[0]
	if evt.Subject != id {
		t.Errorf("Subject = %q, want %q", evt.Subject, id)
	}
	if evt.SessionID != id {
		t.Errorf("SessionID = %q, want %q", evt.SessionID, id)
	}
	if evt.Actor != "gc" {
		t.Errorf("Actor = %q, want %q", evt.Actor, "gc")
	}
	if !strings.Contains(evt.Message, "held") {
		t.Errorf("Message = %q, want it to mention the refusal reason %q", evt.Message, "held")
	}

	var payload api.SessionWakeRefusedPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.SessionID != id {
		t.Errorf("payload.SessionID = %q, want %q", payload.SessionID, id)
	}
	if payload.Template != template {
		t.Errorf("payload.Template = %q, want %q", payload.Template, template)
	}
	if payload.Reason != "held" {
		t.Errorf("payload.Reason = %q, want %q", payload.Reason, "held")
	}
	if payload.WakeRequest != string(sessionpkg.WakeCauseExplicit) {
		t.Errorf("payload.WakeRequest = %q, want %q", payload.WakeRequest, string(sessionpkg.WakeCauseExplicit))
	}
	if payload.Attempts != 1 {
		t.Errorf("payload.Attempts = %d, want 1", payload.Attempts)
	}

	final := wakeRefusalInfo(t, store, id)
	if final.WakeAttemptsMetadata != "1" {
		t.Fatalf("wake_attempts = %q, want %q", final.WakeAttemptsMetadata, "1")
	}
}

// TestEmitSessionWakeRefused_TenConsecutiveTicksGuardHolds is bead
// ga-fxvdit's Tests item 2: ten consecutive refused ticks on the SAME
// unserved wake request emit ONE event and leave wake_attempts=1 -- the
// once-per-wake-request throttle guard (wake_refused_event_at) must hold
// across repeated reconciler ticks, mirroring
// emitSessionStrandedDiagnostic's StrandedEventEmittedAt guard.
func TestEmitSessionWakeRefused_TenConsecutiveTicksGuardHolds(t *testing.T) {
	store, id := newWakeRefusalSession(t, "worker-ticks")
	clk := &clock.Fake{Time: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	rec := &capturingRecorder{}
	var stderr bytes.Buffer

	for i := 0; i < 10; i++ {
		info := wakeRefusalInfo(t, store, id)
		emitSessionWakeRefused(store, info, nil, "gascity/worker", "held", rec, clk, &stderr)
		clk.Time = clk.Time.Add(time.Minute)
	}

	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if got := rec.wakeRefusedEvents(); len(got) != 1 {
		t.Fatalf("wakeRefusedEvents after 10 ticks = %d, want 1 (the guard must hold)", len(got))
	}
	final := wakeRefusalInfo(t, store, id)
	if final.WakeAttemptsMetadata != "1" {
		t.Fatalf("wake_attempts after 10 refused ticks = %q, want %q", final.WakeAttemptsMetadata, "1")
	}
}

// TestEmitSessionWakeRefused_FreshWakeClearsGuardAndReemits is bead
// ga-fxvdit's Tests item 4: a fresh explicit wake after a refusal clears
// wake_refused_event_at and wake_attempts (ClearWakeBlockersPatch, alongside
// its existing wake_attempts=0 reset), so a second refusal emits again.
func TestEmitSessionWakeRefused_FreshWakeClearsGuardAndReemits(t *testing.T) {
	store, id := newWakeRefusalSession(t, "worker-fresh")
	clk := &clock.Fake{Time: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	rec := &capturingRecorder{}
	var stderr bytes.Buffer

	info := wakeRefusalInfo(t, store, id)
	emitSessionWakeRefused(store, info, nil, "gascity/worker", "held", rec, clk, &stderr)
	if got := rec.wakeRefusedEvents(); len(got) != 1 {
		t.Fatalf("wakeRefusedEvents after first refusal = %d, want 1", len(got))
	}

	patch := sessionpkg.ClearWakeBlockersPatch(sessionpkg.StateAsleep, "", clk.Now())
	if v, ok := patch["wake_refused_event_at"]; !ok || v != "" {
		t.Errorf(`ClearWakeBlockersPatch()["wake_refused_event_at"] = (%q, ok=%v), want ("", ok=true): `+
			"a fresh explicit wake must clear the throttle guard alongside wake_attempts so a subsequent refusal can emit again", v, ok)
	}

	info, err := sessionFrontDoor(store).ApplyPatchInfo(wakeRefusalInfo(t, store, id), patch)
	if err != nil {
		t.Fatalf("ApplyPatchInfo(ClearWakeBlockersPatch): %v", err)
	}

	clk.Time = clk.Time.Add(time.Hour)
	emitSessionWakeRefused(store, info, nil, "gascity/worker", "held", rec, clk, &stderr)

	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if got := rec.wakeRefusedEvents(); len(got) != 2 {
		t.Fatalf("wakeRefusedEvents after fresh-wake + second refusal = %d, want 2", len(got))
	}
	final := wakeRefusalInfo(t, store, id)
	if final.WakeAttemptsMetadata != "1" {
		t.Fatalf("wake_attempts after fresh wake + one refusal = %q, want %q", final.WakeAttemptsMetadata, "1")
	}
}
