package nudgequeue

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// nudgeShadowBeadFromItem builds a nudge shadow bead the way
// cmd/gc/nudge_beads.go ensureQueuedNudgeBead does, so the decoder test asserts
// a true round-trip against the real write codec's output shape.
func nudgeShadowBeadFromItem(item Item, state, terminalReason, commitBoundary string) beads.Bead {
	refJSON := ""
	if item.Reference != nil {
		data, _ := json.Marshal(item.Reference)
		refJSON = string(data)
	}
	return beads.Bead{
		ID:    "nb-1",
		Title: "nudge:" + item.ID,
		Type:  nudgeBeadType,
		Labels: []string{
			nudgeBeadLabel,
			"agent:" + item.Agent,
			"nudge:" + item.ID,
			"source:" + item.Source,
		},
		Metadata: map[string]string{
			"nudge_id":        item.ID,
			"agent":           item.Agent,
			"session_id":      item.SessionID,
			"state":           state,
			"source":          item.Source,
			"message":         item.Message,
			"deliver_after":   item.DeliverAfter.UTC().Format(time.RFC3339),
			"expires_at":      item.ExpiresAt.UTC().Format(time.RFC3339),
			"reference_json":  refJSON,
			"terminal_reason": terminalReason,
			"commit_boundary": commitBoundary,
		},
	}
}

// TestDecodeNudgeItemRoundTrip proves the net-new decoder reads back every
// controller-stamped field — including the previously write-only reference_json
// — that the write codec stamps.
func TestDecodeNudgeItemRoundTrip(t *testing.T) {
	deliver := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	item := Item{
		ID:           "nudge-abc",
		Agent:        "polecat-7",
		SessionID:    "s-1",
		Source:       "controller",
		Message:      "wake up",
		Reference:    &Reference{Kind: "bead", ID: "gc-42"},
		DeliverAfter: deliver,
		ExpiresAt:    expires,
	}
	b := nudgeShadowBeadFromItem(item, "injected", "delivered", "post-commit")

	got := decodeNudgeItem(b)

	if got.ID != "nudge-abc" || got.BeadID != "nb-1" {
		t.Errorf("id/beadid = (%q,%q), want (nudge-abc, nb-1)", got.ID, got.BeadID)
	}
	if got.State != "injected" || got.TerminalReason != "delivered" || got.CommitBoundary != "post-commit" {
		t.Errorf("terminal fields = (%q,%q,%q), want (injected, delivered, post-commit)",
			got.State, got.TerminalReason, got.CommitBoundary)
	}
	if got.Reference == nil || got.Reference.Kind != "bead" || got.Reference.ID != "gc-42" {
		t.Errorf("reference = %+v, want {bead gc-42} (reference_json must finally be read back)", got.Reference)
	}
	if !got.DeliverAfter.Equal(deliver) || !got.ExpiresAt.Equal(expires) {
		t.Errorf("times = (%v,%v), want (%v,%v)", got.DeliverAfter, got.ExpiresAt, deliver, expires)
	}
	if got.Agent != "polecat-7" || got.SessionID != "s-1" || got.Source != "controller" || got.Message != "wake up" {
		t.Errorf("identity fields mismatch: %+v", got)
	}
}

// TestDecodeNudgeItemNoReference proves a missing reference_json yields a nil
// Reference (not a zero-value struct), matching the write codec which stores ""
// for a nil reference.
func TestDecodeNudgeItemNoReference(t *testing.T) {
	item := Item{ID: "n", Agent: "a", Source: "s"}
	b := nudgeShadowBeadFromItem(item, "queued", "", "")
	got := decodeNudgeItem(b)
	if got.Reference != nil {
		t.Errorf("Reference = %+v, want nil when reference_json is empty", got.Reference)
	}
	if got.State != "queued" {
		t.Errorf("state = %q, want queued", got.State)
	}
}

// TestNewStoreHoldsTypedStore proves the wrapper holds the typed nudges store
// (skeleton wiring sanity).
func TestNewStoreHoldsTypedStore(t *testing.T) {
	mem := beads.NewMemStore()
	st := NewStore(beads.NudgesStore{Store: mem})
	if st.store.Store != mem {
		t.Errorf("NewStore did not retain the embedded store")
	}
}
