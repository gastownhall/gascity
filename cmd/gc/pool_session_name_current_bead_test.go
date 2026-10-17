package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// seedPredecessorClaimedWork creates in_progress pool work in store whose
// assignee and gc.session_id still name a drained predecessor session that
// has no open session bead: the claim projection a replacement session
// inherits when it continues the work without a fresh `gc hook --claim`
// (gascity#6362).
func seedPredecessorClaimedWork(t *testing.T, store beads.Store, route string) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    "adopted work",
		Assignee: "gc-predecessor",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:  route,
			beadmeta.SessionIDMetadataKey: "gc-predecessor",
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	return work
}

// replacementSessionBead is an open session bead sharing no identity form
// with the predecessor, in the given state, that reports currentBead as its
// currently_processing_bead_id.
func replacementSessionBead(id, state, currentBead string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  "replacement session",
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":           id,
			"template":               "worker",
			"state":                  state,
			session.CurrentBeadIDKey: currentBead,
		},
	}
}

func singleWorkerPoolCity() *config.City {
	return &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
}

// gascity#6362: a replacement session is awake and reports the work as its
// currently_processing_bead_id while the work's claim still names the drained
// predecessor. The dead-assignee sweep must keep the claim instead of
// reopening work a live session is executing.
func TestReleaseOrphanedPoolAssignments_KeepsWorkAnAwakeSessionIsProcessing(t *testing.T) {
	for _, state := range []session.State{session.StateAwake, session.StateActive} {
		t.Run(string(state), func(t *testing.T) {
			store := beads.NewMemStore()
			work := seedPredecessorClaimedWork(t, store, "worker")

			released := releaseOrphanedPoolAssignmentsFromBeads(store, singleWorkerPoolCity(), "",
				[]beads.Bead{replacementSessionBead("gc-replacement", string(state), work.ID)},
				[]beads.Bead{work}, nil, nil, nil)
			if len(released) != 0 {
				t.Fatalf("released = %v, want none: an awake session reports %s as its current bead", released, work.ID)
			}
			got, err := store.Get(work.ID)
			if err != nil {
				t.Fatalf("get work: %v", err)
			}
			if got.Status != "in_progress" || got.Assignee != "gc-predecessor" {
				t.Fatalf("work = status %q assignee %q, want in_progress/gc-predecessor untouched", got.Status, got.Assignee)
			}
		})
	}
}

// A currently_processing_bead_id left on a session that is not awake is a
// stale pointer, not evidence of live work: it must not strand a dead
// assignee's claim for as long as that session sleeps.
func TestReleaseOrphanedPoolAssignments_ReleasesWhenOnlyASleepingSessionNamesTheBead(t *testing.T) {
	for _, state := range []session.State{session.StateAsleep, session.StateDrained, session.StateCreating} {
		t.Run(string(state), func(t *testing.T) {
			store := beads.NewMemStore()
			work := seedPredecessorClaimedWork(t, store, "worker")

			released := releaseOrphanedPoolAssignmentsFromBeads(store, singleWorkerPoolCity(), "",
				[]beads.Bead{replacementSessionBead("gc-replacement", string(state), work.ID)},
				[]beads.Bead{work}, nil, nil, nil)
			if len(released) != 1 || released[0].ID != work.ID {
				t.Fatalf("released = %v, want [%s]: a %s session's pointer is not live work", released, work.ID, state)
			}
		})
	}
}

// An awake session processing a DIFFERENT bead says nothing about this one.
func TestReleaseOrphanedPoolAssignments_ReleasesWhenAwakeSessionProcessesAnotherBead(t *testing.T) {
	store := beads.NewMemStore()
	work := seedPredecessorClaimedWork(t, store, "worker")

	released := releaseOrphanedPoolAssignmentsFromBeads(store, singleWorkerPoolCity(), "",
		[]beads.Bead{replacementSessionBead("gc-other", string(session.StateAwake), "zz-unrelated")},
		[]beads.Bead{work}, nil, nil, nil)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}
}

// The pointer is a bare bead ID. An awake session that cannot reach the work's
// store must not shield a same-ID bead there, matching how assignee ownership
// is store-scoped (ReleasesRigWorkAssignedToUnreachableOpenSession).
func TestReleaseOrphanedPoolAssignments_CurrentBeadDoesNotShieldUnreachableStore(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work := seedPredecessorClaimedWork(t, rigStore, "repo/worker")
	sess, err := cityStore.Create(beads.Bead{
		Title:  "city worker",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":           "worker-live",
			"template":               "worker",
			"agent_name":             "worker",
			"state":                  string(session.StateAwake),
			poolManagedMetadataKey:   boolMetadata(true),
			session.CurrentBeadIDKey: work.ID,
		},
	})
	if err != nil {
		t.Fatalf("create city session: %v", err)
	}
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "repo", Path: t.TempDir()}},
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			{Name: "worker", Dir: "repo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		},
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(cityStore, cfg, cityPath,
		[]beads.Bead{sess}, []beads.Bead{work},
		[]beads.Store{rigStore}, []string{"repo"}, map[string]beads.Store{"repo": rigStore})
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]: the session cannot reach the repo store", released, work.ID)
	}
}

// The reachable-store form of the #6362 keep: a rig-scoped awake session that
// serves the work's store keeps the claim.
func TestReleaseOrphanedPoolAssignments_KeepsReachableStoreWorkAnAwakeSessionIsProcessing(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work := seedPredecessorClaimedWork(t, rigStore, "repo/worker")
	sess, err := cityStore.Create(beads.Bead{
		Title:  "rig worker",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":           "repo--worker-live",
			"template":               "repo/worker",
			"agent_name":             "repo/worker",
			"state":                  string(session.StateAwake),
			poolManagedMetadataKey:   boolMetadata(true),
			session.CurrentBeadIDKey: work.ID,
		},
	})
	if err != nil {
		t.Fatalf("create rig session: %v", err)
	}
	cfg := &config.City{
		Rigs:   []config.Rig{{Name: "repo", Path: t.TempDir()}},
		Agents: []config.Agent{{Name: "worker", Dir: "repo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(cityStore, cfg, cityPath,
		[]beads.Bead{sess}, []beads.Bead{work},
		[]beads.Store{rigStore}, []string{"repo"}, map[string]beads.Store{"repo": rigStore})
	if len(released) != 0 {
		t.Fatalf("released = %v, want none: an awake repo session reports %s as its current bead", released, work.ID)
	}
}
