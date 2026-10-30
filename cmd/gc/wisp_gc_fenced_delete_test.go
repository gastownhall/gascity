package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// reopenAfterReReadStore reopens one session right after the purge's live
// re-read returns it: the window between the eligibility check and the
// delete, where another process (a configured named session coming back) can
// write. It declares its inner store as the conditional-writes resolution
// target, the way the cmd/gc policy store does, so a fenced delete must look
// through the wrapper to find the store's capability.
type reopenAfterReReadStore struct {
	beads.Store
	reopen string
	reads  int
}

func (s *reopenAfterReReadStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if id == s.reopen {
		s.reads++
		if s.reads == 1 {
			open := "open"
			if uerr := s.Update(id, beads.UpdateOpts{Status: &open}); uerr != nil {
				return b, uerr
			}
		}
	}
	return b, err
}

func (s *reopenAfterReReadStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// TestPurgeClosedInfraSessionsFencesTheDeleteOnTheReRead is the stale-read
// regression for the closed-session purge. The live re-read sees the session
// closed and past the cutoff, then a reopen lands before the delete. Before
// the fix the delete was unconditional and removed the reopened session. On
// every store that can fence, the delete is now conditional on the re-read's
// revision, so the reopened row survives and the other purgeable row goes.
func TestPurgeClosedInfraSessionsFencesTheDeleteOnTheReRead(t *testing.T) {
	now := time.Now()
	old := now.Add(-4 * 24 * time.Hour)
	rows := []beads.Bead{
		closedSessionPurgeBead("gcg-session-reopened", "session", old),
		closedSessionPurgeBead("gcg-session-old", "session", old),
	}
	for _, backend := range []struct {
		name string
		open func(t *testing.T) beads.Store
	}{
		{name: "SQLiteStore", open: func(t *testing.T) beads.Store {
			ledger := openSessionPurgeSQLiteStore(t)
			for _, row := range rows {
				mustCreateSessionPurgeBead(t, ledger, row)
			}
			return ledger
		}},
		// NewMemStoreFrom seeds the rows verbatim; Create would restamp
		// their timestamps to now and put them inside the cutoff.
		{name: "MemStore", open: func(*testing.T) beads.Store { return beads.NewMemStoreFrom(len(rows), rows, nil) }},
	} {
		t.Run(backend.name, func(t *testing.T) {
			ledger := backend.open(t)

			store := &reopenAfterReReadStore{Store: ledger, reopen: "gcg-session-reopened"}
			purged, err := purgeClosedInfraSessions(store, now, infraSessionPurgeAgeDefault, 500)
			if err != nil {
				t.Fatalf("purgeClosedInfraSessions: %v", err)
			}
			if purged != 1 {
				t.Fatalf("purged = %d, want 1", purged)
			}
			got, err := ledger.Get("gcg-session-reopened")
			if err != nil {
				t.Fatalf("a session reopened after the re-read was deleted: %v", err)
			}
			if got.Status != "open" {
				t.Fatalf("reopened session status = %q, want open", got.Status)
			}
			if _, err := ledger.Get("gcg-session-old"); !errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("old session Get = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestPurgeClosedInfraSessionsKeepsTheReReadDeleteWithoutCAS pins the
// fallback for a store that cannot fence (BdStore without --if-revision, a
// legacy SQLite layout): the purge keeps today's live re-read and plain
// delete, so closed sessions are still collected there.
func TestPurgeClosedInfraSessionsKeepsTheReReadDeleteWithoutCAS(t *testing.T) {
	now := time.Now()
	old := now.Add(-4 * 24 * time.Hour)
	mem := beads.NewMemStoreFrom(1, []beads.Bead{closedSessionPurgeBead("gcg-session-old", "session", old)}, nil)
	mem.DisableConditionalWrites = true

	purged, err := purgeClosedInfraSessions(mem, now, infraSessionPurgeAgeDefault, 500)
	if err != nil {
		t.Fatalf("purgeClosedInfraSessions: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if _, err := mem.Get("gcg-session-old"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("old session Get = %v, want ErrNotFound", err)
	}
}
