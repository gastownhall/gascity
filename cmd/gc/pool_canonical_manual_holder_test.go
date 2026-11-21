package main

import (
	"errors"
	"reflect"
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

// TestCanonicalSingletonPool_ManualSessionOfTemplateDoesNotHoldSeat is the
// issue #8945 matrix: an open manual session of a canonical singleton pool's
// template never holds the pool's seat, whatever its agent_name shape
// (its own alias or the template), whether the tick's snapshot sees it or not,
// and whether it is asleep-drained or awake. The release depends on origin,
// not sleep state. The controls keep the legitimate blocks and frees.
func TestCanonicalSingletonPool_ManualSessionOfTemplateDoesNotHoldSeat(t *testing.T) {
	cases := []struct {
		name          string
		seed          bool // seed a manual session bead of the pool's template
		agentName     string
		state         string
		sleepReason   string
		inSnapshot    bool
		closeBead     bool
		otherTemplate bool
	}{
		{name: "asleep_drained_agent_alias", seed: true, agentName: "fixer", state: "asleep", sleepReason: "drained"},
		{name: "asleep_drained_agent_template", seed: true, agentName: "worker", state: "asleep", sleepReason: "drained"},
		{name: "asleep_drained_agent_template_in_snapshot", seed: true, agentName: "worker", state: "asleep", sleepReason: "drained", inSnapshot: true},
		{name: "awake_agent_alias", seed: true, agentName: "fixer", state: "active"},
		{name: "awake_agent_alias_in_snapshot", seed: true, agentName: "fixer", state: "active", inSnapshot: true},
		{name: "awake_agent_template", seed: true, agentName: "worker", state: "active"},
		{name: "awake_agent_template_in_snapshot", seed: true, agentName: "worker", state: "active", inSnapshot: true},
		{name: "control_manual_session_closed", seed: true, agentName: "fixer", state: "asleep", sleepReason: "drained", closeBead: true},
		{name: "control_manual_session_of_other_template", seed: true, agentName: "fixer", state: "asleep", sleepReason: "drained", otherTemplate: true},
		{name: "control_no_manual_session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			var manualBefore *beads.Bead
			if tc.seed {
				b := seedManualSessionOfPoolTemplate(t, store, tc.agentName, tc.state, tc.sleepReason)
				if tc.otherTemplate {
					if err := store.Update(b.ID, beads.UpdateOpts{Metadata: map[string]string{"template": "other"}}); err != nil {
						t.Fatalf("retargeting manual session template: %v", err)
					}
				}
				if tc.closeBead {
					if err := store.Close(b.ID); err != nil {
						t.Fatalf("closing manual session: %v", err)
					}
				}
				fresh, err := store.Get(b.ID)
				if err != nil {
					t.Fatalf("reading the seeded manual bead: %v", err)
				}
				manualBefore = &fresh
			}
			worker := canonicalSingletonWorker(t)
			snapshot := &sessionBeadSnapshot{}
			if tc.inSnapshot {
				for _, b := range sessionBeadsInStore(t, store) {
					snapshot.addInfo(sessiontest.SeedBead(t, b))
				}
			}
			bp := &agentBuildParams{
				cityPath:     t.TempDir(),
				beadStore:    store,
				sessionBeads: snapshot,
				agents:       []config.Agent{worker},
			}
			if tc.inSnapshot {
				bp.sessionOccupancyInfos = snapshot.OpenInfos()
				bp.sessionSnapshotCompletenessKnown = true
				bp.sessionSnapshotComplete = true
			}

			seat, err := selectOrCreateDependencyPoolSessionBead(bp, &worker, "worker")
			if err != nil {
				t.Fatalf("pool worker cannot start beside its manual session: %v", err)
			}
			if seat.ID == "" {
				t.Fatal("pool worker created no seat")
			}
			if !seat.PoolManaged {
				t.Fatalf("seat %s is not pool-managed", seat.ID)
			}
			if seat.Template != "worker" || seat.AgentName != "worker" {
				t.Fatalf("seat template/agent_name = %q/%q, want worker/worker", seat.Template, seat.AgentName)
			}
			// The seat's runtime session_name is bead-scoped, never the bare
			// template identity the manual holder shares.
			if want := PoolSessionName("worker", seat.ID); seat.SessionNameMetadata != want {
				t.Fatalf("seat session_name = %q, want bead-scoped %q", seat.SessionNameMetadata, want)
			}
			// The manual bead is never written: not woken, adopted or closed.
			if manualBefore != nil {
				after, err := store.Get(manualBefore.ID)
				if err != nil {
					t.Fatalf("re-reading the manual bead: %v", err)
				}
				if !reflect.DeepEqual(*manualBefore, after) {
					t.Fatalf("manual bead changed:\nbefore: %#v\nafter:  %#v", *manualBefore, after)
				}
			}
		})
	}
}

// TestCanonicalSingletonPool_SeatBesideManualSessionIsReusedNextTick covers
// the convergence half of issue #8945: the seat minted beside a manual
// session of the template is REUSED by the next tick's pool demand (the
// request path), not duplicated — and never by adopting the manual bead.
func TestCanonicalSingletonPool_SeatBesideManualSessionIsReusedNextTick(t *testing.T) {
	store := beads.NewMemStore()
	seedManualSessionOfPoolTemplate(t, store, "worker", "asleep", "drained")
	worker := canonicalSingletonWorker(t)

	// Tick 1: dependency demand mints the seat before the tick's snapshot
	// sees the manual row.
	bp1 := &agentBuildParams{
		cityPath:     t.TempDir(),
		beadStore:    store,
		sessionBeads: &sessionBeadSnapshot{},
		agents:       []config.Agent{worker},
	}
	seat, err := selectOrCreateDependencyPoolSessionBead(bp1, &worker, "worker")
	if err != nil {
		t.Fatalf("tick 1 cannot mint the pool seat beside the manual session: %v", err)
	}
	if seat.ID == "" || !seat.PoolManaged {
		t.Fatalf("tick 1 minted no pool-managed seat: %+v", seat)
	}

	// Tick 2: the snapshot now sees both the manual row and the seat. A pool
	// request must reuse the seat (canonical reuse tier), not plan a second
	// one and not adopt the manual bead.
	snapshot := &sessionBeadSnapshot{}
	for _, b := range sessionBeadsInStore(t, store) {
		snapshot.addInfo(sessiontest.SeedBead(t, b))
	}
	bp2 := &agentBuildParams{
		cityPath:     t.TempDir(),
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{worker},
	}
	bp2.sessionOccupancyInfos = snapshot.OpenInfos()
	bp2.sessionSnapshotCompletenessKnown = true
	bp2.sessionSnapshotComplete = true

	got, slot, plan, err := selectOrPlanPoolSessionBead(bp2, &worker, "worker", nil, SessionRequest{}, time.Now(), map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("tick 2 cannot satisfy pool demand beside the manual session: %v", err)
	}
	if plan != nil {
		t.Fatalf("tick 2 planned a second seat (%q, slot %d) instead of reusing %s", plan.qualifiedInstance, plan.slot, seat.ID)
	}
	if got.ID != seat.ID {
		t.Fatalf("tick 2 reused %q, want the minted seat %s", got.ID, seat.ID)
	}
	if !got.PoolManaged {
		t.Fatalf("tick 2 returned a non-pool-managed row: %+v", got)
	}
	if slot != 0 {
		t.Fatalf("tick 2 slot = %d, want canonical slot 0", slot)
	}
	pooled := 0
	for _, b := range sessionBeadsInStore(t, store) {
		if b.Metadata["template"] == "worker" && b.Metadata[poolManagedMetadataKey] == boolMetadata(true) {
			pooled++
		}
	}
	if pooled != 1 {
		t.Fatalf("store holds %d pool-managed worker beads, want exactly the one seat", pooled)
	}
}

// sessionBeadsInStore lists the store's session beads for snapshot seeding.
func sessionBeadsInStore(t testing.TB, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	return append(make([]beads.Bead, 0, len(rows)), rows...)
}
