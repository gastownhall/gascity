package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// newSkipDispatchCity writes a minimal city with its .gc directory and routes
// the production dispatch's scope-store opens to store. The .gc directory has
// to exist up front: writeDispatchWakeFile gives up when it is missing, which
// would make every "no wake file" assertion vacuous.
func newSkipDispatchCity(t *testing.T, store beads.Store) string {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("create .gc: %v", err)
	}
	prev := openControlStoreForDispatch
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() { openControlStoreForDispatch = prev })
	return cityDir
}

// createNonOpenScopeCheck seeds a scope-check control bead (no city-config
// resolution) and moves it to status, the shape of a stale ready-queue row.
func createNonOpenScopeCheck(t *testing.T, store beads.Store, status string) beads.Bead {
	t.Helper()
	control, err := store.Create(beads.Bead{
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck},
	})
	if err != nil {
		t.Fatalf("seed control bead: %v", err)
	}
	if status == "closed" {
		err = store.Close(control.ID)
	} else {
		err = store.Update(control.ID, beads.UpdateOpts{Status: &status})
	}
	if err != nil {
		t.Fatalf("set control bead %s to %s: %v", control.ID, status, err)
	}
	got, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("read control bead %s: %v", control.ID, err)
	}
	if got.Status != status {
		t.Fatalf("premise: control bead %s status = %q, want %q", control.ID, got.Status, status)
	}
	return got
}

// TestRunControlDispatcherNotOpenSkipContract pins both entry points on a
// control bead that is not open: the one-shot `gc convoy control <id>` path
// keeps returning nil with no output, while the serve seam reports the no-op as
// errControlNotProcessed so the drain can tell it from progress, and still
// closes the scope store it opened.
func TestRunControlDispatcherNotOpenSkipContract(t *testing.T) {
	store := newCloseCountingStore(t, false)
	cityDir := newSkipDispatchCity(t, store)
	stale := createNonOpenScopeCheck(t, store, "closed")

	var oneShotOut, oneShotErr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityDir, cityDir, store, stale.ID, nil, &oneShotOut, &oneShotErr); err != nil {
		t.Fatalf("one-shot dispatch of a closed control bead: err = %v, want nil (stderr=%q)", err, oneShotErr.String())
	}
	if oneShotOut.Len() != 0 {
		t.Fatalf("one-shot dispatch of a closed control bead wrote stdout %q, want nothing", oneShotOut.String())
	}

	var seamOut, seamErr bytes.Buffer
	err := runControlDispatcherInStore(cityDir, cityDir, stale.ID, &seamOut, &seamErr, nil)
	if !errors.Is(err, errControlNotProcessed) {
		t.Fatalf("serve seam dispatch of a closed control bead: err = %v, want errControlNotProcessed (stderr=%q)", err, seamErr.String())
	}
	if seamOut.Len() != 0 {
		t.Fatalf("serve seam dispatch of a closed control bead wrote stdout %q, want nothing", seamOut.String())
	}
	if got := store.closes(); got != 1 {
		t.Fatalf("scope store closed %d times, want 1: the skip path must close the store the seam opened", got)
	}
}
