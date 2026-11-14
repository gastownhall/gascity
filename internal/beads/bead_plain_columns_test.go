package beads

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// Bead is converted to and from wire form by five hand-written mappings, and a
// field added to the struct does NOT flow through any of them automatically:
//
//	bdIssue.toBead        (internal/beads/bdstore.go)
//	beadFromNativeIssue   (internal/beads/native_dolt_store.go)
//	beadWire.toBead       (internal/beads/exec)          — consumer
//	bridgeBead            (cmd/gc/cmd_bd_store_bridge.go) — its producer
//	beadFromGen           (internal/api/decode_convoys.go)
//
// Each is asserted below or in its own package. Without a per-mapping
// assertion a mapping that silently drops a field looks exactly like one that
// carries it: the struct compiles, the spec regenerates, and every suite stays
// green while the field never reaches a caller.

// TestBdIssueToBeadCarriesPlainColumns pins bd's plain columns through the
// bd-backed store's JSON envelope. This is the mapping that sees real `bd
// --json` output, so a field bd emits and bdIssue does not declare is dropped
// at the outermost edge, before any other layer could carry it.
func TestBdIssueToBeadCarriesPlainColumns(t *testing.T) {
	const raw = `{
		"id": "bd-1",
		"title": "Gate: gh:pr 4912",
		"status": "open",
		"issue_type": "gate",
		"await_type": "gh:pr",
		"await_id": "4912",
		"created_by": "seeder",
		"owner": "owner@example.com",
		"notes": "waiting on a PR"
	}`
	var issue bdIssue
	if err := json.Unmarshal([]byte(raw), &issue); err != nil {
		t.Fatalf("unmarshal bd issue: %v", err)
	}
	got := issue.toBead()
	assertPlainColumns(t, "bdIssue.toBead", got, "gh:pr", "4912", "seeder", "owner@example.com", "waiting on a PR")
}

// TestBeadFromNativeIssueCarriesPlainColumns pins the native-Dolt mapping. It
// reads beadslib.Issue directly rather than JSON, so it drops fields BEFORE the
// wire: carrying them on Bead and in the OpenAPI spec has no effect on this
// backend unless the literal below names them.
func TestBeadFromNativeIssueCarriesPlainColumns(t *testing.T) {
	got, err := beadFromNativeIssue(&beadslib.Issue{
		ID:        "native-1",
		Title:     "Gate: timer",
		AwaitType: "timer",
		AwaitID:   "2026-08-01T12:00:00Z",
		CreatedBy: "seeder",
		Owner:     "owner@example.com",
		Notes:     "waiting on a timer",
	})
	if err != nil {
		t.Fatalf("beadFromNativeIssue: %v", err)
	}
	assertPlainColumns(t, "beadFromNativeIssue", got, "timer", "2026-08-01T12:00:00Z", "seeder", "owner@example.com", "waiting on a timer")
}

// TestBeadPlainColumnsSurviveJSONRoundTrip pins the struct's own wire form.
// A field with no json tag, or one whose tag does not match the key bd emits,
// is invisible on every wire path at once — the HTTP responses, the SSE bead
// events and the generated clients all derive from these tags.
func TestBeadPlainColumnsSurviveJSONRoundTrip(t *testing.T) {
	in := Bead{
		ID: "gc-1", Title: "t", Status: "open", Type: "gate",
		AwaitType: "gh:pr", AwaitID: "4912", CreatedBy: "seeder", Owner: "owner@example.com", Notes: "n",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Assert the wire KEYS, not just the round trip: a tag typo would round
	// trip perfectly through this type and still not match bd's output.
	var keyed map[string]any
	if err := json.Unmarshal(raw, &keyed); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, key := range []string{"await_type", "await_id", "created_by", "owner", "notes"} {
		if _, ok := keyed[key]; !ok {
			t.Errorf("marshaled Bead has no %q key; bd emits it under that name", key)
		}
	}
	var out Bead
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertPlainColumns(t, "Bead JSON round trip", out, "gh:pr", "4912", "seeder", "owner@example.com", "n")
}

// TestBeadExportedFieldsAreWireTagged catches a field added to Bead without a
// json tag. Revision and ClaimFence are deliberately json:"-" (store-internal,
// kept off every wire path); an untagged field is different — it reaches the
// wire under its Go name, which no store or client agrees on.
func TestBeadExportedFieldsAreWireTagged(t *testing.T) {
	rt := reflect.TypeOf(Bead{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		tag, ok := f.Tag.Lookup("json")
		if !ok {
			t.Errorf("Bead.%s has no json tag; it would reach the wire under its Go name", f.Name)
			continue
		}
		if strings.HasPrefix(tag, ",") {
			t.Errorf("Bead.%s json tag %q names no key", f.Name, tag)
		}
	}
}

func assertPlainColumns(t *testing.T, mapping string, got Bead, awaitType, awaitID, createdBy, owner, notes string) {
	t.Helper()
	for _, tc := range []struct{ field, got, want string }{
		{"AwaitType", got.AwaitType, awaitType},
		{"AwaitID", got.AwaitID, awaitID},
		{"CreatedBy", got.CreatedBy, createdBy},
		{"Owner", got.Owner, owner},
		{"Notes", got.Notes, notes},
	} {
		if tc.got != tc.want {
			t.Errorf("%s dropped %s: got %q, want %q", mapping, tc.field, tc.got, tc.want)
		}
	}
}
