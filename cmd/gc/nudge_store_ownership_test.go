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

// TestNudgeOwningFramesLeaveTheStorageRoutesServingALiveStore covers the nudge
// frames that open and close their own store (#5979), each on a relocated city:
// the controller's fence census, `gc nudge drop`, the maintenance sweep, the
// enqueue / record-failure helpers when the caller passes no store, and the
// wait-nudge withdrawal.
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
		{"wait-nudge withdrawal", func(t *testing.T, cityPath string) {
			item := enqueueRoutedNudge(t, cityPath)
			if err := withdrawQueuedWaitNudges(cityPath, []string{item.ID}); err != nil {
				t.Fatalf("withdraw wait nudges: %v", err)
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

// closeCountingWorkStore is a work store handle that counts its own closes.
// Once closed it refuses the reads and writes a wait-nudge withdrawal makes, as
// a backend that holds a database handle per store handle (native Dolt,
// DoltLite, the proxied store) does. The file store's own close is a no-op, so
// without the refusal a frame that closes before it is done still passes there.
type closeCountingWorkStore struct {
	beads.Store
	closes         int
	usedAfterClose []string
}

// CloseStore counts the close and forwards it to the real work store, so the
// handle is released exactly as it would be in production.
func (s *closeCountingWorkStore) CloseStore() error {
	s.closes++
	return closeBeadStoreHandle(s.Store)
}

// refuseAfterClose records op and refuses it once the handle has been closed.
func (s *closeCountingWorkStore) refuseAfterClose(op string) error {
	if s.closes == 0 {
		return nil
	}
	s.usedAfterClose = append(s.usedAfterClose, op)
	return beads.ErrStoreClosed
}

// Get reads through to the real work store unless the handle is closed.
func (s *closeCountingWorkStore) Get(id string) (beads.Bead, error) {
	if err := s.refuseAfterClose("Get"); err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Get(id)
}

// List reads through to the real work store unless the handle is closed.
func (s *closeCountingWorkStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if err := s.refuseAfterClose("List"); err != nil {
		return nil, err
	}
	return s.Store.List(query)
}

// SetMetadataBatch writes through to the real work store unless the handle is
// closed.
func (s *closeCountingWorkStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.refuseAfterClose("SetMetadataBatch"); err != nil {
		return err
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// Close closes the bead in the real work store unless the handle is closed.
func (s *closeCountingWorkStore) Close(id string) error {
	if err := s.refuseAfterClose("Close"); err != nil {
		return err
	}
	return s.Store.Close(id)
}

// countNudgeWorkStoreOpens wraps every work store the nudge openers open in a
// closeCountingWorkStore and returns the handles in open order. The real store
// is still opened, so the frame runs against the real fixture.
func countNudgeWorkStoreOpens(t *testing.T) *[]*closeCountingWorkStore {
	t.Helper()
	var opened []*closeCountingWorkStore
	prev := openNudgeWorkStore
	openNudgeWorkStore = func(storePath, cityPath string) (beads.Store, error) {
		store, err := prev(storePath, cityPath)
		if err != nil {
			return nil, err
		}
		handle := &closeCountingWorkStore{Store: store}
		opened = append(opened, handle)
		return handle, nil
	}
	t.Cleanup(func() { openNudgeWorkStore = prev })
	return &opened
}

// TestNudgeOwningFramesCloseTheWorkStoreTheyOpenedOnARelocatedCity is the
// ownership rule itself, per frame: on a relocated city the class store is the
// routes' engine, but the open still opened a work store, and each owning frame
// must close exactly that handle, exactly once.
//
// With emittingClassStore.CloseStore a no-op, closing the class store no longer
// breaks the routes, so the frame tests above pass even if a frame closes the
// wrong store. This test is what fails in that case: a frame that closes
// store.Store instead of the handle it opened leaves the handle at zero closes,
// which leaks one work store per call (per fence census in the controller).
func TestNudgeOwningFramesCloseTheWorkStoreTheyOpenedOnARelocatedCity(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, cityPath string, item queuedNudge)
	}{
		{"fence census", func(_ *testing.T, cityPath string, _ queuedNudge) {
			_ = loadLiveNudgeFenceSessionIDsFromCity(cityPath)
		}},
		{"nudge drop", func(t *testing.T, cityPath string, item queuedNudge) {
			var stdout, stderr bytes.Buffer
			if code := doNudgeDrop(cityPath, []string{item.ID}, false, &stdout, &stderr); code != 0 {
				t.Fatalf("gc nudge drop exited %d: %s", code, stderr.String())
			}
		}},
		{"maintenance sweep", func(t *testing.T, cityPath string, _ queuedNudge) {
			// A day on the item expires, so the sweep has a shadow debt to pay.
			if err := runNudgeQueueMaintenanceSweep(cityPath, time.Now().Add(defaultQueuedNudgeTTL+time.Minute)); err != nil {
				t.Fatalf("maintenance sweep: %v", err)
			}
		}},
		{"enqueue with no store", func(t *testing.T, cityPath string, _ queuedNudge) {
			if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{}, newQueuedNudge("mayor", "hi", time.Now())); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}},
		{"record failure with no store", func(t *testing.T, cityPath string, item queuedNudge) {
			if _, err := recordQueuedNudgeFailureDetailed(cityPath, beads.NudgesStore{}, []string{item.ID}, errNudgeManualDrop, time.Now()); err != nil {
				t.Fatalf("record failure: %v", err)
			}
		}},
		{"wait-nudge withdrawal", func(t *testing.T, cityPath string, item queuedNudge) {
			if err := withdrawQueuedWaitNudges(cityPath, []string{item.ID}); err != nil {
				t.Fatalf("withdraw wait nudges: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, _ := migratedOneShotCLICity(t)
			captureCLIStorageStderr(t)
			item := enqueueRoutedNudge(t, cityPath)
			opened := countNudgeWorkStoreOpens(t)

			tc.run(t, cityPath, item)

			if len(*opened) == 0 {
				t.Fatalf("%s opened no work store; the frame under test did not run its owning path", tc.name)
			}
			routed := cliNudgesStore(beads.NewMemStore(), nil, cityPath).Store
			for i, handle := range *opened {
				if beads.Store(handle) == routed {
					t.Fatalf("fixture is not relocated: open %d returned the routes' nudges store", i)
				}
				if handle.closes != 1 {
					t.Errorf("%s: work store open %d of %d was closed %d time(s), want exactly 1", tc.name, i+1, len(*opened), handle.closes)
				}
			}
			requireRoutesServeALiveStore(t, cityPath, tc.name)
		})
	}
}

// TestWaitNudgeWithdrawalClosesTheWorkStoreAfterUsingItOnAnUnrelocatedCity is
// the withdrawal frame's ownership rule on the default city shape. With the
// nudges class not relocated, the class store is the very handle the frame
// opened, so the frame must close it exactly once and only after the
// withdrawal. A frame that closes first runs the whole withdrawal on a closed
// store, and on a backend that holds a handle the queued nudge stays pending.
// The relocated table above cannot see that: there the handle the frame opened
// is not the store doing the work.
func TestWaitNudgeWithdrawalClosesTheWorkStoreAfterUsingItOnAnUnrelocatedCity(t *testing.T) {
	setWaitTestFileBeads(t)
	cityPath := t.TempDir()
	item := newQueuedNudgeWithOptions("worker", "Wait satisfied.", "wait", time.Now().Add(-time.Minute), queuedNudgeOptions{
		ID:        "wait-gc-1-1-1",
		Reference: &nudgeReference{Kind: "bead", ID: "gc-1"},
	})
	if err := enqueueQueuedNudge(cityPath, item); err != nil {
		t.Fatalf("enqueueing a wait nudge: %v", err)
	}
	work := beads.NewMemStore()
	if cliNudgesStore(work, nil, cityPath).Store != beads.Store(work) {
		t.Fatalf("fixture is relocated: the nudges route did not hand back the work store")
	}
	opened := countNudgeWorkStoreOpens(t)

	err := withdrawQueuedWaitNudges(cityPath, []string{item.ID})

	if len(*opened) != 1 {
		t.Fatalf("the withdrawal opened %d work store(s), want exactly 1", len(*opened))
	}
	handle := (*opened)[0]
	if handle.closes != 1 {
		t.Errorf("the withdrawal closed the work store it opened %d time(s), want exactly 1", handle.closes)
	}
	if len(handle.usedAfterClose) != 0 {
		t.Errorf("the withdrawal used the work store after closing it: %v", handle.usedAfterClose)
	}
	if err != nil {
		t.Fatalf("withdraw wait nudges: %v", err)
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
