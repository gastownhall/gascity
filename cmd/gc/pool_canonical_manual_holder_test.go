package main

import (
	"errors"
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Issue #8945: a manual session started from a canonical singleton pool's
// template holds that pool's bare identity only through a name it explicitly
// claimed (alias, session_name), never through its template-derived
// identifiers.

func manualTemplateHolderMetadata(agentName, state string) map[string]string {
	metadata := map[string]string{
		"template":       "worker",
		"agent_name":     agentName,
		"alias":          "fixer",
		"session_name":   "s-fixer",
		"session_origin": "manual",
		"state":          state,
	}
	if state == "asleep" {
		metadata["sleep_reason"] = "drained"
	}
	return metadata
}

func createManualTemplateHolderBead(t *testing.T, store beads.Store, template string, metadata map[string]string) beads.Bead {
	t.Helper()
	metadata = maps.Clone(metadata)
	metadata["template"] = template
	b, err := store.Create(beads.Bead{
		Title:    "fixer",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "agent:fixer"},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("creating manual holder: %v", err)
	}
	return b
}

func canonicalSingletonWorker(t *testing.T) config.Agent {
	t.Helper()
	worker := config.Agent{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)}
	if !worker.UsesCanonicalSingletonPoolIdentity() {
		t.Fatal("precondition: worker uses the canonical singleton pool identity")
	}
	return worker
}

func canonicalSingletonBuildParams(t *testing.T, store beads.Store, worker config.Agent, snapshot []beads.Bead) *agentBuildParams {
	t.Helper()
	return &agentBuildParams{
		cityPath:     t.TempDir(),
		beadStore:    store,
		sessionBeads: newSessionBeadSnapshot(snapshot),
		agents:       []config.Agent{worker},
	}
}

func assertCanonicalWorkerSeat(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	if id == "" {
		t.Fatal("pool create returned no seat bead")
	}
	seat, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(seat %s): %v", id, err)
	}
	for key, want := range map[string]string{
		"pool_managed": "true",
		"template":     "worker",
		"agent_name":   "worker",
	} {
		if got := seat.Metadata[key]; got != want {
			t.Fatalf("seat %s = %q, want %q", key, got, want)
		}
	}
	return seat
}

// An asleep, drained manual session started from a singleton pool's template
// must not keep the pool from minting its session (issue #8945).
func TestAsleepManualTemplateSessionDoesNotBlockSingletonPool(t *testing.T) {
	for _, agentName := range []string{"fixer", "worker"} {
		t.Run("agent_name_"+agentName, func(t *testing.T) {
			store := beads.NewMemStore()
			createManualTemplateHolderBead(t, store, "worker", manualTemplateHolderMetadata(agentName, "asleep"))
			worker := canonicalSingletonWorker(t)
			bp := &agentBuildParams{
				cityPath:     t.TempDir(),
				beadStore:    store,
				sessionBeads: &sessionBeadSnapshot{},
				agents:       []config.Agent{worker},
			}
			_, err := selectOrCreateDependencyPoolSessionBead(bp, &worker, "worker")
			if errors.Is(err, errPoolSessionNameUnavailable) {
				t.Fatalf("singleton pool %q blocked by asleep manual session: %v", "worker", err)
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCanonicalSingletonPool_ManualSessionOfTemplateDoesNotHoldSeat(t *testing.T) {
	cases := []struct {
		name      string
		template  string // template of the manual bead; "" means no manual bead
		agentName string
		closed    bool
	}{
		{name: "asleep_manual_agent_name_alias", template: "worker", agentName: "fixer"},
		{name: "asleep_manual_agent_name_template", template: "worker", agentName: "worker"},
		{name: "control_closed_manual", template: "worker", agentName: "worker", closed: true},
		{name: "control_manual_of_other_template", template: "other", agentName: "fixer"},
		{name: "control_no_manual"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			if tc.template != "" {
				manual := createManualTemplateHolderBead(t, store, tc.template, manualTemplateHolderMetadata(tc.agentName, "asleep"))
				if tc.closed {
					if err := store.Close(manual.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			worker := canonicalSingletonWorker(t)
			bp := canonicalSingletonBuildParams(t, store, worker, nil)
			info, err := selectOrCreateDependencyPoolSessionBead(bp, &worker, "worker")
			if err != nil {
				t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
			}
			assertCanonicalWorkerSeat(t, store, info.ID)
		})
	}
}
