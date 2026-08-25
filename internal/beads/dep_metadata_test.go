package beads

import "testing"

// TestDepMetadataCarries pins the rule that separates a real edge payload from
// an engine's rendering of an absent one.
//
// It runs without Dolt on purpose. The integration test proves the live engine
// hands back "{}" for an edge added with no metadata; this one proves the rule
// that reading covers, and keeps it covered on a machine where the Dolt rows
// are skipped.
func TestDepMetadataCarries(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
		want     bool
	}{
		{"empty", "", false},
		{"blank", "   ", false},
		{"the engine's rendering of no payload", "{}", false},
		{"an empty object with whitespace", " {\n} ", false},
		{"an empty array", "[]", false},
		{"a JSON null", "null", false},
		{"a real payload", `{"gate":"waits_for"}`, true},
		{"a payload holding only a false value", `{"strict":false}`, true},
		{"a non-empty array", `["a"]`, true},
		{"a bare JSON scalar", `"note"`, true},
		{"a bare zero", "0", true},
		{"something that is not JSON at all", "not json", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DepMetadataCarries(tc.metadata); got != tc.want {
				t.Fatalf("DepMetadataCarries(%q) = %v, want %v", tc.metadata, got, tc.want)
			}
		})
	}
}

// TestMemStoreSatisfiesDepMetadataReader pins that MemStore ANSWERS the payload
// question rather than being unable to. A caller that refuses on uncertainty —
// the infra-class migration does — would otherwise refuse every test city, and
// the refusal would be untestable through the source stub every migration test
// uses.
func TestMemStoreSatisfiesDepMetadataReader(t *testing.T) {
	var store Store = NewMemStore()
	reader, ok := store.(DepMetadataReader)
	if !ok {
		t.Fatal("MemStore no longer implements DepMetadataReader")
	}
	a, err := reader.(*MemStore).Create(Bead{Title: "a"})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := reader.(*MemStore).Create(Bead{Title: "b"})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	if err := reader.(*MemStore).DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	payload, carried, err := reader.DepMetadata(a.ID, b.ID)
	if err != nil {
		t.Fatalf("DepMetadata: %v", err)
	}
	if carried || payload != "" {
		t.Fatalf("DepMetadata = (%q, %v), want (\"\", false); MemStore has no way to store an edge payload", payload, carried)
	}
}
