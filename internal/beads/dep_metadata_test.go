package beads

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestSQLiteStoreCarriesAnEdgePayload pins the write half of the payload
// contract: an edge added with a payload reads back carrying it, and an edge
// added without one reads back absent rather than present-and-empty.
//
// The second half is the one worth a test. setGraphEdgeMetadataTx CLEARS the
// pair's sidecar before deciding it has nothing to store, so a writer that
// passed through an engine's rendering of "no payload" — Dolt's "{}" — would
// store that literally, and the destination would read back ("{}", true) where
// the source read ("", false). Absent and present-but-empty are exactly the two
// the adoption witness insists must stay distinguishable, and a copy that
// blurred them would be a copy that changed the graph.
func TestSQLiteStoreCarriesAnEdgePayload(t *testing.T) {
	store := newSQLiteGraphApplyStore(t, t.TempDir())
	var asStore Store = store
	writer, ok := asStore.(DepMetadataWriter)
	if !ok {
		t.Fatal("SQLiteStore does not implement DepMetadataWriter, so the infra-class copy has no way to carry an edge payload")
	}

	seed := func(title string) Bead {
		b, err := store.Create(Bead{Title: title})
		if err != nil {
			t.Fatalf("seeding %s: %v", title, err)
		}
		return b
	}
	gated, gate, plain := seed("gated"), seed("gate"), seed("plain")

	const payload = `{"gate":"waits_for","threshold":3}`
	if err := writer.DepAddWithMetadata(gated.ID, gate.ID, "blocks", payload); err != nil {
		t.Fatalf("DepAddWithMetadata: %v", err)
	}
	got, carried, err := store.DepMetadata(gated.ID, gate.ID)
	if err != nil {
		t.Fatalf("DepMetadata: %v", err)
	}
	if !carried || got != payload {
		t.Errorf("DepMetadata = (%q, %v), want (%q, true)", got, carried, payload)
	}

	if err := writer.DepAddWithMetadata(gated.ID, plain.ID, "blocks", ""); err != nil {
		t.Fatalf("DepAddWithMetadata with no payload: %v", err)
	}
	got, carried, err = store.DepMetadata(gated.ID, plain.ID)
	if err != nil {
		t.Fatalf("DepMetadata on the payloadless edge: %v", err)
	}
	if carried || got != "" {
		t.Errorf("an edge added with no payload reads back (%q, %v), want (\"\", false)", got, carried)
	}

	// The edge itself must exist either way — a payload writer that only wrote
	// sidecars would pass both assertions above and copy no graph at all.
	deps, err := store.DepList(gated.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("DepAddWithMetadata wrote %d edges, want 2: %+v", len(deps), deps)
	}
}

// TestSQLiteStoreReadsANonCarryingPayloadAsAbsent pins the read half of the
// parity NativeDoltStore.DepMetadata's doc claims "to the letter": a sidecar
// holding a payload that renders none — "{}"/"[]"/"null" — reads back
// ("", false), the same answer the native reader gives.
//
// setGraphEdgeMetadataTx declines only the empty STRING, so these three land in
// kv verbatim; without the reader gate SQLite would answer ("{}", true) where
// native answers ("{}", false), and the cross-engine migration witness — which
// hashes (payload, carried) — would read a byte-identical edge as payload lost.
// The gap this closes was invisible to the suite because no test asserted the
// carried bit for a persisted non-carrying-but-nonempty payload.
func TestSQLiteStoreReadsANonCarryingPayloadAsAbsent(t *testing.T) {
	store := newSQLiteGraphApplyStore(t, t.TempDir())
	var asStore Store = store
	writer, ok := asStore.(DepMetadataWriter)
	if !ok {
		t.Fatal("SQLiteStore does not implement DepMetadataWriter, so the infra-class copy has no way to carry an edge payload")
	}

	seed := func(title string) Bead {
		b, err := store.Create(Bead{Title: title})
		if err != nil {
			t.Fatalf("seeding %s: %v", title, err)
		}
		return b
	}

	for _, payload := range []string{"{}", "[]", "null", " {\n} "} {
		t.Run(payload, func(t *testing.T) {
			if DepMetadataCarries(payload) {
				t.Fatalf("test payload %q carries; it must be a non-carrying-but-nonempty value", payload)
			}
			gated, gate := seed("gated"), seed("gate")
			if err := writer.DepAddWithMetadata(gated.ID, gate.ID, "blocks", payload); err != nil {
				t.Fatalf("DepAddWithMetadata(%q): %v", payload, err)
			}
			got, carried, err := store.DepMetadata(gated.ID, gate.ID)
			if err != nil {
				t.Fatalf("DepMetadata: %v", err)
			}
			if carried || got != "" {
				t.Errorf("an edge whose sidecar holds the non-carrying payload %q reads back (%q, %v), want (\"\", false) to match NativeDoltStore", payload, got, carried)
			}
			// The edge itself must still exist — the gate hides the payload, not the dependency.
			deps, err := store.DepList(gated.ID, "down")
			if err != nil {
				t.Fatalf("DepList: %v", err)
			}
			if len(deps) != 1 {
				t.Fatalf("wrote %d edges, want 1: %+v", len(deps), deps)
			}
		})
	}
}

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

// TestFileStoreSatisfiesDepMetadataReader closes the gap between what the
// infra-class migration's refusal claims about the deployed stores and what the
// tree actually guarantees.
//
// The refusal treats a store it cannot ask as UNABLE TO ANSWER and blocks the
// city. Its doc names the file store among the stores that answer, and that is
// true only because FileStore embeds *MemStore and the method is promoted —
// nothing declared it and nothing checked it. A FileStore-backed work store
// that lost the promotion would be refused for a capability it still has, and
// every existing test would pass. The compile-time assertion in filestore.go is
// the guard; this is the behavioral half, because promotion satisfying an
// interface is not the same as the promoted method giving the right answer for
// the outer store.
func TestFileStoreSatisfiesDepMetadataReader(t *testing.T) {
	opened, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	var store Store = opened
	reader, ok := store.(DepMetadataReader)
	if !ok {
		t.Fatal("FileStore no longer implements DepMetadataReader, so the infra-class migration would refuse a file-backed work store for a capability it still has")
	}

	a, err := opened.Create(Bead{Title: "a"})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := opened.Create(Bead{Title: "b"})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	if err := opened.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	payload, carried, err := reader.DepMetadata(a.ID, b.ID)
	if err != nil {
		t.Fatalf("DepMetadata: %v", err)
	}
	if carried || payload != "" {
		t.Fatalf("DepMetadata = (%q, %v), want (\"\", false); FileStore persists MemStore's bead logic and has no way to store an edge payload", payload, carried)
	}
}
