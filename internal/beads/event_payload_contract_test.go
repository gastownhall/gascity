package beads

import (
	"encoding/json"
	"testing"
	"time"
)

// TestNotifyChangePayloadDecodesViaSharedDecoder is the producer↔consumer
// contract test the run-view RCA asked for: the exact bytes
// CachingStore.notifyChange emits for a bead.* event must decode back to the
// same bead through the shared canonical decoder. This pins the raw-bead wire
// shape so a future "fix" that switches the producer to the wrapped
// {"bead":...} form (or tightens a consumer's tolerance) fails here instead of
// silently starving the run-view projection.
func TestNotifyChangePayloadDecodesViaSharedDecoder(t *testing.T) {
	prio := 2
	seed := Bead{
		ID:        "gcg-run-1",
		Title:     "adopt PR #42",
		Status:    "in_progress",
		Type:      "molecule",
		Priority:  &prio,
		CreatedAt: time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC),
		Assignee:  "worker-a",
		ParentID:  "gcg-root",
		Ref:       "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.kind":             "run",
			"gc.step_id":          "gcg-step-1",
		},
	}

	var got json.RawMessage
	cs := NewCachingStore(NewMemStore(), func(_, _, _, _, _ string, payload json.RawMessage) {
		got = payload
	})
	cs.notifyChange("bead.created", seed)

	if len(got) == 0 {
		t.Fatal("notifyChange emitted an empty payload")
	}

	decoded, ok := DecodeBeadEventPayload(got)
	if !ok {
		t.Fatalf("shared decoder could not decode notifyChange bytes: %s", got)
	}
	if decoded.ID != seed.ID {
		t.Errorf("id = %q, want %q", decoded.ID, seed.ID)
	}
	if decoded.Title != seed.Title || decoded.Status != seed.Status || decoded.Type != seed.Type {
		t.Errorf("decoded = %+v, want title/status/type of %+v", decoded, seed)
	}
	if decoded.Priority == nil || *decoded.Priority != prio {
		t.Errorf("priority = %v, want %d", decoded.Priority, prio)
	}
	if !decoded.CreatedAt.Equal(seed.CreatedAt) {
		t.Errorf("created_at = %v, want %v", decoded.CreatedAt, seed.CreatedAt)
	}
	for k, want := range seed.Metadata {
		if decoded.Metadata[k] != want {
			t.Errorf("metadata[%q] = %q, want %q", k, decoded.Metadata[k], want)
		}
	}
}
