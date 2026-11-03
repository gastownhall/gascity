package main

import (
	"context"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestBeadReconcileTick_OrphanReleaseCallSite_RetainsLiveAndWakeProtectedWork
// is the production-call-site control for the rebased orphan-release stack
// (precondition 3.0): it drives the REAL tick — beadReconcileTick — through
// the snapshot-staleness window and asserts both cures hold AT THE CALL SITE,
// so a lost or mis-merged hunk at city_runtime.go's release call turns this
// red rather than leaving a green suite over regressed production code.
//
// Two work beads, one per cure:
//
//   - W1 (sessionStore cure, upstream 512b79c67 / ga-g3pf0): assigned to a
//     session whose bead lives ONLY in the relocated sessions-class store.
//     Its liveness is invisible to the work store; only the call site passing
//     the typed session store keeps it from being released every tick.
//     LEG D reachable-red: repoint the call site's sessionStore argument at
//     the work store and W1 is falsely released — this test MUST fail.
//
//   - W2 (protectedWakeWork cure, local ce42c9c7c / gc-ft31x): assigned to
//     the bare template identity with no session bead anywhere (the
//     replacement bead is not yet in the pre-tick snapshot), while an open
//     session of that template makes it reachable to the SAME tick's wake
//     arm. Only the pre-release wake-candidate exemption retains it.
//     LEG E reachable-red: drop protectedWakeWorkIDs(preWakeCandidates) from
//     the call site (pass nil) and W2 is reopened — this test MUST fail.
//
//   - W3 (non-vacuity control, ga-8uf72n): assigned to a session that has no
//     bead anywhere — a true orphan. The sweep now runs as an in-tick DELTA over
//     the assignees whose session just went away (the rest is the off-tick
//     backstop's), so W1 and W2 only reach the release loop if their assignees
//     are queued. The test queues them, and asserts W3 IS released: without
//     that, "W1 and W2 were retained" would be satisfied by a call site that no
//     longer sweeps at all.
//
// Coverage denominator, stated per the receipt rule: this control covers the
// beadReconcileTick call site (city_runtime.go). The one-shot path's separate
// call site in cmd_start.go and the v2 reconciler's orphanRelease are NOT
// covered by this test. The off-tick backstop's own deference to the published
// protected set is covered by orphan_release_lane_test.go.
func TestBeadReconcileTick_OrphanReleaseCallSite_RetainsLiveAndWakeProtectedWork(t *testing.T) {
	workStore := beads.NewMemStore()
	sessionStore := beads.NewMemStore() // relocated [beads.classes.sessions] binding

	inProgress := "in_progress"

	// W1: assignee live ONLY in the relocated sessions store.
	w1, err := workStore.Create(beads.Bead{
		Title:    "w1 routed work, assignee live in relocated sessions store",
		Assignee: "worker-w1-live",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create w1: %v", err)
	}
	if err := workStore.Update(w1.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("w1 in_progress: %v", err)
	}
	if _, err := sessionStore.Create(beads.Bead{
		Title:  "live session for w1, resident in the sessions class only",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name": "worker-w1-live",
			"template":     "worker",
			"agent_name":   "worker",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("create w1 session bead: %v", err)
	}

	// W2: bare-template assignee, no session bead anywhere (staleness window),
	// wake-reachable via the open worker session in the snapshot below.
	w2, err := workStore.Create(beads.Bead{
		Title:    "w2 routed work in the snapshot-staleness window",
		Assignee: "worker",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create w2: %v", err)
	}
	if err := workStore.Update(w2.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("w2 in_progress: %v", err)
	}

	// W3: a true orphan. Its assignee has no session bead anywhere, and nothing
	// wake-protects it.
	w3, err := workStore.Create(beads.Bead{
		Title:    "w3 routed work whose assignee has no session",
		Assignee: "worker-dead",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create w3: %v", err)
	}
	if err := workStore.Update(w3.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("w3 in_progress: %v", err)
	}

	// Snapshot: one open worker session whose own identities match neither
	// assignee — it exists to make W2 wake-reachable through the template key,
	// exactly the "wake arm is about to serve this" half of the treadmill.
	snapshotSession := beads.Bead{
		ID:     "sc-snap-worker",
		Title:  "open worker session in the pre-tick snapshot",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"template":     "worker",
			"agent_name":   "worker",
			"state":        "active",
		},
	}

	cfg := &config.City{Agents: []config.Agent{{
		Name:              "worker",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(2),
	}}}

	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "callsite-control-city",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: workStore,
		storageRoutes: &storageRoutes{stores: map[coordclass.Class]beads.Store{
			coordclass.ClassSessions: sessionStore,
		}},
		sessionDrains: newDrainTracker(),
		rec:           events.Discard,
		stdout:        io.Discard,
		stderr:        io.Discard,
	}

	result := DesiredStateResult{
		State:                 map[string]TemplateParams{},
		AssignedWorkBeads:     []beads.Bead{callsiteControlGet(t, workStore, w1.ID), callsiteControlGet(t, workStore, w2.ID), callsiteControlGet(t, workStore, w3.ID)},
		AssignedWorkStores:    []beads.Store{workStore, workStore, workStore},
		AssignedWorkStoreRefs: []string{"", "", ""},
	}

	// The delta trigger: these assignees' sessions just went away. Queuing all
	// three puts every bead in front of the release loop, so W1's and W2's
	// retention is the cures' doing and W3's release is the sweep's.
	cr.orphanReleaseLaneOf().queueAssignees("worker-w1-live", "worker", "worker-dead")

	cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), result, newSessionBeadSnapshot([]beads.Bead{snapshotSession}), nil, false)

	for _, tc := range []struct {
		id, assignee, cure string
	}{
		{w1.ID, "worker-w1-live", "sessionStore (liveness read must hit the relocated sessions class, not the work store)"},
		{w2.ID, "worker", "protectedWakeWork (release arm must not reopen work this tick's wake arm serves)"},
	} {
		got := callsiteControlGet(t, workStore, tc.id)
		if got.Assignee != tc.assignee || got.Status != inProgress {
			t.Errorf("%s was released at the production call site: assignee=%q status=%q, want assignee=%q status=%q — lost cure: %s",
				tc.id, got.Assignee, got.Status, tc.assignee, inProgress, tc.cure)
		}
	}

	// Control: the same call released the true orphan, so the retentions above
	// were decisions rather than a sweep that never ran.
	if got := callsiteControlGet(t, workStore, w3.ID); got.Assignee != "" || got.Status != "open" {
		t.Errorf("%s (true orphan) not released at the production call site: assignee=%q status=%q, want an open, unassigned bead — the call site no longer sweeps, so the W1/W2 assertions are vacuous",
			w3.ID, got.Assignee, got.Status)
	}
}

func callsiteControlGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}
