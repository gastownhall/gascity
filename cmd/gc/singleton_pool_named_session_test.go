package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// singletonPoolNamedSessionCity is the config shape behind the kill loop: ONE
// agent that is simultaneously
//   - pool-shaped, because MinActiveSessions is explicitly set (a *int, so "= 0"
//     is a SET pointer — see config.(*Agent).SupportsInstanceExpansion), and
//   - the backing template of a [[named_session]] that reserves the same
//     qualified identity,
//
// with max_active_sessions = 1 so the pool slot takes the UNSUFFIXED canonical
// identity instead of a "-1" slot name. Agents with max > 1 cannot reach this
// state: "myrig/poller-1" never collides with the named "myrig/poller".
func singletonPoolNamedSessionCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "poller",
			Dir:               "myrig",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			MinActiveSessions: intPtr(0),
		}},
		NamedSessions: []config.NamedSession{
			{Template: "poller", Dir: "myrig"},
		},
	}
}

// TestSyncSessionBeadsKeepsCollapsedSingletonPoolSession is the regression test
// for the 181-second create/kill loop (srvcity sr-jnvmm).
//
// A singleton pool slot runs under the pool runtime session name
// ("<identity>-pool"), but gc collapses its bead onto the canonical identity and
// stamps it configured_named_session=true. The reconfigured-named scan in
// syncSessionBeads then compares that bead's session_name against the NAMED
// session's runtime name, finds the "-pool" suffix it just created, calls the
// bead a reconfigured named session, and KILLS the live provider —
// closeSessionBeadIfRuntimeStoppedAndUnassigned stops the runtime rather than
// merely observing it stopped. The controller recreates the slot under the same
// pool name and the mismatch reappears, so this never converges.
//
// Measured on srvcity: 611 sessions, 508 closed "reconfigured", a hard 181s
// median lifetime every day the agent ran, and ~40% of routed poll steps never
// claimed because no generation survived long enough to claim one.
func TestSyncSessionBeadsKeepsCollapsedSingletonPoolSession(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 10, 1, 6, 40, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := singletonPoolNamedSessionCity()

	identity := "myrig/poller"
	// What poolRuntimeSessionName derives for the slot, and what the live
	// provider actually runs under.
	poolSessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, identity) + "-pool"

	// The bead exactly as srvcity records it AFTER the phantom-identity
	// collapse: canonical alias/agent_name, configured_named_session stamped,
	// pool_managed cleared — but session_name still the pool runtime name.
	bead, err := store.Create(beads.Bead{
		Title:  identity,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:" + identity},
		Metadata: map[string]string{
			"session_name":               poolSessionName,
			"alias":                      identity,
			"agent_name":                 identity,
			"template":                   identity,
			"state":                      "awake",
			"session_origin":             "named",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create collapsed singleton pool bead: %v", err)
	}
	if err := sp.Start(context.Background(), poolSessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start pool runtime: %v", err)
	}

	// The controller does desire this seat — that is why it keeps recreating it
	// — so the desired state carries the slot under its pool runtime name.
	ds := map[string]TemplateParams{
		poolSessionName: {
			TemplateName:            identity,
			InstanceName:            identity,
			Alias:                   identity,
			Command:                 "true",
			ConfiguredNamedIdentity: identity,
			ConfiguredNamedMode:     "on_demand",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("singleton pool session bead was closed as %q; a pool slot running under its own "+
			"pool runtime name is not a reconfigured named session. stderr=%q",
			got.Metadata["close_reason"], stderr.String())
	}
	if !sp.IsRunning(poolSessionName) {
		t.Fatalf("live provider %q was killed; the session was healthy and holds the agent's only "+
			"seat, so killing it is what produces the create/kill loop. stderr=%q",
			poolSessionName, stderr.String())
	}
}
