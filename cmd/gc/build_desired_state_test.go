package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type listFailStore struct {
	beads.Store
}

func (s listFailStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list failed")
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
	if bead.Type == sessionBeadType && bead.Metadata["agent_name"] == s.alias {
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
	liveInProgressLists int
	liveOpenMolecules   int
}

func (s *demandListCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Live && query.Status == "in_progress" {
		s.liveInProgressLists++
	}
	if query.Live && query.Status == "open" && query.Type == "molecule" {
		s.liveOpenMolecules++
	}
	return s.Store.List(query)
}

type demandRefreshFailStore struct {
	beads.Store
	failNextGet         bool
	liveInProgressLists int
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
		s.liveInProgressLists++
	}
	return s.Store.List(query)
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
	if len(query) == 0 {
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
	if s.partialInProgress && query.Status == "in_progress" && query.Live {
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

func TestCollectAssignedWorkBeadsUsesCachedReadyReadModel(t *testing.T) {
	backing := &readyFailStore{Store: beads.NewMemStore()}
	handoff, err := backing.Create(beads.Bead{
		Title:    "merge me",
		Type:     "task",
		Status:   "open",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("create handoff bead: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, cache)
	if len(got) != 1 || got[0].ID != handoff.ID {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want [%s]", got, handoff.ID)
	}
	if backing.readyCalls != 0 {
		t.Fatalf("backing Ready calls = %d, want cached demand read", backing.readyCalls)
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
	if backing.liveInProgressLists != 0 {
		t.Fatalf("live in_progress list calls = %d, want cached demand read", backing.liveInProgressLists)
	}
}

func TestCollectAssignedWorkBeadsFallsBackLiveWhenCachedInProgressDirty(t *testing.T) {
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
		t.Fatal("collectAssignedWorkBeads reported partial with successful live fallback")
	}
	if len(got) != 1 || got[0].ID != work.ID || got[0].Status != "in_progress" || got[0].Assignee != "repo/refinery" {
		t.Fatalf("collectAssignedWorkBeads returned %#v, want live in-progress %s", got, work.ID)
	}
	if backing.liveInProgressLists != 1 {
		t.Fatalf("live in_progress list calls = %d, want dirty cache fallback", backing.liveInProgressLists)
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

func TestDefaultScaleCheckCountsUsesCachedReadyReadModel(t *testing.T) {
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
	if got := counts["gascity/workflows.codex-min"]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts = %d, want 1", got)
	}
	if backing.readyCalls != 0 {
		t.Fatalf("backing Ready calls = %d, want cached demand read", backing.readyCalls)
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

func TestDefaultScaleCheckCountsHonorsCachedWriteThroughDependencies(t *testing.T) {
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
			"gc.routed_to": "gascity/workflows.codex-max",
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

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: "gascity/workflows.codex-max",
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts["gascity/workflows.codex-max"]; got != 0 {
		t.Fatalf("defaultScaleCheckCounts = %d, want blocked future work excluded", got)
	}
	if backing.readyCalls != 0 {
		t.Fatalf("backing Ready calls = %d, want cached demand read", backing.readyCalls)
	}
}

func TestDefaultScaleCheckCountsFallsBackWhenCachedEventDepsUnknown(t *testing.T) {
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
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	cache.ApplyEvent("bead.created", []byte(`{"id":"gc-blocked","status":"open","metadata":{"gc.routed_to":"gascity/workflows.codex-max"}}`))

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: "gascity/workflows.codex-max",
		storeKey: "rig:gascity",
		store:    cache,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts["gascity/workflows.codex-max"]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts = %d, want live ready fallback count only", got)
	}
	if backing.readyCalls != 1 {
		t.Fatalf("backing Ready calls = %d, want one live ready fallback", backing.readyCalls)
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

func TestDefaultNamedSessionDemandUsesPartialReadyRows(t *testing.T) {
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
	if !demand["primary"] {
		t.Fatalf("defaultNamedSessionDemand[primary] = false, want survivor row counted")
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

	got, stores, storeRefs, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, nil)
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

	got, stores, storeRefs, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, nil)
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

	got, _, _, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, snapshot)
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

	got, _, _, partial := collectAssignedWorkBeadsWithStores(
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

	got, _, _, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, snapshot)
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

	got, _, _, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, snapshot)
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

	got, stores, storeRefs, partial := collectAssignedWorkBeadsWithStores(
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

	got, stores, storeRefs, partial := collectAssignedWorkBeadsWithStores(
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
name = "import-regression"
provider = "claude"

[[rigs]]
name = "repo"
path = "./repo"

[rigs.imports.gs]
source = "./assets/sidecar"
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

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
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
		bead beads.Bead
		slot int
		err  error
	}
	results := make(chan createResult, 2)
	create := func() {
		bead, slot, err := selectOrCreatePoolSessionBead(newBuildParams(), &cfgAgent, "claude", nil, map[string]bool{}, map[int]bool{})
		results <- createResult{bead: bead, slot: slot, err: err}
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
		if result.bead.ID == "" {
			t.Fatalf("selectOrCreatePoolSessionBead result %d returned empty bead", i+1)
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

	bead, err := createPoolSessionBeadWithGuardedAlias(bp, "claude", "claude-1", 1)
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithGuardedAlias: %v", err)
	}
	if got := bead.Metadata["alias"]; got != "" {
		t.Fatalf("alias = %q, want empty fallback when alias lock setup fails", got)
	}
	if !strings.Contains(stderr.String(), "locking alias \"claude-1\"") || !strings.Contains(stderr.String(), "creating without alias") {
		t.Fatalf("stderr = %q, want alias-lock setup failure and unaliased fallback", stderr.String())
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

func TestBuildDesiredState_OnDemandNamedSession_DefaultRoutedWorkMaterializesNamedSession(t *testing.T) {
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
	if !foundNamed {
		t.Fatal("default routed work should materialize the on-demand named session")
	}
	if foundGeneric {
		t.Fatal("default routed work should not create a parallel generic session for the named template")
	}
	if !dsResult.NamedSessionDemand["mayor"] {
		t.Fatal("NamedSessionDemand should record default routed work for mayor")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_DefaultRoutedTemplateMaterializesSingletonIdentity(t *testing.T) {
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
	for _, tp := range dsResult.State {
		if tp.TemplateName != "worker" {
			continue
		}
		if tp.ConfiguredNamedIdentity == "primary" {
			foundNamed = true
			continue
		}
		t.Fatalf("routed singleton template created generic worker session: %+v", tp)
	}
	if !foundNamed {
		t.Fatal("default routed work should materialize the singleton named identity for worker")
	}
	if !dsResult.NamedSessionDemand["primary"] {
		t.Fatal("NamedSessionDemand should record singleton identity demand")
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

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
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
		t.Fatalf("db desired slots = %d, want 1", dbSlots)
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

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
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
		t.Fatalf("db desired slots = %d, want 1", dbSlots)
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
		PoolDesiredCounts(ComputePoolDesiredStates(cfg, nil, snapshot.Open(), result.ScaleCheckCounts)),
		snapshot,
		result.PoolScaleCheckPartialTemplates,
	)
	if got := poolDesired["worker"]; got != 0 {
		t.Fatalf("poolDesired[worker] = %d, want dormant preservation without wake demand", got)
	}
}

func TestBuildDesiredState_NamedScaleCheckPartialDoesNotRetainGenericPoolSession(t *testing.T) {
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

	if result.StoreQueryPartial {
		t.Fatalf("StoreQueryPartial = true, want false for scoped named scale_check failure; stderr=%s", stderr.String())
	}
	if !result.ScaleCheckPartialTemplates["worker"] {
		t.Fatalf("ScaleCheckPartialTemplates[worker] = false, want named-session partial recorded; templates=%v stderr=%s", result.ScaleCheckPartialTemplates, stderr.String())
	}
	if result.PoolScaleCheckPartialTemplates["worker"] {
		t.Fatalf("PoolScaleCheckPartialTemplates[worker] = true, want false for named-session partial; templates=%v", result.PoolScaleCheckPartialTemplates)
	}
	if !result.NamedScaleCheckPartialTemplates["worker"] {
		t.Fatalf("NamedScaleCheckPartialTemplates[worker] = false, want true; templates=%v", result.NamedScaleCheckPartialTemplates)
	}
	if _, ok := result.State["worker-bd-123"]; ok {
		t.Fatalf("generic pool session retained by named-session partial: keys=%v stderr=%s", mapKeys(result.State), stderr.String())
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

func TestClaimPoolSlot_PreservesStampedOutOfBoundsLiveIdentity(t *testing.T) {
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
	used := map[int]bool{}
	if slot := claimPoolSlot(cfgAgent, bead, used); slot != 7 {
		t.Fatalf("claimPoolSlot(stamped live slot) = %d, want 7", slot)
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
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
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
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
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
			Name:      "payments",
			Path:      rigPath,
			Suspended: true,
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
	snapshot.add(drained)
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
	if result.Metadata["pool_slot"] != "1" {
		t.Fatalf("pool_slot after previous create error = %q, want 1", result.Metadata["pool_slot"])
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
	startedAt, err := time.Parse(time.RFC3339, result.Metadata["pending_create_started_at"])
	if err != nil {
		t.Fatalf("parse pending_create_started_at %q: %v", result.Metadata["pending_create_started_at"], err)
	}
	if startedAt.Before(beforeCreate) {
		t.Fatalf("pending_create_started_at = %s, want current create time after %s", startedAt, beforeCreate)
	}
	if !startedAt.After(oldBeacon.Add(staleCreatingStateTimeout)) {
		t.Fatalf("pending_create_started_at = %s, want independent from stale beacon %s", startedAt, oldBeacon)
	}
	result.CreatedAt = oldBeacon
	if staleCreatingState(result, &clock.Fake{Time: startedAt.Add(30 * time.Second)}) {
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
	snapshot.add(drained)
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, slot, err := selectOrCreatePoolSessionBead(bp, &cfgAgent, "claude", &drained, map[string]bool{}, map[int]bool{})
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
	snapshot.add(drained)
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
	if got := result.Metadata["agent_name"]; got != "claude-1" {
		t.Fatalf("dependency agent_name = %q, want claude-1", got)
	}
	if got := result.Metadata["alias"]; got != "claude-1" {
		t.Fatalf("dependency alias = %q, want claude-1", got)
	}
	if got := result.Metadata["pool_slot"]; got != "1" {
		t.Fatalf("dependency pool_slot = %q, want 1", got)
	}
	if got := result.Title; got != "claude-1" {
		t.Fatalf("dependency title = %q, want claude-1", got)
	}
	if !containsString(result.Labels, "agent:claude-1") {
		t.Fatalf("dependency labels = %#v, want concrete slot agent label", result.Labels)
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
	if got := result.Metadata["agent_name"]; got != "claude-1" {
		t.Fatalf("dependency agent_name = %q, want claude-1", got)
	}
	if got := result.Metadata["alias"]; got != "" {
		t.Fatalf("dependency alias = %q, want deferred until alias guard accepts it", got)
	}
	if got := result.Metadata["pool_slot"]; got != "1" {
		t.Fatalf("dependency pool_slot = %q, want 1", got)
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
	snapshot.add(awake)
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
	snapshot.add(assigned)
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
