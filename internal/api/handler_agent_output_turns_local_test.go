package api

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/worker"
)

// A turn must carry the entry ID the before/after cursors match on. Without it
// a client can see has_older_messages but has nothing to send as `before`.
func TestEntryToTurnCarriesEntryID(t *testing.T) {
	e := &worker.TranscriptEntry{
		UUID:      "11111111-2222-3333-4444-555555555555",
		Type:      "assistant",
		Timestamp: time.Now(),
		Message:   json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"hello"}]}`),
	}
	turn := entryToTurn(e)
	if turn.ID != e.UUID {
		t.Fatalf("turn.ID = %q, want %q", turn.ID, e.UUID)
	}
}

// A history turn carries the provider entry ID, the same value entryToTurn
// carries. HistoryEntry.ID is not a cursor: for an entry without a provider
// UUID the normalizer mints "derived-<order>", which no transcript cursor
// matches, so such a turn omits id instead. The second return value stays on
// HistoryEntry.ID because the stream's sent-entry bookkeeping keys on it.
// TestNormalizeEntryCarriesModelStopReasonAndDerivedFlag pins the worker side:
// Provenance.RawEntryID is the provider UUID, and empty without one.
func TestHistorySnapshotTurnsCarryProviderEntryID(t *testing.T) {
	snapshot := &worker.HistorySnapshot{Entries: []worker.HistoryEntry{
		{
			ID:         "uuid-1",
			Kind:       "assistant",
			Text:       "from the provider",
			Provenance: worker.Provenance{RawEntryID: "uuid-1", RawType: "assistant"},
		},
		{
			ID:         "derived-1",
			Kind:       "assistant",
			Text:       "minted by gc",
			Provenance: worker.Provenance{RawType: "assistant", Derived: true},
		},
	}}

	turns, ids := historySnapshotTurns(snapshot)
	if len(turns) != 2 {
		t.Fatalf("turns = %+v, want 2", turns)
	}
	if turns[0].ID != "uuid-1" {
		t.Errorf("turns[0].ID = %q, want the provider entry ID uuid-1", turns[0].ID)
	}
	if turns[1].ID != "" {
		t.Errorf("turns[1].ID = %q, want empty for an entry without a provider ID", turns[1].ID)
	}
	raw, err := json.Marshal(turns[1])
	if err != nil {
		t.Fatalf("marshal turn: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal turn: %v", err)
	}
	if _, ok := fields["id"]; ok {
		t.Errorf("turn JSON = %s, want id omitted for an entry without a provider ID", raw)
	}
	if want := []string{"uuid-1", "derived-1"}; !slices.Equal(ids, want) {
		t.Errorf("ids = %q, want %q", ids, want)
	}
}
