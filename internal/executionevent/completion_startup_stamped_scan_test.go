package executionevent

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestCompletionBackstopStartupSweepDoesNotScanPerStampedRoot (ga-vn0uok,
// #7172): a boot sweep used to re-verify stamped, converged roots, and on a
// native Dolt store every per-root gc.root_bead_id lookup is a full table
// scan (~3.4s per root; hours of store load after every controller start).
// The sweep reason no longer selects what the sweep visits, so re-verifying
// N already-converged roots must not cost N step lookups.
func TestCompletionBackstopStartupSweepDoesNotScanPerStampedRoot(t *testing.T) {
	const roots = 8
	backing, rootIDs, _ := closedCompletionCorpus(t, roots)
	journal := events.NewFake()
	seed := []beads.GraphStore{{Store: backing}}
	// First sweep emits the facts; the second reads them back and stamps.
	for range 2 {
		(&CompletionBackstop{}).Pass(journal, seed, "execution-reconcile")
	}
	for _, id := range rootIDs {
		root, err := backing.Get(id)
		if err != nil || root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] == "" {
			t.Fatalf("seed stamping failed for %s: %v", id, err)
		}
	}

	store := &sweepCountingGraphStore{Store: backing}
	r := (&CompletionBackstop{}).Pass(journal, []beads.GraphStore{{Store: store}}, "execution-reconcile")
	if r.Emitted != 0 {
		t.Fatalf("startup sweep = %+v, want nothing emitted for converged roots", r)
	}
	if store.stepLists >= roots {
		t.Fatalf("startup sweep over %d stamped, converged roots issued %d gc.root_bead_id step lookups (one full table scan each on Dolt); want fewer than one per root (result %+v)", roots, store.stepLists, r)
	}
}
