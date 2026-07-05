package beads

import (
	"encoding/json"
	"testing"
	"time"
)

// TestDecodeBeadEventPayloadRawCanonical proves the canonical raw-bead shape —
// what CachingStore.notifyChange actually marshals (json.Marshal(b)) and what
// every .gc/events.jsonl row holds — decodes with full field fidelity.
func TestDecodeBeadEventPayloadRawCanonical(t *testing.T) {
	prio := 3
	created := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	want := Bead{
		ID:          "gcg-1",
		Title:       "port the fold",
		Status:      "in_progress",
		Type:        "task",
		Priority:    &prio,
		CreatedAt:   created,
		Assignee:    "worker-a",
		From:        "sling",
		ParentID:    "gcg-root",
		Ref:         "step-a",
		Needs:       []string{"step-x"},
		Description: "instructions",
		Labels:      []string{"lane:a"},
		Metadata:    map[string]string{"gc.kind": "run", "gc.step_id": "gcg-step-1"},
		Dependencies: []Dep{
			{IssueID: "gcg-1", DependsOnID: "gcg-dep", Type: "blocks"},
		},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, ok := DecodeBeadEventPayload(raw)
	if !ok {
		t.Fatalf("raw canonical decode returned ok=false for %s", raw)
	}
	if got.ID != want.ID || got.Title != want.Title || got.Status != want.Status ||
		got.Type != want.Type || got.Assignee != want.Assignee || got.From != want.From ||
		got.ParentID != want.ParentID || got.Ref != want.Ref || got.Description != want.Description {
		t.Fatalf("scalar fields mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if got.Priority == nil || *got.Priority != prio {
		t.Errorf("priority = %v, want %d", got.Priority, prio)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, created)
	}
	if len(got.Needs) != 1 || got.Needs[0] != "step-x" {
		t.Errorf("needs = %v, want [step-x]", got.Needs)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "lane:a" {
		t.Errorf("labels = %v, want [lane:a]", got.Labels)
	}
	if got.Metadata["gc.kind"] != "run" || got.Metadata["gc.step_id"] != "gcg-step-1" {
		t.Errorf("metadata = %v, want gc.kind=run gc.step_id=gcg-step-1", got.Metadata)
	}
	if len(got.Dependencies) != 1 || got.Dependencies[0].DependsOnID != "gcg-dep" {
		t.Errorf("dependencies = %v, want one dep on gcg-dep", got.Dependencies)
	}
}

// TestDecodeBeadEventPayloadWrappedFallback proves the tolerant {"bead":<snap>}
// fallback still decodes (older/registered-contract shape), so a producer that
// switched to the wrapped shape would not silently starve consumers.
func TestDecodeBeadEventPayloadWrappedFallback(t *testing.T) {
	inner := Bead{ID: "gcg-2", Status: "open", Type: "bug"}
	wrapped, err := json.Marshal(struct {
		Bead Bead `json:"bead"`
	}{inner})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, ok := DecodeBeadEventPayload(wrapped)
	if !ok {
		t.Fatalf("wrapped fallback decode returned ok=false for %s", wrapped)
	}
	if got.ID != "gcg-2" || got.Type != "bug" || got.Status != "open" {
		t.Errorf("wrapped decode = %+v, want id=gcg-2 type=bug status=open", got)
	}
}

// TestDecodeBeadEventPayloadTypeCompat proves the bd-hook "type" field is honored
// as an issue_type compat fallback (exec-style payloads use "type", bd hooks use
// "issue_type"). A plain Bead unmarshal alone would drop it.
func TestDecodeBeadEventPayloadTypeCompat(t *testing.T) {
	got, ok := DecodeBeadEventPayload([]byte(`{"id":"gcg-3","type":"chore","status":"open"}`))
	if !ok {
		t.Fatal("type-compat decode returned ok=false")
	}
	if got.Type != "chore" {
		t.Errorf("Type = %q, want chore (from compat \"type\" field)", got.Type)
	}

	// issue_type wins when both are present.
	got2, ok := DecodeBeadEventPayload([]byte(`{"id":"gcg-4","issue_type":"feature","type":"chore"}`))
	if !ok {
		t.Fatal("issue_type-precedence decode returned ok=false")
	}
	if got2.Type != "feature" {
		t.Errorf("Type = %q, want feature (issue_type wins over type)", got2.Type)
	}
}

// TestDecodeBeadEventPayloadNonStringMetadata proves StringMap coercion survives
// the shared decoder (bd CLI type-inference emits bool/number metadata values).
func TestDecodeBeadEventPayloadNonStringMetadata(t *testing.T) {
	got, ok := DecodeBeadEventPayload([]byte(`{"id":"gcg-5","metadata":{"gc.iteration":3,"done":true}}`))
	if !ok {
		t.Fatal("non-string metadata decode returned ok=false")
	}
	if got.Metadata["gc.iteration"] != "3" || got.Metadata["done"] != "true" {
		t.Errorf("metadata = %v, want gc.iteration=3 done=true (coerced)", got.Metadata)
	}
}

// TestDecodeBeadEventPayloadMisses proves the decode-miss cases return
// (Bead{}, false): empty payload, empty id, and undecodable bytes.
func TestDecodeBeadEventPayloadMisses(t *testing.T) {
	cases := map[string]json.RawMessage{
		"empty":       nil,
		"blank id":    json.RawMessage(`{"id":"","status":"open"}`),
		"no id":       json.RawMessage(`{"status":"open"}`),
		"garbage":     json.RawMessage(`not json`),
		"wrapped-nil": json.RawMessage(`{"bead":null}`),
	}
	for name, payload := range cases {
		if b, ok := DecodeBeadEventPayload(payload); ok {
			t.Errorf("%s: ok=true bead=%+v, want (Bead{}, false)", name, b)
		}
	}
}
