package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestNudgeMaintenanceFrameLeavesTheStorageRoutesServingALiveStore pins the
// ownership rule the nudge frames depend on: a per-tick frame closes only a
// handle it opened.
//
// On a city that has relocated the NUDGES class onto a storage binding, the
// store openNudgeBeadStore returns is the storage routes' process-shared
// engine. Closing it from the frame did not detach the routes' memo, so the
// memo went on serving a closed engine and every later class read in the same
// process failed with ErrStoreClosed — in a long-lived process (the controller,
// a poll sidecar) that reached nudge delivery itself and the session-circuit
// reset socket handler, and it lasted until the process exited.
func TestNudgeMaintenanceFrameLeavesTheStorageRoutesServingALiveStore(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)

	work := beads.NewMemStore()
	before := cliSessionStore(work, cfg, cityPath)
	if before == beads.Store(work) {
		t.Fatalf("fixture is not relocated: the session route handed back the work store")
	}
	if _, err := before.Get("gcg-000000"); errors.Is(err, beads.ErrStoreClosed) {
		t.Fatalf("the routes served a closed store before any maintenance frame ran: %v", err)
	}

	// The frame a long-lived process runs on every nudge tick that has work.
	maint := nudgeMaintenanceStore{cityPath: cityPath}
	maint.ensureOpen()
	if err := maint.close(); err != nil {
		t.Fatalf("closing the nudge maintenance frame: %v", err)
	}

	after := cliSessionStore(work, cfg, cityPath)
	if _, err := after.Get("gcg-000000"); errors.Is(err, beads.ErrStoreClosed) {
		t.Fatalf("the nudge maintenance frame closed the storage routes' shared engine; "+
			"the routes still serve it and every later class read fails: %v", err)
	}
}
