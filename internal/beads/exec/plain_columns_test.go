package exec //nolint:revive // internal package, always imported with alias

import (
	"encoding/json"
	"testing"
)

// TestBeadWireToBeadCarriesPlainColumns pins bd's plain columns through the
// exec provider's wire type.
//
// This is one of four hand-written Bead mappings, and adding a field to
// beads.Bead does not make it flow through any of them: beadWire declares its
// own field set, so a column it omits is dropped before toBead runs. An
// exec:<script> store that faithfully emits bd's JSON would still have had
// these discarded here.
func TestBeadWireToBeadCarriesPlainColumns(t *testing.T) {
	const raw = `{
		"id": "exec-1",
		"title": "Gate: gh:pr 4912",
		"status": "open",
		"type": "gate",
		"await_type": "gh:pr",
		"await_id": "4912",
		"created_by": "seeder",
		"owner": "owner@example.com",
		"notes": "waiting on a PR"
	}`
	var w beadWire
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("unmarshal bead wire: %v", err)
	}
	got := w.toBead()
	for _, tc := range []struct{ field, got, want string }{
		{"AwaitType", got.AwaitType, "gh:pr"},
		{"AwaitID", got.AwaitID, "4912"},
		{"CreatedBy", got.CreatedBy, "seeder"},
		{"Owner", got.Owner, "owner@example.com"},
		{"Notes", got.Notes, "waiting on a PR"},
	} {
		if tc.got != tc.want {
			t.Errorf("beadWire.toBead dropped %s: got %q, want %q", tc.field, tc.got, tc.want)
		}
	}
}
