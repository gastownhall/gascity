package main

import (
	"io"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The closed named-session index reads the sessions store (S3c, site 5).
//
// An on_demand named session's closed bead tells the demand pass that the
// session's work is filed under its runtime name, so that name is probed too
// (session.BuildClosedNamedSessionBeadIndex). The pass built the index from its
// LEADING store. Every controller and gc start pass leads with the sessions
// store, but gc doctor's demand-migration sweep leads with the city WORK store,
// so on a split city it listed session beads in the work store, which holds
// none, and the index came back empty.
//
// The split leg is a real converged one-shot city (file work store, SQLite
// binding); the control is the same city with no [storage] section, where the
// one store is the sessions store and the index is read from it.

// sessionListSpy records the closed-session listings the index makes.
type sessionListSpy struct {
	beads.Store
	mu    sync.Mutex
	lists int
}

func (s *sessionListSpy) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.IncludeClosed && (q.Type == session.BeadType || q.Label == session.LabelSession) {
		s.mu.Lock()
		s.lists++
		s.mu.Unlock()
	}
	return s.Store.List(q)
}

func (s *sessionListSpy) sessionLists() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

func TestDoctorDemandSweepReadsClosedNamedSessionsFromTheSessionsStore(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "single-store"
		if split {
			name = "split"
		}
		t.Run(name, func(t *testing.T) {
			var cityPath string
			if split {
				cityPath, _ = migratedOneShotCLICity(t)
			} else {
				cityPath = oneShotCLICity(t, "")
			}
			captureCLIStorageStderr(t)
			cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Agents = append(cfg.Agents, config.Agent{Name: "probe", StartCommand: "true"})
			cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Template: "probe", Mode: "on_demand"})
			work, err := openCityStoreAt(cityPath)
			if err != nil {
				t.Fatalf("opening the city work store: %v", err)
			}
			t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
			spy := &sessionListSpy{Store: work}
			c := newV2DemandMigrationsCheck(cfg, cityPath, func(path string) (beads.Store, error) {
				if path == cityPath {
					return spy, nil
				}
				return openCityStoreAt(path)
			})

			if sweep := c.sweep(false, io.Discard); len(sweep.problems) != 0 {
				t.Fatalf("sweep problems = %q, want none", sweep.problems)
			}
			got := spy.sessionLists()
			switch {
			case split && got != 0:
				t.Fatalf("the sweep listed closed session beads in the city work store %d time(s); on a split city they live in the binding", got)
			case !split && got == 0:
				t.Fatal("the sweep never listed closed session beads in the one store; the closed named-session index was not read")
			}
		})
	}
}

// The index the sweep names is the one the demand pass uses: an on_demand
// named session whose closed bead is in the sessions store has its runtime
// name probed, so ready work filed under that name is collected (#5231's
// assignee form). Without the sessions store named, a split city's pass
// builds the index from the work store and never probes the runtime name.
func TestClosedNamedIndexFromTheSessionsStoreProbesTheRuntimeName(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		cfg := e.cfg
		// A session template makes the runtime name differ from the identity,
		// so only the index can name it.
		cfg.Workspace.SessionTemplate = "{{.City}}-{{.Agent}}"
		cfg.Agents = append(cfg.Agents, config.Agent{Name: "probe", StartCommand: "true"})
		cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Template: "probe", Mode: "on_demand"})
		identity := cfg.NamedSessions[len(cfg.NamedSessions)-1].QualifiedName()
		runtimeName := config.NamedSessionRuntimeName(config.EffectiveCityName(cfg, ""), cfg.Workspace, identity)
		if runtimeName == identity {
			t.Fatalf("premise: runtime name %q equals the identity", runtimeName)
		}
		closed, err := e.sessionsStore().Create(beads.Bead{
			Title: "probe", Type: session.BeadType, Labels: []string{session.LabelSession},
			Metadata: map[string]string{session.NamedSessionIdentityMetadata: identity, "session_name": runtimeName, "state": "asleep"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.sessionsStore().Close(closed.ID); err != nil {
			t.Fatal(err)
		}
		work, err := e.work.Create(beads.Bead{Title: "work for the named session", Type: "task", Assignee: runtimeName})
		if err != nil {
			t.Fatal(err)
		}
		collected := func(cache *readyDemandCache) bool {
			got, _, _, _, _ := collectAssignedWorkBeadsWithStores(e.cityPath, cfg, e.work, nil, nil, nil, cache)
			for _, b := range got {
				if b.ID == work.ID {
					return true
				}
			}
			return false
		}
		if !collected(newReadyDemandCache().withSessionsStore(beads.SessionStore{Store: e.sessionsStore()})) {
			t.Fatalf("ready work %s assigned to the runtime name %q was not collected with the sessions store named", work.ID, runtimeName)
		}
		if e.split && collected(newReadyDemandCache()) {
			t.Fatalf("premise: a split city's work-led pass collected %s without the sessions store named", work.ID)
		}
	})
}
