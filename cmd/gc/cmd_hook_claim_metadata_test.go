package main

import "testing"

// TestDecodeHookClaimBeadsToleratesNonStringMetadata pins the fix for the
// rig-wide claim outage (gcw-d95): the external `bd` CLI type-infers
// `--set-metadata key=true` as a JSON boolean (and numbers as JSON numbers),
// so a single bead carrying such a value used to poison the whole work_query
// decode with "cannot unmarshal bool into Go struct field Bead.metadata of
// type string" — failing decodeHookClaimBeads for the entire batch and
// blocking every worker in the rig from claiming any work.
//
// The decoder must tolerate non-string metadata values by coercing them to
// their string form, exactly as beads.StringMap already does on the bd list
// path.
func TestDecodeHookClaimBeadsToleratesNonStringMetadata(t *testing.T) {
	output := `[{"id":"gcw-1","metadata":{"refinery_reviewed":true,"count":42,"note":"ok"}}]`

	got, err := decodeHookClaimBeads(output)
	if err != nil {
		t.Fatalf("decodeHookClaimBeads returned error on non-string metadata: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("decoded %d beads, want 1", len(got))
	}

	meta := got[0].Metadata
	for _, tc := range []struct{ key, want string }{
		{"refinery_reviewed", "true"},
		{"count", "42"},
		{"note", "ok"},
	} {
		if meta[tc.key] != tc.want {
			t.Errorf("metadata[%q] = %q, want %q", tc.key, meta[tc.key], tc.want)
		}
	}
}

// TestDecodeHookClaimBeadsOneBadBeadDoesNotPoisonBatch proves the batch-level
// invariant that was the actual outage: a boolean-metadata bead sitting next to
// ordinary beads must not drop the good beads from the claim candidate set.
func TestDecodeHookClaimBeadsOneBadBeadDoesNotPoisonBatch(t *testing.T) {
	output := `[{"id":"a","metadata":{"note":"fine"}},{"id":"b","metadata":{"gc.parked":true}},{"id":"c"}]`

	got, err := decodeHookClaimBeads(output)
	if err != nil {
		t.Fatalf("decodeHookClaimBeads returned error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("decoded %d beads, want 3 (one bool-metadata bead must not drop the batch)", len(got))
	}
	if got[1].Metadata["gc.parked"] != "true" {
		t.Errorf("metadata[gc.parked] = %q, want %q", got[1].Metadata["gc.parked"], "true")
	}
}
