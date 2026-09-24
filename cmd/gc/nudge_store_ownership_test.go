package main

import (
	"bytes"
	"errors"
	"testing"
	"time"

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

// requireRoutesServeALiveStore reads the sessions class through the CLI routes
// and fails if the routes' shared engine has been closed. Any class would do:
// on the whole-split fixture every relocated class shares one engine, so a
// nudge frame that closes it takes the sessions class down with it.
func requireRoutesServeALiveStore(t *testing.T, cityPath, after string) {
	t.Helper()
	store := cliSessionStore(beads.NewMemStore(), nil, cityPath)
	if _, err := store.Get("gcg-000000"); errors.Is(err, beads.ErrStoreClosed) {
		t.Fatalf("%s closed the storage routes' shared engine; the routes still serve it and every later class read fails: %v", after, err)
	}
}

// enqueueRoutedNudge queues one nudge through the routed nudges store, so the
// frames under test find real work on the relocated binding.
func enqueueRoutedNudge(t *testing.T, cityPath string) queuedNudge {
	t.Helper()
	item := newQueuedNudge("mayor", "hello", time.Now())
	if err := enqueueQueuedNudgeWithStore(cityPath, cliNudgesStore(beads.NewMemStore(), nil, cityPath), item); err != nil {
		t.Fatalf("enqueueing a nudge on the relocated city: %v", err)
	}
	return item
}

// TestNudgeOwningFramesLeaveTheStorageRoutesServingALiveStore covers every
// nudge frame that opens and closes its own store (#5979), each on a relocated
// city: the controller's fence census, `gc nudge drop`, the maintenance sweep,
// and the enqueue / record-failure helpers when the caller passes no store.
func TestNudgeOwningFramesLeaveTheStorageRoutesServingALiveStore(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, cityPath string)
	}{
		{"fence census", func(t *testing.T, cityPath string) {
			enqueueRoutedNudge(t, cityPath)
			_ = loadLiveNudgeFenceSessionIDsFromCity(cityPath)
		}},
		{"nudge drop", func(t *testing.T, cityPath string) {
			item := enqueueRoutedNudge(t, cityPath)
			var stdout, stderr bytes.Buffer
			if code := doNudgeDrop(cityPath, []string{item.ID}, false, &stdout, &stderr); code != 0 {
				t.Fatalf("gc nudge drop exited %d: %s", code, stderr.String())
			}
		}},
		{"maintenance sweep", func(t *testing.T, cityPath string) {
			enqueueRoutedNudge(t, cityPath)
			if err := runNudgeQueueMaintenanceSweep(cityPath, time.Now()); err != nil {
				t.Fatalf("maintenance sweep: %v", err)
			}
		}},
		{"enqueue with no store", func(t *testing.T, cityPath string) {
			if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{}, newQueuedNudge("mayor", "hi", time.Now())); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}},
		{"record failure with no store", func(t *testing.T, cityPath string) {
			item := enqueueRoutedNudge(t, cityPath)
			if _, err := recordQueuedNudgeFailureDetailed(cityPath, beads.NudgesStore{}, []string{item.ID}, errNudgeManualDrop, time.Now()); err != nil {
				t.Fatalf("record failure: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, _ := migratedOneShotCLICity(t)
			captureCLIStorageStderr(t)
			requireRoutesServeALiveStore(t, cityPath, "nothing (fixture)")
			tc.run(t, cityPath)
			requireRoutesServeALiveStore(t, cityPath, tc.name)
		})
	}
}

// TestNudgeOwningFramesCloseTheWorkStoreTheyOpenedOnARelocatedCity is the leak
// half of the ownership rule: on a relocated city the class store is the
// routes' engine, but the open still opened a work store, and that work store is
// the handle the frame must close. Closing "nothing" would stop the engine
// damage and leak one work store per frame instead.
func TestNudgeOwningFramesCloseTheWorkStoreTheyOpenedOnARelocatedCity(t *testing.T) {
	cityPath, _ := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)

	store, opened, err := openNudgeBeadStoreOwned(cityPath)
	if err != nil {
		t.Fatalf("opening the nudges store: %v", err)
	}
	if opened == nil {
		t.Fatal("the owned opener reported no opened handle; the work store it opened would leak")
	}
	if opened == store.Store {
		t.Fatal("fixture is not relocated: the opened handle is the class store")
	}
	routed := cliNudgesStore(beads.NewMemStore(), nil, cityPath)
	if store.Store != routed.Store {
		t.Fatalf("the class store %p is not the routes' nudges store %p", store.Store, routed.Store)
	}

	maint := nudgeMaintenanceStore{cityPath: cityPath}
	maint.ensureOpen()
	if maint.handle == nil || maint.handle == maint.store.Store {
		t.Fatalf("the maintenance frame holds handle %p for class store %p; it must hold the work store it opened", maint.handle, maint.store.Store)
	}
	if err := maint.close(); err != nil {
		t.Fatalf("closing the maintenance frame: %v", err)
	}
}

// TestClosingABorrowedRoutedStoreDoesNotCloseTheEngine is the class-wide guard
// behind the nudge fix: every store the CLI class resolvers hand out on a
// relocated city is borrowed from the process-lived storage routes, so a
// borrower closing it must not close the routes' engine. Route teardown still
// owns the engine and must still close it.
func TestClosingABorrowedRoutedStoreDoesNotCloseTheEngine(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)

	work := beads.NewMemStore()
	borrowed := map[string]beads.Store{
		"sessions":  cliSessionStore(work, cfg, cityPath),
		"nudges":    cliNudgesStore(work, cfg, cityPath).Store,
		"graph":     cliGraphStore(work, cfg, cityPath).Store,
		"messaging": cliMailStore(work, cfg, cityPath).Store,
		"orders":    resolveOrderStore(cliStorageRoutes(cityPath), work, cfg, cityPath, nil),
	}
	for class, store := range borrowed {
		if store == beads.Store(work) {
			t.Fatalf("fixture is not relocated: the %s route handed back the work store", class)
		}
		if err := closeBeadStoreHandle(store); err != nil {
			t.Fatalf("closing the borrowed %s store: %v", class, err)
		}
		requireRoutesServeALiveStore(t, cityPath, "closing the borrowed "+class+" store")
	}

	held := cliSessionStore(work, cfg, cityPath)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("tearing down the storage routes: %v", err)
	}
	if _, err := held.Get("gcg-000000"); !errors.Is(err, beads.ErrStoreClosed) {
		t.Fatalf("route teardown left the engine open (Get err = %v); the routes' closers must still release it", err)
	}
}
