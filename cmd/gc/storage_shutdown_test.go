package main

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// observedStorageCloser wraps the engine closer the routes own so a test can
// count closes and read the binding at the instant of the close.
//
// The read is what makes the ordering assertable. "The routes closed last" is a
// claim about order, and a test that only inspects the database afterwards
// cannot tell a write that happened before the close from one that happened
// after it — both are on disk when the test looks.
type observedStorageCloser struct {
	closer  io.Closer
	closes  int
	observe func()
}

func (c *observedStorageCloser) Close() error {
	c.closes++
	if c.observe != nil {
		c.observe()
	}
	return c.closer.Close()
}

// splitCityRuntime builds a running city runtime over a genesis split city: the
// five infrastructure classes are served from an opened SQLite binding, work
// stays on the store the runtime already holds.
func splitCityRuntime(t *testing.T) (*CityRuntime, infraBindingTarget, beads.Store) {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Storage = infraSplitConfig(filepath.Join(t.TempDir(), "store")).Storage
	// An empty work source: this city has nothing to move, so the gate admits
	// it as a genesis and opens the binding.
	stubInfraMigrationSource(t)

	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if cr.storageRoutes == nil {
		t.Fatal("a genesis split city opened no storage routes, so nothing below is about a binding")
	}
	work := beads.NewMemStore()
	cr.standaloneCityStore = work
	return cr, mustResolveInfraTarget(t, cityPath, cfg), work
}

// TestCityRuntimeShutdownClosesStorageRoutesAfterItsLastWrite pins the shutdown
// ordering on a split city: every write shutdown performs goes through an OPEN
// binding, and the binding closes once, after them.
//
// The order is not cosmetic. Shutdown drains order dispatchers, marks every
// awake session with the city-stop sleep reason and stops the fleet, and on a
// split city all three of those are session- and order-class writes served from
// the binding. Closing the engine first turns each of them into a write against
// a closed store, and the sleep reason an operator reads after `gc stop` is the
// one that never lands.
func TestCityRuntimeShutdownClosesStorageRoutesAfterItsLastWrite(t *testing.T) {
	cr, target, work := splitCityRuntime(t)

	routed := cr.sessionsBeadStore().Store
	if routed == nil {
		t.Fatal("the sessions class resolved to no store at all")
	}
	if routed == work {
		t.Fatal("the sessions class still resolves to the work store on a split city")
	}
	awake, err := routed.Create(beads.Bead{
		Title:  "awake at shutdown",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "awake",
		},
	})
	if err != nil {
		t.Fatalf("seeding a session bead in the binding: %v", err)
	}

	// The instant-of-close observation, read through the routes' own handle
	// while it is still open.
	observed := &observedStorageCloser{closer: cr.storageRoutes.closers[0]}
	sleepReasonAtClose := ""
	observed.observe = func() {
		bead, err := routed.Get(awake.ID)
		if err != nil {
			t.Errorf("reading the session bead at close time: %v", err)
			return
		}
		sleepReasonAtClose = bead.Metadata["sleep_reason"]
	}
	cr.storageRoutes.closers = []io.Closer{observed}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("shutdown panicked: %v", r)
			}
		}()
		cr.shutdown()
	}()

	if observed.closes != 1 {
		t.Fatalf("the storage binding was closed %d time(s), want exactly 1", observed.closes)
	}
	if want := string(sessionpkg.SleepReasonCityStop); sleepReasonAtClose != want {
		t.Errorf("at the moment the binding closed the session's sleep_reason was %q, want %q; the close ran before shutdown's last write",
			sleepReasonAtClose, want)
	}

	// The durable half: the write is in the database the next process opens,
	// not just in the handle this one held.
	binding := openMigratedDestination(t, target)
	persisted, err := binding.Get(awake.ID)
	if err != nil {
		t.Fatalf("the session bead is not readable from the binding after shutdown: %v", err)
	}
	if got, want := persisted.Metadata["sleep_reason"], string(sessionpkg.SleepReasonCityStop); got != want {
		t.Errorf("sleep_reason in the binding = %q, want %q", got, want)
	}
	if rows := infraStoreFingerprint(t, work); len(rows) != 0 {
		t.Errorf("the work store holds %v; a relocated session class must not write there", rows)
	}
}

// TestCityRuntimeShutdownClosesStorageRoutesWhenSessionsArePreserved covers the
// other exit: a shutdown that hands its sessions to the next supervisor returns
// early, and it must still hand over the database file. A process that exits
// holding the engine leaves the successor's open racing this one's.
func TestCityRuntimeShutdownClosesStorageRoutesWhenSessionsArePreserved(t *testing.T) {
	cr, _, _ := splitCityRuntime(t)

	observed := &observedStorageCloser{closer: cr.storageRoutes.closers[0]}
	cr.storageRoutes.closers = []io.Closer{observed}

	cr.preserveSessionsOnShutdown()
	cr.shutdown()

	if observed.closes != 1 {
		t.Fatalf("a preserved-session shutdown closed the storage binding %d time(s), want exactly 1", observed.closes)
	}
	// The Once is what makes "exactly once" a property of the process rather
	// than of this call, so the repeat call is part of the claim.
	cr.shutdown()
	if observed.closes != 1 {
		t.Fatalf("a second shutdown closed the storage binding again (%d closes total)", observed.closes)
	}
}
