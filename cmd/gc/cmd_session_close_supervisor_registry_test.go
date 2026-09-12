package main

import (
	"bytes"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// TestCmdSessionCloseLeavesSupervisorRegistryUntouched is a regression guard
// for gascity-8zn: closing a configured named session (mode=always) must not
// remove the owning city from the supervisor registry. When this invariant
// was reported as broken on the rebased-out feat/nix-flake@a6086b7e tip,
// `gc session close <named-session>` was observed to emit
// "Unregistered city '<name>', stopping..." and tear the whole city down.
// The session close path (cmdSessionClose → handle.CloseDetailed →
// Manager.CloseDetailed) is purely bead-store + runtime-provider work and
// must never call supervisor.Registry.Unregister.
func TestCmdSessionCloseLeavesSupervisorRegistryUntouched(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1

[[named_session]]
template = "worker"
mode = "always"
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityDir, "test-city"); err != nil {
		t.Fatalf("Register(%s): %v", cityDir, err)
	}
	before, err := reg.List()
	if err != nil {
		t.Fatalf("registry List (before): %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("registry entries before close = %d, want 1", len(before))
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "test-city--worker",
			"alias":                     "worker",
			"template":                  "worker",
			"configured_named_session":  "true",
			"configured_named_identity": "worker",
			"configured_named_mode":     "always",
			"state":                     "suspended",
			"continuity_eligible":       "true",
		},
	})
	if err != nil {
		t.Fatalf("Create(named session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{"worker"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionClose(worker) = %d, want 0; stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("named-session bead status = %q, want closed", got.Status)
	}

	after, err := reg.List()
	if err != nil {
		t.Fatalf("registry List (after): %v", err)
	}
	if !registryEntriesEqual(before, after) {
		t.Fatalf("supervisor registry mutated by cmdSessionClose (gascity-8zn regression)\nbefore: %#v\nafter:  %#v",
			before, after)
	}
}

// TestCmdSessionCloseLeavesSupervisorRegistryUntouchedForOrdinarySession
// is a regression guard for gascity-8zn covering the non-named-session case.
// The bug report was specifically about a named session in mode=always, but
// the invariant — `gc session close` is a session-scoped operation and must
// never touch the supervisor registry — applies to every bead the command
// can target.
func TestCmdSessionCloseLeavesSupervisorRegistryUntouchedForOrdinarySession(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityDir, "test-city"); err != nil {
		t.Fatalf("Register(%s): %v", cityDir, err)
	}
	before, err := reg.List()
	if err != nil {
		t.Fatalf("registry List (before): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "test-city--worker-1",
			"template":     "worker",
			"state":        "suspended",
		},
	})
	if err != nil {
		t.Fatalf("Create(ordinary session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{bead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionClose(%s) = %d, want 0; stdout=%s stderr=%s",
			bead.ID, code, stdout.String(), stderr.String())
	}

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("ordinary session bead status = %q, want closed", got.Status)
	}

	after, err := reg.List()
	if err != nil {
		t.Fatalf("registry List (after): %v", err)
	}
	if !registryEntriesEqual(before, after) {
		t.Fatalf("supervisor registry mutated by cmdSessionClose (gascity-8zn regression)\nbefore: %#v\nafter:  %#v",
			before, after)
	}
}

func registryEntriesEqual(a, b []supervisor.CityEntry) bool {
	if len(a) != len(b) {
		return false
	}
	ax := append([]supervisor.CityEntry{}, a...)
	bx := append([]supervisor.CityEntry{}, b...)
	sort.Slice(ax, func(i, j int) bool { return ax[i].Path < ax[j].Path })
	sort.Slice(bx, func(i, j int) bool { return bx[i].Path < bx[j].Path })
	for i := range ax {
		if ax[i].Path != bx[i].Path || ax[i].Name != bx[i].Name {
			return false
		}
	}
	return true
}
