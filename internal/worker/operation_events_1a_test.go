package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestOperationEventCarriesAgentNameFromMetadata verifies the 1a addition
// (issue #1252): when a session has canonical agent_name metadata,
// WorkerOperation events surface it so dashboards can group by agent
// identity even when the optional alias is empty.
func TestOperationEventCarriesAgentNameFromMetadata(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, _ := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "polecat",
		Title:    "Polecat",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
		Metadata: map[string]string{
			"agent_name": "myrig/polecat-1",
		},
	}, recorder)

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var payload operationEventPayload
	if err := json.Unmarshal(lastRecordedWorkerOperation(t, recorder).Payload, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if payload.AgentName != "myrig/polecat-1" {
		t.Errorf("AgentName = %q, want %q", payload.AgentName, "myrig/polecat-1")
	}
}

func TestOperationEventCarriesAgentNameFromAliasFallback(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, _ := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "polecat",
		Alias:    "myrig/polecat-alias",
		Title:    "Polecat",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}, recorder)

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var payload operationEventPayload
	if err := json.Unmarshal(lastRecordedWorkerOperation(t, recorder).Payload, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if payload.AgentName != "myrig/polecat-alias" {
		t.Errorf("AgentName = %q, want alias fallback", payload.AgentName)
	}
}

// TestOperationEventOmitsAgentNameWhenAliasUnset confirms zero-value
// fields are omitted from the JSON payload (omitempty tags), keeping
// events compact for operations that lack the data.
func TestOperationEventOmitsAgentNameWhenAliasUnset(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, _ := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}, recorder)

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	raw := string(lastRecordedWorkerOperation(t, recorder).Payload)
	if strings.Contains(raw, `"agent_name"`) {
		t.Errorf("payload should omit agent_name when alias unset; got %s", raw)
	}
}

// TestOperationEventNew1aFieldsAreOmitEmpty asserts the on-the-wire shape:
// every 1a addition that is zero must be omitted from the JSON payload.
// Guards against accidental empty-string emissions that bloat events.
func TestOperationEventNew1aFieldsAreOmitEmpty(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, _ := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}, recorder)
	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	raw := string(lastRecordedWorkerOperation(t, recorder).Payload)
	for _, field := range []string{
		"model", "prompt_version", "prompt_sha", "bead_id",
		"prompt_tokens", "completion_tokens", "cache_read_tokens",
		"cache_creation_tokens", "latency_ms", "cost_usd_estimate",
	} {
		if strings.Contains(raw, `"`+field+`"`) {
			t.Errorf("payload contains %q without source data; expected omitempty: %s", field, raw)
		}
	}
}

// TestOperationEventPayloadShapeIsStable round-trips a fully populated
// payload through JSON to make sure the new fields serialize and parse
// using the same names. Decouples the test from any handle-side
// population logic — it's a pure shape check.
func TestOperationEventPayloadShapeIsStable(t *testing.T) {
	original := operationEventPayload{
		Operation:           "message",
		Result:              operationResultSucceeded,
		Model:               "claude-opus-4-7",
		AgentName:           "rig/polecat-1",
		PromptVersion:       "v3",
		PromptSHA:           "abc123",
		BeadID:              "rig-42",
		PromptTokens:        100,
		CompletionTokens:    50,
		CacheReadTokens:     2000,
		CacheCreationTokens: 800,
		LatencyMs:           1234,
		CostUSDEstimate:     0.0123,
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var roundtripped operationEventPayload
	if err := json.Unmarshal(raw, &roundtripped); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if roundtripped.Model != original.Model {
		t.Errorf("Model lost: got %q want %q", roundtripped.Model, original.Model)
	}
	if roundtripped.PromptVersion != "v3" {
		t.Errorf("PromptVersion lost: got %q", roundtripped.PromptVersion)
	}
	if roundtripped.PromptSHA != "abc123" {
		t.Errorf("PromptSHA lost: got %q", roundtripped.PromptSHA)
	}
	if roundtripped.BeadID != "rig-42" {
		t.Errorf("BeadID lost: got %q", roundtripped.BeadID)
	}
	if roundtripped.PromptTokens != 100 {
		t.Errorf("PromptTokens lost: got %d", roundtripped.PromptTokens)
	}
	if roundtripped.CompletionTokens != 50 {
		t.Errorf("CompletionTokens lost: got %d", roundtripped.CompletionTokens)
	}
	if roundtripped.CacheReadTokens != 2000 {
		t.Errorf("CacheReadTokens lost: got %d", roundtripped.CacheReadTokens)
	}
	if roundtripped.CacheCreationTokens != 800 {
		t.Errorf("CacheCreationTokens lost: got %d", roundtripped.CacheCreationTokens)
	}
	if roundtripped.LatencyMs != 1234 {
		t.Errorf("LatencyMs lost: got %d", roundtripped.LatencyMs)
	}
	if roundtripped.CostUSDEstimate != 0.0123 {
		t.Errorf("CostUSDEstimate lost: got %v", roundtripped.CostUSDEstimate)
	}
}

// TestOperationEventCarriesLaunchProvenanceFromMetadata pins that the
// launch_model / prompt_version / prompt_sha stamps CommitStartedPatch writes
// into session metadata surface on EVERY worker.operation payload the session
// records — start, nudge and message alike — because finish re-runs
// populateOperationEventIdentity.
func TestOperationEventCarriesLaunchProvenanceFromMetadata(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, _ := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "polecat",
		Title:    "Polecat",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
		Metadata: map[string]string{
			sessionpkg.LaunchModelMetadataKey:   "claude-sonnet-5",
			sessionpkg.PromptVersionMetadataKey: "v3",
			sessionpkg.PromptSHAMetadataKey:     "abc",
		},
	}, recorder)

	actor := handleActor(t, handle, sessionpkg.ActorAgent)
	if err := handle.Start(context.Background(), actor); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Nudge(context.Background(), actor, NudgeRequest{Text: "go"}); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if _, err := handle.Message(context.Background(), actor, MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	var ops []string
	for _, event := range recorder.events {
		if event.Type != events.WorkerOperation {
			continue
		}
		var payload operationEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		ops = append(ops, payload.Operation)
		t.Logf("worker.operation %s payload: %s", payload.Operation, event.Payload)
		if payload.Model != "claude-sonnet-5" {
			t.Errorf("%s: Model = %q, want claude-sonnet-5", payload.Operation, payload.Model)
		}
		if payload.PromptVersion != "v3" {
			t.Errorf("%s: PromptVersion = %q, want v3", payload.Operation, payload.PromptVersion)
		}
		if payload.PromptSHA != "abc" {
			t.Errorf("%s: PromptSHA = %q, want abc", payload.Operation, payload.PromptSHA)
		}
	}
	for _, want := range []string{"nudge", "message"} {
		found := false
		for _, op := range ops {
			if op == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no worker.operation %q recorded; got %v", want, ops)
		}
	}
	if len(ops) < 3 {
		t.Fatalf("expected start, nudge and message operations, got %v", ops)
	}
}

// TestOperationEventOmitsLaunchProvenanceWhenAbsent confirms a session whose
// start stamped no provenance (no model option, no prompt template) leaves
// the three fields off the wire rather than emitting empty strings.
func TestOperationEventOmitsLaunchProvenanceWhenAbsent(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, _ := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}, recorder)

	actor := handleActor(t, handle, sessionpkg.ActorAgent)
	if err := handle.Start(context.Background(), actor); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Nudge(context.Background(), actor, NudgeRequest{Text: "go"}); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	for _, event := range recorder.events {
		if event.Type != events.WorkerOperation {
			continue
		}
		raw := string(event.Payload)
		for _, field := range []string{"model", "prompt_version", "prompt_sha"} {
			if strings.Contains(raw, `"`+field+`"`) {
				t.Errorf("payload carries %q without a session stamp; expected omitempty: %s", field, raw)
			}
		}
	}
}
