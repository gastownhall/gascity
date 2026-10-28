package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestReapStaleSessionBeads_PoolRowCommittedAfterListKeepsItsRuntime: the
// stuck-creating reaper listed a pool row before its start committed; by the
// time the pass reaches the row's teardown the commit has landed. The stale
// list entry must not cost the committed row its box: the teardown re-reads
// the row under the name's flock, sees the commit, and holds, and the reaper
// leaves the row open and active.
//
// Kills: a pool arm that stops from the listed snapshot without re-reading.
func TestReapStaleSessionBeads_PoolRowCommittedAfterListKeepsItsRuntime(t *testing.T) {
	env, session, name := newRestartedPendingCreate(t, rowPool)
	mem, ok := env.store.(*beads.MemStore)
	if !ok {
		t.Fatalf("fixture: want a *beads.MemStore store, got %T", env.store)
	}
	// The reaper's listing, taken before the row's start commits.
	listed := snapshotOf(t, mem, session.ID)
	// The row's start commits after the reaper listed it: state active, the
	// claim and start marker cleared, and a fresh incarnation (token,
	// generation, last_woke_at).
	env.setSessionMetadata(&session, map[string]string{
		"state":                     string(sessionpkg.StateActive),
		"state_reason":              "creation_complete",
		"creation_complete_at":      env.clk.Now().UTC().Format(time.RFC3339),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
		"instance_token":            "test-token-2",
		"generation":                "2",
		"last_woke_at":              env.clk.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
	})
	store := staleListStore{MemStore: mem, snapshot: listed}
	sp := blindLivenessProvider{execProfile(env.sp)}
	// The store stamps CreatedAt from the wall clock.
	clk := &clock.Fake{Time: time.Now().Add(24 * time.Hour)}
	reapStaleSessionBeads(env.city, store, testSeatWork(env.city, nil, env.store, nil), sp, env.dt, nil, clk, &env.stderr)
	if stops := countCalls(env.sp, "Stop"); stops != 0 {
		t.Fatalf("Stop called %d times on a box its start committed after the listing; stderr=%s", stops, env.stderr.String())
	}
	if !env.sp.IsRunning(name) {
		t.Fatalf("the committed row's box was torn down from a listing that predates the commit; stderr=%s", env.stderr.String())
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "closed" || got.Metadata["state"] != string(sessionpkg.StateActive) {
		t.Fatalf("status=%s state=%q, want the row open and active; stderr=%s", got.Status, got.Metadata["state"], env.stderr.String())
	}
}
