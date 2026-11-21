package main

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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

func TestCanonicalSingletonPool_GuardedCreateBesideManualSession(t *testing.T) {
	store := beads.NewMemStore()
	manual := createManualTemplateHolderBead(t, store, "worker", manualTemplateHolderMetadata("worker", "asleep"))
	worker := canonicalSingletonWorker(t)
	bp := canonicalSingletonBuildParams(t, store, worker, []beads.Bead{manual})

	usedSlots := map[int]bool{}
	_, slot, plan, err := selectOrPlanPoolSessionBead(bp, &worker, "worker", nil,
		SessionRequest{Template: "worker", Tier: "new"}, time.Time{}, map[string]bool{}, usedSlots)
	if err != nil {
		t.Fatalf("selectOrPlanPoolSessionBead: %v", err)
	}
	if plan == nil {
		t.Fatal("selectOrPlanPoolSessionBead returned no fresh-seat plan")
	}
	if slot != 0 || !usedSlots[0] {
		t.Fatalf("planned slot = %d (used %#v), want canonical slot 0", slot, usedSlots)
	}

	info, err := createPoolSessionBeadWithGuardedAliasUsingLock(poolCreateViewOf(bp), &worker, "worker",
		plan.qualifiedInstance, plan.poolSlot, nil,
		func(_ string, _ []string, fn func() error) error { return fn() })
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithGuardedAliasUsingLock: %v", err)
	}
	seat := assertCanonicalWorkerSeat(t, store, info.ID)
	// The guarded create takes the proven-collision branch of the alias check:
	// the seat's public alias is deferred (left empty) for the
	// pool_alias_conflict bookkeeping, not failed closed.
	aliasErr := sessionpkg.EnsureAliasAvailableWithConfig(beadstest.Sessions(store), nil, "worker", seat.ID)
	if !errors.Is(aliasErr, sessionpkg.ErrSessionAliasExists) {
		t.Fatalf("alias %q availability beside manual agent_name=worker = %v, want %v", "worker", aliasErr, sessionpkg.ErrSessionAliasExists)
	}
	if alias := seat.Metadata["alias"]; alias != "" {
		t.Fatalf("seat alias = %q, want deferred (empty) beside manual agent_name=worker", alias)
	}

	bp.sessionBeads = newSessionBeadSnapshot([]beads.Bead{manual, seat})
	selected, _, nextPlan, err := selectOrPlanPoolSessionBead(bp, &worker, "worker", nil,
		SessionRequest{Template: "worker", Tier: "new"}, time.Time{}, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrPlanPoolSessionBead(next tick): %v", err)
	}
	if nextPlan != nil {
		t.Fatal("next tick planned a second fresh seat")
	}
	if selected.ID != seat.ID {
		t.Fatalf("next tick selected %q, want seat %q", selected.ID, seat.ID)
	}
}

func TestCanonicalSingletonPool_ActiveManualSessionOfTemplateDoesNotHoldSeat(t *testing.T) {
	for _, agentName := range []string{"fixer", "worker"} {
		for _, populated := range []bool{false, true} {
			name := "agent_name_" + agentName + "/empty_snapshot"
			if populated {
				name = "agent_name_" + agentName + "/populated_snapshot"
			}
			t.Run(name, func(t *testing.T) {
				store := beads.NewMemStore()
				manual := createManualTemplateHolderBead(t, store, "worker", manualTemplateHolderMetadata(agentName, "active"))
				var snapshot []beads.Bead
				if populated {
					snapshot = []beads.Bead{manual}
				}
				worker := canonicalSingletonWorker(t)
				bp := canonicalSingletonBuildParams(t, store, worker, snapshot)
				info, err := selectOrCreateDependencyPoolSessionBead(bp, &worker, "worker")
				if err != nil {
					t.Fatalf("selectOrCreateDependencyPoolSessionBead beside active manual session: %v", err)
				}
				assertCanonicalWorkerSeat(t, store, info.ID)
			})
		}
	}
}

func TestCanonicalSingletonPool_SeatBesideManualSessionLeavesManualBeadUnchanged(t *testing.T) {
	for _, agentName := range []string{"fixer", "worker"} {
		t.Run("agent_name_"+agentName, func(t *testing.T) {
			store := beads.NewMemStore()
			manual := createManualTemplateHolderBead(t, store, "worker", manualTemplateHolderMetadata(agentName, "asleep"))
			worker := canonicalSingletonWorker(t)
			bp := canonicalSingletonBuildParams(t, store, worker, nil)
			info, err := selectOrCreateDependencyPoolSessionBead(bp, &worker, "worker")
			if err != nil {
				t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
			}
			seat := assertCanonicalWorkerSeat(t, store, info.ID)
			if got, want := seat.Metadata["session_name"], PoolSessionName("worker", seat.ID); got != want {
				t.Fatalf("seat session_name = %q, want bead-scoped %q", got, want)
			}

			after, err := store.Get(manual.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != manual.Status {
				t.Fatalf("manual status = %q, want unchanged %q", after.Status, manual.Status)
			}
			if !maps.Equal(after.Metadata, manual.Metadata) {
				t.Fatalf("manual metadata changed:\n got %#v\nwant %#v", after.Metadata, manual.Metadata)
			}
		})
	}
}
