package beads_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestCachingStoreCloseReasonConformance runs the close-reason contract through
// a primed CachingStore, so its cached patches (Reopen's included) report the
// same close_reason and metadata the backing does.
func TestCachingStoreCloseReasonConformance(t *testing.T) {
	beadstest.RunCloseReasonTests(t, func() beads.Store {
		cs := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
		if err := cs.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		return cs
	})
}
