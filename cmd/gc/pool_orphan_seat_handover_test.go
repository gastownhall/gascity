package main

import (
	"reflect"
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

// TestReleaseOrphanedPoolAssignments_SeatHandOverWritesOnlyAssignee (AC-2):
// the hand-over moves the claim and nothing else. Status, the claim
// projection (gc.session_id, gc.session_name, gc.claimed_at), routing and all
// other metadata stay as they were until the successor's own hook rebinds
// them. Revision, ClaimFence and UpdatedAt are the store's bookkeeping of the
// write itself.
func TestReleaseOrphanedPoolAssignments_SeatHandOverWritesOnlyAssignee(t *testing.T) {
	store := beads.NewMemStore()
	cfg := seatHandoverCity()
	pred := createClosedSeatPredecessor(t, store, nil)
	work := createInProgressWorkClaimedBy(t, store, pred)
	if err := store.SetMetadata(work.ID, "gc.session_affinity", "require"); err != nil {
		t.Fatalf("set affinity: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Labels: []string{"pool:worker"}, Description: stringPtr("bake for a while")}); err != nil {
		t.Fatalf("set labels: %v", err)
	}
	work = getSeatBead(t, store, work.ID)
	successor := createSeatSessionRow(t, store, "successor", seatRowMeta("awake", nil))

	if released := sweepSeatWork(store, cfg, []beads.Bead{successor}, []beads.Bead{work}); len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}
	got := getSeatBead(t, store, work.ID)
	if got.Assignee != successor.ID {
		t.Fatalf("assignee = %q, want successor %q", got.Assignee, successor.ID)
	}
	got.Assignee = work.Assignee
	got.Revision, got.ClaimFence, got.UpdatedAt = work.Revision, work.ClaimFence, work.UpdatedAt
	if !reflect.DeepEqual(got, work) {
		t.Fatalf("hand-over changed more than the assignee:\n got  %+v\n want %+v", got, work)
	}
}

// TestReleaseOrphanedPoolAssignments_ReleasesClaimOfSeatlessClosedRow (AC-3):
// a closed row with no seat identity has no successor, even when a sibling of
// its template is processing the bead: siblings never inherit its claims
// (ga-r22k2y), so the sweep releases it as at base.
func TestReleaseOrphanedPoolAssignments_ReleasesClaimOfSeatlessClosedRow(t *testing.T) {
	poolRow := map[string]string{
		namedSessionMetadataKey:      "",
		namedSessionIdentityMetadata: "",
		poolManagedMetadataKey:       "true",
	}
	for _, tc := range []struct {
		name      string
		pred      map[string]string
		successor map[string]string
	}{{
		name:      "unaliased pool row",
		pred:      mergeSeatMeta(poolRow, map[string]string{"session_name": "worker-gc-pred"}),
		successor: mergeSeatMeta(poolRow, map[string]string{"session_name": "worker-gc-sibling"}),
	}, {
		name:      "transient pool-slot alias",
		pred:      mergeSeatMeta(poolRow, map[string]string{"session_name": "worker-1", "alias": "worker-1", "pool_slot": "1"}),
		successor: mergeSeatMeta(poolRow, map[string]string{"session_name": "worker-1", "alias": "worker-1", "pool_slot": "1"}),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := seatHandoverCity()
			pred := createClosedSeatPredecessor(t, store, tc.pred)
			work := createInProgressWorkClaimedBy(t, store, pred)
			sibling := createSeatSessionRow(t, store, "sibling", seatRowMeta("awake", mergeSeatMeta(tc.successor, map[string]string{"currently_processing_bead_id": work.ID})))

			released := sweepSeatWork(store, cfg, []beads.Bead{sibling}, []beads.Bead{work})
			assertSeatWorkReleased(t, store, work, released)
		})
	}
}

func assertSeatWorkReleased(t *testing.T, store beads.Store, work beads.Bead, released []releasedPoolAssignment) {
	t.Helper()
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %+v, want exactly %s", released, work.ID)
	}
	got := getSeatBead(t, store, work.ID)
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("work = status %q assignee %q, want open/unassigned", got.Status, got.Assignee)
	}
}

func mergeSeatMeta(base, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
