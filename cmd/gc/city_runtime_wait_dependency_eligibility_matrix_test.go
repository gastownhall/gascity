package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestSessionWaitDependencyPrePokeUnsupportedEligibilityFallsBackToLegacy
// protects the deliberately narrow exact-start cohort. Every row here is a
// valid legacy dependency wait, but is outside the keyed ordinary-session
// contract. It must stay free of keyed ownership and still
// be advanced by the existing legacy wait preparation in both rollout modes.
func TestSessionWaitDependencyPrePokeUnsupportedEligibilityFallsBackToLegacy(t *testing.T) {
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		for _, test := range []struct {
			name            string
			sessionMetadata map[string]string
			agentDependsOn  []string
			depMode         string
			dependencyCount int
			namedSpec       bool
		}{
			{name: "named-mismatched", sessionMetadata: map[string]string{"configured_named_session": "true", "configured_named_identity": "worker", "configured_named_mode": "always", "session_name": "stale-worker"}, dependencyCount: 1, depMode: "all", namedSpec: true},
			{name: "pool-managed", sessionMetadata: map[string]string{"pool_managed": "true"}, dependencyCount: 1, depMode: "all"},
			{name: "dependency-only", sessionMetadata: map[string]string{"dependency_only": "true"}, dependencyCount: 1, depMode: "all"},
			{name: "configured-agent-depends-on", agentDependsOn: []string{"database"}, dependencyCount: 1, depMode: "all"},
		} {
			t.Run(string(mode)+"/"+test.name, func(t *testing.T) {
				env := newReconcilerTestEnv()
				env.cfg = &config.City{
					Workspace: config.Workspace{Name: "test-city"},
					Agents: []config.Agent{
						{Name: "database", StartCommand: "true"},
						{Name: "worker", StartCommand: "true", DependsOn: test.agentDependsOn},
					},
				}
				if test.namedSpec {
					env.cfg.NamedSessions = []config.NamedSession{{Template: "worker", Mode: "always"}}
				}

				dependencies := make([]beads.Bead, 0, test.dependencyCount)
				for range test.dependencyCount {
					dependency, err := env.store.Create(beads.Bead{Title: "dependency"})
					if err != nil {
						t.Fatal(err)
					}
					if err := env.store.Close(dependency.ID); err != nil {
						t.Fatal(err)
					}
					dependencies = append(dependencies, dependency)
				}

				target := env.createSessionBead("worker", "worker")
				env.setSessionMetadata(&target, map[string]string{
					"state":              string(sessionpkg.StateAsleep),
					"continuation_epoch": "7",
					"wait_hold":          "true",
					"sleep_intent":       string(sessionpkg.SleepReasonWaitHold),
					"sleep_reason":       string(sessionpkg.SleepReasonWaitHold),
				})
				env.setSessionMetadata(&target, test.sessionMetadata)

				dependencyIDs := make([]string, 0, len(dependencies))
				for _, dependency := range dependencies {
					dependencyIDs = append(dependencyIDs, dependency.ID)
				}
				wait, err := env.store.Create(sessionWaitShadowBead(target.ID, dependencyIDs[0]))
				if err != nil {
					t.Fatal(err)
				}
				if err := env.store.SetMetadata(wait.ID, "dep_ids", strings.Join(dependencyIDs, ",")); err != nil {
					t.Fatal(err)
				}
				if err := env.store.SetMetadata(wait.ID, "dep_mode", test.depMode); err != nil {
					t.Fatal(err)
				}
				if err := env.store.SetMetadata(wait.ID, "registered_epoch", "7"); err != nil {
					t.Fatal(err)
				}
				beforeWait, err := env.store.Get(wait.ID)
				if err != nil {
					t.Fatal(err)
				}
				beforeTarget, err := env.store.Get(target.ID)
				if err != nil {
					t.Fatal(err)
				}

				cs := &controllerState{
					cfg:                         env.cfg,
					sp:                          env.sp,
					cityPath:                    t.TempDir(),
					cityBeadStore:               env.store,
					eventProv:                   events.NewFake(),
					rolloutFlags:                rollout.ForTest(rollout.WithSessionReconciler(mode)),
					sessionStartGeneration:      1,
					sessionStartStoreGeneration: 1,
				}
				cr := &CityRuntime{
					cs:                     cs,
					cfg:                    env.cfg,
					sessionStartOwnership:  sessionStartOwnershipKeyed,
					sessionStartMode:       mode,
					sessionStartController: &sessionStartController{},
				}
				cr.sessionWaitDependencyIndex = newSessionWaitDependencyIndex()
				if err := cr.sessionWaitDependencyIndex.Rebuild([]sessionpkg.WaitInfo{{
					ID: wait.ID, SessionID: target.ID, Kind: "deps", Status: "open",
					State: waitStatePending, DepIDs: dependencyIDs, DepMode: test.depMode,
				}}); err != nil {
					t.Fatal(err)
				}
				cr.sessionWaitDependencyIndexGeneration = 1

				cr.reserveSessionWaitDependencyTargets(t.Context(), dependencyIDs[0])
				waitInfo, err := sessionFrontDoor(env.store).GetWait(wait.ID)
				if err != nil {
					t.Fatal(err)
				}
				if cr.ownsReservedSessionWaitDependencyStart(target.ID) || cr.ownsReservedSessionWaitDependencyWait(waitInfo) {
					t.Fatal("unsupported wait acquired a pre-poke reservation")
				}
				if cr.ownsSessionWaitDependencyStart(target.ID) || cr.ownsSessionWaitDependencyWait(waitInfo) {
					t.Fatal("unsupported wait acquired keyed controller ownership")
				}
				if got := env.sp.CountCalls("Start", "worker"); got != 0 {
					t.Fatalf("provider Starts = %d, want 0", got)
				}
				afterReservationWait, err := env.store.Get(wait.ID)
				if err != nil {
					t.Fatal(err)
				}
				afterReservationTarget, err := env.store.Get(target.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(afterReservationWait, beforeWait) || !reflect.DeepEqual(afterReservationTarget, beforeTarget) {
					t.Fatal("unsupported wait reservation path mutated durable wait or session state")
				}

				ready, err := prepareWaitWakeStateWithSnapshot(
					sessionFrontDoor(env.store),
					newWaitDependencyStoreSet(env.store, nil, beads.GraphStore{}),
					beads.NudgesStore{Store: env.store},
					env.clk.Now(),
					nil,
					cr.ownsSessionWaitDependencyWait,
				)
				if err != nil {
					t.Fatalf("prepare legacy wait state: %v", err)
				}
				if !ready[target.ID] {
					t.Fatal("legacy wait preparation did not select the unsupported session")
				}
				storedWait, err := sessionFrontDoor(env.store).GetWait(wait.ID)
				if err != nil {
					t.Fatal(err)
				}
				if storedWait.State != waitStateReady {
					t.Fatalf("legacy durable wait state = %q, want %q", storedWait.State, waitStateReady)
				}
				if got := env.sp.CountCalls("Start", "worker"); got != 0 {
					t.Fatalf("legacy wait preparation provider Starts = %d, want 0", got)
				}
			})
		}
	}
}
