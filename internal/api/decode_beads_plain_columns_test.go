package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// TestBeadFromGenCarriesPlainColumns pins bd's plain columns through the
// generated-client mapping.
//
// This is the last of the hand-written Bead mappings, and the one furthest
// from the store: it backs client.ListBeads/GetBead and the `gc bead show`
// rendering. Regenerating the OpenAPI spec puts the fields on genclient.Bead
// but does NOT copy them into beads.Bead — every field here is copied by an
// explicit nil-checked statement, so an omitted one is dropped silently after
// the store, the wire and the generated type all carried it correctly.
func TestBeadFromGenCarriesPlainColumns(t *testing.T) {
	awaitType, awaitID, createdBy := "gh:pr", "4912", "seeder"
	owner, notes := "owner@example.com", "waiting on a PR"
	got := beadFromGen(genclient.Bead{
		Id:        "gen-1",
		AwaitType: &awaitType,
		AwaitId:   &awaitID,
		CreatedBy: &createdBy,
		Owner:     &owner,
		Notes:     &notes,
	})
	for _, tc := range []struct{ field, got, want string }{
		{"AwaitType", got.AwaitType, awaitType},
		{"AwaitID", got.AwaitID, awaitID},
		{"CreatedBy", got.CreatedBy, createdBy},
		{"Owner", got.Owner, owner},
		{"Notes", got.Notes, notes},
	} {
		if tc.got != tc.want {
			t.Errorf("beadFromGen dropped %s: got %q, want %q", tc.field, tc.got, tc.want)
		}
	}
}

// TestBeadFromGenToleratesAbsentPlainColumns pins the nil case: these fields
// are omitempty on the wire, so a bead that carries none of them decodes to
// nil pointers and must not panic or fabricate values.
func TestBeadFromGenToleratesAbsentPlainColumns(t *testing.T) {
	got := beadFromGen(genclient.Bead{Id: "gen-2"})
	if got.AwaitType != "" || got.AwaitID != "" || got.CreatedBy != "" || got.Owner != "" || got.Notes != "" {
		t.Fatalf("absent plain columns produced %+v, want all empty", got)
	}
}
