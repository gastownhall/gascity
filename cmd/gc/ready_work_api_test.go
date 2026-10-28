package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/gastownhall/gascity/internal/runtime"
)

// readyWorkSplitFixture is a split city as the controller holds it: a city work
// store, two serving rigs, a suspended-on-start rig, and a relocated graph
// binding carrying one work-shaped relic of a city bead.
type readyWorkSplitFixture struct {
	cityPath  string
	cfg       *config.City
	city      beads.Store
	rigs      map[string]beads.Store
	binding   beads.Store
	cityName  string
	relicID   string
	assignee  string
	routedTo  string
	suspended string
}

func newReadyWorkSplitFixture(t *testing.T) readyWorkSplitFixture {
	t.Helper()
	f := readyWorkSplitFixture{
		cityPath:  t.TempDir(),
		city:      splittest.NewWorkStore(t, "gc"),
		binding:   splittest.NewClassStore(t, config.BeadClassGraph),
		cityName:  "readywork-city",
		assignee:  "worker-1",
		routedTo:  "pool/worker",
		suspended: "gamma",
	}
	alpha, bravo, gamma := splittest.NewWorkStore(t, "ra"), splittest.NewWorkStore(t, "rb"), splittest.NewWorkStore(t, "rg")
	f.rigs = map[string]beads.Store{"alpha": alpha, "bravo": bravo, f.suspended: gamma}
	f.cfg = &config.City{
		Workspace: config.Workspace{Name: f.cityName},
		Rigs: []config.Rig{
			{Name: "alpha", Path: filepath.Join(f.cityPath, "rigs", "alpha"), Prefix: "ra"},
			{Name: "bravo", Path: filepath.Join(f.cityPath, "rigs", "bravo"), Prefix: "rb"},
			{Name: f.suspended, Path: filepath.Join(f.cityPath, "rigs", f.suspended), Prefix: "rg", SuspendedOnStart: true},
		},
	}
	inProgress := readywork.StatusInProgress
	for i, store := range []beads.Store{f.city, alpha, bravo, gamma, f.binding} {
		mustCreateReadyBead(t, store, beads.Bead{Title: "plain", Type: "task"})
		mustCreateReadyBead(t, store, beads.Bead{Title: "assigned", Type: "task", Assignee: f.assignee})
		mustCreateReadyBead(t, store, beads.Bead{Title: "routed", Type: "task", Metadata: map[string]string{"gc.routed_to": f.routedTo}})
		claimed := mustCreateReadyBead(t, store, beads.Bead{Title: "claimed", Type: "task"})
		if err := store.Update(claimed.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &f.assignee}); err != nil {
			t.Fatalf("claim in store %d: %v", i, err)
		}
	}
	// A migrated bead: the work store keeps its stale copy, the binding holds a
	// live copy under the same work-shaped id.
	stale := mustCreateReadyBead(t, f.city, beads.Bead{Title: "stale work copy", Type: "task", Assignee: "someone-else"})
	forced, ok := f.binding.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("binding %T cannot model the migration's forced copy", f.binding)
	}
	if _, err := forced.CreateWithForeignID(beads.Bead{ID: stale.ID, Title: "live binding copy", Type: "task", Metadata: map[string]string{"gc.routed_to": f.routedTo}}); err != nil {
		t.Fatalf("relic copy: %v", err)
	}
	f.relicID = stale.ID
	return f
}

// controller is the fixture as the controller holds it.
func (f readyWorkSplitFixture) controller() *controllerState {
	return &controllerState{
		cfg:           f.cfg,
		cityBeadStore: f.city,
		beadStores:    f.rigs,
		storageRoutes: splitRoutes(f.binding),
		cityName:      f.cityName,
		cityPath:      f.cityPath,
	}
}

// inProcess is the answer a fresh `gc ready` reads over the same stores: the
// serving frame, the explicit-binding leg seam, and the reader.
func (f readyWorkSplitFixture) inProcess(t *testing.T, q readywork.Query) []readywork.ReadyBead {
	t.Helper()
	serving, err := readyServingRigs(f.cfg, f.cityPath)
	if err != nil {
		t.Fatalf("readyServingRigs: %v", err)
	}
	rigStores := make(map[string]beads.Store, len(serving))
	for _, rig := range serving {
		rigStores[rig.Name] = f.rigs[rig.Name]
	}
	legs, err := readyFederationLegsOverBinding(f.cityName, f.city, rigStores, f.binding)
	if err != nil {
		t.Fatalf("legs: %v", err)
	}
	rows, err := readywork.Read(legs, q)
	if err != nil {
		t.Fatalf("in-process read: %v", err)
	}
	return rows
}

func readyWorkQueries(f readyWorkSplitFixture) []readywork.Query {
	return []readywork.Query{
		{},
		{Limit: 3},
		{Assignee: f.assignee, Limit: 1},
		{Status: readywork.StatusInProgress, Assignee: f.assignee, Limit: 1},
		{Status: readywork.StatusInProgress},
		{MetadataFields: []string{"gc.routed_to=" + f.routedTo}, Unassigned: true, ExcludeTypes: []string{"epic"}, ExcludeLabels: []string{"hold:mayor"}, Limit: 20},
		{MetadataFields: []string{"gc.routed_to=" + f.routedTo}, Unassigned: true, Sort: readywork.SortOldest, Limit: 20},
		{Status: readywork.StatusOpen},
	}
}

// TestControllerReadyWorkIsTheInProcessAnswer is the CLI == API property the
// endpoint rests on: over the same stores, the controller's answer is the
// in-process reader's, row for row and field for field, on every arm and on a
// city whose binding carries a relic of a work bead.
func TestControllerReadyWorkIsTheInProcessAnswer(t *testing.T) {
	f := newReadyWorkSplitFixture(t)
	cs := f.controller()
	for _, q := range readyWorkQueries(f) {
		got, err := cs.ReadyWork(context.Background(), q)
		if err != nil {
			t.Fatalf("ReadyWork(%+v): %v", q, err)
		}
		want := f.inProcess(t, q)
		if !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			t.Fatalf("ReadyWork(%+v):\ncontroller %s\nin-process %s", q, gotJSON, wantJSON)
		}
	}
}

// TestControllerReadyWorkReadsPastAWarmCache pins freshness: the controller's
// stores are CachingStores, and a bead written behind a primed cache (an agent's
// `bd` write the cache has not absorbed) is in the very next answer.
func TestControllerReadyWorkReadsPastAWarmCache(t *testing.T) {
	f := newReadyWorkSplitFixture(t)
	backing := f.rigs["alpha"]
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	f.rigs["alpha"] = cache
	cs := f.controller()

	written := mustCreateReadyBead(t, backing, beads.Bead{Title: "written by an agent's bd", Type: "task"})
	rows, err := cs.ReadyWork(context.Background(), readywork.Query{})
	if err != nil {
		t.Fatalf("ReadyWork: %v", err)
	}
	if !containsReadyID(rows, written.ID) {
		t.Fatalf("ReadyWork missed %s, written behind the controller's warm cache; the endpoint must read live", written.ID)
	}
}

// TestControllerReadyWorkSkipsSuspendedRigs pins the frame: the controller reads
// the serving rigs a fresh `gc ready` reads, and a suspended rig's rows are in
// neither answer.
func TestControllerReadyWorkSkipsSuspendedRigs(t *testing.T) {
	f := newReadyWorkSplitFixture(t)
	rows, err := f.controller().ReadyWork(context.Background(), readywork.Query{})
	if err != nil {
		t.Fatalf("ReadyWork: %v", err)
	}
	for _, row := range rows {
		if strings.HasPrefix(row.ID, "rg-") {
			t.Fatalf("ReadyWork served %s from the suspended rig %q", row.ID, f.suspended)
		}
	}
}

// TestControllerReadyWorkRefusesWhileAConfigReloadIsPending pins the leg-set
// guard: a staged config change could add, remove or rebind a rig, so the
// controller refuses rather than answer for a different set of stores than a
// fresh read would.
func TestControllerReadyWorkRefusesWhileAConfigReloadIsPending(t *testing.T) {
	f := newReadyWorkSplitFixture(t)
	cs := f.controller()
	var dirty atomic.Bool
	dirty.Store(true)
	cs.configDirty = &dirty
	if _, err := cs.ReadyWork(context.Background(), readywork.Query{}); !errors.Is(err, api.ErrReadyWorkConfigNotCurrent) {
		t.Fatalf("ReadyWork with a pending reload = %v, want api.ErrReadyWorkConfigNotCurrent", err)
	}
}

// readyWorkReloadFixture is a controller whose ready-work answer rides real
// config changes: a city runtime and its API state sharing one dirty flag, as
// the controller wires them.
type readyWorkReloadFixture struct {
	cr       *CityRuntime
	cs       *controllerState
	tomlPath string
	dirty    *atomic.Bool
}

func newReadyWorkReloadFixture(t *testing.T) readyWorkReloadFixture {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cr, cs, _ := newPublicationTestRuntime(t, cityPath, runtime.NewFake(), io.Discard, io.Discard)
	dirty := &atomic.Bool{}
	cr.configDirty, cs.configDirty = dirty, dirty
	f := readyWorkReloadFixture{cr: cr, cs: cs, tomlPath: tomlPath, dirty: dirty}
	f.requireAnswers(t, "before any config change")
	return f
}

// tickReload runs the tick's config-reload phase, as the controller loop does,
// and returns its outcome.
func (f readyWorkReloadFixture) tickReload() reloadOutcome {
	lastProviderName := "fake"
	p := &tickPass{ctx: context.Background(), dirty: f.dirty, lastProviderName: &lastProviderName, cityRoot: f.cr.cityPath}
	f.cr.tickConfigReload(p)
	return p.manualReply.Outcome
}

// startupReload runs the reload the controller applies as its loop starts.
func (f readyWorkReloadFixture) startupReload() {
	lastProviderName := "fake"
	f.cr.applyStartupConfigReload(context.Background(), f.dirty, &lastProviderName, f.cr.cityPath)
}

func (f readyWorkReloadFixture) requireAnswers(t *testing.T, when string) {
	t.Helper()
	if _, err := f.cs.ReadyWork(context.Background(), readywork.Query{}); err != nil {
		t.Fatalf("ReadyWork %s = %v, want an answer", when, err)
	}
}

func (f readyWorkReloadFixture) requireRefuses(t *testing.T, when string) {
	t.Helper()
	if _, err := f.cs.ReadyWork(context.Background(), readywork.Query{}); !errors.Is(err, api.ErrReadyWorkConfigNotCurrent) {
		t.Fatalf("ReadyWork %s = %v, want api.ErrReadyWorkConfigNotCurrent", when, err)
	}
}

// TestControllerReadyWorkRefusesWhileAReloadIsBeingApplied pins the window the
// dirty flag alone leaves open: a reload clears the flag as it begins, and the
// controller holds the old config's stores until it publishes the new one. A
// read in between would answer for the config being replaced, so it refuses
// until the reload publishes, on the tick's reload and the startup reload
// alike.
func TestControllerReadyWorkRefusesWhileAReloadIsBeingApplied(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reload func(readyWorkReloadFixture)
	}{
		{name: "tick", reload: func(f readyWorkReloadFixture) { f.tickReload() }},
		{name: "startup", reload: readyWorkReloadFixture.startupReload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReadyWorkReloadFixture(t)
			parked, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var parkOnce, releaseOnce sync.Once
			releaseReload := func() { releaseOnce.Do(func() { close(release) }) }
			// The reload parks in its bead lifecycle step: after it cleared the
			// dirty flag, before it publishes.
			stubReloadBeadsLifecycle(t, func(string, string, *config.City, cityStoreRecord, io.Writer) error {
				parkOnce.Do(func() { close(parked) })
				<-release
				return nil
			})
			oldRevision := f.cr.configRev
			writeCityRuntimeConfigWithOneSecondShutdownTimeout(t, f.tomlPath)
			f.dirty.Store(true)

			go func() {
				defer close(done)
				tc.reload(f)
			}()
			defer func() {
				releaseReload()
				<-done
			}()
			select {
			case <-parked:
			case <-done:
				t.Fatal("the reload finished without reaching its bead lifecycle step")
			}
			if f.dirty.Load() {
				t.Fatal("the reload in progress left the dirty flag set; this test needs the window after the reload clears it")
			}
			f.requireRefuses(t, "while a reload is being applied")

			releaseReload()
			<-done
			if f.cr.configRev == oldRevision {
				t.Fatal("the reload did not apply the new config")
			}
			f.requireAnswers(t, "once the reload published")
		})
	}
}

// TestControllerReadyWorkRefusesAfterAFailedReloadUntilOneApplies pins the
// failed reload: it clears the dirty flag and leaves the controller on the old
// config while disk holds another, so the controller refuses until a reload
// applies.
func TestControllerReadyWorkRefusesAfterAFailedReloadUntilOneApplies(t *testing.T) {
	f := newReadyWorkReloadFixture(t)
	stubReloadBeadsLifecycle(t, func(string, string, *config.City, cityStoreRecord, io.Writer) error { return nil })
	if err := os.WriteFile(f.tomlPath, []byte("[workspace\n"), 0o644); err != nil {
		t.Fatalf("write a malformed config: %v", err)
	}
	f.dirty.Store(true)
	if got := f.tickReload(); got != reloadOutcomeFailed {
		t.Fatalf("reload of a malformed config = %q, want %q", got, reloadOutcomeFailed)
	}
	if f.dirty.Load() {
		t.Fatal("the failed reload left the dirty flag set; this test needs the case where it does not")
	}
	f.requireRefuses(t, "after a failed reload")

	writeCityRuntimeConfigWithOneSecondShutdownTimeout(t, f.tomlPath)
	f.dirty.Store(true)
	if got := f.tickReload(); got != reloadOutcomeApplied {
		t.Fatalf("reload of the repaired config = %q, want %q", got, reloadOutcomeApplied)
	}
	f.requireAnswers(t, "once a reload applied")
}

// TestControllerReadyWorkRefusesWhileAnAPIConfigWriteIsInFlight pins an API
// config edit's window: it changes the config files before it marks the reload
// pending, so a read in between must refuse; from the mark on, the pending
// reload refuses it until the reload applies.
func TestControllerReadyWorkRefusesWhileAnAPIConfigWriteIsInFlight(t *testing.T) {
	f := newReadyWorkReloadFixture(t)
	written, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseWrite := func() { releaseOnce.Do(func() { close(release) }) }
	var writeErr, mutateErr error
	go func() {
		defer close(done)
		mutateErr = f.cs.mutateAndPoke(func() error {
			writeErr = os.WriteFile(f.tomlPath, []byte(supersedingCityConfig), 0o644)
			close(written)
			<-release
			return writeErr
		})
	}()
	defer func() {
		releaseWrite()
		<-done
	}()
	select {
	case <-written:
	case <-done:
		t.Fatalf("the API write ended (%v) before it wrote the config", mutateErr)
	}
	if writeErr != nil {
		t.Fatalf("write the edited config: %v", writeErr)
	}
	if f.dirty.Load() {
		t.Fatal("the API write marked the reload pending before it finished; this test needs the window before the mark")
	}
	f.requireRefuses(t, "while an API config write is in flight")

	releaseWrite()
	<-done
	if mutateErr != nil {
		t.Fatalf("API config write: %v", mutateErr)
	}
	if !f.dirty.Load() {
		t.Fatal("the API write did not mark the reload pending")
	}
	f.requireRefuses(t, "while the API write's reload is pending")
	if got := f.tickReload(); got != reloadOutcomeApplied {
		t.Fatalf("reload of the API write = %q, want %q", got, reloadOutcomeApplied)
	}
	f.requireAnswers(t, "once the API write's reload applied")
}

// TestControllerReadyWorkRefusesAfterAFailedAPIConfigWriteChangedDisk pins an
// API config write that fails after it changed the config files: one that
// failed partway, as a rig rebind that rewrote .gc/site.toml and then failed to
// persist its binding does, and one whose rollback failed. The controller still
// holds the config it published before the write while disk holds another, and
// the config watcher never sees .gc/, so the write marks the reload pending as
// it ends; the controller refuses until that reload applies.
func TestControllerReadyWorkRefusesAfterAFailedAPIConfigWriteChangedDisk(t *testing.T) {
	errPartway := errors.New("persisting the rig binding failed")
	for _, tc := range []struct {
		name    string
		mutate  func(sitePath, tomlPath string) error
		wantErr func(error) bool
	}{
		{
			name: "it failed partway",
			mutate: func(sitePath, _ string) error {
				if err := writeFileMkdir(sitePath, "# rebound\n"); err != nil {
					return err
				}
				return errPartway
			},
			wantErr: func(err error) bool { return errors.Is(err, errPartway) },
		},
		{
			name: "its rollback failed",
			mutate: func(sitePath, tomlPath string) error {
				// The config no longer loads, and a directory where
				// .gc/site.toml was absent defeats the rollback's remove.
				if err := os.MkdirAll(filepath.Join(sitePath, "squatter"), 0o755); err != nil {
					return err
				}
				return os.WriteFile(tomlPath, []byte("[workspace\n"), 0o644)
			},
			wantErr: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "restoring previous city config")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReadyWorkReloadFixture(t)
			stubReloadBeadsLifecycle(t, func(string, string, *config.City, cityStoreRecord, io.Writer) error { return nil })
			sitePath := filepath.Join(filepath.Dir(f.tomlPath), ".gc", "site.toml")
			if _, err := os.Lstat(sitePath); !os.IsNotExist(err) {
				t.Fatalf(".gc/site.toml before the write: %v, want it absent", err)
			}
			if err := f.cs.mutateAndPoke(func() error { return tc.mutate(sitePath, f.tomlPath) }); !tc.wantErr(err) {
				t.Fatalf("API config write = %v, want the failure under test", err)
			}
			if !f.dirty.Load() {
				t.Fatal("the failed API write that changed disk did not mark the reload pending")
			}
			f.requireRefuses(t, "after a failed API write changed disk")

			if err := os.RemoveAll(sitePath); err != nil {
				t.Fatalf("remove .gc/site.toml: %v", err)
			}
			if got := f.tickReload(); got != reloadOutcomeApplied && got != reloadOutcomeNoChange {
				t.Fatalf("reload after the failed write = %q, want %q or %q", got, reloadOutcomeApplied, reloadOutcomeNoChange)
			}
			f.requireAnswers(t, "once the reload applied")
		})
	}
}

// TestControllerReadyWorkAnswersAfterAFailedAPIConfigWriteLeftDiskAsItFoundIt
// pins the other side: an API config write that failed before it wrote, or
// whose rollback restored what it wrote, leaves the config the controller
// published on disk, so it marks no reload and the controller answers.
func TestControllerReadyWorkAnswersAfterAFailedAPIConfigWriteLeftDiskAsItFoundIt(t *testing.T) {
	errBeforeWrite := errors.New("the write was refused")
	for _, tc := range []struct {
		name   string
		mutate func(tomlPath string) error
	}{
		{name: "it failed before it wrote", mutate: func(string) error { return errBeforeWrite }},
		{name: "it was rolled back", mutate: func(tomlPath string) error {
			return os.WriteFile(tomlPath, []byte("[workspace\n"), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReadyWorkReloadFixture(t)
			before, err := os.ReadFile(f.tomlPath)
			if err != nil {
				t.Fatalf("read city.toml: %v", err)
			}
			if err := f.cs.mutateAndPoke(func() error { return tc.mutate(f.tomlPath) }); err == nil {
				t.Fatal("API config write succeeded, want it to fail")
			}
			if after, err := os.ReadFile(f.tomlPath); err != nil || !bytes.Equal(after, before) {
				t.Fatalf("city.toml after the failed write = %q (%v), want it as it was", after, err)
			}
			if f.dirty.Load() {
				t.Fatal("a failed API write that left disk as it found it marked a reload pending")
			}
			f.requireAnswers(t, "after a failed API write left disk as it found it")
		})
	}
}

// overtakenStore stands in for a leg store whose read a config change
// overtakes: each ready read reports itself to overtake, then reads the store
// as held.
type overtakenStore struct {
	beads.Store
	overtake func()
}

func (s overtakenStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.overtake()
	return s.Store.Ready(q...)
}

// overtakeLegReads binds every ready-work leg to an overtakenStore that runs
// change once, on the first leg read: after the legs were assembled, while they
// are read. It returns the count of leg reads started.
func overtakeLegReads(t *testing.T, change func()) *atomic.Int64 {
	t.Helper()
	var once sync.Once
	reads := &atomic.Int64{}
	prev := readyWorkScopedStore
	readyWorkScopedStore = func(_ context.Context, _ string, _ *config.City, store beads.Store) (beads.Store, error) {
		return overtakenStore{Store: store, overtake: func() {
			reads.Add(1)
			once.Do(change)
		}}, nil
	}
	t.Cleanup(func() { readyWorkScopedStore = prev })
	return reads
}

// TestControllerReadyWorkRefusesWhenAConfigChangeOvertakesTheRead pins that the
// freshness check spans the store reads as well as the leg assembly: a change
// begun or marked pending after the legs were assembled, while they are read,
// may publish other stores and close the ones being read, so the read it
// overtook is refused rather than answered for the config being replaced.
func TestControllerReadyWorkRefusesWhenAConfigChangeOvertakesTheRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*controllerState)
		answers bool
	}{
		{name: "nothing changed", change: func(*controllerState) {}, answers: true},
		{name: "a change began", change: func(cs *controllerState) { cs.freshness.beginChange() }},
		{name: "a change began and ended", change: func(cs *controllerState) {
			cs.freshness.beginChange()
			cs.freshness.endChange()
		}},
		// The config watcher marks an edit it saw pending without beginning a change.
		{name: "a change was marked pending", change: func(cs *controllerState) { cs.configDirty.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := newReadyWorkSplitFixture(t).controller()
			cs.configDirty = &atomic.Bool{}
			reads := overtakeLegReads(t, func() { tc.change(cs) })
			_, err := cs.ReadyWork(context.Background(), readywork.Query{})
			if reads.Load() == 0 {
				t.Fatalf("ReadyWork = %v without reading a leg; no change overtook a read", err)
			}
			switch {
			case tc.answers && err != nil:
				t.Fatalf("ReadyWork = %v, want an answer", err)
			case !tc.answers && !errors.Is(err, api.ErrReadyWorkConfigNotCurrent):
				t.Fatalf("ReadyWork when %s mid-read = %v, want api.ErrReadyWorkConfigNotCurrent", tc.name, err)
			}
		})
	}
	// The real sequence: the config watcher marks an edit, and the tick's reload
	// applies it and publishes the new config while the legs read the old one's
	// stores.
	t.Run("a reload applied", func(t *testing.T) {
		f := newReadyWorkReloadFixture(t)
		stubReloadBeadsLifecycle(t, func(string, string, *config.City, cityStoreRecord, io.Writer) error { return nil })
		oldRevision := f.cr.configRev
		writeCityRuntimeConfigWithOneSecondShutdownTimeout(t, f.tomlPath)
		var outcome reloadOutcome
		reads := overtakeLegReads(t, func() {
			f.dirty.Store(true)
			outcome = f.tickReload()
		})
		f.requireRefuses(t, "when a reload applied mid-read")
		if reads.Load() == 0 {
			t.Fatal("ReadyWork read no leg; no reload overtook a read")
		}
		if outcome != reloadOutcomeApplied || f.cr.configRev == oldRevision {
			t.Fatalf("the reload mid-read = %q at revision %q, want it applied past %q", outcome, f.cr.configRev, oldRevision)
		}
		f.requireAnswers(t, "once the reload published")
	})
}

// writeFileMkdir writes data to path, creating its parent directories.
func writeFileMkdir(path, data string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(data), 0o644)
}

// TestConfigFreshnessRead pins when a ready-work read may answer: nothing
// pending, nothing in progress, the last reload applied or found no change, and
// no change begun or marked pending while it answered.
func TestConfigFreshnessRead(t *testing.T) {
	reload := func(outcome reloadOutcome) func(*configFreshness) {
		return func(f *configFreshness) {
			f.beginChange()
			f.endReload(outcome)
		}
	}
	apiWrite := func(f *configFreshness) {
		f.beginChange()
		f.endChange()
	}
	for _, tc := range []struct {
		name        string
		dirty       bool
		before      []func(*configFreshness)
		during      func(*configFreshness)
		dirtyDuring bool
		want        bool
	}{
		{name: "current", want: true},
		{name: "a change is pending", dirty: true},
		{name: "a change is in progress", before: []func(*configFreshness){(*configFreshness).beginChange}},
		{name: "the last reload applied", before: []func(*configFreshness){reload(reloadOutcomeApplied)}, want: true},
		{name: "the last reload found no change", before: []func(*configFreshness){reload(reloadOutcomeNoChange)}, want: true},
		{name: "the last reload failed", before: []func(*configFreshness){reload(reloadOutcomeFailed)}},
		{name: "the last reload ended without an outcome", before: []func(*configFreshness){reload("")}},
		{name: "a reload applied after one failed", before: []func(*configFreshness){reload(reloadOutcomeFailed), reload(reloadOutcomeApplied)}, want: true},
		{name: "a reload found no change after one failed", before: []func(*configFreshness){reload(reloadOutcomeFailed), reload(reloadOutcomeNoChange)}, want: true},
		{name: "an API write finished", before: []func(*configFreshness){apiWrite}, want: true},
		{name: "an API write does not clear a failed reload", before: []func(*configFreshness){reload(reloadOutcomeFailed), apiWrite}},
		{name: "a change began during the answer", during: (*configFreshness).beginChange},
		{name: "a change began and ended during the answer", during: apiWrite},
		// The config watcher marks an edit it saw pending without beginning a change.
		{name: "a change was marked pending during the answer", dirtyDuring: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fresh configFreshness
			for _, step := range tc.before {
				step(&fresh)
			}
			var dirty atomic.Bool
			dirty.Store(tc.dirty)
			answers := 0
			got := fresh.read(&dirty, func() {
				answers++
				if tc.during != nil {
					tc.during(&fresh)
				}
				if tc.dirtyDuring {
					dirty.Store(true)
				}
			})
			if got != tc.want {
				t.Fatalf("read = %v, want %v", got, tc.want)
			}
			if tc.want && answers != 1 {
				t.Fatalf("read answered after running answer %d times, want 1", answers)
			}
		})
	}
	t.Run("a state without a dirty flag reads on the marker alone", func(t *testing.T) {
		var fresh configFreshness
		if !fresh.read(nil, func() {}) {
			t.Fatal("read with no dirty flag refused a current config")
		}
		fresh.beginChange()
		if fresh.read(nil, func() {}) {
			t.Fatal("read with no dirty flag answered while a change is in progress")
		}
	})
	t.Run("a runtime without an API state keeps no marker", func(t *testing.T) {
		fresh := (&CityRuntime{}).freshness()
		if fresh != nil {
			t.Fatalf("freshness() of a runtime without an API state = %p, want nil", fresh)
		}
		// The reload hooks call it regardless, so each must be safe on nil.
		fresh.beginChange()
		fresh.endChange()
		fresh.beginChange()
		fresh.endReload(reloadOutcomeApplied)
	})
}

// TestControllerReadyWorkFailsWhenAServingRigIsNotHeld pins fail-loud: a
// serving rig the controller holds no store for fails the request instead of
// shrinking the answer.
func TestControllerReadyWorkFailsWhenAServingRigIsNotHeld(t *testing.T) {
	f := newReadyWorkSplitFixture(t)
	delete(f.rigs, "bravo")
	_, err := f.controller().ReadyWork(context.Background(), readywork.Query{})
	if err == nil || !strings.Contains(err.Error(), `rig "bravo"`) {
		t.Fatalf("ReadyWork without bravo's store = %v, want a failure naming the rig", err)
	}
}

// requestBoundStore stands in for a leg store bound to one request: its ready
// read waits on the request's context, as a bd read the context kills does, and
// counts itself while it waits.
type requestBoundStore struct {
	beads.Store
	ctx     context.Context
	waiting *atomic.Int64
}

func (s requestBoundStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	s.waiting.Add(1)
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

// TestControllerReadyWorkReadsEveryLegBoundToTheRequest pins request binding:
// every planned leg is read through a store bound to the request's context, so
// a request that ends (the CLI's timeout, a client that hangs up) ends the leg
// reads in flight instead of leaving them running in the supervisor.
func TestControllerReadyWorkReadsEveryLegBoundToTheRequest(t *testing.T) {
	f := newReadyWorkSplitFixture(t)
	cs := f.controller()
	// The plan, read through the production binding before the seam below is
	// installed: these stores run no subprocess, so scopedStoreLike has no clone
	// for them and each leg reads the store the controller holds.
	planned, err := cs.readyWorkLegs(context.Background())
	if err != nil {
		t.Fatalf("readyWorkLegs: %v", err)
	}
	held := map[beads.Store]bool{f.city: true, f.binding: true}
	for _, store := range f.rigs {
		held[store] = true
	}
	for _, leg := range planned {
		if !held[leg.Store] {
			t.Fatalf("leg %q reads %T, not a store the controller holds; a store that runs no subprocess is read as held", leg.Label, leg.Store)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var waiting atomic.Int64
		var mu sync.Mutex
		var bound []beads.Store
		prev := readyWorkScopedStore
		readyWorkScopedStore = func(scope context.Context, _ string, _ *config.City, store beads.Store) (beads.Store, error) {
			mu.Lock()
			defer mu.Unlock()
			bound = append(bound, store)
			return requestBoundStore{Store: store, ctx: scope, waiting: &waiting}, nil
		}
		defer func() { readyWorkScopedStore = prev }()

		var readErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, readErr = cs.ReadyWork(ctx, readywork.Query{})
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatalf("ReadyWork answered (err %v) without waiting on its request; its legs were not read through stores bound to it", readErr)
		default:
		}
		if waiting.Load() == 0 {
			t.Fatal("ReadyWork is blocked, but in no leg read bound to its request")
		}
		cancel()
		<-done
		if !errors.Is(readErr, context.Canceled) {
			t.Fatalf("ReadyWork after its request ended = %v, want the request's cancellation", readErr)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(bound) != len(planned) {
			t.Fatalf("%d leg stores were bound to the request, want every one of the %d planned", len(bound), len(planned))
		}
		for i, leg := range planned {
			if bound[i] != leg.Store {
				t.Fatalf("bound store %d is %T, want leg %q's planned store %T", i, bound[i], leg.Label, leg.Store)
			}
		}
	})
}

// readyWorkTestServer serves the API over a controller state, as the
// supervisor does, and returns a client of it.
func readyWorkTestServer(t *testing.T, cs *controllerState) *api.Client {
	t.Helper()
	mux := api.NewSupervisorMux(&singleCityStateResolver{state: cs}, nil, false, "test", "", time.Now()).WithAnyHostAllowed()
	srv := httptest.NewServer(mux.Handler())
	t.Cleanup(srv.Close)
	return api.NewCityScopedClient(srv.URL, cs.cityName)
}

// TestReadyRoutesThroughTheController pins the CLI half: with a controller up,
// `gc ready` prints the controller's answer, byte for byte what the in-process
// reader prints, without opening a store of its own. The city's rig is broken,
// so an in-process read would fail; the routed one succeeds.
func TestReadyRoutesThroughTheController(t *testing.T) {
	newReadyCityWithBrokenRig(t)
	f := newReadyWorkSplitFixture(t)
	client := readyWorkTestServer(t, f.controller())
	prev := readyAPIClient
	readyAPIClient = func(string) (*api.Client, string) { return client, "" }
	t.Cleanup(func() { readyAPIClient = prev })

	q := readywork.Query{MetadataFields: []string{"gc.routed_to=" + f.routedTo}, Unassigned: true, Limit: 20}
	var stdout, stderr bytes.Buffer
	if code := cmdReady(readyFlags{query: q}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReady = %d; stderr=%s — the routed read must not open the broken rig", code, stderr.String())
	}
	var want bytes.Buffer
	if err := writeJSON(&want, f.inProcess(t, q)); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	if stdout.String() != want.String() {
		t.Fatalf("routed gc ready printed\n%s\nwant the in-process bytes\n%s", stdout.String(), want.String())
	}
}

// TestReadyUntilUnblockedCutsTheControllersAnswer pins --until-unblocked on the
// controller route. The endpoint takes no cut, so the command asks for the whole
// window and cuts the controller's answer itself: it prints the cut of the
// in-process window, a gated claim and then the first unblocked one. The city's
// rig is broken, so an in-process read would fail; the answer is the
// controller's.
func TestReadyUntilUnblockedCutsTheControllersAnswer(t *testing.T) {
	newReadyCityWithBrokenRig(t)
	f := newReadyWorkSplitFixture(t)
	gate := mustCreateReadyBead(t, f.city, beads.Bead{Title: "open gate", Type: "task"})
	gated := mustCreateReadyBead(t, f.city, beads.Bead{Title: "gated claim", Type: "task", Priority: readyPriority(0)})
	inProgress := readywork.StatusInProgress
	if err := f.city.Update(gated.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &f.assignee}); err != nil {
		t.Fatalf("claim %s: %v", gated.ID, err)
	}
	if err := f.city.DepAdd(gated.ID, gate.ID, "blocks"); err != nil {
		t.Fatalf("gate %s: %v", gated.ID, err)
	}
	client := readyWorkTestServer(t, f.controller())
	prev := readyAPIClient
	readyAPIClient = func(string) (*api.Client, string) { return client, "" }
	t.Cleanup(func() { readyAPIClient = prev })

	q := readywork.Query{Status: readywork.StatusInProgress, Assignee: f.assignee, Limit: 20}
	var stdout, stderr bytes.Buffer
	if code := cmdReady(readyFlags{query: q, untilUnblocked: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReady(--until-unblocked) = %d; stderr=%s — the routed read must not open the broken rig", code, stderr.String())
	}
	cut := readywork.CutUntilUnblocked(f.inProcess(t, q))
	if len(cut) != 2 || cut[0].ID != gated.ID {
		t.Fatalf("the fixture window cuts to %v, want the gated claim %s then one unblocked claim", readyWireIDs(cut), gated.ID)
	}
	var want bytes.Buffer
	if err := writeJSON(&want, cut); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	if stdout.String() != want.String() {
		t.Fatalf("routed gc ready --until-unblocked printed\n%s\nwant the cut of the in-process window\n%s", stdout.String(), want.String())
	}
}

// readyCrossStoreBlocker is a blocking edge's target no store of the city
// holds, so the leg serving the row cannot resolve it.
const readyCrossStoreBlocker = "gr-elsewhere"

// newReadyUntilUnblockedCity is a file-backed city with one rig, in which the
// session sess-1 holds two claims: a resumable one in the city store, first in
// ready order, and one in the rig store blocked by readyCrossStoreBlocker.
func newReadyUntilUnblockedCity(t *testing.T) (string, beads.Bead) {
	t.Helper()
	cityDir := newReadyCityWithRigs(t, "frontend")
	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	rigStore, err := openStoreAtForCity(filepath.Join(cityDir, "rigs", "frontend"), cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	assignee := "sess-1"
	inProgress := readywork.StatusInProgress
	claim := func(store beads.Store, title string, priority int) beads.Bead {
		b := mustCreateReadyBead(t, store, beads.Bead{Title: title, Type: "task", Priority: readyPriority(priority)})
		if err := store.Update(b.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			t.Fatalf("claim %s: %v", b.ID, err)
		}
		return b
	}
	leading := claim(cityStore, "resumable claim", 0)
	broken := claim(rigStore, "claim with a cross-store blocker", 1)
	if err := rigStore.DepAdd(broken.ID, readyCrossStoreBlocker, "blocks"); err != nil {
		t.Fatalf("dep %s -> %s: %v", broken.ID, readyCrossStoreBlocker, err)
	}
	return cityDir, leading
}

// untilUnblockedWindow is the crash-recovery tier's read of sess-1's claims.
var untilUnblockedWindow = readywork.Query{Status: readywork.StatusInProgress, Assignee: "sess-1", Limit: 20}

// decodeReadyIDs decodes a gc ready answer into its ids.
func decodeReadyIDs(t *testing.T, out []byte) []string {
	t.Helper()
	var rows []readywork.ReadyBead
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("gc ready printed %q, not a JSON array: %v", out, err)
	}
	return readyWireIDs(rows)
}

// TestReadyUntilUnblockedServesTheLeadingClaimPastAnUnresolvableRow pins the
// in-process read: a claim whose blocker no store can resolve, behind the
// session's resumable claim, does not fail --until-unblocked, while the whole
// window still fails loud on it.
func TestReadyUntilUnblockedServesTheLeadingClaimPastAnUnresolvableRow(t *testing.T) {
	_, leading := newReadyUntilUnblockedCity(t)
	prev := readyAPIClient
	readyAPIClient = func(string) (*api.Client, string) { return nil, "controller-down" }
	t.Cleanup(func() { readyAPIClient = prev })

	var stdout, stderr bytes.Buffer
	if code := cmdReady(readyFlags{query: untilUnblockedWindow}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), readyCrossStoreBlocker) {
		t.Fatalf("whole window = %d (stdout %s, stderr %q), want a failure naming the unresolvable blocker", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdReady(readyFlags{query: untilUnblockedWindow, untilUnblocked: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReady(--until-unblocked) = %d; stderr=%s", code, stderr.String())
	}
	if got := decodeReadyIDs(t, stdout.Bytes()); !reflect.DeepEqual(got, []string{leading.ID}) {
		t.Fatalf("gc ready --until-unblocked = %v, want the leading claim [%s]", got, leading.ID)
	}
}

// TestReadyUntilUnblockedReadsInProcessWhenTheControllersWindowFails pins the
// fallback the controller route rests on: the controller resolves the whole
// window, so the unresolvable claim fails its read, and the command answers
// --until-unblocked from the in-process read instead.
func TestReadyUntilUnblockedReadsInProcessWhenTheControllersWindowFails(t *testing.T) {
	cityDir, leading := newReadyUntilUnblockedCity(t)
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	cityStore, err := openCityStoreAtWithConfig(cityDir, cfg)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	rigStores, err := readyRigLegStores(cfg, cityDir)
	if err != nil {
		t.Fatalf("open rig stores: %v", err)
	}
	cs := &controllerState{cfg: cfg, cityBeadStore: cityStore, beadStores: rigStores, cityName: loadedCityName(cfg, cityDir), cityPath: cityDir}
	if _, err := cs.ReadyWork(context.Background(), untilUnblockedWindow); err == nil || !strings.Contains(err.Error(), readyCrossStoreBlocker) {
		t.Fatalf("controller window = %v, want a failure naming the unresolvable blocker", err)
	}
	client := readyWorkTestServer(t, cs)
	prev := readyAPIClient
	readyAPIClient = func(string) (*api.Client, string) { return client, "" }
	t.Cleanup(func() { readyAPIClient = prev })
	t.Setenv("GC_DEBUG", "1")

	var stdout, stderr bytes.Buffer
	if code := cmdReady(readyFlags{query: untilUnblockedWindow, untilUnblocked: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReady(--until-unblocked) = %d; stderr=%s", code, stderr.String())
	}
	if got := decodeReadyIDs(t, stdout.Bytes()); !reflect.DeepEqual(got, []string{leading.ID}) {
		t.Fatalf("gc ready --until-unblocked = %v, want the leading claim [%s]", got, leading.ID)
	}
	if !strings.Contains(stderr.String(), "route=fallback reason=api-error") {
		t.Fatalf("stderr = %q, want the controller's failed window logged as the fallback", stderr.String())
	}
}

// TestReadyUntilUnblockedIsRefusedOffTheInProgressArm pins the CLI validation:
// --until-unblocked without --status in_progress is refused before any store is
// opened, so the error names the flag, not the city's dead rig.
func TestReadyUntilUnblockedIsRefusedOffTheInProgressArm(t *testing.T) {
	newReadyCityWithBrokenRig(t)
	var stdout, stderr bytes.Buffer
	if code := cmdReady(readyFlags{untilUnblocked: true}, &stdout, &stderr); code == 0 {
		t.Fatalf("gc ready --until-unblocked without --status exited 0; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--until-unblocked needs --status in_progress") {
		t.Fatalf("stderr = %q, want the refusal naming the flag", stderr.String())
	}
	if strings.Contains(stderr.String(), readyBrokenRigName) {
		t.Fatalf("stderr = %q names the dead rig: the stores were opened before the flags were validated", stderr.String())
	}
}

// TestReadyFallsBackToTheInProcessReadOnAnyControllerFailure pins the safe
// route: a controller that refuses (here a pending config reload) is answered
// by the in-process read, which on this city fails loud on its broken rig,
// exactly as `gc ready` did before the endpoint existed.
func TestReadyFallsBackToTheInProcessReadOnAnyControllerFailure(t *testing.T) {
	newReadyCityWithBrokenRig(t)
	f := newReadyWorkSplitFixture(t)
	cs := f.controller()
	var dirty atomic.Bool
	dirty.Store(true)
	cs.configDirty = &dirty
	client := readyWorkTestServer(t, cs)
	prev := readyAPIClient
	readyAPIClient = func(string) (*api.Client, string) { return client, "" }
	t.Cleanup(func() { readyAPIClient = prev })

	var stdout, stderr bytes.Buffer
	if code := cmdReady(readyFlags{}, &stdout, &stderr); code == 0 {
		t.Fatalf("cmdReady = 0 with stdout %s; the fallback read must fail loud on the broken rig", stdout.String())
	}
	if !strings.Contains(stderr.String(), readyBrokenRigName) {
		t.Fatalf("stderr = %q, want the in-process read's failure naming the broken rig", stderr.String())
	}
}

// TestReadyReadsInProcessWithoutAController pins the default: no controller,
// the in-process read, unchanged.
func TestReadyReadsInProcessWithoutAController(t *testing.T) {
	newReadyCityWithRigs(t)
	prev := readyAPIClient
	readyAPIClient = func(string) (*api.Client, string) { return nil, "controller-down" }
	t.Cleanup(func() { readyAPIClient = prev })
	var stdout, stderr bytes.Buffer
	if code := cmdReady(readyFlags{}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReady = %d; stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("stdout = %q, want the empty array of an empty city", stdout.String())
	}
}

// TestReadyFallsBackWhenTheControllerHangs pins the bound: a controller that
// accepts the request and never answers costs `gc ready` readyAPITimeout, and
// then the in-process read answers.
func TestReadyFallsBackWhenTheControllerHangs(t *testing.T) {
	newReadyCityWithRigs(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	prevClient, prevTimeout := readyAPIClient, readyAPITimeout
	readyAPIClient = func(string) (*api.Client, string) { return api.NewCityScopedClient(srv.URL, "readycounted"), "" }
	readyAPITimeout = 50 * time.Millisecond
	t.Cleanup(func() { readyAPIClient, readyAPITimeout = prevClient, prevTimeout })

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- cmdReady(readyFlags{}, &stdout, &stderr) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cmdReady = %d; stderr=%s", code, stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("gc ready was still waiting on a hung controller; the call must be bounded by readyAPITimeout")
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("stdout = %q, want the in-process answer", stdout.String())
	}
}
