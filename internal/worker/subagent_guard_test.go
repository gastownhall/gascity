package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

type fakeSubagentGuardTranscript struct {
	mappings []AgentMapping
	raw      []json.RawMessage
	err      error
}

func (t fakeSubagentGuardTranscript) AgentMappings(context.Context) ([]AgentMapping, error) {
	return t.mappings, t.err
}

func (t fakeSubagentGuardTranscript) TranscriptRecords(context.Context) ([]json.RawMessage, error) {
	if t.err != nil {
		return nil, t.err
	}
	return t.raw, nil
}

func TestInFlightBackgroundSubagents(t *testing.T) {
	// Captured Claude parent-transcript shape: Agent is the exact subagent tool name.
	spawn := json.RawMessage(`{"type":"assistant","uuid":"captured-parent-entry","timestamp":"2026-08-02T09:00:40.121Z","message":{"content":[{"type":"tool_use","id":"toolu_live","name":"Agent","input":{"description":"Investigate make check slowness","prompt":"Inspect the slow check"}}]}}`)
	terminated := json.RawMessage(`{"type":"queue-operation","operation":"enqueue","content":"<task-notification><task-id>done</task-id><status>completed</status></task-notification>"}`)
	for _, tt := range []struct {
		name     string
		mappings []AgentMapping
		raw      []json.RawMessage
		want     int
	}{
		{"live omitted background default", []AgentMapping{{AgentID: "live", ParentToolUseID: "toolu_live"}}, []json.RawMessage{spawn}, 1},
		{"terminated", []AgentMapping{{AgentID: "done", ParentToolUseID: "toolu_live"}}, []json.RawMessage{spawn, terminated}, 0},
		{"explicit synchronous", []AgentMapping{{AgentID: "live", ParentToolUseID: "toolu_live"}}, []json.RawMessage{json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_live","name":"Agent","input":{"run_in_background":false}}]}}`)}, 0},
		{"attachment terminal", []AgentMapping{{AgentID: "done", ParentToolUseID: "toolu_live"}}, []json.RawMessage{spawn, json.RawMessage(`{"type":"attachment","attachment":{"prompt":"<task-notification><task-id>done</task-id><status>completed</status></task-notification>"}}`)}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InFlightBackgroundSubagents(context.Background(), fakeSubagentGuardTranscript{mappings: tt.mappings, raw: tt.raw})
			if err != nil || len(got) != tt.want {
				t.Fatalf("InFlightBackgroundSubagents() = %#v, %v; want %d live", got, err, tt.want)
			}
			if tt.want == 1 && (got[0].AgentID != "live" || got[0].Description != "Investigate make check slowness" || got[0].StartedAt.IsZero()) {
				t.Fatalf("live = %#v", got[0])
			}
		})
	}
}

func TestInFlightBackgroundSubagentsReturnsTranscriptErrors(t *testing.T) {
	_, err := InFlightBackgroundSubagents(context.Background(), fakeSubagentGuardTranscript{err: errors.New("corrupt")})
	if err == nil {
		t.Fatal("error = nil, want corrupt transcript error")
	}
	_ = time.Second
}

func TestInFlightBackgroundSubagents_Incident20260802Golden(t *testing.T) {
	mappings := loadSubagentGuardMappings(t)
	for _, tt := range []struct {
		name, fixture, wantID, wantDescription string
	}{
		{"live at original kill timestamp", "testdata/subagent_guard_incident_20260802.jsonl", "a98bd7d5aacd4763a", "Investigate make check slowness"},
		{"terminated subagent excluded", "testdata/subagent_guard_terminated_20260802.jsonl", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Drive the real SessionLogAdapter from a file on disk: injecting
			// RawMessages skips the read path these fixtures exist to pin.
			path := fixtureOnDisk(t, tt.fixture)
			live, err := InFlightBackgroundSubagents(context.Background(), adapterBackedTranscript{mappings: mappings, path: path})
			if err != nil {
				t.Fatalf("InFlightBackgroundSubagents: %v", err)
			}
			if tt.wantID == "" {
				if len(live) != 0 {
					t.Fatalf("live = %#v, want none", live)
				}
				return
			}
			if len(live) != 1 || live[0].AgentID != tt.wantID || live[0].Description != tt.wantDescription {
				t.Fatalf("live = %#v, want %s %q", live, tt.wantID, tt.wantDescription)
			}
		})
	}
}

func loadSubagentGuardMappings(t *testing.T) []AgentMapping {
	t.Helper()
	data, err := os.ReadFile("testdata/subagent_guard_incident_20260802_mappings.json")
	if err != nil {
		t.Fatal(err)
	}
	var mappings []AgentMapping
	if err := json.Unmarshal(data, &mappings); err != nil {
		t.Fatal(err)
	}
	return mappings
}
