package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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

// liveSeatAgent is the pool every live-seat case realizes: worker, whose
// provider names the agent process, so a pane whose agent died reads as a
// zombie (runtime.ObserveLiveness), and whose own directory is pool.
func liveSeatAgent(t *testing.T, minSessions, maxSessions int) *config.City {
	t.Helper()
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			ProcessNames:      []string{"worker-agent"},
			WorkDir:           filepath.Join(t.TempDir(), "pool"),
			MinActiveSessions: intPtr(minSessions),
			MaxActiveSessions: intPtr(maxSessions),
		}},
	}
}

// liveSeatReuse is one legacy realization of a single active, idle pool seat
// "worker-1" whose row records seatDir and triggers gc-a. The pool holds three
// sessions unless occupied or canonical says otherwise.
type liveSeatReuse struct {
	seatDir string
	running bool
	// zombie leaves the seat's pane running with its agent process dead.
	zombie bool
	// occupied caps the pool at two sessions and fills the other slot with a
	// busy session ("worker-2", running assigned work gc-z), so no fresh
	// session can start.
	occupied bool
	// canonical makes the pool a canonical singleton (max_active_sessions 1)
	// whose one session, "worker", is the seat.
	canonical bool
	// partial makes the session snapshot incomplete, so no fresh session can
	// start for a reason other than a full pool.
	partial bool
	// oneShot makes the pool one_shot, whose seat asleep for a freeable
	// reason is reusable (reusablePoolSessionInfo).
	oneShot bool
	// store wraps the store realization reads and writes, once the seat's
	// row exists.
	store    func(beads.Store) beads.Store
	extra    map[string]string
	request  func(id string) SessionRequest
	assigned func(id string) []beads.Bead
}

type liveSeatReuseResult struct {
	store      beads.Store
	sp         *runtime.Fake
	cityPath   string
	id         string
	seatName   string
	occupantID string
	cfg        *config.City
	request    SessionRequest
	assigned   []beads.Bead
	partial    bool
	// base is the desired set realization computed, before the session
	// overlay (DesiredStateResult.BaseState); desired includes the overlay.
	base    map[string]TemplateParams
	desired map[string]TemplateParams
	// retired is the pool seats realization retired to free a slot
	// (agentBuildParams.retiredPoolSeats).
	retired map[string]bool
	stderr  string
}

func (r liveSeatReuse) run(t *testing.T) liveSeatReuseResult {
	t.Helper()
	store := beads.NewMemStore()
	seatName, maxSessions := "worker-1", 3
	meta := map[string]string{
		"template":                        "worker",
		"agent_name":                      seatName,
		"session_name":                    seatName,
		"session_origin":                  "ephemeral",
		"state":                           "active",
		"instance_token":                  "tok",
		poolManagedMetadataKey:            boolMetadata(true),
		"pool_slot":                       "1",
		beadmeta.TriggerBeadIDMetadataKey: "gc-a",
	}
	switch {
	case r.canonical:
		seatName, maxSessions = "worker", 1
		meta["agent_name"], meta["alias"], meta["session_name"] = seatName, seatName, seatName
		delete(meta, "pool_slot")
	case r.occupied:
		maxSessions = 2
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
	res := liveSeatReuseResult{
		store: store, sp: runtime.NewFake(), cityPath: t.TempDir(), id: seat.ID, seatName: seatName,
		cfg: liveSeatAgent(t, 0, maxSessions), request: r.request(seat.ID), partial: r.partial,
	}
	if r.oneShot {
		res.cfg.Agents[0].Lifecycle = config.AgentLifecycleOneShot
	}
	if r.store != nil {
		res.store = r.store(store)
	}
	res.request.Template = "worker"
	if r.running {
		if err := res.sp.Start(context.Background(), seatName, runtime.Config{WorkDir: r.seatDir}); err != nil {
			t.Fatal(err)
		}
	}
	if r.zombie {
		res.sp.Zombies[seatName] = true
	}
	if r.assigned != nil {
		res.assigned = r.assigned(seat.ID)
	}
	if r.occupied {
		res.occupy(t)
	}
	res.realize(t)
	return res
}

// occupy adds the busy session "worker-2", in slot 2 and running work gc-z
// assigned to it, which no request for other work can reuse; realization
// resumes it for that work, so the pool's template stays desired.
func (res *liveSeatReuseResult) occupy(t *testing.T) {
	t.Helper()
	poolDir := res.cfg.Agents[0].WorkDir
	occupant, err := res.store.Create(beads.Bead{
		Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template": "worker", "agent_name": "worker-2", "session_name": "worker-2", "session_origin": "ephemeral",
			"state": "active", "instance_token": "tok-2", poolManagedMetadataKey: boolMetadata(true),
			"pool_slot": "2", beadmeta.TriggerBeadIDMetadataKey: "gc-z",
			beadmeta.WorkDirMetadataKey: poolDir, beadmeta.LegacyWorkDirMetadataKey: poolDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := res.sp.Start(context.Background(), "worker-2", runtime.Config{WorkDir: poolDir}); err != nil {
		t.Fatal(err)
	}
	res.occupantID = occupant.ID
	res.assigned = append(res.assigned, workBead("gc-z", "worker", occupant.ID, "in_progress", 1))
}

// realize runs one legacy realization of res.request, after the busy
// occupant's resume when there is one, against the session rows the store
// holds now, then the session overlay buildDesiredState applies to it.
func (res *liveSeatReuseResult) realize(t *testing.T) {
	t.Helper()
	snapshot, err := loadSessionBeadSnapshot(res.store)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", res.cityPath, res.cfg, res.sp, time.Now().UTC(), res.store, &stderr)
	bp.sessionBeads = snapshot
	bp.assignedWorkBeads = res.assigned
	if res.partial {
		bp.sessionSnapshotCompletenessKnown, bp.sessionSnapshotComplete = true, false
	}
	requests := []SessionRequest{res.request}
	if res.occupantID != "" {
		requests = append([]SessionRequest{{Template: "worker", Tier: "resume", SessionBeadID: res.occupantID, WorkBeadID: "gc-z"}}, requests...)
	}
	res.desired = map[string]TemplateParams{}
	realizePoolDesiredSessions(bp, &res.cfg.Agents[0], PoolDesiredState{Template: "worker", Requests: requests}, res.desired, &stderr)
	res.base = cloneDesiredState(res.desired)
	discoverSessionBeads(bp, res.cfg, res.desired, &stderr)
	res.retired = bp.retiredPoolSeats
	res.stderr = stderr.String()
}

// reused runs r and requires that legacy realization reused the seat.
func (r liveSeatReuse) reused(t *testing.T) liveSeatReuseResult {
	t.Helper()
	res := r.run(t)
	if _, ok := res.desired[res.seatName]; !ok {
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

// otherRows returns the open session rows besides the seat's and the busy
// occupant's.
func (res liveSeatReuseResult) otherRows(t *testing.T) []session.Info {
	t.Helper()
	snapshot, err := loadSessionBeadSnapshot(res.store)
	if err != nil {
		t.Fatal(err)
	}
	var rows []session.Info
	for _, info := range snapshot.OpenInfos() {
		if info.ID != res.id && info.ID != res.occupantID {
			rows = append(rows, info)
		}
	}
	return rows
}

// freshSession requires that realization started exactly one other session,
// for workID in dir, and returns its row.
func (res liveSeatReuseResult) freshSession(t *testing.T, workID, dir string) session.Info {
	t.Helper()
	rows := res.otherRows(t)
	if len(rows) != 1 {
		t.Fatalf("got %d session rows besides the seat, want one fresh session for %s; desired=%v stderr=%s", len(rows), workID, mapKeys(res.desired), res.stderr)
	}
	fresh := rows[0]
	if fresh.TriggerBeadID != workID || preparedStartSessionWorkDir(fresh) != dir {
		t.Fatalf("fresh session trigger %q, work dir %q; want %s in %s", fresh.TriggerBeadID, preparedStartSessionWorkDir(fresh), workID, dir)
	}
	if _, ok := res.desired[fresh.SessionNameMetadata]; !ok {
		t.Fatalf("fresh session %s is not desired; desired=%v stderr=%s", fresh.SessionNameMetadata, mapKeys(res.desired), res.stderr)
	}
	return fresh
}

// requireSeatNotRepointed requires that realization left the seat's row bound
// to gc-a in dir: it never re-pointed the seat at work it runs elsewhere from.
// Whether the seat stays desired is the session overlay's question: with a
// fresh session started for the work, the pool's template is desired, and the
// overlay keeps the seat as it was.
func (res liveSeatReuseResult) requireSeatNotRepointed(t *testing.T, dir string) {
	t.Helper()
	row := res.row(t)
	if got := row.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "gc-a" {
		t.Fatalf("seat trigger = %q; want gc-a, the seat never re-pointed at work it would leave", got)
	}
	if got := row.Metadata[beadmeta.WorkDirMetadataKey]; got != dir {
		t.Fatalf("seat gc.work_dir = %q; want %q", got, dir)
	}
}

// requireSeatRetired requires that realization retired the seat to free a
// slot: it left the desired set, the session overlay included, and is the
// only seat retired.
func (res liveSeatReuseResult) requireSeatRetired(t *testing.T) {
	t.Helper()
	if _, ok := res.desired[res.seatName]; ok {
		t.Fatalf("the retired seat is desired; desired=%v stderr=%s", mapKeys(res.desired), res.stderr)
	}
	if len(res.retired) != 1 || !res.retired[res.id] {
		t.Fatalf("retired seats = %v; want only the seat %s", res.retired, res.id)
	}
}

// drainSeatOrphaned does what the reconciler does with a live seat no desired
// state wants: it drains it as orphaned, a drain the min_active_sessions
// floor does not veto (TestCancelSelfInitiatedDrainAckAtMinFloorRefusals), so
// its runtime stops and its row sleeps with the drain's reason
// (completeDrain).
func drainSeatOrphaned(t *testing.T, store beads.Store, sp *runtime.Fake, id, name string) {
	t.Helper()
	if err := sp.Stop(name); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(id, session.CompleteDrainPatch(time.Now().UTC(), drainOrphaned, false)); err != nil {
		t.Fatal(err)
	}
}

// claimFrom runs `gc hook --claim` as the session whose row is id, over
// candidates, and returns what it claimed and printed.
func (res liveSeatReuseResult) claimFrom(t *testing.T, id string, candidates ...beads.Bead) ([]string, string) {
	t.Helper()
	info, err := sessionFrontDoor(res.store).Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, candidates, &claimed, &reclaimed)
	opts := affinitySeatOpts("")
	opts.PoolSeat = hookClaimPoolSeatFor(res.cityPath, "test-city", nil, &info, &res.cfg.Agents[0])
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rigs/r1", opts, ops, &stdout, &stderr)
	return claimed, stdout.String() + stderr.String()
}

func newDemand(request SessionRequest) func(string) SessionRequest {
	return func(string) SessionRequest { return request }
}

// A running seat is not handed work bound to another verified worktree: its
// claim would leave that work. The work gets a fresh session in its own
// worktree, and the seat's row keeps its own trigger and directory.
func TestLegacyRunningSeatElsewhereLeavesManagedWorkToAFreshSession(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.run(t)
	res.requireSeatNotRepointed(t, pathA)
	res.freshSession(t, "gc-b", pathB)
}

// End to end: the fresh session's claim takes the work, and the running seat's
// claim, which reads its own row, still leaves it.
func TestLegacyRunningSeatElsewhereWorkIsClaimedByTheFreshSession(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.run(t)
	fresh := res.freshSession(t, "gc-b", pathB)
	if claimed, out := res.claimFrom(t, fresh.ID, managedWorktreeCandidate("gc-b", pathB)); len(claimed) != 1 || claimed[0] != "gc-b" {
		t.Fatalf("fresh session in %s claimed %v; want gc-b; output=%s", pathB, claimed, out)
	}
	if claimed, out := res.claimFrom(t, res.id, managedWorktreeCandidate("gc-b", pathB)); len(claimed) != 0 || !strings.Contains(out, `"reason":"no_work"`) {
		t.Fatalf("seat in %s claimed %v; want gc-b left to the session in %s; output=%s", pathA, claimed, pathB, out)
	}
}

// With no slot free, work bound to another worktree is not handed to a
// running seat that would leave it, and does not wait on that seat leaving by
// itself: the seat is not re-pointed, it retires to free its slot, and both
// are named. Once the reconciler has drained the seat and closed its row, the
// work gets a fresh session in its own worktree.
func TestLegacyRunningSeatElsewhereAtCapacityRetiresSoManagedWorkGetsASession(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, occupied: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.run(t)
	res.requireSeatNotRepointed(t, pathA)
	res.requireSeatRetired(t)
	if rows := res.otherRows(t); len(rows) != 0 {
		t.Fatalf("started %d sessions in a full pool; want none", len(rows))
	}
	for _, want := range []string{res.id, pathA, pathB, "no free concrete slot", "left for a session in its own directory", "freeing a slot: session " + res.id} {
		if !strings.Contains(res.stderr, want) {
			t.Fatalf("stderr = %q; want it to name %q", res.stderr, want)
		}
	}
	drainSeatOrphaned(t, res.store, res.sp, res.id, res.seatName)
	res.realize(t)
	res.requireSeatRetired(t)
	if err := res.store.Close(res.id); err != nil {
		t.Fatal(err)
	}
	res.realize(t)
	res.freshSession(t, "gc-b", pathB)
	if len(res.retired) != 0 {
		t.Fatalf("retired %v once a slot was free; want none", res.retired)
	}
}

// A pool that cannot start a fresh session for a reason other than being
// full (here a partial session snapshot) retires no seat: one leaving would
// not let a session start.
func TestLegacyRunningSeatElsewhereRetiresNothingWhenThePoolIsNotFull(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, _ := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, partial: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.run(t)
	res.requireSeatNotRepointed(t, pathA)
	if len(res.retired) != 0 || strings.Contains(res.stderr, "freeing a slot") {
		t.Fatalf("retired %v with free slots; stderr=%s", res.retired, res.stderr)
	}
	if !strings.Contains(res.stderr, "left for a session in its own directory") {
		t.Fatalf("stderr = %q; want the wait named", res.stderr)
	}
}

// Selection observes only the seats it would choose: the busy occupant, which
// no request for other work can reuse, is never probed, and the seat passed
// over is probed once.
func TestLegacySelectionProbesOnlySeatsItCanChoose(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, _ := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, occupied: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.run(t)
	probes := map[string]int{}
	for _, call := range res.sp.Calls {
		if call.Method == "IsRunning" {
			probes[call.Name]++
		}
	}
	if probes["worker-2"] != 0 {
		t.Fatalf("probed the busy occupant %d times; want none", probes["worker-2"])
	}
	if probes[res.seatName] != 1 {
		t.Fatalf("probed the seat passed over %d times; want once", probes[res.seatName])
	}
}

// A seat passed over while running, whose runtime then stops, is reused for
// the same work on the next realization and moves to the work's directory,
// where it will start.
func TestLegacySeatPassedOverThenStoppedMovesToTheWork(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, occupied: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.run(t)
	res.requireSeatNotRepointed(t, pathA)
	res.requireStoppedSeatMoves(t, "gc-b", pathB)
}

// requireStoppedSeatMoves stops the seat's runtime, realizes again, and
// requires that the seat was reused for workID and moved to dir.
func (res *liveSeatReuseResult) requireStoppedSeatMoves(t *testing.T, workID, dir string) {
	t.Helper()
	if err := res.sp.Stop(res.seatName); err != nil {
		t.Fatal(err)
	}
	res.realize(t)
	if _, ok := res.desired[res.seatName]; !ok {
		t.Fatalf("stopped seat was not reused; desired=%v stderr=%s", mapKeys(res.desired), res.stderr)
	}
	row := res.row(t)
	if got := row.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != workID {
		t.Fatalf("trigger = %q, want %s", got, workID)
	}
	if got := row.Metadata[beadmeta.WorkDirMetadataKey]; got != dir {
		t.Fatalf("stopped seat's gc.work_dir = %q; want %q, where it will start", got, dir)
	}
}

// A canonical singleton pool has one session and no fresh one to start while
// it runs. Running elsewhere, it is not handed work bound to another
// worktree; the work waits, and once the session stops it is reused there.
func TestLegacyCanonicalSeatElsewhereLeavesManagedWorkWaitingUntilItStops(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, canonical: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.run(t)
	res.requireSeatNotRepointed(t, pathA)
	for _, want := range []string{res.id, pathA, pathB, "left for a session in its own directory"} {
		if !strings.Contains(res.stderr, want) {
			t.Fatalf("stderr = %q; want it to name %q", res.stderr, want)
		}
	}
	res.requireStoppedSeatMoves(t, "gc-b", pathB)
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

// A pane whose agent process died is a zombie the reconciler restarts, not a
// running seat: it is reused and moves like a stopped one.
func TestLegacyReuseMovesAZombieSeat(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, zombie: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
	})}.reused(t)
	if got := res.row(t).Metadata[beadmeta.WorkDirMetadataKey]; got != pathB {
		t.Fatalf("zombie seat's gc.work_dir = %q; want %q, where the reconciler restarts it", got, pathB)
	}
	if rows := res.otherRows(t); len(rows) != 0 {
		t.Fatalf("started %d sessions; want the zombie seat reused", len(rows))
	}
}

// A running seat inside one bead's verified worktree is not handed work bound
// to no worktree while a fresh session can start in the pool's own directory,
// where the controller places that work.
func TestLegacyRunningSeatInAWorktreeLeavesUnboundWorkToAFreshSession(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	res := liveSeatReuse{seatDir: pathA, running: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-u", WorkStoreRef: "rig:r1",
	})}.run(t)
	res.requireSeatNotRepointed(t, pathA)
	res.freshSession(t, "gc-u", res.cfg.Agents[0].WorkDir)
}

// When no fresh session can start, work bound to no worktree may still go to
// a running seat elsewhere, whose claim decides whether it runs it there. The
// seat keeps the directory its process runs in, and the controller says so.
func TestLegacyRunningSeatKeepsItsDirForUnboundWorkWhenNoFreshSessionCanStart(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	res := liveSeatReuse{seatDir: pathA, running: true, occupied: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-u", WorkStoreRef: "rig:r1",
	})}.reused(t)
	row := res.row(t)
	if got := row.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "gc-u" {
		t.Fatalf("trigger = %q, want gc-u", got)
	}
	if got := row.Metadata[beadmeta.WorkDirMetadataKey]; got != pathA {
		t.Fatalf("running seat's gc.work_dir = %q; want it kept at %q, where its process runs", got, pathA)
	}
	if got := row.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != pathA {
		t.Fatalf("running seat's work_dir = %q; want it kept at %q", got, pathA)
	}
	for _, want := range []string{"keeps work dir " + pathA, "gc-u"} {
		if !strings.Contains(res.stderr, want) {
			t.Fatalf("stderr = %q; want it to name %q", res.stderr, want)
		}
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

// Unusable worktree evidence still skips a running seat: passing over a seat
// that runs elsewhere never masks a verification failure.
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

// A request that only names its seat (NameOnly) binds a reused running seat
// unnamed when its work's worktree evidence is unusable, where the same named
// request skips it (TestLegacyReuseRunningSeatWithUnusableEvidenceIsSkipped):
// the seat a custom count places stays desired in the directory it runs in,
// its trigger cleared, and the refusal is reported.
func TestLegacyReuseNameOnlyRequestWithUnusableEvidenceBindsTheSeatUnnamed(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	res := liveSeatReuse{seatDir: pathA, running: true, request: newDemand(SessionRequest{
		Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeError: "spec unreadable", NameOnly: true,
	})}.reused(t)
	row := res.row(t)
	if got := row.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "" {
		t.Fatalf("trigger = %q; want the seat bound unnamed", got)
	}
	if got := row.Metadata[beadmeta.WorkDirMetadataKey]; got != pathA {
		t.Fatalf("gc.work_dir = %q; want %q, where its row records it runs", got, pathA)
	}
	if !strings.Contains(res.stderr, errPoolTriggerWorktreeEvidence.Error()) || !strings.Contains(res.stderr, "(binding the seat unnamed)") {
		t.Fatalf("stderr = %q; want the evidence refusal and the unnamed binding", res.stderr)
	}
}

// A live seat whose request was rebound to work that only names it
// (NameOnly), work whose worktree fails verification (here: removed), stays:
// no fresh session can start in that worktree, so the seat it was passed over
// for binds unnamed where it runs instead of leaving the desired set.
func TestLegacyLiveSeatReboundToUnverifiableNameOnlyWorkStays(t *testing.T) {
	spec, path := liveSeatManagedSpec(t, "gc-b")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	seatDir := t.TempDir()
	res := liveSeatReuse{seatDir: seatDir, running: true, request: func(id string) SessionRequest {
		return SessionRequest{Tier: "new", SessionBeadID: id, WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &spec, NameOnly: true}
	}}.reused(t)
	row := res.row(t)
	if got := row.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "" {
		t.Fatalf("trigger = %q; want the seat bound unnamed", got)
	}
	if got := row.Metadata[beadmeta.WorkDirMetadataKey]; got != seatDir {
		t.Fatalf("gc.work_dir = %q; want %q, where it runs", got, seatDir)
	}
	if rows := res.otherRows(t); len(rows) != 0 {
		t.Fatalf("started %d other sessions, want none for work whose worktree fails verification", len(rows))
	}
	if !strings.Contains(res.stderr, "verification failed") || !strings.Contains(res.stderr, "(starting the seat unnamed)") {
		t.Fatalf("stderr = %q; want the verification refusal and the unnamed seat", res.stderr)
	}
}

// realizeFreshRequest realizes request alone on an empty pool and returns the
// session rows realization created, its desired set and what it printed.
func realizeFreshRequest(t *testing.T, request SessionRequest) ([]session.Info, map[string]TemplateParams, string) {
	t.Helper()
	store := beads.NewMemStore()
	cfg := liveSeatAgent(t, 0, 3)
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = snapshot
	desired := map[string]TemplateParams{}
	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{Template: "worker", Requests: []SessionRequest{request}}, desired, &stderr)
	created, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatal(err)
	}
	return created.OpenInfos(), desired, stderr.String()
}

// A fresh request that only names its seat (NameOnly), whose work's worktree
// fails verification (here: removed), starts the seat unnamed: the session
// has no trigger and does not start in that worktree, and the refusal is
// reported. The control, the same request naming its seat for good, is
// refused and starts nothing, as before.
func TestLegacyNameOnlyFreshRequestWithFailedVerificationStartsTheSeatUnnamed(t *testing.T) {
	spec, path := liveSeatManagedSpec(t, "gc-b")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	request := SessionRequest{Template: "worker", Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &spec}

	rows, _, stderr := realizeFreshRequest(t, request)
	if len(rows) != 0 || !strings.Contains(stderr, "verification failed") {
		t.Fatalf("control: the named request created %d sessions; stderr=%s; want the verification refusal and none", len(rows), stderr)
	}

	request.NameOnly = true
	rows, desired, stderr := realizeFreshRequest(t, request)
	if len(rows) != 1 {
		t.Fatalf("created %d sessions, want the seat; stderr=%s", len(rows), stderr)
	}
	seat := rows[0]
	if seat.TriggerBeadID != "" || preparedStartSessionWorkDir(seat) == path {
		t.Fatalf("seat trigger %q, work dir %q; want it unnamed, outside %s", seat.TriggerBeadID, preparedStartSessionWorkDir(seat), path)
	}
	if _, ok := desired[seat.SessionNameMetadata]; !ok {
		t.Fatalf("seat %s is not desired; desired=%v", seat.SessionNameMetadata, mapKeys(desired))
	}
	if !strings.Contains(stderr, "verification failed") || !strings.Contains(stderr, "(starting the seat unnamed)") {
		t.Fatalf("stderr = %q; want the verification refusal and the unnamed seat", stderr)
	}
}

// seatLeavesUnboundWork reports whether the seat's claim, reading its row as
// `gc hook --claim` does (hookClaimPoolSeatFor) and probing its directory as
// the claim does (hookClaimSeatInManagedWorktree), leaves work bound to no
// managed worktree.
func (res liveSeatReuseResult) seatLeavesUnboundWork(t *testing.T) bool {
	t.Helper()
	info, err := sessionFrontDoor(res.store).Get(res.id)
	if err != nil {
		t.Fatal(err)
	}
	seat := hookClaimPoolSeatFor(res.cityPath, "test-city", nil, &info, &res.cfg.Agents[0])
	return hookClaimSeatLeaves(unboundRoutedCandidate("gc-u"), seat, func() bool {
		managed, err := hookClaimSeatInManagedWorktree(seat.WorkDir)
		if err != nil {
			t.Fatalf("probing work dir %s: %v", seat.WorkDir, err)
		}
		return managed
	})
}

// A seat that is not alive starts in the directory its row records, so a
// name-only request whose work's worktree evidence is refused binds it unnamed
// in the pool's own directory, where a fresh unnamed seat starts and its claim
// takes the pool's unbound work. Left in the directory its last binding
// recorded, it would start in that managed worktree and leave that work
// (hookClaimUnboundWorkOutsideSeat), or start in the refused worktree itself.
// A live seat stays where it runs
// (TestLegacyReuseNameOnlyRequestWithUnusableEvidenceBindsTheSeatUnnamed).
func TestLegacyRefusedNameMovesASeatThatIsNotAliveToThePoolDir(t *testing.T) {
	unusable := newDemand(SessionRequest{Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeError: "spec unreadable", NameOnly: true})
	for _, tc := range []struct {
		name string
		seat func(t *testing.T) liveSeatReuse
	}{
		{name: "stopped seat", seat: func(t *testing.T) liveSeatReuse {
			_, pathA := liveSeatManagedSpec(t, "gc-a")
			return liveSeatReuse{seatDir: pathA, request: unusable}
		}},
		{name: "zombie seat", seat: func(t *testing.T) liveSeatReuse {
			_, pathA := liveSeatManagedSpec(t, "gc-a")
			return liveSeatReuse{seatDir: pathA, running: true, zombie: true, request: unusable}
		}},
		{name: "one_shot seat asleep in the worktree of the work it last served", seat: func(t *testing.T) liveSeatReuse {
			_, pathA := liveSeatManagedSpec(t, "gc-a")
			specB, pathB := liveSeatManagedSpec(t, "gc-b")
			if err := os.RemoveAll(pathB); err != nil {
				t.Fatal(err)
			}
			return liveSeatReuse{
				seatDir: pathA, oneShot: true, extra: map[string]string{"state": "asleep", "sleep_reason": "idle"},
				request: newDemand(SessionRequest{Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB, NameOnly: true}),
			}
		}},
		{name: "seat matched by identity to the refused work", seat: func(t *testing.T) liveSeatReuse {
			specA, pathA := liveSeatManagedSpec(t, "gc-a")
			if err := os.RemoveAll(pathA); err != nil {
				t.Fatal(err)
			}
			return liveSeatReuse{seatDir: pathA, request: func(id string) SessionRequest {
				return SessionRequest{Tier: "new", SessionBeadID: id, WorkBeadID: "gc-a", WorkStoreRef: "rig:r1", WorktreeSpec: &specA, NameOnly: true}
			}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.seat(t).reused(t)
			poolDir := res.cfg.Agents[0].WorkDir
			row := res.row(t)
			if got := row.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "" {
				t.Fatalf("trigger = %q; want the seat bound unnamed", got)
			}
			for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
				if got := row.Metadata[key]; got != poolDir {
					t.Fatalf("%s = %q; want %q, the pool's own directory, where the seat starts", key, got, poolDir)
				}
			}
			if rows := res.otherRows(t); len(rows) != 0 {
				t.Fatalf("started %d other sessions; want the seat reused", len(rows))
			}
			if res.seatLeavesUnboundWork(t) {
				t.Fatalf("the seat in %s leaves the pool's unbound work", poolDir)
			}
			if !strings.Contains(res.stderr, errPoolTriggerWorktreeEvidence.Error()) || !strings.Contains(res.stderr, "(binding the seat unnamed)") {
				t.Fatalf("stderr = %q; want the evidence refusal and the unnamed binding", res.stderr)
			}
		})
	}
}

// writeFailingStore fails every update, as a store whose writes are down
// does, and counts them.
type writeFailingStore struct {
	beads.Store
	updates *int
}

func (s writeFailingStore) Update(string, beads.UpdateOpts) error {
	*s.updates++
	return errors.New("store write failed")
}

// A failed binding write skips a name-only request's seat that is not alive
// (bindWriteFailure): the row still names the work its last binding left,
// where a seat kept desired would start. The named bind's failed write
// refuses no worktree evidence, so no unnamed bind retries it; the unnamed
// bind's failed write was to move the seat to the pool's own directory.
func TestLegacyNameOnlyBindWriteFailureSkipsTheSeat(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func(specB worktree.Spec) SessionRequest
		unnamed bool
	}{
		{name: "named bind", request: func(specB worktree.Spec) SessionRequest {
			return SessionRequest{Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB, NameOnly: true}
		}},
		{name: "unnamed bind of a refused name", unnamed: true, request: func(worktree.Spec) SessionRequest {
			return SessionRequest{Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeError: "spec unreadable", NameOnly: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, pathA := liveSeatManagedSpec(t, "gc-a")
			specB, _ := liveSeatManagedSpec(t, "gc-b")
			writes := 0
			res := liveSeatReuse{
				seatDir: pathA,
				store:   func(s beads.Store) beads.Store { return writeFailingStore{Store: s, updates: &writes} },
				request: newDemand(tc.request(specB)),
			}.run(t)
			if _, ok := res.base[res.seatName]; ok {
				t.Fatalf("the seat whose binding write failed is desired; stderr=%s", res.stderr)
			}
			if writes != 1 || !strings.Contains(res.stderr, "binding write failed") || !strings.Contains(res.stderr, "(skipping)") {
				t.Fatalf("%d writes, stderr = %q; want the one failed write, and the seat skipped", writes, res.stderr)
			}
			if got := strings.Contains(res.stderr, "(binding the seat unnamed)"); got != tc.unnamed {
				t.Fatalf("stderr = %q; unnamed bind = %v, want %v", res.stderr, got, tc.unnamed)
			}
		})
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
// allocateScaleDemandToConcrete rebinds that request to other work. When that
// work is bound to another worktree, the seat is not re-pointed at it: the
// work gets a fresh session there.
func TestLegacyProtectedSeatElsewhereLeavesReboundWorkToAFreshSession(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{seatDir: pathA, running: true, request: func(id string) SessionRequest {
		return SessionRequest{Tier: "new", SessionBeadID: id, WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB}
	}}.run(t)
	res.requireSeatNotRepointed(t, pathA)
	res.freshSession(t, "gc-b", pathB)
}

// liveSeatCity is a store holding one active pool seat "worker-1", running in
// seatDir and bound to gc-a, plus the realization inputs that compute and
// realize legacy pool demand for it.
type liveSeatCity struct {
	store    beads.Store
	sp       *runtime.Fake
	cityPath string
	cfg      *config.City
	seatID   string
	// drains counts, by session ID, the reconcile passes that found the
	// seat undesired and live.
	drains map[string]int
}

func newLiveSeatCity(t *testing.T, seatDir string, minSessions int, extra map[string]string) liveSeatCity {
	t.Helper()
	store := beads.NewMemStore()
	meta := map[string]string{
		"template": "worker", "agent_name": "worker-1", "alias": "worker-1", "session_name": "worker-1",
		"session_origin": "ephemeral", "state": "active", "pool_slot": "1",
		"instance_token": "tok", poolManagedMetadataKey: boolMetadata(true),
		beadmeta.TriggerBeadIDMetadataKey: "gc-a", beadmeta.TriggerBeadStoreRefMetadataKey: "rig:r1",
		beadmeta.WorkDirMetadataKey: seatDir, beadmeta.LegacyWorkDirMetadataKey: seatDir,
	}
	for k, v := range extra {
		meta[k] = v
	}
	seat, err := store.Create(beads.Bead{
		Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{WorkDir: seatDir}); err != nil {
		t.Fatal(err)
	}
	return liveSeatCity{store: store, sp: sp, cityPath: t.TempDir(), cfg: liveSeatAgent(t, minSessions, 3), seatID: seat.ID, drains: map[string]int{}}
}

// addSeat adds another active, idle pool seat, running in dir in slot and
// bound to trigger, and returns its session ID.
func (c liveSeatCity) addSeat(t *testing.T, name string, slot int, dir, trigger string) string {
	t.Helper()
	seat, err := c.store.Create(beads.Bead{
		Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template": "worker", "agent_name": name, "alias": name, "session_name": name,
			"session_origin": "ephemeral", "state": "active", "pool_slot": strconv.Itoa(slot),
			"instance_token": "tok-" + name, poolManagedMetadataKey: boolMetadata(true),
			beadmeta.TriggerBeadIDMetadataKey: trigger, beadmeta.TriggerBeadStoreRefMetadataKey: "rig:r1",
			beadmeta.WorkDirMetadataKey: dir, beadmeta.LegacyWorkDirMetadataKey: dir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.sp.Start(context.Background(), name, runtime.Config{WorkDir: dir}); err != nil {
		t.Fatal(err)
	}
	return seat.ID
}

// tick computes legacy pool demand for the bead of work whose managed
// worktree is spec from the session rows the store holds now, the
// min_active_sessions floor fill included, realizes it, and applies the
// session overlay, as buildDesiredState does.
func (c liveSeatCity) tick(t *testing.T, now time.Time, spec *worktree.Spec) liveSeatReuseResult {
	t.Helper()
	workID := spec.BeadID
	snapshot, err := loadSessionBeadSnapshot(c.store)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", c.cityPath, c.cfg, c.sp, now, c.store, &stderr)
	bp.sessionBeads = snapshot
	demand := map[string]scaleCheckDemand{"worker": {
		Count:         1,
		WorkBeadIDs:   []string{workID},
		StoreRefs:     map[string]string{workID: "rig:r1"},
		WorktreeSpecs: map[string]*worktree.Spec{workID: spec},
	}}
	desired := map[string]TemplateParams{}
	for _, state := range computePoolDesiredStatesAt(c.cfg, nil, snapshot.OpenInfos(), map[string]int{"worker": 1}, demand, now, nil) {
		realizePoolDesiredSessionsAt(bp, &c.cfg.Agents[0], state, now, desired, &stderr)
	}
	base := cloneDesiredState(desired)
	discoverSessionBeads(bp, c.cfg, desired, &stderr)
	return liveSeatReuseResult{
		store: c.store, sp: c.sp, cityPath: c.cityPath, id: c.seatID, seatName: "worker-1", cfg: c.cfg,
		base: base, desired: desired, retired: bp.retiredPoolSeats, stderr: stderr.String(),
	}
}

// reconcile does, to the session rows the store holds now, what the
// reconciler does with desired. A live seat no desired state wants is
// drained as orphaned, a drain the min_active_sessions floor does not veto
// (TestCancelSelfInitiatedDrainAckAtMinFloorRefusals): the first pass only
// begins it (the drain lives in memory, the row stays active), the second
// marks the row draining once the drain is acknowledged, and the third stops
// the runtime, leaving the row asleep for the drain's reason. A row no desired
// state wants and nothing runs is closed.
func (c liveSeatCity) reconcile(t *testing.T, desired map[string]TemplateParams) {
	t.Helper()
	snapshot, err := loadSessionBeadSnapshot(c.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range snapshot.OpenInfos() {
		name := info.SessionNameMetadata
		if _, ok := desired[name]; ok {
			continue
		}
		if !c.sp.IsRunning(name) {
			if err := c.store.Close(info.ID); err != nil {
				t.Fatal(err)
			}
			continue
		}
		c.drains[info.ID]++
		switch c.drains[info.ID] {
		case 1:
		case 2:
			if err := c.store.SetMetadataBatch(info.ID, map[string]string{"state": string(session.StateDraining), "state_reason": session.DrainAckStopPendingReason}); err != nil {
				t.Fatal(err)
			}
		default:
			drainSeatOrphaned(t, c.store, c.sp, info.ID, name)
		}
	}
}

// End to end from demand: a seat started 30s ago in A, whose trigger gc-a is
// no longer demanded, has its protected request rebound to gc-b (bound to B).
// The seat is not re-pointed at gc-b; a fresh session in B claims it.
func TestLegacyProtectedRebindWorkIsClaimedByAFreshSession(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	now := time.Now().UTC()
	city := newLiveSeatCity(t, pathA, 0, map[string]string{
		"state_reason": "creation_complete", "creation_complete_at": now.Add(-30 * time.Second).Format(time.RFC3339),
	})
	res := city.tick(t, now, &specB)
	res.requireSeatNotRepointed(t, pathA)
	fresh := res.freshSession(t, "gc-b", pathB)
	if claimed, out := res.claimFrom(t, fresh.ID, managedWorktreeCandidate("gc-b", pathB)); len(claimed) != 1 || claimed[0] != "gc-b" {
		t.Fatalf("fresh session in %s claimed %v; want gc-b; output=%s", pathB, claimed, out)
	}
	if claimed, out := res.claimFrom(t, city.seatID, managedWorktreeCandidate("gc-b", pathB)); len(claimed) != 0 {
		t.Fatalf("seat in %s claimed %v; want gc-b left to the session in %s; output=%s", pathA, claimed, pathB, out)
	}
}

// A min_active_sessions floor seat running in one bead's worktree is never
// re-pointed at work bound to another, tick after tick: the work gets one
// fresh session in its own worktree, which claims it, and the floor seat's
// row stays bound to its own trigger in its own worktree. (Re-pointing it
// instead would leave it bound to work its claim leaves, with its drain-ack
// canceled at the floor every time.)
func TestLegacyFloorSeatElsewhereIsNotRepointedAcrossTicks(t *testing.T) {
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	city := newLiveSeatCity(t, pathA, 1, nil)
	now := time.Now().UTC()
	var res liveSeatReuseResult
	for tick := 1; tick <= 3; tick++ {
		res = city.tick(t, now.Add(time.Duration(tick)*time.Minute), &specB)
		row := res.row(t)
		if got := row.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "gc-a" {
			t.Fatalf("tick %d: floor seat trigger = %q; want gc-a, never re-pointed at gc-b; stderr=%s", tick, got, res.stderr)
		}
		if got := row.Metadata[beadmeta.WorkDirMetadataKey]; got != pathA {
			t.Fatalf("tick %d: floor seat gc.work_dir = %q; want %q", tick, got, pathA)
		}
	}
	fresh := res.freshSession(t, "gc-b", pathB)
	if claimed, out := res.claimFrom(t, fresh.ID, managedWorktreeCandidate("gc-b", pathB)); len(claimed) != 1 || claimed[0] != "gc-b" {
		t.Fatalf("fresh session in %s claimed %v; want gc-b; output=%s", pathB, claimed, out)
	}
}

// newFullFloorCity is a pool whose min_active_sessions floor holds every
// slot (min == max == 2): worker-1 idles in pathA1's worktree and worker-2,
// started after it, in pathA2's. It returns the city and worker-2's ID.
func newFullFloorCity(t *testing.T, pathA1, pathA2 string) (liveSeatCity, string) {
	t.Helper()
	city := newLiveSeatCity(t, pathA1, 2, nil)
	city.cfg.Agents[0].MaxActiveSessions = intPtr(2)
	return city, city.addSeat(t, "worker-2", 2, pathA2, "gc-a2")
}

// A min_active_sessions floor that holds every slot, its seats idle in other
// beads' worktrees, does not leave work bound to a third worktree waiting
// forever. The oldest seat it passes over retires: it leaves the desired set,
// the floor fill and the session overlay included, while the reconciler drains
// it and until its row closes. Then the work gets a fresh session in its own
// worktree. The other floor seat stays desired and running throughout, and
// never retires, not even while the first is draining or asleep.
func TestLegacyFullFloorRetiresASeatElsewhereSoManagedWorkGetsASession(t *testing.T) {
	_, pathA1 := liveSeatManagedSpec(t, "gc-a")
	_, pathA2 := liveSeatManagedSpec(t, "gc-a2")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	city, second := newFullFloorCity(t, pathA1, pathA2)
	now := time.Now().UTC()
	for tick := 1; tick <= 6; tick++ {
		res := city.tick(t, now.Add(time.Duration(tick)*time.Minute), &specB)
		if _, ok := res.desired["worker-2"]; !ok || !city.sp.IsRunning("worker-2") {
			t.Fatalf("tick %d: the other floor seat is not desired and running; desired=%v stderr=%s", tick, mapKeys(res.desired), res.stderr)
		}
		if res.retired[second] {
			t.Fatalf("tick %d: retired the other floor seat too; retired=%v stderr=%s", tick, res.retired, res.stderr)
		}
		for _, info := range res.otherRows(t) {
			if info.ID == second {
				continue
			}
			if info.TriggerBeadID != "gc-b" || preparedStartSessionWorkDir(info) != pathB {
				t.Fatalf("tick %d: session %s bound to %q in %q; want only a fresh session for gc-b in %s", tick, info.ID, info.TriggerBeadID, preparedStartSessionWorkDir(info), pathB)
			}
			if _, ok := res.desired[info.SessionNameMetadata]; !ok {
				t.Fatalf("tick %d: the fresh session for gc-b is not desired; desired=%v", tick, mapKeys(res.desired))
			}
			if tick < 5 {
				t.Fatalf("tick %d: a fresh session started before the retired seat's row closed", tick)
			}
			return
		}
		res.requireSeatRetired(t)
		if tick == 1 && !strings.Contains(res.stderr, "freeing a slot: session "+city.seatID) {
			t.Fatalf("tick 1: stderr = %q; want the retirement named", res.stderr)
		}
		city.reconcile(t, res.desired)
	}
	t.Fatal("work bound to gc-b's worktree got no session in 6 ticks of a full floor")
}

// The per-tick refresh re-applies the session overlay with fresh build
// params. A seat the build retired stays out of the desired set it refreshes,
// where it would be back without DesiredStateResult.RetiredPoolSeats.
func TestRefreshDesiredStateKeepsARetiredPoolSeatOut(t *testing.T) {
	_, pathA1 := liveSeatManagedSpec(t, "gc-a")
	_, pathA2 := liveSeatManagedSpec(t, "gc-a2")
	specB, _ := liveSeatManagedSpec(t, "gc-b")
	city, _ := newFullFloorCity(t, pathA1, pathA2)
	res := city.tick(t, time.Now().UTC(), &specB)
	res.requireSeatRetired(t)
	refresh := func(retired map[string]bool) map[string]TemplateParams {
		t.Helper()
		snapshot, err := loadSessionBeadSnapshot(city.store)
		if err != nil {
			t.Fatal(err)
		}
		result := DesiredStateResult{State: res.desired, BaseState: res.base, RetiredPoolSeats: retired, BeaconTime: time.Now().UTC()}
		return refreshDesiredStateWithSessionBeads(result, "test-city", city.cityPath, city.cfg, city.sp, city.store, snapshot, io.Discard).State
	}
	if _, ok := refresh(res.retired)["worker-1"]; ok {
		t.Fatal("the refresh put the retired seat back in the desired set")
	}
	if _, ok := refresh(nil)["worker-1"]; !ok {
		t.Fatal("without the retired seats the refresh leaves worker-1 out too; the overlay no longer re-adds a sibling of a desired seat, so this test proves nothing")
	}
}

// poolSeatLeaving tells a seat whose slot frees by itself (draining, drained,
// asleep with no hold, asleep after an orphaned drain) from one that keeps
// its slot (active, held asleep, suspended, or holding work).
func TestPoolSeatLeaving(t *testing.T) {
	now := time.Now().UTC()
	slept := now.Add(-time.Minute).Format(time.RFC3339)
	bp := &agentBuildParams{assignedWorkBeads: []beads.Bead{workBead("gc-z", "worker", "s-busy", "in_progress", 1)}}
	for _, tc := range []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"active", map[string]string{"state": "active"}, false},
		{"draining", map[string]string{"state": "draining", "state_reason": session.DrainAckStopPendingReason}, true},
		{"drained", map[string]string{"state": "drained"}, true},
		{"asleep after an orphaned drain", session.CompleteDrainPatch(now, drainOrphaned, false), true},
		{"asleep idle", map[string]string{"state": "asleep", "sleep_reason": "idle", "slept_at": slept}, true},
		{"asleep on a user hold", map[string]string{"state": "asleep", "sleep_reason": "user-hold", "slept_at": slept}, false},
		{"asleep with no wake reason", map[string]string{"state": "asleep", "sleep_reason": "no-wake-reason", "slept_at": slept}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := map[string]string{"session_name": "worker-1", "template": "worker"}
			for k, v := range tc.meta {
				meta[k] = v
			}
			info := sessiontest.SeedBead(t, beads.Bead{ID: "s-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
			if got := poolSeatLeaving(bp, info); got != tc.want {
				t.Fatalf("poolSeatLeaving(%v) = %v; want %v", tc.meta, got, tc.want)
			}
		})
	}
	t.Run("draining while holding work", func(t *testing.T) {
		info := sessiontest.SeedBead(t, beads.Bead{ID: "s-busy", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "s-busy", "template": "worker", "state": "draining",
		}})
		if poolSeatLeaving(bp, info) {
			t.Fatal("a draining seat holding assigned work is leaving; want it kept")
		}
	})
}
