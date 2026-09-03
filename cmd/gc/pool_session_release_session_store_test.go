package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// seedReleaseFixture builds the two-class fixture the tests below share: a
// pool session bead carrying alias "nux" in sessionStore, and an in_progress
// work bead claimed under that alias in workStore.
//
// openSessionInfos is deliberately left nil at every call site. That models
// the live maintainer-city arm where the store-ref index misses — the city leg
// records the work under store-ref "" while the owning session is indexed
// under "gascity", so openSessionOwnsWork returns false and neither fail-open
// sentinel fires. The liveness re-read is then the only thing standing between
// a live worker and having its claim yanked.
func seedReleaseFixture(t *testing.T, sessionStore, workStore beads.Store, sessionStatus string) beads.Bead {
	t.Helper()

	sess, err := sessionStore.Create(beads.Bead{
		Title:    "pool session",
		Type:     sessionBeadType,
		Status:   "open",
		Labels:   []string{sessionBeadLabel},
		Metadata: map[string]string{"session_name": "worker-1", "alias": "nux"},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if sessionStatus != "open" {
		if err := sessionStore.Update(sess.ID, beads.UpdateOpts{Status: &sessionStatus}); err != nil {
			t.Fatalf("set session bead status %q: %v", sessionStatus, err)
		}
	}

	work, err := workStore.Create(beads.Bead{
		Title:    "work claimed by the live pool session",
		Type:     "task",
		Status:   "open",
		Assignee: "nux",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := workStore.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	work, err = workStore.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work bead: %v", err)
	}
	return work
}

// TestReleaseOrphanedPoolAssignmentsReadsLivenessFromSessionStore is the
// ga-g3pf0 repro.
//
// releaseOrphanedPoolAssignments took a single store and used it for two
// different storage classes: the work-class owner fallback and the
// session-class liveness read. On maintainer-city, [storage.classes] relocates
// the sessions class to its own binding, so the controller's work store serves
// zero session beads. liveOpenSessionAssignmentExists issued its gc:session
// label query against that store, got an empty result with no error, and read
// it as "this assignee is dead" — only an actual query *error* fails closed.
//
// Every ~15 minutes the controller reopened live work under live workers:
// bead.dead_assignee_reopened, then ~1s later the session drained as orphaned.
// All 25 released beads in the live journal were binding-resident.
func TestReleaseOrphanedPoolAssignmentsReadsLivenessFromSessionStore(t *testing.T) {
	sessionStore := beads.NewMemStore()
	workStore := beads.NewMemStore()
	work := seedReleaseFixture(t, sessionStore, workStore, "open")

	released := releaseOrphanedPoolAssignments(
		workStore,
		beads.SessionStore{Store: sessionStore},
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{workStore},
		nil,
		nil,
	)

	if len(released) != 0 {
		t.Fatalf("released %v, want none — alias \"nux\" belongs to an open session bead in the sessions store, "+
			"so its holder is alive. Releasing it means the liveness read went to the work store, which serves "+
			"no session beads, and mistook an empty label query for a dead assignee.", released)
	}
	got, err := workStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "nux" {
		t.Fatalf("live worker's claim was dropped: status=%q assignee=%q, want in_progress/nux", got.Status, got.Assignee)
	}
}

// TestReleaseOrphanedPoolAssignmentsStillReleasesWhenSessionStoreSaysDead is
// the control for the test above. Reading liveness from the sessions store
// must not make orphan release inert — if the repro passed simply because
// nothing is ever released now, the bug would be traded for a worse one
// (claims stuck forever on sessions that really are gone).
func TestReleaseOrphanedPoolAssignmentsStillReleasesWhenSessionStoreSaysDead(t *testing.T) {
	sessionStore := beads.NewMemStore()
	workStore := beads.NewMemStore()
	work := seedReleaseFixture(t, sessionStore, workStore, "closed")

	released := releaseOrphanedPoolAssignments(
		workStore,
		beads.SessionStore{Store: sessionStore},
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{workStore},
		nil,
		nil,
	)

	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s] — the only session bead naming alias \"nux\" is closed, "+
			"so the claim is genuinely orphaned and must be recovered", released, work.ID)
	}
	got, err := workStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("orphaned work not recovered: status=%q assignee=%q, want open/unassigned", got.Status, got.Assignee)
	}
}

// TestReleaseOrphanedPoolAssignmentsFallsBackToWorkStoreForLiveness pins the
// zero-value SessionStore path. liveOpenSessionAssignmentExists returns false
// for a nil store and false means release, so an unset session store would
// silently declare the whole fleet dead. The fallback has to reach the work
// store — which is exactly where a city with no [storage.classes] sessions
// binding keeps its session beads — making the change behavior-preserving for
// any caller still passing one store.
func TestReleaseOrphanedPoolAssignmentsFallsBackToWorkStoreForLiveness(t *testing.T) {
	single := beads.NewMemStore()
	work := seedReleaseFixture(t, single, single, "open")

	released := releaseOrphanedPoolAssignments(
		single,
		beads.SessionStore{}, // unset: no session class configured
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{single},
		nil,
		nil,
	)

	if len(released) != 0 {
		t.Fatalf("released %v, want none — with no session store supplied the liveness read must fall back to "+
			"the work store, which holds the live session bead here. Releasing means a nil session store reads "+
			"as \"every assignee is dead\".", released)
	}
}
