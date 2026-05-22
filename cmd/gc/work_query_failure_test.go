package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

func TestClassifyWorkQueryKill(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantKilled bool
	}{
		{"nil", nil, false},
		{"signal killed", errors.New(`running work query "sh -c '...'": signal: killed`), true},
		{"signal terminated", errors.New("signal: terminated"), true},
		{"exit 137 sigkill", errors.New("exit status 137"), true},
		{"exit 143 sigterm", errors.New("exit status 143"), true},
		{"exit 130 sigint", errors.New("exit status 130"), true},
		{"runner timeout", errors.New(`running work query "x": timed out after 30s`), true},
		{"ordinary command error", errors.New("exit status 1"), false},
		{"non signal high exit", errors.New("exit status 200"), false},
		{"config error", errors.New("agent not found"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, killed := classifyWorkQueryKill(tc.err)
			if killed != tc.wantKilled {
				t.Fatalf("classifyWorkQueryKill killed=%v want %v (err=%v)", killed, tc.wantKilled, tc.err)
			}
			if killed && reason == "" {
				t.Fatalf("classifyWorkQueryKill returned empty reason for killed error %v", tc.err)
			}
			if !killed && reason != "" {
				t.Fatalf("classifyWorkQueryKill returned reason %q for non-kill error %v", reason, tc.err)
			}
		})
	}
}

func TestEmitWorkQueryFailureRecordsEventOnKill(t *testing.T) {
	rec := &memRecorder{}
	emitted := emitWorkQueryFailure(rec, "gm-abc123", "demo/agent", `sh -c 'bd ready'`, errors.New("signal: killed"))
	if !emitted {
		t.Fatal("expected emitWorkQueryFailure to record an event for a killed work query")
	}
	if !rec.hasType(events.SessionWorkQueryFailed) {
		t.Fatalf("expected a %s event, got %+v", events.SessionWorkQueryFailed, rec.events)
	}
	if !rec.hasSubject("demo/agent") {
		t.Fatalf("expected event subject to be the template, got %+v", rec.events)
	}
	if len(rec.events) != 1 {
		t.Fatalf("recorded events = %d, want 1", len(rec.events))
	}
	payload := decodeSessionLifecyclePayload(t, rec.events[0])
	if payload.SessionID != "gm-abc123" {
		t.Fatalf("payload SessionID = %q, want gm-abc123", payload.SessionID)
	}
	if payload.Template != "demo/agent" {
		t.Fatalf("payload Template = %q, want demo/agent", payload.Template)
	}
	if payload.Reason != "work query killed (signal: killed)" {
		t.Fatalf("payload Reason = %q, want work query killed (signal: killed)", payload.Reason)
	}
}

func TestEmitWorkQueryFailureFallsBackToSessionIDSubject(t *testing.T) {
	rec := &memRecorder{}
	if !emitWorkQueryFailure(rec, "gm-xyz", "", "cmd", errors.New("exit status 137")) {
		t.Fatal("expected an event for an exit-137 work query")
	}
	if !rec.hasSubject("gm-xyz") {
		t.Fatalf("expected subject to fall back to the session ID, got %+v", rec.events)
	}
}

func TestEmitWorkQueryFailureSkipsEmptySessionID(t *testing.T) {
	rec := &memRecorder{}
	if emitWorkQueryFailure(rec, "", "demo/agent", "cmd", errors.New("signal: killed")) {
		t.Fatal("expected killed work query without a session ID to be left on the stderr path")
	}
	if len(rec.events) != 0 {
		t.Fatalf("expected no events recorded, got %+v", rec.events)
	}
}

func TestEmitWorkQueryFailureIgnoresOrdinaryErrors(t *testing.T) {
	rec := &memRecorder{}
	if emitWorkQueryFailure(rec, "gm-abc", "t", "cmd", nil) {
		t.Fatal("nil error must not emit an event")
	}
	if emitWorkQueryFailure(rec, "gm-abc", "t", "cmd", errors.New("exit status 1")) {
		t.Fatal("ordinary command error must not emit an event")
	}
	if len(rec.events) != 0 {
		t.Fatalf("expected no events recorded, got %+v", rec.events)
	}
}

func TestEmitWorkQueryFailureNilRecorderSafe(t *testing.T) {
	if !emitWorkQueryFailure(nil, "gm-1", "t", "cmd", errors.New("signal: killed")) {
		t.Fatal("a nil recorder must still classify the kill and report it as emitted")
	}
}
