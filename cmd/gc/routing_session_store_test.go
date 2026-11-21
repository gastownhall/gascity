package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphroute"
)

// rigControlDispatchCity writes a city with one rig whose store is separate
// from the city's and returns the city path, the rig path and its config.
func rigControlDispatchCity(t *testing.T) (cityPath, rigPath string, cfg *config.City) {
	t.Helper()
	cityPath = t.TempDir()
	rigPath = filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Rigs:      []config.Rig{{Name: "frontend", Path: rigPath}},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend")
	return cityPath, rigPath, cfg
}

// TestControlDispatcherReleasesTheCitySessionStoreItOpens is AC-6 for the
// per-bead control dispatch root: a rig-scope control of every kind that can
// route or recycle a session opens the city store once for its sessions and
// closes it on return, whether the dispatch succeeds or fails; the kinds with
// no session work open nothing.
func TestControlDispatcherReleasesTheCitySessionStoreItOpens(t *testing.T) {
	sessionKinds := map[string]bool{
		beadmeta.KindCheck:     true,
		beadmeta.KindFanout:    true,
		beadmeta.KindDrain:     true,
		beadmeta.KindRetryEval: true,
		beadmeta.KindRetry:     true,
		beadmeta.KindRalph:     true,
	}
	for _, kind := range beadmeta.ControlKinds {
		t.Run(kind, func(t *testing.T) {
			cityPath, rigPath, cfg := rigControlDispatchCity(t)
			var opened []*closeStoreSpy
			prevOpen := slingOpenCityStore
			slingOpenCityStore = func(string) (beads.Store, error) {
				spy := &closeStoreSpy{Store: beads.NewMemStore()}
				opened = append(opened, spy)
				return spy, nil
			}
			t.Cleanup(func() { slingOpenCityStore = prevOpen })

			workflow := beads.NewMemStore()
			control := mustCreateBead(t, workflow, beads.Bead{Title: kind + " control", Metadata: map[string]string{beadmeta.KindMetadataKey: kind}})
			// A bare control carries none of the metadata its kind needs, so
			// most dispatches fail: the release must run on that return too.
			_ = runControlDispatcherWithStoreAndConfig(cityPath, rigPath, workflow, control.ID, cfg, io.Discard, io.Discard)

			want := 0
			if sessionKinds[kind] {
				want = 1
			}
			if len(opened) != want {
				t.Fatalf("%s control opened the city store %d time(s), want %d", kind, len(opened), want)
			}
			for _, spy := range opened {
				if got := spy.closeCount(); got != 1 {
					t.Errorf("%s control closed the city session store %d time(s), want 1", kind, got)
				}
			}
		})
	}
}

// TestControlDispatcherRefusesWhenTheCitySessionStoreDoesNotOpen is AC-6's
// error arm for control dispatch: a city store that fails to open fails the
// dispatch with context instead of routing sessions through the rig workflow
// store, and the control is left untouched.
func TestControlDispatcherRefusesWhenTheCitySessionStoreDoesNotOpen(t *testing.T) {
	cityPath, rigPath, cfg := rigControlDispatchCity(t)
	openErr := errors.New("city ledger unreadable")
	prevOpen := slingOpenCityStore
	slingOpenCityStore = func(string) (beads.Store, error) { return nil, openErr }
	t.Cleanup(func() { slingOpenCityStore = prevOpen })

	workflow := beads.NewMemStore()
	control := mustCreateBead(t, workflow, beads.Bead{Title: "retry control", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRetry}})
	var stderr bytes.Buffer
	err := runControlDispatcherWithStoreAndConfig(cityPath, rigPath, workflow, control.ID, cfg, io.Discard, &stderr)
	if !errors.Is(err, openErr) || !strings.Contains(err.Error(), control.ID) {
		t.Fatalf("dispatch error = %v, want the city-store open error naming control %s", err, control.ID)
	}
	got, getErr := workflow.Get(control.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != "open" {
		t.Errorf("control status = %q after a refused dispatch, want open", got.Status)
	}
	if n := countSessionBeads(t, workflow); n != 0 {
		t.Errorf("the rig workflow store holds %d session bead(s), want none", n)
	}
}

// TestRecycleDispatchSubjectSessionRefusesAZeroSessionsHandle is AC-6/AC-8
// for the retry recycle hook: with no sessions store it fails the recycle with
// context instead of killing through some other store.
func TestRecycleDispatchSubjectSessionRefusesAZeroSessionsHandle(t *testing.T) {
	recycle := recycleDispatchSubjectSession(t.TempDir(), beads.SessionStore{}, &config.City{})
	err := recycle(beads.Bead{ID: "gc-1", Assignee: "polecat-1"})
	if !errors.Is(err, graphroute.ErrNoSessionStore) || !strings.Contains(err.Error(), "gc-1") {
		t.Fatalf("recycle error = %v, want ErrNoSessionStore naming subject gc-1", err)
	}
}
