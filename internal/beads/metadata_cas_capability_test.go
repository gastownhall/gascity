package beads

import "testing"

// TestMetadataCASCapableFor pins the real-capability-answer contract:
// MetadataCASCapableFor must NOT be satisfiable by a store that merely
// type-asserts to MetadataCASWriter (BdStore always does, regardless of
// whether the installed bd CLI can honor a fenced write) — it must consult
// the store's live capability prober when one exists.
func TestMetadataCASCapableFor(t *testing.T) {
	t.Parallel()

	t.Run("nil store is incapable", func(t *testing.T) {
		capable, reason := MetadataCASCapableFor(nil)
		if capable {
			t.Fatal("nil store reported capable")
		}
		if reason == "" {
			t.Fatal("expected a non-empty reason for a nil store")
		}
	})

	t.Run("a plain capable MemStore is capable", func(t *testing.T) {
		store := NewMemStore()
		capable, reason := MetadataCASCapableFor(store)
		if !capable {
			t.Fatalf("MemStore reported incapable: %s", reason)
		}
	})

	t.Run("a MemStore with conditional writes disabled is incapable", func(t *testing.T) {
		store := NewMemStore()
		store.DisableConditionalWrites = true
		capable, reason := MetadataCASCapableFor(store)
		if capable {
			t.Fatal("MemStore with DisableConditionalWrites reported capable")
		}
		if reason == "" {
			t.Fatal("expected a non-empty reason")
		}
	})

	t.Run("a BdStore that structurally satisfies MetadataCASWriter but fails the live probe is incapable", func(t *testing.T) {
		// This is the exact false-green trap the spec calls out:
		// MetadataCASWriterFor alone reports (writer, true) for every BdStore
		// regardless of whether bd actually supports --if-revision.
		incapableHelp := []byte("Usage:\n  bd update [flags]\n\nFlags:\n  --json   emit JSON\n")
		store := NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
			return incapableHelp, nil
		})

		if _, ok := MetadataCASWriterFor(store); !ok {
			t.Fatal("test invariant broken: BdStore must structurally satisfy MetadataCASWriter")
		}

		capable, reason := MetadataCASCapableFor(store)
		if capable {
			t.Fatal("incapable BdStore reported capable by MetadataCASCapableFor")
		}
		if reason == "" {
			t.Fatal("expected a non-empty reason")
		}
	})

	t.Run("a BdStore whose bd supports --if-revision on all four verbs is capable", func(t *testing.T) {
		capableHelp := func(verb string) []byte {
			return []byte("Usage:\n  bd " + verb + " [flags]\n\nFlags:\n  --if-revision int   apply only at this revision\n")
		}
		store := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			return capableHelp(args[0]), nil
		})

		capable, reason := MetadataCASCapableFor(store)
		if !capable {
			t.Fatalf("capable BdStore reported incapable: %s", reason)
		}
	})
}
