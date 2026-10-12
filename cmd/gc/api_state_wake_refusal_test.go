package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestWakeStartRefusal is the one will-not-start predicate (CONTRACT v5.9
// D8 7(a)) the API wake and `gc session wake` share: each refusal arm, and no
// refusal where an override wakes the session (pin_awake on a latched seat,
// assigned work on a latched one, and pool demand, unseen here, on a
// non-interactive latch). A drained pool seat is refused pinned or not; an
// asleep pool seat, pinned or not, is not refused but uncertain (it starts
// only for pool demand). Assigned work parked on a dispatch hold overrides the
// latch only when it is in_progress, a claimed ownership fact that stays
// hold-transparent (ga-5736js); held OPEN work is neither wake readiness nor
// keep-awake work, so the latch still refuses. Kills each arm, a false refusal
// for a session the controller starts, and a will_start claimed for a seat
// waiting on demand.
func TestWakeStartRefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		agent     config.Agent
		rigs      []config.Rig
		info      sessionpkg.Info
		episode   bool
		work      bool
		heldWork  string // status of assigned work parked on hold:external; "" for none
		want      string
		uncertain bool
	}{
		"suspended city":           {agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{Template: "worker"}, want: "city is suspended"},
		"drained pool seat":        {agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{Template: "worker", PoolManaged: true, MetadataState: "drained"}, want: "drained pool seat"},
		"pinned drained pool seat": {agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{Template: "worker", PoolManaged: true, MetadataState: "drained", PinAwake: "true"}, want: "drained pool seat"},
		"asleep pool seat":         {agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{Template: "worker", PoolManaged: true, MetadataState: "asleep"}, uncertain: true},
		"pinned asleep pool seat":  {agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{Template: "worker", PoolManaged: true, MetadataState: "asleep", PinAwake: "true"}, uncertain: true},
		"open circuit":             {agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{ID: "gc-1", Template: "worker", SessionCircuitState: sessionpkg.SessionCircuitStateOpen}, want: "circuit breaker is open"},
		"pinned latch":             {agent: config.Agent{Name: "worker", SleepAfterIdle: "1m"}, info: sessionpkg.Info{Template: "worker", SessionNameMetadata: "worker", SleepReason: "idle", PinAwake: "true"}},
		"latch with work":          {agent: config.Agent{Name: "worker", SleepAfterIdle: "1m"}, info: sessionpkg.Info{ID: "gc-1", Template: "worker", SessionNameMetadata: "worker", SleepReason: "idle"}, work: true},
		"startable":                {agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{Template: "worker"}},
		"no template":              {agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{Template: "gone"}, want: "no configured agent"},
		"dependency-only":          {agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{Template: "worker", DependencyOnly: true}, want: "dependency-only"},
		"suspended rig": {
			agent: config.Agent{Name: "worker", Dir: "frontend"}, rigs: []config.Rig{{Name: "frontend", SuspendedOnStart: true}},
			info: sessionpkg.Info{Template: "frontend/worker"}, want: `rig "frontend" is suspended`,
		},
		"suspended agent": {agent: config.Agent{Name: "worker", Suspended: true}, info: sessionpkg.Info{Template: "worker"}, want: "is suspended"},
		"startup quarantine": {
			agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{Template: "worker", SessionNameMetadata: "worker"},
			episode: true, want: "startup-health quarantine",
		},
		"abandoned create":      {agent: config.Agent{Name: "worker"}, info: sessionpkg.Info{Template: "worker", MetadataState: "creating"}, want: "create never completed"},
		"non-interactive latch": {agent: config.Agent{Name: "worker", SleepAfterIdle: "1m", Attach: boolPtr(false)}, info: sessionpkg.Info{Template: "worker", SessionNameMetadata: "worker", SleepReason: "idle"}},
		"idle latch":            {agent: config.Agent{Name: "worker", SleepAfterIdle: "1m"}, info: sessionpkg.Info{Template: "worker", SessionNameMetadata: "worker", SleepReason: "idle"}, want: "idle sleep policy"},
		"latch with held open work": {
			agent: config.Agent{Name: "worker", SleepAfterIdle: "1m"}, info: sessionpkg.Info{ID: "gc-1", Template: "worker", SessionNameMetadata: "worker", SleepReason: "idle"},
			heldWork: "open", want: "idle sleep policy",
		},
		"latch with held in_progress work": {
			agent: config.Agent{Name: "worker", SleepAfterIdle: "1m"}, info: sessionpkg.Info{ID: "gc-1", Template: "worker", SessionNameMetadata: "worker", SleepReason: "idle"},
			heldWork: "in_progress",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			cs := &controllerState{cityPath: t.TempDir(), cfg: &config.City{Agents: []config.Agent{tc.agent}, Rigs: tc.rigs}, cityBeadStore: store, sp: runtime.NewFake()}
			if name == "suspended city" {
				cs.cfg.Workspace.Suspended = true
			}
			if tc.work {
				if _, err := store.Create(beads.Bead{Title: "task", Type: "task", Status: "in_progress", Assignee: "gc-1"}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.heldWork != "" {
				held, err := store.Create(beads.Bead{Title: "parked", Type: "task", Assignee: "gc-1", Labels: []string{beadmeta.HoldExternalLabel}})
				if err != nil {
					t.Fatal(err)
				}
				// MemStore.Create always opens a bead, so the status under test is set after.
				if err := store.Update(held.ID, beads.UpdateOpts{Status: &tc.heldWork}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.episode {
				if err := sessionpkg.NewStore(beads.SessionStore{Store: store}).SaveStartupHealthEpisode(sessionpkg.StartupHealthEpisode{
					SessionName: "worker", QuarantinedUntil: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.info.SleepReason == "idle" {
				// Slept by this very policy: legacy's idle latch.
				tc.info.SleepPolicyFingerprint = resolveSessionSleepPolicyInfo(tc.info, cs.cfg, cs.sp).Fingerprint
			}
			got, certain := cs.WakeStartRefusal(tc.info)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("WakeStartRefusal = %q, want %q", got, tc.want)
			}
			if certain == tc.uncertain {
				t.Fatalf("WakeStartRefusal certain = %v, want %v", certain, !tc.uncertain)
			}
		})
	}
}

// The API wake reads assigned work through the city work store, not the
// sessions binding it resolves sessions through: on a split city, work on the
// work store overrides the idle latch.
func TestWakeStartRefusalReadsTheWorkStoreOnASplitCity(t *testing.T) {
	work, binding := beads.NewMemStore(), beads.NewMemStore()
	agent := config.Agent{Name: "worker", SleepAfterIdle: "1m"}
	cs := &controllerState{cityPath: t.TempDir(), cfg: &config.City{Agents: []config.Agent{agent}}, cityBeadStore: work, storageRoutes: splitRoutes(binding), sp: runtime.NewFake()}
	if cs.SessionsBeadStore().Store != binding {
		t.Fatal("fixture: the sessions store is not the binding")
	}
	if _, err := work.Create(beads.Bead{Title: "task", Type: "task", Status: "in_progress", Assignee: "gc-1"}); err != nil {
		t.Fatal(err)
	}
	info := sessionpkg.Info{ID: "gc-1", Template: "worker", SessionNameMetadata: "worker", SleepReason: "idle"}
	info.SleepPolicyFingerprint = resolveSessionSleepPolicyInfo(info, cs.cfg, cs.sp).Fingerprint
	if got, _ := cs.WakeStartRefusal(info); got != "" {
		t.Fatalf("WakeStartRefusal = %q, want none: the work store's claim overrides the latch", got)
	}
}
