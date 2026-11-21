package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestAsleepManualTemplateSessionDoesNotBlockSingletonPool reproduces issue
// #8945: an asleep, drained manual session started from a singleton pool's
// template must not keep the pool from minting its session.
func TestAsleepManualTemplateSessionDoesNotBlockSingletonPool(t *testing.T) {
	for _, agentName := range []string{"fixer", "worker"} {
		t.Run("agent_name_"+agentName, func(t *testing.T) {
			store := beads.NewMemStore()
			if _, err := store.Create(beads.Bead{
				Title:  "fixer",
				Type:   sessionBeadType,
				Labels: []string{sessionBeadLabel, "agent:fixer"},
				Metadata: map[string]string{
					"template":       "worker",
					"agent_name":     agentName,
					"alias":          "fixer",
					"session_name":   "s-fixer",
					"session_origin": "manual",
					"state":          "asleep",
					"sleep_reason":   "drained",
				},
			}); err != nil {
				t.Fatal(err)
			}
			worker := config.Agent{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)}
			if !worker.UsesCanonicalSingletonPoolIdentity() {
				t.Fatal("precondition: worker uses the canonical singleton pool identity")
			}
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
