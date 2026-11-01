package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/worktree"
)

// liveSeatManagedSpec publishes a real managed worktree for beadID, as
// `gc worktree ensure` does, and returns its spec and verified path.
func liveSeatManagedSpec(t *testing.T, beadID string) (worktree.Spec, string) {
	t.Helper()
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	spec := worktree.Spec{
		RepoDir: repo, Root: root, Path: filepath.Join(root, beadID), Branch: "work/" + beadID, Base: base,
		BeadID: beadID, StoreRef: "rig:r1", Creator: "provisioner",
		Owner: "provisioner", Generation: "1", Lifecycle: worktree.LifecycleActive,
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	spec.BaseSHA = report.Provenance.BaseSHA
	return spec, report.Path
}

// liveSeatReuse is one legacy realization of a single active, idle pool seat
// "worker-1" whose row records seatDir and triggers gc-a.
type liveSeatReuse struct {
	seatDir  string
	running  bool
	extra    map[string]string
	request  func(id string) SessionRequest
	assigned func(id string) []beads.Bead
}

type liveSeatReuseResult struct {
	store   beads.Store
	id      string
	cfg     *config.City
	desired map[string]TemplateParams
	stderr  string
}

func (r liveSeatReuse) run(t *testing.T) liveSeatReuseResult {
	t.Helper()
	store := beads.NewMemStore()
	meta := map[string]string{
		"template":                        "worker",
		"agent_name":                      "worker-1",
		"session_name":                    "worker-1",
		"state":                           "active",
		"instance_token":                  "tok",
		poolManagedMetadataKey:            boolMetadata(true),
		"pool_slot":                       "1",
		beadmeta.TriggerBeadIDMetadataKey: "gc-a",
	}
	if r.seatDir != "" {
		meta[beadmeta.WorkDirMetadataKey] = r.seatDir
		meta[beadmeta.LegacyWorkDirMetadataKey] = r.seatDir
	}
	for k, v := range r.extra {
		meta[k] = v
	}
	seat, err := store.Create(beads.Bead{
		Title:    "worker",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "agent:worker"},
		Metadata: meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := r.request(seat.ID)
	request.Template = "worker"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			WorkDir:           filepath.Join(t.TempDir(), "pool"),
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
		}},
	}
	sp := runtime.NewFake()
	if r.running {
		if err := sp.Start(context.Background(), "worker-1", runtime.Config{WorkDir: r.seatDir}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, seat))
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, sp, time.Now().UTC(), store, &stderr)
	bp.sessionBeads = snapshot
	if r.assigned != nil {
		bp.assignedWorkBeads = r.assigned(seat.ID)
	}
	desired := map[string]TemplateParams{}
	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{Template: "worker", Requests: []SessionRequest{request}}, desired, &stderr)
	return liveSeatReuseResult{store: store, id: seat.ID, cfg: cfg, desired: desired, stderr: stderr.String()}
}

// reused runs r and requires that legacy realization reused the seat.
func (r liveSeatReuse) reused(t *testing.T) liveSeatReuseResult {
	t.Helper()
	res := r.run(t)
	if _, ok := res.desired["worker-1"]; !ok {
		t.Fatalf("legacy realization did not reuse the idle seat; desired=%v stderr=%s", mapKeys(res.desired), res.stderr)
	}
	return res
}

func (res liveSeatReuseResult) row(t *testing.T) beads.Bead {
	t.Helper()
	row, err := res.store.Get(res.id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func newDemand(request SessionRequest) func(string) SessionRequest {
	return func(string) SessionRequest { return request }
}

// A running seat reused for work bound to another verified worktree keeps the
// directory its process runs in; its trigger moves to the new work.
func TestLegacyReuseKeepsARunningSeatsWorkDir(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, _ := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.reused(t)
	row := res.row(t)
	if got := row.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "gc-b" {
		t.Fatalf("trigger = %q, want gc-b", got)
	}
	if got := row.Metadata[beadmeta.WorkDirMetadataKey]; got != pathA {
		t.Fatalf("running seat's gc.work_dir = %q; want it kept at %q, where its process runs", got, pathA)
	}
	if got := row.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != pathA {
		t.Fatalf("running seat's work_dir = %q; want it kept at %q", got, pathA)
	}
}

// A seat whose runtime is not running starts where its row points: it moves.
func TestLegacyReuseMovesAStoppedSeat(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: false, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.reused(t)
	if got := res.row(t).Metadata[beadmeta.WorkDirMetadataKey]; got != pathB {
		t.Fatalf("stopped seat's gc.work_dir = %q; want %q, where it will start", got, pathB)
	}
}

// A running seat inside one bead's verified worktree, reused for work bound to
// no worktree, keeps that worktree rather than naming the pool's directory it
// never ran in.
func TestLegacyReuseKeepsARunningSeatsWorktreeForUnboundWork(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	res := liveSeatReuse{seatDir: pathA, running: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-u", WorkStoreRef: "rig:r1",
	})}.reused(t)
	if got := res.row(t).Metadata[beadmeta.WorkDirMetadataKey]; got != pathA {
		t.Fatalf("running seat's gc.work_dir = %q; want it kept at %q", got, pathA)
	}
}

// End to end: after the reuse, the seat's claim reads its row and leaves the
// work bound to the other worktree instead of claiming it.
func TestLegacyReuseClaimLeavesWorkBoundElsewhere(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.reused(t)
	info, err := sessionFrontDoor(res.store).Get(res.id)
	if err != nil {
		t.Fatal(err)
	}
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{managedWorktreeCandidate("gc-b", pathB)}, &claimed, &reclaimed)
	opts := affinitySeatOpts("")
	opts.PoolSeat = hookClaimPoolSeatFor(t.TempDir(), "test-city", nil, &info, &res.cfg.Agents[0])
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rigs/r1", opts, ops, &stdout, &stderr)
	if len(claimed) != 0 || !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("claimed %v, result %s; want gc-b left to a session in %s (the seat runs in %s); stderr=%s", claimed, stdout.String(), pathB, pathA, stderr.String())
	}
}

// A running fresh-mode seat given its own next work (a resume request) is
// cycled by the reconciler into that work's directory, so its row still moves.
func TestLegacyReuseResumeOfARunningFreshSeatStillMoves(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{
		seatDir: pathA, running: true, extra: map[string]string{"wake_mode": "fresh"},
		request: func(id string) SessionRequest {
			return SessionRequest{Tier: "resume", SessionBeadID: id, WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB}
		},
		assigned: func(id string) []beads.Bead {
			return []beads.Bead{workBead("gc-b", "worker", id, "in_progress", 1)}
		},
	}.reused(t)
	if got := res.row(t).Metadata[beadmeta.WorkDirMetadataKey]; got != pathB {
		t.Fatalf("fresh-mode resume left gc.work_dir at %q; want %q, where the fresh cycle restarts it", got, pathB)
	}
}

// A resume-mode seat that resumes its own next work in place keeps its
// directory, as before (the live retry continuation).
func TestLegacyReuseResumeOfARunningResumeModeSeatKeepsItsDir(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, _ := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{
		seatDir: pathA, running: true,
		request: func(id string) SessionRequest {
			return SessionRequest{Tier: "resume", SessionBeadID: id, WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB}
		},
		assigned: func(id string) []beads.Bead {
			return []beads.Bead{workBead("gc-b", "worker", id, "in_progress", 1)}
		},
	}.reused(t)
	if got := res.row(t).Metadata[beadmeta.WorkDirMetadataKey]; got != pathA {
		t.Fatalf("resume-mode resume moved gc.work_dir to %q; want it kept at %q", got, pathA)
	}
}

// Unusable worktree evidence still skips a running seat: keeping its recorded
// directory never masks a verification failure.
func TestLegacyReuseRunningSeatWithUnusableEvidenceIsSkipped(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	res := liveSeatReuse{seatDir: pathA, running: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeError: "spec unreadable",
	})}.run(t)
	if _, ok := res.desired["worker-1"]; ok {
		t.Fatalf("seat with unusable evidence stayed desired; stderr=%s", res.stderr)
	}
	if !strings.Contains(res.stderr, errPoolTriggerWorktreeEvidence.Error()) || !strings.Contains(res.stderr, "(skipping)") {
		t.Fatalf("stderr = %q; want the worktree evidence skip", res.stderr)
	}
	row := res.row(t)
	if got := row.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "gc-a" {
		t.Fatalf("trigger = %q; want gc-a left unbound", got)
	}
	if got := row.Metadata[beadmeta.WorkDirMetadataKey]; got != pathA {
		t.Fatalf("gc.work_dir = %q; want %q untouched", got, pathA)
	}
}

// A running seat whose row records no directory takes the new work's.
func TestLegacyReuseRunningSeatWithoutRecordedDirTakesTheNewDir(t *testing.T) {
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: "", running: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.reused(t)
	if got := res.row(t).Metadata[beadmeta.WorkDirMetadataKey]; got != pathB {
		t.Fatalf("gc.work_dir = %q; want %q", got, pathB)
	}
}

// No build params: the verified dir, and no panic.
func TestPoolTriggerBindWorkDirWithoutBuildParams(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	info := sessiontest.SeedBead(t, beads.Bead{ID: "s-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "worker-1", beadmeta.WorkDirMetadataKey: pathA, beadmeta.LegacyWorkDirMetadataKey: pathA,
	}})
	got, err := poolTriggerBindWorkDir(nil, &config.Agent{Name: "worker"}, "worker", info, SessionRequest{Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB})
	if err != nil || got != pathB {
		t.Fatalf("poolTriggerBindWorkDir(nil bp) = %q, %v; want %q", got, err, pathB)
	}
}

// A running seat inside its post-create protection window carries its own
// SessionBeadID on a new-tier request. Once its trigger is no longer demanded,
// allocateScaleDemandToConcrete rebinds that request to other work. Nothing
// restarts the seat, so it keeps the directory its process runs in.
func TestLegacyReuseKeepsARunningProtectedSeatsWorkDir(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, _ := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, request: func(id string) SessionRequest {
		return SessionRequest{Tier: "new", SessionBeadID: id, WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB}
	}}.reused(t)
	row := res.row(t)
	if got := row.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "gc-b" {
		t.Fatalf("trigger = %q, want gc-b", got)
	}
	if got := row.Metadata[beadmeta.WorkDirMetadataKey]; got != pathA {
		t.Fatalf("running protected seat's gc.work_dir = %q; want it kept at %q, where its process runs", got, pathA)
	}
}

// End to end from demand: a seat started 30s ago in A, whose trigger gc-a is
// no longer demanded, has its protected request rebound to gc-b (bound to B).
// It keeps A, and its claim leaves gc-b to a session in B.
func TestLegacyProtectedRebindClaimLeavesWorkBoundElsewhere(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	now := time.Now().UTC()
	store := beads.NewMemStore()
	seat, err := store.Create(beads.Bead{
		Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template": "worker", "agent_name": "worker-1", "alias": "worker-1", "session_name": "worker-1",
			"session_origin": "ephemeral", "state": "active", "state_reason": "creation_complete",
			"creation_complete_at": now.Add(-30 * time.Second).Format(time.RFC3339), "pool_slot": "1",
			"instance_token": "tok", poolManagedMetadataKey: boolMetadata(true),
			beadmeta.TriggerBeadIDMetadataKey: "gc-a", beadmeta.TriggerBeadStoreRefMetadataKey: "rig:r1",
			beadmeta.WorkDirMetadataKey: pathA, beadmeta.LegacyWorkDirMetadataKey: pathA,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{
		Name: "worker", StartCommand: "true", WorkDir: filepath.Join(t.TempDir(), "pool"),
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
	}}}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{WorkDir: pathA}); err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, seat))
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, sp, now, store, &stderr)
	bp.sessionBeads = snapshot
	demand := map[string]scaleCheckDemand{"worker": {
		Count:         1,
		WorkBeadIDs:   []string{"gc-b"},
		StoreRefs:     map[string]string{"gc-b": "rig:r1"},
		WorktreeSpecs: map[string]*worktree.Spec{"gc-b": &specB},
	}}
	states := computePoolDesiredStatesAt(cfg, nil, snapshot.OpenInfos(), map[string]int{"worker": 1}, demand, now, nil)
	if len(states) != 1 || len(states[0].Requests) != 1 || states[0].Requests[0].SessionBeadID != seat.ID {
		t.Fatalf("desired states = %#v; want one protected request for %s", states, seat.ID)
	}
	desired := map[string]TemplateParams{}
	realizePoolDesiredSessionsAt(bp, &cfg.Agents[0], states[0], now, desired, &stderr)
	info, err := sessionFrontDoor(store).Get(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.TriggerBeadID != "gc-b" || preparedStartSessionWorkDir(info) != pathA {
		t.Fatalf("row trigger %q, work dir %q; want gc-b, kept at %q; stderr=%s", info.TriggerBeadID, preparedStartSessionWorkDir(info), pathA, stderr.String())
	}
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{managedWorktreeCandidate("gc-b", pathB)}, &claimed, &reclaimed)
	opts := affinitySeatOpts("")
	opts.PoolSeat = hookClaimPoolSeatFor(t.TempDir(), "test-city", nil, &info, &cfg.Agents[0])
	var stdout, claimErr bytes.Buffer
	doHookClaim("query", "/rigs/r1", opts, ops, &stdout, &claimErr)
	if len(claimed) != 0 || !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("claimed %v, result %s; want gc-b left to a session in %s (the seat runs in %s); stderr=%s", claimed, stdout.String(), pathB, pathA, claimErr.String())
	}
}
