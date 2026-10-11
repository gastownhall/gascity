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
// Coverage denominator, stated per the receipt rule: this control covers the
// beadReconcileTick call site (city_runtime.go). The one-shot path's separate
// call site in cmd_start.go is NOT covered by this test.
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
		AssignedWorkBeads:     []beads.Bead{callsiteControlGet(t, workStore, w1.ID), callsiteControlGet(t, workStore, w2.ID)},
		AssignedWorkStores:    []beads.Store{workStore, workStore},
		AssignedWorkStoreRefs: []string{"", ""},
	}

	cr.beadReconcileTick(context.Background(), result, newSessionBeadSnapshot([]beads.Bead{snapshotSession}), nil, false)

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
}

func callsiteControlGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}
