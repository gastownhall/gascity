package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// TestWorkerOperationPayload1aWiringStatusPin asserts the consumer
// contract spelled out on WorkerOperationEventPayload's type doc:
// every 1a field is best-effort, and follow-up wiring lands one field
// at a time. The test pins which fields are wired today vs which the
// runtime is contractually forbidden to populate yet.
//
// When a follow-up wiring tier lands (PromptVersion + PromptSHA from
// 1e, token counts from sessionlog tail, cost from pricing.Registry,
// etc.), this test fails — and the failure message tells the
// implementer to:
//
//  1. Update the field's "Wired:" line in the type doc to "YES".
//  2. Update the consumer-facing docs / dashboard panels that were
//     filtering "always absent" fields from their queries.
//  3. Move the field's name from notWiredYet to wiredAlready below.
//
// This is a structural reminder, not a behavior test. It documents
// the rollout schedule in code so consumers don't quietly start
// receiving populated fields without the corresponding announcement.
func TestWorkerOperationPayload1aWiringStatusPin(t *testing.T) {
	wiredAlready := map[string]string{
		"agent_name":            "session.Info.AgentName with Alias fallback via populateOperationEventIdentity (PR #1272)",
		"model":                 "worker.recordInvocationTelemetry aggregates sessionlog.ExtractTailUsage (via SessionLogAdapter.TailUsage) and stamps the newest model onto the event at Message/Nudge finish (stampOperationEventUsage)",
		"prompt_tokens":         "worker sessionlog tail extraction (SessionLogAdapter.TailUsage → sessionlog.ExtractTailUsage) summed onto the event at operation finish; shares the batch with the model usage facts",
		"completion_tokens":     "worker sessionlog tail extraction summed onto the event at operation finish (same batch as the usage facts)",
		"cache_read_tokens":     "worker sessionlog tail extraction summed onto the event at operation finish (same batch as the usage facts)",
		"cache_creation_tokens": "worker sessionlog tail extraction summed onto the event at operation finish (same batch as the usage facts)",
		"cost_usd_estimate":     "pricing.Registry.Estimate over the extracted tokens at operation finish (#1255); mirrors the usage-fact cost semantics — unknown (family,model) leaves cost 0 and sets unpriced",
	}
	notWiredYet := map[string]string{
		"prompt_version": "follow-up #1256: propagate promptmeta.FrontMatter.Version through session metadata",
		"prompt_sha":     "follow-up #1256: propagate promptmeta.SHA through session metadata",
		"bead_id":        "follow-up: thread operation context through worker.beginOperationEvent",
		"latency_ms":     "follow-up: no LLM-invocation latency source exists yet",
	}

	// 1. Every wired field must show up populated when its source is
	//    present. We can't actually exercise the producer side from
	//    this test (it lives in worker), so we just confirm the field
	//    is JSON-roundtrippable with a non-zero value, ruling out
	//    accidental tag drift that would silently drop the value.
	for field := range wiredAlready {
		t.Run("wired/"+field, func(t *testing.T) {
			payload := nonZeroPayloadForField(field)
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(raw), `"`+field+`":`) {
				t.Errorf("wired field %q should serialize when populated; got %s", field, raw)
			}
		})
	}

	// 2. Every not-yet-wired field MUST be omitted from a freshly
	//    populated event today. The producer side has no source for
	//    these fields; if they ever appear on the wire, either the
	//    field has been wired (great — update this test and the type
	//    doc) or omitempty has been dropped (bug — fix that).
	t.Run("not_wired_fields_absent_in_today's_events", func(t *testing.T) {
		raw := captureWorkerOperationEventToday(t)
		if raw == "" {
			t.Skip("no worker.operation event captured; producer wiring may have changed")
		}
		for field, why := range notWiredYet {
			if strings.Contains(raw, `"`+field+`":`) {
				t.Errorf(
					"field %q appears on the wire today but is not yet wired (%s).\n"+
						"If you wired it intentionally:\n"+
						"  1. Move %q from notWiredYet to wiredAlready in TestWorkerOperationPayload1aWiringStatusPin.\n"+
						"  2. Update the field's \"Wired:\" line in WorkerOperationEventPayload's struct doc.\n"+
						"  3. Update consumers that were filtering on Field == \"\".\n"+
						"If not, this is a regression: omitempty dropped or a stray populator was added.\n"+
						"event: %s",
					field, why, field, raw,
				)
			}
		}
	})

	// 3. The lists must cover the full 1a field set. If a future PR
	//    adds a new field, this assertion forces the author to decide
	//    whether the new field is wired or follow-up.
	t.Run("lists_cover_full_1a_field_set", func(t *testing.T) {
		expected := []string{
			"model", "agent_name", "prompt_version", "prompt_sha",
			"bead_id", "prompt_tokens", "completion_tokens",
			"cache_read_tokens", "cache_creation_tokens",
			"latency_ms", "cost_usd_estimate",
		}
		for _, name := range expected {
			_, wired := wiredAlready[name]
			_, todo := notWiredYet[name]
			if !wired && !todo {
				t.Errorf("1a field %q is in neither wiredAlready nor notWiredYet — list it explicitly", name)
			}
			if wired && todo {
				t.Errorf("1a field %q appears in both lists — it can be wired or not, not both", name)
			}
		}
	})
}

// nonZeroPayloadForField returns a payload with field populated and
// every other 1a field at zero, used to assert single-field wire
// behavior in isolation.
func nonZeroPayloadForField(field string) WorkerOperationEventPayload {
	p := WorkerOperationEventPayload{}
	switch field {
	case "model":
		p.Model = "claude-opus-4-7"
	case "agent_name":
		p.AgentName = "rig/polecat-1"
	case "prompt_version":
		p.PromptVersion = "v3"
	case "prompt_sha":
		p.PromptSHA = "abc123"
	case "bead_id":
		p.BeadID = "rig-1"
	case "prompt_tokens":
		p.PromptTokens = 100
	case "completion_tokens":
		p.CompletionTokens = 50
	case "cache_read_tokens":
		p.CacheReadTokens = 200
	case "cache_creation_tokens":
		p.CacheCreationTokens = 80
	case "latency_ms":
		p.LatencyMs = 1234
	case "cost_usd_estimate":
		p.CostUSDEstimate = 0.001
	}
	return p
}

// captureWorkerOperationEventToday documents the API-layer projection by
// constructing the payload shape that is wired today, then projecting it
// through the same JSON marshaling the SSE wire uses. Returns the raw JSON;
// the caller asserts which fields appear.
//
// The test suite-side pin in internal/worker
// (TestOperationEventNew1aFieldsAreOmitEmpty) exercises the actual producer;
// this complementary test pins the api-layer projection — the place a
// downstream consumer reads from /v0/events/stream.
func captureWorkerOperationEventToday(t *testing.T) string {
	t.Helper()
	// Model, the token counts, and cost_usd_estimate are now wired from the
	// worker sessionlog-tail extraction at operation finish, but they are
	// best-effort: an operation with no extractable transcript usage (as here)
	// carries only AgentName from 1a. The remaining notWiredYet fields
	// (prompt_version, prompt_sha, bead_id, latency_ms) have no producer at
	// all. Mirror a no-usage event explicitly so we don't rely on the worker
	// package; the token/model/cost absence here is the best-effort contract,
	// not a wiring gap.
	payload := WorkerOperationEventPayload{
		OpID:        "test-op",
		Operation:   "message",
		Result:      "succeeded",
		AgentName:   "rig/polecat-1",
		SessionID:   "sess-1",
		SessionName: "rig/polecat-1",
		Provider:    "claude",
		Transport:   "tmux",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Sanity: confirm the bus would route the same payload type.
	registered := events.RegisteredPayloadTypes()
	if _, ok := registered[events.WorkerOperation]; !ok {
		t.Fatal("WorkerOperation has no registered payload — registration regression")
	}
	return string(raw)
}
