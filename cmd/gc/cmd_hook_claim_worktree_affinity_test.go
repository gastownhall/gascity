package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worktree"
)

const affinityRoute = "r1/dev"

// managedWorktreeCandidate is a routed, unassigned pool step that publishes the
// complete managed-worktree evidence worktreeSpecForBead turns into a verified
// spec: gc.work_dir plus gc.work_branch and the eight ownership keys. The
// controller starts the pool seat it counts for such a bead inside workDir
// (verifiedPoolTriggerWorkDir).
func managedWorktreeCandidate(id, workDir string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to":                         affinityRoute,
			beadmeta.RootStoreRefMetadataKey:       "rig:r1",
			beadmeta.WorkDirMetadataKey:            workDir,
			beadmeta.WorktreeRepoMetadataKey:       "/rigs/r1",
			beadmeta.WorktreeRootMetadataKey:       "/wt/r1",
			beadmeta.WorkBranchMetadataKey:         "wt/task/" + id,
			beadmeta.WorktreeBaseRefMetadataKey:    "main",
			beadmeta.WorktreeBaseSHAMetadataKey:    "4a43c5650000000000000000000000000000000",
			beadmeta.WorktreeCreatorMetadataKey:    "provisioner",
			beadmeta.WorktreeOwnerMetadataKey:      "provisioner",
			beadmeta.WorktreeGenerationMetadataKey: "g1",
			beadmeta.WorktreeLifecycleMetadataKey:  "active",
		},
	}
}

func unboundRoutedCandidate(id string) beads.Bead {
	return beads.Bead{ID: id, Status: "open", Metadata: map[string]string{"gc.routed_to": affinityRoute}}
}

// affinityClaimOps stubs every session- and store-touching seam a claim by a
// session-run seat reaches, so a case observes only which candidates the claim
// CAS (and the stale reclaim) were attempted on.
func affinityClaimOps(t *testing.T, candidates []beads.Bead, claimed, reclaimed *[]string) hookClaimOps {
	t.Helper()
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			*claimed = append(*claimed, beadID)
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		ReclaimStale: func(_ context.Context, _ string, _ []string, beadID string) (bool, string, error) {
			*reclaimed = append(*reclaimed, beadID)
			return true, "dead-seat", nil
		},
		DrainAck:              func(io.Writer) error { return nil },
		DrainPending:          func(string) (bool, error) { return false, nil },
		ResolveSessionWorkDir: func(string) string { return "" },
		SeatInManagedWorktree: func(string) bool { return false },
		ResolveWorkBranch:     func(hookClaimWorkTree) string { return "" },
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			return nil
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, error) {
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, nil
		},
		RestampAdopted: func(context.Context, string, []string, string, string, string) (bool, error) {
			return true, nil
		},
		StampSessionClaim: func(string, string) error { return nil },
		RecordWorkStarted: func(string, hookStartedFence, hookStartedKey, string) (hookStartedResult, error) {
			return hookStartedRecorded, nil
		},
		ForgetWorkStarted:           func(string, hookStartedKey) error { return nil },
		ReadWorkStarted:             func(string) (string, error) { return "", nil },
		StoreRef:                    func(string) string { return "rig:r1" },
		EmitWorkStartedUnrecorded:   func(string, string, string, error) {},
		EmitExecutionStepStarted:    func(beads.Bead, string, []string, string) {},
		PublishRunMap:               func(string, string, ...string) error { return nil },
		EnqueueContinuationNudge:    func(string, string, io.Writer) {},
		EmitClaimRejected:           func(string, string, string) {},
		EmitClaimFailed:             func(string, hookClaimOptions, error) {},
		EmitHookClaimReclaimedStale: func(string, string, string) {},
	}
}

func affinitySeatOpts(seatWorkDir string) hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "r1/dev-2",
		SessionID:          "s-2",
		IdentityCandidates: []string{"r1/dev-2", "s-2"},
		RouteTargets:       []string{affinityRoute},
		Env:                []string{"GC_SESSION_ID=s-2"},
		JSON:               true,
		DrainAck:           true,
		PoolSeatWorkDir:    seatWorkDir,
	}
}

// Two managed steps are ready for one pool and the controller started one seat
// per step inside that step's verified worktree. The seat in r1-b's worktree
// must take r1-b, not the older r1-a that sits ahead of it in the routed
// window: r1-a's work must run in /wt/r1/r1-a, where its own seat is.
func TestHookClaimLeavesAnotherSeatsVerifiedWorktreeToIt(t *testing.T) {
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{
		managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"),
		managedWorktreeCandidate("r1-b", "/wt/r1/r1-b"),
	}, &claimed, &reclaimed)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rigs/r1", affinitySeatOpts("/wt/r1/r1-b"), ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.Join(claimed, ","); got != "r1-b" {
		t.Fatalf("claim CAS attempted on %q, want only r1-b (the seat's own verified worktree)", got)
	}
	if !strings.Contains(stdout.String(), `"bead_id":"r1-b"`) {
		t.Fatalf("claim result = %s, want bead_id r1-b", stdout.String())
	}
}

// A pool seat started anywhere but a bead's verified worktree (a slot
// directory, a rig root) takes routed work that names no worktree, and leaves
// worktree-bound work to the seat started inside it; with nothing else ready
// it drains no_work rather than run that work in the wrong tree.
func TestHookClaimPoolSeatOutsideEveryVerifiedWorktreeTakesOnlyUnboundWork(t *testing.T) {
	t.Run("unbound work behind bound work is claimed", func(t *testing.T) {
		var claimed, reclaimed []string
		ops := affinityClaimOps(t, []beads.Bead{
			managedWorktreeCandidate("r1-b", "/wt/r1/r1-b"),
			unboundRoutedCandidate("r1-u"),
		}, &claimed, &reclaimed)
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("query", "/rigs/r1", affinitySeatOpts("/rigs/r1/.gc/slots/dev-3"), ops, &stdout, &stderr); code != 0 {
			t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
		}
		if got := strings.Join(claimed, ","); got != "r1-u" {
			t.Fatalf("claim CAS attempted on %q, want only r1-u", got)
		}
	})
	t.Run("only bound work drains no_work", func(t *testing.T) {
		var claimed, reclaimed []string
		ops := affinityClaimOps(t, []beads.Bead{
			managedWorktreeCandidate("r1-b", "/wt/r1/r1-b"),
		}, &claimed, &reclaimed)
		var stdout, stderr bytes.Buffer
		doHookClaim("query", "/rigs/r1", affinitySeatOpts("/rigs/r1"), ops, &stdout, &stderr)
		if len(claimed) != 0 {
			t.Fatalf("claim CAS attempted on %v, want none", claimed)
		}
		if !strings.Contains(stdout.String(), `"action":"drain"`) || !strings.Contains(stdout.String(), `"reason":"no_work"`) {
			t.Fatalf("result = %s, want a no_work drain", stdout.String())
		}
	})
}

// A claimant with no pool-seat directory (a named or manual session, a
// session-less hook, or a row the fence could not read) keeps the behavior from
// before the check: it claims the first routed candidate.
func TestHookClaimWorktreeCheckIsOffWithoutPoolSeatWorkDir(t *testing.T) {
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{
		managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"),
		managedWorktreeCandidate("r1-b", "/wt/r1/r1-b"),
	}, &claimed, &reclaimed)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("query", "/rigs/r1", affinitySeatOpts(""), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.Join(claimed, ","); got != "r1-a" {
		t.Fatalf("claim CAS attempted on %q, want r1-a (check off)", got)
	}
}

// The opt-in stale reclaim must not free a bead this seat would then skip.
func TestHookClaimWorktreeCheckRunsBeforeStaleReclaim(t *testing.T) {
	stale := managedWorktreeCandidate("r1-a", "/wt/r1/r1-a")
	stale.Assignee = "dead-seat"
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{stale}, &claimed, &reclaimed)
	opts := affinitySeatOpts("/wt/r1/r1-b")
	opts.AutoReclaimStaleClaims = true
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rigs/r1", opts, ops, &stdout, &stderr)
	if len(reclaimed) != 0 || len(claimed) != 0 {
		t.Fatalf("reclaim on %v, claim on %v; want neither for a bead bound to another worktree", reclaimed, claimed)
	}
}

// Work already assigned to this seat is an assignment, not a pull: the check
// is confined to the fresh-claim tier, so a ready assignment is still promoted.
func TestHookClaimWorktreeCheckLeavesAssignedWorkAlone(t *testing.T) {
	assigned := managedWorktreeCandidate("r1-a", "/wt/r1/r1-a")
	assigned.Assignee = "r1/dev-2"
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{assigned}, &claimed, &reclaimed)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("query", "/rigs/r1", affinitySeatOpts("/wt/r1/r1-b"), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.Join(claimed, ","); got != "r1-a" {
		t.Fatalf("claim CAS attempted on %q, want r1-a promoted as a ready assignment", got)
	}
}

// hookClaimManagedWorktreeDir must agree with worktreeSpecForBead on which
// shapes are a managed worktree, since the controller places seats from the
// latter: a copied predicate that drifts is the demand/claim disagreement
// class all over again.
func TestHookClaimManagedWorktreeDirAgreesWithWorktreeSpecForBead(t *testing.T) {
	complete := managedWorktreeCandidate("r1-a", "/wt/r1/r1-a")
	without := func(keys ...string) beads.Bead {
		b := managedWorktreeCandidate("r1-a", "/wt/r1/r1-a")
		for _, k := range keys {
			delete(b.Metadata, k)
		}
		return b
	}
	with := func(b beads.Bead, kv ...string) beads.Bead {
		for i := 0; i+1 < len(kv); i += 2 {
			b.Metadata[kv[i]] = kv[i+1]
		}
		return b
	}
	legacyOnly := without(beadmeta.WorkDirMetadataKey)
	legacyOnly.Metadata[beadmeta.LegacyWorkDirMetadataKey] = "/wt/r1/r1-a"
	for _, tc := range []struct {
		name string
		bead beads.Bead
	}{
		{"complete evidence", complete},
		{"complete evidence under the legacy path key", legacyOnly},
		{"agreeing canonical and legacy paths", with(managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"), beadmeta.LegacyWorkDirMetadataKey, "/wt/r1/r1-a")},
		{"disagreeing canonical and legacy paths", with(managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"), beadmeta.LegacyWorkDirMetadataKey, "/elsewhere")},
		{"no work dir", without(beadmeta.WorkDirMetadataKey)},
		{"missing gc.work_branch", without(beadmeta.WorkBranchMetadataKey)},
		{"partial ownership keys", without(beadmeta.WorktreeOwnerMetadataKey, beadmeta.WorktreeGenerationMetadataKey)},
		{"work dir with no ownership keys", beads.Bead{ID: "r1-a", Metadata: map[string]string{beadmeta.WorkDirMetadataKey: "/wt/r1/r1-a"}}},
		{"work dir with only a work branch", beads.Bead{ID: "r1-a", Metadata: map[string]string{beadmeta.WorkDirMetadataKey: "/wt/r1/r1-a", beadmeta.WorkBranchMetadataKey: "b"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := worktreeSpecForBead(tc.bead, "rig:r1")
			wantManaged := err == nil && spec != nil
			dir, managed := hookClaimManagedWorktreeDir(tc.bead)
			if managed != wantManaged {
				t.Fatalf("hookClaimManagedWorktreeDir managed=%v, worktreeSpecForBead spec=%v err=%v", managed, spec, err)
			}
			if managed && dir != spec.Path {
				t.Fatalf("dir = %q, want spec path %q", dir, spec.Path)
			}
		})
	}
}

// The seat's directory and the bead's path are compared as directories, so a
// trailing separator or a symlinked spelling of the seat's own worktree does
// not turn it into "elsewhere".
func TestHookClaimWorktreeHeldElsewhereComparesResolvedPaths(t *testing.T) {
	root := t.TempDir()
	own := filepath.Join(root, "wt", "r1-b")
	sibling := filepath.Join(root, "wt", "r1-a")
	for _, d := range []string{own, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "wt"), alias); err != nil {
		t.Fatal(err)
	}
	bead := managedWorktreeCandidate("r1-b", own)
	for _, tc := range []struct {
		seat string
		want bool
	}{
		{own, false},
		{own + string(filepath.Separator), false},
		{filepath.Join(alias, "r1-b"), false},
		{sibling, true},
		{"", false},
	} {
		if got := hookClaimWorktreeHeldElsewhere(bead, tc.seat); got != tc.want {
			t.Errorf("hookClaimWorktreeHeldElsewhere(seat=%q) = %v, want %v", tc.seat, got, tc.want)
		}
	}
	if hookClaimWorktreeHeldElsewhere(unboundRoutedCandidate("r1-u"), sibling) {
		t.Error("a bead naming no managed worktree is never held elsewhere")
	}
}

// Only a pool seat's own row turns the check on.
func TestHookClaimPoolSeatWorkDirReadsOnlyAPoolSeatsRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *session.Info
		want string
	}{
		{"no fenced row", nil, ""},
		{"pool seat placed in a verified worktree", &session.Info{PoolManaged: true, WorkDirCanonical: "/wt/r1/r1-b", WorkDir: "/wt/r1/r1-b"}, "/wt/r1/r1-b"},
		{"pool seat with only the legacy key", &session.Info{PoolManaged: true, WorkDir: "/slots/dev-3"}, "/slots/dev-3"},
		{"pool slot row", &session.Info{PoolSlot: "3", WorkDirCanonical: "/slots/dev-3"}, "/slots/dev-3"},
		{"named session", &session.Info{PoolManaged: true, ConfiguredNamedSession: true, WorkDirCanonical: "/agents/dev"}, ""},
		{"manual session", &session.Info{PoolSlot: "1", SessionOrigin: "manual", WorkDirCanonical: "/home/me"}, ""},
		{"not a pool seat", &session.Info{WorkDirCanonical: "/agents/dev"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hookClaimPoolSeatWorkDir(tc.info); got != tc.want {
				t.Fatalf("hookClaimPoolSeatWorkDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

// `gc hook --claim` reads the pool seat's directory off the session row its
// claim fence verified. A seat whose row places it in /wt/r1/r1-b leaves the
// only ready work, bound to /wt/r1/r1-a, to the seat started there and drains
// no_work without attempting the claim. The cases above drive doHookClaim with
// PoolSeatWorkDir already set; this one proves cmdHookWithOptions sets it.
func TestHookClaimCommandLeavesWorkBoundToAnotherWorktreeToItsSeat(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	bound := managedWorktreeCandidate("r1-a", "/wt/r1/r1-a")
	bound.Metadata["gc.routed_to"] = "dev"
	candidates, err := json.Marshal([]beads.Bead{bound})
	if err != nil {
		t.Fatal(err)
	}
	candidatesPath := filepath.Join(cityDir, "candidates.json")
	if err := os.WriteFile(candidatesPath, candidates, 0o644); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "dev"
scope = "city"
max_active_sessions = 3
work_query = "cat '` + candidatesPath + `'"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	seat, err := store.Create(beads.Bead{
		Title:  "dev",
		Type:   session.BeadType,
		Labels: []string{"gc:session", "agent:dev"},
		Metadata: map[string]string{
			"session_name":                    "dev-2",
			"template":                        "dev",
			"pool_slot":                       "2",
			"state":                           string(session.StateActive),
			"instance_token":                  "live-token",
			beadmeta.WorkDirMetadataKey:       "/wt/r1/r1-b",
			beadmeta.LegacyWorkDirMetadataKey: "/wt/r1/r1-b",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TEMPLATE", "dev")
	t.Setenv("GC_ALIAS", "dev-2")
	t.Setenv("GC_AGENT", "dev-2")
	t.Setenv("GC_SESSION_ID", seat.ID)
	t.Setenv("GC_SESSION_NAME", "dev-2")
	t.Setenv("GC_SESSION_ORIGIN", "ephemeral")
	t.Setenv("GC_INSTANCE_TOKEN", "live-token")

	var stdout, stderr bytes.Buffer
	_ = cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON claim result: %v\n%s; stderr=%s", err, stdout.String(), stderr.String())
	}
	if result.Action != "drain" || result.Reason != "no_work" {
		t.Fatalf("result = %+v, want a no_work drain: the seat in /wt/r1/r1-b must leave r1-a to the seat in its own worktree; stderr=%s", result, stderr.String())
	}
}

// countingSeatProbe stubs the managed-worktree probe with a fixed answer and
// counts how often a claim asks it.
func countingSeatProbe(answer bool, calls *int) func(string) bool {
	return func(string) bool {
		*calls++
		return answer
	}
}

// The controller started this seat inside r1-a's verified worktree for r1-a,
// and work bound to no worktree (r1-u) sits ahead of r1-a in the routed window.
// The seat must leave r1-u: it would run in r1-a's checkout, on r1-a's branch,
// while r1-a's demand starts a second seat in the same worktree.
func TestHookClaimSeatInVerifiedWorktreeLeavesUnboundWork(t *testing.T) {
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{
		unboundRoutedCandidate("r1-u"),
		managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"),
	}, &claimed, &reclaimed)
	calls := 0
	ops.SeatInManagedWorktree = countingSeatProbe(true, &calls)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("query", "/rigs/r1", affinitySeatOpts("/wt/r1/r1-a"), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.Join(claimed, ","); got != "r1-a" {
		t.Fatalf("claim CAS attempted on %q, want only r1-a (the seat's own verified worktree)", got)
	}
	if calls != 1 {
		t.Fatalf("managed-worktree probe ran %d times, want once", calls)
	}
}

// The same seat with only unbound work ready drains no_work rather than run
// that work inside another bead's worktree, and asks the probe once per claim.
func TestHookClaimSeatInVerifiedWorktreeDrainsRatherThanTakeUnboundWork(t *testing.T) {
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{
		unboundRoutedCandidate("r1-v"),
		unboundRoutedCandidate("r1-w"),
	}, &claimed, &reclaimed)
	calls := 0
	ops.SeatInManagedWorktree = countingSeatProbe(true, &calls)
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rigs/r1", affinitySeatOpts("/wt/r1/r1-a"), ops, &stdout, &stderr)
	if len(claimed) != 0 {
		t.Fatalf("claim CAS attempted on %v, want none", claimed)
	}
	if !strings.Contains(stdout.String(), `"action":"drain"`) || !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("result = %s, want a no_work drain", stdout.String())
	}
	if calls != 1 {
		t.Fatalf("managed-worktree probe ran %d times, want once per claim", calls)
	}
}

// A seat outside every managed worktree (the pool's own directory, where the
// controller places seats for unbound work) still takes unbound work.
func TestHookClaimSeatOutsideManagedWorktreeTakesUnboundWork(t *testing.T) {
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{unboundRoutedCandidate("r1-x")}, &claimed, &reclaimed)
	calls := 0
	ops.SeatInManagedWorktree = countingSeatProbe(false, &calls)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("query", "/rigs/r1", affinitySeatOpts("/rigs/r1/.gc/slots/dev-3"), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.Join(claimed, ","); got != "r1-x" {
		t.Fatalf("claim CAS attempted on %q, want r1-x", got)
	}
	if calls != 1 {
		t.Fatalf("managed-worktree probe ran %d times, want once", calls)
	}
}

// The probe spawns git, so a claim asks it only when it reaches a candidate
// bound to no worktree, and never without a pool-seat directory.
func TestHookClaimSeatProbeIsNotAskedWithoutUnboundCandidateOrSeatDir(t *testing.T) {
	for _, tc := range []struct {
		name       string
		seat       string
		candidates []beads.Bead
	}{
		{"every candidate bound", "/wt/r1/r1-b", []beads.Bead{
			managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"),
			managedWorktreeCandidate("r1-b", "/wt/r1/r1-b"),
		}},
		{"no seat dir", "", []beads.Bead{unboundRoutedCandidate("r1-y")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var claimed, reclaimed []string
			ops := affinityClaimOps(t, tc.candidates, &claimed, &reclaimed)
			calls := 0
			ops.SeatInManagedWorktree = countingSeatProbe(true, &calls)
			var stdout, stderr bytes.Buffer
			doHookClaim("query", "/rigs/r1", affinitySeatOpts(tc.seat), ops, &stdout, &stderr)
			if calls != 0 {
				t.Fatalf("managed-worktree probe ran %d times, want 0", calls)
			}
			if len(claimed) != 1 {
				t.Fatalf("claim CAS attempted on %v, want exactly one claim", claimed)
			}
		})
	}
}

// The opt-in stale reclaim must not free unbound work a seat inside a managed
// worktree would then skip.
func TestHookClaimSeatInVerifiedWorktreeSkipsStaleUnboundBeforeReclaim(t *testing.T) {
	stale := unboundRoutedCandidate("r1-s")
	stale.Assignee = "dead-seat"
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{stale}, &claimed, &reclaimed)
	ops.SeatInManagedWorktree = func(string) bool { return true }
	opts := affinitySeatOpts("/wt/r1/r1-a")
	opts.AutoReclaimStaleClaims = true
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rigs/r1", opts, ops, &stdout, &stderr)
	if len(reclaimed) != 0 || len(claimed) != 0 {
		t.Fatalf("reclaim on %v, claim on %v; want neither for unbound work from a seat inside a managed worktree", reclaimed, claimed)
	}
}

// ensureAffinityWorktree creates a managed worktree with worktree.Ensure, the
// way the controller does before placing a pool seat in it, and returns the
// main checkout, the base ref, and the worktree path.
func ensureAffinityWorktree(t *testing.T) (string, string, string) {
	t.Helper()
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	managed := filepath.Join(root, "gc-a")
	if _, err := worktree.Ensure(worktree.Spec{
		RepoDir: repo, Root: root, Path: managed, Branch: "work/gc-a", Base: base,
		BeadID: "gc-a", StoreRef: "rig:r1", Creator: "provisioner",
		Owner: "provisioner", Generation: "1", Lifecycle: worktree.LifecycleActive,
	}); err != nil {
		t.Fatalf("worktree.Ensure: %v", err)
	}
	return repo, base, managed
}

// The default probe answers true only for a directory whose git dir holds the
// provenance a completed Ensure published; anything else leaves unbound work
// claimable.
func TestHookClaimSeatInManagedWorktreeReadsEnsureProvenance(t *testing.T) {
	repo, base, managed := ensureAffinityWorktree(t)
	plain := filepath.Join(t.TempDir(), "plain-linked")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "plain", plain, base)
	for _, tc := range []struct {
		name, dir string
		want      bool
	}{
		{"worktree Ensure created", managed, true},
		{"main checkout", repo, false},
		{"linked worktree with no provenance", plain, false},
		{"not a repository", t.TempDir(), false},
		{"missing directory", filepath.Join(t.TempDir(), "absent"), false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hookClaimSeatInManagedWorktree(tc.dir); got != tc.want {
				t.Fatalf("hookClaimSeatInManagedWorktree(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}

// `gc hook --claim` from a pool seat whose row places it in a real managed
// worktree leaves the only ready work, bound to no worktree, to a seat in the
// pool's own directory and drains no_work. This drives the default probe and
// the PoolSeatWorkDir wiring end to end.
func TestHookClaimCommandSeatInManagedWorktreeLeavesUnboundWork(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	_, _, managed := ensureAffinityWorktree(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	unbound := unboundRoutedCandidate("r1-z")
	unbound.Metadata["gc.routed_to"] = "dev"
	candidates, err := json.Marshal([]beads.Bead{unbound})
	if err != nil {
		t.Fatal(err)
	}
	candidatesPath := filepath.Join(cityDir, "candidates.json")
	if err := os.WriteFile(candidatesPath, candidates, 0o644); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "dev"
scope = "city"
max_active_sessions = 3
work_query = "cat '` + candidatesPath + `'"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	seat, err := store.Create(beads.Bead{
		Title:  "dev",
		Type:   session.BeadType,
		Labels: []string{"gc:session", "agent:dev"},
		Metadata: map[string]string{
			"session_name":                    "dev-2",
			"template":                        "dev",
			"pool_slot":                       "2",
			"state":                           string(session.StateActive),
			"instance_token":                  "live-token",
			beadmeta.WorkDirMetadataKey:       managed,
			beadmeta.LegacyWorkDirMetadataKey: managed,
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TEMPLATE", "dev")
	t.Setenv("GC_ALIAS", "dev-2")
	t.Setenv("GC_AGENT", "dev-2")
	t.Setenv("GC_SESSION_ID", seat.ID)
	t.Setenv("GC_SESSION_NAME", "dev-2")
	t.Setenv("GC_SESSION_ORIGIN", "ephemeral")
	t.Setenv("GC_INSTANCE_TOKEN", "live-token")

	var stdout, stderr bytes.Buffer
	_ = cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON claim result: %v\n%s; stderr=%s", err, stdout.String(), stderr.String())
	}
	if result.Action != "drain" || result.Reason != "no_work" {
		t.Fatalf("result = %+v, want a no_work drain: the seat in a managed worktree must leave unbound work to a seat in the pool's own directory; stderr=%s", result, stderr.String())
	}
}
