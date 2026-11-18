package main

import (
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"sort"
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

// doctorRetiredSessionRows writes open session rows for agents config no longer
// has, with the runtime names the controller would leave behind: one still
// active, one the controller already marked orphaned. It returns their runtime
// session names.
func doctorRetiredSessionRows(t *testing.T, cityPath string, cfg *config.City) []string {
	t.Helper()
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening city store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	sessions := cliSessionStore(work, cfg, cityPath)
	rows := []map[string]string{
		{"template": "retired", "state": "active", "session_name": "retired"},
		{"template": "gone", "state": "orphaned", "session_name": "gone-gcg-x"},
	}
	names := make([]string, 0, len(rows))
	for _, meta := range rows {
		if _, err := sessions.Create(beads.Bead{
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession},
			Metadata: meta,
		}); err != nil {
			t.Fatalf("creating session row for %q: %v", meta["template"], err)
		}
		names = append(names, meta["session_name"])
	}
	return names
}

// The runtime of an agent removed from config is orphaned even while its
// session row is still open: the controller drains it as "configured agent
// removed", and with the controller down nothing closes the row. gc doctor's
// orphan-sessions check, as buildDoctorChecks wires it, reports it, and --fix
// stops it while the configured agent's pool session keeps running. This holds
// wherever the city keeps its sessions.
func TestDoctorOrphanSessionsReportsRemovedAgentRuntimesOnBothTopologies(t *testing.T) {
	doctorTopologies(t, func(t *testing.T, split bool) {
		cityPath, cfg, sessionName := doctorSessionCity(t, split)
		retired := doctorRetiredSessionRows(t, cityPath, cfg)
		sp := runtime.NewFake()
		for _, name := range append([]string{sessionName, "stray-runtime"}, retired...) {
			if err := sp.Start(t.Context(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
		}
		oldBuild := buildSessionProviderByName
		buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
			return sp, nil
		}
		t.Cleanup(func() { buildSessionProviderByName = oldBuild })

		var orphans doctor.Check
		for _, c := range buildDoctorChecks(cityPath, cfg, nil, buildDoctorChecksOpts{
			SkipCityDoltCheck:    true,
			SkipManagedDoltCheck: true,
			SkipStorePreflight:   true,
		}) {
			if c.Name() == "orphan-sessions" {
				orphans = c
			}
		}
		if orphans == nil {
			t.Fatal("buildDoctorChecks registered no orphan-sessions check")
		}
		ctx := &doctor.CheckContext{CityPath: cityPath}
		r := orphans.Run(ctx)
		got := append([]string(nil), r.Details...)
		sort.Strings(got)
		want := append([]string{"stray-runtime"}, retired...)
		sort.Strings(want)
		if r.Status != doctor.StatusWarning || !reflect.DeepEqual(got, want) {
			t.Fatalf("orphan-sessions = %d %q %v, want Warning naming %v", r.Status, r.Message, r.Details, want)
		}
		if err := orphans.Fix(ctx); err != nil {
			t.Fatalf("orphan-sessions Fix: %v", err)
		}
		for _, name := range want {
			if sp.IsRunning(name) {
				t.Errorf("orphan %q still running after Fix", name)
			}
		}
		if !sp.IsRunning(sessionName) {
			t.Errorf("configured agent's pool session %q was stopped by Fix", sessionName)
		}
	})
}

// The session registry maps only rows whose agent is still configured: a row
// for an agent removed from config does not account for its runtime.
func TestDoctorSessionRegistryLeavesOutRemovedAgentsOnBothTopologies(t *testing.T) {
	doctorTopologies(t, func(t *testing.T, split bool) {
		cityPath, cfg, sessionName := doctorSessionCity(t, split)
		doctorRetiredSessionRows(t, cityPath, cfg)
		got, err := doctorSessionRegistry(cityPath, cfg)()
		if err != nil {
			t.Fatalf("reading the session registry: %v", err)
		}
		if want := map[string]string{sessionName: "worker"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("session registry = %v, want %v", got, want)
		}
	})
}

// doctorNamedSessionCity extends doctorSessionCity with two configured named
// sessions whose identity differs from their template, boss on the city agent
// solo and rigA/max on the rig agent rigA/crew, and writes open named-session
// rows for them and for three named sessions no longer declared: boss2 on solo
// and rigA/joe on rigA/crew, whose templates stay configured, and solo, whose
// identity is its template. It returns the live and removed rows' runtime
// session names, and the identity==template row's.
func doctorNamedSessionCity(t *testing.T, split bool) (cityPath string, cfg *config.City, live, removed []string, sameAsTemplate string) {
	t.Helper()
	cityPath, _, poolSession := doctorSessionCity(t, split)
	appendCityTOML(t, cityPath, `
[[agent]]
name = "solo"
start_command = "true"

[[agent]]
name = "crew"
dir = "rigA"
start_command = "true"

[[named_session]]
name = "boss"
template = "solo"

[[named_session]]
name = "max"
template = "crew"
dir = "rigA"
`)
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loading city config: %v", err)
	}
	for _, identity := range []string{"boss", "rigA/max"} {
		if config.FindNamedSession(cfg, identity) == nil {
			t.Fatalf("named session %q is not configured", identity)
		}
	}
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening city store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	sessions := cliSessionStore(work, cfg, cityPath)
	runtimeName := func(identity string) string {
		return config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
	}
	for _, row := range []struct{ identity, template string }{
		{"boss", "solo"},
		{"rigA/max", "rigA/crew"},
		{"boss2", "solo"},
		{"rigA/joe", "rigA/crew"},
		{"solo", "solo"},
	} {
		if _, err := sessions.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                           row.template,
				"agent_name":                         row.template,
				"state":                              "active",
				"session_name":                       runtimeName(row.identity),
				session.NamedSessionMetadataKey:      "true",
				session.NamedSessionIdentityMetadata: row.identity,
			},
		}); err != nil {
			t.Fatalf("creating named session row %q: %v", row.identity, err)
		}
	}
	live = []string{poolSession, runtimeName("boss"), runtimeName("rigA/max")}
	removed = []string{runtimeName("boss2"), runtimeName("rigA/joe")}
	return cityPath, cfg, live, removed, runtimeName("solo")
}

// The runtime of a named session removed from config is orphaned even while
// its row is open and its template agent is still configured: the controller
// drains it as orphaned and retires its row. gc doctor's orphan-sessions check,
// as buildDoctorChecks wires it, reports it and --fix stops it, while the
// configured named sessions keep running. A removed named session whose
// identity is its template keeps that agent's own session name, so it is
// accounted for as before. This holds wherever the city keeps its sessions.
func TestDoctorOrphanSessionsReportsRemovedNamedSessionRuntimesOnBothTopologies(t *testing.T) {
	doctorTopologies(t, func(t *testing.T, split bool) {
		cityPath, cfg, live, removed, sameAsTemplate := doctorNamedSessionCity(t, split)
		sp := runtime.NewFake()
		running := append(append(append([]string(nil), live...), removed...), sameAsTemplate)
		for _, name := range running {
			if err := sp.Start(t.Context(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
		}
		oldBuild := buildSessionProviderByName
		buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
			return sp, nil
		}
		t.Cleanup(func() { buildSessionProviderByName = oldBuild })

		var orphans doctor.Check
		for _, c := range buildDoctorChecks(cityPath, cfg, nil, buildDoctorChecksOpts{
			SkipCityDoltCheck:    true,
			SkipManagedDoltCheck: true,
			SkipStorePreflight:   true,
		}) {
			if c.Name() == "orphan-sessions" {
				orphans = c
			}
		}
		if orphans == nil {
			t.Fatal("buildDoctorChecks registered no orphan-sessions check")
		}
		ctx := &doctor.CheckContext{CityPath: cityPath}
		r := orphans.Run(ctx)
		got := append([]string(nil), r.Details...)
		sort.Strings(got)
		want := append([]string(nil), removed...)
		sort.Strings(want)
		if r.Status != doctor.StatusWarning || !reflect.DeepEqual(got, want) {
			t.Fatalf("orphan-sessions = %d %q %v, want Warning naming %v", r.Status, r.Message, r.Details, want)
		}
		if err := orphans.Fix(ctx); err != nil {
			t.Fatalf("orphan-sessions Fix: %v", err)
		}
		for _, name := range removed {
			if sp.IsRunning(name) {
				t.Errorf("removed named session %q still running after Fix", name)
			}
		}
		for _, name := range append(append([]string(nil), live...), sameAsTemplate) {
			if !sp.IsRunning(name) {
				t.Errorf("accounted-for session %q was stopped by Fix", name)
			}
		}
	})
}

// The session registry maps a named-session row only while its named session
// is configured, even when the row's template agent still is.
func TestDoctorSessionRegistryLeavesOutRemovedNamedSessionsOnBothTopologies(t *testing.T) {
	doctorTopologies(t, func(t *testing.T, split bool) {
		cityPath, cfg, live, _, _ := doctorNamedSessionCity(t, split)
		got, err := doctorSessionRegistry(cityPath, cfg)()
		if err != nil {
			t.Fatalf("reading the session registry: %v", err)
		}
		want := map[string]string{live[0]: "worker", live[1]: "solo", live[2]: "rigA/crew"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("session registry = %v, want %v", got, want)
		}
	})
}
