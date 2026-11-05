package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestCommittedStartStillChurns pins what #7564 (mc-pxtbw) rests on (its
// review, mutant A4): the start's commit clears pending_create_started_at, so
// a runtime that committed and then died inside the churn band still accrues
// churn. Only a start that never committed is exempt.
func TestCommittedStartStillChurns(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.clk.Time = time.Now().UTC()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	row := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&row, map[string]string{"session_origin": "manual", "state": "asleep"})
	if woken := env.reconcile([]beads.Bead{mustGetBead(t, env.store, row.ID)}); woken != 1 || !env.sp.IsRunning("worker") {
		t.Fatalf("woken %d running %v; stderr %s", woken, env.sp.IsRunning("worker"), env.stderr.String())
	}
	meta := mustGetBead(t, env.store, row.ID).Metadata
	if meta["state"] != "active" || meta["pending_create_started_at"] != "" || meta["last_woke_at"] == "" {
		t.Fatalf("after the commit: state=%q pending_create_started_at=%q last_woke_at=%q; want active, the marker cleared, the wake stamped",
			meta["state"], meta["pending_create_started_at"], meta["last_woke_at"])
	}
	_ = env.sp.Stop("worker")
	env.clk.Advance(stabilityThreshold + 10*time.Second)
	env.reconcile([]beads.Bead{mustGetBead(t, env.store, row.ID)})
	if got := mustGetBead(t, env.store, row.ID).Metadata["churn_count"]; got != "1" {
		t.Fatalf("churn_count after a committed runtime died in the churn band = %q, want 1", got)
	}
}
