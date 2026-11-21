package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// seedManualSessionOfPoolTemplate leaves behind what
// `gc session new worker --alias fixer` leaves behind once it has drained: an
// open manual bead of the pool's template, under its own alias, not
// pool-managed.
func seedManualSessionOfPoolTemplate(t testing.TB, store beads.Store, agentName, state, sleepReason string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "fixer",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:fixer"},
		Metadata: map[string]string{
			"template":       "worker",
			"agent_name":     agentName,
			"alias":          "fixer",
			"session_name":   "s-fixer",
			"session_origin": "manual",
			"state":          state,
			"sleep_reason":   sleepReason,
		},
	})
	if err != nil {
		t.Fatalf("seeding manual session of pool template: %v", err)
	}
	return b
}

func canonicalSingletonWorker(t testing.TB) config.Agent {
	t.Helper()
	worker := config.Agent{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)}
	if !worker.UsesCanonicalSingletonPoolIdentity() {
		t.Fatal("precondition: worker uses the canonical singleton pool identity")
	}
	return worker
}

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

// TestCanonicalSingletonPool_GuardedCreateBesideManualSession covers AC-2 of
// issue #8945: with the manual session of the pool's template (agent_name =
// the template, what gc session new stamps on a single-session agent) present
// in the tick's snapshot, the planner claims slot 0 and the guarded create
// mints the seat, deferring its public alias under pool_alias_conflict,
// instead of failing with `canonical pool template "worker" is held by
// session <id>`.
func TestCanonicalSingletonPool_GuardedCreateBesideManualSession(t *testing.T) {
	store := beads.NewMemStore()
	manual := seedManualSessionOfPoolTemplate(t, store, "worker", "asleep", "drained")
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, manual))
	worker := canonicalSingletonWorker(t)
	bp := &agentBuildParams{
		cityPath:     t.TempDir(),
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{worker},
	}
	bp.sessionOccupancyInfos = snapshot.OpenInfos()
	bp.sessionSnapshotCompletenessKnown = true
	bp.sessionSnapshotComplete = true

	_, slot, plan, err := selectOrPlanPoolSessionBead(bp, &worker, "worker", nil, SessionRequest{}, time.Now(), map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("planning a fresh seat beside a manual session of the template: %v", err)
	}
	if plan == nil {
		t.Fatal("expected a fresh-seat plan, got none")
	}
	if slot != 0 || plan.slot != 0 {
		t.Fatalf("planned slot = %d (plan %d), want canonical slot 0", slot, plan.slot)
	}
	if plan.qualifiedInstance != "worker" {
		t.Fatalf("planned qualifiedInstance = %q, want worker", plan.qualifiedInstance)
	}

	seat, err := executePlannedPoolSessionBeadCreate(bp, &worker, "worker", *plan)
	if err != nil {
		t.Fatalf("guarded create beside a manual session of the template: %v", err)
	}
	if seat.ID == "" {
		t.Fatal("guarded create returned no seat")
	}
	if !seat.PoolManaged {
		t.Fatalf("seat %s is not pool-managed", seat.ID)
	}
	// The manual holder keeps agent_name=worker, so the seat's public alias
	// defers under the existing pool_alias_conflict contract: the bead is
	// created without a public alias rather than failing the create.
	if seat.Alias != "" {
		t.Fatalf("seat alias = %q, want deferred while the manual holder keeps agent_name worker", seat.Alias)
	}
}
