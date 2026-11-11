package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
)

// fakeWorktreeDriftGit is a configurable fake for the worktreeDriftGitProbe
// interface.
type fakeWorktreeDriftGit struct {
	isRepo              bool
	currentBranch       string
	currentBranchErr    error
	hasUncommitted      bool
	defaultBranch       string
	defaultBranchErr    error
	ahead               int
	behind              int
	aheadBehindErr      error
	aheadBehindRefSeen  string
	inProgressOperation bool
}

func (f *fakeWorktreeDriftGit) IsRepo() bool { return f.isRepo }

func (f *fakeWorktreeDriftGit) CurrentBranch() (string, error) {
	return f.currentBranch, f.currentBranchErr
}

func (f *fakeWorktreeDriftGit) HasUncommittedWork() bool { return f.hasUncommitted }

func (f *fakeWorktreeDriftGit) DefaultBranch() (string, error) {
	return f.defaultBranch, f.defaultBranchErr
}

func (f *fakeWorktreeDriftGit) AheadBehindRef(ref string) (int, int, error) {
	f.aheadBehindRefSeen = ref
	return f.ahead, f.behind, f.aheadBehindErr
}

func (f *fakeWorktreeDriftGit) InProgressOperation() bool { return f.inProgressOperation }

// fakeDriftRecorder captures every event Recorded, in order.
type fakeDriftRecorder struct {
	events []events.Event
}

func (f *fakeDriftRecorder) Record(e events.Event) {
	f.events = append(f.events, e)
}

func setupWorktreeDriftPatrolTest(t *testing.T) (cityPath string, store beads.Store) {
	t.Helper()
	cityPath = t.TempDir()
	builderWTPath := filepath.Join(cityPath, ".gc", "worktrees", "ga-rig", "builder")
	if err := os.MkdirAll(builderWTPath, 0o755); err != nil {
		t.Fatalf("creating builder worktree: %v", err)
	}
	store = beads.NewMemStore()
	return
}

func commitClassAgentConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Agents: []config.Agent{
			{Name: "builder", Dir: "ga-rig", PreStart: []string{"scripts/worktree-setup.sh . . base --sync --freshen-commit"}},
		},
	}
}

func resetClassAgentConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Agents: []config.Agent{
			{Name: "builder", Dir: "ga-rig", PreStart: []string{"scripts/worktree-setup.sh . . base --sync --reset-main"}},
		},
	}
}

const testDriftThreshold = time.Hour

func TestPatrolCommitClassWorktreeDrift_NilConfig(t *testing.T) {
	_, store := setupWorktreeDriftPatrolTest(t)
	fired := patrolCommitClassWorktreeDrift(t.TempDir(), nil, beads.WorkStore{Store: store}, testDriftThreshold, nil, time.Now(), nil)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 for nil config", fired)
	}
}

func TestPatrolCommitClassWorktreeDrift_NilStore(t *testing.T) {
	cityPath, _ := setupWorktreeDriftPatrolTest(t)
	cfg := commitClassAgentConfig()
	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{}, testDriftThreshold, nil, time.Now(), nil)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 for nil store", fired)
	}
}

func TestPatrolCommitClassWorktreeDrift_ZeroThreshold(t *testing.T) {
	cityPath, store := setupWorktreeDriftPatrolTest(t)
	cfg := commitClassAgentConfig()

	orig := newWorktreeDriftGitProbe
	defer func() { newWorktreeDriftGitProbe = orig }()
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe {
		return &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", behind: 5}
	}

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, 0, nil, time.Now(), nil)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 when threshold disables the feature", fired)
	}
}

func TestPatrolCommitClassWorktreeDrift_SkipsNonCommitClassAgent(t *testing.T) {
	cityPath, store := setupWorktreeDriftPatrolTest(t)
	cfg := resetClassAgentConfig()

	called := false
	orig := newWorktreeDriftGitProbe
	defer func() { newWorktreeDriftGitProbe = orig }()
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe {
		called = true
		return &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", behind: 5}
	}

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, nil, time.Now(), nil)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 for reset-class agent", fired)
	}
	if called {
		t.Error("git probe constructed for a reset-class agent, want skipped before probing")
	}
	beadsInStore, err := store.List(beads.ListQuery{Type: worktreeDriftBeadType, AllowScan: true})
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(beadsInStore) != 0 {
		t.Errorf("got %d worktree_drift beads, want 0 for reset-class agent", len(beadsInStore))
	}
}

func TestPatrolCommitClassWorktreeDrift_SkipsMissingWorktree(t *testing.T) {
	cityPath := t.TempDir() // no .gc/worktrees/ga-rig/builder created
	store := beads.NewMemStore()
	cfg := commitClassAgentConfig()

	called := false
	orig := newWorktreeDriftGitProbe
	defer func() { newWorktreeDriftGitProbe = orig }()
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe {
		called = true
		return &fakeWorktreeDriftGit{isRepo: true}
	}

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, nil, time.Now(), nil)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 when worktree was never materialized", fired)
	}
	if called {
		t.Error("git probe constructed for a nonexistent worktree, want skipped before probing")
	}
}

func TestPatrolCommitClassWorktreeDrift_SkipsInProgressOperation(t *testing.T) {
	cityPath, store := setupWorktreeDriftPatrolTest(t)
	cfg := commitClassAgentConfig()

	orig := newWorktreeDriftGitProbe
	defer func() { newWorktreeDriftGitProbe = orig }()
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe {
		return &fakeWorktreeDriftGit{
			isRepo:              true,
			currentBranch:       "builder",
			behind:              5,
			inProgressOperation: true,
		}
	}

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, nil, time.Now(), nil)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 when a rebase/merge/cherry-pick is in progress", fired)
	}
	beadsInStore, err := store.List(beads.ListQuery{Type: worktreeDriftBeadType, AllowScan: true})
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(beadsInStore) != 0 {
		t.Errorf("got %d worktree_drift beads, want 0: an ambiguous TOCTOU state must not start an observation window", len(beadsInStore))
	}
}

func TestPatrolCommitClassWorktreeDrift_SkipsUncommittedWork(t *testing.T) {
	cityPath, store := setupWorktreeDriftPatrolTest(t)
	cfg := commitClassAgentConfig()

	orig := newWorktreeDriftGitProbe
	defer func() { newWorktreeDriftGitProbe = orig }()
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe {
		return &fakeWorktreeDriftGit{
			isRepo:         true,
			currentBranch:  "builder",
			behind:         5,
			hasUncommitted: true,
		}
	}

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, nil, time.Now(), nil)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 when worktree has uncommitted work (grace window)", fired)
	}
	beadsInStore, err := store.List(beads.ListQuery{Type: worktreeDriftBeadType, AllowScan: true})
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(beadsInStore) != 0 {
		t.Errorf("got %d worktree_drift beads, want 0: uncommitted work must not start an observation window", len(beadsInStore))
	}
}

func TestPatrolCommitClassWorktreeDrift_SkipsUnresolvableComparison(t *testing.T) {
	cityPath, store := setupWorktreeDriftPatrolTest(t)
	cfg := commitClassAgentConfig()

	orig := newWorktreeDriftGitProbe
	defer func() { newWorktreeDriftGitProbe = orig }()
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe {
		return &fakeWorktreeDriftGit{
			isRepo:         true,
			currentBranch:  "builder",
			aheadBehindErr: errFakeResolver,
		}
	}

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, nil, time.Now(), nil)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 when origin/<default> is not locally resolvable", fired)
	}
}

func TestPatrolCommitClassWorktreeDrift_DetachedHeadIsDrift(t *testing.T) {
	cityPath, store := setupWorktreeDriftPatrolTest(t)
	cfg := commitClassAgentConfig()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	orig := newWorktreeDriftGitProbe
	defer func() { newWorktreeDriftGitProbe = orig }()
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe {
		return &fakeWorktreeDriftGit{
			isRepo:        true,
			currentBranch: "HEAD", // detached
			defaultBranch: "main",
			ahead:         0,
			behind:        0, // even with 0 behind, detached HEAD is itself drift
		}
	}

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, nil, now, nil)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 on first observation (under threshold)", fired)
	}
	beadsInStore, err := store.List(beads.ListQuery{Type: worktreeDriftBeadType, AllowScan: true})
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(beadsInStore) != 1 {
		t.Fatalf("got %d worktree_drift beads, want 1 tracking bead for detached HEAD", len(beadsInStore))
	}
	if got := beadsInStore[0].Metadata[worktreeDriftMetaFirstObservedAt]; got != now.Format(time.RFC3339) {
		t.Errorf("first_observed_at = %q, want %q", got, now.Format(time.RFC3339))
	}
}

func TestPatrolCommitClassWorktreeDrift_UsesOriginPlusDefaultBranchForComparison(t *testing.T) {
	cityPath, store := setupWorktreeDriftPatrolTest(t)
	cfg := commitClassAgentConfig()

	var fake *fakeWorktreeDriftGit
	orig := newWorktreeDriftGitProbe
	defer func() { newWorktreeDriftGitProbe = orig }()
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe {
		fake = &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "develop", behind: 0}
		return fake
	}

	patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, nil, time.Now(), nil)
	if fake.aheadBehindRefSeen != "origin/develop" {
		t.Errorf("AheadBehindRef called with %q, want %q", fake.aheadBehindRefSeen, "origin/develop")
	}
}

// TestPatrolCommitClassWorktreeDrift_Lifecycle exercises the full
// observe -> under-threshold -> stall-fires -> dedup -> resolve -> re-observe
// cycle across five simulated controller ticks, since these transitions are
// only meaningful in sequence (each phase depends on state persisted by the
// bead from the previous tick).
func TestPatrolCommitClassWorktreeDrift_Lifecycle(t *testing.T) {
	cityPath, store := setupWorktreeDriftPatrolTest(t)
	cfg := commitClassAgentConfig()
	rec := &fakeDriftRecorder{}

	drifted := &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "main", ahead: 0, behind: 3}
	orig := newWorktreeDriftGitProbe
	defer func() { newWorktreeDriftGitProbe = orig }()
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe { return drifted }

	tick1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// Tick 1: drift first observed, under threshold — no event yet.
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick1, nil); fired != 0 {
		t.Fatalf("tick1: fired = %d, want 0 (first observation, under threshold)", fired)
	}
	if len(rec.events) != 0 {
		t.Fatalf("tick1: recorded %d events, want 0", len(rec.events))
	}
	beadsInStore, err := store.List(beads.ListQuery{Type: worktreeDriftBeadType, AllowScan: true})
	if err != nil || len(beadsInStore) != 1 {
		t.Fatalf("tick1: got %d worktree_drift beads (err=%v), want exactly 1", len(beadsInStore), err)
	}
	trackingID := beadsInStore[0].ID
	if got := beadsInStore[0].Metadata[worktreeDriftMetaFirstObservedAt]; got != tick1.Format(time.RFC3339) {
		t.Errorf("tick1: first_observed_at = %q, want %q", got, tick1.Format(time.RFC3339))
	}

	// Tick 2: still drifted, now past threshold — event fires exactly once.
	tick2 := tick1.Add(testDriftThreshold + time.Second)
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick2, nil); fired != 1 {
		t.Fatalf("tick2: fired = %d, want 1 (past threshold)", fired)
	}
	if len(rec.events) != 1 {
		t.Fatalf("tick2: recorded %d events, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.Type != events.WorktreeDriftStalled {
		t.Errorf("tick2: event.Type = %q, want %q", ev.Type, events.WorktreeDriftStalled)
	}
	if ev.Subject != "ga-rig/builder" {
		t.Errorf("tick2: event.Subject = %q, want %q", ev.Subject, "ga-rig/builder")
	}
	var payload events.WorktreeDriftStalledPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("tick2: unmarshaling payload: %v", err)
	}
	if payload.Identity != "ga-rig/builder" {
		t.Errorf("tick2: payload.Identity = %q, want %q", payload.Identity, "ga-rig/builder")
	}
	if payload.BehindCount != 3 {
		t.Errorf("tick2: payload.BehindCount = %d, want 3", payload.BehindCount)
	}
	if payload.Detached {
		t.Error("tick2: payload.Detached = true, want false (on branch \"builder\")")
	}
	b, err := store.Get(trackingID)
	if err != nil {
		t.Fatalf("tick2: fetching tracking bead: %v", err)
	}
	if b.Metadata[worktreeDriftMetaStallEventFired] != "true" {
		t.Errorf("tick2: stall_event_fired = %q, want %q", b.Metadata[worktreeDriftMetaStallEventFired], "true")
	}

	// Tick 3: still drifted, still past threshold — no duplicate event.
	tick3 := tick2.Add(time.Minute)
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick3, nil); fired != 0 {
		t.Fatalf("tick3: fired = %d, want 0 (already fired, must not duplicate)", fired)
	}
	if len(rec.events) != 1 {
		t.Fatalf("tick3: recorded %d total events, want still 1 (no duplicate)", len(rec.events))
	}

	// Tick 4: drift resolves (worktree caught back up) — observation clears.
	resolved := &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "main", ahead: 0, behind: 0}
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe { return resolved }
	tick4 := tick3.Add(time.Minute)
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick4, nil); fired != 0 {
		t.Fatalf("tick4: fired = %d, want 0 (resolved)", fired)
	}
	b, err = store.Get(trackingID)
	if err != nil {
		t.Fatalf("tick4: fetching tracking bead: %v", err)
	}
	if b.Metadata[worktreeDriftMetaFirstObservedAt] != "" {
		t.Errorf("tick4: first_observed_at = %q, want cleared", b.Metadata[worktreeDriftMetaFirstObservedAt])
	}
	if b.Metadata[worktreeDriftMetaStallEventFired] != "" {
		t.Errorf("tick4: stall_event_fired = %q, want cleared", b.Metadata[worktreeDriftMetaStallEventFired])
	}

	// Tick 5: drift reappears — fresh observation window (not the old timestamp).
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe { return drifted }
	tick5 := tick4.Add(time.Minute)
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick5, nil); fired != 0 {
		t.Fatalf("tick5: fired = %d, want 0 (fresh observation, under threshold)", fired)
	}
	b, err = store.Get(trackingID)
	if err != nil {
		t.Fatalf("tick5: fetching tracking bead: %v", err)
	}
	if got := b.Metadata[worktreeDriftMetaFirstObservedAt]; got != tick5.Format(time.RFC3339) {
		t.Errorf("tick5: first_observed_at = %q, want reset to %q", got, tick5.Format(time.RFC3339))
	}
	// Still only the single reusable tracking bead — never grows unbounded.
	beadsInStore, err = store.List(beads.ListQuery{Type: worktreeDriftBeadType, AllowScan: true})
	if err != nil || len(beadsInStore) != 1 {
		t.Fatalf("tick5: got %d worktree_drift beads (err=%v), want exactly 1 (reused, not re-created)", len(beadsInStore), err)
	}
}

// errInjectedMetadataWrite is the failure clearFailingStore injects.
var errInjectedMetadataWrite = errors.New("injected metadata write failure")

// clearFailingStore wraps a beads.Store and, while armed, fails SetMetadata
// calls that clear (write "") failKey. It simulates a store error landing
// between the two writes of clearWorktreeDriftObservation, which the
// in-memory store never produces on its own.
type clearFailingStore struct {
	beads.Store
	failKey string
	armed   bool
}

func (s *clearFailingStore) SetMetadata(id, key, value string) error {
	if s.armed && key == s.failKey && value == "" {
		return errInjectedMetadataWrite
	}
	return s.Store.SetMetadata(id, key, value)
}

// setWorktreeDriftProbe makes every worktree resolve to p until the test ends
// or the next call replaces it.
func setWorktreeDriftProbe(t *testing.T, p worktreeDriftGitProbe) {
	t.Helper()
	orig := newWorktreeDriftGitProbe
	t.Cleanup(func() { newWorktreeDriftGitProbe = orig })
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe { return p }
}

// driftTrackingBead returns the single worktree_drift tracking bead in store.
func driftTrackingBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	got, err := store.List(beads.ListQuery{Type: worktreeDriftBeadType, AllowScan: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d worktree_drift beads (err=%v), want exactly 1", len(got), err)
	}
	return got[0]
}

// driftAndFireOnce drives a drifted worktree through its first observation at
// tick1 and its stall event one threshold later, returning the firing tick.
func driftAndFireOnce(t *testing.T, cityPath string, cfg *config.City, store beads.Store, rec events.Recorder, tick1 time.Time) time.Time {
	t.Helper()
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick1, nil); fired != 0 {
		t.Fatalf("tick1: fired = %d, want 0 (first observation, under threshold)", fired)
	}
	tick2 := tick1.Add(testDriftThreshold + time.Second)
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick2, nil); fired != 1 {
		t.Fatalf("tick2: fired = %d, want 1 (past threshold)", fired)
	}
	return tick2
}

// A failed clear of stall_event_fired must not have already discarded the
// observation window: the two flags describe one episode, so a half-reset
// (window gone, "already fired" still set) opens a fresh window that the stale
// fire-guard then suppresses. Once the store recovers, the retry completes the
// clear and the next episode alerts normally.
func TestPatrolCommitClassWorktreeDrift_FailedStallFlagClearKeepsObservationWindow(t *testing.T) {
	cityPath, mem := setupWorktreeDriftPatrolTest(t)
	store := &clearFailingStore{Store: mem, failKey: worktreeDriftMetaStallEventFired}
	cfg := commitClassAgentConfig()
	rec := &fakeDriftRecorder{}
	tick1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	setWorktreeDriftProbe(t, &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "main", behind: 3})
	tick2 := driftAndFireOnce(t, cityPath, cfg, store, rec, tick1)

	// Tick 3: drift resolves, but clearing the stall flag fails.
	setWorktreeDriftProbe(t, &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "main"})
	store.armed = true
	tick3 := tick2.Add(time.Minute)
	patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick3, nil)
	b := driftTrackingBead(t, mem)
	if got, want := b.Metadata[worktreeDriftMetaFirstObservedAt], tick1.Format(time.RFC3339); got != want {
		t.Errorf("tick3: first_observed_at = %q, want %q kept: a failed stall-flag clear must not half-reset the episode", got, want)
	}
	if got := b.Metadata[worktreeDriftMetaStallEventFired]; got != "true" {
		t.Errorf("tick3: stall_event_fired = %q, want %q (the failed write changes nothing)", got, "true")
	}

	// Tick 4: still resolved, store recovered — the retry finishes the clear.
	store.armed = false
	tick4 := tick3.Add(time.Minute)
	patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick4, nil)
	b = driftTrackingBead(t, mem)
	if b.Metadata[worktreeDriftMetaFirstObservedAt] != "" || b.Metadata[worktreeDriftMetaStallEventFired] != "" {
		t.Fatalf("tick4: metadata = %v, want both cleared by the retry", b.Metadata)
	}

	// Ticks 5-6: a new drift episode past the threshold still fires.
	setWorktreeDriftProbe(t, &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "main", behind: 3})
	tick5 := tick4.Add(time.Minute)
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick5, nil); fired != 0 {
		t.Fatalf("tick5: fired = %d, want 0 (fresh window, under threshold)", fired)
	}
	tick6 := tick5.Add(testDriftThreshold + time.Second)
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick6, nil); fired != 1 {
		t.Fatalf("tick6: fired = %d, want 1 (new episode past threshold)", fired)
	}
	if len(rec.events) != 2 {
		t.Errorf("recorded %d events, want 2 (one per episode)", len(rec.events))
	}
}

// When clearing the observation window fails after the stall flag was already
// cleared, the leftover window is past threshold, so the next drift re-alerts.
// A partial failure must fail loud (a repeat alert), never silent (an alert
// suppressed by a stale "already fired" flag).
func TestPatrolCommitClassWorktreeDrift_PartialClearFailureRefiresInsteadOfSuppressing(t *testing.T) {
	cityPath, mem := setupWorktreeDriftPatrolTest(t)
	store := &clearFailingStore{Store: mem, failKey: worktreeDriftMetaFirstObservedAt}
	cfg := commitClassAgentConfig()
	rec := &fakeDriftRecorder{}
	tick1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	drifted := &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "main", behind: 3}
	setWorktreeDriftProbe(t, drifted)
	tick2 := driftAndFireOnce(t, cityPath, cfg, store, rec, tick1)

	// Tick 3: drift resolves, but clearing the observation window fails.
	setWorktreeDriftProbe(t, &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "main"})
	store.armed = true
	tick3 := tick2.Add(time.Minute)
	patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick3, nil)
	b := driftTrackingBead(t, mem)
	if got := b.Metadata[worktreeDriftMetaStallEventFired]; got != "" {
		t.Errorf("tick3: stall_event_fired = %q, want it cleared before the window so a partial failure cannot leave it stranded", got)
	}

	// Tick 4: drift is back and the store has recovered. The leftover window
	// is already past threshold, so the alert fires again.
	store.armed = false
	setWorktreeDriftProbe(t, drifted)
	tick4 := tick3.Add(time.Minute)
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, rec, tick4, nil); fired != 1 {
		t.Fatalf("tick4: fired = %d, want 1: a partial clear failure must re-fire, not suppress the alert", fired)
	}
	if len(rec.events) != 2 {
		t.Errorf("recorded %d events, want 2", len(rec.events))
	}
}

// An agent whose work_dir moves its worktree off the default
// .gc/worktrees/<dir>/<name> location is observed at the work_dir, expanded
// the way session start expands it (relative paths anchor to the city root and
// {{.Rig}}/{{.AgentBase}} templates resolve). Nothing exists at the default
// location, so only the override can satisfy the probe.
func TestPatrolCommitClassWorktreeDrift_ObservesAgentWithWorkDirOverride(t *testing.T) {
	cases := []struct {
		name         string
		workDir      string
		materialized string // city-relative path of the worktree that exists
	}{
		{"relative path", ".gc/worktrees/custom/builder-home", ".gc/worktrees/custom/builder-home"},
		{"templated path", ".gc/worktrees/{{.Rig}}/{{.AgentBase}}-home", ".gc/worktrees/ga-rig/builder-home"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			store := beads.NewMemStore()
			want := filepath.Join(cityPath, filepath.FromSlash(tc.materialized))
			if err := os.MkdirAll(want, 0o755); err != nil {
				t.Fatalf("creating worktree: %v", err)
			}
			cfg := commitClassAgentConfig()
			cfg.Agents[0].WorkDir = tc.workDir
			cfg.Rigs = []config.Rig{{Name: "ga-rig", Path: filepath.Join(cityPath, "rigs", "ga-rig")}}

			var probed []string
			orig := newWorktreeDriftGitProbe
			defer func() { newWorktreeDriftGitProbe = orig }()
			newWorktreeDriftGitProbe = func(workDir string) worktreeDriftGitProbe {
				probed = append(probed, workDir)
				return &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "main", behind: 3}
			}

			now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
			patrolCommitClassWorktreeDrift(cityPath, cfg, beads.WorkStore{Store: store}, testDriftThreshold, nil, now, nil)
			if len(probed) != 1 || probed[0] != want {
				t.Fatalf("probed worktrees = %v, want [%s]", probed, want)
			}
			b := driftTrackingBead(t, store)
			if got := b.Metadata[worktreeDriftMetaFirstObservedAt]; got != now.Format(time.RFC3339) {
				t.Errorf("first_observed_at = %q, want %q", got, now.Format(time.RFC3339))
			}
		})
	}
}

// TestWorktreeDriftBeadTypeIsRegistered pins the type the patrol writes to the
// vocabulary every scope registers (beads.RequiredCustomTypes). A store that
// validates types — bd on its CLI front door, the native store against its
// custom_types table — refuses a type nobody registered with "invalid issue
// type", so an unlisted type makes the patrol's Create fail on a real city
// while the in-memory store lets it pass.
func TestWorktreeDriftBeadTypeIsRegistered(t *testing.T) {
	if !slices.Contains(beads.RequiredCustomTypes, worktreeDriftBeadType) {
		t.Fatalf("beads.RequiredCustomTypes = %v, want it to list %q", beads.RequiredCustomTypes, worktreeDriftBeadType)
	}
	if !beads.IsReadyExcludedType(worktreeDriftBeadType) {
		t.Errorf("IsReadyExcludedType(%q) = false: a never-closed bookkeeping record must not surface as ready work", worktreeDriftBeadType)
	}
	// Deliberately not controller-owned: an owned type must classify off the
	// work class, and the patrol writes this record to the city work store.
	if coordclass.ControllerOwnedType(worktreeDriftBeadType) {
		t.Errorf("ControllerOwnedType(%q) = true: the record lives in the work class, so it cannot be an owned shape", worktreeDriftBeadType)
	}
}

// typeValidatingStore wraps a store and refuses Create and List for a type that
// is neither a bd built-in nor registered in beads.RequiredCustomTypes, as a bd
// or native store does with "invalid issue type". The in-memory store accepts
// any string.
type typeValidatingStore struct {
	beads.Store
}

func (s typeValidatingStore) validType(typ string) error {
	switch typ {
	case "", "bug", "feature", "task", "epic", "chore", "decision":
		return nil
	}
	if slices.Contains(beads.RequiredCustomTypes, typ) {
		return nil
	}
	return fmt.Errorf("invalid issue type %q", typ)
}

func (s typeValidatingStore) Create(b beads.Bead) (beads.Bead, error) {
	if err := s.validType(b.Type); err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Create(b)
}

func (s typeValidatingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if err := s.validType(q.Type); err != nil {
		return nil, err
	}
	return s.Store.List(q)
}

// The patrol must be able to record its first observation, and then fire its
// stall event, over a store that validates bead types.
func TestPatrolCommitClassWorktreeDrift_WorksOverATypeValidatingStore(t *testing.T) {
	cityPath, mem := setupWorktreeDriftPatrolTest(t)
	store := typeValidatingStore{Store: mem}
	cfg := commitClassAgentConfig()
	rec := &fakeDriftRecorder{}
	setWorktreeDriftProbe(t, &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "main", behind: 3})

	driftAndFireOnce(t, cityPath, cfg, store, rec, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))

	if len(rec.events) != 1 || rec.events[0].Type != events.WorktreeDriftStalled {
		t.Fatalf("recorded events = %+v, want exactly one %s", rec.events, events.WorktreeDriftStalled)
	}
	driftTrackingBead(t, mem)
}
