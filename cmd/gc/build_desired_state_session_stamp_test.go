package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// countingStore wraps a Store and counts SetMetadataBatch calls so a test can
// assert the stamp is idempotent (no writes once the bead already carries the
// resolved identity).
type countingStore struct {
	beads.Store
	writes int
}

func (c *countingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	c.writes++
	return c.Store.SetMetadataBatch(id, kvs)
}

func stampTestSession(name, workDir string) beads.Bead {
	return beads.Bead{
		ID:     "sess-" + name,
		Type:   "session",
		Status: "open",
		Metadata: map[string]string{
			"session_name": name,
			"work_dir":     workDir,
		},
	}
}

func TestStampRunSessionIdentityStampsInProgressAssignedBead(t *testing.T) {
	const sessionName = "codeprobe-worker-gc-1920"
	const workDir = "/home/ds/projects/codeprobe/codeprobe-worker-1"

	run := beads.Bead{ID: "co-run1", Type: "molecule", Status: "in_progress", Assignee: sessionName}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession(sessionName, workDir)})

	stampRunSessionIdentity(gaConfig(), []beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

	got, err := mem.Get("co-run1")
	if err != nil {
		t.Fatalf("Get(co-run1): %v", err)
	}
	if got.Metadata["gc.session_name"] != sessionName {
		t.Errorf("gc.session_name = %q, want %q", got.Metadata["gc.session_name"], sessionName)
	}
	if got.Metadata["gc.work_dir"] != workDir {
		t.Errorf("gc.work_dir = %q, want %q", got.Metadata["gc.work_dir"], workDir)
	}

	// Idempotent: a second pass over the now-stamped bead writes nothing.
	stamped, _ := mem.Get("co-run1")
	store.writes = 0
	stampRunSessionIdentity(gaConfig(), []beads.Bead{stamped}, []beads.Store{store}, sessions, io.Discard)
	if store.writes != 0 {
		t.Errorf("second pass wrote %d times, want 0 (stamp must be idempotent)", store.writes)
	}
}

func TestStampRunSessionIdentityDoesNotManufactureWorktreeEvidence(t *testing.T) {
	const (
		sessionName = "polecat-gc-734732"
		slotDir     = "/home/ds/gascity-worktrees/polecat-slots/polecat-2"
	)
	run := beads.Bead{ID: "gc-demand", Type: "task", Status: "in_progress", Assignee: sessionName}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}
	poolSession := stampTestSession(sessionName, slotDir)
	poolSession.Metadata["pool_managed"] = "true"
	sessions := newSessionBeadSnapshot([]beads.Bead{poolSession})

	stampRunSessionIdentity(gaConfig(), []beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

	got, err := mem.Get(run.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", run.ID, err)
	}
	if got.Metadata["gc.session_name"] != sessionName {
		t.Fatalf("gc.session_name = %q, want %q", got.Metadata["gc.session_name"], sessionName)
	}
	if value, exists := got.Metadata["gc.work_dir"]; exists {
		t.Fatalf("gc.work_dir was manufactured as %q from a pool slot; worktree ownership evidence must come from the worktree creator", value)
	}
}

func TestStampRunSessionIdentityPropagatesToRunRoot(t *testing.T) {
	// #2843: a worked in-progress STEP back-fills its workflow ROOT (which the
	// dashboard's root-only snapshot reads). The root is a control-lane bead,
	// never in_progress+assigned, so it is reached only via gc.root_bead_id.
	const sn = "gascity-packs-polecat-gc-1"
	const wd = "/home/ds/gascity-packs-worktrees/gascity-packs-polecat-1"
	root := beads.Bead{ID: "gpk-root", Type: "molecule", Status: "in_progress", Metadata: map[string]string{"gc.kind": "workflow"}}
	step := beads.Bead{ID: "gpk-step", Type: "step", Status: "in_progress", Assignee: sn, Metadata: map[string]string{"gc.step_ref": "wf.work", "gc.root_bead_id": "gpk-root"}}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
	store := &countingStore{Store: mem}
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession(sn, wd)})

	stampRunSessionIdentity(gaConfig(), []beads.Bead{step}, []beads.Store{store}, sessions, io.Discard)

	gotStep, _ := mem.Get("gpk-step")
	if gotStep.Metadata["gc.session_name"] != sn || gotStep.Metadata["gc.work_dir"] != wd {
		t.Errorf("step not stamped: session_name=%q work_dir=%q", gotStep.Metadata["gc.session_name"], gotStep.Metadata["gc.work_dir"])
	}
	gotRoot, _ := mem.Get("gpk-root")
	if gotRoot.Metadata["gc.session_name"] != sn {
		t.Errorf("root gc.session_name = %q, want %q (propagated from step)", gotRoot.Metadata["gc.session_name"], sn)
	}
	if gotRoot.Metadata["gc.work_dir"] != wd {
		t.Errorf("root gc.work_dir = %q, want %q (propagated from step)", gotRoot.Metadata["gc.work_dir"], wd)
	}

	// Idempotent: a second pass writes nothing (step + root already stamped).
	stamped, _ := mem.Get("gpk-step")
	store.writes = 0
	stampRunSessionIdentity(gaConfig(), []beads.Bead{stamped}, []beads.Store{store}, sessions, io.Discard)
	if store.writes != 0 {
		t.Errorf("second pass wrote %d times, want 0 (step+root already stamped)", store.writes)
	}
}

func TestStampRunSessionIdentityNamedSessionUsesAlias(t *testing.T) {
	// Named sessions (e.g. mayor) carry an empty session_name; their
	// resolvable identifier lives in alias / configured_named_identity.
	mayor := beads.Bead{
		ID: "sess-mayor", Type: "session", Status: "open",
		Metadata: map[string]string{
			"session_name": "", "alias": "mayor",
			"configured_named_identity": "mayor", "work_dir": "/home/ds/gas-city",
		},
	}
	run := beads.Bead{ID: "dr-run", Type: "molecule", Status: "in_progress", Assignee: "mayor"}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}
	sessions := newSessionBeadSnapshot([]beads.Bead{mayor})

	stampRunSessionIdentity(gaConfig(), []beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

	got, _ := mem.Get("dr-run")
	if got.Metadata["gc.session_name"] != "mayor" {
		t.Errorf("gc.session_name = %q, want %q (alias fallback)", got.Metadata["gc.session_name"], "mayor")
	}
	if got.Metadata["gc.work_dir"] != "/home/ds/gas-city" {
		t.Errorf("gc.work_dir = %q, want /home/ds/gas-city", got.Metadata["gc.work_dir"])
	}
}

func TestStampRunSessionIdentityResolvesByIDAndAliasHistory(t *testing.T) {
	cases := map[string]struct {
		assignee string
		sess     beads.Bead
		wantName string
	}{
		"by bead ID": {
			assignee: "sess-pool",
			sess: beads.Bead{
				ID: "sess-pool", Type: "session", Status: "open",
				Metadata: map[string]string{"session_name": "pool-gc-9", "work_dir": "/wt/9"},
			},
			wantName: "pool-gc-9",
		},
		"by rotated alias_history": {
			// A pool worker whose alias rotated mid-run: the bead's Assignee is
			// a prior alias, still a live assignment identity.
			assignee: "old-alias",
			sess: beads.Bead{
				ID: "sess-rot", Type: "session", Status: "open",
				Metadata: map[string]string{
					"session_name": "pool-gc-10", "alias": "new-alias",
					"alias_history": "old-alias", "work_dir": "/wt/10",
				},
			},
			wantName: "pool-gc-10",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			run := beads.Bead{ID: "r", Type: "molecule", Status: "in_progress", Assignee: tc.assignee}
			mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
			store := &countingStore{Store: mem}
			sessions := newSessionBeadSnapshot([]beads.Bead{tc.sess})

			stampRunSessionIdentity(gaConfig(), []beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

			got, _ := mem.Get("r")
			if got.Metadata["gc.session_name"] != tc.wantName {
				t.Errorf("gc.session_name = %q, want %q", got.Metadata["gc.session_name"], tc.wantName)
			}
		})
	}
}

func TestStampRunSessionIdentitySkipsAmbiguousAssignee(t *testing.T) {
	// Two open sessions claim the same identity ("dupe") — a transient
	// duplicate-alias state. The stamp must skip rather than guess.
	a := beads.Bead{ID: "sa", Type: "session", Status: "open", Metadata: map[string]string{"alias": "dupe", "work_dir": "/a"}}
	b := beads.Bead{ID: "sb", Type: "session", Status: "open", Metadata: map[string]string{"alias": "dupe", "work_dir": "/b"}}
	run := beads.Bead{ID: "r", Type: "molecule", Status: "in_progress", Assignee: "dupe"}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}
	sessions := newSessionBeadSnapshot([]beads.Bead{a, b})

	stampRunSessionIdentity(gaConfig(), []beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

	if store.writes != 0 {
		t.Errorf("ambiguous assignee must not be stamped, got %d writes", store.writes)
	}
}

func TestStampRunSessionIdentityReassignmentRestamps(t *testing.T) {
	run := beads.Bead{
		ID: "co-run2", Type: "molecule", Status: "in_progress", Assignee: "worker-b",
		Metadata: map[string]string{"gc.session_name": "worker-a", "gc.work_dir": "/old"},
	}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession("worker-b", "/new")})

	stampRunSessionIdentity(gaConfig(), []beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

	got, _ := mem.Get("co-run2")
	if got.Metadata["gc.session_name"] != "worker-b" || got.Metadata["gc.work_dir"] != "/new" {
		t.Errorf("reassignment not restamped: session_name=%q work_dir=%q",
			got.Metadata["gc.session_name"], got.Metadata["gc.work_dir"])
	}
}

func TestStampRunSessionIdentitySkipsNonExecuting(t *testing.T) {
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession("worker-x", "/wd")})

	cases := map[string]beads.Bead{
		"closed bead":        {ID: "b1", Status: "closed", Assignee: "worker-x"},
		"open (not claimed)": {ID: "b2", Status: "open", Assignee: "worker-x"},
		"no assignee":        {ID: "b3", Status: "in_progress", Assignee: ""},
		"unknown session":    {ID: "b4", Status: "in_progress", Assignee: "ghost"},
	}
	for name, wb := range cases {
		t.Run(name, func(t *testing.T) {
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &countingStore{Store: mem}
			stampRunSessionIdentity(gaConfig(), []beads.Bead{wb}, []beads.Store{store}, sessions, io.Discard)
			if store.writes != 0 {
				t.Errorf("expected no stamp, got %d writes", store.writes)
			}
		})
	}
}

func TestStampRunSessionIdentityToleratesLengthMismatchAndNilSnapshot(t *testing.T) {
	run := beads.Bead{ID: "b", Status: "in_progress", Assignee: "w"}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}

	// Mismatched slice lengths must be a no-op, not a panic.
	stampRunSessionIdentity(gaConfig(), []beads.Bead{run}, []beads.Store{}, newSessionBeadSnapshot(nil), io.Discard)
	// Nil snapshot must be a no-op, not a panic.
	stampRunSessionIdentity(gaConfig(), []beads.Bead{run}, []beads.Store{store}, nil, io.Discard)
	if store.writes != 0 {
		t.Errorf("expected no writes on degenerate input, got %d", store.writes)
	}
}

func TestStampRunSessionIdentityPreservesRealEvidenceAgainstPoolSlotSelfCwd(t *testing.T) {
	// ga-3c5isi / #5193: a session whose OWN bead never carries
	// pool_managed=true (a manual/named session, or a pool session mid
	// classification) can still be physically running from a pool slot's own
	// worktree root. Before this guard, !sbInfo.PoolManaged alone satisfied
	// the stamp condition and gc.work_dir was overwritten unconditionally
	// from that session's cwd -- clobbering real per-bead worktree evidence
	// already on the work bead with a pool-slot label.
	const (
		sessionName  = "gascity--builder-1"
		poolSlotSelf = "/home/jaword/projects/gc-management/.gc/worktrees/gascity/builder-1"
		realEvidence = "/home/ds/gascity-worktrees/ga-3c5isi"
	)
	run := beads.Bead{
		ID: "ga-3c5isi", Type: "task", Status: "in_progress", Assignee: sessionName,
		Metadata: map[string]string{"gc.work_dir": realEvidence},
	}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}
	// Not pool_managed: stampTestSession leaves pool_managed unset/false.
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession(sessionName, poolSlotSelf)})

	stampRunSessionIdentity(gaConfig(), []beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

	got, err := mem.Get(run.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", run.ID, err)
	}
	if got.Metadata["gc.work_dir"] != realEvidence {
		t.Fatalf("gc.work_dir = %q, want %q (real worktree evidence must survive a pool-slot-shaped self cwd)", got.Metadata["gc.work_dir"], realEvidence)
	}
}

func TestStampRunRootFromStepPreservesRootEvidenceAgainstPoolSlotCwd(t *testing.T) {
	// The root back-fill carries the same guard as the step stamp, and needs
	// its own behavior coverage: a non-pool session running from a pool slot
	// satisfies allowUnownedWorkDir, so without workDirStampWouldClobberEvidence
	// the root's real per-bead worktree evidence is overwritten with the slot
	// label -- and the root is the bead the dashboard's root-only snapshot reads.
	const (
		sessionName  = "gascity--builder-1"
		poolSlotSelf = "/home/jaword/projects/gc-management/.gc/worktrees/gascity/builder-1"
		realEvidence = "/home/ds/gascity-worktrees/ga-3c5isi"
	)
	root := beads.Bead{
		ID: "ga-root", Type: "molecule", Status: "in_progress",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.work_dir": realEvidence},
	}
	step := beads.Bead{
		ID: "ga-step", Type: "step", Status: "in_progress", Assignee: sessionName,
		Metadata: map[string]string{"gc.root_bead_id": "ga-root"},
	}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
	store := &countingStore{Store: mem}

	stampRunRootFromStep(gaConfig(), store, step, sessionName, poolSlotSelf, true, map[string]struct{}{}, io.Discard)

	gotRoot, err := mem.Get("ga-root")
	if err != nil {
		t.Fatalf("Get(ga-root): %v", err)
	}
	if gotRoot.Metadata["gc.work_dir"] != realEvidence {
		t.Errorf("root gc.work_dir = %q, want %q (real worktree evidence must survive a pool-slot cwd)",
			gotRoot.Metadata["gc.work_dir"], realEvidence)
	}
	// The session_name half of the back-fill is unaffected by the work_dir guard.
	if gotRoot.Metadata["gc.session_name"] != sessionName {
		t.Errorf("root gc.session_name = %q, want %q", gotRoot.Metadata["gc.session_name"], sessionName)
	}
}

func TestRepairPoolSlotWorkDirClobberThenStampPreservesLiveWorkDir(t *testing.T) {
	// Ordering regression: the reconciler collects the work slice once and
	// neither pass refreshes it, so a repair sweep placed AFTER
	// stampRunSessionIdentity re-reads pre-stamp metadata and reverts the
	// freshly stamped canonical back to the stale legacy value.
	// Reconciliation runs repair first, then the live stamp, so the session's
	// real path survives one full pass.
	const (
		sessionName = "gascity--worker-gc-7"
		liveWorkDir = "/home/ds/gascity-worktrees/ga-live"
		staleLegacy = "/home/ds/gascity-worktrees/ga-stale"
		poolSlot    = ".gc/worktrees/gascity/builder-1"
	)
	newRun := func() beads.Bead {
		return beads.Bead{
			ID: "ga-run", Type: "task", Status: "in_progress", Assignee: sessionName,
			Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       poolSlot,
				beadmeta.LegacyWorkDirMetadataKey: staleLegacy,
			},
		}
	}
	// The store's bead and the collected snapshot must not share a metadata
	// map: a store query returns clones, so writes through the store are
	// invisible to the already-collected slice. MemStore seeded from the same
	// value would alias the map and mask the ordering bug entirely.
	mem := beads.NewMemStoreFrom(0, []beads.Bead{newRun()}, nil)
	store := &countingStore{Store: mem}
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession(sessionName, liveWorkDir)})
	cfg := gaConfig()

	// Same order, and the same collected-once slice, as
	// buildDesiredStateWithSessionBeadsAt.
	snapshot := []beads.Bead{newRun()}
	stores := []beads.Store{store}
	repairPoolSlotWorkDirClobber(cfg, snapshot, stores, io.Discard)
	stampRunSessionIdentity(cfg, snapshot, stores, sessions, io.Discard)

	got, err := mem.Get("ga-run")
	if err != nil {
		t.Fatalf("Get(ga-run): %v", err)
	}
	if got.Metadata[beadmeta.WorkDirMetadataKey] != liveWorkDir {
		t.Errorf("gc.work_dir = %q, want %q (live stamp must get the last word over the repair sweep)",
			got.Metadata[beadmeta.WorkDirMetadataKey], liveWorkDir)
	}
}
