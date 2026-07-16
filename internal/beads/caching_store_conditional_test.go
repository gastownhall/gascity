package beads_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func TestCachingStoreConditionalWriterConformance(t *testing.T) {
	openOver := func(t *testing.T, m *beads.MemStore) beads.Store {
		t.Helper()
		c := beads.NewCachingStoreForTest(m, nil)
		if err := c.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		return c
	}
	beadstest.RunConditionalWriterConformanceWithOptions(t, "CachingStore",
		func(t *testing.T) beads.Store { return openOver(t, beads.NewMemStore()) },
		beadstest.ConditionalWriterOptions{
			// The MemStore backing populates PreconditionFailedError.Current and
			// CachingStore forwards its errors untouched, so the wrapped row
			// asserts Current too.
			SuppliesCurrent: true,
			// Disabled is a backing-level toggle: the backing still claims the
			// interface, returns typed unsupported per call, and CachingStore
			// forwards that verdict.
			OpenDisabled: func(t *testing.T) beads.Store {
				m := beads.NewMemStore()
				m.DisableConditionalWrites = true
				return openOver(t, m)
			},
		})
}

// conditionalCapabilityStrippedStore embeds the Store INTERFACE, so the
// wrapped store's optional ConditionalWriter methods are not promoted: this is
// the natural shape of a backing with no conditional-write capability at all
// (distinct from a disabled one, which still claims the interface).
type conditionalCapabilityStrippedStore struct{ beads.Store }

func TestCachingStoreConditionalWriterCapabilityAbsentBacking(t *testing.T) {
	t.Parallel()

	c := beads.NewCachingStoreForTest(conditionalCapabilityStrippedStore{Store: beads.NewMemStore()}, nil)
	if err := c.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	b, err := c.Create(beads.Bead{Title: "no-cas-backing"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// CachingStore always claims the interface; capability resolves per call
	// against the backing.
	w, ok := beads.ConditionalWriterFor(c)
	if !ok {
		t.Fatal("CachingStore must claim ConditionalWriter regardless of its backing")
	}

	assertUnsupported := func(verb string, err error) {
		t.Helper()
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			t.Fatalf("%s over a capability-absent backing: got %v, want ErrConditionalWriteUnsupported", verb, err)
		}
	}
	title := "x"
	assertUnsupported("UpdateIfMatch", w.UpdateIfMatch(b.ID, 1, beads.UpdateOpts{Title: &title}))
	assertUnsupported("CloseIfMatch", w.CloseIfMatch(b.ID, 1))
	assertUnsupported("DeleteIfMatch", w.DeleteIfMatch(b.ID, 1))
	swapped, casErr := w.CompareAndSetMetadataKey(b.ID, "k", "", "v")
	if swapped {
		t.Fatal("CompareAndSetMetadataKey over a capability-absent backing returned true")
	}
	assertUnsupported("CompareAndSetMetadataKey", casErr)

	// The capability misses must not perturb the cache: the bead is still served.
	got, err := c.Get(b.ID)
	if err != nil || got.ID != b.ID {
		t.Fatalf("Get after unsupported verbs = (%+v, %v), want the cached bead", got, err)
	}
}
