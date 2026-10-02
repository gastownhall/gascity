package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// A named session whose desired bead is missing may coexist with an adopted
// canonical-singleton pool bead holding work. The pool bead's step-aside name
// is not a reconfiguration conflict, including when its suffix is shortened.
func TestSyncSessionBeadsAdoptedPoolStepAsideDoesNotBlockNamedCreation(t *testing.T) {
	const longAgent = "deployer-with-a-deliberately-long-name-to-hit-the-cap"
	for _, tc := range []struct {
		name       string
		agent      string
		conflict   bool
		wantCreate bool
	}{
		{name: "short pool step-aside", agent: "deployer", wantCreate: true},
		{name: "bounded pool step-aside", agent: longAgent, wantCreate: true},
		{name: "unrelated named runtime", agent: "deployer", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := "gascity/" + tc.agent
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents: []config.Agent{{
					Name: tc.agent, Dir: "gascity", StartCommand: "true", MaxActiveSessions: intPtr(1),
				}},
				NamedSessions: []config.NamedSession{{
					Template: tc.agent, Dir: "gascity", Mode: "on_demand",
				}},
			}
			spec, ok := findNamedSessionSpec(cfg, "test-city", identity)
			if !ok {
				t.Fatalf("configured named session %q not found", identity)
			}
			poolName := poolRuntimeSessionName(cfg, identity, identity, false)
			if poolName == spec.SessionName {
				t.Fatalf("pool runtime did not step aside from named runtime %q", spec.SessionName)
			}
			if tc.agent == longAgent {
				if len(spec.SessionName+poolRuntimeNameSuffix) <= session.MaxExplicitSessionNameLen ||
					poolName == spec.SessionName+poolRuntimeNameSuffix {
					t.Fatalf("precondition: %q must have a shortened pool suffix", spec.SessionName)
				}
			}
			oldName := poolName
			if tc.conflict {
				oldName = "unrelated-named-runtime"
			}

			store := beads.NewMemStore()
			old, err := store.Create(beads.Bead{
				Title: identity, Type: sessionBeadType, Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"session_name": oldName, "alias": identity, "template": identity,
					"agent_name": identity, "state": "asleep", "session_origin": "named",
					namedSessionMetadataKey: "true", namedSessionIdentityMetadata: identity,
					namedSessionModeMetadata: "on_demand",
				},
			})
			if err != nil {
				t.Fatalf("creating adopted session bead: %v", err)
			}
			work, err := store.Create(beads.Bead{Title: "assigned work", Type: "task", Assignee: identity})
			if err != nil {
				t.Fatalf("creating assigned work: %v", err)
			}
			inProgress := "in_progress"
			if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
				t.Fatalf("claiming assigned work: %v", err)
			}

			// The desired name is absent from the store. That makes this sync
			// observe the reconfiguration gate's creation decision rather than
			// refreshing the already-present adopted bead. Leave Alias unset on
			// this proposed bead because the adopted bead still owns that alias;
			// alias uniqueness is a separate gate from this creation decision.
			ds := map[string]TemplateParams{spec.SessionName: {
				SessionName: spec.SessionName, TemplateName: identity, InstanceName: identity,
				Command: "true", ConfiguredNamedIdentity: identity,
				ConfiguredNamedMode: "on_demand",
			}}
			var stderr bytes.Buffer
			clk := &clock.Fake{Time: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
			syncSessionBeads("", store, ds, runtime.NewFake(), allConfiguredDS(ds), cfg, clk, &stderr, false)

			kept, err := store.Get(old.ID)
			if err != nil {
				t.Fatalf("reading adopted bead: %v", err)
			}
			if kept.Status != "open" {
				t.Fatalf("adopted bead status = %q, want open while it holds work", kept.Status)
			}
			var created bool
			for _, b := range allSessionBeads(t, store) {
				if b.ID != old.ID && b.Status == "open" && b.Metadata["session_name"] == spec.SessionName {
					created = true
				}
			}
			if created != tc.wantCreate {
				t.Fatalf("created desired named bead = %v, want %v (old name %q, desired %q, sync log %q)",
					created, tc.wantCreate, oldName, spec.SessionName, stderr.String())
			}
		})
	}
}
