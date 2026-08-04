package beads

import (
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// TestNativeDoltStoreDeclaresNarrowCASButNotConditionalWriter pins the exact
// capability split the narrow interface exists to express: NativeDoltStore
// offers a sound metadata value-CAS and makes NO revision-fence claim.
//
// Declaring the full ConditionalWriter would make ResolveConditionalWriter
// RESOLVE under require mode and hand the revision-CAS trio's callers a
// silently wrong-fenced write, because no sound revision token exists at
// beads v1.1.0 (see internal/beads/metadata_cas.go). This asserts the ABSENCE
// of that capability, which no conformance suite can do.
func TestNativeDoltStoreDeclaresNarrowCASButNotConditionalWriter(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())

	if w, ok := ConditionalWriterFor(store); ok {
		t.Fatalf("NativeDoltStore resolved a ConditionalWriter (%T); the revision-CAS trio has "+
			"no sound backend fence at beads v1.1.0, so declaring it is a safety regression", w)
	}
	if _, ok := MetadataCASWriterFor(store); !ok {
		t.Fatal("NativeDoltStore does not resolve a MetadataCASWriter; the narrow value-CAS " +
			"capability is what unblocks target_scope member-declaration and the D3/D5 lease lane")
	}
}

// TestNativeDoltStoreConditionalWritesStillRefuseOrDegrade pins the seam
// behavior the condWritesStamp comment in native_dolt_store.go guarantees:
// require yields a typed refusal and auto yields a loud degrade — never a
// silent legacy write under require. Adding the narrow CAS must not move
// either verdict.
func TestNativeDoltStoreConditionalWritesStillRefuseOrDegrade(t *testing.T) {
	t.Run("require_refuses", func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		store.stampConditionalWritesMode(gate.Require, false)

		writer, diag, err := ResolveConditionalWriter(store)
		if writer != nil {
			t.Fatalf("writer = %T, want nil (require must fail closed)", writer)
		}
		if !IsConditionalWritesRequired(err) {
			t.Fatalf("err = %v, want *ConditionalWritesRequiredError", err)
		}
		if diag == nil {
			t.Fatal("diagnostic = nil, want a refusal diagnostic")
		}
	})

	t.Run("auto_degrades_loudly", func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		store.stampConditionalWritesMode(gate.Auto, false)

		writer, diag, err := ResolveConditionalWriter(store)
		if writer != nil {
			t.Fatalf("writer = %T, want nil (auto must take the legacy path)", writer)
		}
		if err != nil {
			t.Fatalf("err = %v, want nil (auto degrades, it does not refuse)", err)
		}
		if diag == nil {
			t.Fatal("diagnostic = nil, want a loud-degrade diagnostic")
		}
	})
}

// TestCachingStoreOverNativeDoltStoreForwardsNarrowCAS covers the wrapper
// shape the plan calls out: a CachingStore whose backing offers only the
// narrow capability must still forward the metadata CAS. The cache resolves
// its trio verbs through ConditionalWriterFor, so without a narrow fallback
// this path would answer ErrConditionalWriteUnsupported and the lease lane
// would be blocked behind the cache.
func TestCachingStoreOverNativeDoltStoreForwardsNarrowCAS(t *testing.T) {
	backing := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	cache := NewCachingStore(backing, nil)

	b, err := cache.Create(Bead{Title: "cache-over-native-cas"})
	if err != nil {
		t.Fatal(err)
	}

	writer, ok := MetadataCASWriterFor(cache)
	if !ok {
		t.Fatal("CachingStore over a narrow-CAS backing does not resolve a MetadataCASWriter")
	}
	if swapped, err := writer.CompareAndSetMetadataKey(b.ID, "lease", "", "holder-1"); err != nil || !swapped {
		t.Fatalf("claim through cache: (%v, %v), want (true, nil)", swapped, err)
	}
	// A stale expectation loses cleanly rather than erroring.
	if swapped, err := writer.CompareAndSetMetadataKey(b.ID, "lease", "", "holder-2"); err != nil || swapped {
		t.Fatalf("stale claim through cache: (%v, %v), want (false, nil)", swapped, err)
	}
	// The winner's value is visible through the cache (the CAS evicted, so the
	// next read consults the backing).
	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["lease"] != "holder-1" {
		t.Fatalf("lease through cache = %q, want %q", got.Metadata["lease"], "holder-1")
	}

	// The trio stays refused: the backing makes no revision claim, so the
	// cache must not report itself conditionally capable over it.
	if capable, _ := cache.probeConditionalWriteCapability(); capable {
		t.Fatal("CachingStore reports conditional-write capability over a narrow-only backing; " +
			"the revision-CAS trio has no sound fence there")
	}
}
