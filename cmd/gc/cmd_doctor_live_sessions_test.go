package main

import (
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// doctorSessionCity prepares a city with one pool agent, worker, and two of its
// session rows written through the session-class store: one open, one closed.
// split puts the sessions class on a SQLite binding (a born-split city);
// otherwise the city keeps its sessions in its own store. It returns the open
// row's runtime session name, which the runtime derives from the row's bead id.
func doctorSessionCity(t *testing.T, split bool) (cityPath string, cfg *config.City, sessionName string) {
	t.Helper()
	binding := ""
	if split {
		binding = filepath.Join(t.TempDir(), "store")
	}
	cityPath = oneShotCLICity(t, binding)
	captureCLIStorageStderr(t)
	appendCityTOML(t, cityPath, "\n[[agent]]\nname = \"worker\"\nstart_command = \"true\"\nmin_active_sessions = 0\nmax_active_sessions = 3\n")
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loading city config: %v", err)
	}
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening city store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	sessions := cliSessionStore(work, cfg, cityPath)

	var open beads.Bead
	for i, closed := range []bool{false, true} {
		row, err := sessions.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":   "worker",
				"agent_name": "worker",
				"state":      "active",
			},
		})
		if err != nil {
			t.Fatalf("creating session row %d: %v", i, err)
		}
		if err := sessions.SetMetadata(row.ID, "session_name", PoolSessionName("worker", row.ID)); err != nil {
			t.Fatalf("naming session row %s: %v", row.ID, err)
		}
		if closed {
			if err := sessions.Close(row.ID); err != nil {
				t.Fatalf("closing session row %s: %v", row.ID, err)
			}
			continue
		}
		open = row
	}
	if split {
		if _, err := work.Get(open.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("work store Get(%s) error = %v, want ErrNotFound: the row must live only on the binding", open.ID, err)
		}
	}
	return cityPath, cfg, PoolSessionName("worker", open.ID)
}

// doctorTopologies runs fn over a single-store city and a born-split one.
func doctorTopologies(t *testing.T, fn func(t *testing.T, split bool)) {
	t.Run("single-store", func(t *testing.T) { fn(t, false) })
	t.Run("born-split", func(t *testing.T) { fn(t, true) })
}

// gc doctor's agent-sessions and orphan-sessions checks, as buildDoctorChecks
// wires them, account for a live pool session by its session row: its agent has
// a session and it is no orphan. Only a runtime no row or agent accounts for is
// reported. This holds wherever the city keeps its sessions.
func TestDoctorSessionChecksAccountForPoolSessionsOnBothTopologies(t *testing.T) {
	doctorTopologies(t, func(t *testing.T, split bool) {
		cityPath, cfg, sessionName := doctorSessionCity(t, split)
		sp := runtime.NewFake()
		for _, name := range []string{sessionName, "stray-runtime"} {
			if err := sp.Start(t.Context(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
		}
		oldBuild := buildSessionProviderByName
		buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
			return sp, nil
		}
		t.Cleanup(func() { buildSessionProviderByName = oldBuild })

		checks := buildDoctorChecks(cityPath, cfg, nil, buildDoctorChecksOpts{
			SkipCityDoltCheck:    true,
			SkipManagedDoltCheck: true,
			SkipStorePreflight:   true,
		})
		results := map[string]*doctor.CheckResult{}
		for _, c := range checks {
			switch c.Name() {
			case "agent-sessions", "orphan-sessions":
				results[c.Name()] = c.Run(&doctor.CheckContext{CityPath: cityPath})
			}
		}
		agents, orphans := results["agent-sessions"], results["orphan-sessions"]
		if agents == nil || orphans == nil {
			t.Fatalf("buildDoctorChecks registered agent-sessions=%v orphan-sessions=%v, want both", agents != nil, orphans != nil)
		}
		if agents.Status != doctor.StatusOK {
			t.Errorf("agent-sessions = %d %q %v, want OK: worker's pool session %s is running", agents.Status, agents.Message, agents.Details, sessionName)
		}
		if orphans.Status != doctor.StatusWarning || !reflect.DeepEqual(orphans.Details, []string{"stray-runtime"}) {
			t.Errorf("orphan-sessions = %d %q %v, want Warning naming only [stray-runtime]", orphans.Status, orphans.Message, orphans.Details)
		}
	})
}

// The session registry the checks read lists the open session rows the way gc
// session list does: from the session-class store, by the type+label union (a
// session-typed row without the label counts), leaving closed rows out.
func TestDoctorSessionRegistryReadsTheSessionStoreOnBothTopologies(t *testing.T) {
	doctorTopologies(t, func(t *testing.T, split bool) {
		cityPath, cfg, sessionName := doctorSessionCity(t, split)
		work, err := openCityStoreAt(cityPath)
		if err != nil {
			t.Fatalf("opening city store: %v", err)
		}
		t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
		if _, err := cliSessionStore(work, cfg, cityPath).Create(beads.Bead{
			Type: session.BeadType,
			Metadata: map[string]string{
				"template":     "worker",
				"session_name": "worker-unlabeled",
			},
		}); err != nil {
			t.Fatalf("creating unlabeled session row: %v", err)
		}

		got, err := doctorSessionRegistry(cityPath, cfg)()
		if err != nil {
			t.Fatalf("reading the session registry: %v", err)
		}
		want := map[string]string{sessionName: "worker", "worker-unlabeled": "worker"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("session registry = %v, want %v", got, want)
		}
	})
}
