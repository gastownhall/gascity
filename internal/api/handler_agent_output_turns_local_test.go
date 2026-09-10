package api

import (
	"encoding/json"
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
