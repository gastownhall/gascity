package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// seatHandoverCity is a pool-capable "worker" template backing the configured
// named session "reviewer", whose runtime name is "reviewer".
func seatHandoverCity() *config.City {
	return &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}
}

// seatRowMeta is the metadata of a "reviewer" seat row of template "worker",
// with overrides applied (an empty value deletes the key).
func seatRowMeta(state string, overrides map[string]string) map[string]string {
	meta := map[string]string{
		"template":                   "worker",
		"session_name":               "reviewer",
		"state":                      state,
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "reviewer",
	}
	for k, v := range overrides {
		if v == "" {
			delete(meta, k)
			continue
		}
		meta[k] = v
	}
	return meta
}

func createSeatSessionRow(t *testing.T, store beads.Store, title string, meta map[string]string) beads.Bead {
	t.Helper()
	row, err := store.Create(beads.Bead{
		Title:    title,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("create session row %s: %v", title, err)
	}
	return row
}

// createClosedSeatPredecessor creates the predecessor row and closes it, as a
// drained seat's row is closed.
func createClosedSeatPredecessor(t *testing.T, store beads.Store, overrides map[string]string) beads.Bead {
	t.Helper()
	pred := createSeatSessionRow(t, store, "predecessor", seatRowMeta("asleep", overrides))
	if err := store.Close(pred.ID); err != nil {
		t.Fatalf("close predecessor %s: %v", pred.ID, err)
	}
	return pred
}

const seatHandoverClaimedAt = "2026-09-14T19:51:07Z"

// createInProgressWorkClaimedBy creates in_progress "worker"-routed work held
// under holder's bead ID, with the claim projection its hook stamped.
func createInProgressWorkClaimedBy(t *testing.T, store beads.Store, holder beads.Bead) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    "long bake",
		Type:     "task",
		Assignee: holder.ID,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:    "worker",
			beadmeta.SessionIDMetadataKey:   holder.ID,
			beadmeta.SessionNameMetadataKey: holder.Metadata["session_name"],
			beadmeta.ClaimedAtMetadataKey:   seatHandoverClaimedAt,
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set work in_progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	return work
}

// sweepSeatWork runs the dead-assignee sweep over work with openRows as the
// open-session snapshot, on a single-store city.
func sweepSeatWork(store beads.Store, cfg *config.City, openRows []beads.Bead, work []beads.Bead) []releasedPoolAssignment {
	return releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		cfg,
		"",
		sessionInfosFromBeads(openRows),
		work,
		nil, nil, nil, nil, nil, nil,
	)
}

func getSeatBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}

// TestReleaseOrphanedPoolAssignments_HandsClosedSeatRowClaimToSuccessor
// (AC-1): work held under the bead ID of a closed named seat row is handed to
// the seat's one live successor row instead of being reopened, so no
// bead.dead_assignee_reopened is recorded for it (gastownhall/gascity#6362).
func TestReleaseOrphanedPoolAssignments_HandsClosedSeatRowClaimToSuccessor(t *testing.T) {
	store := beads.NewMemStore()
	cfg := seatHandoverCity()
	pred := createClosedSeatPredecessor(t, store, nil)
	work := createInProgressWorkClaimedBy(t, store, pred)
	successor := createSeatSessionRow(t, store, "successor", seatRowMeta("awake", map[string]string{"currently_processing_bead_id": work.ID}))

	snapshot := []beads.Bead{work}
	released := sweepSeatWork(store, cfg, []beads.Bead{successor}, snapshot)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none: the seat's live successor %s took over the claim of closed row %s", released, successor.ID, pred.ID)
	}
	rec := events.NewFake()
	emitDeadAssigneeReopenedEvents(rec, snapshot, released, time.Now())
	if len(rec.Events) != 0 {
		t.Fatalf("recorded %d bead.dead_assignee_reopened events, want none", len(rec.Events))
	}
	got := getSeatBead(t, store, work.ID)
	if got.Assignee != successor.ID {
		t.Fatalf("assignee = %q, want successor %q", got.Assignee, successor.ID)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
}
