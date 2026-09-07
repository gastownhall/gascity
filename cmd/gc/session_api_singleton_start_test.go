package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// An explicitly created API session must reach runtime.Start even when its
// template allows only one session and has no automatic pool demand.
func TestAPIRequestedSessionStartsWithoutPoolDemand(t *testing.T) {
	for _, tc := range []struct {
		name                string
		maximum, wantStarts int
		controllerManaged   bool
		existingManual      bool
	}{
		{name: "explicit_singleton", maximum: 1, wantStarts: 1},
		{name: "explicit_multiple", maximum: 3, wantStarts: 1},
		{name: "explicit_singleton_soft_cap", maximum: 1, wantStarts: 1, existingManual: true},
		{name: "controller_singleton_without_demand", maximum: 1, controllerManaged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			cityPath := t.TempDir()
			env.cfg = &config.City{Agents: []config.Agent{{
				Name: "worker", StartCommand: "true", WorkDir: cityPath,
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(tc.maximum),
			}}}
			// This is the persisted shape produced by POST /sessions kind=agent:
			// an explicit alias, ephemeral origin, and a deferred create claim,
			// without the controller's pool_managed/pool_slot markers.
			pending := env.createSessionBead("s-api-request", "worker")
			env.setSessionMetadata(&pending, map[string]string{
				"agent_name": "worker", "alias": "external-conversation",
				"session_origin": "ephemeral", "state": "start-pending",
				"pending_create_claim":      "true",
				"pending_create_started_at": env.clk.Now().Format(time.RFC3339),
			})
			if tc.controllerManaged {
				env.setSessionMetadata(&pending, map[string]string{poolManagedMetadataKey: "true"})
			}
			rows := []beads.Bead{pending}
			if tc.existingManual {
				// Explicit manual sessions may soft-exceed the automation cap;
				// the existing conversation must not be reused or stopped.
				existing := env.createSessionBead("s-existing", "worker")
				env.setSessionMetadata(&existing, map[string]string{
					"agent_name": "worker", "alias": "existing-conversation",
					"session_origin": "manual", "state": "awake",
				})
				if err := env.sp.Start(context.Background(), "s-existing", runtime.Config{Command: "true"}); err != nil {
					t.Fatal(err)
				}
				if err := env.sp.SetMeta("s-existing", "GC_SESSION_ID", existing.ID); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, existing)
			}
			baselineCalls := len(env.sp.SnapshotCalls())
			built := buildDesiredState("test-city", cityPath, env.clk.Now(), env.cfg, env.sp, env.store, &env.stderr)
			env.desiredState = built.State
			env.reconcileWithPoolDesired(rows, map[string]int{"worker": 0})
			starts := 0
			for _, call := range env.sp.SnapshotCalls()[baselineCalls:] {
				if call.Method == "Stop" && call.Name == "s-existing" {
					t.Error("explicit request stopped the existing conversation")
				}
				if call.Method == "Start" {
					starts++
					if call.Name != "s-api-request" {
						t.Errorf("started session %q, want the existing API session", call.Name)
					}
				}
			}
			if starts != tc.wantStarts {
				t.Fatalf("runtime starts = %d, want %d; desired=%v; stderr=%s", starts, tc.wantStarts, mapKeys(built.State), env.stderr.String())
			}
			all, err := env.store.ListByLabel(sessionBeadLabel, 0)
			if err != nil || len(all) != len(rows) {
				t.Fatalf("request must preserve its original session beads: rows=%v, err=%v", all, err)
			}
			originalIDs := make(map[string]bool, len(rows))
			for _, row := range rows {
				originalIDs[row.ID] = true
			}
			for _, row := range all {
				if !originalIDs[row.ID] {
					t.Fatalf("unexpected replacement session bead %q", row.ID)
				}
			}
		})
	}
}
