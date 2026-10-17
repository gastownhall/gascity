package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/readywork"
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
	if _, err := cs.ReadyWork(context.Background(), readywork.Query{}); !errors.Is(err, errReadyWorkConfigPending) {
		t.Fatalf("ReadyWork with a pending reload = %v, want errReadyWorkConfigPending", err)
	}
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
	if code := cmdReady(q, &stdout, &stderr); code != 0 {
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
	if code := cmdReady(readywork.Query{}, &stdout, &stderr); code == 0 {
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
	if code := cmdReady(readywork.Query{}, &stdout, &stderr); code != 0 {
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
	go func() { done <- cmdReady(readywork.Query{}, &stdout, &stderr) }()
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
