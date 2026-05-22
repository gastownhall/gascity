package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// decodeSessionLifecyclePayload pulls the typed payload off a recorded
// event, asserting it is non-empty and parses as the typed shape.
func decodeSessionLifecyclePayload(t *testing.T, e events.Event) api.SessionLifecyclePayload {
	t.Helper()
	if len(e.Payload) == 0 {
		t.Fatalf("event %q payload is empty; want SessionLifecyclePayload", e.Type)
	}
	var p api.SessionLifecyclePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v (raw=%s)", err, string(e.Payload))
	}
	return p
}

// findEvent returns the first recorded event matching eventType, or fails
// the test.
func findEvent(t *testing.T, rec *events.Fake, eventType string) events.Event {
	t.Helper()
	for _, e := range rec.Events {
		if e.Type == eventType {
			return e
		}
	}
	t.Fatalf("no event of type %q recorded; got %d events", eventType, len(rec.Events))
	return events.Event{}
}

// TestHandoffRemoteEmitsTypedSessionStoppedPayload verifies that
// doHandoffRemote attaches a SessionLifecyclePayload identifying the
// killed session and the "handoff" reason.
func TestHandoffRemoteEmitsTypedSessionStoppedPayload(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "deacon", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	// Pre-create a session bead so resolveSessionID returns its ID.
	sessionBead, err := store.Create(beads.Bead{
		Title:    "deacon session",
		Type:     "session",
		Assignee: "deacon",
		Metadata: map[string]string{"session_name": "deacon"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doHandoffRemote(store, rec, sp, "deacon", "deacon", "mayor",
		[]string{"Context refresh", "body"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	stopped := findEvent(t, rec, events.SessionStopped)
	payload := decodeSessionLifecyclePayload(t, stopped)
	if payload.SessionID != sessionBead.ID {
		t.Errorf("SessionID = %q, want %q", payload.SessionID, sessionBead.ID)
	}
	if payload.Reason != "handoff" {
		t.Errorf("Reason = %q, want %q", payload.Reason, "handoff")
	}
}

// TestStopTargetLifecycleCorrelationID verifies the fallback chain
// for the SessionLifecyclePayload.SessionID correlation key: a populated
// sessionID wins, an empty sessionID falls back to the stable session_name
// (which ResolveSessionID can canonicalize to a bead ID for consumers).
// Targets constructed without a store (e.g. legacy/manual invocations,
// or beads retired before stop) hit the fallback path; without it,
// the typed payload would emit `session_id:""` and violate the
// "always present" contract documented on SessionLifecyclePayload.
func TestStopTargetLifecycleCorrelationID(t *testing.T) {
	cases := []struct {
		name   string
		target stopTarget
		want   string
	}{
		{
			name:   "populated sessionID wins",
			target: stopTarget{sessionID: "sess-abc", name: "worker-1"},
			want:   "sess-abc",
		},
		{
			name:   "empty sessionID falls back to session_name",
			target: stopTarget{sessionID: "", name: "worker-1"},
			want:   "worker-1",
		},
		{
			name:   "whitespace sessionID is treated as populated",
			target: stopTarget{sessionID: " ", name: "worker-1"},
			want:   " ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.target.lifecycleCorrelationID(); got != tc.want {
				t.Errorf("lifecycleCorrelationID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStopTargetsBoundedEmitsTypedSessionStoppedPayload exercises the
// graceful-stop wave path used by `gc stop` to confirm each emitted
// SessionStopped event carries the correlated session ID and template.
func TestStopTargetsBoundedEmitsTypedSessionStoppedPayload(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	targets := []stopTarget{{
		sessionID: "sess-worker-1",
		name:      "worker-1",
		template:  "worker",
		subject:   "worker-1",
		resolved:  true,
	}}

	var stdout, stderr bytes.Buffer
	stopped := stopTargetsBounded(targets, nil, store, sp, rec, "gc", &stdout, &stderr)
	if stopped != 1 {
		t.Fatalf("stopped = %d, want 1", stopped)
	}

	e := findEvent(t, rec, events.SessionStopped)
	payload := decodeSessionLifecyclePayload(t, e)
	if payload.SessionID != "sess-worker-1" {
		t.Errorf("SessionID = %q, want sess-worker-1", payload.SessionID)
	}
	if payload.Template != "worker" {
		t.Errorf("Template = %q, want worker", payload.Template)
	}
	if !strings.Contains(stdout.String(), "Stopped agent 'worker-1'") {
		t.Errorf("stdout = %q, want 'Stopped agent worker-1'", stdout.String())
	}
}

// TestStopTargetsBoundedEmitsNonEmptySessionIDWhenBeadAbsent verifies
// the empty-sessionID fallback fires from the stopTargetsBounded wave
// path: when a target was constructed without a corresponding session
// bead (sessionID==""), the emitted SessionLifecyclePayload.SessionID
// falls back to the session_name rather than violating the "always
// present" contract.
func TestStopTargetsBoundedEmitsNonEmptySessionIDWhenBeadAbsent(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "orphan-worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	targets := []stopTarget{{
		sessionID: "", // simulate target built without store hydration
		name:      "orphan-worker",
		template:  "worker",
		subject:   "orphan-worker",
		resolved:  true,
	}}

	var stdout, stderr bytes.Buffer
	stopped := stopTargetsBounded(targets, nil, store, sp, rec, "gc", &stdout, &stderr)
	if stopped != 1 {
		t.Fatalf("stopped = %d, want 1", stopped)
	}

	e := findEvent(t, rec, events.SessionStopped)
	payload := decodeSessionLifecyclePayload(t, e)
	if payload.SessionID == "" {
		t.Fatalf("SessionID is empty; want fallback to session_name (orphan-worker). payload=%+v", payload)
	}
	if payload.SessionID != "orphan-worker" {
		t.Errorf("SessionID = %q, want fallback to session_name %q", payload.SessionID, "orphan-worker")
	}
}
