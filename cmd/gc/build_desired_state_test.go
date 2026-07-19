package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

type listFailStore struct {
	beads.Store
}

func (s listFailStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list failed")
}

type routeRepairUpdateFailStore struct {
	beads.Store
	err error
}

func (s routeRepairUpdateFailStore) Update(string, beads.UpdateOpts) error {
	return s.err
}

type routeRepairCountingStore struct {
	beads.Store
	updates int
}

func (s *routeRepairCountingStore) Update(id string, opts beads.UpdateOpts) error {
	s.updates++
	return s.Store.Update(id, opts)
}

type routeRepairSelectiveFailStore struct {
	beads.Store
	failIDs map[string]bool
}

func (s *routeRepairSelectiveFailStore) Update(id string, opts beads.UpdateOpts) error {
	if s.failIDs[id] {
		return errors.New("persistent route repair failure")
	}
	return s.Store.Update(id, opts)
}

type readyFailStore struct {
	beads.Store
	readyCalls int
}

func (s *readyFailStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	s.readyCalls++
	return nil, errors.New("backing ready should not be used")
}

type readyStaticStore struct {
	beads.Store
	ready      []beads.Bead
	readyCalls int
}

func (s *readyStaticStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	s.readyCalls++
	out := make([]beads.Bead, len(s.ready))
	copy(out, s.ready)
	return out, nil
}

type controllerDemandHandlesStore struct {
	beads.Store
	handles beads.StoreHandles
}

func (s controllerDemandHandlesStore) Handles() beads.StoreHandles {
	return s.handles
}

type readyQueryRecordingStore struct {
	*beads.MemStore
	readyQueries []beads.ReadyQuery
}

func (s *readyQueryRecordingStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	if len(query) == 0 {
		s.readyQueries = append(s.readyQueries, beads.ReadyQuery{})
	} else {
		s.readyQueries = append(s.readyQueries, query[0])
	}
	return s.MemStore.Ready(query...)
}

type blockingPoolCreateStore struct {
	*beads.MemStore
	alias               string
	mu                  sync.Mutex
	createCount         int
	firstCreateStarted  chan struct{}
	secondCreateStarted chan struct{}
	releaseFirstCreate  chan struct{}
	releaseSecondCreate chan struct{}
}

func newBlockingPoolCreateStore(alias string) *blockingPoolCreateStore {
	return &blockingPoolCreateStore{
		MemStore:            beads.NewMemStore(),
		alias:               alias,
		firstCreateStarted:  make(chan struct{}),
		secondCreateStarted: make(chan struct{}),
		releaseFirstCreate:  make(chan struct{}),
		releaseSecondCreate: make(chan struct{}),
	}
}

func (s *blockingPoolCreateStore) Create(bead beads.Bead) (beads.Bead, error) {
	if bead.Type == sessionBeadType && (s.alias == "" || bead.Metadata["agent_name"] == s.alias) {
		s.mu.Lock()
		s.createCount++
		createNumber := s.createCount
		switch createNumber {
		case 1:
			close(s.firstCreateStarted)
		case 2:
			close(s.secondCreateStarted)
		}
		s.mu.Unlock()
		switch createNumber {
		case 1:
			<-s.releaseFirstCreate
		case 2:
			<-s.releaseSecondCreate
		}
	}
	return s.MemStore.Create(bead)
}

type demandListCountingStore struct {
	beads.Store
	liveInProgressIssueLists int
	liveInProgressWispLists  int
	liveOpenMolecules        int
	fullPrimeLists           int
}

func (s *demandListCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if !query.Live && query.AllowScan && query.SkipLabels && query.TierMode == beads.TierBoth {
		s.fullPrimeLists++
	}
	if query.Live && query.Status == "in_progress" {
		switch query.TierMode {
		case beads.TierWisps:
			s.liveInProgressWispLists++
		default:
			s.liveInProgressIssueLists++
		}
	}
	if query.Live && query.Status == "open" && query.Type == "molecule" {
		s.liveOpenMolecules++
	}
	return s.Store.List(query)
}

type demandRefreshFailStore struct {
	beads.Store
	failNextGet              bool
	liveInProgressIssueLists int
	liveInProgressWispLists  int
}

func (s *demandRefreshFailStore) Get(id string) (beads.Bead, error) {
	if s.failNextGet {
		s.failNextGet = false
		return beads.Bead{}, errors.New("transient get failure")
	}
	return s.Store.Get(id)
}

func (s *demandRefreshFailStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Live && query.Status == "in_progress" {
		switch query.TierMode {
		case beads.TierWisps:
			s.liveInProgressWispLists++
		default:
			s.liveInProgressIssueLists++
		}
	}
	return s.Store.List(query)
}

type cacheUnavailableListStore struct {
	*beads.MemStore
	liveInProgressIssueLists int
}

func (s *cacheUnavailableListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Live {
		if query.Status == "in_progress" {
			s.liveInProgressIssueLists++
		}
		return s.MemStore.List(query)
	}
	return nil, fmt.Errorf("cached list unavailable: %w", beads.ErrCacheUnavailable)
}

type partialAssignedWorkStore struct {
	*beads.MemStore
	partialInProgress bool
	partialReady      bool
}

type controllerDemandPartialStore struct {
	*beads.MemStore
}

func (s *controllerDemandPartialStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.Ready(query...)
	if err != nil {
		return nil, err
	}
	if len(query) == 0 || (query[0].Assignee == "" && query[0].Limit == 0) {
		return rows, &beads.PartialResultError{Op: "bd ready", Err: errors.New("skipped corrupt controller demand bead")}
	}
	return rows, nil
}

type acpOnlyDesiredStateProvider struct {
	*runtime.Fake
}

func (p *acpOnlyDesiredStateProvider) SupportsTransport(transport string) bool {
	return transport == config.SessionTransportACP
}

func (s *partialAssignedWorkStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(query)
	if err != nil {
		return nil, err
	}
	if s.partialInProgress && query.Status == "in_progress" {
		return rows, &beads.PartialResultError{Op: "bd list", Err: errors.New("skipped corrupt in-progress bead")}
	}
	return rows, nil
}

func (s *partialAssignedWorkStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.Ready(query...)
	if err != nil {
		return nil, err
	}
	if s.partialReady {
		return rows, &beads.PartialResultError{Op: "bd ready", Err: errors.New("skipped corrupt ready bead")}
	}
	return rows, nil
}

// partialSessionListStore returns a PartialResultError from every List so the
// session collection can be exercised on its degraded-but-non-empty path.
type partialSessionListStore struct {
	*beads.MemStore
}

func (s *partialSessionListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(query)
	if err != nil {
		return nil, err
	}
	return rows, &beads.PartialResultError{Op: "bd list", Err: errors.New("skipped corrupt session bead")}
}

// TestCollectAllOpenSessionInfos pins the collection edge that projects session
// beads onto session.Info: closed beads are dropped, the projected fields carry
// the bead metadata verbatim, a partial-result store still contributes its
// partial non-closed slice while joining its error, and suspended rig stores
// are skipped.
func TestCollectAllOpenSessionInfos(t *testing.T) {
	t.Run("filters_closed_and_projects_fields_verbatim", func(t *testing.T) {
		cityStore := beads.NewMemStore()
		open, err := cityStore.Create(beads.Bead{
			Status: "open", Type: sessionBeadType,
			Metadata: map[string]string{
				"template":     "worker",
				"state":        "active",
				"pool_managed": "true",
			},
		})
		if err != nil {
			t.Fatalf("create open session bead: %v", err)
		}
		closedBead, err := cityStore.Create(beads.Bead{
			Type:     sessionBeadType,
			Metadata: map[string]string{"template": "worker", "state": "asleep"},
		})
		if err != nil {
			t.Fatalf("create session bead to close: %v", err)
		}
		if err := cityStore.Close(closedBead.ID); err != nil {
			t.Fatalf("close session bead: %v", err)
		}

		infos, err := collectAllOpenSessionInfos(&config.City{}, cityStore, nil, nil)
		if err != nil {
			t.Fatalf("collectAllOpenSessionInfos: %v", err)
		}
		if len(infos) != 1 {
			t.Fatalf("collectAllOpenSessionInfos returned %d infos, want 1 (closed filtered): %#v", len(infos), infos)
		}
		got := infos[0]
		if got.ID != open.ID {
			t.Fatalf("projected ID = %q, want %q", got.ID, open.ID)
		}
		if got.Template != "worker" {
			t.Fatalf("projected Template = %q, want %q", got.Template, "worker")
		}
		if !got.PoolManaged {
			t.Fatal("projected PoolManaged = false, want true")
		}
		if got.MetadataState != "active" {
			t.Fatalf("projected MetadataState = %q, want %q", got.MetadataState, "active")
		}
	})

	t.Run("partial_result_contributes_slice_and_joins_error", func(t *testing.T) {
		backing := beads.NewMemStore()
		created, err := backing.Create(beads.Bead{
			Status: "open", Type: sessionBeadType,
			Metadata: map[string]string{"template": "worker", "state": "active"},
		})
		if err != nil {
			t.Fatalf("create session bead: %v", err)
		}
		store := &partialSessionListStore{MemStore: backing}

		infos, err := collectAllOpenSessionInfos(&config.City{}, store, nil, nil)
		if err == nil {
			t.Fatal("collectAllOpenSessionInfos returned nil error on a partial-result store")
		}
		if !beads.IsPartialResult(err) {
			t.Fatalf("collectAllOpenSessionInfos error = %v, want a joined PartialResultError", err)
		}
		if len(infos) != 1 {
			t.Fatalf("collectAllOpenSessionInfos returned %d infos, want 1 partial contribution: %#v", len(infos), infos)
		}
		if infos[0].ID != created.ID {
			t.Fatalf("partial contribution ID = %q, want %q", infos[0].ID, created.ID)
		}
	})

	t.Run("skips_suspended_rig_stores", func(t *testing.T) {
		rigPath := filepath.Clean("/c/rigs/rig-A")
		cfg := &config.City{Rigs: []config.Rig{{Name: "rig-A", Path: rigPath}}}

		cityStore := beads.NewMemStore()
		cityBead, err := cityStore.Create(beads.Bead{
			Status: "open", Type: sessionBeadType,
			Metadata: map[string]string{"template": "worker", "state": "active"},
		})
		if err != nil {
			t.Fatalf("create city session bead: %v", err)
		}
		rigStore := beads.NewMemStore()
		if _, err := rigStore.Create(beads.Bead{
			Status: "open", Type: sessionBeadType,
			Metadata: map[string]string{"template": "rig-A/worker", "state": "active"},
		}); err != nil {
			t.Fatalf("create rig session bead: %v", err)
		}
		rigStores := map[string]beads.Store{"rig-A": rigStore}

		// Control: with the rig live, both sessions are collected.
		liveInfos, err := collectAllOpenSessionInfos(cfg, cityStore, rigStores, nil)
		if err != nil {
			t.Fatalf("collectAllOpenSessionInfos (live rig): %v", err)
		}
		if len(liveInfos) != 2 {
			t.Fatalf("collectAllOpenSessionInfos (live rig) returned %d infos, want 2: %#v", len(liveInfos), liveInfos)
		}

		// Suspending the rig drops its store from the fan-out.
		suspended := map[string]bool{rigPath: true}
		infos, err := collectAllOpenSessionInfos(cfg, cityStore, rigStores, suspended)
		if err != nil {
			t.Fatalf("collectAllOpenSessionInfos (suspended rig): %v", err)
		}
		if len(infos) != 1 || infos[0].ID != cityBead.ID {
			t.Fatalf("collectAllOpenSessionInfos returned %#v, want only the city session (suspended rig skipped)", infos)
		}
	})
}

func TestCollectAssignedWorkBeads_IncludesReadyOpenAssignedHandoff(t *testing.T) {
	store := beads.NewMemStore()
	handoff, err := store.Create(beads.Bead{
		Title:    "merge me",
		Type:     "task",
		Status:   "open",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("create handoff bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "queued pool work",
		Type:   "task",
		Status: "open",
	}); err != nil {
		t.Fatalf("create queued bead: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, store)
	if len(got) != 1 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 1: %#v", len(got), got)
	}
	if got[0].ID != handoff.ID {
		t.Fatalf("collectAssignedWorkBeads returned %q, want %q", got[0].ID, handoff.ID)
	}
	if got[0].Assignee != "repo/refinery" || got[0].Status != "open" {
		t.Fatalf("assigned handoff bead = assignee %q status %q, want repo/refinery open", got[0].Assignee, got[0].Status)
	}
}

func TestCollectAssignedWorkBeadsIncludesAssignedInProgressWisp(t *testing.T) {
	store := beads.NewMemStore()
	wisp, err := store.Create(beads.Bead{
		Title:     "active workflow step",
		Type:      "task",
		Status:    "in_progress",
		Assignee:  "worker-session",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	})
	if err != nil {
		t.Fatalf("create wisp work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(wisp.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark wisp in progress: %v", err)
	}

	got, partial := collectAssignedWorkBeads(&config.City{}, store)

	if partial {
		t.Fatal("collectAssignedWorkBeads reported partial results")
	}
	if len(got) != 1 || got[0].ID != wisp.ID {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want in-progress wisp %s", got, wisp.ID)
	}
}

func TestCollectAssignedWorkBeadsIncludesReadyOpenAssignedWisp(t *testing.T) {
	store := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	wisp, err := store.Create(beads.Bead{
		Title:     "workflow control",
		Type:      "task",
		Status:    "open",
		Assignee:  "control-dispatcher",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.kind": "retry",
		},
	})
	if err != nil {
		t.Fatalf("create wisp work: %v", err)
	}
	cfg := &config.City{
		NamedSessions: []config.NamedSession{{
			Template: "control-dispatcher",
			Mode:     "on_demand",
		}},
	}

	got, partial := collectAssignedWorkBeads(cfg, store)

	if partial {
		t.Fatal("collectAssignedWorkBeads reported partial results")
	}
	if len(got) != 1 || got[0].ID != wisp.ID {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want ready wisp %s", got, wisp.ID)
	}
	if len(store.readyQueries) == 0 {
		t.Fatal("Ready was not queried")
	}
	for _, query := range store.readyQueries {
		if query.TierMode != beads.TierBoth {
			t.Fatalf("Ready query TierMode = %v, want TierBoth; queries=%#v", query.TierMode, store.readyQueries)
		}
	}
}

func TestCollectAssignedWorkBeadsIncludesOpenAssignedRootOnlyMoleculeWisp(t *testing.T) {
	store := beads.NewMemStore()
	wisp, err := store.Create(beads.Bead{
		Title:     "refinery patrol",
		Type:      "molecule",
		Status:    "open",
		Assignee:  "repo/refinery",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("create root-only wisp: %v", err)
	}
	cfg := &config.City{
		NamedSessions: []config.NamedSession{{
			Template: "repo/refinery",
			Mode:     "on_demand",
		}},
	}

	got, partial := collectAssignedWorkBeads(cfg, store)

	if partial {
		t.Fatal("collectAssignedWorkBeads reported partial results")
	}
	if len(got) != 1 || got[0].ID != wisp.ID {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want assigned root-only molecule wisp %s", got, wisp.ID)
	}
}

func TestCollectAssignedWorkBeadsIgnoresOpenAssignedMoleculeContainer(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "workflow container",
		Type:     "molecule",
		Status:   "open",
		Assignee: "repo/refinery",
	}); err != nil {
		t.Fatalf("create molecule container: %v", err)
	}
	cfg := &config.City{
		NamedSessions: []config.NamedSession{{
			Template: "repo/refinery",
			Mode:     "on_demand",
		}},
	}

	got, partial := collectAssignedWorkBeads(cfg, store)

	if partial {
		t.Fatal("collectAssignedWorkBeads reported partial results")
	}
	if len(got) != 0 {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want assigned non-wisp molecule container ignored", got)
	}
}

func TestCollectAssignedWorkBeadsUsesLiveReadyAfterExternalDependencyClose(t *testing.T) {
	backing := beads.NewMemStore()
	blocker, err := backing.Create(beads.Bead{
		Title:  "first attempt",
		Type:   "task",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	retry, err := backing.Create(beads.Bead{
		Title:     "retry controller",
		Type:      "task",
		Status:    "open",
		Assignee:  "control-dispatcher",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.kind": "retry",
		},
	})
	if err != nil {
		t.Fatalf("create retry: %v", err)
	}
	if err := backing.DepAdd(retry.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("add retry blocker: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	closed := "closed"
	if err := backing.Update(blocker.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close blocker outside cache: %v", err)
	}

	got, partial := collectAssignedWorkBeads(&config.City{}, cache)

	if partial {
		t.Fatal("collectAssignedWorkBeads reported partial results")
	}
	if len(got) != 1 || got[0].ID != retry.ID {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want live-ready retry %s after blocker close", got, retry.ID)
	}
}

func TestCollectAssignedWorkBeadsUsesLiveReadyReadModel(t *testing.T) {
	backing := &readyStaticStore{Store: beads.NewMemStore()}
	handoff, err := backing.Create(beads.Bead{
		Title:    "merge me",
		Type:     "task",
		Status:   "open",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("create handoff bead: %v", err)
	}
	backing.ready = []beads.Bead{handoff}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, cache)
	if len(got) != 1 || got[0].ID != handoff.ID {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want [%s]", got, handoff.ID)
	}
	if backing.readyCalls == 0 {
		t.Fatal("backing Ready was not called; assigned ready demand must use live state")
	}
}

func TestCollectAssignedWorkBeadsUsesCachedInProgressReadModel(t *testing.T) {
	backing := &demandListCountingStore{Store: beads.NewMemStore()}
	work, err := backing.Create(beads.Bead{
		Title:    "active handoff",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("create active bead: %v", err)
	}
	if err := backing.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set active bead in_progress: %v", err)
	}
	work, err = backing.Get(work.ID)
	if err != nil {
		t.Fatalf("reload active bead: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, cache)
	if len(got) != 1 || got[0].ID != work.ID {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want [%s]", got, work.ID)
	}
	if backing.liveInProgressIssueLists != 0 {
		t.Fatalf("live issue in_progress list calls = %d, want cached demand read", backing.liveInProgressIssueLists)
	}
	if backing.liveInProgressWispLists != 0 {
		t.Fatalf("live wisp in_progress list calls = %d, want cached demand read", backing.liveInProgressWispLists)
	}
	if backing.fullPrimeLists != 0 {
		t.Fatalf("full-prime list calls = %d, want controller demand to use PrimeActive snapshot", backing.fullPrimeLists)
	}
}

func TestCollectAssignedWorkBeadsReprimesWhenCachedInProgressDirty(t *testing.T) {
	backing := &demandRefreshFailStore{Store: beads.NewMemStore()}
	work, err := backing.Create(beads.Bead{
		Title: "handoff becomes active",
		Type:  "task",
	})
	if err != nil {
		t.Fatalf("create active bead: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	status := "in_progress"
	assignee := "repo/refinery"
	backing.failNextGet = true
	if err := cache.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(active): %v", err)
	}

	got, partial := collectAssignedWorkBeads(&config.City{}, cache)
	if partial {
		t.Fatal("collectAssignedWorkBeads reported partial with successful cache reprime")
	}
	if len(got) != 1 || got[0].ID != work.ID || got[0].Status != "in_progress" || got[0].Assignee != "repo/refinery" {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want reprime in-progress %s", got, work.ID)
	}
	if backing.liveInProgressIssueLists != 0 {
		t.Fatalf("live issue in_progress list calls = %d, want shared cache reprime", backing.liveInProgressIssueLists)
	}
	if backing.liveInProgressWispLists != 0 {
		t.Fatalf("live wisp in_progress list calls = %d, want shared cache reprime", backing.liveInProgressWispLists)
	}
}

func TestCollectAssignedWorkBeadsFallsBackToLiveWhenCachedInProgressUnavailable(t *testing.T) {
	store := &cacheUnavailableListStore{MemStore: beads.NewMemStore()}
	work, err := store.Create(beads.Bead{
		Title:    "active handoff",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("create active bead: %v", err)
	}
	status := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("mark active bead in_progress: %v", err)
	}

	got, partial := collectAssignedWorkBeads(&config.City{}, store)
	if partial {
		t.Fatal("collectAssignedWorkBeads reported partial with successful live fallback")
	}
	if len(got) != 1 || got[0].ID != work.ID || got[0].Status != "in_progress" || got[0].Assignee != "repo/refinery" {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want live fallback in-progress %s", got, work.ID)
	}
	if store.liveInProgressIssueLists == 0 {
		t.Fatal("live issue in_progress list calls = 0, want live fallback after cache unavailable")
	}
}

func TestCollectAssignedWorkBeads_ExcludesBlockedOpenAssignedHandoff(t *testing.T) {
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{
		Title:  "blocker",
		Type:   "task",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("create blocker bead: %v", err)
	}
	handoff, err := store.Create(beads.Bead{
		Title:    "merge me later",
		Type:     "task",
		Status:   "open",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("create handoff bead: %v", err)
	}
	if err := store.DepAdd(handoff.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("add blocking dep: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, store)
	if len(got) != 0 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 0: %#v", len(got), got)
	}
}

func TestDefaultScaleCheckCountsKeepsCachedRowsWhenLiveFreshnessFails(t *testing.T) {
	backing := &readyFailStore{Store: beads.NewMemStore()}
	if _, err := backing.Create(beads.Bead{
		Title:  "queued routed work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "gascity/workflows.codex-min",
		},
	}); err != nil {
		t.Fatalf("create routed bead: %v", err)
	}
	const template = "gascity/workflows.codex-min"
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, partialTemplates, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 1 || !beads.IsPartialResult(errs[0]) {
		t.Fatalf("defaultScaleCheckCounts errs = %v, want one partial live-freshness error", errs)
	}
	if !partialTemplates[template] {
		t.Fatalf("partialTemplates = %v, want %q marked partial", partialTemplates, template)
	}
	if got := counts[template]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts = %d, want 1", got)
	}
	if backing.readyCalls != 1 {
		t.Fatalf("backing Ready calls = %d, want one live freshness read", backing.readyCalls)
	}
}

func TestDefaultScaleCheckCountsSeesExternalRoutedWorkAfterCachePrime(t *testing.T) {
	const template = "gascity/workflows.codex-min"
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("initial defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 0 {
		t.Fatalf("initial defaultScaleCheckCounts[%q] = %d, want 0", template, got)
	}
	if _, err := backing.Create(beads.Bead{
		Title:  "manual order run wisp",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": template,
		},
	}); err != nil {
		t.Fatalf("create externally routed bead: %v", err)
	}

	counts, _, errs = defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 1 for post-prime external routed work", template, got)
	}
}

func TestDefaultScaleCheckDemandCarriesTriggerBeadID(t *testing.T) {
	const template = "gascity/workflows.codex-min"
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:  "manual order run wisp",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.PackMetadataKey:          "packer",
			beadmeta.PackWorkspaceMetadataKey: "existing-workspace",
			"gc.routed_to":                    template,
		},
	})
	if err != nil {
		t.Fatalf("create routed bead: %v", err)
	}

	counts, demand, _, errs := defaultScaleCheckCountsAndDemand([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    store,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCountsAndDemand errs = %v", errs)
	}
	if got := counts[template]; got != 1 {
		t.Fatalf("defaultScaleCheckCountsAndDemand[%q] = %d, want 1", template, got)
	}
	if got := demand[template].WorkBeadIDs; !reflect.DeepEqual(got, []string{work.ID}) {
		t.Fatalf("WorkBeadIDs = %v, want [%s]", got, work.ID)
	}
	if got := demand[template].Titles[work.ID]; got != "manual order run wisp" {
		t.Fatalf("Titles[%s] = %q, want manual order run wisp", work.ID, got)
	}
	if got := demand[template].Packs[work.ID]; got != "packer" {
		t.Fatalf("Packs[%s] = %q, want packer", work.ID, got)
	}
	if got := demand[template].Workspaces[work.ID]; got != "existing-workspace" {
		t.Fatalf("Workspaces[%s] = %q, want existing-workspace", work.ID, got)
	}
	if got := demand[template].StoreRefs[work.ID]; got != "rig:gascity" {
		t.Fatalf("StoreRefs[%s] = %q, want rig:gascity", work.ID, got)
	}
}

func TestDefaultScaleCheckCountsUsesLiveReadyWhenCachedRowWasAssigned(t *testing.T) {
	const template = "gascity/workflows.codex-min"
	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{
		Title:  "routed pool work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": template,
		},
	})
	if err != nil {
		t.Fatalf("create routed bead: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	assignee := "worker-session"
	if err := backing.Update(work.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("assign live bead: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 0 after live row was assigned", template, got)
	}
}

func TestDefaultScaleCheckCountsUsesLiveReadyWhenCachedRowWasClosed(t *testing.T) {
	const template = "gascity/workflows.codex-min"
	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{
		Title:  "routed pool work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": template,
		},
	})
	if err != nil {
		t.Fatalf("create routed bead: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if err := backing.Close(work.ID); err != nil {
		t.Fatalf("close live bead: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 0 after live row was closed", template, got)
	}
}

func TestDefaultScaleCheckCountsUsesLiveReadyWhenCachedRowWasRerouted(t *testing.T) {
	const (
		staleTemplate = "gascity/workflows.codex-min"
		liveTemplate  = "gascity/workflows.claude-min"
	)
	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{
		Title:  "routed pool work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": staleTemplate,
		},
	})
	if err != nil {
		t.Fatalf("create routed bead: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if err := backing.Update(work.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.routed_to": liveTemplate}}); err != nil {
		t.Fatalf("reroute live bead: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{
		{template: staleTemplate, storeKey: "rig:gascity", store: cache},
		{template: liveTemplate, storeKey: "rig:gascity", store: cache},
	})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[staleTemplate]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 0 after live row was rerouted", staleTemplate, got)
	}
	if got := counts[liveTemplate]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 1 after live row was rerouted", liveTemplate, got)
	}
}

func TestDefaultScaleCheckCountsTreatsCompleteLiveReadyAsAuthoritative(t *testing.T) {
	const (
		staleTemplate = "gascity/workflows.codex-min"
		liveTemplate  = "gascity/workflows.claude-min"
	)
	routed := func(id, route, assignee string) beads.Bead {
		return beads.Bead{
			ID:       id,
			Title:    "routed pool work",
			Type:     "task",
			Status:   "open",
			Assignee: assignee,
			Metadata: map[string]string{
				"gc.routed_to": route,
			},
		}
	}

	tests := []struct {
		name       string
		cachedRows []beads.Bead
		liveRows   []beads.Bead
		wantStale  int
		wantLive   int
	}{
		{
			name:       "assigned live row overrides cached unassigned demand",
			cachedRows: []beads.Bead{routed("bd-1", staleTemplate, "")},
			liveRows:   []beads.Bead{routed("bd-1", staleTemplate, "worker-session")},
		},
		{
			name:       "closed live row removes cached demand",
			cachedRows: []beads.Bead{routed("bd-1", staleTemplate, "")},
			liveRows:   nil,
		},
		{
			name:       "rerouted live row overrides cached route metadata",
			cachedRows: []beads.Bead{routed("bd-1", staleTemplate, "")},
			liveRows:   []beads.Bead{routed("bd-1", liveTemplate, "")},
			wantLive:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := controllerDemandHandlesStore{
				Store: beads.NewMemStore(),
				handles: beads.StoreHandles{
					Cached: &readyStaticStore{ready: tt.cachedRows},
					Live:   &readyStaticStore{ready: tt.liveRows},
				},
			}

			counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{
				{template: staleTemplate, storeKey: "rig:gascity", store: store},
				{template: liveTemplate, storeKey: "rig:gascity", store: store},
			})
			if len(errs) != 0 {
				t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
			}
			if got := counts[staleTemplate]; got != tt.wantStale {
				t.Fatalf("defaultScaleCheckCounts[%q] = %d, want %d", staleTemplate, got, tt.wantStale)
			}
			if got := counts[liveTemplate]; got != tt.wantLive {
				t.Fatalf("defaultScaleCheckCounts[%q] = %d, want %d", liveTemplate, got, tt.wantLive)
			}
		})
	}
}

func TestMergeReadyRowsByIDPrefersSecondaryRows(t *testing.T) {
	tests := []struct {
		name      string
		primary   []beads.Bead
		secondary []beads.Bead
		wantIDs   []string
		wantRoute string
	}{
		{
			name:      "secondary duplicate replaces primary",
			primary:   []beads.Bead{{ID: "bd-1", Metadata: map[string]string{"gc.routed_to": "stale"}}},
			secondary: []beads.Bead{{ID: "bd-1", Metadata: map[string]string{"gc.routed_to": "live"}}},
			wantIDs:   []string{"bd-1"},
			wantRoute: "live",
		},
		{
			name:      "primary-only row survives",
			primary:   []beads.Bead{{ID: "bd-1"}, {ID: ""}},
			secondary: []beads.Bead{{ID: "bd-2"}},
			wantIDs:   []string{"bd-2", "bd-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeReadyRowsByID(tt.primary, tt.secondary)
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("mergeReadyRowsByID returned %d rows, want %d: %#v", len(got), len(tt.wantIDs), got)
			}
			for i, wantID := range tt.wantIDs {
				if got[i].ID != wantID {
					t.Fatalf("mergeReadyRowsByID row %d ID = %q, want %q: %#v", i, got[i].ID, wantID, got)
				}
			}
			if tt.wantRoute != "" && got[0].Metadata["gc.routed_to"] != tt.wantRoute {
				t.Fatalf("mergeReadyRowsByID route = %q, want %q", got[0].Metadata["gc.routed_to"], tt.wantRoute)
			}
		})
	}
}

func TestDefaultScaleCheckCountsCountsUnassignedRoutedPoolWork(t *testing.T) {
	const template = "gascity/reviewer"
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{
		Title:  "routed pool work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": template,
		},
	}); err != nil {
		t.Fatalf("create handoff bead: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 1", template, got)
	}
}

func TestDefaultScaleCheckCountsCountsRoutedVaporWispViaReady(t *testing.T) {
	const template = "maintenance.dog"
	backing := &demandListCountingStore{Store: beads.NewMemStore()}
	if _, err := backing.Create(beads.Bead{
		Title:  "mol-dog-stale-db",
		Type:   "task",
		Ref:    "mol-dog-stale-db",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":      "wisp",
			"gc.routed_to": template,
		},
	}); err != nil {
		t.Fatalf("create vapor wisp root: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "city",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 1", template, got)
	}
	if backing.liveOpenMolecules != 0 {
		t.Fatalf("live open molecule list calls = %d, want Ready-only demand", backing.liveOpenMolecules)
	}
}

func TestDefaultScaleCheckCountsCountsUnassignedRoutedTaskWisp(t *testing.T) {
	const template = "gascity/reviewer"
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{
		Title:     "routed ephemeral pool work",
		Type:      "task",
		Status:    "open",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.routed_to": template,
		},
	}); err != nil {
		t.Fatalf("create routed wisp: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 1 for task-shaped wisp", template, got)
	}
}

func TestDefaultScaleCheckCountsCountsRunTargetOnlyWorkflowDuringMigration(t *testing.T) {
	const template = "gascity/reviewer"
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{
		Title:  "legacy workflow root",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.run_target": template,
		},
	}); err != nil {
		t.Fatalf("create legacy workflow root: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 1", template, got)
	}
}

func TestDefaultScaleCheckCountsIgnoresRunTargetOnNonWorkflowDivergentWork(t *testing.T) {
	const (
		entryTarget = "gascity/controller"
		stepTarget  = "gascity/reviewer"
	)
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{
		Title:  "legacy divergent step",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to":  entryTarget,
			"gc.run_target": stepTarget,
		},
	}); err != nil {
		t.Fatalf("create legacy divergent step: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{
		{template: entryTarget, storeKey: "rig:gascity", store: cache},
		{template: stepTarget, storeKey: "rig:gascity", store: cache},
	})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[entryTarget]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 1", entryTarget, got)
	}
	if got := counts[stepTarget]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 0", stepTarget, got)
	}
}

func TestDefaultScaleCheckCountsIgnoresRunTargetWhenWorkflowRoutedToPresent(t *testing.T) {
	const (
		entryTarget = "gascity/controller"
		staleTarget = "gascity/reviewer"
	)
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{
		Title:  "divergent workflow root",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.routed_to":  entryTarget,
			"gc.run_target": staleTarget,
		},
	}); err != nil {
		t.Fatalf("create divergent workflow root: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{
		{template: entryTarget, storeKey: "rig:gascity", store: cache},
		{template: staleTarget, storeKey: "rig:gascity", store: cache},
	})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[entryTarget]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 1", entryTarget, got)
	}
	if got := counts[staleTarget]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 0", staleTarget, got)
	}
}

func TestDefaultScaleCheckCountsFallsBackToRoutedToWhenRunTargetDoesNotMatchTemplate(t *testing.T) {
	const template = "gascity/reviewer"
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{
		Title:  "new graph step",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.run_target": "reviewer",
			"gc.routed_to":  template,
		},
	}); err != nil {
		t.Fatalf("create graph step: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 1", template, got)
	}
}

func TestDefaultScaleCheckCountsDoesNotTreatTemplateAssigneeAsDemand(t *testing.T) {
	const template = "gascity/reviewer"
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{
		Title:    "assigned routed work",
		Type:     "task",
		Status:   "open",
		Assignee: template,
		Metadata: map[string]string{
			"gc.routed_to": template,
		},
	}); err != nil {
		t.Fatalf("create assigned bead: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 0 (assignee must be a concrete session identity, never pool demand)", template, got)
	}
}

// TestDefaultScaleCheckCountsExcludesBeadsAssignedToSession pins the
// invariant that beads with assignee==<session-id> (Path 1 territory) are
// NOT counted by defaultScaleCheckCounts, to avoid double-counting when
// collectAssignedWorkBeadsWithStores has already counted them.
func TestDefaultScaleCheckCountsExcludesBeadsAssignedToSession(t *testing.T) {
	const template = "gascity/reviewer"
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{
		Title:    "in-flight session work",
		Type:     "task",
		Status:   "open",
		Assignee: "reviewer-gc-12345",
		Metadata: map[string]string{
			"gc.routed_to": template,
		},
	}); err != nil {
		t.Fatalf("create in-flight bead: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts[template]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 0 (session-identity assignee is Path 1's territory; counting here would double-count)", template, got)
	}
}

func TestDefaultScaleCheckCountsIgnoresOpenMoleculeContainers(t *testing.T) {
	backing := &demandListCountingStore{Store: beads.NewMemStore()}
	if _, err := backing.Create(beads.Bead{
		Title:  "workflow root",
		Type:   "molecule",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "gascity/workflows.codex-min",
		},
	}); err != nil {
		t.Fatalf("create molecule bead: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: "gascity/workflows.codex-min",
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts["gascity/workflows.codex-min"]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts = %d, want molecule container ignored", got)
	}
	if backing.liveOpenMolecules != 0 {
		t.Fatalf("live open molecule list calls = %d, want no molecule demand query", backing.liveOpenMolecules)
	}
}

// TestDefaultScaleCheckCountsIgnoresGraphV2StepRoutedToPool pins the
// "graph.v2 step → not counted" invariant @sjarmak enumerated in the
// Option B design discussion on PR #2531. readyExcludeTypes filters Type:"step"
// out of Ready() per PR #1154, so a routed graph step is workflow scaffolding,
// not standalone pool demand.
//
// Guards against future regressions where step gets removed from
// readyExcludeTypes.
func TestDefaultScaleCheckCountsIgnoresGraphV2StepRoutedToPool(t *testing.T) {
	backing := &demandListCountingStore{Store: beads.NewMemStore()}
	if _, err := backing.Create(beads.Bead{
		Title:  "graph.v2 step routed to dog",
		Type:   "step",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "dog",
			"gc.kind":      "workflow",
		},
	}); err != nil {
		t.Fatalf("create graph.v2 step: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: "dog",
		storeKey: "city",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts["dog"]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts[%q] = %d, want 0 (graph.v2 step routed to a pool is not pool demand — it's workflow scaffolding handled by the parent agent)", "dog", got)
	}
}

func TestDefaultScaleCheckCountsHonorsCachedWriteThroughDependencies(t *testing.T) {
	const template = "gascity/workflows.codex-max"
	backing := &readyFailStore{Store: beads.NewMemStore()}
	blocker, err := backing.Create(beads.Bead{
		Title:  "blocked earlier step",
		Type:   "task",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := backing.Create(beads.Bead{
		Title:  "future routed work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": template,
		},
	})
	if err != nil {
		t.Fatalf("create blocked: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if err := cache.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	counts, partialTemplates, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 1 || !beads.IsPartialResult(errs[0]) {
		t.Fatalf("defaultScaleCheckCounts errs = %v, want one partial live-freshness error", errs)
	}
	if !partialTemplates[template] {
		t.Fatalf("partialTemplates = %v, want %q marked partial", partialTemplates, template)
	}
	if got := counts[template]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts = %d, want blocked future work excluded", got)
	}
	if backing.readyCalls != 1 {
		t.Fatalf("backing Ready calls = %d, want one live freshness read", backing.readyCalls)
	}
}

func TestDefaultScaleCheckCountsFallsBackToLiveWhenCachedEventDepsUnknown(t *testing.T) {
	backing := &readyStaticStore{
		Store: beads.NewMemStore(),
		ready: []beads.Bead{{
			ID:     "gc-ready",
			Title:  "ready routed work",
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": "gascity/workflows.codex-max",
			},
		}},
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.ApplyEvent("bead.created", []byte(`{"id":"gc-blocked","status":"open","metadata":{"gc.routed_to":"gascity/workflows.codex-max"}}`))

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: "gascity/workflows.codex-max",
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v, want successful live fallback", errs)
	}
	if got := counts["gascity/workflows.codex-max"]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts = %d, want live fallback count", got)
	}
	if backing.readyCalls == 0 {
		t.Fatal("backing Ready calls = 0, want live ready fallback")
	}
}

func TestDefaultScaleCheckCountsUsesPartialReadyRows(t *testing.T) {
	store := &partialAssignedWorkStore{MemStore: beads.NewMemStore(), partialReady: true}
	if _, err := store.Create(beads.Bead{
		Title:  "queued routed work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "gascity/workflows.codex-max",
		},
	}); err != nil {
		t.Fatalf("create routed bead: %v", err)
	}

	counts, partialTemplates, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: "gascity/workflows.codex-max",
		storeKey: "rig:gascity",
		store:    store,
	}})
	if got := counts["gascity/workflows.codex-max"]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts = %d, want survivor row counted", got)
	}
	if len(errs) != 1 || !beads.IsPartialResult(errs[0]) {
		t.Fatalf("defaultScaleCheckCounts errs = %v, want partial-result diagnostic", errs)
	}
	if !partialTemplates["gascity/workflows.codex-max"] {
		t.Fatalf("partialTemplates = %v, want affected template marked partial", partialTemplates)
	}
}

func TestDefaultScaleCheckCountsReadyErrorNamesAffectedTemplates(t *testing.T) {
	store := &readyFailStore{Store: beads.NewMemStore()}

	_, partialTemplates, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{
		{template: "gascity/workflows.codex-min", storeKey: "rig:gascity", store: store},
		{template: "gascity/workflows.codex-max", storeKey: "rig:gascity", store: store},
	})
	if len(errs) != 1 {
		t.Fatalf("defaultScaleCheckCounts errs = %v, want one grouped Ready diagnostic", errs)
	}
	msg := errs[0].Error()
	for _, want := range []string{"rig:gascity", "gascity/workflows.codex-min", "gascity/workflows.codex-max"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("defaultScaleCheckCounts err = %q, want affected template %q", msg, want)
		}
	}
	for _, want := range []string{"gascity/workflows.codex-min", "gascity/workflows.codex-max"} {
		if !partialTemplates[want] {
			t.Fatalf("partialTemplates = %v, want %q marked partial", partialTemplates, want)
		}
	}
}

func TestDefaultNamedSessionDemandRecordsPartialWithoutRoutedDemand(t *testing.T) {
	store := &partialAssignedWorkStore{MemStore: beads.NewMemStore(), partialReady: true}
	if _, err := store.Create(beads.Bead{
		Title:  "queued worker work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	}); err != nil {
		t.Fatalf("create routed bead: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "worker",
		}},
		NamedSessions: []config.NamedSession{{
			Name:     "primary",
			Template: "worker",
			Mode:     "on_demand",
		}},
	}

	demand, partialTemplates, errs := defaultNamedSessionDemand([]defaultScaleCheckTarget{{
		template: "worker",
		storeKey: "rig:gascity",
		store:    store,
	}}, cfg, "test-city")
	if len(demand) != 0 {
		t.Fatalf("defaultNamedSessionDemand = %v, want no named demand from routed metadata", demand)
	}
	if len(errs) != 1 || !beads.IsPartialResult(errs[0]) {
		t.Fatalf("defaultNamedSessionDemand errs = %v, want partial-result diagnostic", errs)
	}
	msg := errs[0].Error()
	for _, want := range []string{"rig:gascity", "worker"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("defaultNamedSessionDemand err = %q, want affected template %q", msg, want)
		}
	}
	if !partialTemplates["worker"] {
		t.Fatalf("partialTemplates = %v, want worker marked partial", partialTemplates)
	}
}

func TestDefaultNamedSessionDemandIgnoresNamedIdentityRunTargetOnlyWorkflow(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "legacy workflow root",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.run_target": "primary",
		},
	}); err != nil {
		t.Fatalf("create routed bead: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "reviewer",
		}},
		NamedSessions: []config.NamedSession{{
			Name:     "primary",
			Template: "reviewer",
			Mode:     "on_demand",
		}},
	}

	demand, _, errs := defaultNamedSessionDemand([]defaultScaleCheckTarget{{
		template: "reviewer",
		storeKey: "rig:gascity",
		store:    store,
	}}, cfg, "test-city")
	if len(errs) != 0 {
		t.Fatalf("defaultNamedSessionDemand errs = %v", errs)
	}
	if len(demand) != 0 {
		t.Fatalf("defaultNamedSessionDemand = %v, want no named demand from gc.run_target metadata", demand)
	}
}

func TestDefaultScaleCheckCountsReportsMissingRigStore(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name: "repo",
			Path: filepath.Join(cityPath, "repos", "repo"),
		}},
	}
	agent := &config.Agent{Name: "worker", Dir: filepath.Join("repos", "repo")}
	cityStore := beads.NewMemStore()
	if _, err := cityStore.Create(beads.Bead{
		Title:  "wrong-store routed work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "repos/repo/worker",
		},
	}); err != nil {
		t.Fatalf("create city routed bead: %v", err)
	}
	target := defaultScaleCheckTargetForAgent(cityPath, cfg, agent, cityStore, nil)

	counts, partialTemplates, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{target})
	if got := counts["repos/repo/worker"]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts = %d, want 0", got)
	}
	if len(errs) != 1 {
		t.Fatalf("defaultScaleCheckCounts errs = %v, want one missing rig-store diagnostic", errs)
	}
	if !strings.Contains(errs[0].Error(), `rig store "repo" unavailable`) {
		t.Fatalf("defaultScaleCheckCounts err = %v, want missing rig-store diagnostic", errs[0])
	}
	if !partialTemplates["repos/repo/worker"] {
		t.Fatalf("partialTemplates = %v, want missing rig-store template marked partial", partialTemplates)
	}
}

func TestBuildDesiredStateDefaultScaleCheckMissingRigStoreReportsZeroDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "rig-owned routed work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "repos/repo/worker",
		},
	}); err != nil {
		t.Fatalf("create city routed bead: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name: "repo",
			Path: filepath.Join(cityPath, "repos", "repo"),
		}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               filepath.Join("repos", "repo"),
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}

	var stderr strings.Builder
	got := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	if demand := got.ScaleCheckCounts["repos/repo/worker"]; demand != 0 {
		t.Fatalf("ScaleCheckCounts[repos/repo/worker] = %d, want 0 without rig store", demand)
	}
	if got.StoreQueryPartial {
		t.Fatalf("StoreQueryPartial = true, want false for scoped default scale_check failure")
	}
	if !got.ScaleCheckPartialTemplates["repos/repo/worker"] {
		t.Fatalf("ScaleCheckPartialTemplates = %v, want missing rig-store template marked partial", got.ScaleCheckPartialTemplates)
	}
	if len(got.State) != 0 {
		t.Fatalf("desired sessions = %d, want none without rig store demand", len(got.State))
	}
	if !strings.Contains(stderr.String(), `rig store "repo" unavailable`) {
		t.Fatalf("stderr = %q, want missing rig-store diagnostic", stderr.String())
	}
}

func TestCollectAssignedWorkBeads_ExcludesRoutedToMetadataWithoutAssignee(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "check alpha",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "seth"},
	}); err != nil {
		t.Fatalf("create routed bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "unrouted work",
		Type:   "task",
		Status: "open",
	}); err != nil {
		t.Fatalf("create unrouted bead: %v", err)
	}
	got, _ := collectAssignedWorkBeads(&config.City{}, store)
	if len(got) != 0 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 0", len(got))
	}
}

func TestCollectAssignedWorkBeads_ExcludesUnassignedInProgressWorkflowRoot(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title:  "workflow root",
		Type:   "molecule",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":      "workflow",
			"gc.routed_to": "worker",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	if err := store.Update(root.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("mark workflow root in_progress: %v", err)
	}
	cfg := &config.City{Agents: []config.Agent{{
		Name:              "worker",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(2),
	}}}

	got, _ := collectAssignedWorkBeads(cfg, store)
	if len(got) != 0 {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want unassigned workflow root ignored", got)
	}
}

// TestCollectAssignedWorkBeads_IncludesOpenPoolRoutedAssignedWork pins the
// fix for issue #2793: an open step bead carrying both gc.routed_to and a
// stale (dead-session) assignee must enter assignedWorkBeads so the
// orphan-release sweep can iterate it. The status=in_progress pass misses
// it (wrong status) and the Ready(Assignee=...) pass misses it (the
// assignee identity belongs to a session that no longer exists). Without
// this pass, the step bead stays assigned forever, pool demand stays 0,
// and graph.v2 wisps stall after an orphan drain.
func TestCollectAssignedWorkBeads_IncludesOpenPoolRoutedAssignedWork(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{Title: "workflow finalize", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create blocker bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "orphaned step bead",
		Type:     "task",
		Status:   "open",
		Assignee: "rig--pool__coder-dead-session",
		Metadata: map[string]string{"gc.routed_to": "rig/pool.coder"},
	})
	if err != nil {
		t.Fatalf("create orphaned step bead: %v", err)
	}
	if err := store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block orphaned step bead: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, store)
	if len(got) != 1 || got[0].ID != work.ID {
		t.Fatalf("collectAssignedWorkBeads = %#v, want [%s]", got, work.ID)
	}
}

// TestCollectAssignedWorkBeads_IncludesOpenRunTargetAssignedWork covers the
// legacy workflow root shape: workflow beads stamp gc.run_target (not
// gc.routed_to). The third pass accepts that marker only for the same
// workflow-kind beads that releaseOrphanedPoolAssignments can release.
func TestCollectAssignedWorkBeads_IncludesOpenRunTargetAssignedWork(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{Title: "workflow finalize", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create blocker bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "orphaned root step",
		Type:     "task",
		Status:   "open",
		Assignee: "rig--pool__coder-dead-session",
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.run_target": "rig/pool.coder",
		},
	})
	if err != nil {
		t.Fatalf("create orphaned root step: %v", err)
	}
	if err := store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block orphaned root step: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, store)
	if len(got) != 1 || got[0].ID != work.ID {
		t.Fatalf("collectAssignedWorkBeads = %#v, want [%s]", got, work.ID)
	}
}

func TestCollectAssignedWorkBeads_ExcludesOpenNonWorkflowRunTargetAssignedWork(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{Title: "workflow finalize", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create blocker bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "control retry bead",
		Type:     "task",
		Status:   "open",
		Assignee: "gascity--control-dispatcher",
		Metadata: map[string]string{
			"gc.kind":       "retry",
			"gc.run_target": "gascity/gc.implementation-worker",
		},
	})
	if err != nil {
		t.Fatalf("create control retry bead: %v", err)
	}
	if err := store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block control retry bead: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, store)
	if len(got) != 0 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 0 for non-workflow gc.run_target", len(got))
	}
}

func TestCollectAssignedWorkBeads_ExcludesSessionBeads(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	// Session bead with assignee — should be excluded.
	if _, err := store.Create(beads.Bead{
		Title:    "worker session",
		Type:     sessionBeadType,
		Status:   "open",
		Assignee: "worker-1",
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	// Message bead with assignee — excluded from Ready() (messages are
	// delivered via nudge, not the ready/dispatch loop).
	if _, err := store.Create(beads.Bead{
		Title:    "you have mail",
		Type:     "message",
		Status:   "open",
		Assignee: "worker-1",
	}); err != nil {
		t.Fatalf("create message bead: %v", err)
	}
	// Real task bead with assignee — should be included (in_progress path).
	task, err := store.Create(beads.Bead{
		Title:    "do the thing",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "worker-1",
	})
	if err != nil {
		t.Fatalf("create task bead: %v", err)
	}
	got, _ := collectAssignedWorkBeads(&config.City{}, store)
	if len(got) != 1 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 1 (task only): %#v", len(got), got)
	}
	if got[0].ID != task.ID {
		t.Fatalf("expected task %q, got %q", task.ID, got[0].ID)
	}
}

func TestCollectAssignedWorkBeads_PreservesPartialInProgressSurvivors(t *testing.T) {
	t.Parallel()

	store := &partialAssignedWorkStore{
		MemStore:          beads.NewMemStore(),
		partialInProgress: true,
	}
	work, err := store.Create(beads.Bead{
		Title:    "assigned active work",
		Type:     "task",
		Assignee: "worker-1",
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set work in_progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work bead: %v", err)
	}

	got, stores, storeRefs, _, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, nil)
	if !partial {
		t.Fatal("partial = false, want true")
	}
	if len(got) != 1 || got[0].ID != work.ID {
		t.Fatalf("collectAssignedWorkBeadsWithStores returned %#v, want partial survivor %s", got, work.ID)
	}
	if len(stores) != 1 || stores[0] != store {
		t.Fatalf("stores = %#v, want source store for partial survivor", stores)
	}
	if len(storeRefs) != 1 || storeRefs[0] != "" {
		t.Fatalf("storeRefs = %#v, want city store ref for partial survivor", storeRefs)
	}
}

func TestCollectAssignedWorkBeads_PreservesPartialReadySurvivors(t *testing.T) {
	t.Parallel()

	store := &partialAssignedWorkStore{
		MemStore:     beads.NewMemStore(),
		partialReady: true,
	}
	work, err := store.Create(beads.Bead{
		Title:    "assigned ready work",
		Type:     "task",
		Assignee: "worker-1",
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	got, stores, storeRefs, _, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, nil)
	if !partial {
		t.Fatal("partial = false, want true")
	}
	if len(got) != 1 || got[0].ID != work.ID {
		t.Fatalf("collectAssignedWorkBeadsWithStores returned %#v, want partial ready survivor %s", got, work.ID)
	}
	if len(stores) != 1 || stores[0] != store {
		t.Fatalf("stores = %#v, want source store for partial survivor", stores)
	}
	if len(storeRefs) != 1 || storeRefs[0] != "" {
		t.Fatalf("storeRefs = %#v, want city store ref for partial survivor", storeRefs)
	}
}

func TestCollectAssignedWorkBeads_SkipsReadyProbeForInProgressAssignee(t *testing.T) {
	store := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	session, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-session",
			"template":     "worker",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "active work",
		Type:     "task",
		Assignee: "worker-session",
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{session})

	got, _, _, _, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, snapshot)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	if len(got) != 1 || got[0].ID != work.ID {
		t.Fatalf("got = %#v, want in-progress work %s", got, work.ID)
	}
	if len(store.readyQueries) != 0 {
		t.Fatalf("Ready queried while in-progress work was already known: %#v", store.readyQueries)
	}
}

func TestCollectAssignedWorkBeads_SkipsCityReadyProbeForRigInProgressAssignee(t *testing.T) {
	cityStore := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	rigStore := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	session, err := cityStore.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-session",
			"template":     "worker",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	work, err := rigStore.Create(beads.Bead{
		Title:    "active rig work",
		Type:     "task",
		Assignee: "worker-session",
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	work, err = rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{session})

	got, _, _, _, partial := collectAssignedWorkBeadsWithStores(
		&config.City{Rigs: []config.Rig{{Name: "repo", Path: "repo"}}},
		cityStore,
		map[string]beads.Store{"repo": rigStore},
		nil,
		snapshot,
	)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	if len(got) != 1 || got[0].ID != work.ID {
		t.Fatalf("got = %#v, want rig in-progress work %s", got, work.ID)
	}
	if len(cityStore.readyQueries) != 0 || len(rigStore.readyQueries) != 0 {
		t.Fatalf("Ready queried while cross-store in-progress work was already known: city=%#v rig=%#v", cityStore.readyQueries, rigStore.readyQueries)
	}
}

func TestCollectAssignedWorkBeads_ReadyProbeStillRunsForOtherAssignees(t *testing.T) {
	store := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	activeSession, err := store.Create(beads.Bead{
		Title:  "active worker session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-active",
			"template":     "worker",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create active session bead: %v", err)
	}
	readySession, err := store.Create(beads.Bead{
		Title:  "ready worker session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-ready",
			"template":     "worker",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create ready session bead: %v", err)
	}
	activeWork, err := store.Create(beads.Bead{
		Title:    "active work",
		Type:     "task",
		Assignee: "worker-active",
	})
	if err != nil {
		t.Fatalf("create active work bead: %v", err)
	}
	if err := store.Update(activeWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("mark active work in_progress: %v", err)
	}
	activeWork, err = store.Get(activeWork.ID)
	if err != nil {
		t.Fatalf("reload active work: %v", err)
	}
	readyWork, err := store.Create(beads.Bead{
		Title:    "ready work",
		Type:     "task",
		Status:   "open",
		Assignee: "worker-ready",
	})
	if err != nil {
		t.Fatalf("create ready work bead: %v", err)
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{activeSession, readySession})

	got, _, _, _, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, snapshot)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	gotIDs := make(map[string]bool)
	for _, bead := range got {
		gotIDs[bead.ID] = true
	}
	for _, want := range []string{activeWork.ID, readyWork.ID} {
		if !gotIDs[want] {
			t.Fatalf("collected work IDs = %#v, want %s", gotIDs, want)
		}
	}
	queried := make(map[string]bool)
	for _, query := range store.readyQueries {
		queried[query.Assignee] = true
	}
	if queried["worker-active"] || queried[activeSession.ID] {
		t.Fatalf("Ready queries = %#v, want no probe for active assignee", store.readyQueries)
	}
	if !queried["worker-ready"] {
		t.Fatalf("Ready queries = %#v, want probe for worker-ready", store.readyQueries)
	}
}

func TestCollectAssignedWorkBeads_ReadyProbeIncludesActiveSessionAssignees(t *testing.T) {
	store := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	activeSession, err := store.Create(beads.Bead{
		Title:  "active worker session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-active",
			"template":     "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create active session bead: %v", err)
	}
	sleepySession, err := store.Create(beads.Bead{
		Title:  "sleepy worker session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-sleepy",
			"template":     "worker",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create sleepy session bead: %v", err)
	}
	readyWork, err := store.Create(beads.Bead{
		Title:    "ready active work",
		Type:     "task",
		Status:   "open",
		Assignee: "worker-active",
	})
	if err != nil {
		t.Fatalf("create ready work bead: %v", err)
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{activeSession, sleepySession})

	got, _, _, _, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, snapshot)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	if len(got) != 1 || got[0].ID != readyWork.ID {
		t.Fatalf("got = %#v, want ready active-session work %s", got, readyWork.ID)
	}
	queried := make(map[string]bool)
	for _, query := range store.readyQueries {
		queried[query.Assignee] = true
	}
	if !queried["worker-active"] {
		t.Fatalf("Ready queries = %#v, want probe for active session assignee", store.readyQueries)
	}
}

func TestReadyAssignedWorkAssigneesExcludeBroadIdentities(t *testing.T) {
	got := readyAssignedWorkAssignees(&config.City{
		Agents: []config.Agent{{
			Dir:  "repo",
			Name: "worker",
		}},
		NamedSessions: []config.NamedSession{
			{Template: "mayor", Mode: "always"},
			{Dir: "repo", Template: "named-worker", Mode: "on_demand"},
		},
	}, nil, nil)

	for _, disallowed := range []string{"repo/worker", "mayor"} {
		for _, value := range got {
			if value == disallowed {
				t.Fatalf("ready assignees = %#v, want no broad identity %q", got, disallowed)
			}
		}
	}
	foundNamed := false
	for _, value := range got {
		if value == "repo/named-worker" {
			foundNamed = true
		}
	}
	if !foundNamed {
		t.Fatalf("ready assignees = %#v, want on-demand named-session identity", got)
	}
}

func TestCollectAssignedWorkBeads_ReadyProbeExcludesFutureNamedSessionRuntimeAssignee(t *testing.T) {
	cityStore := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	rigStore := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "repo", Path: "/repo"}},
		NamedSessions: []config.NamedSession{{
			Dir:      "repo",
			Name:     "control-dispatcher",
			Template: "control-dispatcher",
			Mode:     "on_demand",
		}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	runtimeName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
	if _, err := rigStore.Create(beads.Bead{
		Title:    "ready control work",
		Type:     "task",
		Status:   "open",
		Assignee: runtimeName,
	}); err != nil {
		t.Fatalf("create ready work bead: %v", err)
	}

	got, stores, storeRefs, _, partial := collectAssignedWorkBeadsWithStores(
		cfg,
		cityStore,
		map[string]beads.Store{"repo": rigStore},
		nil,
		nil,
	)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	if len(got) != 0 {
		t.Fatalf("got = %#v, want no future-runtime-assigned named-session work", got)
	}
	if len(stores) != 0 {
		t.Fatalf("stores = %#v, want none", stores)
	}
	if len(storeRefs) != 0 {
		t.Fatalf("storeRefs = %#v, want none", storeRefs)
	}
	queried := make(map[string]bool)
	for _, query := range rigStore.readyQueries {
		queried[query.Assignee] = true
	}
	if queried[runtimeName] {
		t.Fatalf("rig Ready queries = %#v, must not include future runtime assignee %q", rigStore.readyQueries, runtimeName)
	}
	if !queried[identity] {
		t.Fatalf("rig Ready queries = %#v, want canonical named-session assignee %q", rigStore.readyQueries, identity)
	}
}

func TestCollectAssignedWorkBeadsWithStores_TracksRigStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work, err := rigStore.Create(beads.Bead{
		Title:    "assigned rig work",
		Type:     "task",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create rig work bead: %v", err)
	}
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set rig work in_progress: %v", err)
	}
	work, err = rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("reload rig work bead: %v", err)
	}

	got, stores, storeRefs, _, partial := collectAssignedWorkBeadsWithStores(
		&config.City{Rigs: []config.Rig{{Name: "repo", Path: "/repo"}}},
		cityStore,
		map[string]beads.Store{"repo": rigStore},
		nil,
		nil,
	)
	if partial {
		t.Fatal("partial = true, want false")
	}
	if len(got) != 1 || got[0].ID != work.ID {
		t.Fatalf("collectAssignedWorkBeadsWithStores returned %#v, want [%s]", got, work.ID)
	}
	if len(stores) != 1 || stores[0] != rigStore {
		t.Fatalf("stores = %#v, want [rig store]", stores)
	}
	if len(storeRefs) != 1 || storeRefs[0] != "repo" {
		t.Fatalf("storeRefs = %#v, want [repo]", storeRefs)
	}
}

func TestCollectAssignedWorkBeadsWithStores_PreservesCrossStoreIDCollisions(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cityWork, err := cityStore.Create(beads.Bead{
		Title:    "assigned city work",
		Type:     "task",
		Assignee: "worker-city",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create city work bead: %v", err)
	}
	if err := cityStore.Update(cityWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set city work in_progress: %v", err)
	}
	cityWork, err = cityStore.Get(cityWork.ID)
	if err != nil {
		t.Fatalf("reload city work bead: %v", err)
	}
	rigWork, err := rigStore.Create(beads.Bead{
		Title:    "assigned rig work",
		Type:     "task",
		Assignee: "worker-rig",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create rig work bead: %v", err)
	}
	if err := rigStore.Update(rigWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set rig work in_progress: %v", err)
	}
	rigWork, err = rigStore.Get(rigWork.ID)
	if err != nil {
		t.Fatalf("reload rig work bead: %v", err)
	}
	if cityWork.ID != rigWork.ID {
		t.Fatalf("test setup expected overlapping city/rig IDs, got city %q rig %q", cityWork.ID, rigWork.ID)
	}

	got, stores, storeRefs, _, partial := collectAssignedWorkBeadsWithStores(
		&config.City{Rigs: []config.Rig{{Name: "repo", Path: "/repo"}}},
		cityStore,
		map[string]beads.Store{"repo": rigStore},
		nil,
		nil,
	)
	if partial {
		t.Fatal("partial = true, want false")
	}
	if len(got) != 2 {
		t.Fatalf("collectAssignedWorkBeadsWithStores returned %d beads, want 2: %#v", len(got), got)
	}
	if len(stores) != len(got) {
		t.Fatalf("stores length = %d, want %d", len(stores), len(got))
	}
	if len(storeRefs) != len(got) {
		t.Fatalf("storeRefs length = %d, want %d", len(storeRefs), len(got))
	}
	if got[0].ID != cityWork.ID || stores[0] != cityStore {
		t.Fatalf("first collected work = (%s, %#v), want city work/store", got[0].ID, stores[0])
	}
	if storeRefs[0] != "" {
		t.Fatalf("first store ref = %q, want city ref", storeRefs[0])
	}
	if got[1].ID != rigWork.ID || stores[1] != rigStore {
		t.Fatalf("second collected work = (%s, %#v), want rig work/store", got[1].ID, stores[1])
	}
	if storeRefs[1] != "repo" {
		t.Fatalf("second store ref = %q, want repo", storeRefs[1])
	}
}

// TestCollectAssignedWorkBeadsWithStores_ReadinessIsStoreScoped pins the
// producer side of the cross-store readiness fix: a ready city bead and a
// blocked open rig bead share the same ID, and ReadyAssigned must mark only the
// city copy ready. Keying readiness by bead ID alone would leak the city copy's
// readiness onto the blocked rig copy and reintroduce the awake-demand hang.
func TestCollectAssignedWorkBeadsWithStores_ReadinessIsStoreScoped(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	// Decoy bumps the city sequence so cityWork shares rigWork's generated ID;
	// closed, it is never collected.
	decoy, err := cityStore.Create(beads.Bead{Title: "decoy", Type: "task"})
	if err != nil {
		t.Fatalf("create city decoy: %v", err)
	}
	closed := "closed"
	if err := cityStore.Update(decoy.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close city decoy: %v", err)
	}
	cityWork, err := cityStore.Create(beads.Bead{
		Title:    "ready city work",
		Type:     "task",
		Assignee: "worker-city",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create city work: %v", err)
	}
	if err := cityStore.Update(cityWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set city work in_progress: %v", err)
	}

	// Blocker keeps rigWork un-ready; rigWork shares cityWork's generated ID and
	// is admitted only by the open-routed orphan-release pass (no readiness gate).
	blocker, err := rigStore.Create(beads.Bead{Title: "blocker", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create rig blocker: %v", err)
	}
	rigWork, err := rigStore.Create(beads.Bead{
		Title:    "blocked rig work",
		Type:     "task",
		Assignee: "worker-rig",
		Needs:    []string{blocker.ID},
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create rig work: %v", err)
	}
	if cityWork.ID != rigWork.ID {
		t.Fatalf("test setup expected overlapping city/rig IDs, got city %q rig %q", cityWork.ID, rigWork.ID)
	}

	got, _, _, readyAssigned, partial := collectAssignedWorkBeadsWithStores(
		&config.City{Rigs: []config.Rig{{Name: "repo", Path: "/repo"}}},
		cityStore,
		map[string]beads.Store{"repo": rigStore},
		nil,
		nil,
	)
	if partial {
		t.Fatal("partial = true, want false")
	}
	if len(got) != 2 {
		t.Fatalf("collected %d beads, want 2 (city ready + rig blocked): %#v", len(got), got)
	}
	if !readyAssigned[storeScopedBeadKey{StoreRef: "", ID: cityWork.ID}] {
		t.Fatalf("city copy %q must be store-scoped ready; ReadyAssigned=%#v", cityWork.ID, readyAssigned)
	}
	if readyAssigned[storeScopedBeadKey{StoreRef: "repo", ID: rigWork.ID}] {
		t.Fatalf("blocked rig copy %q must NOT be ready despite the same-ID city bead; ReadyAssigned=%#v", rigWork.ID, readyAssigned)
	}
}

// TestCollectAssignedWorkBeadsWithStores_SkipSetIsStoreScopedAcrossSameID pins
// the assignee Ready-probe skip set to store scope. A ready city bead and a
// blocked rig bead can share a generated ID; the rig assignee also owns a
// separate, genuinely-ready bead reachable only via the Ready probe. The
// same-ID city bead must not collapse the skip set by bare ID and suppress the
// rig assignee's probe, or that separate ready work is silently dropped and the
// rig session stays asleep.
func TestCollectAssignedWorkBeadsWithStores_SkipSetIsStoreScopedAcrossSameID(t *testing.T) {
	cityStore := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	rigStore := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}

	// Decoy bumps the city sequence so cityWork shares rigWork's generated ID;
	// closed, it is never collected.
	decoy, err := cityStore.Create(beads.Bead{Title: "decoy", Type: "task"})
	if err != nil {
		t.Fatalf("create city decoy: %v", err)
	}
	if err := cityStore.Update(decoy.ID, beads.UpdateOpts{Status: stringPtr("closed")}); err != nil {
		t.Fatalf("close city decoy: %v", err)
	}
	// Ready city work for worker-city, captured by the in-progress pass.
	cityWork, err := cityStore.Create(beads.Bead{
		Title:    "ready city work",
		Type:     "task",
		Assignee: "worker-city",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create city work: %v", err)
	}
	if err := cityStore.Update(cityWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set city work in_progress: %v", err)
	}

	// Blocker keeps rigWork un-ready; rigWork shares cityWork's generated ID and
	// is admitted only by the open-routed orphan-release pass (no readiness gate).
	blocker, err := rigStore.Create(beads.Bead{Title: "blocker", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create rig blocker: %v", err)
	}
	rigWork, err := rigStore.Create(beads.Bead{
		Title:    "blocked rig work",
		Type:     "task",
		Assignee: "worker-rig",
		Needs:    []string{blocker.ID},
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create rig work: %v", err)
	}
	if cityWork.ID != rigWork.ID {
		t.Fatalf("test setup expected overlapping city/rig IDs, got city %q rig %q", cityWork.ID, rigWork.ID)
	}
	// worker-rig's genuinely-ready bead, discoverable ONLY by the assignee Ready
	// probe: not in_progress, not a molecule root, not pool-routed, no blocker.
	rigReadyWork, err := rigStore.Create(beads.Bead{
		Title:    "separate ready rig work",
		Type:     "task",
		Status:   "open",
		Assignee: "worker-rig",
	})
	if err != nil {
		t.Fatalf("create rig ready work: %v", err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{
		{ID: "sess-city", Type: sessionBeadType, Status: "open", Metadata: map[string]string{"session_name": "worker-city", "template": "worker", "state": "asleep"}},
		{ID: "sess-rig", Type: sessionBeadType, Status: "open", Metadata: map[string]string{"session_name": "worker-rig", "template": "worker", "state": "asleep"}},
	})

	got, _, _, _, partial := collectAssignedWorkBeadsWithStores(
		&config.City{Rigs: []config.Rig{{Name: "repo", Path: "/repo"}}},
		cityStore,
		map[string]beads.Store{"repo": rigStore},
		nil,
		snapshot,
	)
	if partial {
		t.Fatal("partial = true, want false")
	}

	gotIDs := make(map[string]bool)
	for _, bead := range got {
		gotIDs[bead.ID] = true
	}
	if !gotIDs[rigReadyWork.ID] {
		t.Fatalf("worker-rig ready bead %q was dropped: the same-ID city ready bead collapsed the skip set and suppressed worker-rig's store-scoped Ready probe; collected=%#v", rigReadyWork.ID, gotIDs)
	}

	rigProbedForRigAssignee := false
	for _, query := range rigStore.readyQueries {
		if query.Assignee == "worker-rig" {
			rigProbedForRigAssignee = true
		}
	}
	if !rigProbedForRigAssignee {
		t.Fatalf("rig Ready queries = %#v, want a probe for worker-rig despite the same-ID city ready bead", rigStore.readyQueries)
	}
}

func TestBuildDesiredState_UsesAgentHookOverride(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{
			Name:              "test-city",
			InstallAgentHooks: []string{"gemini"},
		},
		Agents: []config.Agent{{
			Name:              "hookoverride",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
			InstallAgentHooks: []string{"claude"},
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	if len(dsResult.State) != 1 {
		t.Fatalf("desired state size = %d, want 1", len(dsResult.State))
	}

	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "settings.json")); err != nil {
		t.Fatalf("agent claude hook not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gemini", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("workspace gemini hook should not be installed for agent override: %v", err)
	}
}

func TestBuildDesiredStateRejectsExplicitTmuxAgentWhenSessionProviderCannotRouteTmux(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Provider: "opencode"},
		Session:   config.SessionConfig{Provider: config.SessionTransportACP},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				Command:     "echo",
				Args:        []string{"provider"},
				ACPCommand:  "echo",
				ACPArgs:     []string{"acp"},
				PromptMode:  "none",
				SupportsACP: boolPtr(true),
			},
		},
		Agents: []config.Agent{{
			Name:              "worker",
			Provider:          "opencode",
			Session:           config.SessionTransportTmux,
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
		}},
	}
	store := beads.NewMemStore()
	sp := &acpOnlyDesiredStateProvider{Fake: runtime.NewFake()}
	var stderr strings.Builder

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, sp, store, &stderr)
	if len(dsResult.State) != 0 {
		t.Fatalf("desired state size = %d, want 0: %#v", len(dsResult.State), dsResult.State)
	}
	beads, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, err)
	}
	if len(beads) != 0 {
		t.Fatalf("session bead count = %d, want 0: %#v", len(beads), beads)
	}
	if got := stderr.String(); !strings.Contains(got, "cannot route tmux sessions") {
		t.Fatalf("stderr = %q, want tmux routing rejection", got)
	}
}

func TestBuildDesiredState_InstallsGeminiHooksBeforeFingerprinting(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Provider: "test"},
		Providers: map[string]config.ProviderSpec{
			"test": {Command: "echo", PromptMode: "none"},
		},
		Agents: []config.Agent{{
			Name:              "probe",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "echo 1",
			WorkDir:           "worker",
			InstallAgentHooks: []string{"gemini"},
		}},
	}

	first := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	if len(first.State) != 1 {
		t.Fatalf("first desired state size = %d, want 1", len(first.State))
	}
	var firstTP TemplateParams
	for _, tp := range first.State {
		firstTP = tp
	}

	hookPath := filepath.Join(cityPath, "worker", ".gemini", "settings.json")
	if _, err := os.Stat(hookPath); err != nil {
		t.Fatalf("stat gemini hook %q: %v", hookPath, err)
	}

	firstCfg := templateParamsToConfig(firstTP)
	wantRelDst := path.Join("worker", ".gemini", "settings.json")
	foundHook := false
	for _, entry := range firstCfg.CopyFiles {
		if entry.RelDst != wantRelDst {
			continue
		}
		foundHook = true
		if entry.Src != hookPath {
			t.Fatalf("CopyFiles hook src = %q, want %q", entry.Src, hookPath)
		}
	}
	if !foundHook {
		t.Fatalf("first fingerprint missing gemini hook copy file %q: %#v", wantRelDst, firstCfg.CopyFiles)
	}

	second := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	if len(second.State) != 1 {
		t.Fatalf("second desired state size = %d, want 1", len(second.State))
	}
	var secondTP TemplateParams
	for _, tp := range second.State {
		secondTP = tp
	}
	secondCfg := templateParamsToConfig(secondTP)

	if got, want := runtime.CoreFingerprint(secondCfg), runtime.CoreFingerprint(firstCfg); got != want {
		t.Fatalf("core fingerprint changed after hook install: got %q want %q", got, want)
	}
}

func TestBuildDesiredState_MaterializesHookOverlaysBeforeFingerprinting(t *testing.T) {
	cityPath := t.TempDir()
	packOverlay := filepath.Join(cityPath, "packs", "core", "overlay")
	overlayHook := filepath.Join(packOverlay, "per-provider", "gemini", ".gemini", "settings.json")
	workHook := filepath.Join(cityPath, "worker", ".gemini", "settings.json")
	for _, dir := range []string{filepath.Dir(overlayHook), filepath.Dir(workHook)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	// Same semantic hook document as overlayHook, but intentionally not in the
	// canonical JSON shape that runtime overlay staging writes.
	nonCanonicalHook := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"gc prime --hook --hook-format gemini"}],"matcher":""}]},"tools":{"shell":{"enableInteractiveShell":false}}}` + "\n")
	canonicalOverlayHook := []byte(`{
  "tools": {
    "shell": {
      "enableInteractiveShell": false
    }
  },
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "gc prime --hook --hook-format gemini"
          }
        ]
      }
    ]
  }
}
`)
	if err := os.WriteFile(workHook, nonCanonicalHook, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", workHook, err)
	}
	if err := os.WriteFile(overlayHook, canonicalOverlayHook, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", overlayHook, err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Provider: "test"},
		Providers: map[string]config.ProviderSpec{
			"test": {Command: "echo", PromptMode: "none"},
		},
		PackOverlayDirs: []string{packOverlay},
		Agents: []config.Agent{{
			Name:              "probe",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "echo 1",
			WorkDir:           "worker",
			InstallAgentHooks: []string{"gemini"},
		}},
	}

	first := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	var firstTP TemplateParams
	for _, tp := range first.State {
		firstTP = tp
	}
	firstCfg := templateParamsToConfig(firstTP)
	if firstCfg.WorkDir == "" {
		t.Fatalf("first desired state missing runtime config: %#v", first.State)
	}
	firstHash := runtime.CoreFingerprint(firstCfg)

	if err := runtime.StageSessionWorkDir(firstCfg); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}

	second := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	var secondTP TemplateParams
	for _, tp := range second.State {
		secondTP = tp
	}
	secondCfg := templateParamsToConfig(secondTP)
	if got := runtime.CoreFingerprint(secondCfg); got != firstHash {
		t.Fatalf("core fingerprint changed after runtime overlay materialization: first=%s second=%s firstCopyFiles=%#v secondCopyFiles=%#v",
			firstHash, got, firstCfg.CopyFiles, secondCfg.CopyFiles)
	}
}

// TestPrepareTemplateResolution_MaterializesFamilyOverlayForCustomProvider
// guards gc-6bw8o on the pre-fingerprint materialization path. A rig-scoped
// agent runs a custom provider "pi-vllm" (base="builtin:pi") whose rig overlay
// ships only per-provider/pi/.pi/extensions/gc-hooks.js — there is no
// per-provider/pi-vllm/ slot and the agent does NOT declare
// install_agent_hooks=["pi"]. The concrete overlay name therefore has no
// directory, so materializeProviderOverlaysBeforeFingerprint (reached via
// prepareTemplateResolution) must fall back to the launch family (pi) via
// EffectiveOverlayProviderNames and stage the family hook into the workdir
// before resolveTemplate fingerprints CopyFiles. Before the fix this caller
// used the concrete-only OverlayProviderNamesFromParts and never staged the
// hook on this path.
func TestPrepareTemplateResolution_MaterializesFamilyOverlayForCustomProvider(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	overlayDir := filepath.Join(cityDir, "packs", "myrig", "overlay")
	hook := filepath.Join(overlayDir, "per-provider", "pi", ".pi", "extensions", "gc-hooks.js")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatalf("MkdirAll(overlay): %v", err)
	}
	if err := os.WriteFile(hook, []byte("// gc hook"), 0o644); err != nil {
		t.Fatalf("WriteFile(hook): %v", err)
	}

	base := "builtin:pi"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "polecat",
			Provider: "pi-vllm",
			Scope:    "rig",
			Dir:      "myrig",
			// Intentionally no InstallAgentHooks: the family fallback must
			// stage per-provider/pi/ purely from the concrete-absent
			// resolution. Intentionally no StartCommand: it is an escape hatch
			// that bypasses provider resolution (ResolveProvider step 1),
			// which would defeat the base="builtin:pi" family fallback.
		}},
		Providers: map[string]config.ProviderSpec{
			// Explicit command so resolution does not depend on a real `pi`
			// binary on PATH; base still yields BuiltinAncestor=pi.
			"pi-vllm": {Base: &base, Command: "/bin/echo"},
		},
		Rigs:           []config.Rig{{Name: "myrig", Path: rigDir}},
		RigOverlayDirs: map[string][]string{"myrig": {overlayDir}},
	}

	bp := newAgentBuildParams("test-city", cityDir, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	prepareTemplateResolution(bp, &cfg.Agents[0], "myrig/polecat", io.Discard)

	staged := filepath.Join(rigDir, ".pi", "extensions", "gc-hooks.js")
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("family pi overlay not materialized before fingerprint for custom pi-vllm provider (gc-6bw8o): %v", err)
	}
}

func TestBuildDesiredState_IncludesImportedAlwaysNamedSessions(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repo")
	for path, contents := range map[string]string{
		filepath.Join(cityPath, "pack.toml"): `
[pack]
name = "import-regression"
schema = 2

[imports.gs]
source = "./assets/sidecar"
`,
		filepath.Join(cityPath, "city.toml"): `
[workspace]
provider = "claude"

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = "repo"

[rigs.imports.gs]
source = "./assets/sidecar"
`,
		filepath.Join(cityPath, ".gc", "site.toml"): `
workspace_name = "import-regression"

[[rig]]
name = "repo"
path = "./repo"
`,
		filepath.Join(cityPath, "assets", "sidecar", "pack.toml"): `
[pack]
name = "sidecar"
schema = 2

[[named_session]]
template = "captain"
scope = "city"
mode = "always"

[[named_session]]
template = "watcher"
scope = "rig"
mode = "always"
`,
		filepath.Join(cityPath, "assets", "sidecar", "agents", "captain", "agent.toml"): "scope = \"city\"\n",
		filepath.Join(cityPath, "assets", "sidecar", "agents", "captain", "prompt.md"):  "You are the imported captain.\n",
		filepath.Join(cityPath, "assets", "sidecar", "agents", "watcher", "agent.toml"): "scope = \"rig\"\n",
		filepath.Join(cityPath, "assets", "sidecar", "agents", "watcher", "prompt.md"):  "You are the imported watcher.\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", rigPath, err)
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, time.Now().UTC(), cfg, runtime.NewFake(), beads.NewMemStore(), io.Discard)

	captain, ok := dsResult.State["gs__captain"]
	if !ok {
		t.Fatalf("desired state missing gs__captain; keys=%v", mapKeys(dsResult.State))
	}
	if captain.TemplateName != "gs.captain" {
		t.Fatalf("gs__captain TemplateName = %q, want %q", captain.TemplateName, "gs.captain")
	}
	if captain.ConfiguredNamedIdentity != "gs.captain" {
		t.Fatalf("gs__captain ConfiguredNamedIdentity = %q, want %q", captain.ConfiguredNamedIdentity, "gs.captain")
	}

	watcher, ok := dsResult.State["repo--gs__watcher"]
	if !ok {
		t.Fatalf("desired state missing repo--gs__watcher; keys=%v", mapKeys(dsResult.State))
	}
	if watcher.TemplateName != "repo/gs.watcher" {
		t.Fatalf("repo--gs__watcher TemplateName = %q, want %q", watcher.TemplateName, "repo/gs.watcher")
	}
	if watcher.ConfiguredNamedIdentity != "repo/gs.watcher" {
		t.Fatalf("repo--gs__watcher ConfiguredNamedIdentity = %q, want %q", watcher.ConfiguredNamedIdentity, "repo/gs.watcher")
	}
}

func TestBuildDesiredState_TransitiveFalseSkipsNestedImportedNamedSessions(t *testing.T) {
	cityPath := t.TempDir()
	for path, contents := range map[string]string{
		filepath.Join(cityPath, "city.toml"): `
[workspace]
name = "import-regression"
provider = "claude"

[providers.claude]
base = "builtin:claude"

[imports.outer]
source = "./assets/outer"
transitive = false
`,
		filepath.Join(cityPath, "assets", "outer", "pack.toml"): `
[pack]
name = "outer"
schema = 2

[imports.inner]
source = "../inner"

[[named_session]]
template = "captain"
scope = "city"
mode = "always"
`,
		filepath.Join(cityPath, "assets", "outer", "agents", "captain", "agent.toml"): "scope = \"city\"\n",
		filepath.Join(cityPath, "assets", "outer", "agents", "captain", "prompt.md"):  "You are the outer captain.\n",
		filepath.Join(cityPath, "assets", "inner", "pack.toml"): `
[pack]
name = "inner"
schema = 2

[[named_session]]
template = "watcher"
scope = "city"
mode = "always"
`,
		filepath.Join(cityPath, "assets", "inner", "agents", "watcher", "agent.toml"): "scope = \"city\"\n",
		filepath.Join(cityPath, "assets", "inner", "agents", "watcher", "prompt.md"):  "You are the inner watcher.\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, time.Now().UTC(), cfg, runtime.NewFake(), beads.NewMemStore(), io.Discard)
	if _, ok := dsResult.State["outer__captain"]; !ok {
		t.Fatalf("desired state missing outer__captain; keys=%v", mapKeys(dsResult.State))
	}
	if _, ok := dsResult.State["outer__watcher"]; ok {
		t.Fatalf("desired state should not include nested named session when transitive=false; keys=%v", mapKeys(dsResult.State))
	}
}

func TestBuildDesiredState_RoutedQueueDoesNotCreateOneSessionPerBead(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	for i := 0; i < 12; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  "queued claude work",
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": "claude",
			},
		}); err != nil {
			t.Fatalf("create queued bead %d: %v", i, err)
		}
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "claude",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(20),
			ScaleCheck:        "printf 1",
		}},
	}

	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	if len(dsResult.AssignedWorkBeads) != 0 {
		t.Fatalf("AssignedWorkBeads = %d, want 0 for routed-only queue", len(dsResult.AssignedWorkBeads))
	}

	claudeSessions := 0
	for _, tp := range dsResult.State {
		if tp.TemplateName == "claude" {
			claudeSessions++
		}
	}
	if claudeSessions != 1 {
		t.Fatalf("claude desired sessions = %d, want 1 (scale_check only)", claudeSessions)
	}
}

func TestBuildDesiredState_NewPoolSessionBeadCreatedWithConcreteIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "claude",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "printf 1",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if len(dsResult.State) != 1 {
		t.Fatalf("desired sessions = %d, want 1", len(dsResult.State))
	}

	sessionBeads, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session beads = %d, want 1", len(sessionBeads))
	}
	got := sessionBeads[0]
	if got.Metadata["agent_name"] != "rig/claude-1" {
		t.Fatalf("agent_name = %q, want concrete slot identity", got.Metadata["agent_name"])
	}
	if got.Metadata["alias"] != "rig/claude-1" {
		t.Fatalf("alias = %q, want concrete slot identity", got.Metadata["alias"])
	}
	if got.Metadata["pool_slot"] != "1" {
		t.Fatalf("pool_slot = %q, want 1", got.Metadata["pool_slot"])
	}
	if got.Title != "rig/claude-1" {
		t.Fatalf("title = %q, want concrete slot identity", got.Title)
	}
	if !containsString(got.Labels, "agent:rig/claude-1") {
		t.Fatalf("labels = %#v, want concrete slot agent label", got.Labels)
	}
	if !beadOwnsPoolSessionName(got) {
		t.Fatalf("session_name = %q should be the bead-owned pool runtime name", got.Metadata["session_name"])
	}
}

func TestBuildDesiredState_MaxOneAgentDemandUsesCanonicalIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if len(dsResult.State) != 1 {
		t.Fatalf("desired sessions = %d, want 1", len(dsResult.State))
	}
	var tp TemplateParams
	for _, candidate := range dsResult.State {
		tp = candidate
	}
	if tp.InstanceName != "cashmaster/refinery" {
		t.Fatalf("InstanceName = %q, want canonical non-pool identity", tp.InstanceName)
	}
	if tp.Alias != "cashmaster/refinery" {
		t.Fatalf("Alias = %q, want canonical non-pool identity", tp.Alias)
	}
	if tp.PoolSlot != 0 {
		t.Fatalf("PoolSlot = %d, want 0 for max_active_sessions=1", tp.PoolSlot)
	}

	sessionBeads, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session beads = %d, want 1", len(sessionBeads))
	}
	got := sessionBeads[0]
	if got.Metadata["agent_name"] != "cashmaster/refinery" {
		t.Fatalf("agent_name = %q, want canonical non-pool identity", got.Metadata["agent_name"])
	}
	if got.Metadata["alias"] != "cashmaster/refinery" {
		t.Fatalf("alias = %q, want canonical non-pool identity", got.Metadata["alias"])
	}
	if got.Metadata["pool_slot"] != "" {
		t.Fatalf("pool_slot = %q, want empty for max_active_sessions=1", got.Metadata["pool_slot"])
	}
	if got.Title != "cashmaster/refinery" {
		t.Fatalf("title = %q, want canonical non-pool identity", got.Title)
	}
	if containsString(got.Labels, "agent:cashmaster/refinery-1") {
		t.Fatalf("labels = %#v, must not include phantom pool identity", got.Labels)
	}
	if !containsString(got.Labels, "agent:cashmaster/refinery") {
		t.Fatalf("labels = %#v, want canonical agent label", got.Labels)
	}
}

func TestBuildDesiredState_NoStoreMaxOneAgentDemandUsesCanonicalSlotZero(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	if len(dsResult.State) != 1 {
		t.Fatalf("desired sessions = %d, want 1", len(dsResult.State))
	}
	var tp TemplateParams
	for _, candidate := range dsResult.State {
		tp = candidate
	}
	if tp.InstanceName != "cashmaster/refinery" {
		t.Fatalf("InstanceName = %q, want canonical non-pool identity", tp.InstanceName)
	}
	if tp.Alias != "cashmaster/refinery" {
		t.Fatalf("Alias = %q, want canonical non-pool identity", tp.Alias)
	}
	if tp.PoolSlot != 0 {
		t.Fatalf("PoolSlot = %d, want 0 for no-store max_active_sessions=1", tp.PoolSlot)
	}
}

func TestSyncSessionBeads_DoesNotBackfillPoolSlotForCanonicalMaxOneDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	var stderr bytes.Buffer
	syncSessionBeads(
		cityPath,
		store,
		dsResult.State,
		runtime.NewFake(),
		allConfiguredDS(dsResult.State),
		cfg,
		&clock.Fake{Time: time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)},
		&stderr,
		false,
	)

	sessionBeads, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session beads = %d, want 1", len(sessionBeads))
	}
	got := sessionBeads[0]
	if got.Metadata["agent_name"] != "cashmaster/refinery" {
		t.Fatalf("agent_name = %q, want canonical singleton identity; sync stderr=%q", got.Metadata["agent_name"], stderr.String())
	}
	if got.Metadata["pool_slot"] != "" {
		t.Fatalf("pool_slot = %q, want empty after build plus sync for canonical singleton", got.Metadata["pool_slot"])
	}
	if got.Metadata["alias"] != "cashmaster/refinery" {
		t.Fatalf("alias = %q, want canonical singleton identity", got.Metadata["alias"])
	}
}

func TestBuildDesiredState_MaxOneAgentNormalizesStalePoolIdentityBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery-1",
			"session_name":         "s-refinery-stale",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
		}},
	}

	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	if len(dsResult.State) != 1 {
		t.Fatalf("desired sessions = %d, want 1", len(dsResult.State))
	}
	var tp TemplateParams
	for _, candidate := range dsResult.State {
		tp = candidate
	}
	if tp.InstanceName != "cashmaster/refinery" {
		t.Fatalf("InstanceName = %q, want canonical non-pool identity", tp.InstanceName)
	}
	if tp.Alias != "cashmaster/refinery" {
		t.Fatalf("Alias = %q, want canonical non-pool identity", tp.Alias)
	}
	if tp.PoolSlot != 0 {
		t.Fatalf("PoolSlot = %d, want 0 for normalized max_active_sessions=1 bead", tp.PoolSlot)
	}

	got, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", stale.ID, err)
	}
	if got.Metadata["agent_name"] != "cashmaster/refinery" {
		t.Fatalf("agent_name = %q, want canonical non-pool identity", got.Metadata["agent_name"])
	}
	if got.Metadata["alias"] != "cashmaster/refinery" {
		t.Fatalf("alias = %q, want canonical non-pool identity", got.Metadata["alias"])
	}
	if got.Metadata["pool_slot"] != "" {
		t.Fatalf("pool_slot = %q, want empty after singleton normalization", got.Metadata["pool_slot"])
	}
	if got.Title != "cashmaster/refinery" {
		t.Fatalf("title = %q, want canonical non-pool identity", got.Title)
	}
	if containsString(got.Labels, "agent:cashmaster/refinery-1") {
		t.Fatalf("labels = %#v, must not include phantom pool identity", got.Labels)
	}
	if !containsString(got.Labels, "agent:cashmaster/refinery") {
		t.Fatalf("labels = %#v, want canonical agent label", got.Labels)
	}
	if !strings.Contains(stderr.String(), "collapsing phantom pool identity") {
		t.Fatalf("stderr = %q, want scoped phantom identity diagnostic", stderr.String())
	}
}

func TestBuildDesiredState_MaxOneAgentPrefersCanonicalWhenStaleDuplicateExists(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery-1",
			"session_name":         "s-refinery-stale",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-canonical",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
		}},
	}

	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	if strings.Contains(stderr.String(), "(skipping)") {
		t.Fatalf("stderr = %q, want stale duplicate to remain recoverable", stderr.String())
	}
	if _, ok := dsResult.State[stale.Metadata["session_name"]]; ok {
		t.Fatalf("desired state includes stale duplicate %q; keys=%v", stale.Metadata["session_name"], mapKeys(dsResult.State))
	}
	tp, ok := dsResult.State[canonical.Metadata["session_name"]]
	if !ok {
		t.Fatalf("desired state missing canonical singleton %q; keys=%v", canonical.Metadata["session_name"], mapKeys(dsResult.State))
	}
	if tp.InstanceName != "cashmaster/refinery" {
		t.Fatalf("InstanceName = %q, want canonical singleton identity", tp.InstanceName)
	}
	if tp.PoolSlot != 0 {
		t.Fatalf("PoolSlot = %d, want 0 for max_active_sessions=1", tp.PoolSlot)
	}
}

func TestBuildDesiredState_MaxOneAgentPreservesManualStaleIdentityBesideCanonicalDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	manual, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery-1",
			"session_name":         "s-refinery-manual",
			"state":                "awake",
			"session_origin":       "manual",
			"manual_session":       "true",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	manualTP, ok := dsResult.State[manual.Metadata["session_name"]]
	if !ok {
		t.Fatalf("desired state missing manual stale singleton %q; keys=%v", manual.Metadata["session_name"], mapKeys(dsResult.State))
	}
	if !manualTP.ManualSession {
		t.Fatalf("manual stale singleton ManualSession = false, want true")
	}
	if manualTP.InstanceName != "cashmaster/refinery-1" {
		t.Fatalf("manual stale singleton InstanceName = %q, want preserved identity", manualTP.InstanceName)
	}
	if manualTP.Alias != "cashmaster/refinery-1" {
		t.Fatalf("manual stale singleton Alias = %q, want preserved identity", manualTP.Alias)
	}
	if len(dsResult.State) != 2 {
		t.Fatalf("desired sessions = %d, want manual stale session beside canonical demand; keys=%v", len(dsResult.State), mapKeys(dsResult.State))
	}
	canonicalFound := false
	for sessionName, tp := range dsResult.State {
		if sessionName == manual.Metadata["session_name"] {
			continue
		}
		if tp.InstanceName == "cashmaster/refinery" && tp.Alias == "cashmaster/refinery" {
			canonicalFound = true
		}
	}
	if !canonicalFound {
		t.Fatalf("desired state missing canonical singleton demand beside manual stale session; state=%#v", dsResult.State)
	}
}

func TestBuildDesiredState_MaxOneManualAssignedWorkPreservesManualIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	manual, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery-1",
			"session_name":         "s-refinery-manual",
			"state":                "awake",
			"session_origin":       "manual",
			"manual_session":       "true",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	priority := 10
	if _, err := store.Create(beads.Bead{
		Title:    "manual assigned work",
		Type:     "task",
		Status:   "in_progress",
		Priority: &priority,
		Assignee: "cashmaster/refinery-1",
		Metadata: map[string]string{
			"gc.routed_to": "cashmaster/refinery",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 0",
		}},
	}

	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	manualTP, ok := dsResult.State[manual.Metadata["session_name"]]
	if !ok {
		t.Fatalf("desired state missing manual resume session %q; keys=%v stderr=%q", manual.Metadata["session_name"], mapKeys(dsResult.State), stderr.String())
	}
	if len(dsResult.State) != 1 {
		t.Fatalf("desired sessions = %d, want only manual assigned-work session; keys=%v", len(dsResult.State), mapKeys(dsResult.State))
	}
	if !manualTP.ManualSession {
		t.Fatal("manual assigned-work singleton ManualSession = false, want true")
	}
	if manualTP.InstanceName != "cashmaster/refinery-1" {
		t.Fatalf("manual assigned-work singleton InstanceName = %q, want preserved identity", manualTP.InstanceName)
	}
	if manualTP.Alias != "cashmaster/refinery-1" {
		t.Fatalf("manual assigned-work singleton Alias = %q, want preserved identity", manualTP.Alias)
	}
	stored, err := store.Get(manual.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", manual.ID, err)
	}
	if got := stored.Metadata["agent_name"]; got != "cashmaster/refinery-1" {
		t.Fatalf("stored agent_name = %q, want preserved manual identity", got)
	}
	if got := stored.Metadata["alias"]; got != "cashmaster/refinery-1" {
		t.Fatalf("stored alias = %q, want preserved manual identity", got)
	}
	if got := stored.Metadata["pool_slot"]; got != "1" {
		t.Fatalf("stored pool_slot = %q, want preserved manual identity", got)
	}
}

func TestBuildDesiredState_MaxOneAgentSkipsCanonicalDuplicateWhenStaleAssignedWorkWinsCap(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery-1",
			"session_name":         "s-refinery-stale",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-canonical",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stalePriority := 10
	if _, err := store.Create(beads.Bead{
		Title:    "stale assigned work",
		Type:     "task",
		Status:   "in_progress",
		Priority: &stalePriority,
		Assignee: "cashmaster/refinery-1",
		Metadata: map[string]string{
			"gc.routed_to": "cashmaster/refinery",
		},
	}); err != nil {
		t.Fatal(err)
	}
	canonicalPriority := 1
	if _, err := store.Create(beads.Bead{
		Title:    "canonical assigned work",
		Type:     "task",
		Status:   "in_progress",
		Priority: &canonicalPriority,
		Assignee: "cashmaster/refinery",
		Metadata: map[string]string{
			"gc.routed_to": "cashmaster/refinery",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 0",
		}},
	}

	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	if _, ok := dsResult.State[canonical.Metadata["session_name"]]; ok {
		t.Fatalf("desired state includes unselected canonical duplicate %q; keys=%v", canonical.Metadata["session_name"], mapKeys(dsResult.State))
	}
	tp, ok := dsResult.State[stale.Metadata["session_name"]]
	if !ok {
		t.Fatalf("desired state missing stale resume session %q; keys=%v stderr=%q", stale.Metadata["session_name"], mapKeys(dsResult.State), stderr.String())
	}
	if len(dsResult.State) != 1 {
		t.Fatalf("desired state has %d sessions, want singleton cap enforced; keys=%v", len(dsResult.State), mapKeys(dsResult.State))
	}
	if tp.InstanceName != "cashmaster/refinery" {
		t.Fatalf("InstanceName = %q, want canonical singleton identity", tp.InstanceName)
	}
	if tp.Alias != "" {
		t.Fatalf("Alias = %q, want deferred alias while canonical duplicate owns it", tp.Alias)
	}

	storedStale, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", stale.ID, err)
	}
	if !containsString(sessionpkg.AliasHistory(storedStale.Metadata), "cashmaster/refinery-1") {
		t.Fatalf("alias_history = %#v, want stale singleton alias preserved for next tick", sessionpkg.AliasHistory(storedStale.Metadata))
	}

	var secondStderr bytes.Buffer
	secondResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &secondStderr)
	if _, ok := secondResult.State[canonical.Metadata["session_name"]]; ok {
		t.Fatalf("second desired state includes unselected canonical duplicate %q; keys=%v", canonical.Metadata["session_name"], mapKeys(secondResult.State))
	}
	secondTP, ok := secondResult.State[stale.Metadata["session_name"]]
	if !ok {
		t.Fatalf("second desired state missing stale resume session %q; keys=%v stderr=%q", stale.Metadata["session_name"], mapKeys(secondResult.State), secondStderr.String())
	}
	if len(secondResult.State) != 1 {
		t.Fatalf("second desired state has %d sessions, want singleton cap enforced; keys=%v", len(secondResult.State), mapKeys(secondResult.State))
	}
	if secondTP.InstanceName != "cashmaster/refinery" {
		t.Fatalf("second InstanceName = %q, want canonical singleton identity", secondTP.InstanceName)
	}
	if secondTP.Alias != "" {
		t.Fatalf("second Alias = %q, want deferred alias while canonical duplicate owns it", secondTP.Alias)
	}
}

func TestRealizePoolDesiredSessionsDefersAliasWhenNormalizationCollides(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":                        "cashmaster/refinery",
			"agent_name":                      "cashmaster/refinery-1",
			"alias":                           "cashmaster/refinery-1",
			"session_name":                    "s-refinery-stale",
			"state":                           "awake",
			poolManagedMetadataKey:            boolMetadata(true),
			"pool_slot":                       "1",
			poolAliasConflictCountMetadataKey: "2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-canonical",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, stale))
	snapshot.addInfo(sessiontest.SeedBead(t, canonical))
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = snapshot
	desired := map[string]TemplateParams{}

	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "cashmaster/refinery",
		Requests: []SessionRequest{{
			Template:      "cashmaster/refinery",
			Tier:          "resume",
			SessionBeadID: stale.ID,
		}},
	}, desired, &stderr)

	tp, ok := desired[stale.Metadata["session_name"]]
	if !ok {
		t.Fatalf("desired state missing stale resume session; keys=%v stderr=%q", mapKeys(desired), stderr.String())
	}
	if got := tp.Alias; got != "" {
		t.Fatalf("deferred singleton TemplateParams.Alias = %q, want empty while canonical alias is unavailable", got)
	}
	if got := tp.Env["GC_ALIAS"]; got != "" {
		t.Fatalf("deferred singleton GC_ALIAS = %q, want empty while canonical alias is unavailable", got)
	}
	if got := tp.Env["GC_AGENT"]; got != tp.SessionName {
		t.Fatalf("deferred singleton GC_AGENT = %q, want bead session name %q", got, tp.SessionName)
	}
	if tp.EnvIdentityStamped {
		t.Fatal("deferred singleton EnvIdentityStamped = true, want false until alias is available")
	}
	stored, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", stale.ID, err)
	}
	if got := stored.Metadata["alias"]; got != "" {
		t.Fatalf("stored deferred singleton alias = %q, want empty while canonical alias is unavailable", got)
	}
	if got := stored.Metadata[poolAliasConflictMetadataKey]; got != "cashmaster/refinery" {
		t.Fatalf("stored pool_alias_conflict = %q, want canonical alias", got)
	}
	if got := stored.Metadata[poolAliasConflictCountMetadataKey]; got != "3" {
		t.Fatalf("stored pool_alias_conflict_count = %q, want incremented retry count", got)
	}
	if _, err := time.Parse(time.RFC3339, stored.Metadata[poolAliasConflictAtMetadataKey]); err != nil {
		t.Fatalf("stored pool_alias_conflict_at = %q, want RFC3339 timestamp: %v", stored.Metadata[poolAliasConflictAtMetadataKey], err)
	}
	if !strings.Contains(stderr.String(), "deferring singleton pool identity normalization") {
		t.Fatalf("stderr = %q, want normalization deferral diagnostic", stderr.String())
	}
}

// TestRealizePoolDesiredSessionsResumePreservesLegacyBoundSessionName traces
// the bound→unbound adoption through realize: a resume request for the
// current canonical template that carries a session bead persisted under the
// removed binding must keep that bead's persisted session_name, so the live
// runtime session is reused instead of being renamed or recreated (which
// would orphan the running agent holding in-progress work).
func TestRealizePoolDesiredSessionsResumePreservesLegacyBoundSessionName(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	adopted, err := store.Create(beads.Bead{
		Title:  "gascity-packs/gc.implementation-worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:gascity-packs/gc.implementation-worker"},
		Metadata: map[string]string{
			"template":             "gascity-packs/gc.implementation-worker",
			"agent_name":           "gascity-packs/gc.implementation-worker",
			"session_name":         "gc__implementation-worker-mc-1",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "implementation-worker",
			Dir:               "gascity-packs",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(8),
		}},
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, adopted))
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = snapshot
	desired := map[string]TemplateParams{}

	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "gascity-packs/implementation-worker",
		Requests: []SessionRequest{{
			Template:      "gascity-packs/implementation-worker",
			Tier:          "resume",
			SessionBeadID: adopted.ID,
		}},
	}, desired, &stderr)

	tp, ok := desired["gc__implementation-worker-mc-1"]
	if !ok {
		t.Fatalf("desired state missing adopted resume session under persisted session_name; keys=%v stderr=%q",
			mapKeys(desired), stderr.String())
	}
	if tp.SessionName != "gc__implementation-worker-mc-1" {
		t.Fatalf("TemplateParams.SessionName = %q, want persisted session_name preserved", tp.SessionName)
	}
	if got := len(desired); got != 1 {
		t.Fatalf("desired sessions = %d, want 1 (no duplicate spawn for adopted bead)", got)
	}
}

func TestRealizePoolDesiredSessionsLimitsFreshCreatesToWakeBudget(t *testing.T) {
	maxWakes := 2
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(10),
		}},
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = &sessionBeadSnapshot{}
	requests := make([]SessionRequest, 5)
	for i := range requests {
		requests[i] = SessionRequest{Template: "worker", Tier: "new"}
	}
	desired := map[string]TemplateParams{}

	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "worker",
		Requests: requests,
	}, desired, &stderr)

	if got := len(bp.sessionBeads.OpenInfos()); got != maxWakes {
		t.Fatalf("created session beads = %d, want wake budget %d; stderr=%q", got, maxWakes, stderr.String())
	}
	if got := len(desired); got != maxWakes {
		t.Fatalf("desired sessions = %d, want wake budget %d; stderr=%q", got, maxWakes, stderr.String())
	}
	if !strings.Contains(stderr.String(), "pool session create budget exhausted") {
		t.Fatalf("stderr = %q, want budget exhaustion diagnostic", stderr.String())
	}
}

func TestRealizePoolDesiredSessionsBindsTriggerBeadToFreshSession(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			WorkDir:           ".gc/workspaces/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = &sessionBeadSnapshot{}
	desired := map[string]TemplateParams{}

	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "worker",
		Requests: []SessionRequest{{
			Template:      "worker",
			Tier:          "new",
			WorkBeadID:    "gp-59q",
			WorkBeadTitle: "Fix pack route templates!",
			WorkPack:      "packer",
			WorkStoreRef:  "rig:gascity-packs",
		}},
	}, desired, &stderr)

	sessions := bp.sessionBeads.OpenInfos()
	if len(sessions) != 1 {
		t.Fatalf("created session beads = %d, want 1; stderr=%q", len(sessions), stderr.String())
	}
	stored, err := store.Get(sessions[0].ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := stored.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "gp-59q" {
		t.Fatalf("trigger bead metadata = %q, want gp-59q", got)
	}
	if got := stored.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]; got != "rig:gascity-packs" {
		t.Fatalf("trigger store metadata = %q, want rig:gascity-packs", got)
	}
	if got := stored.Metadata[beadmeta.PackWorkspaceMetadataKey]; got != "" {
		t.Fatalf("pack workspace metadata = %q, want empty default pack workspace", got)
	}
	wantWorkDir := filepath.Join(bp.cityPath, ".gc", "workspaces", "packer")
	if got := stored.Metadata["work_dir"]; got != wantWorkDir {
		t.Fatalf("work_dir = %q, want %q", got, wantWorkDir)
	}
	if len(desired) != 1 {
		t.Fatalf("desired sessions = %d, want 1", len(desired))
	}
	for _, tp := range desired {
		if got := tp.Env["GC_TRIGGER_BEAD_ID"]; got != "gp-59q" {
			t.Fatalf("GC_TRIGGER_BEAD_ID = %q, want gp-59q", got)
		}
		if got := tp.Env["GC_TRIGGER_WORK_BEAD_ID"]; got != "gp-59q" {
			t.Fatalf("GC_TRIGGER_WORK_BEAD_ID = %q, want gp-59q", got)
		}
		if got := tp.Env["GC_TRIGGER_BEAD_STORE_REF"]; got != "rig:gascity-packs" {
			t.Fatalf("GC_TRIGGER_BEAD_STORE_REF = %q, want rig:gascity-packs", got)
		}
		if got := tp.Env["GC_TRIGGER_WORK_STORE_REF"]; got != "rig:gascity-packs" {
			t.Fatalf("GC_TRIGGER_WORK_STORE_REF = %q, want rig:gascity-packs", got)
		}
		if got := tp.Env["GC_PACKER_PACK"]; got != "packer" {
			t.Fatalf("GC_PACKER_PACK = %q, want packer", got)
		}
	}
}

func TestRealizePoolDesiredSessionsHonorsExplicitPackWorkspace(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			WorkDir:           ".gc/workspaces/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = &sessionBeadSnapshot{}
	workspace := "gp-59q-fix-pack-route-templates"

	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "worker",
		Requests: []SessionRequest{{
			Template:      "worker",
			Tier:          "new",
			WorkBeadID:    "gp-59q",
			WorkBeadTitle: "Fix pack route templates!",
			WorkPack:      "packer",
			WorkWorkspace: workspace,
			WorkStoreRef:  "rig:gascity-packs",
		}},
	}, map[string]TemplateParams{}, &stderr)

	sessions := bp.sessionBeads.OpenInfos()
	if len(sessions) != 1 {
		t.Fatalf("created session beads = %d, want 1; stderr=%q", len(sessions), stderr.String())
	}
	stored, err := store.Get(sessions[0].ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := stored.Metadata[beadmeta.PackWorkspaceMetadataKey]; got != workspace {
		t.Fatalf("pack workspace metadata = %q, want %q", got, workspace)
	}
	wantWorkDir := filepath.Join(bp.cityPath, ".gc", "workspaces", "packer", workspace)
	if got := stored.Metadata["work_dir"]; got != wantWorkDir {
		t.Fatalf("work_dir = %q, want %q", got, wantWorkDir)
	}
}

func TestRealizePoolDesiredSessionsRebindUpdatesPackWorkspaceMetadata(t *testing.T) {
	store := beads.NewMemStore()
	oldWorkDir := filepath.Join(t.TempDir(), "old")
	reusable, err := store.Create(beads.Bead{
		Title:  "worker reusable",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                              "worker",
			"agent_name":                            "worker-7",
			"alias":                                 "worker-7",
			"session_name":                          "worker-reusable",
			"state":                                 "awake",
			"pool_slot":                             "7",
			poolManagedMetadataKey:                  boolMetadata(true),
			beadmeta.TriggerBeadIDMetadataKey:       "gp-old",
			beadmeta.TriggerBeadStoreRefMetadataKey: "rig:gascity-packs",
			beadmeta.PackMetadataKey:                "packer",
			beadmeta.PackWorkspaceMetadataKey:       "old-workspace",
			beadmeta.WorkDirMetadataKey:             oldWorkDir,
			beadmeta.LegacyWorkDirMetadataKey:       oldWorkDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			WorkDir:           ".gc/workspaces/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, reusable))
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = snapshot
	workspace := "route-smoke"

	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "worker",
		Requests: []SessionRequest{{
			Template:      "worker",
			Tier:          "resume",
			SessionBeadID: reusable.ID,
			WorkBeadID:    "gp-new",
			WorkBeadTitle: "Route smoke",
			WorkPack:      "packer",
			WorkWorkspace: workspace,
			WorkStoreRef:  "rig:gascity-packs",
		}},
	}, map[string]TemplateParams{}, &stderr)

	stored, err := store.Get(reusable.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	wantWorkDir := filepath.Join(bp.cityPath, ".gc", "workspaces", "packer", workspace)
	if got := stored.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "gp-new" {
		t.Fatalf("trigger bead metadata = %q, want gp-new", got)
	}
	if got := stored.Metadata[beadmeta.PackWorkspaceMetadataKey]; got != workspace {
		t.Fatalf("pack workspace metadata = %q, want %q", got, workspace)
	}
	if got := stored.Metadata[beadmeta.WorkDirMetadataKey]; got != wantWorkDir {
		t.Fatalf("gc.work_dir = %q, want %q", got, wantWorkDir)
	}
	if got := stored.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != wantWorkDir {
		t.Fatalf("work_dir = %q, want %q", got, wantWorkDir)
	}
}

func TestRealizePoolDesiredSessionsLiveRetryPreservesLauncherWorkDir(t *testing.T) {
	tests := []struct {
		name          string
		currentBeadID string
	}{
		{name: "worker marker still names prior attempt", currentBeadID: "fi-old"},
		{name: "worker marker already names retry attempt", currentBeadID: "fi-new"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			launcherWorkDir := filepath.Join(t.TempDir(), "fi-old-write-review-report")
			reusable, err := store.Create(beads.Bead{
				Title:  "worker live retry",
				Type:   sessionBeadType,
				Status: "open",
				Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"template":                              "worker",
					"agent_name":                            "worker-1",
					"alias":                                 "worker-1",
					"session_name":                          "worker-live-retry",
					"state":                                 string(sessionpkg.StateAwake),
					"pool_slot":                             "1",
					poolManagedMetadataKey:                  boolMetadata(true),
					sessionpkg.CurrentBeadIDKey:             tt.currentBeadID,
					beadmeta.TriggerBeadIDMetadataKey:       "fi-old",
					beadmeta.WorkDirMetadataKey:             launcherWorkDir,
					beadmeta.LegacyWorkDirMetadataKey:       launcherWorkDir,
					beadmeta.TriggerBeadStoreRefMetadataKey: "rig:fixture",
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents: []config.Agent{{
					Name:              "worker",
					StartCommand:      "true",
					WorkDir:           ".gc/workspaces/{{.AgentBase}}",
					MinActiveSessions: intPtr(0),
					MaxActiveSessions: intPtr(1),
				}},
			}
			snapshot := &sessionBeadSnapshot{}
			snapshot.addInfo(sessiontest.SeedBead(t, reusable))
			var stderr bytes.Buffer
			bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
			bp.sessionBeads = snapshot

			retry := workBead("fi-new", "worker", reusable.ID, "in_progress", 1)
			states := ComputePoolDesiredStates(cfg, []beads.Bead{retry}, snapshot.OpenInfos(), nil)
			if len(states) != 1 || len(states[0].Requests) != 1 {
				t.Fatalf("retry desired state = %#v, want one request", states)
			}
			request := states[0].Requests[0]
			if request.Tier != "resume" || request.SessionBeadID != reusable.ID || request.WorkBeadID != retry.ID {
				t.Fatalf("retry request = %#v, want concrete resume of %s for %s", request, reusable.ID, retry.ID)
			}

			realizePoolDesiredSessions(bp, &cfg.Agents[0], states[0], map[string]TemplateParams{}, &stderr)

			stored, err := store.Get(reusable.ID)
			if err != nil {
				t.Fatalf("Get(session): %v", err)
			}
			if got := stored.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != retry.ID {
				t.Fatalf("trigger bead metadata = %q, want %q", got, retry.ID)
			}
			if got := stored.Metadata[beadmeta.WorkDirMetadataKey]; got != launcherWorkDir {
				t.Fatalf("gc.work_dir = %q, want live launcher path %q", got, launcherWorkDir)
			}
			if got := stored.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != launcherWorkDir {
				t.Fatalf("work_dir = %q, want live launcher path %q", got, launcherWorkDir)
			}
		})
	}
}

func TestRealizePoolDesiredSessionsBudgetExhaustionStillAllowsLaterReuse(t *testing.T) {
	maxWakes := 1
	store := beads.NewMemStore()
	reusable, err := store.Create(beads.Bead{
		Title:  "worker reusable",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":             "worker",
			"agent_name":           "worker-7",
			"alias":                "worker-7",
			"session_name":         "worker-reusable",
			"state":                "awake",
			"pool_slot":            "7",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(10),
		}},
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, reusable))
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = snapshot
	desired := map[string]TemplateParams{}

	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "worker",
		Requests: []SessionRequest{
			{Template: "worker", Tier: "new"},
			{Template: "worker", Tier: "new"},
			{Template: "worker", Tier: "resume", SessionBeadID: reusable.ID},
		},
	}, desired, &stderr)

	if got := len(bp.sessionBeads.OpenInfos()); got != 2 {
		t.Fatalf("open session beads = %d, want one fresh plus one reused; stderr=%q", got, stderr.String())
	}
	if _, ok := desired["worker-reusable"]; !ok {
		t.Fatalf("desired missing reusable session after budget exhaustion; keys=%v stderr=%q", mapKeys(desired), stderr.String())
	}
}

func TestRealizePoolDesiredSessionsRefundsFreshCreateBudgetAfterFailure(t *testing.T) {
	maxWakes := 1
	store := &failingPoolCreateStore{MemStore: beads.NewMemStore(), failuresRemaining: 1}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{
			{
				Name:              "alpha",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(10),
			},
			{
				Name:              "zulu",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(10),
			},
		},
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = &sessionBeadSnapshot{}
	desired := map[string]TemplateParams{}

	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "alpha",
		Requests: []SessionRequest{{
			Template: "alpha",
			Tier:     "new",
		}},
	}, desired, &stderr)
	realizePoolDesiredSessions(bp, &cfg.Agents[1], PoolDesiredState{
		Template: "zulu",
		Requests: []SessionRequest{{
			Template: "zulu",
			Tier:     "new",
		}},
	}, desired, &stderr)

	if got := len(desired); got != 1 {
		t.Fatalf("desired sessions = %d, want later pool create after refund; desired=%v stderr=%q", got, desired, stderr.String())
	}
	for _, tp := range desired {
		if tp.TemplateName != "zulu" {
			t.Fatalf("created template = %q, want zulu after alpha create failure; stderr=%q", tp.TemplateName, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "injected pool create failure") {
		t.Fatalf("stderr = %q, want injected create failure diagnostic", stderr.String())
	}
}

func TestBuildDesiredStateTranslatesFloorDemandIntoCreateBudget(t *testing.T) {
	maxWakes := 1
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{
			{
				Name:              "alpha",
				StartCommand:      "true",
				ScaleCheck:        "printf 5",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(5),
			},
			{
				Name:              "zulu",
				StartCommand:      "true",
				ScaleCheck:        "printf 0",
				MinActiveSessions: intPtr(1),
				MaxActiveSessions: intPtr(5),
			},
		},
	}
	var stderr bytes.Buffer

	result := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	counts := map[string]int{}
	for _, tp := range result.State {
		counts[tp.TemplateName]++
	}
	if got := counts["alpha"]; got != 0 {
		t.Fatalf("alpha fresh creates = %d, want 0 while floor demand consumes the only token; counts=%v stderr=%q", got, counts, stderr.String())
	}
	if got := counts["zulu"]; got != 1 {
		t.Fatalf("zulu fresh creates = %d, want 1 from translated floor demand; counts=%v stderr=%q", got, counts, stderr.String())
	}
}

func TestBuildDesiredStateFairShareIgnoresInFlightPoolCreates(t *testing.T) {
	maxWakes := 2
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "alpha-1",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:alpha-1", "template:alpha"},
		Metadata: map[string]string{
			"template":             "alpha",
			"agent_name":           "alpha-1",
			"alias":                "alpha-1",
			"session_name":         "s-alpha-creating",
			"state":                "creating",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{
			{
				Name:              "alpha",
				StartCommand:      "true",
				ScaleCheck:        "printf 1",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(5),
			},
			{
				Name:              "zulu",
				StartCommand:      "true",
				ScaleCheck:        "printf 2",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(5),
			},
		},
	}
	var stderr bytes.Buffer

	result := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	counts := map[string]int{}
	for _, tp := range result.State {
		counts[tp.TemplateName]++
	}
	if got := counts["alpha"]; got != 1 {
		t.Fatalf("alpha desired sessions = %d, want reused in-flight create; counts=%v stderr=%q", got, counts, stderr.String())
	}
	if got := counts["zulu"]; got != 2 {
		t.Fatalf("zulu fresh creates = %d, want full budget after alpha in-flight reuse; counts=%v stderr=%q", got, counts, stderr.String())
	}
}

func TestBuildDesiredStateDependencyFloorExemptFromFreshCreateBudget(t *testing.T) {
	maxWakes := 1
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{
			{
				Name:              "db",
				Dir:               "svc",
				StartCommand:      "true",
				ScaleCheck:        "printf 0",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
			},
			{
				Name:              "api",
				Dir:               "svc",
				StartCommand:      "true",
				ScaleCheck:        "printf 2",
				DependsOn:         []string{"svc/db"},
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
			},
		},
	}
	var stderr bytes.Buffer

	result := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	var apiCreates, dbFloors int
	for _, tp := range result.State {
		switch {
		case tp.TemplateName == "svc/api" && !tp.DependencyOnly:
			apiCreates++
		case tp.TemplateName == "svc/db" && tp.DependencyOnly:
			dbFloors++
		}
	}
	if apiCreates != 1 {
		t.Fatalf("api fresh creates = %d, want one budgeted root create; stderr=%q", apiCreates, stderr.String())
	}
	if dbFloors != 1 {
		t.Fatalf("db dependency floors = %d, want one exempt dependency floor; stderr=%q", dbFloors, stderr.String())
	}
	if got := len(result.State); got != 2 {
		t.Fatalf("desired entries = %d, want root plus dependency floor; state=%v stderr=%q", got, result.State, stderr.String())
	}
}

func TestSyncSessionBeads_ReclaimsDeferredSingletonAliasAfterConflictClears(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery-1",
			"session_name":         "s-refinery-stale",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-canonical",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, stale))
	snapshot.addInfo(sessiontest.SeedBead(t, canonical))
	var buildStderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &buildStderr)
	bp.sessionBeads = snapshot
	desired := map[string]TemplateParams{}

	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "cashmaster/refinery",
		Requests: []SessionRequest{{
			Template:      "cashmaster/refinery",
			Tier:          "resume",
			SessionBeadID: stale.ID,
		}},
	}, desired, &buildStderr)

	var persistentStderr bytes.Buffer
	persistentClk := &clock.Fake{Time: time.Date(2026, 5, 6, 2, 30, 0, 0, time.UTC)}
	syncSessionBeads(cityPath, store, desired, runtime.NewFake(), allConfiguredDS(desired), cfg, persistentClk, &persistentStderr, false)

	stillConflicted, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", stale.ID, err)
	}
	if got := stillConflicted.Metadata["alias"]; got != "" {
		t.Fatalf("persistent-conflict alias = %q, want still deferred while canonical owner exists", got)
	}
	if got := stillConflicted.Metadata[poolAliasConflictMetadataKey]; got != "cashmaster/refinery" {
		t.Fatalf("persistent-conflict pool_alias_conflict = %q, want canonical alias", got)
	}
	if got := stillConflicted.Metadata[poolAliasConflictCountMetadataKey]; got != "2" {
		t.Fatalf("persistent-conflict pool_alias_conflict_count = %q, want sync retry increment", got)
	}

	if err := store.Close(canonical.ID); err != nil {
		t.Fatalf("Close(%s): %v", canonical.ID, err)
	}
	var syncStderr bytes.Buffer
	clk := &clock.Fake{Time: time.Date(2026, 5, 6, 3, 0, 0, 0, time.UTC)}
	syncSessionBeads(cityPath, store, desired, runtime.NewFake(), allConfiguredDS(desired), cfg, clk, &syncStderr, false)

	got, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", stale.ID, err)
	}
	if got.Metadata["alias"] != "cashmaster/refinery" {
		t.Fatalf("alias = %q, want canonical alias after conflict clears; sync stderr=%q", got.Metadata["alias"], syncStderr.String())
	}
	if got.Metadata[poolAliasConflictMetadataKey] != "" {
		t.Fatalf("pool_alias_conflict = %q, want cleared", got.Metadata[poolAliasConflictMetadataKey])
	}
	if got.Metadata[poolAliasConflictCountMetadataKey] != "" {
		t.Fatalf("pool_alias_conflict_count = %q, want cleared", got.Metadata[poolAliasConflictCountMetadataKey])
	}
	if got.Metadata[poolAliasConflictAtMetadataKey] != "" {
		t.Fatalf("pool_alias_conflict_at = %q, want cleared", got.Metadata[poolAliasConflictAtMetadataKey])
	}
}

func TestNormalizeNonExpandingPoolSessionBeadReclaimsDeferredAlias(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":                        "cashmaster/refinery",
			"agent_name":                      "cashmaster/refinery-1",
			"alias":                           "",
			"session_name":                    "s-refinery-stale",
			"state":                           "awake",
			poolManagedMetadataKey:            boolMetadata(true),
			"pool_slot":                       "1",
			poolAliasConflictMetadataKey:      "cashmaster/refinery",
			poolAliasConflictCountMetadataKey: "3",
			poolAliasConflictAtMetadataKey:    "2026-05-06T02:30:00Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)

	result, err := normalizeNonExpandingPoolSessionInfo(bp, &cfg.Agents[0], sessiontest.SeedBead(t, stale))
	if err != nil {
		t.Fatalf("normalizeNonExpandingPoolSessionInfo: %v", err)
	}
	if got := result.Alias; got != "cashmaster/refinery" {
		t.Fatalf("result alias = %q, want canonical alias", got)
	}
	if got := result.PoolAliasConflict; got != "" {
		t.Fatalf("result pool_alias_conflict = %q, want cleared", got)
	}
	stored, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", stale.ID, err)
	}
	if got := stored.Metadata["alias"]; got != "cashmaster/refinery" {
		t.Fatalf("stored alias = %q, want canonical alias", got)
	}
	if got := stored.Metadata[poolAliasConflictMetadataKey]; got != "" {
		t.Fatalf("stored pool_alias_conflict = %q, want cleared", got)
	}
	if got := stored.Metadata[poolAliasConflictCountMetadataKey]; got != "" {
		t.Fatalf("stored pool_alias_conflict_count = %q, want cleared", got)
	}
	if got := stored.Metadata[poolAliasConflictAtMetadataKey]; got != "" {
		t.Fatalf("stored pool_alias_conflict_at = %q, want cleared", got)
	}
}

func TestReconcilerClosesUnselectedCanonicalSingletonBeforeAliasReclaim(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery-1",
			"session_name":         "s-refinery-stale",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-canonical",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	priority := 10
	if _, err := store.Create(beads.Bead{
		Title:    "stale assigned work",
		Type:     "task",
		Status:   "in_progress",
		Priority: &priority,
		Assignee: "cashmaster/refinery-1",
		Metadata: map[string]string{
			"gc.routed_to": "cashmaster/refinery",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 0",
		}},
	}

	var buildStderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &buildStderr)
	if _, ok := dsResult.State[canonical.Metadata["session_name"]]; ok {
		t.Fatalf("desired state includes unselected canonical singleton %q; keys=%v", canonical.Metadata["session_name"], mapKeys(dsResult.State))
	}
	if _, ok := dsResult.State[stale.Metadata["session_name"]]; !ok {
		t.Fatalf("desired state missing stale singleton resume %q; keys=%v stderr=%q", stale.Metadata["session_name"], mapKeys(dsResult.State), buildStderr.String())
	}

	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 5, 6, 4, 0, 0, 0, time.UTC)}
	var reconcileStdout, reconcileStderr bytes.Buffer
	reconcileSessionBeads(
		context.Background(), sessions, dsResult.State, configuredSessionNames(cfg, "", store), cfg, sp,
		store, nil, nil, nil, newDrainTracker(), map[string]int{"cashmaster/refinery": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &reconcileStdout, &reconcileStderr,
	)

	closedCanonical, err := store.Get(canonical.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", canonical.ID, err)
	}
	if closedCanonical.Status != "closed" {
		t.Fatalf("canonical status = %q, want closed; stdout=%q stderr=%q", closedCanonical.Status, reconcileStdout.String(), reconcileStderr.String())
	}
	if want := sessionpkg.CanonicalCloseReason("orphaned"); closedCanonical.Metadata["close_reason"] != want {
		t.Fatalf("canonical close_reason = %q, want %q", closedCanonical.Metadata["close_reason"], want)
	}

	var syncStderr bytes.Buffer
	syncSessionBeads(cityPath, store, dsResult.State, sp, allConfiguredDS(dsResult.State), cfg, clk, &syncStderr, false)
	reclaimed, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", stale.ID, err)
	}
	if got := reclaimed.Metadata["alias"]; got != "cashmaster/refinery" {
		t.Fatalf("reclaimed alias = %q, want canonical alias; sync stderr=%q", got, syncStderr.String())
	}
	if got := reclaimed.Metadata[poolAliasConflictMetadataKey]; got != "" {
		t.Fatalf("pool_alias_conflict = %q, want cleared", got)
	}
}

// TestSyncDoesNotMintDuplicateForSameCycleSingletonCreate is the load-bearing
// no-duplicate-mint regression across the W-delete raw-half deletion. buildDesiredState
// creates a canonical-singleton pool session (poolSlot 0) through the Info create front
// door, which persists the bead and appends its Info to the snapshot via addInfo. When
// sync runs on that SAME snapshot it must observe the just-created session_name — else,
// because poolSlot-0 identities skip the store-recovery fallback, it would MINT A
// DUPLICATE open bead. Now that the snapshot holds no raw half, sync always re-lists the
// raw beads from the store, which durably holds the created bead, so it takes the clean
// update path. Reverting sync to reuse a stale in-memory raw slice re-mints the
// duplicate and fails this test.
func TestSyncDoesNotMintDuplicateForSameCycleSingletonCreate(t *testing.T) {
	store := beads.NewMemStore()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MinActiveSessions: intPtr(1),
			MaxActiveSessions: intPtr(1), // canonical singleton pool => poolSlot 0
		}},
	}
	sessionBeads := newSessionBeadSnapshot(nil)
	var buildStderr bytes.Buffer
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, sessionBeads, nil, &buildStderr,
	)

	// The create appended the new session to the snapshot's typed half (addInfo).
	infos := sessionBeads.OpenInfos()
	if len(infos) != 1 {
		t.Fatalf("expected 1 created singleton session, got %d; stderr=%q", len(infos), buildStderr.String())
	}
	sessionName := infos[0].SessionNameMetadata
	if sessionName == "" {
		t.Fatalf("created singleton has empty session_name")
	}
	if got := infos[0].PoolSlot; got != "" {
		t.Fatalf("singleton pool session pool_slot = %q, want empty (the un-recovered poolSlot-0 case)", got)
	}

	// Sync on the SAME snapshot (the production no-reload window).
	clk := &clock.Fake{Time: time.Date(2026, 5, 6, 4, 0, 0, 0, time.UTC)}
	var syncStderr bytes.Buffer
	syncSessionBeadsWithSnapshotAndRigStores(
		cityPath, beads.SessionStore{Store: store}, nil, dsResult.State,
		runtime.NewFake(), allConfiguredDS(dsResult.State), cfg, clk, &syncStderr, false, sessionBeads,
	)

	// No duplicate was minted: exactly one open bead carries the created session_name.
	open, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	count := 0
	for _, b := range open {
		if strings.TrimSpace(b.Metadata["session_name"]) == sessionName {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("open beads for session_name %q = %d, want 1 (sync minted a duplicate); sync stderr=%q", sessionName, count, syncStderr.String())
	}
	// The created bead's pending_create_claim survives (a duplicate mint would orphan it).
	createdStored, err := store.Get(infos[0].ID)
	if err != nil {
		t.Fatalf("Get(created %s): %v", infos[0].ID, err)
	}
	if strings.TrimSpace(createdStored.Metadata["pending_create_claim"]) != "true" {
		t.Fatalf("created singleton pending_create_claim = %q, want true (orphaned by a duplicate mint?)", createdStored.Metadata["pending_create_claim"])
	}
}

func TestProductionOrderDeferredSingletonAliasReclaimsOnSecondTick(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery-1",
			"session_name":         "s-refinery-stale",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-canonical",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	priority := 10
	if _, err := store.Create(beads.Bead{
		Title:    "stale assigned work",
		Type:     "task",
		Status:   "in_progress",
		Priority: &priority,
		Assignee: "cashmaster/refinery-1",
		Metadata: map[string]string{
			"gc.routed_to": "cashmaster/refinery",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 0",
		}},
	}
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 5, 6, 4, 0, 0, 0, time.UTC)}

	var firstBuildStderr bytes.Buffer
	firstTick := buildDesiredState("test-city", cityPath, clk.Now().UTC(), cfg, sp, store, &firstBuildStderr)
	if _, ok := firstTick.State[stale.Metadata["session_name"]]; !ok {
		t.Fatalf("first tick desired state missing stale singleton resume %q; keys=%v stderr=%q", stale.Metadata["session_name"], mapKeys(firstTick.State), firstBuildStderr.String())
	}

	var firstSyncStderr bytes.Buffer
	syncSessionBeadsWithSnapshotAndRigStores(
		cityPath,
		beads.SessionStore{Store: store},
		nil,
		firstTick.State,
		sp,
		configuredSessionNames(cfg, "", store),
		cfg,
		clk,
		&firstSyncStderr,
		true,
		nil,
	)
	stillDeferred, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", stale.ID, err)
	}
	if got := stillDeferred.Metadata["alias"]; got != "" {
		t.Fatalf("first sync alias = %q, want deferred while canonical owner remains open", got)
	}
	if got := stillDeferred.Metadata[poolAliasConflictMetadataKey]; got != "cashmaster/refinery" {
		t.Fatalf("first sync pool_alias_conflict = %q, want canonical alias; sync stderr=%q", got, firstSyncStderr.String())
	}

	open, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	var reconcileStdout, reconcileStderr bytes.Buffer
	reconcileSessionBeads(
		context.Background(), open, firstTick.State, configuredSessionNames(cfg, "", store), cfg, sp,
		store, nil, nil, nil, newDrainTracker(), map[string]int{"cashmaster/refinery": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &reconcileStdout, &reconcileStderr,
	)
	closedCanonical, err := store.Get(canonical.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", canonical.ID, err)
	}
	if closedCanonical.Status != "closed" {
		t.Fatalf("canonical status = %q, want closed after first production-order reconcile; stdout=%q stderr=%q", closedCanonical.Status, reconcileStdout.String(), reconcileStderr.String())
	}

	clk.Advance(time.Minute)
	var secondBuildStderr bytes.Buffer
	secondTick := buildDesiredState("test-city", cityPath, clk.Now().UTC(), cfg, sp, store, &secondBuildStderr)
	var secondSyncStderr bytes.Buffer
	syncSessionBeads(
		cityPath,
		store,
		secondTick.State,
		sp,
		allConfiguredDS(secondTick.State),
		cfg,
		clk,
		&secondSyncStderr,
		true,
	)

	reclaimed, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", stale.ID, err)
	}
	if got := reclaimed.Metadata["alias"]; got != "cashmaster/refinery" {
		t.Fatalf("second tick alias = %q, want canonical alias; build stderr=%q sync stderr=%q", got, secondBuildStderr.String(), secondSyncStderr.String())
	}
	if got := reclaimed.Metadata["pool_slot"]; got != "" {
		t.Fatalf("second tick pool_slot = %q, want empty after singleton recovery", got)
	}
	if got := reclaimed.Metadata[poolAliasConflictMetadataKey]; got != "" {
		t.Fatalf("pool_alias_conflict = %q, want cleared", got)
	}
	if got := reclaimed.Metadata[poolAliasConflictCountMetadataKey]; got != "" {
		t.Fatalf("pool_alias_conflict_count = %q, want cleared", got)
	}
	if got := reclaimed.Metadata[poolAliasConflictAtMetadataKey]; got != "" {
		t.Fatalf("pool_alias_conflict_at = %q, want cleared", got)
	}
}

func TestDiscoverSessionBeadsSkipsStaleMaxOneWhenDependencyFloorDesired(t *testing.T) {
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "gascity/db-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:gascity/db-1", "template:gascity/db"},
		Metadata: map[string]string{
			"template":             "gascity/db",
			"agent_name":           "gascity/db-1",
			"alias":                "gascity/db-1",
			"session_name":         "s-db-stale",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, stale))
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "db",
			Dir:               "gascity",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
	}
	desired := map[string]TemplateParams{
		"s-db-canonical": {
			TemplateName:   "gascity/db",
			InstanceName:   "s-db-canonical",
			DependencyOnly: true,
		},
	}
	bp := &agentBuildParams{
		cityPath:     t.TempDir(),
		city:         cfg,
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       cfg.Agents,
	}

	discoverSessionBeadsWithRoots(bp, cfg, desired, nil, map[string]bool{"gascity/db": true}, nil, io.Discard)

	if _, ok := desired[stale.Metadata["session_name"]]; ok {
		t.Fatalf("desired state includes stale duplicate dependency-floor sibling; keys=%v", mapKeys(desired))
	}
}

func TestNonExpandingPoolIdentitySlotRecognizesOutOfRangeNumericSuffix(t *testing.T) {
	cfgAgent := &config.Agent{
		Name:              "refinery",
		Dir:               "cashmaster",
		StartCommand:      "true",
		MaxActiveSessions: intPtr(1),
	}

	if got := nonExpandingPoolIdentitySlot(cfgAgent, "cashmaster/refinery-10"); got != 10 {
		t.Fatalf("slot = %d, want out-of-range stale singleton suffix 10 recognized", got)
	}
}

func TestBuildDesiredState_MaxOneCanonicalBeadIsIdempotent(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	canonical, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-canonical",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
		}},
	}

	before, err := store.Get(canonical.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", canonical.ID, err)
	}
	var stderr bytes.Buffer
	first := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	afterFirst, err := store.Get(canonical.ID)
	if err != nil {
		t.Fatalf("Get(%s) after first pass: %v", canonical.ID, err)
	}
	second := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	afterSecond, err := store.Get(canonical.ID)
	if err != nil {
		t.Fatalf("Get(%s) after second pass: %v", canonical.ID, err)
	}

	if _, ok := first.State[canonical.Metadata["session_name"]]; !ok {
		t.Fatalf("first desired state missing canonical singleton; keys=%v", mapKeys(first.State))
	}
	if _, ok := second.State[canonical.Metadata["session_name"]]; !ok {
		t.Fatalf("second desired state missing canonical singleton; keys=%v", mapKeys(second.State))
	}
	if before.Title != afterFirst.Title || !reflect.DeepEqual(before.Metadata, afterFirst.Metadata) || !reflect.DeepEqual(before.Labels, afterFirst.Labels) {
		t.Fatalf("first pass mutated canonical bead: before=%#v after=%#v", before, afterFirst)
	}
	if afterFirst.Title != afterSecond.Title || !reflect.DeepEqual(afterFirst.Metadata, afterSecond.Metadata) || !reflect.DeepEqual(afterFirst.Labels, afterSecond.Labels) {
		t.Fatalf("second pass mutated canonical bead: first=%#v second=%#v", afterFirst, afterSecond)
	}
	if strings.Contains(stderr.String(), "collapsing phantom pool identity") {
		t.Fatalf("stderr = %q, want no normalization diagnostic for canonical bead", stderr.String())
	}
}

func TestBuildDesiredState_NamepoolMaxOneUsesNamepoolIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			NamepoolNames:     []string{"furiosa"},
			ScaleCheck:        "printf 1",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if len(dsResult.State) != 1 {
		t.Fatalf("desired sessions = %d, want 1", len(dsResult.State))
	}
	var tp TemplateParams
	for _, candidate := range dsResult.State {
		tp = candidate
	}
	if tp.InstanceName != "rig/furiosa" {
		t.Fatalf("InstanceName = %q, want namepool identity", tp.InstanceName)
	}
	if tp.PoolSlot != 1 {
		t.Fatalf("PoolSlot = %d, want namepool slot 1", tp.PoolSlot)
	}
	sessionBeads, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session beads = %d, want 1", len(sessionBeads))
	}
	got := sessionBeads[0]
	if got.Metadata["agent_name"] != "rig/furiosa" {
		t.Fatalf("agent_name = %q, want namepool identity", got.Metadata["agent_name"])
	}
	if got.Metadata["alias"] != "rig/furiosa" {
		t.Fatalf("alias = %q, want namepool identity", got.Metadata["alias"])
	}
	if got.Metadata["pool_slot"] != "1" {
		t.Fatalf("pool_slot = %q, want 1 for namepool singleton", got.Metadata["pool_slot"])
	}
}

func TestBuildDesiredState_NewPoolSessionBeadDefersAliasWhenConcreteAliasTaken(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()

	if _, err := store.Create(beads.Bead{
		Title:  "manual session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:rig/manual"},
		Metadata: map[string]string{
			"template":       "rig/manual",
			"agent_name":     "rig/manual",
			"alias":          "rig/claude-1",
			"session_name":   "manual-rig-claude-1",
			"state":          "awake",
			"session_origin": "manual",
			"manual_session": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "claude",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "printf 1",
		}},
	}

	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	sessionBeads, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	var created beads.Bead
	for _, candidate := range sessionBeads {
		if candidate.Metadata[poolManagedMetadataKey] == boolMetadata(true) {
			created = candidate
			break
		}
	}
	if created.ID == "" {
		t.Fatalf("did not create a managed pool session bead; beads=%#v", sessionBeads)
	}
	if got := created.Metadata["agent_name"]; got != "rig/claude-1" {
		t.Fatalf("created agent_name = %q, want concrete slot identity", got)
	}
	if got := created.Metadata["alias"]; got != "" {
		t.Fatalf("created alias = %q, want deferred until alias guard accepts it", got)
	}
	if got := created.Metadata["pool_slot"]; got != "1" {
		t.Fatalf("created pool_slot = %q, want 1", got)
	}
	tp, ok := dsResult.State[created.Metadata["session_name"]]
	if !ok {
		t.Fatalf("desired state missing created session %q; keys=%v", created.Metadata["session_name"], mapKeys(dsResult.State))
	}
	if got := tp.Alias; got != "" {
		t.Fatalf("deferred pool TemplateParams.Alias = %q, want empty until alias is claimed", got)
	}
	if got := tp.Env["GC_ALIAS"]; got != "" {
		t.Fatalf("deferred pool GC_ALIAS = %q, want empty until alias is claimed", got)
	}
	if got := tp.Env["GC_AGENT"]; got != tp.SessionName {
		t.Fatalf("deferred pool GC_AGENT = %q, want bead session name %q", got, tp.SessionName)
	}
	if tp.EnvIdentityStamped {
		t.Fatal("deferred pool EnvIdentityStamped = true, want false until alias is claimed")
	}

	clk := &clock.Fake{Time: time.Date(2026, 5, 7, 15, 10, 0, 0, time.UTC)}
	var syncStderr bytes.Buffer
	syncSessionBeads(cityPath, store, dsResult.State, runtime.NewFake(), allConfiguredDS(dsResult.State), cfg, clk, &syncStderr, false)
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["alias"] != "" {
		t.Fatalf("synced alias = %q, want still deferred after conflict", got.Metadata["alias"])
	}
	if got.Metadata[poolAliasConflictMetadataKey] != "rig/claude-1" {
		t.Fatalf("pool_alias_conflict = %q, want rig/claude-1", got.Metadata[poolAliasConflictMetadataKey])
	}
	if !strings.Contains(syncStderr.String(), "unavailable") {
		t.Fatalf("sync stderr %q does not mention alias conflict", syncStderr.String())
	}
}

func TestSelectOrCreatePoolSessionBead_SerializesAliasCheckAndCreate(t *testing.T) {
	store := newBlockingPoolCreateStore("claude-1")
	cityPath := t.TempDir()
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	newBuildParams := func() *agentBuildParams {
		return &agentBuildParams{
			cityPath:     cityPath,
			beadStore:    store,
			sessionBeads: &sessionBeadSnapshot{},
			agents:       []config.Agent{cfgAgent},
		}
	}

	type createResult struct {
		info sessionpkg.Info
		slot int
		err  error
	}
	results := make(chan createResult, 2)
	create := func() {
		info, slot, err := selectOrCreatePoolSessionBead(newBuildParams(), &cfgAgent, "claude", nil, map[string]bool{}, map[int]bool{})
		results <- createResult{info: info, slot: slot, err: err}
	}
	go create()
	go create()

	select {
	case <-store.firstCreateStarted:
	case <-time.After(time.Second):
		t.Fatal("first pool create did not start")
	}

	select {
	case <-store.secondCreateStarted:
		close(store.releaseFirstCreate)
		close(store.releaseSecondCreate)
		t.Fatal("second pool create reached the store before first create finished; alias lock did not serialize create")
	case <-time.After(150 * time.Millisecond):
		close(store.releaseFirstCreate)
		select {
		case <-store.secondCreateStarted:
			close(store.releaseSecondCreate)
		case <-time.After(time.Second):
			t.Fatal("second pool create did not start after first create completed")
		}
	}

	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("selectOrCreatePoolSessionBead result %d: %v", i+1, result.err)
		}
		if result.info.ID == "" {
			t.Fatalf("selectOrCreatePoolSessionBead result %d returned empty session", i+1)
		}
		if result.slot != 1 {
			t.Fatalf("selectOrCreatePoolSessionBead result %d slot = %d, want 1", i+1, result.slot)
		}
	}

	sessionBeads, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	aliasOwners := 0
	for _, bead := range sessionBeads {
		if bead.Metadata["alias"] == "claude-1" {
			aliasOwners++
		}
	}
	if aliasOwners != 1 {
		t.Fatalf("pool alias owners = %d, want exactly one; beads=%#v", aliasOwners, sessionBeads)
	}
}

func TestCreatePoolSessionBeadWithGuardedAliasSerializesResolvedTmuxAlias(t *testing.T) {
	store := newBlockingPoolCreateStore("")
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
			TmuxAlias:         "crew--{{.CityName}}",
		}},
	}
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, io.Discard)
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	cfgAgent := &cfg.Agents[0]

	results := make(chan struct {
		info sessionpkg.Info
		err  error
	}, 2)
	create := func(qualifiedInstance string, slot int) {
		info, err := createPoolSessionBeadWithGuardedAlias(bp, cfgAgent, "worker", qualifiedInstance, slot, nil)
		results <- struct {
			info sessionpkg.Info
			err  error
		}{info: info, err: err}
	}
	go create("worker-1", 1)
	go create("worker-2", 2)

	select {
	case <-store.firstCreateStarted:
	case <-time.After(time.Second):
		t.Fatal("first pool create did not start")
	}

	select {
	case <-store.secondCreateStarted:
		close(store.releaseFirstCreate)
		close(store.releaseSecondCreate)
		t.Fatal("second pool create reached the store before first tmux_alias create finished")
	case <-time.After(150 * time.Millisecond):
		close(store.releaseFirstCreate)
		select {
		case <-store.secondCreateStarted:
			close(store.releaseSecondCreate)
		case <-time.After(time.Second):
			t.Fatal("second pool create did not start after first create completed")
		}
	}

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("create result %d: %v", i+1, result.err)
		}
		sessionName := result.info.SessionNameMetadata
		if sessionName == "" {
			t.Fatalf("create result %d has empty session_name: %#v", i+1, result.info)
		}
		if seen[sessionName] {
			t.Fatalf("duplicate session_name %q across tmux_alias pool creates", sessionName)
		}
		stored, err := store.Get(result.info.ID)
		if err != nil {
			t.Fatalf("store.Get(%s): %v", result.info.ID, err)
		}
		if got := stored.Metadata["session_name"]; got != sessionName {
			t.Fatalf("stored session_name for %s = %q, want %q", result.info.ID, got, sessionName)
		}
		seen[sessionName] = true
	}
	if !seen["crew--test-city"] {
		t.Fatalf("created names = %#v, want one unsuffixed tmux_alias name", seen)
	}
}

func TestCreatePoolSessionBeadWithGuardedAliasDropsTmuxAliasWhenIdentifierLockFails(t *testing.T) {
	store := beads.NewMemStore()
	cityPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cityPath, []byte("city path blocks lock dir creation"), 0o600); err != nil {
		t.Fatalf("write cityPath file: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
			TmuxAlias:         "crew--{{.CityName}}",
		}},
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot(nil)

	info, err := createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], "worker", "worker-1", 1, nil)
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithGuardedAlias: %v", err)
	}

	want := PoolSessionName("worker", info.ID)
	if got := info.SessionNameMetadata; got != want {
		t.Fatalf("session_name = %q, want unique pool fallback %q when tmux_alias lock fails", got, want)
	}
	if strings.Contains(stderr.String(), "creating without alias") && strings.Contains(info.SessionNameMetadata, "crew--test-city") {
		t.Fatalf("lock failure warning emitted but session_name still used tmux_alias: %q", info.SessionNameMetadata)
	}
}

// delayingPoolCreateStore sleeps for `delay` on every session-bead create so
// tests can measure whether realizePoolDesiredSessions runs distinct-alias
// creates in parallel or serializes them. Wraps MemStore for all other ops.
type delayingPoolCreateStore struct {
	*beads.MemStore
	delay                   time.Duration
	mu                      sync.Mutex
	activeSessionCreates    int
	maxActiveSessionCreates int
}

type failingPoolCreateStore struct {
	*beads.MemStore
	mu                sync.Mutex
	failuresRemaining int
}

func (s *failingPoolCreateStore) Create(bead beads.Bead) (beads.Bead, error) {
	if bead.Type == sessionBeadType && s.claimFailure() {
		return beads.Bead{}, errors.New("injected pool create failure")
	}
	return s.MemStore.Create(bead)
}

func (s *failingPoolCreateStore) claimFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failuresRemaining <= 0 {
		return false
	}
	s.failuresRemaining--
	return true
}

func (s *delayingPoolCreateStore) Create(bead beads.Bead) (beads.Bead, error) {
	if bead.Type == sessionBeadType {
		s.beginSessionCreate()
		defer s.endSessionCreate()
		time.Sleep(s.delay)
	}
	return s.MemStore.Create(bead)
}

func (s *delayingPoolCreateStore) beginSessionCreate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeSessionCreates++
	if s.activeSessionCreates > s.maxActiveSessionCreates {
		s.maxActiveSessionCreates = s.activeSessionCreates
	}
}

func (s *delayingPoolCreateStore) endSessionCreate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeSessionCreates--
}

func (s *delayingPoolCreateStore) maxConcurrentSessionCreates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxActiveSessionCreates
}

// TestRealizePoolDesiredSessions_ParallelizesDistinctAliasCreates verifies
// that the three-phase pipeline drives distinct-alias pool creates in parallel
// rather than serializing them one-per-tick. Issue #2319 reported O(N) wall
// time on pool fanouts because each create acquired a per-alias session lock
// + dolt commit in a tight serial loop. With bounded-parallel phase B, wall
// time should collapse to roughly ceil(N/poolRealizeParallelism) × delay.
//
// The store records in-flight session-bead creates directly so a regression
// that re-serializes the loop (e.g., a future refactor that accidentally holds
// a mutex across the create call) or collapses the worker fanout fails without
// depending on wall-clock scheduler slack.
func TestRealizePoolDesiredSessions_ParallelizesDistinctAliasCreates(t *testing.T) {
	const (
		requestCount = 16
		// Keep concurrent creates overlapping long enough to observe the full
		// worker cap without using elapsed time as the assertion.
		createDelay = 100 * time.Millisecond
	)
	store := &delayingPoolCreateStore{MemStore: beads.NewMemStore(), delay: createDelay}
	cityPath := t.TempDir()
	maxWakes := requestCount
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{{
			Name:              "claude",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(requestCount),
		}},
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = &sessionBeadSnapshot{}
	desired := map[string]TemplateParams{}

	requests := make([]SessionRequest, 0, requestCount)
	for i := 0; i < requestCount; i++ {
		requests = append(requests, SessionRequest{Template: "claude", Tier: "new"})
	}
	state := PoolDesiredState{Template: "claude", Requests: requests}

	realizePoolDesiredSessions(bp, &cfg.Agents[0], state, desired, &stderr)

	if got := len(desired); got != requestCount {
		t.Fatalf("desired count = %d, want %d; stderr=%q", got, requestCount, stderr.String())
	}

	if got := store.maxConcurrentSessionCreates(); got < poolRealizeParallelism {
		t.Fatalf("session bead creates max concurrency = %d, want at least %d", got, poolRealizeParallelism)
	}

	aliases := make(map[string]bool, requestCount)
	sessionNames := make(map[string]bool, requestCount)
	slots := make(map[int]bool, requestCount)
	for name, tp := range desired {
		if sessionNames[name] {
			t.Fatalf("duplicate desired session name %q across pool entries", name)
		}
		sessionNames[name] = true
		if tp.Alias == "" {
			t.Fatalf("desired entry %q has empty alias; want unique per-slot alias", name)
		}
		if aliases[tp.Alias] {
			t.Fatalf("duplicate alias %q across desired entries (session %q)", tp.Alias, name)
		}
		aliases[tp.Alias] = true
		if slots[tp.PoolSlot] {
			t.Fatalf("duplicate pool_slot %d across desired entries (session %q)", tp.PoolSlot, name)
		}
		slots[tp.PoolSlot] = true
	}
}

func TestCreatePoolSessionBeadWithGuardedAlias_LogsAliasLockSetupFailure(t *testing.T) {
	store := beads.NewMemStore()
	cityPath := filepath.Join(t.TempDir(), "city-file")
	if err := os.WriteFile(cityPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	bp := &agentBuildParams{
		cityPath:     cityPath,
		beadStore:    store,
		sessionBeads: &sessionBeadSnapshot{},
		stderr:       &stderr,
	}

	bead, err := createPoolSessionBeadWithGuardedAlias(bp, nil, "claude", "claude-1", 1, nil)
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithGuardedAlias: %v", err)
	}
	if got := bead.Alias; got != "" {
		t.Fatalf("alias = %q, want empty fallback when alias lock setup fails", got)
	}
	if !strings.Contains(stderr.String(), "locking alias \"claude-1\"") || !strings.Contains(stderr.String(), "creating without alias") {
		t.Fatalf("stderr = %q, want alias-lock setup failure and unaliased fallback", stderr.String())
	}
}

func TestCreatePoolSessionBeadWithGuardedAliasRejectsUnsupportedTransport(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Provider: "opencode"},
		Session:   config.SessionConfig{Provider: config.SessionTransportACP},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				Command:     "echo",
				Args:        []string{"provider"},
				ACPCommand:  "echo",
				ACPArgs:     []string{"acp"},
				PromptMode:  "none",
				SupportsACP: boolPtr(true),
			},
		},
		Agents: []config.Agent{{
			Name:              "worker",
			Provider:          "opencode",
			Session:           config.SessionTransportTmux,
			MaxActiveSessions: intPtr(1),
		}},
	}
	store := beads.NewMemStore()
	sp := &acpOnlyDesiredStateProvider{Fake: runtime.NewFake()}
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, sp, time.Now().UTC(), store, io.Discard)

	_, err := createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], "worker", "worker", 0, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot route tmux sessions") {
		t.Fatalf("createPoolSessionBeadWithGuardedAlias error = %v, want tmux routing rejection", err)
	}
	beads, listErr := store.ListByLabel(sessionBeadLabel, 0)
	if listErr != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, listErr)
	}
	if len(beads) != 0 {
		t.Fatalf("session bead count = %d, want 0 after rejected transport: %#v", len(beads), beads)
	}
}

func TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession(t *testing.T) {
	skipSlowCmdGCTest(t, "uses real bd subprocesses for routed-work scale checks; run make test-cmd-gc-process for full coverage")
	bdPath, err := findPreferredBinary("bd", "/home/ubuntu/.local/bin/bd")
	if err != nil {
		t.Skip("bd not installed")
	}
	jqPath, err := findPreferredBinary("jq")
	if err != nil {
		t.Skip("jq not installed")
	}

	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(jqPath), os.Getenv("PATH")}, string(os.PathListSeparator)))
	t.Setenv("BEADS_DIR", beadsDir)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	runExternal(t, cityPath, bdPath, "init", "-p", "ct", "--skip-hooks", "-q")
	runExternal(t, cityPath, bdPath, "config", "set", "types.custom", "session")

	store := beads.NewBdStore(cityPath, beads.ExecCommandRunnerWithEnv(map[string]string{
		"BEADS_DIR": beadsDir,
	}))
	if _, err := store.Create(beads.Bead{
		Title:  "queued polecat work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "polecat",
		},
	}); err != nil {
		t.Fatalf("create routed work bead: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "polecat",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
		}},
	}

	var stderr strings.Builder
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	if len(dsResult.AssignedWorkBeads) != 0 {
		t.Fatalf("AssignedWorkBeads = %d, want 0 for routed unassigned work", len(dsResult.AssignedWorkBeads))
	}
	if got := dsResult.ScaleCheckCounts["polecat"]; got != 1 {
		t.Fatalf("ScaleCheckCounts[polecat] = %d, want 1 from default scale_check routed ready work", got)
	}
	polecatSessions := 0
	for _, tp := range dsResult.State {
		if tp.TemplateName == "polecat" {
			polecatSessions++
		}
	}
	if polecatSessions != 1 {
		t.Fatalf("polecat desired sessions = %d, want 1 for min=0 routed ready work; stderr:\n%s", polecatSessions, stderr.String())
	}
}

func TestBuildDesiredState_GH1654PoolReadyWorkGrowsPastMinActiveSessions(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	const template = "worker"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              template,
			StartCommand:      "true",
			MinActiveSessions: intPtr(3),
			MaxActiveSessions: intPtr(100),
		}},
	}
	cfgAgent := &cfg.Agents[0]

	for i := 0; i < 6; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("queued work %d", i+1),
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": template,
			},
		}); err != nil {
			t.Fatalf("create queued work: %v", err)
		}
	}

	existingSessionNames := make(map[string]bool)
	for slot := 1; slot <= 3; slot++ {
		_, qualifiedInstance := poolInstanceIdentity(cfgAgent, slot, io.Discard)
		session, err := createPoolSessionBead(sessionFrontDoor(store), cfgAgent.QualifiedName(), time.Now().UTC(), poolSessionCreateIdentity{
			AgentName: qualifiedInstance,
			Alias:     qualifiedInstance,
			Slot:      slot,
		})
		if err != nil {
			t.Fatalf("create active pool session: %v", err)
		}
		if err := store.SetMetadata(session.ID, "state", "active"); err != nil {
			t.Fatalf("set state: %v", err)
		}
		if err := store.SetMetadata(session.ID, "pending_create_claim", ""); err != nil {
			t.Fatalf("clear pending_create_claim: %v", err)
		}
		if err := store.SetMetadata(session.ID, "pending_create_started_at", ""); err != nil {
			t.Fatalf("clear pending_create_started_at: %v", err)
		}
		existingSessionNames[session.SessionNameMetadata] = true
	}

	sessionSnapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	var stderr strings.Builder
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, sessionSnapshot, nil, &stderr,
	)

	if got := dsResult.ScaleCheckCounts[template]; got != 6 {
		t.Fatalf("ScaleCheckCounts[%s] = %d, want 6 queued ready beads", template, got)
	}
	desiredSessionNames := make(map[string]bool)
	for _, tp := range dsResult.State {
		if tp.TemplateName == template {
			desiredSessionNames[tp.SessionName] = true
		}
	}
	if got := len(desiredSessionNames); got != 6 {
		t.Fatalf("%s desired sessions = %d, want 6 (3 retained min sessions + 3 new slots); stderr:\n%s", template, got, stderr.String())
	}
	for sessionName := range existingSessionNames {
		if !desiredSessionNames[sessionName] {
			t.Fatalf("existing min-floor session %s was not retained in desired state; desired=%#v", sessionName, desiredSessionNames)
		}
	}
	sessions, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("list session beads: %v", err)
	}
	if len(sessions) != 6 {
		t.Fatalf("stored session beads = %d, want 6 total after growing past min_active_sessions; stderr:\n%s", len(sessions), stderr.String())
	}
}

func TestBuildDesiredState_MinZeroDefaultScaleCheckNoWorkDropsPendingPoolCreate(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	const template = "polecat"
	session := beads.Bead{
		ID:     "session-pending",
		Title:  "polecat",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:" + template},
		Metadata: map[string]string{
			"template":             template,
			"session_name":         PoolSessionName(template, "session-pending"),
			"agent_name":           template,
			"state":                "creating",
			"pending_create_claim": boolMetadata(true),
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              template,
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
		}},
	}

	var stderr strings.Builder
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, newSessionBeadSnapshot([]beads.Bead{session}), nil, &stderr,
	)

	if got := dsResult.ScaleCheckCounts[template]; got != 0 {
		t.Fatalf("ScaleCheckCounts[%s] = %d, want 0 with no routed ready work", template, got)
	}
	if _, ok := dsResult.State[session.Metadata["session_name"]]; ok {
		t.Fatalf("pending pool session was preserved with no runnable work; desired=%#v stderr:\n%s", dsResult.State, stderr.String())
	}
}

func TestBuildDesiredState_PoolInFlightSessionsPreservePartialScaleDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	const template = "worker"

	for i := 0; i < 5; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("queued work %d", i+1),
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": template,
			},
		}); err != nil {
			t.Fatalf("create queued work: %v", err)
		}
	}
	var inFlightSessionIDs []string
	for i := 0; i < 2; i++ {
		session, err := store.Create(beads.Bead{
			Title:  template,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "agent:" + template},
			Metadata: map[string]string{
				"template":             template,
				"agent_name":           template,
				"state":                "asleep",
				"pending_create_claim": boolMetadata(true),
				poolManagedMetadataKey: boolMetadata(true),
			},
		})
		if err != nil {
			t.Fatalf("create pending pool session: %v", err)
		}
		if err := store.SetMetadata(session.ID, "session_name", PoolSessionName(template, session.ID)); err != nil {
			t.Fatalf("set session_name: %v", err)
		}
		inFlightSessionIDs = append(inFlightSessionIDs, session.ID)
	}
	sessionSnapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              template,
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(10),
		}},
	}

	var stderr strings.Builder
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, sessionSnapshot, nil, &stderr,
	)

	if got := dsResult.ScaleCheckCounts[template]; got != 5 {
		t.Fatalf("ScaleCheckCounts[%s] = %d, want 5", template, got)
	}
	desired := 0
	for _, tp := range dsResult.State {
		if tp.TemplateName == template {
			desired++
		}
	}
	if desired != 5 {
		t.Fatalf("%s desired sessions = %d, want 5 with two in-flight plus three new; stderr:\n%s", template, desired, stderr.String())
	}
	desiredSessionNames := make(map[string]bool)
	for _, tp := range dsResult.State {
		if tp.TemplateName == template {
			desiredSessionNames[tp.SessionName] = true
		}
	}
	for _, id := range inFlightSessionIDs {
		name := PoolSessionName(template, id)
		if !desiredSessionNames[name] {
			t.Fatalf("desired state did not preserve in-flight session %s (%s); desired=%#v", id, name, desiredSessionNames)
		}
	}
	sessions, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("list session beads: %v", err)
	}
	if len(sessions) != 5 {
		t.Fatalf("stored session beads = %d, want 5 total", len(sessions))
	}
}

func TestBuildDesiredState_OnDemandNamedSession_DefaultRoutedWorkUsesTemplatePoolDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "queued mayor work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "mayor",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	foundNamed := false
	foundGeneric := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			if tp.ConfiguredNamedIdentity == "mayor" {
				foundNamed = true
				continue
			}
			foundGeneric = true
		}
	}
	if foundNamed {
		t.Fatal("template-routed work should not materialize the on-demand named session")
	}
	if !foundGeneric {
		t.Fatal("template-routed work should create generic template demand")
	}
	if dsResult.NamedSessionDemand["mayor"] {
		t.Fatal("NamedSessionDemand should not record template-routed work for mayor")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_DefaultRoutedTaskWispUsesTemplatePoolDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:     "queued mayor wisp",
		Type:      "task",
		Status:    "open",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.routed_to": "mayor",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	foundNamed := false
	foundGeneric := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" && tp.ConfiguredNamedIdentity == "mayor" {
			foundNamed = true
			continue
		}
		if tp.TemplateName == "mayor" {
			foundGeneric = true
		}
	}
	if foundNamed {
		t.Fatal("template-routed task wisp should not materialize the on-demand named session")
	}
	if !foundGeneric {
		t.Fatal("template-routed task wisp should create generic template demand")
	}
	if dsResult.NamedSessionDemand["mayor"] {
		t.Fatal("NamedSessionDemand should not record template-routed task wisp for mayor")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_DefaultRoutedTemplateUsesGenericPoolDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "queued worker work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Name:     "primary",
			Template: "worker",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	foundNamed := false
	foundGeneric := false
	for _, tp := range dsResult.State {
		if tp.TemplateName != "worker" {
			continue
		}
		if tp.ConfiguredNamedIdentity == "primary" {
			foundNamed = true
			continue
		}
		foundGeneric = true
	}
	if foundNamed {
		t.Fatal("template-routed work should not materialize the singleton named identity for worker")
	}
	if !foundGeneric {
		t.Fatal("template-routed work should create generic worker demand")
	}
	if dsResult.NamedSessionDemand["primary"] {
		t.Fatal("NamedSessionDemand should not record singleton identity demand from template route")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_DefaultRoutedTemplateDoesNotPickAmbiguousIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "queued worker work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{
			{Name: "primary", Template: "worker", Mode: "on_demand"},
			{Name: "secondary", Template: "worker", Mode: "on_demand"},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if dsResult.NamedSessionDemand["primary"] || dsResult.NamedSessionDemand["secondary"] {
		t.Fatalf("ambiguous template route recorded named demand: %v", dsResult.NamedSessionDemand)
	}
	for _, tp := range dsResult.State {
		switch tp.ConfiguredNamedIdentity {
		case "primary", "secondary":
			t.Fatalf("ambiguous template route materialized named identity: %+v", tp)
		}
	}
}

func TestBuildDesiredState_OnDemandNamedSession_DefaultRoutedNoMatchDoesNotMaterialize(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "queued unmatched work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "missing",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Name:     "primary",
			Template: "worker",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if dsResult.NamedSessionDemand["primary"] {
		t.Fatal("unmatched route should not record named-session demand")
	}
	if len(dsResult.State) != 0 {
		t.Fatalf("unmatched route should not materialize sessions: %+v", dsResult.State)
	}
}

func TestBuildDesiredState_OnDemandNamedSession_DirectAssigneeMaterializes(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "assigned mayor work",
		Type:     "task",
		Status:   "open",
		Assignee: "mayor",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	found := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("direct assignee should materialize on-demand named session")
	}
}

// TestBuildDesiredState_NamedBackingPoolNoCap_RoutedDemandDoesNotSpawnPhantoms
// is the ga-xq1fsv regression repro (molecule-patrol demand ratchet, healthy
// reads). A [[named_session]] backed by a pool-capable agent template that has
// NO custom scale_check and NO max_active_sessions cap must resolve unassigned
// gc.routed_to=<template> patrol demand to its single canonical named identity
// — never to generic "{name}-N" pool phantoms.
//
// Root cause (bda368b0e "keep template-routed graph work unassigned"): the
// backsNamedSession branch in buildDesiredStateWithSessionBeads now appends the
// template's default probe to defaultScaleTargets (build_desired_state.go:561),
// so the unassigned routed patrol roots are counted as generic pool demand by
// defaultScaleCheckCounts. ComputePoolDesiredStates then emits N "new" requests,
// and because the template has no max=1 cap (UsesCanonicalSingletonPoolIdentity
// is false) realizePoolDesiredSessions materializes them as witness-1..N
// alongside the canonical "witness" named session. Before bda368b0e the same
// probe fed only defaultNamedScaleTargets, so routed demand resolved to the
// single named identity.
//
// This facet engages on fully healthy (non-partial) reads, so the ga-01yukx /
// #3686 partial-read fail-closed guards never trigger. MemStore reads are
// non-partial, asserted below. MUST fail on e22049f86 (1 named + N phantoms)
// and pass after the fix.
func TestBuildDesiredState_NamedBackingPoolNoCap_RoutedDemandDoesNotSpawnPhantoms(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	const template = "witness"
	const routedDemand = 3
	for i := 0; i < routedDemand; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  "witness patrol root",
			Type:   "task", // actionable root (wisp/task); molecule/step roots are Ready()-excluded
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": template,
			},
		}); err != nil {
			t.Fatalf("create routed patrol demand %d: %v", i, err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         template,
			StartCommand: "true",
			WorkQuery:    "printf ''",
			// no ScaleCheck        -> default routed-work probe drives demand
			// no MaxActiveSessions -> UsesCanonicalSingletonPoolIdentity()==false,
			//                         so pool path synthesizes {name}-N identities
		}},
		NamedSessions: []config.NamedSession{{
			Template: template,
			Mode:     "always",
		}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	// This facet must engage on healthy reads, distinguishing it from the
	// ga-01yukx partial-read facet whose guards fail closed.
	if len(dsResult.ScaleCheckPartialTemplates) != 0 ||
		len(dsResult.PoolScaleCheckPartialTemplates) != 0 ||
		len(dsResult.NamedScaleCheckPartialTemplates) != 0 {
		t.Fatalf("repro must exercise healthy (non-partial) reads, but reads were partial: scale=%v pool=%v named=%v",
			dsResult.ScaleCheckPartialTemplates, dsResult.PoolScaleCheckPartialTemplates, dsResult.NamedScaleCheckPartialTemplates)
	}

	var named, phantom []string
	for key, tp := range dsResult.State {
		if tp.ConfiguredNamedIdentity == identity {
			named = append(named, key)
			continue
		}
		phantom = append(phantom, key)
	}
	if len(named) != 1 {
		t.Fatalf("expected exactly 1 canonical named session %q, got %d: %v (state=%+v)", identity, len(named), named, dsResult.State)
	}
	if len(phantom) != 0 {
		t.Fatalf("named-backing pool template %q spawned %d phantom pool session(s) %v alongside the canonical named session; "+
			"unassigned gc.routed_to patrol demand must resolve to the single named identity (<=1), not generic {name}-N pool workers "+
			"(scale_check_counts=%v)", template, len(phantom), phantom, dsResult.ScaleCheckCounts)
	}
}

func TestBuildDesiredState_OnDemandNamedSession_RuntimeAssigneeDoesNotMaterialize(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "fixture")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("create rig dir: %v", err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "fixture", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "control-dispatcher",
			Dir:               "fixture",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "control-dispatcher",
			Dir:      "fixture",
			Mode:     "on_demand",
		}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	runtimeName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
	if _, err := rigStore.Create(beads.Bead{
		Title:    "runtime-assigned control work",
		Type:     "task",
		Status:   "open",
		Assignee: runtimeName,
	}); err != nil {
		t.Fatalf("create rig work: %v", err)
	}

	dsResult := buildDesiredStateWithSessionBeads(
		cfg.EffectiveCityName(), cityPath, time.Now().UTC(), cfg,
		runtime.NewFake(), cityStore, map[string]beads.Store{"fixture": rigStore}, nil, nil, io.Discard,
	)

	if dsResult.NamedSessionDemand[identity] {
		t.Fatalf("NamedSessionDemand[%s] = true for future runtime assignee %q", identity, runtimeName)
	}
	found := false
	for _, tp := range dsResult.State {
		if tp.ConfiguredNamedIdentity == identity {
			found = true
			break
		}
	}
	if found {
		t.Fatalf("future runtime assignee %q should not materialize on-demand named session %q", runtimeName, identity)
	}
}

func TestBuildDesiredState_RigOnDemandNamedSessionAssigneeWithRouteMaterializesNamedOnly(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]string
	}{
		{
			name: "retained route metadata",
			metadata: map[string]string{
				"gc.routed_to": "riga/refinery",
			},
		},
		{
			name: "cleared route metadata",
			metadata: map[string]string{
				"gc.routed_to": "",
			},
		},
		{
			name: "absent route metadata",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "test-anthropic-auth-token")
			t.Setenv("ANTHROPIC_BASE_URL", "https://ollama.example.test")
			t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "kimi-k2.5")
			t.Setenv("OLLAMA_API_KEY", "test-ollama-token")
			cityPath := t.TempDir()
			rigPath := filepath.Join(cityPath, "riga")
			if err := os.MkdirAll(rigPath, 0o755); err != nil {
				t.Fatalf("create rig dir: %v", err)
			}
			cityStore := beads.NewMemStore()
			rigStore := beads.NewMemStore()
			if _, err := rigStore.Create(beads.Bead{
				Title:    "refinery handoff",
				Type:     "task",
				Status:   "open",
				Assignee: "riga/refinery",
				Metadata: tc.metadata,
			}); err != nil {
				t.Fatalf("create rig work: %v", err)
			}
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Rigs:      []config.Rig{{Name: "riga", Path: rigPath}},
				Agents: []config.Agent{{
					Name:              "refinery",
					Dir:               "riga",
					StartCommand:      "true",
					MaxActiveSessions: intPtr(1),
					WorkQuery:         "printf ''",
				}},
				NamedSessions: []config.NamedSession{{
					Template: "refinery",
					Dir:      "riga",
					Mode:     "on_demand",
				}},
			}

			sp := runtime.NewFake()
			clk := &clock.Fake{Time: time.Date(2026, 5, 26, 15, 0, 0, 0, time.UTC)}
			dsResult := buildDesiredStateWithSessionBeads(
				"test-city", cityPath, clk.Now().UTC(), cfg, sp,
				cityStore, map[string]beads.Store{"riga": rigStore}, nil, nil, io.Discard,
			)
			if !dsResult.NamedSessionDemand["riga/refinery"] {
				t.Fatal("NamedSessionDemand[riga/refinery] = false for direct named-session assignee")
			}
			if got := dsResult.ScaleCheckCounts["riga/refinery"]; got != 0 {
				t.Fatalf("ScaleCheckCounts[riga/refinery] = %d, want 0 for assigned named-session handoff", got)
			}
			var refineryEntries []TemplateParams
			for _, tp := range dsResult.State {
				if tp.ConfiguredNamedIdentity == "riga/refinery" || tp.TemplateName == "riga/refinery" {
					refineryEntries = append(refineryEntries, tp)
				}
			}
			if len(refineryEntries) != 1 {
				t.Fatalf("refinery desired entries = %d, want exactly one named session: %+v", len(refineryEntries), refineryEntries)
			}
			refinery := refineryEntries[0]
			if refinery.ConfiguredNamedIdentity != "riga/refinery" {
				t.Fatalf("ConfiguredNamedIdentity = %q, want riga/refinery", refinery.ConfiguredNamedIdentity)
			}
			if refinery.PoolSlot != 0 {
				t.Fatalf("PoolSlot = %d, want 0 for named session", refinery.PoolSlot)
			}
			if got := refinery.Env["GC_ALIAS"]; got != "riga/refinery" {
				t.Fatalf("GC_ALIAS = %q, want riga/refinery", got)
			}
			if got := refinery.Env["GC_TEMPLATE"]; got != "riga/refinery" {
				t.Fatalf("GC_TEMPLATE = %q, want riga/refinery", got)
			}
			for key, want := range map[string]string{
				"ANTHROPIC_AUTH_TOKEN":       "test-anthropic-auth-token",
				"ANTHROPIC_BASE_URL":         "https://ollama.example.test",
				"CLAUDE_CODE_SUBAGENT_MODEL": "kimi-k2.5",
				"OLLAMA_API_KEY":             "test-ollama-token",
			} {
				if got := refinery.Env[key]; got != want {
					t.Fatalf("refinery Env[%s] = %q, want %q", key, got, want)
				}
			}

			var stdout, stderr bytes.Buffer
			cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), cityStore)
			syncSessionBeads(cityPath, cityStore, dsResult.State, sp, cfgNames, cfg, clk, &stderr, true)
			sessions, err := loadSessionBeads(cityStore)
			if err != nil {
				t.Fatalf("loadSessionBeads: %v", err)
			}
			poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, dsResult.AssignedWorkBeads, sessionInfosFromBeads(sessions), dsResult.ScaleCheckCounts))
			if poolDesired == nil {
				poolDesired = map[string]int{}
			}
			mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)
			woken := reconcileSessionBeadsAtPath(
				context.Background(), cityPath, sessions, dsResult.State, cfgNames,
				cfg, sp, cityStore, nil, dsResult.AssignedWorkBeads, map[string]beads.Store{"riga": rigStore},
				nil, newDrainTracker(), poolDesired, dsResult.StoreQueryPartial, nil, cfg.EffectiveCityName(),
				nil, clk, events.Discard, 0, 0, &stdout, &stderr,
			)
			if woken != 1 {
				t.Fatalf("woken = %d, want 1 for refinery named session; stdout:\n%s\nstderr:\n%s", woken, stdout.String(), stderr.String())
			}
			startCfg := sp.LastStartConfig(refinery.SessionName)
			if startCfg == nil {
				t.Fatalf("LastStartConfig(%q) = nil; stdout:\n%s\nstderr:\n%s", refinery.SessionName, stdout.String(), stderr.String())
			}
			for key, want := range map[string]string{
				"ANTHROPIC_AUTH_TOKEN":       "test-anthropic-auth-token",
				"ANTHROPIC_BASE_URL":         "https://ollama.example.test",
				"CLAUDE_CODE_SUBAGENT_MODEL": "kimi-k2.5",
				"OLLAMA_API_KEY":             "test-ollama-token",
			} {
				if got := startCfg.Env[key]; got != want {
					t.Fatalf("start Env[%s] = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestBuildDesiredState_OnDemandNamedSession_IgnoresUnreachableAssignedWork(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("create rig dir: %v", err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	if _, err := cityStore.Create(beads.Bead{
		Title:    "assigned mayor work in city store",
		Type:     "task",
		Status:   "open",
		Assignee: "riga/mayor",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "mayor",
			Dir:               "riga",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Dir:      "riga",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"riga": rigStore}, nil, nil, io.Discard,
	)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "riga/mayor" || tp.ConfiguredNamedIdentity == "riga/mayor" {
			t.Fatalf("unreachable city-store assignee should not materialize rig named session: %+v", tp)
		}
	}
	if dsResult.NamedSessionDemand["riga/mayor"] {
		t.Fatal("unreachable city-store assignee should not record named-session demand")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ReachabilityUsesPerBeadSourceNotID(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("create rig dir: %v", err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cityWork, err := cityStore.Create(beads.Bead{
		Title:    "phantom city work",
		Type:     "task",
		Status:   "open",
		Assignee: "riga/mayor",
	})
	if err != nil {
		t.Fatal(err)
	}
	rigWork, err := rigStore.Create(beads.Bead{
		Title:    "same ID rig work for another session",
		Type:     "task",
		Status:   "open",
		Assignee: "riga/other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cityWork.ID != rigWork.ID {
		t.Fatalf("test setup expected overlapping city/rig IDs, got city %q rig %q", cityWork.ID, rigWork.ID)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "mayor",
			Dir:               "riga",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Dir:      "riga",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"riga": rigStore}, nil, nil, io.Discard,
	)
	if dsResult.NamedSessionDemand["riga/mayor"] {
		t.Fatal("same-ID rig bead should not make the city-store assignment reachable")
	}
}

func TestBuildDesiredState_RigPoolIgnoresAssignedWorkInUnreachableStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("create rig dir: %v", err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	sessionBead, err := cityStore.Create(beads.Bead{
		Title:  "asleep rig worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:riga/worker"},
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-gc-1",
			"state":        "asleep",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	work, err := cityStore.Create(beads.Bead{
		Title:    "unreachable city work for rig worker",
		Type:     "task",
		Assignee: sessionBead.ID,
		Metadata: map[string]string{"gc.routed_to": "riga/worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := cityStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set work in_progress: %v", err)
	}
	sessionSnapshot, err := loadSessionBeadSnapshot(cityStore)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "riga",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(5),
			ScaleCheck:        "printf 0",
		}},
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"riga": rigStore}, sessionSnapshot, nil, io.Discard,
	)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "riga/worker" {
			t.Fatalf("unreachable city-store work should not resume rig pool session: %+v", tp)
		}
	}
}

func TestBuildDesiredState_AlwaysNamedSession_MaterializesWithoutWorkBeads(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "always",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	found := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			if tp.ConfiguredNamedIdentity != "mayor" {
				t.Fatalf("ConfiguredNamedIdentity = %q, want mayor", tp.ConfiguredNamedIdentity)
			}
			if tp.ConfiguredNamedMode != "always" {
				t.Fatalf("ConfiguredNamedMode = %q, want always", tp.ConfiguredNamedMode)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("always-mode named session should materialize without work beads")
	}
}

func TestBuildDesiredState_SuspendedNamedSession_DoesNotMaterialize(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			Suspended:         true,
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "always",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			t.Fatalf("suspended named session should not materialize: %+v", tp)
		}
	}
	if dsResult.NamedSessionDemand["mayor"] {
		t.Fatal("suspended named session should not record demand")
	}
}

func TestBuildDesiredState_ProductionDemandSkipsSuspendedAgentScaleCheck(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	liveMarker := filepath.Join(cityPath, "live.probed")
	parkedMarker := filepath.Join(cityPath, "parked.probed")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{
				Name:       "live",
				ScaleCheck: scaleCheckMarkerCommand(liveMarker, 0),
			},
			{
				Name:       "parked",
				Suspended:  true,
				ScaleCheck: scaleCheckMarkerCommand(parkedMarker, 1),
			},
		},
	}

	var stderr strings.Builder
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	assertMarkerExists(t, liveMarker)
	assertMarkerAbsent(t, parkedMarker)
	if _, ok := dsResult.ScaleCheckCounts["parked"]; ok {
		t.Fatalf("ScaleCheckCounts contains suspended agent: %#v", dsResult.ScaleCheckCounts)
	}
}

func TestBuildDesiredState_ProductionDemandSkipsSuspendedRigScaleCheck(t *testing.T) {
	cityPath := t.TempDir()
	liveRigPath := filepath.Join(cityPath, "rigs", "live-rig")
	parkedRigPath := filepath.Join(cityPath, "rigs", "parked-rig")
	if err := os.MkdirAll(liveRigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parkedRigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	liveMarker := filepath.Join(cityPath, "live-rig.probed")
	parkedMarker := filepath.Join(cityPath, "parked-rig.probed")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "live-rig", Path: liveRigPath},
			{Name: "parked-rig", Path: parkedRigPath, Suspended: true},
		},
		Agents: []config.Agent{
			{
				Name:       "alpha",
				Dir:        "live-rig",
				ScaleCheck: scaleCheckMarkerCommand(liveMarker, 0),
			},
			{
				Name:       "beta",
				Dir:        "parked-rig",
				ScaleCheck: scaleCheckMarkerCommand(parkedMarker, 1),
			},
		},
	}

	var stderr strings.Builder
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	assertMarkerExists(t, liveMarker)
	assertMarkerAbsent(t, parkedMarker)
	if _, ok := dsResult.ScaleCheckCounts["parked-rig/beta"]; ok {
		t.Fatalf("ScaleCheckCounts contains agent on suspended rig: %#v", dsResult.ScaleCheckCounts)
	}
}

func TestBuildDesiredState_ProductionDemandSkipsAllScaleChecksWhenCitySuspended(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	marker := filepath.Join(cityPath, "worker.probed")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Suspended: true},
		Agents: []config.Agent{{
			Name:       "worker",
			ScaleCheck: scaleCheckMarkerCommand(marker, 1),
		}},
	}

	var stderr strings.Builder
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	assertMarkerAbsent(t, marker)
	if len(dsResult.State) != 0 {
		t.Fatalf("State = %#v, want empty for suspended city", dsResult.State)
	}
	if len(dsResult.ScaleCheckCounts) != 0 {
		t.Fatalf("ScaleCheckCounts = %#v, want none for suspended city", dsResult.ScaleCheckCounts)
	}
}

func scaleCheckMarkerCommand(marker string, count int) string {
	return fmt.Sprintf("printf probed > %s; printf %d", strconv.Quote(marker), count)
}

func assertMarkerExists(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected scale_check marker %s: %v", marker, err)
	}
}

func assertMarkerAbsent(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("scale_check marker %s exists; suspended agent was probed", marker)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat scale_check marker %s: %v", marker, err)
	}
}

// TestBuildDesiredState_OnDemandNamedSession_NoPhantomPoolInstance verifies the
// ga-fiw fix: when work is assigned to a max_active_sessions=1 named-session
// agent (e.g. refinery), only ONE desired session entry exists — not the
// canonical named identity plus a phantom "{name}-1" pool sibling.
//
// Pre-fix bug: ComputePoolDesiredStates emitted a resume request for the
// named-session bead, which realizePoolDesiredSessions then renamed to
// "{name}-1" because claimPoolSlot returns 1 for beads without pool_slot
// metadata and poolInstanceName always appends a numeric suffix.
func TestBuildDesiredState_OnDemandNamedSession_NoPhantomPoolInstance(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:    "refinery work",
		Type:     "task",
		Status:   "open",
		Assignee: "refinery",
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "refinery",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	refineryEntries := []TemplateParams{}
	for _, tp := range dsResult.State {
		if tp.TemplateName == "refinery" {
			refineryEntries = append(refineryEntries, tp)
		}
	}
	if len(refineryEntries) != 1 {
		var names []string
		for _, tp := range refineryEntries {
			names = append(names, tp.SessionName)
		}
		t.Fatalf("refinery desired entries = %d, want 1 (no phantom pool sibling); got session_names %v", len(refineryEntries), names)
	}
	if got := refineryEntries[0].InstanceName; got == "refinery-1" || got == "test-city/refinery-1" {
		t.Errorf("desired refinery has phantom pool-instance identity %q (max_active_sessions=1 forbids -N suffix)", got)
	}
}

// TestRealizePoolDesiredSessions_NamedSessionBeadRefusedAsPoolInstance verifies
// the defense-in-depth in realizePoolDesiredSessions: even if a pool resume
// request slips through with a SessionBeadID that points to a named-session
// bead, the bead is NOT materialized as a pool instance. ComputePoolDesiredStates
// is supposed to filter these out, but the defense layer guards against a
// future regression that would re-introduce the phantom "{name}-1" sibling.
func TestRealizePoolDesiredSessions_NamedSessionBeadRefusedAsPoolInstance(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
	}
	namedBead := beads.Bead{
		ID:     "sess-refinery",
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               "refinery",
			"template":                   "refinery",
			"agent_name":                 "refinery",
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "refinery",
			namedSessionModeMetadata:     "on_demand",
		},
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), nil, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot([]beads.Bead{namedBead})

	poolState := PoolDesiredState{
		Template: "refinery",
		Requests: []SessionRequest{{
			Template:      "refinery",
			Tier:          "resume",
			SessionBeadID: namedBead.ID,
			WorkBeadID:    "w1",
		}},
	}
	desired := map[string]TemplateParams{}
	realizePoolDesiredSessions(bp, &cfg.Agents[0], poolState, desired, &stderr)

	if len(desired) != 0 {
		t.Fatalf("desired entries = %d, want 0 (named-session bead must not become a pool instance); got %v", len(desired), desired)
	}
	if !strings.Contains(stderr.String(), "refusing to materialize named-session bead") {
		t.Errorf("expected defense-in-depth warning, got: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), namedBead.ID) {
		t.Errorf("expected warning to mention bead %q, got: %q", namedBead.ID, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"refinery"-N sibling`) {
		t.Errorf("expected warning to describe phantom sibling, got: %q", stderr.String())
	}
}

func TestBuildDesiredState_OnDemandNamedSession_InProgressAssigneeMaterializes(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	// Create an in-progress bead assigned to the named session.
	b, err := store.Create(beads.Bead{
		Title:    "in-progress mayor work",
		Type:     "task",
		Status:   "open",
		Assignee: "mayor",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Transition to in_progress.
	inProgress := "in_progress"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	found := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("in-progress assignee should materialize on-demand named session")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_AssigneeDemandSignalsPoolDesired(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "assigned mayor work",
		Type:     "task",
		Status:   "open",
		Assignee: "mayor",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if !dsResult.NamedSessionDemand["mayor"] {
		t.Fatal("NamedSessionDemand should include 'mayor' when assignee-only demand exists")
	}
}

func TestMergeNamedSessionDemand_NilPoolDesiredNoPanic(t *testing.T) {
	// PoolDesiredCounts returns nil when there are no pool states. Verify
	// that mergeNamedSessionDemand handles this without panic.
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}
	demand := map[string]bool{"mayor": true}
	// Should not panic — callers now ensure poolDesired is non-nil,
	// but verify the function itself handles nil gracefully.
	poolDesired := make(map[string]int)
	mergeNamedSessionDemand(poolDesired, demand, cfg)
	if poolDesired["mayor"] != 1 {
		t.Fatalf("poolDesired[mayor] = %d, want 1", poolDesired["mayor"])
	}
}

func TestBuildDesiredState_PlainTemplateMaxOneDoesNotMaterializeWithoutDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "echo 0",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if len(dsResult.State) != 0 {
		t.Fatalf("plain max=1 template should not auto-materialize without demand: %+v", dsResult.State)
	}
}

func TestBuildDesiredState_PlainTemplateMaxOneScaleCheckCreatesEphemeralDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "echo 1",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if len(dsResult.State) != 1 {
		t.Fatalf("desired session count = %d, want 1", len(dsResult.State))
	}
	for _, tp := range dsResult.State {
		if tp.TemplateName != "worker" {
			t.Fatalf("TemplateName = %q, want worker", tp.TemplateName)
		}
		if tp.ConfiguredNamedIdentity != "" {
			t.Fatalf("ConfiguredNamedIdentity = %q, want empty", tp.ConfiguredNamedIdentity)
		}
		if got := tp.Env["GC_SESSION_ORIGIN"]; got != "ephemeral" {
			t.Fatalf("GC_SESSION_ORIGIN = %q, want ephemeral", got)
		}
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ScaleCheckCreatesEphemeralDemandOnly(t *testing.T) {
	// Phase 1 treats scale_check as generic ephemeral demand only. It must not
	// materialize on-demand named identities without direct named continuity.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "dog",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "echo 2",
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	dogCount := 0
	for _, tp := range dsResult.State {
		if tp.TemplateName == "dog" {
			dogCount++
			if tp.ConfiguredNamedIdentity != "" {
				t.Fatalf("scale_check materialized configured named identity: %+v", tp)
			}
			if tp.ConfiguredNamedMode != "" {
				t.Fatalf("scale_check materialized configured named mode: %+v", tp)
			}
		}
	}
	if dogCount != 2 {
		t.Fatalf("dog ephemeral desired count = %d, want 2", dogCount)
	}
	if dsResult.NamedSessionDemand["dog"] {
		t.Fatal("NamedSessionDemand should not include 'dog' from scale_check alone")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ScaleCheckZeroDoesNotMaterialize(t *testing.T) {
	// When scale_check returns 0 and work_query returns nothing, the
	// on-demand named session should NOT materialize.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "dog",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "echo 0",
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "dog" {
			t.Fatalf("scale_check=0 should not materialize on-demand named session: %+v", tp)
		}
	}
	if dsResult.ScaleCheckCounts["dog"] != 0 {
		t.Fatalf("ScaleCheckCounts[dog] = %d, want 0", dsResult.ScaleCheckCounts["dog"])
	}
}

func TestBuildDesiredState_OnDemandNamedSession_NoExplicitScaleCheckUsesWorkQuery(t *testing.T) {
	// work_query is session-local introspection in Phase 1 and must not drive
	// controller-side named materialization.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         `echo '["ready"]'`,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			t.Fatalf("work_query should not materialize on-demand named session: %+v", tp)
		}
	}
	if dsResult.NamedSessionDemand["mayor"] {
		t.Fatal("NamedSessionDemand should not include mayor from work_query")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ScaleCheckCreatesEphemeralSessions(t *testing.T) {
	// A named-session agent with scale_check should create generic ephemeral
	// capacity only, not the configured named session.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "dog",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "echo 3",
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	dogCount := 0
	for _, tp := range dsResult.State {
		if tp.TemplateName == "dog" {
			dogCount++
			if tp.ConfiguredNamedIdentity != "" {
				t.Fatalf("scale_check materialized configured named identity: %+v", tp)
			}
		}
	}
	if dogCount != 3 {
		t.Fatalf("expected 3 ephemeral sessions for dog from scale_check, got %d", dogCount)
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ScaleCheckErrorDoesNotFallToWorkQuery(t *testing.T) {
	// Controller-side work_query is no longer a named-session materialization
	// signal, even when scale_check fails.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "dog",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "exit 1",
			WorkQuery:         `echo '["ready"]'`,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "dog" {
			t.Fatalf("on-demand named session materialized from work_query fallback after scale_check error: %+v", tp)
		}
	}
	if dsResult.NamedSessionDemand["dog"] {
		t.Fatal("NamedSessionDemand should not include 'dog' via work_query fallback")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ScaleCheckNonIntegerDoesNotFallToWorkQuery(t *testing.T) {
	// A malformed scale_check must not re-enable controller-side work_query
	// materialization for named sessions.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "dog",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        `echo "ready"`,
			WorkQuery:         `echo '["ready"]'`,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "dog" {
			t.Fatalf("on-demand named session materialized from work_query fallback after scale_check parse error: %+v", tp)
		}
	}
	if dsResult.NamedSessionDemand["dog"] {
		t.Fatal("NamedSessionDemand should not include 'dog' via work_query fallback after parse error")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_RigWorkQueryDoesNotMaterialize(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "demo")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCanonicalConfig(t, cityPath, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeRigEndpointCanonicalConfig(t, rigPath, contract.ConfigState{
		IssuePrefix:    "dm",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "3308",
		DoltUser:       "rig-user",
	})
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name:   "demo",
			Path:   rigPath,
			Prefix: "dm",
		}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "demo",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         `sh -c 'test "$BEADS_DOLT_PASSWORD" = "rig-secret" && printf "[{\"id\":\"DM-1\"}]"'`,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Dir:      "demo",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	found := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "demo/worker" {
			found = true
			break
		}
	}
	if found {
		t.Fatal("on-demand rig named session materialized from controller-side work_query")
	}
}

func TestBuildDesiredState_SingletonTemplateDoesNotRealizeDependencyPoolFloorWithoutSession(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:      "api",
				DependsOn: []string{"db"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 0 {
		t.Fatalf("db desired slots = %d, want 0 without a realized dependent session", dbSlots)
	}
}

func TestBuildDesiredState_DoesNotRealizeDependencyFloorForZeroScaledDependentPool(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
				DependsOn: []string{"db"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			t.Fatalf("unexpected dependency-only db slot for zero-scaled dependent pool: %+v", tp)
		}
	}
}

func TestBuildDesiredState_DoesNotRealizeDependencyFloorForSuspendedDependent(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:      "api",
				Suspended: true,
				DependsOn: []string{"db"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			t.Fatalf("unexpected dependency-only db slot for suspended dependent: %+v", tp)
		}
	}
}

func TestBuildDesiredState_SingletonTemplatesDoNotRealizeTransitiveDependencyPoolFloorWithoutSession(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
				DependsOn: []string{"db"},
			},
			{
				Name:      "web",
				DependsOn: []string{"api"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
	apiSlots := 0
	dbSlots := 0
	for _, tp := range desired {
		switch tp.TemplateName {
		case "api":
			apiSlots++
		case "db":
			dbSlots++
		}
	}
	if apiSlots != 0 {
		t.Fatalf("api desired slots = %d, want 0 without a realized root session", apiSlots)
	}
	if dbSlots != 0 {
		t.Fatalf("db desired slots = %d, want 0 without a realized root session", dbSlots)
	}
}

func TestBuildDesiredState_DiscoveredSessionRootGetsDependencyPoolFloor(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "s-gc-100",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "helper",
				Suspended:         true,
				MaxActiveSessions: intPtr(1),
				DependsOn:         []string{"db"},
				StartCommand:      "echo",
			},
		},
	}

	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	desired := dsResult.State
	if _, ok := desired["s-gc-100"]; !ok {
		t.Fatalf("expected discovered helper session in desired state, got keys %v", desired)
	}
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 1 {
		t.Fatalf("db desired slots = %d, want 1; stderr=%s", dbSlots, stderr.String())
	}
}

func TestBuildDesiredState_ManualZeroScaledPoolSessionStaysDesiredAndKeepsDependencyFloor(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "debug api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:api"},
		Metadata: map[string]string{
			"template":       "api",
			"session_name":   "s-gc-200",
			"state":          "creating",
			"session_origin": "manual",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				DependsOn:         []string{"db"},
				StartCommand:      "echo",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
		},
	}

	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	desired := dsResult.State
	if _, ok := desired["s-gc-200"]; !ok {
		t.Fatalf("expected manual pool session in desired state, got keys %v", desired)
	}
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 1 {
		t.Fatalf("db desired slots = %d, want 1; stderr=%s", dbSlots, stderr.String())
	}
}

func TestRefreshDesiredStateWithSessionBeadsIncludesManualCreatedDuringBuild(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	staleSnapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load stale snapshot: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "debug api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:api"},
		Metadata: map[string]string{
			"template":       "api",
			"session_name":   "s-gc-late",
			"state":          "creating",
			"manual_session": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "api",
			StartCommand:      "echo",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(0),
		}},
	}

	result := buildDesiredStateWithSessionBeads("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, nil, staleSnapshot, nil, io.Discard)
	if _, ok := result.State["s-gc-late"]; ok {
		t.Fatalf("stale session snapshot unexpectedly included late manual session")
	}
	latestSnapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load latest snapshot: %v", err)
	}
	refreshed := refreshDesiredStateWithSessionBeads(result, "test-city", cityPath, cfg, runtime.NewFake(), store, latestSnapshot, io.Discard)
	tp, ok := refreshed.State["s-gc-late"]
	if !ok {
		t.Fatalf("expected refreshed desired state to include late manual session, got keys %v", mapKeys(refreshed.State))
	}
	if !tp.ManualSession {
		t.Fatalf("refreshed manual session flag = false, want true")
	}
}

func TestBuildDesiredState_ManualImplicitPoolSessionsStayDesired(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "prompts", "worker.md"), []byte("worker prompt"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	for _, bead := range []beads.Bead{
		{
			Title:  "helper",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "template:helper"},
			Metadata: map[string]string{
				"template":             "helper",
				"session_name":         "s-real-world-app-4wq",
				"state":                "creating",
				"manual_session":       "true",
				"pending_create_claim": "true",
			},
		},
		{
			Title:  "hal",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "template:helper"},
			Metadata: map[string]string{
				"template":             "helper",
				"session_name":         "s-real-world-app-bmr",
				"alias":                "hal",
				"state":                "suspended",
				"manual_session":       "true",
				"pending_create_claim": "true",
			},
		},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.City{
		Workspace: config.Workspace{
			Name:     "my-city",
			Provider: "claude",
		},
		Providers: map[string]config.ProviderSpec{
			"claude": {
				Command:    "echo",
				PromptMode: "arg",
			},
		},
		Agents: []config.Agent{
			{
				Name:           "mayor",
				PromptTemplate: "prompts/mayor.md",
			},
			{
				Name:           "helper",
				PromptTemplate: "prompts/worker.md",
			},
		},
	}

	dsResult := buildDesiredState("my-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	for _, sn := range []string{"s-real-world-app-4wq", "s-real-world-app-bmr"} {
		tp, ok := desired[sn]
		if !ok {
			t.Fatalf("expected manual helper session %q in desired state, got keys %v", sn, mapKeys(desired))
		}
		if tp.TemplateName != "helper" {
			t.Fatalf("desired[%q].TemplateName = %q, want helper", sn, tp.TemplateName)
		}
		if !tp.ManualSession {
			t.Fatalf("desired[%q].ManualSession = false, want true", sn)
		}
	}
	if got := desired["s-real-world-app-bmr"].Alias; got != "hal" {
		t.Fatalf("desired[s-real-world-app-bmr].Alias = %q, want hal", got)
	}
}

func TestBuildDesiredState_ScaleCheckErrorRetainsOnlyAffectedPoolSessions(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	workerSession := beads.Bead{
		ID:     "session-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "awake",
		},
	}
	helperSession := beads.Bead{
		ID:     "session-helper",
		Title:  "helper",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":         "helper-bd-123",
			"template":             "helper",
			"agent_name":           "helper",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "awake",
		},
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				StartCommand:      "echo",
				ScaleCheck:        "exit 42",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
			},
			{
				Name:              "helper",
				StartCommand:      "echo",
				ScaleCheck:        "printf 0",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
			},
		},
	}

	var stderr strings.Builder
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		cityPath,
		time.Now().UTC(),
		cfg,
		runtime.NewFake(),
		store,
		nil,
		newSessionBeadSnapshot([]beads.Bead{workerSession, helperSession}),
		nil,
		&stderr,
	)

	if result.StoreQueryPartial {
		t.Fatalf("StoreQueryPartial = true, want false for scoped scale_check failure; stderr=%s", stderr.String())
	}
	if !result.ScaleCheckPartialTemplates["worker"] {
		t.Fatalf("ScaleCheckPartialTemplates[worker] = false, want true; templates=%v stderr=%s", result.ScaleCheckPartialTemplates, stderr.String())
	}
	if !result.PoolScaleCheckPartialTemplates["worker"] {
		t.Fatalf("PoolScaleCheckPartialTemplates[worker] = false, want true; templates=%v", result.PoolScaleCheckPartialTemplates)
	}
	if result.ScaleCheckPartialTemplates["helper"] {
		t.Fatalf("ScaleCheckPartialTemplates[helper] = true, want false; templates=%v", result.ScaleCheckPartialTemplates)
	}
	if _, ok := result.State["worker-bd-123"]; !ok {
		t.Fatalf("affected worker session not retained in desired state: keys=%v stderr=%s", mapKeys(result.State), stderr.String())
	}
	if _, ok := result.State["helper-bd-123"]; ok {
		t.Fatalf("unaffected helper session retained despite clean zero demand: keys=%v", mapKeys(result.State))
	}
	if got := result.ScaleCheckCounts["worker"]; got != 0 {
		t.Fatalf("ScaleCheckCounts[worker] = %d, want 0 on failed new-demand probe", got)
	}
}

func TestBuildDesiredState_ScaleCheckErrorPreservesDormantAffectedPoolSessionWithoutWakeDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	workerSession := beads.Bead{
		ID:     "session-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "asleep",
		},
	}
	helperSession := beads.Bead{
		ID:     "session-helper",
		Title:  "helper",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":         "helper-bd-123",
			"template":             "helper",
			"agent_name":           "helper",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "asleep",
		},
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				StartCommand:      "echo",
				ScaleCheck:        "exit 42",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
			},
			{
				Name:              "helper",
				StartCommand:      "echo",
				ScaleCheck:        "printf 0",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
			},
		},
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{workerSession, helperSession})

	var stderr strings.Builder
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		cityPath,
		time.Now().UTC(),
		cfg,
		runtime.NewFake(),
		store,
		nil,
		snapshot,
		nil,
		&stderr,
	)

	if result.StoreQueryPartial {
		t.Fatalf("StoreQueryPartial = true, want false for scoped scale_check failure; stderr=%s", stderr.String())
	}
	if _, ok := result.State["worker-bd-123"]; !ok {
		t.Fatalf("dormant affected worker session not preserved in desired state: keys=%v stderr=%s", mapKeys(result.State), stderr.String())
	}
	if _, ok := result.State["helper-bd-123"]; ok {
		t.Fatalf("unaffected dormant helper session retained despite clean zero demand: keys=%v", mapKeys(result.State))
	}

	poolDesired := retainScaleCheckPartialPoolDesired(
		cfg,
		PoolDesiredCounts(ComputePoolDesiredStates(cfg, nil, snapshot.OpenInfos(), result.ScaleCheckCounts)),
		snapshot,
		result.PoolScaleCheckPartialTemplates,
	)
	if got := poolDesired["worker"]; got != 0 {
		t.Fatalf("poolDesired[worker] = %d, want dormant preservation without wake demand", got)
	}
}

func TestBuildDesiredState_NamedBackedPoolPartialRetainsGenericPoolSession(t *testing.T) {
	cityPath := t.TempDir()
	store := &controllerDemandPartialStore{MemStore: beads.NewMemStore()}
	poolSession := beads.Bead{
		ID:     "session-worker-pool",
		Title:  "worker pool",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "awake",
		},
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "echo",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
		}},
		NamedSessions: []config.NamedSession{{
			Name:     "primary",
			Template: "worker",
			Mode:     "on_demand",
		}},
	}

	var stderr strings.Builder
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		cityPath,
		time.Now().UTC(),
		cfg,
		runtime.NewFake(),
		store,
		nil,
		newSessionBeadSnapshot([]beads.Bead{poolSession}),
		nil,
		&stderr,
	)

	// The demand phase now shares one full ready read per store across the
	// assigned-work, scale-check, and named-session probes (readyDemandCache),
	// so a partial ready read is reported uniformly: the assigned-work
	// collection is marked partial too, suppressing drains conservatively
	// (over-retention, never a demand under-count). Before the shared snapshot
	// the assigned-work pass issued its own *limited* ready read, which this
	// synthetic store — which returns partial only for unlimited reads — treated
	// as clean. The scoped template partials below still fire, so the
	// generic-pool session is retained either way.
	if !result.StoreQueryPartial {
		t.Fatalf("StoreQueryPartial = false, want true once the shared ready snapshot is partial; stderr=%s", stderr.String())
	}
	if !result.ScaleCheckPartialTemplates["worker"] {
		t.Fatalf("ScaleCheckPartialTemplates[worker] = false, want named-session partial recorded; templates=%v stderr=%s", result.ScaleCheckPartialTemplates, stderr.String())
	}
	if !result.PoolScaleCheckPartialTemplates["worker"] {
		t.Fatalf("PoolScaleCheckPartialTemplates[worker] = false, want true for template pool partial; templates=%v", result.PoolScaleCheckPartialTemplates)
	}
	if !result.NamedScaleCheckPartialTemplates["worker"] {
		t.Fatalf("NamedScaleCheckPartialTemplates[worker] = false, want true; templates=%v", result.NamedScaleCheckPartialTemplates)
	}
	if _, ok := result.State["worker-bd-123"]; !ok {
		t.Fatalf("generic pool session was not retained by template pool partial: keys=%v stderr=%s", mapKeys(result.State), stderr.String())
	}
}

func TestBuildDesiredState_DrainedPoolManagedSessionIsNotRediscovered(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:claude"},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "s-gc-drained",
			"state":        "asleep",
			"sleep_reason": "drained",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "claude",
			StartCommand:      "true",
			MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5),
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State

	if _, ok := desired["s-gc-drained"]; ok {
		t.Fatalf("drained pool-managed session should not be rediscovered into desired state")
	}

	claudeSessions := 0
	for _, tp := range desired {
		if tp.TemplateName == "claude" {
			claudeSessions++
		}
	}
	if claudeSessions != 1 {
		t.Fatalf("claude desired sessions = %d, want 1", claudeSessions)
	}
}

func TestBuildDesiredState_LegacyNamepoolPoolSessionWithoutMetadataDoesNotBypassScaleCheck(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:furiosa"},
		Metadata: map[string]string{
			"template":     "worker",
			"agent_name":   "furiosa",
			"session_name": "worker-live",
			"state":        "active",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
			NamepoolNames:     []string{"furiosa", "nux"},
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State

	if _, ok := desired["worker-live"]; ok {
		t.Fatalf("legacy themed pool session should not be rediscovered when scale_check demand is 0")
	}
}

func TestBuildDesiredState_UsesBeadNamedPoolSessionsForScaleCheckDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title: "queued worker job",
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Demand is supplied by the explicit scale_check here. This test only
	// verifies that pool sessions created under demand use bead-derived names
	// and pool-managed metadata, not that routed work itself increments demand.
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 1",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if len(desired) != 1 {
		t.Fatalf("desired sessions = %d, want 1", len(desired))
	}

	var (
		sessionName string
		tp          TemplateParams
	)
	for sn, got := range desired {
		sessionName = sn
		tp = got
	}
	if tp.TemplateName != "worker" {
		t.Fatalf("TemplateName = %q, want worker", tp.TemplateName)
	}
	if !strings.HasPrefix(sessionName, "worker-") {
		t.Fatalf("session name = %q, want worker-<beadID>", sessionName)
	}
	if strings.HasSuffix(sessionName, "-1") {
		t.Fatalf("session name = %q, want bead-derived name instead of slot alias", sessionName)
	}

	sessionBeads, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(sessionBeads))
	}
	if got := sessionBeads[0].Metadata["session_name"]; got != sessionName {
		t.Fatalf("stored session_name = %q, want %q", got, sessionName)
	}
	if got := sessionBeads[0].Metadata[poolManagedMetadataKey]; got != "true" {
		t.Fatalf("pool_managed = %q, want true", got)
	}
}

func TestBuildDesiredState_PoolSessionCoreFingerprintStableAcrossTicks(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "polecat",
			Dir:               "gascity",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "printf 1",
		}},
	}

	first := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	var (
		sessionName string
		firstTP     TemplateParams
	)
	for sn, tp := range first.State {
		if tp.TemplateName == "gascity/polecat" {
			sessionName = sn
			firstTP = tp
			break
		}
	}
	if sessionName == "" {
		t.Fatalf("first desired state missing gascity/polecat session: %#v", first.State)
	}
	startedHash := runtime.CoreFingerprint(templateParamsToConfig(firstTP))

	second := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	secondTP, ok := second.State[sessionName]
	if !ok {
		t.Fatalf("second desired state missing existing session %q: %#v", sessionName, second.State)
	}
	currentHash := runtime.CoreFingerprint(templateParamsToConfig(secondTP))
	if currentHash != startedHash {
		t.Fatalf("pool session core fingerprint changed across desired-state ticks: first=%s second=%s first_alias=%q second_alias=%q",
			startedHash, currentHash, firstTP.Env["GC_ALIAS"], secondTP.Env["GC_ALIAS"])
	}
}

func TestBuildDesiredState_FallsBackToLegacyPoolDemandWhenListFails(t *testing.T) {
	cityPath := t.TempDir()
	memStore := beads.NewMemStore()
	store := listFailStore{Store: memStore}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				StartCommand:      "true",
				MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1),
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	// With min=1, max=1: both the singleton path and the pool-floor path
	// may contribute a session, yielding 1 or 2 desired entries depending
	// on timing. Accept either.
	if len(desired) < 1 || len(desired) > 2 {
		t.Fatalf("desired sessions = %d, want 1 or 2", len(desired))
	}
	// At least one session should have a worker-prefixed name.
	found := false
	for sn := range desired {
		if strings.HasPrefix(sn, "worker") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no worker-prefixed session in desired: %v", desired)
	}
}

func TestBuildDesiredState_DependencyFloorDoesNotReuseRegularPoolWorkerBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker active",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template":             "worker",
			"session_name":         "worker-existing",
			"agent_name":           "worker",
			"state":                "active",
			"pool_slot":            "1",
			poolManagedMetadataKey: "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "helper-session",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
			},
			{
				Name:         "helper",
				Suspended:    true,
				DependsOn:    []string{"worker"},
				StartCommand: "echo",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["worker-existing"]; ok {
		t.Fatalf("dependency floor reused regular worker bead: keys=%v", mapKeys(desired))
	}
	workerSessions := 0
	for sn, tp := range desired {
		if tp.TemplateName != "worker" {
			continue
		}
		workerSessions++
		if sn == "worker-existing" {
			t.Fatalf("dependency floor kept regular worker bead %q desired", sn)
		}
	}
	if workerSessions != 1 {
		t.Fatalf("worker desired sessions = %d, want 1; desired keys=%v", workerSessions, mapKeys(desired))
	}
}

func TestBuildDesiredState_StoreBackedPoolUsesLogicalInstanceIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(2),
				ScaleCheck:        "printf 2",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if len(dsResult.State) != 2 {
		t.Fatalf("desired session count = %d, want 2", len(dsResult.State))
	}

	want := map[string]int{"worker-1": 1, "worker-2": 2}
	for _, tp := range dsResult.State {
		slot, ok := want[tp.InstanceName]
		if !ok {
			t.Fatalf("unexpected instance name %q in desired state", tp.InstanceName)
		}
		if tp.TemplateName != "worker" {
			t.Fatalf("TemplateName = %q, want worker", tp.TemplateName)
		}
		if tp.PoolSlot != slot {
			t.Fatalf("PoolSlot(%q) = %d, want %d", tp.InstanceName, tp.PoolSlot, slot)
		}
		if tp.Alias != tp.InstanceName {
			t.Fatalf("Alias(%q) = %q, want %q", tp.InstanceName, tp.Alias, tp.InstanceName)
		}
		if got := tp.Env["GC_AGENT"]; got != tp.InstanceName {
			t.Fatalf("GC_AGENT(%q) = %q, want %q", tp.InstanceName, got, tp.InstanceName)
		}
		if got := tp.Env["GC_ALIAS"]; got != tp.InstanceName {
			t.Fatalf("GC_ALIAS(%q) = %q, want %q", tp.InstanceName, got, tp.InstanceName)
		}
		delete(want, tp.InstanceName)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected instance identities: %v", want)
	}
}

func TestBuildDesiredState_StoreBackedPoolUsesQualifiedInstanceNameForBindings(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "ops worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:ops.worker"},
		Metadata: map[string]string{
			"template":     "ops.worker",
			"session_name": "ops-worker-1",
			"agent_name":   "ops.worker",
			"state":        "active",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			BindingName:       "ops",
			StartCommand:      "true",
			WorkDir:           ".gc/worktrees/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
			ScaleCheck:        "printf 1",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	var got TemplateParams
	found := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "ops.worker" {
			got = tp
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("desired state missing binding-qualified pool session: keys=%v", mapKeys(dsResult.State))
	}

	wantInstance := cfg.Agents[0].QualifiedInstanceName("worker-1")
	if got.InstanceName != wantInstance {
		t.Fatalf("InstanceName = %q, want %q", got.InstanceName, wantInstance)
	}
	if got.Alias != wantInstance {
		t.Fatalf("Alias = %q, want %q", got.Alias, wantInstance)
	}
	if got.Env["GC_AGENT"] != wantInstance {
		t.Fatalf("GC_AGENT = %q, want %q", got.Env["GC_AGENT"], wantInstance)
	}
	wantWorkDir := filepath.Join(cityPath, ".gc", "worktrees", "ops.worker-1")
	if got.WorkDir != wantWorkDir {
		t.Fatalf("WorkDir = %q, want %q", got.WorkDir, wantWorkDir)
	}
}

func TestBuildDesiredState_RecoversPoolTemplateFromAliasOnlyBindingIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "ops furiosa",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "ops-furiosa-session",
			"alias":        "frontend/ops.furiosa",
			"pool_slot":    "1",
			"pool_managed": "true",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:          "worker",
			Dir:           "frontend",
			BindingName:   "ops",
			StartCommand:  "true",
			NamepoolNames: []string{"furiosa", "nux"},
			WorkDir:       ".gc/worktrees/{{.AgentBase}}",
			ScaleCheck:    "printf 1",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	got, ok := dsResult.State["ops-furiosa-session"]
	if !ok {
		t.Fatalf("desired state missing alias-only pool session: keys=%v", mapKeys(dsResult.State))
	}
	if got.TemplateName != "frontend/ops.worker" {
		t.Fatalf("TemplateName = %q, want %q", got.TemplateName, "frontend/ops.worker")
	}
	wantInstance := cfg.Agents[0].QualifiedInstanceName("furiosa")
	if got.InstanceName != wantInstance {
		t.Fatalf("InstanceName = %q, want %q", got.InstanceName, wantInstance)
	}
	if got.Alias != wantInstance {
		t.Fatalf("Alias = %q, want %q", got.Alias, wantInstance)
	}
	if got.Env["GC_AGENT"] != wantInstance {
		t.Fatalf("GC_AGENT = %q, want %q", got.Env["GC_AGENT"], wantInstance)
	}
	wantWorkDir := filepath.Join(cityPath, ".gc", "worktrees", "ops.furiosa")
	if got.WorkDir != wantWorkDir {
		t.Fatalf("WorkDir = %q, want %q", got.WorkDir, wantWorkDir)
	}
}

func TestBuildDesiredState_PendingCreatePoolSessionUsesConcreteBeadIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	workDir := filepath.Join(cityPath, ".gc", "worktrees", "demo", "ants", "ant-adhoc-abc123")
	if _, err := store.Create(beads.Bead{
		Title:  "adhoc ant",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:demo/ant"},
		Metadata: map[string]string{
			"template":              "demo/ant",
			"session_name":          "ant-adhoc-abc123",
			"session_name_explicit": boolMetadata(true),
			"agent_name":            "demo/ant-adhoc-abc123",
			"session_origin":        "manual",
			"pending_create_claim":  boolMetadata(true),
			"state":                 "creating",
			"work_dir":              workDir,
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{{
			Name:              "ant",
			Dir:               "demo",
			Provider:          "test-agent",
			StartCommand:      "true",
			WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(4),
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	got, ok := dsResult.State["ant-adhoc-abc123"]
	if !ok {
		t.Fatalf("desired state missing pending create session: keys=%v", mapKeys(dsResult.State))
	}
	if got.TemplateName != "demo/ant" {
		t.Fatalf("TemplateName = %q, want %q", got.TemplateName, "demo/ant")
	}
	if got.InstanceName != "demo/ant-adhoc-abc123" {
		t.Fatalf("InstanceName = %q, want %q", got.InstanceName, "demo/ant-adhoc-abc123")
	}
	if got.WorkDir != workDir {
		t.Fatalf("WorkDir = %q, want %q", got.WorkDir, workDir)
	}
	if got.Env["GC_ALIAS"] != "demo/ant-adhoc-abc123" {
		t.Fatalf("GC_ALIAS = %q, want %q", got.Env["GC_ALIAS"], "demo/ant-adhoc-abc123")
	}
}

func TestBuildDesiredState_PendingCreatePoolSessionDropsWithoutScaleDemand(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repos", "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("create rig path: %v", err)
	}
	store := beads.NewMemStore()
	sessionName := "workflows__codex-max-mc-new"
	if _, err := store.Create(beads.Bead{
		Title:  "codex-max",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:gascity/workflows.codex-max-1"},
		Metadata: map[string]string{
			"template":             "gascity/workflows.codex-max",
			"session_name":         sessionName,
			"agent_name":           "gascity/workflows.codex-max-1",
			"session_origin":       "ephemeral",
			"pool_managed":         boolMetadata(true),
			"pool_slot":            "1",
			"pending_create_claim": boolMetadata(true),
			"state":                "stopped",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "gascity", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "workflows.codex-max",
			Dir:               "gascity",
			Provider:          "test-agent",
			StartCommand:      "true",
			WorkDir:           ".",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
			ScaleCheck:        "printf 0",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if got := dsResult.ScaleCheckCounts["gascity/workflows.codex-max"]; got != 0 {
		t.Fatalf("ScaleCheckCounts[gascity/workflows.codex-max] = %d, want 0", got)
	}
	if _, ok := dsResult.State[sessionName]; ok {
		t.Fatalf("pending-create pool session stayed desired without runnable work: keys=%v base=%v", mapKeys(dsResult.State), mapKeys(dsResult.BaseState))
	}
}

func TestBuildDesiredState_PendingCreatePoolSessionCountsTowardScaleDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	const template = "worker"
	sessionName := "worker-mc-starting"
	for i := 0; i < 2; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("queued work %d", i+1),
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": template,
			},
		}); err != nil {
			t.Fatalf("create queued work: %v", err)
		}
	}
	if _, err := store.Create(beads.Bead{
		Title:  template,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker-1"},
		Metadata: map[string]string{
			"template":                  template,
			"session_name":              sessionName,
			"agent_name":                "worker-1",
			"session_origin":            "ephemeral",
			"pool_managed":              boolMetadata(true),
			"pool_slot":                 "1",
			"pending_create_claim":      boolMetadata(true),
			"pending_create_started_at": time.Now().UTC().Format(time.RFC3339),
			"state":                     "creating",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              template,
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
		}},
	}
	sessionSnapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}

	trace := newPoolDesiredStateTestTrace(template)
	var stderr strings.Builder
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, sessionSnapshot, trace, &stderr,
	)
	if got := dsResult.ScaleCheckCounts[template]; got != 2 {
		t.Fatalf("ScaleCheckCounts[%s] = %d, want 2", template, got)
	}
	// The trace pins the buildDesiredState integration point: the pending
	// create consumes one scale-demand slot before anonymous new requests are
	// materialized.
	if got := trace.decisionCounts[string(TraceSitePoolInFlightReuse)]; got != 1 {
		t.Fatalf("in-flight reuse trace decisions = %d, want 1; stderr:\n%s", got, stderr.String())
	}
	rec := poolTraceDecision(t, trace, TraceSitePoolInFlightReuse)
	for key, want := range map[string]int{
		"scale_check":   2,
		"in_flight":     1,
		"reused":        1,
		"anonymous_new": 1,
	} {
		if got := poolTraceFieldInt(t, rec.Fields, key); got != want {
			t.Fatalf("%s = %d, want %d", key, got, want)
		}
	}

	var templateCount int
	existing, ok := dsResult.State[sessionName]
	if !ok {
		t.Fatalf("desired state missing pending-create pool session: keys=%v", mapKeys(dsResult.State))
	}
	for _, tp := range dsResult.State {
		if tp.TemplateName == template {
			templateCount++
		}
	}
	if templateCount != 2 {
		t.Fatalf("desired %s sessions = %d, want 2; keys=%v", template, templateCount, mapKeys(dsResult.State))
	}
	var anonymousNew *TemplateParams
	for name, tp := range dsResult.State {
		if tp.TemplateName == template && name != sessionName {
			tpCopy := tp
			anonymousNew = &tpCopy
			break
		}
	}
	if anonymousNew == nil {
		t.Fatalf("desired state missing anonymous new pool session: keys=%v", mapKeys(dsResult.State))
	}
	if existing.InstanceName != "worker-1" {
		t.Fatalf("existing InstanceName = %q, want worker-1", existing.InstanceName)
	}
	if existing.PoolSlot != 1 {
		t.Fatalf("existing PoolSlot = %d, want 1", existing.PoolSlot)
	}
	if anonymousNew.PoolSlot != 2 {
		t.Fatalf("anonymous new PoolSlot = %d, want 2", anonymousNew.PoolSlot)
	}
}

func TestBuildDesiredState_LegacyAliaslessEphemeralPoolSessionFallsBackToSessionNameIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "legacy ant",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:demo/ant"},
		Metadata: map[string]string{
			"template":       "demo/ant",
			"agent_name":     "demo/ant",
			"session_name":   "s-gc-legacy",
			"session_origin": "ephemeral",
			"state":          "creating",
			"work_dir":       filepath.Join(cityPath, ".gc", "worktrees", "demo", "ants", "ant"),
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{{
			Name:              "ant",
			Dir:               "demo",
			Provider:          "test-agent",
			StartCommand:      "true",
			WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(4),
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	got, ok := dsResult.State["s-gc-legacy"]
	if !ok {
		t.Fatalf("desired state missing legacy session: keys=%v", mapKeys(dsResult.State))
	}
	if got.InstanceName != "demo/s-gc-legacy" {
		t.Fatalf("InstanceName = %q, want %q", got.InstanceName, "demo/s-gc-legacy")
	}
	wantWorkDir := filepath.Join(cityPath, ".gc", "worktrees", "demo", "ants", "s-gc-legacy")
	if got.WorkDir != wantWorkDir {
		t.Fatalf("WorkDir = %q, want %q", got.WorkDir, wantWorkDir)
	}
}

func TestBuildDesiredState_RediscoveriesUniqueLegacyLocalPoolTemplate(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-5",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(5)},
			{Name: "worker", Dir: "backend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(1)},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	got, ok := dsResult.State["worker-5"]
	if !ok {
		t.Fatalf("desired state missing legacy local session: keys=%v", mapKeys(dsResult.State))
	}
	if got.TemplateName != "frontend/worker" {
		t.Fatalf("TemplateName = %q, want %q", got.TemplateName, "frontend/worker")
	}
}

func TestBuildDesiredState_DoesNotRediscoverAmbiguousLegacyLocalPoolTemplate(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-5",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(5)},
			{Name: "worker", Dir: "backend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(5)},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, ok := dsResult.State["worker-5"]; ok {
		t.Fatalf("desired state %#v unexpectedly rediscovered ambiguous local pool template", dsResult.State["worker-5"])
	}
}

func TestBuildDesiredState_RecoversPoolTemplateFromAgentNameOnlyLegacyLocalIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"agent_name":   "worker-5",
			"session_name": "worker-5",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(5)},
			{Name: "worker", Dir: "backend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(1)},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	got, ok := dsResult.State["worker-5"]
	if !ok {
		t.Fatalf("desired state missing agent_name-only legacy session: keys=%v", mapKeys(dsResult.State))
	}
	if got.TemplateName != "frontend/worker" {
		t.Fatalf("TemplateName = %q, want %q", got.TemplateName, "frontend/worker")
	}
}

func TestBuildDesiredState_DoesNotRecoverPoolTemplateFromAmbiguousLegacyLocalAlias(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"alias":        "worker-5",
			"session_name": "worker-5",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(5)},
			{Name: "worker", Dir: "backend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(5)},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, ok := dsResult.State["worker-5"]; ok {
		t.Fatalf("desired state %#v unexpectedly recovered ambiguous local alias identity", dsResult.State["worker-5"])
	}
}

func TestBuildDesiredState_RediscoveriesLegacyCommonNamePoolTemplate(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"common_name":  "worker",
			"session_name": "worker-5",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(5)},
			{Name: "worker", Dir: "backend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(1)},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	got, ok := dsResult.State["worker-5"]
	if !ok {
		t.Fatalf("desired state missing legacy common_name session: keys=%v", mapKeys(dsResult.State))
	}
	if got.TemplateName != "frontend/worker" {
		t.Fatalf("TemplateName = %q, want %q", got.TemplateName, "frontend/worker")
	}
}

func TestBuildDesiredState_DoesNotRediscoverFreshCreatingOutOfBoundsQualifiedPoolIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"agent_name":   "frontend/worker-7",
			"session_name": "custom-worker-7",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(5)},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, ok := dsResult.State["custom-worker-7"]; ok {
		t.Fatalf("desired state %#v unexpectedly kept fresh out-of-bounds qualified pool identity", dsResult.State["custom-worker-7"])
	}
}

func TestBuildDesiredState_DoesNotRediscoverZeroCapacityQualifiedPoolIdentity(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"agent_name":   "frontend/worker-1",
			"session_name": "custom-worker-1",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(0)},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, ok := dsResult.State["custom-worker-1"]; ok {
		t.Fatalf("desired state %#v unexpectedly kept zero-capacity qualified pool identity", dsResult.State["custom-worker-1"])
	}
}

func TestBuildDesiredState_DoesNotRediscoverStaleCreatingLegacyPoolTemplate(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"common_name":               "worker",
			"session_name":              "worker-7",
			"state":                     "creating",
			"pending_create_started_at": time.Now().Add(-staleCreatingStateTimeout - time.Minute).UTC().Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(5)},
			{Name: "worker", Dir: "backend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(1)},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, ok := dsResult.State["worker-7"]; ok {
		t.Fatalf("desired state %#v unexpectedly kept stale creating legacy pool bead", dsResult.State["worker-7"])
	}
}

func TestBuildDesiredState_DoesNotPreserveOutOfBoundsBoundedPoolSlotWithoutIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(5)},
		},
	}
	cfgAgent := &cfg.Agents[0]
	bead := beads.Bead{
		Metadata: map[string]string{
			"template":  "frontend/worker",
			"pool_slot": "99",
		},
	}

	if slot := existingPoolSlotWithConfig(cfg, cfgAgent, bead); slot != 0 {
		t.Fatalf("existingPoolSlotWithConfig(out-of-bounds bounded slot) = %d, want 0", slot)
	}
}

func TestBuildDesiredState_PrefersInBoundsPoolSlotOverOutOfBoundsAgentName(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(5)},
		},
	}
	cfgAgent := &cfg.Agents[0]
	bead := beads.Bead{
		Metadata: map[string]string{
			"template":   "frontend/worker",
			"pool_slot":  "2",
			"agent_name": "frontend/worker-99",
		},
	}

	if slot := existingPoolSlotWithConfig(cfg, cfgAgent, bead); slot != 2 {
		t.Fatalf("existingPoolSlotWithConfig(in-bounds pool_slot, out-of-bounds agent_name) = %d, want 2", slot)
	}
}

func TestBuildDesiredState_DoesNotRecoverOutOfBoundsAliasOnlyBoundedPoolSlot(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "frontend/worker",
			"alias":        "frontend/worker-7",
			"session_name": "custom-worker-7",
			"state":        "active",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Provider: "test-agent", StartCommand: "true", WorkDir: ".", MaxActiveSessions: intPtr(5)},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, ok := dsResult.State["custom-worker-7"]; ok {
		t.Fatalf("desired state %#v unexpectedly preserved out-of-bounds alias-only pool identity", dsResult.State["custom-worker-7"])
	}
}

func TestExistingPoolSlot_PreservesStampedOutOfBoundsLiveIdentity(t *testing.T) {
	cfgAgent := &config.Agent{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(5)}
	bead := beads.Bead{
		Metadata: map[string]string{
			"pool_slot":  "7",
			"agent_name": "frontend/worker-7",
			"alias":      "frontend/worker-7",
		},
	}

	if slot := existingPoolSlot(cfgAgent, bead); slot != 7 {
		t.Fatalf("existingPoolSlot(stamped live slot) = %d, want 7", slot)
	}
}

func TestValidateAgentSessionTransportForBuild_ProductionShapeRunsTransportValidation(t *testing.T) {
	bp := &agentBuildParams{
		workspace: &config.Workspace{},
		providers: map[string]config.ProviderSpec{
			"test-agent": {
				Command:     "test-agent",
				SupportsACP: boolPtr(true),
			},
		},
		lookPath: func(string) (string, error) {
			return "/usr/bin/test-agent", nil
		},
		sp: runtime.NewFake(),
	}
	cfgAgent := &config.Agent{Name: "worker", Provider: "test-agent", Session: config.SessionTransportACP}

	err := validateAgentSessionTransportForBuild(bp, cfgAgent, cfgAgent.QualifiedName())
	if err == nil {
		t.Fatal("validateAgentSessionTransportForBuild returned nil, want transport routing error")
	}
	if !strings.Contains(err.Error(), "requires ACP transport") {
		t.Fatalf("validateAgentSessionTransportForBuild error = %v, want ACP transport validation error", err)
	}
}

func TestBuildDesiredState_DoesNotCreateDuplicatePoolBeadForDiscoveredSession(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":             "worker",
			"session_name":         "worker-gc-existing",
			"manual_session":       "true",
			poolManagedMetadataKey: "true",
			"state":                "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["worker-gc-existing"]; !ok {
		t.Fatalf("desired state missing discovered pool session: keys=%v", mapKeys(desired))
	}

	sessionBeads, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session bead count = %d, want 1 (no duplicate bead)", len(sessionBeads))
	}
}

func TestBuildDesiredState_ZeroScaledPoolSessionKeepsDependencyFloorWhileDraining(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "api-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:api"},
		Metadata: map[string]string{
			"template":     "api",
			"session_name": "api-1",
			"agent_name":   "api-1",
			"state":        "active",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				StartCommand:      "true",
				DependsOn:         []string{"db"},
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["api-1"]; ok {
		t.Fatalf("did not expect zero-scaled pool bead to re-enter desired state: %+v", desired["api-1"])
	}
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 1 {
		t.Fatalf("db desired slots = %d, want 1", dbSlots)
	}
}

func TestBuildDesiredState_PoolCheckInjectsDoltPortForRigScopedAgent(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// The check command outputs "2" only when BEADS_DOLT_SERVER_PORT is set.
	// If the fix works, buildDesiredState prefixes the command with
	// BEADS_DOLT_SERVER_PORT=9876, so the inner shell sees the variable.
	checkCmd := `sh -c 'test -n "$BEADS_DOLT_SERVER_PORT" && printf 2 || printf 0'`
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:     "myrig",
			Path:     rigPath,
			DoltPort: "9876",
		}},
		Agents: []config.Agent{
			{
				Name:              "worker",
				Dir:               "myrig",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: checkCmd,
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	workerSlots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "myrig/worker" {
			workerSlots++
		}
	}
	if workerSlots != 2 {
		t.Fatalf("worker desired slots = %d, want 2 (BEADS_DOLT_SERVER_PORT injection should make check output 2)", workerSlots)
	}
}

func TestBuildDesiredState_PoolCheckUsesCityDoltPortForCityScopedAgent(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	writeRigEndpointCanonicalConfig(t, cityPath, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	ln := listenOnRandomPort(t)
	defer func() { _ = ln.Close() }()
	_, portText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", ln.Addr().String(), err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portText, err)
	}
	if err := writeDoltState(cityPath, doltRuntimeState{Running: true, PID: os.Getpid(), Port: port, DataDir: filepath.Join(cityPath, ".beads", "dolt"), StartedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatalf("writeDoltState: %v", err)
	}
	// Same check command but for a city-scoped agent (no rig). The canonical
	// projected Dolt port should still be present, so the check outputs 2.
	checkCmd := `sh -c 'test -n "$BEADS_DOLT_SERVER_PORT" && printf 2 || printf 0'`
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: checkCmd,
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	workerSlots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "worker" {
			workerSlots++
		}
	}
	if workerSlots != 2 {
		t.Fatalf("worker desired slots = %d, want 2 (projected DoltPort for city-scoped agent)", workerSlots)
	}
}

func TestBuildDesiredState_PoolCheckUsesExplicitRigPassword(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "demo")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCanonicalConfig(t, rigPath, contract.ConfigState{
		IssuePrefix:    "dm",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "3308",
		DoltUser:       "rig-user",
	})
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	checkCmd := `sh -c 'test "$BEADS_DOLT_PASSWORD" = "rig-secret" && printf 2 || printf 0'`
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name: "demo",
			Path: rigPath,
		}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "demo",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
			ScaleCheck:        checkCmd,
		}},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	workerSlots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "demo/worker" {
			workerSlots++
		}
	}
	if workerSlots != 2 {
		t.Fatalf("worker desired slots = %d, want 2 when explicit rig scale_check sees rig-scoped password", workerSlots)
	}
}

func TestBuildDesiredState_PoolCheckUsesManagedCityDoltPortWhenRigHasNoOverride(t *testing.T) {
	skipSlowCmdGCTest(t, "uses a live managed-dolt port probe for scale_check coverage; run make test-cmd-gc-process for full coverage")
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	writeRigEndpointCanonicalConfig(t, cityPath, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	rigPath := filepath.Join(cityPath, "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	defer func() {
		if err := ln.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
	}()
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	checkCmd := `sh -c 'test -n "$BEADS_DOLT_SERVER_PORT" && printf 2 || printf 0'`
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name: "myrig",
			Path: rigPath,
		}},
		Agents: []config.Agent{
			{
				Name:              "worker",
				Dir:               "myrig",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: checkCmd,
			},
		},
	}

	queryEnv, err := controllerQueryRuntimeEnv(cityPath, cfg, &cfg.Agents[0])
	if err != nil {
		t.Fatalf("controllerQueryRuntimeEnv: %v", err)
	}
	wantPort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if got := queryEnv["BEADS_DOLT_SERVER_PORT"]; got != wantPort {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q; GC_DOLT=%q provider=%q scopeProvider=%q cityUses=%v scopeUses=%v current=%q resolvable=%q",
			got, wantPort, os.Getenv("GC_DOLT"), rawBeadsProvider(cityPath), rawBeadsProviderForScope(rigPath, cityPath),
			cityUsesBdStoreContract(cityPath), scopeUsesManagedBdStoreContract(cityPath, rigPath),
			currentManagedDoltPort(cityPath), currentResolvableManagedDoltPort(cityPath))
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	workerSlots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "myrig/worker" {
			workerSlots++
		}
	}
	if workerSlots != 2 {
		t.Fatalf("worker desired slots = %d, want 2 (managed city dolt port should be injected for rig)", workerSlots)
	}
}

func TestBuildDesiredState_ManualPoolSessionInSuspendedRigStaysStopped(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "payments")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "debug api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:payments/api"},
		Metadata: map[string]string{
			"template":       "payments/api",
			"session_name":   "s-gc-300",
			"state":          "creating",
			"manual_session": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:             "payments",
			Path:             rigPath,
			SuspendedOnStart: true,
		}},
		Agents: []config.Agent{
			{
				Name:              "db",
				Dir:               "payments",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				Dir:               "payments",
				DependsOn:         []string{"payments/db"},
				StartCommand:      "echo",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["s-gc-300"]; ok {
		t.Fatalf("manual pool session in suspended rig should not enter desired state: %+v", desired["s-gc-300"])
	}
	for _, tp := range desired {
		if tp.TemplateName == "payments/db" {
			t.Fatalf("suspended-rig manual session should not hold dependency floor: %+v", tp)
		}
	}
}

func TestSelectOrCreatePoolSessionBead_SkipsDrained(t *testing.T) {
	store := beads.NewMemStore()
	drained, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "claude-drained",
			"state":        "drained",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, drained))
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, slot, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "claude", nil, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID == drained.ID {
		t.Fatal("should not reuse drained session bead for new-tier request")
	}
	if slot != 1 {
		t.Fatalf("fresh create slot = %d, want 1", slot)
	}
}

func TestSelectOrCreatePoolSessionBead_PrefersConcreteAgentSlotOverStalePoolMetadata(t *testing.T) {
	store := beads.NewMemStore()
	poisoned, err := store.Create(beads.Bead{
		Title:  "frontend/worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":       "frontend/worker",
			"agent_name":     "frontend/worker-3",
			"alias":          "backend/worker-4",
			"pool_slot":      "4",
			"session_name":   "s-poisoned",
			"pool_managed":   "true",
			"session_origin": "ephemeral",
			"state":          "asleep",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Agents: []config.Agent{
		{Dir: "frontend", Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(10)},
		{Dir: "backend", Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(10)},
	}}
	cfgAgent := &cfg.Agents[0]
	bp := &agentBuildParams{
		city:         cfg,
		beadStore:    store,
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{poisoned}),
		agents:       cfg.Agents,
	}

	preferredPoisoned := sessiontest.SeedBead(t, poisoned)
	result, slot, err := selectOrCreatePoolSessionBead(bp, cfgAgent, "frontend/worker", &preferredPoisoned, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != poisoned.ID {
		t.Fatalf("selected bead %q, want poisoned preferred bead %q", result.ID, poisoned.ID)
	}
	if slot != 3 {
		t.Fatalf("slot = %d, want concrete agent_name slot 3 over stale pool_slot/alias", slot)
	}
}

func TestSelectOrCreatePoolSessionBead_DoesNotRetagDuplicateConcreteSlot(t *testing.T) {
	store := beads.NewMemStore()
	duplicate, err := store.Create(beads.Bead{
		Title:  "kimi",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":       "kimi",
			"agent_name":     "kimi-9",
			"alias":          "kimi-15",
			"pool_slot":      "9",
			"session_name":   "workflows__kimi-mc-duplicate",
			"pool_managed":   "true",
			"session_origin": "ephemeral",
			"state":          "creating",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Agents: []config.Agent{
		{Name: "kimi", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(20)},
	}}
	bp := &agentBuildParams{
		city:         cfg,
		beadStore:    store,
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{duplicate}),
		agents:       cfg.Agents,
	}

	preferredDuplicate := sessiontest.SeedBead(t, duplicate)
	_, _, err = selectOrCreatePoolSessionBead(bp, &cfg.Agents[0], "kimi", &preferredDuplicate, map[string]bool{}, map[int]bool{9: true})
	if err == nil {
		t.Fatal("selectOrCreatePoolSessionBead returned nil error, want duplicate slot rejection")
	}
	if !strings.Contains(err.Error(), "concrete slot already claimed") {
		t.Fatalf("error = %v, want concrete slot already claimed", err)
	}
}

func TestSelectOrCreatePoolSessionBead_DoesNotReserveFreshSlotOnCreateError(t *testing.T) {
	store := &failingPoolSessionNameStore{MemStore: beads.NewMemStore()}
	snapshot := &sessionBeadSnapshot{}
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	usedSlots := map[int]bool{}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	if _, slot, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "claude", nil, map[string]bool{}, usedSlots); err == nil {
		t.Fatal("selectOrCreatePoolSessionBead returned nil error, want create failure")
	} else if slot != 0 {
		t.Fatalf("slot on create error = %d, want 0", slot)
	}
	if usedSlots[1] {
		t.Fatalf("usedSlots[1] = true after create error, want released")
	}

	successStore := beads.NewMemStore()
	bp.beadStore = successStore
	bp.sessionBeads = &sessionBeadSnapshot{}
	result, slot, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "claude", nil, map[string]bool{}, usedSlots)
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead after failure: %v", err)
	}
	if slot != 1 {
		t.Fatalf("slot after previous create error = %d, want 1", slot)
	}
	if result.PoolSlot != "1" {
		t.Fatalf("pool_slot after previous create error = %q, want 1", result.PoolSlot)
	}
}

func TestSelectOrCreatePoolSessionBead_UsesFreshCreateTimeNotBeaconTime(t *testing.T) {
	store := beads.NewMemStore()
	snapshot := &sessionBeadSnapshot{}
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	anchor := time.Now().UTC()
	oldBeacon := anchor.Add(-2 * staleCreatingStateTimeout)
	beforeCreate := anchor.Add(-time.Second)
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
		beaconTime:   oldBeacon,
	}

	result, _, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "claude", nil, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	startedAt, err := time.Parse(time.RFC3339, result.PendingCreateStartedAt)
	if err != nil {
		t.Fatalf("parse pending_create_started_at %q: %v", result.PendingCreateStartedAt, err)
	}
	if startedAt.Before(beforeCreate) {
		t.Fatalf("pending_create_started_at = %s, want current create time after %s", startedAt, beforeCreate)
	}
	if !startedAt.After(oldBeacon.Add(staleCreatingStateTimeout)) {
		t.Fatalf("pending_create_started_at = %s, want independent from stale beacon %s", startedAt, oldBeacon)
	}
	result.CreatedAt = oldBeacon
	if staleCreatingStateInfo(result, &clock.Fake{Time: startedAt.Add(30 * time.Second)}) {
		t.Fatal("fresh pool session was stale when row CreatedAt matched old controller beacon")
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesPreferredDrained(t *testing.T) {
	store := beads.NewMemStore()
	drained, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "claude-drained",
			"state":        "drained",
			"pool_slot":    "4",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, drained))
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	preferredDrained := sessiontest.SeedBead(t, drained)
	result, slot, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "claude", &preferredDrained, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != drained.ID {
		t.Fatal("resume tier should reuse preferred drained session bead")
	}
	if slot != 4 {
		t.Fatalf("preferred reuse slot = %d, want 4", slot)
	}
}

func TestSelectOrCreateDependencyPoolSessionBead_SkipsDrained(t *testing.T) {
	store := beads.NewMemStore()
	drained, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":        "claude",
			"agent_name":      "claude",
			"session_name":    "claude-dep-drained",
			"state":           "asleep",
			"sleep_reason":    "drained",
			"dependency_only": "true",
			"pool_managed":    "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, drained))
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreateDependencyPoolSessionBead(bp, &cfgAgent, "claude")
	if err != nil {
		t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
	}
	if result.ID == drained.ID {
		t.Fatal("should not reuse drained dependency session bead for generic dependency demand")
	}
	if got := result.AgentName; got != "claude-1" {
		t.Fatalf("dependency agent_name = %q, want claude-1", got)
	}
	if got := result.Alias; got != "claude-1" {
		t.Fatalf("dependency alias = %q, want claude-1", got)
	}
	if got := result.PoolSlot; got != "1" {
		t.Fatalf("dependency pool_slot = %q, want 1", got)
	}
	if got := result.Title; got != "claude-1" {
		t.Fatalf("dependency title = %q, want claude-1", got)
	}
	if !containsString(result.Labels, "agent:claude-1") {
		t.Fatalf("dependency labels = %#v, want concrete slot agent label", result.Labels)
	}
}

func TestSelectOrCreateDependencyPoolSessionBead_MaxOneUsesCanonicalIdentity(t *testing.T) {
	store := beads.NewMemStore()
	cfgAgent := config.Agent{
		Name:              "refinery",
		Dir:               "cashmaster",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(1),
	}
	bp := &agentBuildParams{
		cityPath:     t.TempDir(),
		beadStore:    store,
		sessionBeads: &sessionBeadSnapshot{},
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreateDependencyPoolSessionBead(bp, &cfgAgent, "cashmaster/refinery")
	if err != nil {
		t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
	}
	if got := result.AgentName; got != "cashmaster/refinery" {
		t.Fatalf("dependency agent_name = %q, want canonical non-pool identity", got)
	}
	if got := result.Alias; got != "cashmaster/refinery" {
		t.Fatalf("dependency alias = %q, want canonical non-pool identity", got)
	}
	if got := result.PoolSlot; got != "" {
		t.Fatalf("dependency pool_slot = %q, want empty for max_active_sessions=1", got)
	}
	if got := result.Title; got != "cashmaster/refinery" {
		t.Fatalf("dependency title = %q, want canonical non-pool identity", got)
	}
	if containsString(result.Labels, "agent:cashmaster/refinery-1") {
		t.Fatalf("dependency labels = %#v, must not include phantom pool identity", result.Labels)
	}
	if !containsString(result.Labels, "agent:cashmaster/refinery") {
		t.Fatalf("dependency labels = %#v, want canonical agent label", result.Labels)
	}
}

func TestSelectOrCreateDependencyPoolSessionBead_MaxOneNormalizesExistingStaleIdentity(t *testing.T) {
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":                        "cashmaster/refinery",
			"agent_name":                      "cashmaster/refinery-1",
			"alias":                           "cashmaster/refinery-1",
			"session_name":                    "s-refinery-dep-stale",
			"state":                           "awake",
			"dependency_only":                 boolMetadata(true),
			poolManagedMetadataKey:            boolMetadata(true),
			"pool_slot":                       "1",
			poolAliasConflictMetadataKey:      "cashmaster/refinery",
			poolAliasConflictCountMetadataKey: "4",
			poolAliasConflictAtMetadataKey:    "2026-05-06T01:00:00Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, stale))
	cfgAgent := config.Agent{
		Name:              "refinery",
		Dir:               "cashmaster",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(1),
	}
	bp := &agentBuildParams{
		cityPath:     t.TempDir(),
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreateDependencyPoolSessionBead(bp, &cfgAgent, "cashmaster/refinery")
	if err != nil {
		t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
	}
	if result.ID != stale.ID {
		t.Fatalf("dependency reuse ID = %q, want stale bead %q", result.ID, stale.ID)
	}
	if got := result.AgentName; got != "cashmaster/refinery" {
		t.Fatalf("dependency agent_name = %q, want canonical non-pool identity", got)
	}
	if got := result.Alias; got != "cashmaster/refinery" {
		t.Fatalf("dependency alias = %q, want canonical non-pool identity", got)
	}
	if got := result.PoolSlot; got != "" {
		t.Fatalf("dependency pool_slot = %q, want empty after normalization", got)
	}
	if got := result.PoolAliasConflict; got != "" {
		t.Fatalf("dependency pool_alias_conflict = %q, want cleared after successful normalization", got)
	}
	if got := result.PoolAliasConflictCount; got != "" {
		t.Fatalf("dependency pool_alias_conflict_count = %q, want cleared after successful normalization", got)
	}
	if got := result.PoolAliasConflictAt; got != "" {
		t.Fatalf("dependency pool_alias_conflict_at = %q, want cleared after successful normalization", got)
	}
	if containsString(result.Labels, "agent:cashmaster/refinery-1") {
		t.Fatalf("dependency labels = %#v, must not include stale label", result.Labels)
	}
	if !containsString(result.Labels, "agent:cashmaster/refinery") {
		t.Fatalf("dependency labels = %#v, want canonical agent label", result.Labels)
	}
	stored, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", stale.ID, err)
	}
	if stored.Metadata["agent_name"] != "cashmaster/refinery" || stored.Metadata["alias"] != "cashmaster/refinery" || stored.Metadata["pool_slot"] != "" {
		t.Fatalf("stored dependency metadata = %#v, want normalized singleton identity", stored.Metadata)
	}
	if stored.Metadata[poolAliasConflictMetadataKey] != "" || stored.Metadata[poolAliasConflictCountMetadataKey] != "" || stored.Metadata[poolAliasConflictAtMetadataKey] != "" {
		t.Fatalf("stored dependency conflict metadata = %#v, want cleared after successful normalization", stored.Metadata)
	}
}

func TestSelectOrCreateDependencyPoolSessionBead_MaxOnePrefersCanonicalDependencyDuplicate(t *testing.T) {
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery-1",
			"session_name":         "s-refinery-dep-stale",
			"state":                "awake",
			"dependency_only":      boolMetadata(true),
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-dep-canonical",
			"state":                "awake",
			"dependency_only":      boolMetadata(true),
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, stale))
	snapshot.addInfo(sessiontest.SeedBead(t, canonical))
	cfgAgent := config.Agent{
		Name:              "refinery",
		Dir:               "cashmaster",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(1),
	}
	bp := &agentBuildParams{
		cityPath:     t.TempDir(),
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreateDependencyPoolSessionBead(bp, &cfgAgent, "cashmaster/refinery")
	if err != nil {
		t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
	}
	if result.ID != canonical.ID {
		t.Fatalf("dependency reuse ID = %q, want canonical bead %q instead of stale duplicate %q", result.ID, canonical.ID, stale.ID)
	}
	if got := result.AgentName; got != "cashmaster/refinery" {
		t.Fatalf("dependency agent_name = %q, want canonical non-pool identity", got)
	}
	if got := result.PoolSlot; got != "" {
		t.Fatalf("dependency pool_slot = %q, want empty for canonical max-one bead", got)
	}
}

func TestSelectOrCreateDependencyPoolSessionBead_MaxOnePicksEarliestCanonicalDuplicate(t *testing.T) {
	base := time.Date(2026, 5, 16, 10, 20, 0, 0, time.UTC)
	later := beads.Bead{
		ID:        "session-later",
		Title:     "cashmaster/refinery",
		Type:      sessionBeadType,
		Status:    "open",
		CreatedAt: base.Add(time.Minute),
		Labels:    []string{sessionBeadLabel, "agent:cashmaster/refinery", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-dep-later",
			"state":                "awake",
			"dependency_only":      boolMetadata(true),
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
	earliest := beads.Bead{
		ID:        "session-earliest",
		Title:     "cashmaster/refinery",
		Type:      sessionBeadType,
		Status:    "open",
		CreatedAt: base,
		Labels:    []string{sessionBeadLabel, "agent:cashmaster/refinery", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-dep-earliest",
			"state":                "awake",
			"dependency_only":      boolMetadata(true),
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
	cfgAgent := config.Agent{
		Name:              "refinery",
		Dir:               "cashmaster",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(1),
	}
	bp := &agentBuildParams{
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{later, earliest}),
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreateDependencyPoolSessionBead(bp, &cfgAgent, "cashmaster/refinery")
	if err != nil {
		t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
	}
	if result.ID != earliest.ID {
		t.Fatalf("dependency reuse ID = %q, want earliest canonical duplicate %q", result.ID, earliest.ID)
	}
}

func TestSelectOrCreatePoolSessionBeadPicksEarliestReusableSingletonCandidate(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	earliest, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-2",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-2", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-2",
			"alias":                "cashmaster/refinery-2",
			"session_name":         "s-refinery-earliest",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	later, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery-1",
			"session_name":         "s-refinery-later",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, later))
	snapshot.addInfo(sessiontest.SeedBead(t, earliest))
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = snapshot

	result, _, err := selectOrCreatePoolSessionBead(bp, &cfg.Agents[0], "cashmaster/refinery", nil, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != earliest.ID {
		t.Fatalf("selected bead = %q, want earliest reusable candidate %q", result.ID, earliest.ID)
	}
}

func TestSelectOrCreateDependencyPoolSessionBead_DefersAliasWhenConcreteAliasTaken(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "manual session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:manual"},
		Metadata: map[string]string{
			"template":       "manual",
			"agent_name":     "manual",
			"alias":          "claude-1",
			"session_name":   "manual-claude-1",
			"state":          "awake",
			"session_origin": "manual",
			"manual_session": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		cityPath:     t.TempDir(),
		beadStore:    store,
		sessionBeads: &sessionBeadSnapshot{},
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreateDependencyPoolSessionBead(bp, &cfgAgent, "claude")
	if err != nil {
		t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
	}
	if got := result.AgentName; got != "claude-1" {
		t.Fatalf("dependency agent_name = %q, want claude-1", got)
	}
	if got := result.Alias; got != "" {
		t.Fatalf("dependency alias = %q, want deferred until alias guard accepts it", got)
	}
	if got := result.PoolSlot; got != "1" {
		t.Fatalf("dependency pool_slot = %q, want 1", got)
	}
}

func TestSelectOrCreateDependencyPoolSessionBead_ReusesLegacyUnqualifiedTemplateWithFullConfig(t *testing.T) {
	store := beads.NewMemStore()
	legacy, err := store.Create(beads.Bead{
		Title:  "legacy db dependency",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":        "db",
			"session_name":    "s-db-dep-legacy",
			"state":           "awake",
			"dependency_only": "true",
			"pool_managed":    "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, legacy))
	cfg := &config.City{Agents: []config.Agent{{
		Name:              "db",
		Dir:               "gascity",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(3),
	}}}
	bp := &agentBuildParams{
		city:         cfg,
		cityPath:     t.TempDir(),
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       cfg.Agents,
	}

	result, err := selectOrCreateDependencyPoolSessionBead(bp, &cfg.Agents[0], "gascity/db")
	if err != nil {
		t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
	}
	if result.ID != legacy.ID {
		t.Fatalf("dependency reuse ID = %q, want legacy bead %q", result.ID, legacy.ID)
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesAvailableForNewTier(t *testing.T) {
	store := beads.NewMemStore()
	// Existing awake session bead without assigned work — should be reused
	// for new-tier to prevent session bead duplication across ticks.
	awake, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude-3",
			"alias":        "claude-3",
			"session_name": "claude-awake",
			"state":        "awake",
			"pool_slot":    "3",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, awake))
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, slot, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "claude", nil, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != awake.ID {
		t.Fatal("new-tier should reuse available (non-drained) session bead")
	}
	if slot != 3 {
		t.Fatalf("available reuse slot = %d, want 3", slot)
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesLegacyUnqualifiedTemplateWithFullConfig(t *testing.T) {
	store := beads.NewMemStore()
	legacy, err := store.Create(beads.Bead{
		Title:  "legacy refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "refinery",
			"session_name": "s-refinery-legacy",
			"state":        "awake",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, legacy))
	cfg := &config.City{Agents: []config.Agent{{
		Name:              "refinery",
		Dir:               "cashmaster",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(3),
	}}}
	bp := &agentBuildParams{
		city:         cfg,
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       cfg.Agents,
	}

	result, _, err := selectOrCreatePoolSessionBead(bp, &cfg.Agents[0], "cashmaster/refinery", nil, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != legacy.ID {
		t.Fatalf("pool reuse ID = %q, want legacy bead %q", result.ID, legacy.ID)
	}
}

func TestSelectOrCreatePoolSessionBead_SkipsAssignedForNewTier(t *testing.T) {
	store := beads.NewMemStore()
	assigned, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "claude-assigned",
			"state":        "active",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, assigned))
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
		assignedWorkBeads: []beads.Bead{{
			ID:       "w-assigned",
			Status:   "in_progress",
			Assignee: assigned.ID,
		}},
	}

	result, _, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "claude", nil, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID == assigned.ID {
		t.Fatal("new-tier should not reuse a session bead that has assigned work")
	}
}

func TestSelectOrCreatePoolSessionBead_SkipsAsleepBeads(t *testing.T) {
	// An asleep pool session should NOT be reused for new demand.
	// The reconciler should create a fresh session instead.
	// This prevents a deadlock where an asleep bead fills a pool slot
	// but ComputeAwakeSet correctly refuses to wake it (asleep
	// ephemerals are not reused).
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	asleep, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-real-world-app-old",
			"state":        "asleep",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{asleep})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, _, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "polecat", nil, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID == asleep.ID {
		t.Fatal("asleep pool session should not be reused — a fresh session should be created instead")
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesActiveBeforeCreatingNew(t *testing.T) {
	// An active (awake) pool session IS reused — no fresh bead created.
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	active, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-real-world-app-live",
			"state":        "active",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{active})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, _, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "polecat", nil, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != active.ID {
		t.Fatalf("active pool session should be reused, got %s want %s", result.ID, active.ID)
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesCreatingBeforeCreatingNew(t *testing.T) {
	// A creating pool session IS reused — no fresh bead created.
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	creating, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-real-world-app-new",
			"state":        "creating",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{creating})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, _, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "polecat", nil, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != creating.ID {
		t.Fatalf("creating pool session should be reused, got %s want %s", result.ID, creating.ID)
	}
}

func TestSelectOrCreatePoolSessionBead_SkipsAsleepButReusesActive(t *testing.T) {
	// With both an asleep and active bead for the same template,
	// the active one is reused and the asleep one is ignored.
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	asleep, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-real-world-app-old",
			"state":        "asleep",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-real-world-app-live",
			"state":        "active",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{asleep, active})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, _, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "polecat", nil, map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID == asleep.ID {
		t.Fatal("should skip asleep bead")
	}
	if result.ID != active.ID {
		t.Fatalf("should reuse active bead, got %s want %s", result.ID, active.ID)
	}
}

// TestCanonicalSessionIdentity is a regression test for the config-drift
// oscillation caused by divergent agent-identity resolution across the
// paths in buildDesiredState. Different paths (rediscovery, store-backed
// dependency-floor, realizePoolDesiredSessions) were feeding the same
// session bead through resolveTemplate with either the base qualified
// name or a deep-copied instance-agent qualified name. Before GC_ALIAS
// was excluded from CoreFingerprint, that identity mismatch flipped the
// fingerprint every tick and the reconciler drained the live session as
// config drift. See PRs #833 and #869.
//
// Pool-instance agents with a stamped pool_slot must resolve to the
// instance identity; named beads must resolve to the named identity;
// everything else falls back to the base qualified name.
func TestCanonicalSessionIdentity(t *testing.T) {
	poolAgent := &config.Agent{
		Name:              "dog",
		Dir:               "gascity",
		MinActiveSessions: intPtr(0),
		// MaxActiveSessions nil = unlimited, which makes SupportsInstanceExpansion true.
	}
	singleton := &config.Agent{
		Name:              "refinery",
		Dir:               "gascity",
		MaxActiveSessions: intPtr(1),
	}

	stampedPoolBead := beads.Bead{
		Metadata: map[string]string{
			"template":     "gascity/dog",
			"agent_name":   "gascity/dog",
			"pool_slot":    "1",
			"pool_managed": "true",
			"session_name": "s-dog-1",
			"state":        "active",
		},
	}
	unstampedCreatingBead := beads.Bead{
		Metadata: map[string]string{
			"template":     "gascity/dog",
			"agent_name":   "gascity/dog",
			"pool_managed": "true",
			"session_name": "s-dog-new",
			"state":        "creating",
		},
	}
	namedBead := beads.Bead{
		Metadata: map[string]string{
			"template":                   "gascity/dog",
			"agent_name":                 "gascity/dog",
			"session_name":               "s-opus",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "gascity/opus",
		},
	}

	t.Run("pool-instance agent with stamped slot returns instance identity", func(t *testing.T) {
		agent, qn := canonicalSessionIdentity(poolAgent, stampedPoolBead)
		if agent == poolAgent {
			t.Errorf("agent = base cfgAgent, want deep-copied instance agent")
		}
		if agent == nil || agent.Name != "dog-1" {
			t.Errorf("agent.Name = %q, want %q", agentName(agent), "dog-1")
		}
		if agent != nil && agent.PoolName != "gascity/dog" {
			t.Errorf("agent.PoolName = %q, want %q", agent.PoolName, "gascity/dog")
		}
		if qn != "gascity/dog-1" {
			t.Errorf("qn = %q, want %q", qn, "gascity/dog-1")
		}
	})

	t.Run("pool-instance agent without slot stamp falls back to base", func(t *testing.T) {
		agent, qn := canonicalSessionIdentity(poolAgent, unstampedCreatingBead)
		if agent != poolAgent {
			t.Errorf("agent = deep-copy, want base cfgAgent (no slot stamped yet)")
		}
		if qn != "gascity/dog" {
			t.Errorf("qn = %q, want base %q", qn, "gascity/dog")
		}
	})

	t.Run("named bead keeps base identity (out of scope for this canonicalization)", func(t *testing.T) {
		// Named-session TemplateParams carry ConfiguredNamedIdentity/Mode,
		// GC_SESSION_ORIGIN=named, and a canonical session_name set by the
		// main named-sessions loop and reconstructNamedSessionTemplateParams.
		// Rewriting just the identity qualifier in rediscovery without also
		// repopulating that contract would produce a partially-named
		// TemplateParams that downstream consumers don't expect — so the
		// helper intentionally leaves named beads on the base shape.
		agent, qn := canonicalSessionIdentity(poolAgent, namedBead)
		if agent != poolAgent {
			t.Errorf("named bead must not produce a deep-copied instance agent")
		}
		if qn != "gascity/dog" {
			t.Errorf("qn = %q, want base %q (named canonicalization is scoped out)", qn, "gascity/dog")
		}
	})

	t.Run("singleton (non-expanding) agent returns base regardless of bead shape", func(t *testing.T) {
		agent, qn := canonicalSessionIdentity(singleton, stampedPoolBead)
		if agent != singleton {
			t.Errorf("singleton agent should not be deep-copied")
		}
		if qn != "gascity/refinery" {
			t.Errorf("qn = %q, want base %q", qn, "gascity/refinery")
		}
	})

	t.Run("nil agent returns empty", func(t *testing.T) {
		agent, qn := canonicalSessionIdentity(nil, stampedPoolBead)
		if agent != nil || qn != "" {
			t.Errorf("nil agent: got (%v, %q), want (nil, \"\")", agent, qn)
		}
	})
}

func agentName(a *config.Agent) string {
	if a == nil {
		return "<nil>"
	}
	return a.Name
}

func TestSessionBeadConfigAgent_UsesMultipleSessionShapeForMaxZero(t *testing.T) {
	cfgAgent := &config.Agent{
		Name:              "ant",
		Dir:               "demo",
		MaxActiveSessions: intPtr(0),
	}

	got := sessionBeadConfigAgent(cfgAgent, "demo/ant-adhoc-123")
	if got == cfgAgent {
		t.Fatal("sessionBeadConfigAgent returned base agent, want deep-copied instance agent")
	}
	if got == nil || got.Name != "ant-adhoc-123" {
		t.Fatalf("agent.Name = %q, want %q", agentName(got), "ant-adhoc-123")
	}
	if got.PoolName != "demo/ant" {
		t.Fatalf("agent.PoolName = %q, want %q", got.PoolName, "demo/ant")
	}
	if template := templateNameFor(got, "demo/ant-adhoc-123"); template != "demo/ant" {
		t.Fatalf("templateNameFor(instance) = %q, want %q", template, "demo/ant")
	}
}

// TestEnsureDependencyOnlyTemplate_StoreBackedUsesInstanceIdentity is a
// regression test for the second half of PR #833's fix. Before the fix,
// the store-backed dependency-floor path used the base agent identity
// ("rig/db") while the no-store path used the pool-instance identity
// ("rig/db-1"). Both paths build FingerprintExtra from their agent and
// feed qualifiedName into resolveTemplate. If a live dep-floor session
// ever had its bead touched by both code paths, or the system transitioned
// from no-store to store-backed mid-lifetime, the divergent shape drove
// the reconciler to declare config drift and drain. GC_ALIAS is no longer
// a fingerprint input, but the canonicalization still protects the
// remaining identity-sensitive inputs and runtime-visible identity.
//
// The fix canonicalizes the store-backed path onto instance identity to
// match the no-store branch and realizePoolDesiredSessions. This test
// exercises the store-backed path (via a seeded pool-managed root bead
// that anchors realizeDependencyFloors) and asserts GC_ALIAS is the
// instance qualified name.
func TestEnsureDependencyOnlyTemplate_StoreBackedUsesInstanceIdentity(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{
				Name:              "db",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 0",
			},
			{
				Name:              "api",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 0",
				DependsOn:         []string{"gascity/db"},
			},
		},
	}

	// Seed a pool-managed root bead for api so discoverSessionBeadsWithRoots
	// reports api as a realized root; realizeDependencyFloors then walks the
	// dep graph and materializes the dep-floor for db via the store-backed
	// branch of ensureDependencyOnlyTemplate.
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:gascity/api"},
		Metadata: map[string]string{
			"template":     "gascity/api",
			"agent_name":   "gascity/api",
			"session_name": "s-api-root",
			"state":        "active",
			"pool_managed": "true",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatalf("seed api root bead: %v", err)
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	var tp TemplateParams
	var found bool
	for _, entry := range dsResult.State {
		if entry.TemplateName == "gascity/db" && entry.DependencyOnly {
			tp = entry
			found = true
			break
		}
	}
	if !found {
		entries := make([]string, 0, len(dsResult.State))
		for k, v := range dsResult.State {
			entries = append(entries, fmt.Sprintf("%s{template=%s depOnly=%v alias=%s}", k, v.TemplateName, v.DependencyOnly, v.Env["GC_ALIAS"]))
		}
		t.Fatalf("store-backed dependency floor for db not found, desired = %v", entries)
	}

	alias := tp.Env["GC_ALIAS"]
	if want := "gascity/db-1"; alias != want {
		t.Fatalf("store-backed dep-floor GC_ALIAS = %q, want instance identity %q. "+
			"Before PR #833's canonicalization this came back as base %q, which "+
			"disagreed with realizePoolDesiredSessions and triggered config-drift drain.",
			alias, want, "gascity/db")
	}
	if template := tp.Env["GC_TEMPLATE"]; template != "gascity/db" {
		t.Fatalf("store-backed dep-floor GC_TEMPLATE = %q, want base %q", template, "gascity/db")
	}
}

func TestEnsureDependencyOnlyTemplate_StoreBackedMaxOneUsesCanonicalIdentity(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{
				Name:              "db",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(1),
				ScaleCheck:        "printf 0",
			},
			{
				Name:              "api",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 0",
				DependsOn:         []string{"gascity/db"},
			},
		},
	}

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:gascity/api"},
		Metadata: map[string]string{
			"template":             "gascity/api",
			"agent_name":           "gascity/api",
			"session_name":         "s-api-root",
			"state":                "active",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	}); err != nil {
		t.Fatalf("seed api root bead: %v", err)
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	var tp TemplateParams
	var found bool
	for _, entry := range dsResult.State {
		if entry.TemplateName == "gascity/db" && entry.DependencyOnly {
			tp = entry
			found = true
			break
		}
	}
	if !found {
		entries := make([]string, 0, len(dsResult.State))
		for k, v := range dsResult.State {
			entries = append(entries, fmt.Sprintf("%s{template=%s depOnly=%v alias=%s}", k, v.TemplateName, v.DependencyOnly, v.Env["GC_ALIAS"]))
		}
		t.Fatalf("store-backed dependency floor for db not found, desired = %v", entries)
	}
	if alias := tp.Env["GC_ALIAS"]; alias != "gascity/db" {
		t.Fatalf("store-backed dep-floor GC_ALIAS = %q, want canonical identity %q", alias, "gascity/db")
	}

	sessionBeads, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	var depBead beads.Bead
	found = false
	for _, candidate := range sessionBeads {
		if candidate.Metadata["template"] == "gascity/db" && candidate.Metadata["agent_name"] == "gascity/db" {
			depBead = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("dependency-floor bead for db not found; beads=%#v", sessionBeads)
	}
	if got := depBead.Metadata["agent_name"]; got != "gascity/db" {
		t.Fatalf("dependency agent_name = %q, want canonical non-pool identity", got)
	}
	if got := depBead.Metadata["alias"]; got != "gascity/db" {
		t.Fatalf("dependency alias = %q, want canonical non-pool identity", got)
	}
	if got := depBead.Metadata["pool_slot"]; got != "" {
		t.Fatalf("dependency pool_slot = %q, want empty for max_active_sessions=1", got)
	}
	if depBead.Title != "gascity/db" {
		t.Fatalf("dependency title = %q, want canonical non-pool identity", depBead.Title)
	}
	if containsString(depBead.Labels, "agent:gascity/db-1") {
		t.Fatalf("dependency labels = %#v, must not include phantom pool identity", depBead.Labels)
	}
	if !containsString(depBead.Labels, "agent:gascity/db") {
		t.Fatalf("dependency labels = %#v, want canonical agent label", depBead.Labels)
	}
}

func TestBuildDesiredState_DependencyFloorSkipsFailedCreate(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{
				Name:              "db",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 0",
			},
			{
				Name:              "api",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 0",
				DependsOn:         []string{"gascity/db"},
			},
		},
	}

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:gascity/api"},
		Metadata: map[string]string{
			"template":     "gascity/api",
			"agent_name":   "gascity/api",
			"session_name": "s-api-root",
			"state":        "active",
			"pool_managed": "true",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatalf("seed api root bead: %v", err)
	}
	failed, err := store.Create(beads.Bead{
		Title:  "db failed create",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:gascity/db"},
		Metadata: map[string]string{
			"template":             "gascity/db",
			"agent_name":           "gascity/db-failed",
			"session_name":         "s-db-failed",
			"state":                string(sessionpkg.StateFailedCreate),
			"dependency_only":      "true",
			"pool_managed":         "true",
			"pool_slot":            "1",
			"pending_create_claim": "true",
		},
	})
	if err != nil {
		t.Fatalf("seed failed-create dependency bead: %v", err)
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	var tp TemplateParams
	var found bool
	for _, entry := range dsResult.State {
		if entry.TemplateName == "gascity/db" && entry.DependencyOnly {
			tp = entry
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("store-backed dependency floor for db not found: %+v", dsResult.State)
	}
	if tp.SessionName == failed.Metadata["session_name"] {
		t.Fatalf("dependency floor reused failed-create bead %s with session %q", failed.ID, tp.SessionName)
	}
	if tp.SessionName == "" {
		t.Fatal("dependency floor session name is empty")
	}
}

func TestBuildDesiredState_DependencyFloorIgnoresConfigBlindLegacySlotRecovery(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{
				Name:              "db",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 0",
			},
			{
				Name:              "api",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 0",
				DependsOn:         []string{"gascity/db"},
			},
		},
	}

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:gascity/api"},
		Metadata: map[string]string{
			"template":     "gascity/api",
			"agent_name":   "gascity/api",
			"session_name": "s-api-root",
			"state":        "active",
			"pool_managed": "true",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatalf("seed api root bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "db",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"agent_name":      "db-2",
			"session_name":    "s-db-dep-legacy",
			"state":           "active",
			"dependency_only": "true",
			"pool_managed":    "true",
		},
	}); err != nil {
		t.Fatalf("seed dependency-only db bead: %v", err)
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	var tp TemplateParams
	var found bool
	for _, entry := range dsResult.State {
		if entry.TemplateName == "gascity/db" && entry.DependencyOnly {
			tp = entry
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("store-backed dependency floor for db not found: %+v", dsResult.State)
	}

	if got, want := tp.Env["GC_ALIAS"], "gascity/db-1"; got != want {
		t.Fatalf("store-backed dep-floor GC_ALIAS = %q, want %q when legacy bead lacks matching template metadata", got, want)
	}
}

// TestBuildDesiredState_PoolBeadIdentityAgreesAcrossRealizeAndCanonicalHelper
// is the round-trip regression for PR #833's canonicalization. It locks in the
// actual invariant the fix promises: a pool-managed session bead produces the
// same identity shape and same CoreFingerprint-contributing (GC_TEMPLATE,
// FingerprintExtra) pair whether it is resolved through realizePoolDesiredSessions
// or through canonicalSessionIdentity (the shared helper rediscovery and the
// store-backed dependency-floor path both use).
//
// Catching a regression here matters because the drift bug was silent — the
// reconciler just drained live sessions every other tick. If a future change
// to realizePoolDesiredSessions (different poolInstanceName format, new
// identity field in deepCopyAgent) diverges from the helper, nothing else in
// CI will notice until a city starts losing sessions again.
func TestBuildDesiredState_PoolBeadIdentityAgreesAcrossRealizeAndCanonicalHelper(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{
				Name:              "dog",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 1",
			},
		},
	}

	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "dog pool session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:gascity/dog"},
		Metadata: map[string]string{
			"template":     "gascity/dog",
			"agent_name":   "gascity/dog-1",
			"session_name": "s-dog-1",
			"state":        "active",
			"pool_managed": "true",
			"pool_slot":    "1",
		},
	})
	if err != nil {
		t.Fatalf("seed pool bead: %v", err)
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	// realize should have claimed our seeded bead (slot 1) and produced a
	// desired entry keyed by session_name.
	var realizeTP TemplateParams
	var realized bool
	for _, tp := range dsResult.State {
		if tp.TemplateName == "gascity/dog" && !tp.DependencyOnly {
			realizeTP = tp
			realized = true
			break
		}
	}
	if !realized {
		keys := make([]string, 0, len(dsResult.State))
		for k, v := range dsResult.State {
			keys = append(keys, fmt.Sprintf("%s{template=%s depOnly=%v}", k, v.TemplateName, v.DependencyOnly))
		}
		t.Fatalf("realize did not produce a desired entry for gascity/dog; desired = %v", keys)
	}

	// The helper is what rediscovery and the store-backed dep-floor path
	// feed into resolveTemplate. For a stamped pool bead this must exactly
	// match what realize produced — same qualified name, same agent shape,
	// same FingerprintExtra.
	helperAgent, helperQN := canonicalSessionIdentity(&cfg.Agents[0], bead)
	if helperAgent == nil || helperAgent.Name != "dog-1" {
		t.Fatalf("canonicalSessionIdentity agent = %v, want dog-1", helperAgent)
	}
	if want := "gascity/dog-1"; helperQN != want {
		t.Fatalf("canonicalSessionIdentity qn = %q, want %q", helperQN, want)
	}

	if realizeAlias := realizeTP.Env["GC_ALIAS"]; realizeAlias != helperQN {
		t.Fatalf("realize GC_ALIAS = %q, canonical helper qn = %q — runtime identity diverged across rediscovery/realize",
			realizeAlias, helperQN)
	}
	if want := "gascity/dog"; realizeTP.Env["GC_TEMPLATE"] != want {
		t.Fatalf("realize GC_TEMPLATE = %q, want base %q", realizeTP.Env["GC_TEMPLATE"], want)
	}

	helperFPExtra := buildFingerprintExtra(helperAgent)
	if len(helperFPExtra) != len(realizeTP.FPExtra) {
		t.Fatalf("FPExtra size mismatch: realize=%v helper=%v", realizeTP.FPExtra, helperFPExtra)
	}
	for k, rv := range realizeTP.FPExtra {
		if hv, present := helperFPExtra[k]; !present {
			t.Errorf("helper FPExtra missing key %q (realize has %q)", k, rv)
		} else if hv != rv {
			t.Errorf("FPExtra[%q] mismatch: realize=%q helper=%q", k, rv, hv)
		}
	}
	// pool.check must be absent from both — it was the QualifiedName-bearing
	// field that drove the original oscillation.
	if _, has := realizeTP.FPExtra["pool.check"]; has {
		t.Errorf("realize FPExtra still contains pool.check — fix incomplete: %v", realizeTP.FPExtra)
	}
}

// TestBuildDesiredState_RigScopedScaleCheckExpandsRigTemplate verifies that
// {{.Rig}} in a pool agent's scale_check is substituted with the configured
// rig name before the shell command runs — regression test for #793.
//
// The scale_check grep-counts the expanded rig name. Literal "{{.Rig}}"
// never matches the target rig name, so the broken (pre-fix) behavior
// returns 0; the fixed behavior returns 1 for both rig-specific commands,
// proving per-rig substitution is happening on each branch.
func TestBuildDesiredState_RigScopedScaleCheckExpandsRigTemplate(t *testing.T) {
	cityPath := t.TempDir()
	rigAlpha := filepath.Join(cityPath, "alpha")
	rigBeta := filepath.Join(cityPath, "beta")
	if err := os.MkdirAll(rigAlpha, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigBeta, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: rigAlpha},
			{Name: "beta", Path: rigBeta},
		},
		Agents: []config.Agent{
			{
				Name:              "ant",
				Dir:               "alpha",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(5),
				ScaleCheck:        "echo {{.Rig}} | grep -c alpha",
			},
			{
				Name:              "ant",
				Dir:               "beta",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(5),
				ScaleCheck:        "echo {{.Rig}} | grep -c beta",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	alphaCount, ok := dsResult.ScaleCheckCounts["alpha/ant"]
	if !ok {
		t.Fatalf("ScaleCheckCounts missing alpha/ant; got %#v", dsResult.ScaleCheckCounts)
	}
	if alphaCount != 1 {
		t.Errorf("alpha/ant scale_check count = %d, want 1 (expansion of {{.Rig}} -> alpha makes grep match)", alphaCount)
	}

	betaCount, ok := dsResult.ScaleCheckCounts["beta/ant"]
	if !ok {
		t.Fatalf("ScaleCheckCounts missing beta/ant; got %#v", dsResult.ScaleCheckCounts)
	}
	if betaCount != 1 {
		t.Errorf("beta/ant scale_check count = %d, want 1 (expansion of {{.Rig}} -> beta makes grep match)", betaCount)
	}
}

func TestBuildDesiredState_NamedSessionWorkQueryDoesNotDriveControllerDemand(t *testing.T) {
	cityPath := t.TempDir()
	rigDir := filepath.Join(cityPath, "alpha")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "alpha", Path: rigDir}},
		Agents: []config.Agent{{
			Name:              "dog",
			Dir:               "alpha",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "echo {{.Rig}} | grep alpha",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "alpha/dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	if dsResult.NamedSessionDemand["alpha/dog"] {
		t.Fatal("NamedSessionDemand[alpha/dog] came from controller-side work_query")
	}
}

func TestBuildDesiredState_OpenBlockedControlDispatcherWorkRetainsDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{
		Title:  "blocking worker attempt",
		Type:   "task",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	control, err := store.Create(beads.Bead{
		Title:  "Finalize cleanup",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":      "retry",
			"gc.routed_to": "core.control-dispatcher",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	if err := store.DepAdd(control.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block control: %v", err)
	}

	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              config.ControlDispatcherAgentName,
			BindingName:       "core",
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MaxActiveSessions: &maxActive,
		}},
	}
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		cityPath,
		time.Now().UTC(),
		cfg,
		runtime.NewFake(),
		store,
		nil,
		newSessionBeadSnapshot(nil),
		nil,
		io.Discard,
	)

	if got := result.ScaleCheckCounts["core.control-dispatcher"]; got != 1 {
		t.Fatalf("ScaleCheckCounts[core.control-dispatcher] = %d, want 1", got)
	}
	if len(result.State) != 1 {
		t.Fatalf("desired state count = %d, want 1; state=%v", len(result.State), mapKeys(result.State))
	}
	for _, desired := range result.State {
		if desired.TemplateName != "core.control-dispatcher" {
			t.Fatalf("desired TemplateName = %q, want core.control-dispatcher", desired.TemplateName)
		}
	}
}

func TestBuildDesiredState_RepairsCityRoutedRigControlWork(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	blocker, err := rigStore.Create(beads.Bead{
		Title:  "blocking worker attempt",
		Type:   "task",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	control, err := rigStore.Create(beads.Bead{
		Title:  "Finalize rig workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":                        "workflow-finalize",
			"gc.routed_to":                   "core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "rig:fixture",
			"gc.control_dispatcher_fallback": "fixture/core.control-dispatcher->core.control-dispatcher",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	if err := rigStore.DepAdd(control.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block control: %v", err)
	}

	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "fixture", Path: t.TempDir()}},
		Agents: []config.Agent{
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				Dir:               "fixture",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
		},
	}
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		cityPath,
		time.Now().UTC(),
		cfg,
		runtime.NewFake(),
		cityStore,
		map[string]beads.Store{"fixture": rigStore},
		newSessionBeadSnapshot(nil),
		nil,
		io.Discard,
	)

	stored, err := rigStore.Get(control.ID)
	if err != nil {
		t.Fatalf("get repaired control: %v", err)
	}
	if got := stored.Metadata["gc.routed_to"]; got != "fixture/core.control-dispatcher" {
		t.Fatalf("stored gc.routed_to = %q, want fixture/core.control-dispatcher", got)
	}
	if got := stored.Metadata[beadmeta.ControlDispatcherFallbackMetadataKey]; got != "" {
		t.Fatalf("stored gc.control_dispatcher_fallback = %q, want retired fallback marker cleared", got)
	}
	if got := result.ScaleCheckCounts["fixture/core.control-dispatcher"]; got != 1 {
		t.Fatalf("ScaleCheckCounts[fixture/core.control-dispatcher] = %d, want 1", got)
	}
	if got := result.ScaleCheckCounts["core.control-dispatcher"]; got != 0 {
		t.Fatalf("ScaleCheckCounts[core.control-dispatcher] = %d, want 0", got)
	}
	for _, desired := range result.State {
		if desired.TemplateName == "core.control-dispatcher" {
			t.Fatalf("desired state includes city dispatcher for rig-store control work: %+v", desired)
		}
	}
}

func TestBuildDesiredState_RepairsAliasedRigControlWorkOnlyOnce(t *testing.T) {
	cityPath := t.TempDir()
	cityStore, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	control, err := cityStore.Create(beads.Bead{
		Title:  "Finalize rig workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     "core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "rig:fixture",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	// Legacy unscoped file mode opens the city view separately while every rig
	// shares another cache over the same .gc/beads.json. Use a second physical
	// handle for the rig view, and expose it through two rig candidates, so a
	// pointer-only dedup cannot make this regression pass.
	rigStore, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open aliased rig store: %v", err)
	}

	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "fixture", Path: t.TempDir()},
			{Name: "other", Path: t.TempDir()},
		},
		Agents: []config.Agent{
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				Dir:               "fixture",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				Dir:               "other",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
		},
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city",
		cityPath,
		time.Now().UTC(),
		cfg,
		runtime.NewFake(),
		cityStore,
		map[string]beads.Store{"fixture": rigStore, "other": rigStore},
		newSessionBeadSnapshot(nil),
		nil,
		io.Discard,
	)

	verificationStore, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open verification store: %v", err)
	}
	stored, err := verificationStore.Get(control.ID)
	if err != nil {
		t.Fatalf("get repaired control: %v", err)
	}
	if got := stored.Metadata[beadmeta.RoutedToMetadataKey]; got != "fixture/core.control-dispatcher" {
		t.Fatalf("stored gc.routed_to = %q, want fixture/core.control-dispatcher", got)
	}
	if got := result.ScaleCheckCounts["fixture/core.control-dispatcher"]; got != 1 {
		t.Fatalf("rig dispatcher demand = %d, want 1", got)
	}
	if got := result.ScaleCheckCounts["core.control-dispatcher"]; got != 0 {
		t.Fatalf("city dispatcher phantom demand = %d, want 0 for one aliased rig-owned bead", got)
	}
	if got := result.ScaleCheckCounts["other/core.control-dispatcher"]; got != 0 {
		t.Fatalf("other rig dispatcher phantom demand = %d, want 0 for fixture-owned bead", got)
	}
}

func TestRepairControlDispatcherRoutesDoesNotGuessUnscopedControlOwnership(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title:  "Finalize unscoped workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:     beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey: "core.control-dispatcher",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "fixture", Path: t.TempDir()}},
		Agents: []config.Agent{
			{Name: config.ControlDispatcherAgentName, BindingName: "core"},
			{Name: config.ControlDispatcherAgentName, BindingName: "core", Dir: "fixture"},
		},
	}
	work := []beads.Bead{control}
	repairControlDispatcherRoutesForStoreScope(
		t.TempDir(),
		cfg,
		work,
		[]beads.Store{store},
		[]string{"rig:fixture"},
		io.Discard,
	)

	stored, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if got := stored.Metadata[beadmeta.RoutedToMetadataKey]; got != "core.control-dispatcher" {
		t.Fatalf("stored gc.routed_to = %q, want unchanged without authoritative gc.root_store_ref", got)
	}
	if got := work[0].Metadata[beadmeta.RoutedToMetadataKey]; got != "core.control-dispatcher" {
		t.Fatalf("in-memory gc.routed_to = %q, want unchanged without authoritative gc.root_store_ref", got)
	}
}

func TestBuildDesiredState_RepairsRigRoutedCityControlWork(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	control, err := cityStore.Create(beads.Bead{
		Title:  "Finalize city workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     "fixture/core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "city:test-city",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}

	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "fixture", Path: t.TempDir()}},
		Agents: []config.Agent{
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				Dir:               "fixture",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
		},
	}
	result := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), cityStore,
		map[string]beads.Store{"fixture": rigStore}, newSessionBeadSnapshot(nil), nil, io.Discard,
	)

	stored, err := cityStore.Get(control.ID)
	if err != nil {
		t.Fatalf("get repaired control: %v", err)
	}
	if got := stored.Metadata[beadmeta.RoutedToMetadataKey]; got != "core.control-dispatcher" {
		t.Fatalf("stored gc.routed_to = %q, want city route core.control-dispatcher", got)
	}
	if got := result.ScaleCheckCounts["core.control-dispatcher"]; got != 1 {
		t.Fatalf("city dispatcher demand = %d, want 1", got)
	}
	if got := result.ScaleCheckCounts["fixture/core.control-dispatcher"]; got != 0 {
		t.Fatalf("rig dispatcher demand = %d, want 0 for city-store control work", got)
	}
}

func TestBuildDesiredState_DoesNotWakeRigDispatcherWhenCityRouteRepairFails(t *testing.T) {
	cityPath := t.TempDir()
	cityBase := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	control, err := cityBase.Create(beads.Bead{
		Title:  "Finalize city workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     "fixture/core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "city:test-city",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	writeErr := errors.New("route repair unavailable")
	cityStore := routeRepairUpdateFailStore{Store: cityBase, err: writeErr}

	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "fixture", Path: t.TempDir()}},
		Agents: []config.Agent{
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				Dir:               "fixture",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
		},
	}
	var stderr bytes.Buffer
	result := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), cityStore,
		map[string]beads.Store{"fixture": rigStore}, newSessionBeadSnapshot(nil), nil, &stderr,
	)

	stored, err := cityBase.Get(control.ID)
	if err != nil {
		t.Fatalf("get unrepaired control: %v", err)
	}
	if got := stored.Metadata[beadmeta.RoutedToMetadataKey]; got != "fixture/core.control-dispatcher" {
		t.Fatalf("durable gc.routed_to = %q, want unchanged after failed repair", got)
	}
	if got := result.ScaleCheckCounts["fixture/core.control-dispatcher"]; got != 0 {
		t.Fatalf("rig dispatcher demand = %d, want 0 for unreachable city-store control work", got)
	}
	if !strings.Contains(stderr.String(), writeErr.Error()) {
		t.Fatalf("stderr = %q, want route repair failure", stderr.String())
	}
}

func TestBuildDesiredState_DoesNotWakeCityDispatcherForUnrepairableRigControlWork(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	control, err := rigStore.Create(beads.Bead{
		Title:  "Finalize rig workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     "core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "rig:fixture",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}

	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "fixture", Path: t.TempDir()}},
		Agents: []config.Agent{{
			Name:              config.ControlDispatcherAgentName,
			BindingName:       "core",
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MaxActiveSessions: &maxActive,
		}},
	}
	var stderr bytes.Buffer
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		t.TempDir(),
		time.Now().UTC(),
		cfg,
		runtime.NewFake(),
		cityStore,
		map[string]beads.Store{"fixture": rigStore},
		newSessionBeadSnapshot(nil),
		nil,
		&stderr,
	)

	stored, err := rigStore.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if got := stored.Metadata["gc.routed_to"]; got != "core.control-dispatcher" {
		t.Fatalf("stored gc.routed_to = %q, want unchanged for operator repair", got)
	}
	if got := result.ScaleCheckCounts["core.control-dispatcher"]; got != 0 {
		t.Fatalf("ScaleCheckCounts[core.control-dispatcher] = %d, want 0", got)
	}
	if !strings.Contains(stderr.String(), `control bead `+control.ID+` in rig store "fixture" has no configured control-dispatcher`) {
		t.Fatalf("stderr missing actionable rig dispatcher error: %q", stderr.String())
	}
}

func TestCollectOpenUnassignedRoutedWorkKeepsSameIDAcrossStoreScopes(t *testing.T) {
	sharedID := "shared-control-id"
	cityStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: sharedID, Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow-finalize", "gc.routed_to": "core.control-dispatcher",
		},
	}}, nil)
	rigStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: sharedID, Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow-finalize", "gc.routed_to": "city/core.control-dispatcher",
		},
	}}, nil)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "city", Path: t.TempDir()}},
	}

	work, _, refs := collectOpenUnassignedRoutedWork(
		cfg,
		cityStore,
		map[string]beads.Store{"city": rigStore},
		nil,
		io.Discard,
	)
	if len(work) != 2 {
		t.Fatalf("collected work count = %d, want both same-ID rows from independent stores", len(work))
	}
	if len(refs) != 2 || refs[0] != "city:test-city" || refs[1] != "rig:city" {
		t.Fatalf("store refs = %v, want [city:test-city rig:city]", refs)
	}
}

func TestCollectOpenUnassignedRoutedWorkReportsCanonicalStoreRefs(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "fixture", Path: t.TempDir()}},
	}
	var stderr bytes.Buffer
	collectOpenUnassignedRoutedWork(
		cfg,
		listFailStore{},
		map[string]beads.Store{"fixture": listFailStore{}},
		nil,
		&stderr,
	)
	for _, want := range []string{"city:test-city: List(open)", "rig:fixture: List(open)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want canonical store diagnostic %q", stderr.String(), want)
		}
	}
}

func TestRepairControlDispatcherRoutesSuppressesCrossScopeDemandOnWriteFailure(t *testing.T) {
	maxActive := 1
	cfg := &config.City{Agents: []config.Agent{
		{
			Name:              config.ControlDispatcherAgentName,
			BindingName:       "core",
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MaxActiveSessions: &maxActive,
		},
		{
			Name:              config.ControlDispatcherAgentName,
			BindingName:       "core",
			Dir:               "fixture",
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MaxActiveSessions: &maxActive,
		},
	}}
	work := []beads.Bead{{
		ID: "control-1", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     "core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "rig:fixture",
		},
	}}
	boom := errors.New("route write failed")
	store := routeRepairUpdateFailStore{Store: beads.NewMemStore(), err: boom}
	var stderr bytes.Buffer

	repairControlDispatcherRoutesForStoreScope(
		t.Name(),
		cfg,
		work,
		[]beads.Store{store},
		[]string{"fixture"},
		&stderr,
	)
	if got := work[0].Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("in-memory gc.routed_to = %q, want suppressed until durable repair succeeds", got)
	}
	if !strings.Contains(stderr.String(), boom.Error()) {
		t.Fatalf("stderr = %q, want write failure", stderr.String())
	}
}

func TestRepairControlDispatcherRoutesClearsFallbackMarkerFromCanonicalRoute(t *testing.T) {
	maxActive := 1
	cfg := &config.City{Agents: []config.Agent{{
		Name:              config.ControlDispatcherAgentName,
		BindingName:       "core",
		Dir:               "fixture",
		StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
		MaxActiveSessions: &maxActive,
	}}}
	const controlID = "control-1"
	store := &routeRepairCountingStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: controlID, Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:                      beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:                  "fixture/core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey:              "rig:fixture",
			beadmeta.ControlDispatcherFallbackMetadataKey: "fixture/core.control-dispatcher->core.control-dispatcher",
		},
	}}, nil)}
	work := []beads.Bead{{
		ID: controlID, Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:                      beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:                  "fixture/core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey:              "rig:fixture",
			beadmeta.ControlDispatcherFallbackMetadataKey: "fixture/core.control-dispatcher->core.control-dispatcher",
		},
	}}

	repairControlDispatcherRoutesForStoreScope(
		t.Name(),
		cfg,
		work,
		[]beads.Store{store},
		[]string{"rig:fixture"},
		io.Discard,
	)

	if store.updates != 1 {
		t.Fatalf("route repair updates = %d, want one bounded marker cleanup", store.updates)
	}
	persisted, err := store.Get(controlID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if got := persisted.Metadata[beadmeta.RoutedToMetadataKey]; got != "fixture/core.control-dispatcher" {
		t.Fatalf("persisted route = %q, want canonical rig route preserved", got)
	}
	if got := persisted.Metadata[beadmeta.ControlDispatcherFallbackMetadataKey]; got != "" {
		t.Fatalf("persisted fallback marker = %q, want cleared", got)
	}
	if got := work[0].Metadata[beadmeta.ControlDispatcherFallbackMetadataKey]; got != "" {
		t.Fatalf("snapshot fallback marker = %q, want cleared", got)
	}
}

func TestRepairControlDispatcherRoutesBoundsUpgradeWritesPerTick(t *testing.T) {
	maxActive := 1
	cfg := &config.City{Agents: []config.Agent{{
		Name:              config.ControlDispatcherAgentName,
		BindingName:       "core",
		Dir:               "fixture",
		StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
		MaxActiveSessions: &maxActive,
	}}}
	count := controlDispatcherRouteRepairLimitPerTick + 1
	work := make([]beads.Bead, 0, count)
	stores := make([]beads.Store, 0, count)
	refs := make([]string, 0, count)
	seed := make([]beads.Bead, 0, count)
	for i := 0; i < count; i++ {
		bead := beads.Bead{
			ID: fmt.Sprintf("control-%02d", i), Type: "task", Status: "open",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
				beadmeta.RoutedToMetadataKey:     "core.control-dispatcher",
				beadmeta.RootStoreRefMetadataKey: "rig:fixture",
			},
		}
		work = append(work, bead)
		seed = append(seed, beads.Bead{
			ID: bead.ID, Type: bead.Type, Status: bead.Status,
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
				beadmeta.RoutedToMetadataKey:     "core.control-dispatcher",
				beadmeta.RootStoreRefMetadataKey: "rig:fixture",
			},
		})
		refs = append(refs, "fixture")
	}
	store := &routeRepairCountingStore{Store: beads.NewMemStoreFrom(1, seed, nil)}
	for range count {
		stores = append(stores, store)
	}

	repairControlDispatcherRoutesForStoreScope(t.Name(), cfg, work, stores, refs, io.Discard)
	if store.updates != controlDispatcherRouteRepairLimitPerTick {
		t.Fatalf("route repair updates = %d, want bounded %d", store.updates, controlDispatcherRouteRepairLimitPerTick)
	}
	deferredIndex := -1
	for i := range work {
		if work[i].Metadata["gc.routed_to"] == "" {
			deferredIndex = i
			break
		}
	}
	if deferredIndex < 0 {
		t.Fatal("expected one deferred snapshot route")
	}
	persisted, err := store.Get(work[deferredIndex].ID)
	if err != nil {
		t.Fatalf("get deferred control: %v", err)
	}
	if got := persisted.Metadata["gc.routed_to"]; got != "core.control-dispatcher" {
		t.Fatalf("deferred durable route = %q, want unchanged for later retry", got)
	}
}

func TestRepairControlDispatcherRoutesCursorIsIndependentAcrossCities(t *testing.T) {
	const cityADomain = "/cities/alpha"
	const cityBDomain = "/cities/beta"
	for _, domain := range []string{cityADomain, cityBDomain} {
		key := controlDispatcherRouteRepairDomainKey(domain)
		controlDispatcherRouteRepairCursors.Delete(key)
		t.Cleanup(func() { controlDispatcherRouteRepairCursors.Delete(key) })
	}

	maxActive := 1
	cfg := &config.City{Agents: []config.Agent{{
		Name:              config.ControlDispatcherAgentName,
		BindingName:       "core",
		Dir:               "fixture",
		StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
		MaxActiveSessions: &maxActive,
	}}}

	const count = 2 * controlDispatcherRouteRepairLimitPerTick
	seed := make([]beads.Bead, 0, count)
	failIDs := make(map[string]bool, controlDispatcherRouteRepairLimitPerTick)
	for i := range count {
		id := fmt.Sprintf("control-%02d", i)
		seed = append(seed, beads.Bead{
			ID: id, Type: "task", Status: "open",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
				beadmeta.RoutedToMetadataKey:     "core.control-dispatcher",
				beadmeta.RootStoreRefMetadataKey: "rig:fixture",
			},
		})
		if i < controlDispatcherRouteRepairLimitPerTick {
			failIDs[id] = true
		}
	}
	cityAStore := &routeRepairSelectiveFailStore{
		Store:   beads.NewMemStoreFrom(1, seed, nil),
		failIDs: failIDs,
	}
	cityBSeed := make([]beads.Bead, 0, count)
	for i := range count {
		cityBSeed = append(cityBSeed, beads.Bead{
			ID: fmt.Sprintf("control-%02d", i), Type: "task", Status: "open",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
				beadmeta.RoutedToMetadataKey:     "core.control-dispatcher",
				beadmeta.RootStoreRefMetadataKey: "rig:fixture",
			},
		})
	}
	cityBStore := &routeRepairSelectiveFailStore{
		Store:   beads.NewMemStoreFrom(1, cityBSeed, nil),
		failIDs: failIDs,
	}

	repairPass := func(domain string, store beads.Store, count int) {
		work := make([]beads.Bead, 0, count)
		stores := make([]beads.Store, 0, count)
		refs := make([]string, 0, count)
		for i := range count {
			bead, err := store.Get(fmt.Sprintf("control-%02d", i))
			if err != nil {
				t.Fatalf("load control %d: %v", i, err)
			}
			work = append(work, bead)
			stores = append(stores, store)
			refs = append(refs, "fixture")
		}
		repairControlDispatcherRoutesForStoreScope(domain, cfg, work, stores, refs, io.Discard)
	}

	for range 3 {
		repairPass(cityADomain, cityAStore, count)
		repairPass(cityBDomain, cityBStore, count)
	}
	for i := controlDispatcherRouteRepairLimitPerTick; i < count; i++ {
		stored, err := cityAStore.Get(fmt.Sprintf("control-%02d", i))
		if err != nil {
			t.Fatalf("get later control %d: %v", i, err)
		}
		if got := stored.Metadata[beadmeta.RoutedToMetadataKey]; got != "fixture/core.control-dispatcher" {
			t.Fatalf("later control %d route = %q, want repaired despite interleaved city", i, got)
		}
	}
}

// TestOpenControlDispatcherDemandHonorsBareLegacyRoute guards the upgrade gap:
// control beads created by pre-1.3 builds route to the binding-stripped bare
// name ("control-dispatcher"), not the qualified "core.control-dispatcher".
// In-flight work persisted across the upgrade must still scale the qualified
// dispatcher, and demand must be keyed by its qualified template name so the
// scaler matches it.
func TestOpenControlDispatcherDemandHonorsBareLegacyRoute(t *testing.T) {
	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              config.ControlDispatcherAgentName,
			BindingName:       "core",
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MaxActiveSessions: &maxActive,
		}},
	}
	work := []beads.Bead{{
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":      "retry",
			"gc.routed_to": config.ControlDispatcherAgentName, // bare, pre-1.3 route
		},
	}}
	demand := openControlDispatcherDemand(cfg, work)
	if !demand["core.control-dispatcher"] {
		t.Fatalf("openControlDispatcherDemand = %v, want demand keyed by qualified name from bare route", demand)
	}
}

// TestBuildDesiredState_ScaleCheckPartialPoolBlocksNewCreates verifies that
// when the demand store returns a partial result for a pool agent:
//   - poolScaleCheckPartialTemplates is set for the affected template
//   - existing active/awake session beads are retained in desired state
//   - no new session creates are added beyond the existing beads
//   - retainScaleCheckPartialPoolDesired counts only active/awake beads (not creating)
//
// This exercises Changes 1+2 from ga-0xljyj.
func TestBuildDesiredState_ScaleCheckPartialPoolBlocksNewCreates(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "echo",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
		}},
	}

	makeSessionBead := func(id, sessionName, state, slot string) beads.Bead {
		return beads.Bead{
			ID:     id,
			Title:  "worker",
			Type:   sessionBeadType,
			Status: "open",
			Labels: []string{sessionBeadLabel, "template:worker"},
			Metadata: map[string]string{
				"session_name":         sessionName,
				"template":             "worker",
				"agent_name":           "worker",
				"pool_slot":            slot,
				poolManagedMetadataKey: boolMetadata(true),
				"state":                state,
			},
		}
	}

	t.Run("one active bead retained, no new creates", func(t *testing.T) {
		store := &controllerDemandPartialStore{MemStore: beads.NewMemStore()}
		activeSession := makeSessionBead("session-worker-active", "worker-active-1", "active", "1")

		var stderr strings.Builder
		result := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(),
			cfg, runtime.NewFake(), store, nil,
			newSessionBeadSnapshot([]beads.Bead{activeSession}),
			nil, &stderr,
		)

		if !result.PoolScaleCheckPartialTemplates["worker"] {
			t.Fatalf("PoolScaleCheckPartialTemplates[worker] = false, want true; stderr=%s", stderr.String())
		}
		if _, ok := result.State["worker-active-1"]; !ok {
			t.Fatalf("active session not retained in desired state: keys=%v stderr=%s", mapKeys(result.State), stderr.String())
		}
		workerEntries := 0
		for _, tp := range result.State {
			if tp.TemplateName == "worker" {
				workerEntries++
			}
		}
		if workerEntries != 1 {
			t.Fatalf("desired state has %d worker entries, want exactly 1; keys=%v", workerEntries, mapKeys(result.State))
		}

		snapshot := newSessionBeadSnapshot([]beads.Bead{activeSession})
		poolDesired := retainScaleCheckPartialPoolDesired(
			cfg,
			PoolDesiredCounts(ComputePoolDesiredStates(cfg, nil, snapshot.OpenInfos(), result.ScaleCheckCounts)),
			snapshot,
			result.PoolScaleCheckPartialTemplates,
		)
		if got := poolDesired["worker"]; got != 1 {
			t.Fatalf("poolDesired[worker] = %d, want 1 (confirmed-alive active bead)", got)
		}
	})

	// Criterion #3 (ga-4qbgqf.1): awake sessions are also retained, not just active.
	t.Run("one awake bead retained, no new creates", func(t *testing.T) {
		store := &controllerDemandPartialStore{MemStore: beads.NewMemStore()}
		awakeSession := makeSessionBead("session-worker-awake", "worker-awake-1", "awake", "1")

		var stderr strings.Builder
		result := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(),
			cfg, runtime.NewFake(), store, nil,
			newSessionBeadSnapshot([]beads.Bead{awakeSession}),
			nil, &stderr,
		)

		if !result.PoolScaleCheckPartialTemplates["worker"] {
			t.Fatalf("PoolScaleCheckPartialTemplates[worker] = false, want true; stderr=%s", stderr.String())
		}
		if _, ok := result.State["worker-awake-1"]; !ok {
			t.Fatalf("awake session not retained in desired state: keys=%v stderr=%s", mapKeys(result.State), stderr.String())
		}
		workerEntries := 0
		for _, tp := range result.State {
			if tp.TemplateName == "worker" {
				workerEntries++
			}
		}
		if workerEntries != 1 {
			t.Fatalf("desired state has %d worker entries, want exactly 1; keys=%v", workerEntries, mapKeys(result.State))
		}

		snapshot := newSessionBeadSnapshot([]beads.Bead{awakeSession})
		poolDesired := retainScaleCheckPartialPoolDesired(
			cfg,
			PoolDesiredCounts(ComputePoolDesiredStates(cfg, nil, snapshot.OpenInfos(), result.ScaleCheckCounts)),
			snapshot,
			result.PoolScaleCheckPartialTemplates,
		)
		if got := poolDesired["worker"]; got != 1 {
			t.Fatalf("poolDesired[worker] = %d, want 1 (confirmed-alive awake bead)", got)
		}
	})

	t.Run("zero existing beads yields zero creates", func(t *testing.T) {
		store := &controllerDemandPartialStore{MemStore: beads.NewMemStore()}

		var stderr strings.Builder
		result := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(),
			cfg, runtime.NewFake(), store, nil,
			newSessionBeadSnapshot([]beads.Bead{}),
			nil, &stderr,
		)

		if !result.PoolScaleCheckPartialTemplates["worker"] {
			t.Fatalf("PoolScaleCheckPartialTemplates[worker] = false, want true; stderr=%s", stderr.String())
		}
		for _, tp := range result.State {
			if tp.TemplateName == "worker" {
				t.Fatalf("unexpected worker entry in desired state during partial read with no existing sessions: keys=%v", mapKeys(result.State))
			}
		}

		snapshot := newSessionBeadSnapshot([]beads.Bead{})
		poolDesired := retainScaleCheckPartialPoolDesired(
			cfg,
			PoolDesiredCounts(ComputePoolDesiredStates(cfg, nil, snapshot.OpenInfos(), result.ScaleCheckCounts)),
			snapshot,
			result.PoolScaleCheckPartialTemplates,
		)
		if got := poolDesired["worker"]; got != 0 {
			t.Fatalf("poolDesired[worker] = %d, want 0 with no existing sessions", got)
		}
	})

	// Change 2 regression: creating beads must be preserved in desired (not
	// drained) but must NOT inflate poolDesired — only active/awake count.
	t.Run("creating bead preserved but not counted in poolDesired", func(t *testing.T) {
		store := &controllerDemandPartialStore{MemStore: beads.NewMemStore()}
		activeSession := makeSessionBead("session-worker-active", "worker-active-1", "active", "1")
		creatingSession := makeSessionBead("session-worker-creating", "worker-creating-2", "creating", "2")

		var stderr strings.Builder
		result := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(),
			cfg, runtime.NewFake(), store, nil,
			newSessionBeadSnapshot([]beads.Bead{activeSession, creatingSession}),
			nil, &stderr,
		)

		if !result.PoolScaleCheckPartialTemplates["worker"] {
			t.Fatalf("PoolScaleCheckPartialTemplates[worker] = false, want true; stderr=%s", stderr.String())
		}
		if _, ok := result.State["worker-active-1"]; !ok {
			t.Fatalf("active session not retained: keys=%v stderr=%s", mapKeys(result.State), stderr.String())
		}
		if _, ok := result.State["worker-creating-2"]; !ok {
			t.Fatalf("creating session not preserved in desired state (would allow premature drain): keys=%v stderr=%s", mapKeys(result.State), stderr.String())
		}

		snapshot := newSessionBeadSnapshot([]beads.Bead{activeSession, creatingSession})
		poolDesired := retainScaleCheckPartialPoolDesired(
			cfg,
			PoolDesiredCounts(ComputePoolDesiredStates(cfg, nil, snapshot.OpenInfos(), result.ScaleCheckCounts)),
			snapshot,
			result.PoolScaleCheckPartialTemplates,
		)
		// Only the active bead counts; creating must not inflate poolDesired.
		if got := poolDesired["worker"]; got != 1 {
			t.Fatalf("poolDesired[worker] = %d, want 1 (creating bead must not be counted as wake demand)", got)
		}
	})

	// Criterion #5 (ga-4qbgqf.1): the narrow alive-on-partial guard for pool creates
	// allows stale creating beads to roll back within the partial tick itself.
	// Only in-flight creates holding an active pending_create_claim lease are
	// shielded; expired/cleared creates (no lease) drain immediately.
	t.Run("stale creating bead rolls back during partial tick", func(t *testing.T) {
		partialStore := &controllerDemandPartialStore{MemStore: beads.NewMemStore()}
		staleSession := makeSessionBead("session-worker-stale", "worker-stale-4", "creating", "4")
		snapshot := newSessionBeadSnapshot([]beads.Bead{staleSession})

		// Partial tick: the narrow pool-create guard (poolPartialCreate) does NOT
		// shield a stale creating bead (no pending_create_claim), so it rolls back now.
		var pStderr strings.Builder
		partialResult := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(),
			cfg, runtime.NewFake(), partialStore, nil,
			snapshot, nil, &pStderr,
		)
		if !partialResult.PoolScaleCheckPartialTemplates["worker"] {
			t.Fatalf("partial tick: PoolScaleCheckPartialTemplates[worker] = false; stderr=%s", pStderr.String())
		}
		if _, ok := partialResult.State["worker-stale-4"]; ok {
			t.Fatalf("partial tick: stale creating bead unexpectedly retained in State; narrow guard must allow rollback; keys=%v stderr=%s", mapKeys(partialResult.State), pStderr.String())
		}
	})

	// Criterion #6 (ga-4qbgqf.3): fresh in-flight creates (pending_create_claim=true)
	// are retained in desired state and in the retained count during a partial tick.
	// poolPartialAlive is true via isPendingPoolCreateInfo, so the narrow guard keeps them.
	t.Run("fresh pending_create_claim creating bead retained during partial tick", func(t *testing.T) {
		partialStore := &controllerDemandPartialStore{MemStore: beads.NewMemStore()}
		freshCreate := beads.Bead{
			ID:     "session-worker-fresh",
			Title:  "worker",
			Type:   sessionBeadType,
			Status: "open",
			Labels: []string{sessionBeadLabel, "template:worker"},
			Metadata: map[string]string{
				"session_name":         "worker-fresh-3",
				"template":             "worker",
				"agent_name":           "worker",
				"pool_slot":            "3",
				poolManagedMetadataKey: boolMetadata(true),
				"state":                "creating",
				"pending_create_claim": boolMetadata(true),
			},
		}
		snapshot := newSessionBeadSnapshot([]beads.Bead{freshCreate})

		var pStderr strings.Builder
		partialResult := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(),
			cfg, runtime.NewFake(), partialStore, nil,
			snapshot, nil, &pStderr,
		)
		if !partialResult.PoolScaleCheckPartialTemplates["worker"] {
			t.Fatalf("partial tick: PoolScaleCheckPartialTemplates[worker] = false; stderr=%s", pStderr.String())
		}
		if _, ok := partialResult.State["worker-fresh-3"]; !ok {
			t.Fatalf("partial tick: fresh pending_create_claim=true bead absent from State; poolPartialAlive must retain it; keys=%v stderr=%s", mapKeys(partialResult.State), pStderr.String())
		}
		poolDesired := retainScaleCheckPartialPoolDesired(
			cfg,
			PoolDesiredCounts(ComputePoolDesiredStates(cfg, nil, snapshot.OpenInfos(), partialResult.ScaleCheckCounts)),
			snapshot,
			partialResult.PoolScaleCheckPartialTemplates,
		)
		if got := poolDesired["worker"]; got != 1 {
			t.Fatalf("poolDesired[worker] = %d, want 1 (fresh pending_create_claim=true create counts as retained capacity)", got)
		}
	})

	// Change 3 (ga-btv7tm): draining/drained/archived sessions must NOT be
	// preserved in desired state during a partial tick. Preserving them would
	// interrupt an in-progress drain lifecycle.
	t.Run("draining/drained/archived NOT preserved during partial tick", func(t *testing.T) {
		for _, state := range []string{"draining", "drained", "archived"} {
			state := state
			t.Run(state, func(t *testing.T) {
				store := &controllerDemandPartialStore{MemStore: beads.NewMemStore()}
				b := makeSessionBead("session-worker-"+state, "worker-"+state+"-1", state, "1")
				var pStderr strings.Builder
				result := buildDesiredStateWithSessionBeads(
					"test-city", cityPath, time.Now().UTC(),
					cfg, runtime.NewFake(), store, nil,
					newSessionBeadSnapshot([]beads.Bead{b}),
					nil, &pStderr,
				)
				if _, ok := result.State["worker-"+state+"-1"]; ok {
					t.Fatalf("state=%s bead must NOT be preserved in desired during partial tick (would interrupt drain); keys=%v stderr=%s",
						state, mapKeys(result.State), pStderr.String())
				}
			})
		}
	})
}

func TestBuildDesiredState_ProviderRedBlocksNewPoolSessionCreate(t *testing.T) {
	writeHealthFile := func(t *testing.T, cityPath, status string) {
		t.Helper()
		dir := filepath.Join(cityPath, ".gc", "cache")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		probedAt := float64(time.Now().UnixNano()) / 1e9
		content := fmt.Sprintf(`{"providers":[{"provider":"claude","status":%q,"probed_at":%f}]}`, status, probedAt)
		if err := os.WriteFile(filepath.Join(dir, "provider-health.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	makeCfg := func() *config.City {
		return &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents: []config.Agent{{
				Name:              "worker",
				Provider:          "claude",
				StartCommand:      "echo",
				ScaleCheck:        "printf 3",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(5),
			}},
		}
	}

	makeSessionBead := func(id, sessionName, state, slot string) beads.Bead {
		return beads.Bead{
			ID:     id,
			Title:  "worker",
			Type:   sessionBeadType,
			Status: "open",
			Labels: []string{sessionBeadLabel, "template:worker"},
			Metadata: map[string]string{
				"session_name":         sessionName,
				"template":             "worker",
				"agent_name":           "worker",
				"pool_slot":            slot,
				poolManagedMetadataKey: boolMetadata(true),
				"state":                state,
			},
		}
	}

	// Scenario A: registry red — no new creates for pool agent.
	t.Run("registry red blocks new creates", func(t *testing.T) {
		cityPath := t.TempDir()
		writeHealthFile(t, cityPath, "unhealthy")
		cfg := makeCfg()
		store := beads.NewMemStore()
		var stderr strings.Builder
		result := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(),
			cfg, runtime.NewFake(), store, nil,
			newSessionBeadSnapshot(nil),
			nil, &stderr,
		)
		for _, tp := range result.State {
			if tp.TemplateName == "worker" {
				t.Fatalf("expected no worker creates when provider is red; got entry %q; stderr=%s", tp.SessionName, stderr.String())
			}
		}
		if !strings.Contains(stderr.String(), "provider red, fresh create blocked") {
			t.Fatalf("expected 'provider red, fresh create blocked' in stderr; stderr=%s", stderr.String())
		}
	})

	// Scenario B: registry absent — fail-open, creates proceed normally.
	t.Run("registry absent fails open", func(t *testing.T) {
		cityPath := t.TempDir()
		// no provider-health.json written
		cfg := makeCfg()
		store := beads.NewMemStore()
		var stderr strings.Builder
		result := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(),
			cfg, runtime.NewFake(), store, nil,
			newSessionBeadSnapshot(nil),
			nil, &stderr,
		)
		workerCreates := 0
		for _, tp := range result.State {
			if tp.TemplateName == "worker" {
				workerCreates++
			}
		}
		if workerCreates == 0 {
			t.Fatalf("expected worker creates when registry absent (fail-open); stderr=%s", stderr.String())
		}
	})

	// Scenario C: registry healthy — creates proceed normally.
	t.Run("registry healthy allows create", func(t *testing.T) {
		cityPath := t.TempDir()
		writeHealthFile(t, cityPath, "healthy")
		cfg := makeCfg()
		store := beads.NewMemStore()
		var stderr strings.Builder
		result := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(),
			cfg, runtime.NewFake(), store, nil,
			newSessionBeadSnapshot(nil),
			nil, &stderr,
		)
		workerCreates := 0
		for _, tp := range result.State {
			if tp.TemplateName == "worker" {
				workerCreates++
			}
		}
		if workerCreates == 0 {
			t.Fatalf("expected worker creates when provider is healthy; stderr=%s", stderr.String())
		}
	})

	// Scenario D: red gate does not block reuse of an existing active session;
	// no additional new creates are planned.
	t.Run("red gate does not block reuse of active session", func(t *testing.T) {
		cityPath := t.TempDir()
		writeHealthFile(t, cityPath, "unhealthy")
		cfg := makeCfg()
		active := makeSessionBead("session-worker-1", "worker-1", "active", "1")
		store := beads.NewMemStore()
		var stderr strings.Builder
		result := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(),
			cfg, runtime.NewFake(), store, nil,
			newSessionBeadSnapshot([]beads.Bead{active}),
			nil, &stderr,
		)
		if _, ok := result.State["worker-1"]; !ok {
			t.Fatalf("active session must be reused even when provider is red; keys=%v stderr=%s", mapKeys(result.State), stderr.String())
		}
		workerEntries := 0
		for _, tp := range result.State {
			if tp.TemplateName == "worker" {
				workerEntries++
			}
		}
		// The active session is reused; the red gate blocks any additional creates.
		if workerEntries != 1 {
			t.Fatalf("expected exactly 1 worker entry (reused active); got %d; keys=%v stderr=%s", workerEntries, mapKeys(result.State), stderr.String())
		}
	})
}

// TestBuildDesiredState_NamedAliasHolderSuppressesPoolStandby is the FAITHFUL
// gc-7e40y dual-mayor repro at the buildDesiredState level, mirroring the exact
// live gastown.mayor bead shapes.
//
// The mayor shape: a bound agent (gastown.mayor) that is BOTH a configured
// [[named_session]] (mode=always) AND trips UsesCanonicalSingletonPoolIdentity
// (max_active_sessions=1, no min, no scale_check, no namepool). A live named
// session holds the canonical alias "gastown.mayor". A pool-managed standby
// session already exists, alias-deferred (pool_alias_conflict=gastown.mayor).
//
// EMPIRICAL ROOT (bisected against the live controller, commit dbc702304):
// the dual is DEMANDED by ready, unassigned, routed work (live: gc-78iq with
// gc.routed_to=gastown.mayor). defaultScaleCheckCounts counts that work →
// scaleCheckCounts[gastown.mayor]=1 → computePoolDesiredStates emits one
// tier="new" pool request → realizePoolDesiredSessions reuses the standby bead,
// which then DEFERS on the held canonical alias (recordDeferredNonExpandingPool
// AliasConflict). That standby is added to desired state alongside the named
// alias-holder: two desired sessions for a max=1 agent. The standby can never
// acquire the alias (held by the named session), so it defers forever and,
// with effective_sleep_after_idle=off, never reaps.
//
// The fix must credit the live canonical-alias holder as occupying the
// singleton's one slot, suppressing the redundant pool new-demand. Keyed on
// CANONICAL-ALIAS OWNERSHIP, not isNamedSessionBead — a manual session holding
// NO alias must still let the pool want its instance (see
// TestComputePoolDesiredStates_ManualSessionDoesNotConsumeSingletonNewDemand).
//
// RED before the fix: produces 2 desired entries for gastown.mayor (the named
// holder + the alias-deferred pool standby).
func TestBuildDesiredState_NamedAliasHolderSuppressesPoolStandby(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()

	// Ready, unassigned, routed work — mirrors live gc-78iq.
	if _, err := store.Create(beads.Bead{
		Title:    "routed mayor work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "gastown.mayor"},
	}); err != nil {
		t.Fatal(err)
	}

	// Named-session alias holder for "gastown.mayor" — mirrors live gc-wisp-ak5aoga.
	if _, err := store.Create(beads.Bead{
		Title:  "gastown.mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:gastown.mayor"},
		Metadata: map[string]string{
			"agent_name":                 "gastown.mayor",
			"alias":                      "gastown.mayor",
			"session_name":               "gastown__mayor",
			"session_name_explicit":      "true",
			"state":                      "awake",
			"session_origin":             "named",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "gastown.mayor",
			namedSessionModeMetadata:     "always",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Pool-managed standby, alias-deferred — mirrors live gc-wisp-tig9h8s.
	if _, err := store.Create(beads.Bead{
		Title:  "gastown.mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:gastown.mayor"},
		Metadata: map[string]string{
			"agent_name":                      "gastown.mayor",
			"alias":                           "",
			"session_name":                    "gastown__mayor-gc-wisp-tig9h8s",
			"state":                           "awake",
			"session_origin":                  "ephemeral",
			poolManagedMetadataKey:            boolMetadata(true),
			poolAliasConflictMetadataKey:      "gastown.mayor",
			poolAliasConflictCountMetadataKey: "249",
		},
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "gchq"},
		Agents: []config.Agent{{
			Name:              "mayor",
			BindingName:       "gastown",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "mayor",
			BindingName: "gastown",
			Scope:       "city",
			Mode:        "always",
		}},
	}

	dsResult := buildDesiredState("gchq", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	var mayorEntries []TemplateParams
	for _, tp := range dsResult.State {
		if tp.TemplateName == "gastown.mayor" {
			mayorEntries = append(mayorEntries, tp)
		}
	}
	if len(mayorEntries) != 1 {
		var names []string
		for _, tp := range mayorEntries {
			names = append(names, fmt.Sprintf("%s(alias=%q,named=%q)", tp.SessionName, tp.Alias, tp.ConfiguredNamedIdentity))
		}
		t.Fatalf("gastown.mayor desired entries = %d, want 1 — a live session already holds the canonical alias, so the pool must not demand a redundant standby (gc-7e40y dual). Got: %v", len(mayorEntries), names)
	}
	// The single retained entry must be the named alias-holder, not a deferred
	// pool standby (alias empty because it lost the alias-acquisition race).
	if mayorEntries[0].ConfiguredNamedIdentity != "gastown.mayor" {
		t.Fatalf("retained entry is not the named alias-holder: %+v", mayorEntries[0])
	}
}

// TestBuildDesiredState_AsleepNamedAliasHolderStaysSingle documents the
// gc-7e40y review ruling (mayor): an asleep canonical-singleton holder is NOT
// a live alias holder, so pool demand SHOULD flow and WAKE the existing asleep
// holder — net desired stays exactly 1, no redundant standby spawns.
func TestBuildDesiredState_AsleepNamedAliasHolderStaysSingle(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()

	// Ready, unassigned, routed work — mirrors live gc-78iq.
	if _, err := store.Create(beads.Bead{
		Title:    "routed mayor work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "gastown.mayor"},
	}); err != nil {
		t.Fatal(err)
	}

	// Named-session alias holder for "gastown.mayor" — mirrors live gc-wisp-ak5aoga.
	if _, err := store.Create(beads.Bead{
		Title:  "gastown.mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:gastown.mayor"},
		Metadata: map[string]string{
			"agent_name":                 "gastown.mayor",
			"alias":                      "gastown.mayor",
			"session_name":               "gastown__mayor",
			"session_name_explicit":      "true",
			"state":                      "asleep",
			"session_origin":             "named",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "gastown.mayor",
			namedSessionModeMetadata:     "always",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Pool-managed standby, alias-deferred — mirrors live gc-wisp-tig9h8s.
	if _, err := store.Create(beads.Bead{
		Title:  "gastown.mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:gastown.mayor"},
		Metadata: map[string]string{
			"agent_name":                      "gastown.mayor",
			"alias":                           "",
			"session_name":                    "gastown__mayor-gc-wisp-tig9h8s",
			"state":                           "awake",
			"session_origin":                  "ephemeral",
			poolManagedMetadataKey:            boolMetadata(true),
			poolAliasConflictMetadataKey:      "gastown.mayor",
			poolAliasConflictCountMetadataKey: "249",
		},
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "gchq"},
		Agents: []config.Agent{{
			Name:              "mayor",
			BindingName:       "gastown",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "mayor",
			BindingName: "gastown",
			Scope:       "city",
			Mode:        "always",
		}},
	}

	dsResult := buildDesiredState("gchq", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	var mayorEntries []TemplateParams
	for _, tp := range dsResult.State {
		if tp.TemplateName == "gastown.mayor" {
			mayorEntries = append(mayorEntries, tp)
		}
	}
	if len(mayorEntries) != 1 {
		var names []string
		for _, tp := range mayorEntries {
			names = append(names, fmt.Sprintf("%s(alias=%q,named=%q)", tp.SessionName, tp.Alias, tp.ConfiguredNamedIdentity))
		}
		t.Fatalf("gastown.mayor desired entries = %d, want 1 — a live session already holds the canonical alias, so the pool must not demand a redundant standby (gc-7e40y dual). Got: %v", len(mayorEntries), names)
	}
	// The single retained entry must be the named alias-holder, not a deferred
	// pool standby (alias empty because it lost the alias-acquisition race).
	if mayorEntries[0].ConfiguredNamedIdentity != "gastown.mayor" {
		t.Fatalf("retained entry is not the named alias-holder: %+v", mayorEntries[0])
	}
}

// TestBuildDesiredStateRecordsDemandSubPhases verifies the sub-phase operation
// records emitted inside buildDesiredStateWithSessionBeads (sr-5rz /
// gastownhall/gascity#2463): the aggregate load_demand_snapshot tick phase
// regularly dominates the controller cycle, and these records are what make
// its internal split (collection reads vs demand probes vs scale_check execs)
// attributable from a trace instead of requiring an instrumented rebuild.
func TestBuildDesiredStateRecordsDemandSubPhases(t *testing.T) {
	// The non-tick path passes no trace; the recorder must be nil-safe.
	recordDemandSubPhase(nil, "demand_snapshot.collect_open_session_beads", time.Now(), nil)

	cityDir := t.TempDir()
	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}

	store := beads.NewMemStore()
	sessionSnapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	var stderr strings.Builder
	buildDesiredStateWithSessionBeads(
		"trace-town", cityDir, time.Now().UTC(), &config.City{}, runtime.NewFake(),
		store, nil, sessionSnapshot, cycle, &stderr,
	)

	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	// Sub-phases that must fire on every store-backed build, even with an
	// empty config (the scale/named demand probes only fire with matching
	// agents, so they are intentionally not asserted here).
	want := map[string]bool{
		"demand_snapshot.collect_open_session_beads": false,
		"demand_snapshot.collect_assigned_work":      false,
		"demand_snapshot.collect_unassigned_routed":  false,
		"demand_snapshot.evaluate_pending_pools":     false,
	}
	for i := range records {
		r := &records[i]
		if r.RecordType != TraceRecordOperation || r.SiteCode != TraceSiteDemandSnapshot {
			continue
		}
		name, _ := r.Fields["operation_name"].(string)
		if _, tracked := want[name]; tracked {
			want[name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing demand sub-phase operation record %q", name)
		}
	}
}
