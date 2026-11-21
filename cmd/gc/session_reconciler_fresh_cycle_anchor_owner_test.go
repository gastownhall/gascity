package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A released anchor must not let a stale legacy tick kill the worker between
// releasing its bead and slinging the handoff (ga-70takxcc, #9320). The previous
// bead closed during this incarnation, and neither Q1 nor a claim stamp can
// defer the cycle: the live anchor's owner must decide it.
func freshCycleAnchorOwnerEnv(t *testing.T) (*restartRequestTestEnv, beads.Bead, string, *beads.MemStore, beads.Bead) {
	t.Helper()
	// The session template keeps the runtime name apart from the template and
	// named identity ("witness"), so assigning the anchor to the pool template
	// is distinguishable from assigning it to the session's runtime name.
	env, session, name := freshCycleReproEnvIn(t, config.Workspace{Name: "test-city", SessionTemplate: "{{.City}}-{{.Agent}}"}, nil)
	// Production supplies the ready flag with the assigned/open snapshot;
	// omitting it would filter the anchor out before the cycle guard runs.
	env.startOptions = append(env.startOptions, withReadyAssignedFlags([]bool{true}))
	env.setSessionMetadata(&session, map[string]string{
		"state":                                "awake",
		sessionpkg.CurrentBeadIDKey:            "ga-prev",
		"awake_started_at":                     env.clk.Now().Add(-time.Hour).Format(time.RFC3339Nano),
		beadmeta.CurrentClaimBeadIDMetadataKey: "",
	})
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-prev", Title: "completed previous work", Type: "task", Assignee: "witness"})
	reproCloseForReal(t, rig, "ga-prev")
	// Control the close time with the fixture clock so row C cannot hide the
	// missing guard, regardless of the machine's wall clock.
	if err := rig.SetMetadata("ga-prev", "closed_at", env.clk.Now().Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	reproCreate(t, rig, beads.Bead{ID: "ga-anchor", Title: "handoff in progress", Type: "task", Assignee: "witness"})
	anchor := mustGetBead(t, rig, "ga-anchor")
	return env, session, name, rig, anchor
}

func assertFreshAnchorOwnerRetained(t *testing.T, env *restartRequestTestEnv, before beads.Bead, name string, callsBefore int) {
	t.Helper()
	for _, call := range env.sp.SnapshotCalls()[callsBefore:] {
		switch call.Method {
		case "Stop", "TerminateRuntime", "Start", "Relaunch":
			t.Errorf("runtime %s(%q) after anchor handoff; want no stop or restart", call.Method, call.Name)
		}
	}
	after := mustGetBead(t, env.store, before.ID)
	for _, key := range []string{
		"state", "session_key", sessionpkg.CurrentBeadIDKey,
		beadmeta.CurrentClaimBeadIDMetadataKey, "restart_requested",
		"continuation_reset_pending", sessionpkg.ResetCommittedAtKey,
	} {
		if after.Metadata[key] != before.Metadata[key] {
			t.Errorf("session %s = %q, want %q preserved after anchor handoff", key, after.Metadata[key], before.Metadata[key])
		}
	}
	if !env.sp.IsRunning(name) {
		t.Error("worker stopped before it could finish the handoff")
	}
}

func TestFreshCycleRepro_LiveAnchorOwnershipControlsCycle(t *testing.T) {
	for _, tc := range []struct {
		name, assignee string
		meta           map[string]string
		pool, namepool bool
		cycle          bool
	}{
		{name: "released"},
		{name: "reassigned away", assignee: "another-session"},
		{name: "blank assignee", assignee: " \t "},
		{name: "transient pool slot excluded", assignee: "worker-1", pool: true, meta: map[string]string{"alias": "worker-1"}},
		{name: "pool template excluded", assignee: "witness", pool: true},
		{name: "alias history excluded", assignee: "old-alias", meta: map[string]string{"alias": "worker-alias", "alias_history": "old-alias"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session, name, rig, anchor := freshCycleAnchorOwnerEnv(t)
			if tc.pool {
				env.cfg.NamedSessions = nil
				env.cfg.Agents[0].MaxActiveSessions = restartRequestTestIntPtr(2)
				env.setSessionMetadata(&session, map[string]string{
					namedSessionMetadataKey: "false", namedSessionIdentityMetadata: "",
					poolManagedMetadataKey: "true", "pool_slot": "1",
				})
				if tc.namepool {
					env.cfg.Agents[0].NamepoolNames = []string{"worker-one", "worker-two"}
				}
			}
			env.setSessionMetadata(&session, tc.meta)
			// The tick saw the anchor owned by this session before the release.
			anchor.Assignee = session.ID
			if err := rig.Update(anchor.ID, beads.UpdateOpts{Assignee: &anchor.Assignee}); err != nil {
				t.Fatal(err)
			}
			rigs := map[string]beads.Store{"gascity": rig}
			sw := testSeatWork(env.city, env.cfg, env.store, rigs)
			sw.once.Do(sw.load)
			assignee := tc.assignee
			switch assignee {
			case "$id":
				assignee = session.ID
			case "$name":
				assignee = name
			}
			if err := rig.Update(anchor.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
				t.Fatal(err)
			}
			before := mustGetBead(t, env.store, session.ID)
			callsBefore := len(env.sp.SnapshotCalls())

			reconcileFreshCycleReproWithSeatWork(env, sw, []beads.Bead{session}, []beads.Bead{anchor}, rigs)

			if tc.cycle {
				assertCycled(t, env, session, "a genuinely different open anchor is still owned by this session")
			} else {
				assertFreshAnchorOwnerRetained(t, env, before, name, callsBefore)
			}
		})
	}
}
