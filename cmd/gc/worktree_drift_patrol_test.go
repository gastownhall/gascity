package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
	fired := patrolCommitClassWorktreeDrift(t.TempDir(), nil, store, testDriftThreshold, nil, time.Now(), nil)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 for nil config", fired)
	}
}

func TestPatrolCommitClassWorktreeDrift_NilStore(t *testing.T) {
	cityPath, _ := setupWorktreeDriftPatrolTest(t)
	cfg := commitClassAgentConfig()
	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, nil, testDriftThreshold, nil, time.Now(), nil)
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

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, 0, nil, time.Now(), nil)
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

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, nil, time.Now(), nil)
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

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, nil, time.Now(), nil)
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

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, nil, time.Now(), nil)
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

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, nil, time.Now(), nil)
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

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, nil, time.Now(), nil)
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

	fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, nil, now, nil)
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

	patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, nil, time.Now(), nil)
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
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, rec, tick1, nil); fired != 0 {
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
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, rec, tick2, nil); fired != 1 {
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
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, rec, tick3, nil); fired != 0 {
		t.Fatalf("tick3: fired = %d, want 0 (already fired, must not duplicate)", fired)
	}
	if len(rec.events) != 1 {
		t.Fatalf("tick3: recorded %d total events, want still 1 (no duplicate)", len(rec.events))
	}

	// Tick 4: drift resolves (worktree caught back up) — observation clears.
	resolved := &fakeWorktreeDriftGit{isRepo: true, currentBranch: "builder", defaultBranch: "main", ahead: 0, behind: 0}
	newWorktreeDriftGitProbe = func(_ string) worktreeDriftGitProbe { return resolved }
	tick4 := tick3.Add(time.Minute)
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, rec, tick4, nil); fired != 0 {
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
	if fired := patrolCommitClassWorktreeDrift(cityPath, cfg, store, testDriftThreshold, rec, tick5, nil); fired != 0 {
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
