package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/pricing"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// newUsageEventHandle builds a started session handle wired to an event
// recorder AND a search-path root, returning the handle, the resolved
// transcript path to write usage entries to, and the recorder that captures
// worker.operation events. It is the operation-event analog of
// newUsageFactHandle: the recorder lets a test read the emitted event's 1a
// token/cost fields, and the resolvable transcript path lets the shared
// invocation-usage extraction feed them.
func newUsageEventHandle(t *testing.T) (handle *SessionHandle, transcriptPath string, recorder *recordingEventRecorder) {
	t.Helper()
	searchBase := t.TempDir()
	workDir := t.TempDir()
	recorder = &recordingEventRecorder{}

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp)
	h, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchBase},
		Recorder:    recorder,
		Session: SessionSpec{
			Profile:  ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "claude",
			WorkDir:  workDir,
			Provider: "claude",
			Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := manager.Get(h.sessionID)
	if err != nil {
		t.Fatalf("Get(%q): %v", h.sessionID, err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	return h, filepath.Join(slugDir, info.SessionKey+".jsonl"), recorder
}

// lastMessageOperationEvent returns the payload of the most recent
// worker.operation event whose Operation is "message". The recorder also
// captures the Start op event, so the test cannot blindly take the newest.
func lastMessageOperationEvent(t *testing.T, recorder *recordingEventRecorder) operationEventPayload {
	t.Helper()
	for i := len(recorder.events) - 1; i >= 0; i-- {
		var payload operationEventPayload
		if err := json.Unmarshal(recorder.events[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Operation == string(workerOperationMessage) {
			return payload
		}
	}
	t.Fatalf("no worker.operation message event recorded; got %d events", len(recorder.events))
	return operationEventPayload{}
}

// TestOperationEventCarriesTokensAndCostFromTranscript is the 1a wiring
// regression: a completed transcript invocation must surface its tokens,
// model, and priced cost on the wrapping worker.operation event, agreeing
// with the usage fact emitted from the same extraction.
func TestOperationEventCarriesTokensAndCostFromTranscript(t *testing.T) {
	handle, transcriptPath, recorder := newUsageEventHandle(t)

	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "claude-opus-4-7", 100, 50, 2000, 800),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	payload := lastMessageOperationEvent(t, recorder)
	if payload.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want claude-opus-4-7", payload.Model)
	}
	if payload.PromptTokens != 100 || payload.CompletionTokens != 50 ||
		payload.CacheReadTokens != 2000 || payload.CacheCreationTokens != 800 {
		t.Errorf("tokens wrong: prompt=%d completion=%d cacheRead=%d cacheCreation=%d",
			payload.PromptTokens, payload.CompletionTokens,
			payload.CacheReadTokens, payload.CacheCreationTokens)
	}

	wantCost, ok := pricing.BuildRegistry(nil, nil).Estimate("claude", "claude-opus-4-7", pricing.Usage{
		PromptTokens:        100,
		CompletionTokens:    50,
		CacheReadTokens:     2000,
		CacheCreationTokens: 800,
	})
	if !ok {
		t.Fatal("default pricing registry has no claude-opus-4-7 entry; fix the test fixture")
	}
	if payload.CostUSDEstimate != wantCost {
		t.Errorf("CostUSDEstimate = %v, want %v", payload.CostUSDEstimate, wantCost)
	}
	if payload.Unpriced == nil {
		t.Fatal("Unpriced must be set (tokens observed); got nil")
	}
	if *payload.Unpriced {
		t.Errorf("a priced model must not be flagged Unpriced")
	}
}

// TestOperationEventUnpricedModelCarriesTokensZeroCost proves the tri-state
// honesty on the event surface: an unknown model yields Unpriced=true and a
// zero cost while the tokens still ride the wire — "not measured", never a
// free invocation.
func TestOperationEventUnpricedModelCarriesTokensZeroCost(t *testing.T) {
	handle, transcriptPath, recorder := newUsageEventHandle(t)

	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "totally-unknown-model-xyz", 100, 50, 0, 0),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	payload := lastMessageOperationEvent(t, recorder)
	if payload.Model != "totally-unknown-model-xyz" {
		t.Errorf("Model = %q, want the observed unknown model", payload.Model)
	}
	if payload.PromptTokens != 100 || payload.CompletionTokens != 50 {
		t.Errorf("tokens must still ride the wire for an unpriced model: prompt=%d completion=%d",
			payload.PromptTokens, payload.CompletionTokens)
	}
	if payload.Unpriced == nil || !*payload.Unpriced {
		t.Errorf("unknown model must be flagged Unpriced=true; got %v", payload.Unpriced)
	}
	if payload.CostUSDEstimate != 0 {
		t.Errorf("unpriced model must carry zero cost, got %v", payload.CostUSDEstimate)
	}
}

// TestOperationEventOmitsUsageWhenNoTranscript confirms the best-effort
// contract: with no transcript data to extract, the token/cost/model fields
// stay zero (omitted from the JSON) and the event still emits.
func TestOperationEventOmitsUsageWhenNoTranscript(t *testing.T) {
	handle, _, recorder := newUsageEventHandle(t)

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	// The event must still have been recorded.
	payload := lastMessageOperationEvent(t, recorder)
	if payload.Result != operationResultSucceeded {
		t.Fatalf("message op must still emit a succeeded event; got %q", payload.Result)
	}

	// And no usage fields on the wire.
	var raw string
	for i := len(recorder.events) - 1; i >= 0; i-- {
		var p operationEventPayload
		if err := json.Unmarshal(recorder.events[i].Payload, &p); err != nil {
			continue
		}
		if p.Operation == string(workerOperationMessage) {
			raw = string(recorder.events[i].Payload)
			break
		}
	}
	for _, field := range []string{
		"model", "prompt_tokens", "completion_tokens",
		"cache_read_tokens", "cache_creation_tokens",
		"cost_usd_estimate", "unpriced",
	} {
		if strings.Contains(raw, `"`+field+`"`) {
			t.Errorf("field %q must be omitted when no transcript usage was extracted: %s", field, raw)
		}
	}
}
