package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
		SeatInManagedWorktree: func(string) (bool, error) { return false, nil },
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
		PoolSeat:           hookPoolSeat{WorkDir: seatWorkDir},
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

// Only a pool seat's own row turns the check on. Manual is read for the
// session's agent, as the controller reads it.
func TestHookClaimPoolSeatWorkDirReadsOnlyAPoolSeatsRow(t *testing.T) {
	three, one := 3, 1
	pool := &config.Agent{Name: "dev", MaxActiveSessions: &three}
	single := &config.Agent{Name: "dev", MaxActiveSessions: &one}
	for _, tc := range []struct {
		name  string
		info  *session.Info
		agent *config.Agent
		want  string
	}{
		{"no fenced row", nil, pool, ""},
		{"pool seat placed in a verified worktree", &session.Info{PoolManaged: true, WorkDirCanonical: "/wt/r1/r1-b", WorkDir: "/wt/r1/r1-b"}, pool, "/wt/r1/r1-b"},
		{"pool seat with only the legacy key", &session.Info{PoolManaged: true, WorkDir: "/slots/dev-3"}, pool, "/slots/dev-3"},
		{"pool slot row", &session.Info{PoolSlot: "3", WorkDirCanonical: "/slots/dev-3"}, pool, "/slots/dev-3"},
		{"named session", &session.Info{PoolManaged: true, ConfiguredNamedSession: true, WorkDirCanonical: "/agents/dev"}, pool, ""},
		{"manual session", &session.Info{PoolSlot: "1", SessionOrigin: "manual", WorkDirCanonical: "/home/me"}, pool, ""},
		// A user-created row persisted before the manual-origin backfill:
		// ephemeral origin and no pool_managed, pool_slot or dependency_only.
		{"legacy manual row of a multi-session agent", &session.Info{SessionOrigin: "ephemeral", WorkDirCanonical: "/home/me"}, pool, ""},
		{"ephemeral row of a single-session agent", &session.Info{SessionOrigin: "ephemeral", WorkDirCanonical: "/agents/dev"}, single, "/agents/dev"},
		{"not a pool seat", &session.Info{WorkDirCanonical: "/agents/dev"}, pool, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hookClaimPoolSeatWorkDir(tc.info, tc.agent); got != tc.want {
				t.Fatalf("hookClaimPoolSeatWorkDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

// affinityHookCity writes a file-store city whose pool agent "dev" serves
// candidates through its work_query, creates one session row carrying
// seatMeta, and points the process env at that row the way the controller
// starts a seat.
func affinityHookCity(t *testing.T, candidates []beads.Bead, seatMeta map[string]string) {
	t.Helper()
	affinityHookCityWithWorkDir(t, candidates, seatMeta, "")
}

// affinityHookCityWithWorkDir is affinityHookCity with the pool agent's
// work_dir set to workDir, when it is not empty.
func affinityHookCityWithWorkDir(t *testing.T, candidates []beads.Bead, seatMeta map[string]string, workDir string) {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		c.Metadata["gc.routed_to"] = "dev"
	}
	raw, err := json.Marshal(candidates)
	if err != nil {
		t.Fatal(err)
	}
	candidatesPath := filepath.Join(cityDir, "candidates.json")
	if err := os.WriteFile(candidatesPath, raw, 0o644); err != nil {
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
	if workDir != "" {
		cityToml += "work_dir = '" + workDir + "'\n"
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	meta := map[string]string{
		"session_name":   "dev-2",
		"template":       "dev",
		"state":          string(session.StateActive),
		"instance_token": "live-token",
	}
	for k, v := range seatMeta {
		meta[k] = v
	}
	seat, err := store.Create(beads.Bead{
		Title:    "dev",
		Type:     session.BeadType,
		Labels:   []string{"gc:session", "agent:dev"},
		Metadata: meta,
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
}

// affinityPoolSeatIn is the row of a pool seat the controller started in dir.
func affinityPoolSeatIn(dir string) map[string]string {
	return map[string]string{
		"pool_slot":                       "2",
		beadmeta.WorkDirMetadataKey:       dir,
		beadmeta.LegacyWorkDirMetadataKey: dir,
	}
}

func runHookClaimCommand(t *testing.T) (hookClaimJSONResult, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	_ = cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON claim result: %v\n%s; stderr=%s", err, stdout.String(), stderr.String())
	}
	return result, stderr.String()
}

// `gc hook --claim` reads the pool seat's directory off the session row its
// claim fence verified. A seat whose row places it in /wt/r1/r1-b leaves the
// only ready work, bound to /wt/r1/r1-a, to the seat started there and drains
// no_work without attempting the claim. The cases above drive doHookClaim with
// PoolSeatWorkDir already set; this one proves cmdHookWithOptions sets it.
func TestHookClaimCommandLeavesWorkBoundToAnotherWorktreeToItsSeat(t *testing.T) {
	affinityHookCity(t, []beads.Bead{managedWorktreeCandidate("r1-a", "/wt/r1/r1-a")}, affinityPoolSeatIn("/wt/r1/r1-b"))
	result, stderr := runHookClaimCommand(t)
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want a no_work drain: the seat in /wt/r1/r1-b must leave r1-a to the seat in its own worktree; stderr=%s", result, stderr)
	}
}

// countingSeatProbe stubs the managed-worktree probe with a fixed answer and
// counts how often a claim asks it.
func countingSeatProbe(answer bool, calls *int) func(string) (bool, error) {
	return func(string) (bool, error) {
		*calls++
		return answer, nil
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
	ops.SeatInManagedWorktree = func(string) (bool, error) { return true, nil }
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
			if got, err := hookClaimSeatInManagedWorktree(tc.dir); err != nil || got != tc.want {
				t.Fatalf("hookClaimSeatInManagedWorktree(%q) = %v, %v; want %v, nil", tc.dir, got, err, tc.want)
			}
		})
	}
}

// `gc hook --claim` from a pool seat whose row places it in a real managed
// worktree leaves the only ready work, bound to no worktree, to a seat in the
// pool's own directory and drains no_work. This drives the default probe and
// the PoolSeatWorkDir wiring end to end.
func TestHookClaimCommandSeatInManagedWorktreeLeavesUnboundWork(t *testing.T) {
	_, _, managed := ensureAffinityWorktree(t)
	affinityHookCity(t, []beads.Bead{unboundRoutedCandidate("r1-z")}, affinityPoolSeatIn(managed))
	result, stderr := runHookClaimCommand(t)
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want a no_work drain: the seat in a managed worktree must leave unbound work to a seat in the pool's own directory; stderr=%s", result, stderr)
	}
}

// A user-created row persisted before the manual-origin backfill is manual for
// a multi-session agent, so its claim attempts the bound bead, as every manual
// claimant does, rather than leaving it.
func TestHookClaimCommandLegacyManualRowClaimsAsBefore(t *testing.T) {
	affinityHookCity(t, []beads.Bead{managedWorktreeCandidate("r1-a", "/wt/r1/r1-a")}, map[string]string{
		"session_origin":                  "ephemeral",
		beadmeta.WorkDirMetadataKey:       "/home/me",
		beadmeta.LegacyWorkDirMetadataKey: "/home/me",
	})
	result, stderr := runHookClaimCommand(t)
	if result.Reason == hookClaimReasonNoWork || strings.Contains(stderr, "left r1-a") {
		t.Fatalf("result = %+v; want the claim of r1-a attempted, not left to another worktree; stderr=%s", result, stderr)
	}
}

// When the seat checks leave the seat nothing to claim, each bead they left is
// named with where its session runs, so a bound step no seat is started for
// does not starve silently. Only claimable beads are named: one held by another
// seat was never this seat's to take.
func TestHookClaimNamesWorkLeftToAnotherWorktreeWhenItClaimsNothing(t *testing.T) {
	held := managedWorktreeCandidate("r1-c", "/wt/r1/r1-c")
	held.Assignee = "r1/dev-3"
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"), held}, &claimed, &reclaimed)
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rigs/r1", affinitySeatOpts("/wt/r1/r1-b"), ops, &stdout, &stderr)
	if !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("result = %s, want a no_work drain", stdout.String())
	}
	want := "gc hook --claim: left r1-a to a session in its managed worktree /wt/r1/r1-a; this session's work dir is /wt/r1/r1-b\n"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
	if strings.Contains(stderr.String(), "r1-c") {
		t.Fatalf("stderr = %q names r1-c, which another seat holds", stderr.String())
	}
}

// A seat inside a managed worktree that leaves unbound work and claims nothing
// names that work and the pool's own directory, where its session runs.
func TestHookClaimNamesUnboundWorkLeftToThePoolsDirectoryWhenItClaimsNothing(t *testing.T) {
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{unboundRoutedCandidate("r1-u")}, &claimed, &reclaimed)
	ops.SeatInManagedWorktree = func(string) (bool, error) { return true, nil }
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rigs/r1", affinitySeatOpts("/wt/r1/r1-a"), ops, &stdout, &stderr)
	if len(claimed) != 0 || !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("claimed %v, result = %s; want a no_work drain", claimed, stdout.String())
	}
	want := "gc hook --claim: left r1-u, which names no managed worktree, to a session in the pool's own directory; this session's work dir /wt/r1/r1-a is a managed worktree\n"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

// A seat that skips its siblings' beads on the way to its own says nothing.
func TestHookClaimSaysNothingOfLeftWorkWhenItClaims(t *testing.T) {
	var claimed, reclaimed []string
	ops := affinityClaimOps(t, []beads.Bead{
		managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"),
		managedWorktreeCandidate("r1-b", "/wt/r1/r1-b"),
	}, &claimed, &reclaimed)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("query", "/rigs/r1", affinitySeatOpts("/wt/r1/r1-b"), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "left r1-a") {
		t.Fatalf("stderr = %q; a seat that claims its own bead must not report the ones it left", stderr.String())
	}
}

// filteredHookIDs runs candidates through plain `gc hook`'s visibility filter
// and returns the ids it keeps, in order.
func filteredHookIDs(t *testing.T, candidates []beads.Bead, visibility hookVisibility) string {
	t.Helper()
	raw, err := json.Marshal(candidates)
	if err != nil {
		t.Fatal(err)
	}
	var kept []beads.Bead
	if err := json.Unmarshal([]byte(filterForeignHookCandidates(string(raw), visibility)), &kept); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(kept))
	for _, b := range kept {
		ids = append(ids, b.ID)
	}
	return strings.Join(ids, ",")
}

// affinitySeatVisibility is a pool seat's plain `gc hook` visibility: where
// the seat was placed and whether its directory is a managed worktree.
func affinitySeatVisibility(seat hookPoolSeat, inManaged bool) hookVisibility {
	return hookVisibility{
		Identities:            []string{"r1/dev-2", "s-2"},
		RouteTargets:          []string{affinityRoute},
		PoolSeat:              func() hookPoolSeat { return seat },
		SeatInManagedWorktree: func() bool { return inManaged },
	}
}

// Plain `gc hook` from a pool seat lists what `gc hook --claim` would take:
// work the seat checks leave to another session is left out, so a prompt that
// lists work and then claims it by id cannot run it in this seat's checkout.
// The seat's own assignment stays: the claim's assigned tiers serve it.
func TestFilterForeignHookCandidatesLeavesWorkTheClaimLeaves(t *testing.T) {
	assigned := managedWorktreeCandidate("r1-c", "/wt/r1/r1-c")
	assigned.Assignee = "r1/dev-2"
	assignedUnbound := unboundRoutedCandidate("r1-d")
	assignedUnbound.Assignee = "r1/dev-2"
	candidates := []beads.Bead{
		managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"),
		managedWorktreeCandidate("r1-b", "/wt/r1/r1-b"),
		unboundRoutedCandidate("r1-u"),
		assigned,
		assignedUnbound,
	}
	for _, tc := range []struct {
		name      string
		seat      hookPoolSeat
		inManaged bool
		want      string
	}{
		{"seat in a managed worktree", hookPoolSeat{WorkDir: "/wt/r1/r1-b"}, true, "r1-b,r1-c,r1-d"},
		{"seat in the pool's own directory", hookPoolSeat{WorkDir: "/rigs/r1/.gc/slots/dev-2"}, false, "r1-u,r1-c,r1-d"},
		{"seat in the pool's own directory inside a managed worktree", hookPoolSeat{WorkDir: "/wt/pool/dev-2", PoolDir: "/wt/pool/dev-2"}, true, "r1-u,r1-c,r1-d"},
		{"not a pool seat", hookPoolSeat{}, true, "r1-a,r1-b,r1-u,r1-c,r1-d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := filteredHookIDs(t, candidates, affinitySeatVisibility(tc.seat, tc.inManaged)); got != tc.want {
				t.Fatalf("kept %q, want %q", got, tc.want)
			}
		})
	}
	if got := filteredHookIDs(t, candidates, hookVisibility{Identities: []string{"r1/dev-2"}, RouteTargets: []string{affinityRoute}}); got != "r1-a,r1-b,r1-u,r1-c,r1-d" {
		t.Fatalf("kept %q without a seat resolver, want every visible candidate", got)
	}
}

// Plain `gc hook` is a status-line hot path: the seat's row is read only for an
// unassigned candidate, and the git probe only for one bound to no worktree.
func TestFilterForeignHookCandidatesResolvesTheSeatOnlyWhenACandidateNeedsIt(t *testing.T) {
	ownAssignment := unboundRoutedCandidate("r1-e")
	ownAssignment.Assignee = "r1/dev-2"
	for _, tc := range []struct {
		name                  string
		candidates            []beads.Bead
		wantReads, wantProbes int
	}{
		{"only assigned work", []beads.Bead{ownAssignment}, 0, 0},
		{"only bound work", []beads.Bead{managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"), managedWorktreeCandidate("r1-f", "/wt/r1/r1-f")}, 1, 0},
		{"unbound work", []beads.Bead{unboundRoutedCandidate("r1-v"), unboundRoutedCandidate("r1-w")}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, probes := 0, 0
			visibility := hookVisibility{
				Identities:            []string{"r1/dev-2"},
				RouteTargets:          []string{affinityRoute},
				PoolSeat:              sync.OnceValue(func() hookPoolSeat { reads++; return hookPoolSeat{WorkDir: "/wt/r1/r1-b"} }),
				SeatInManagedWorktree: sync.OnceValue(func() bool { probes++; return false }),
			}
			filteredHookIDs(t, tc.candidates, visibility)
			if reads != tc.wantReads || probes != tc.wantProbes {
				t.Fatalf("seat row read %d times and probed %d times, want %d and %d", reads, probes, tc.wantReads, tc.wantProbes)
			}
		})
	}
}

// The discovery door reads the row the claim fence verifies, and only for a
// session the controller could have started as a pool seat.
func TestHookDiscoveryPoolSeatReadsTheFencedRow(t *testing.T) {
	three := 3
	pool := &config.Agent{Name: "dev", MaxActiveSessions: &three}
	seatRow := beads.Bead{
		ID:     "s-2",
		Type:   session.BeadType,
		Status: "open",
		Title:  "dev",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":                     string(session.StateActive),
			"instance_token":            "live-token",
			"pool_slot":                 "2",
			beadmeta.WorkDirMetadataKey: "/wt/r1/r1-b",
		},
	}
	for _, tc := range []struct {
		name, origin, token string
		frontErr            error
		wantDir, wantStderr string
		wantReads           int
	}{
		{name: "pool seat", origin: "ephemeral", token: "live-token", wantDir: "/wt/r1/r1-b", wantReads: 1},
		{name: "named session", origin: "named", token: "live-token"},
		{name: "manual session", origin: "manual", token: "live-token"},
		{name: "tokenless runtime", origin: "ephemeral"},
		{name: "superseded token", origin: "ephemeral", token: "old-token", wantReads: 1},
		{name: "store unavailable", origin: "ephemeral", token: "live-token", frontErr: errors.New("store down"), wantReads: 1, wantStderr: "gc hook: session row unavailable for s-2: opening session store: store down; listing work the claim would leave to another session unfiltered\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			t.Setenv("GC_SESSION_ID", "s-2")
			t.Setenv("GC_SESSION_ORIGIN", tc.origin)
			t.Setenv("GC_INSTANCE_TOKEN", tc.token)
			reads := 0
			front := func() (*session.Store, error) {
				reads++
				if tc.frontErr != nil {
					return nil, tc.frontErr
				}
				return session.NewStore(beads.SessionStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{seatRow}, nil)}), nil
			}
			var stderr bytes.Buffer
			if got := hookDiscoveryPoolSeat(front, "/city", "fixture", nil, pool, &stderr).WorkDir; got != tc.wantDir {
				t.Fatalf("hookDiscoveryPoolSeat().WorkDir = %q, want %q; stderr=%s", got, tc.wantDir, stderr.String())
			}
			if reads != tc.wantReads {
				t.Fatalf("session front opened %d times, want %d", reads, tc.wantReads)
			}
			if stderr.String() != tc.wantStderr {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// The command proves the discovery door reads the seat's row: the seat in
// /wt/r1/r1-b lists r1-u and not r1-a, exactly what its claim would take, and
// with only r1-a ready it reports no work.
func TestHookDiscoveryLeavesWorkTheClaimLeavesToAnotherSession(t *testing.T) {
	runDiscovery := func(t *testing.T) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := cmdHookWithOptions(nil, hookCommandOptions{}, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	t.Run("bound work beside unbound work", func(t *testing.T) {
		affinityHookCity(t, []beads.Bead{managedWorktreeCandidate("r1-a", "/wt/r1/r1-a"), unboundRoutedCandidate("r1-u")}, affinityPoolSeatIn("/wt/r1/r1-b"))
		code, stdout, stderr := runDiscovery(t)
		if code != 0 || strings.Contains(stdout, `"r1-a"`) || !strings.Contains(stdout, `"r1-u"`) {
			t.Fatalf("discovery = exit %d stdout %s; want exit 0 listing r1-u and not r1-a; stderr=%s", code, stdout, stderr)
		}
	})
	t.Run("only bound work", func(t *testing.T) {
		affinityHookCity(t, []beads.Bead{managedWorktreeCandidate("r1-a", "/wt/r1/r1-a")}, affinityPoolSeatIn("/wt/r1/r1-b"))
		code, stdout, stderr := runDiscovery(t)
		if code != 1 || strings.Contains(stdout, `"r1-a"`) {
			t.Fatalf("discovery = exit %d stdout %s; want exit 1 without r1-a; stderr=%s", code, stdout, stderr)
		}
	})
	t.Run("unbound work from a seat in a managed worktree", func(t *testing.T) {
		_, _, managed := ensureAffinityWorktree(t)
		affinityHookCity(t, []beads.Bead{unboundRoutedCandidate("r1-z")}, affinityPoolSeatIn(managed))
		code, stdout, stderr := runDiscovery(t)
		if code != 1 || strings.Contains(stdout, `"r1-z"`) {
			t.Fatalf("discovery = exit %d stdout %s; want exit 1 without r1-z; stderr=%s", code, stdout, stderr)
		}
	})
	// No instance token: the claim door has no fenced row and claims r1-a, so
	// the discovery door lists it too.
	t.Run("tokenless runtime", func(t *testing.T) {
		affinityHookCity(t, []beads.Bead{managedWorktreeCandidate("r1-a", "/wt/r1/r1-a")}, affinityPoolSeatIn("/wt/r1/r1-b"))
		t.Setenv("GC_INSTANCE_TOKEN", "")
		code, stdout, stderr := runDiscovery(t)
		if code != 0 || !strings.Contains(stdout, `"r1-a"`) {
			t.Fatalf("discovery = exit %d stdout %s; want exit 0 listing r1-a; stderr=%s", code, stdout, stderr)
		}
	})
}

// affinityStoreRunner serves rows[dir] as the work-query answer of the store
// read in dir, the way a fan-out over several bead stores answers.
func affinityStoreRunner(t *testing.T, rows map[string][]beads.Bead) hookStoreRunner {
	t.Helper()
	answers := make(map[string]string, len(rows))
	for dir, beadRows := range rows {
		raw, err := json.Marshal(beadRows)
		if err != nil {
			t.Fatalf("marshal %s rows: %v", dir, err)
		}
		answers[dir] = string(raw)
	}
	return func(_, dir string, _ []string) (string, error) {
		answer, ok := answers[dir]
		if !ok {
			t.Fatalf("unexpected store dir %q", dir)
		}
		return answer, nil
	}
}

// listedHookIDs is the ids plain `gc hook` printed on stdout, in order.
func listedHookIDs(t *testing.T, stdout string) string {
	t.Helper()
	var listed []beads.Bead
	if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
		t.Fatalf("stdout is not a JSON listing: %v\n%s", err, stdout)
	}
	ids := make([]string, 0, len(listed))
	for _, b := range listed {
		ids = append(ids, b.ID)
	}
	return strings.Join(ids, ",")
}

// On a fan-out over several stores, plain `gc hook` lists what
// `gc hook --claim` claims. The claim drops a store it claimed nothing from and
// claims from a later one, so a store whose rows the seat checks all leave to
// another session must not end the listing: a seat that lists before it claims
// would drain with its work waiting in the next store. The left work is named
// once, and only when the whole invocation claims nothing.
func TestHookDoorsAgreeAcrossStoresWhenAStoreHoldsOnlyWorkLeftToAnotherSession(t *testing.T) {
	urgent := 0
	boundA := managedWorktreeCandidate("r1-a", "/wt/r1/r1-a")
	boundA.Priority = &urgent
	boundB := managedWorktreeCandidate("r1-b", "/wt/r1/r1-b")
	const seatDir = "/rigs/r1/.gc/slots/dev-3"
	for _, tc := range []struct {
		name               string
		rows               map[string][]beads.Bead
		wantClaim          string
		wantListing        string
		wantLeft, wantQuit []string
	}{
		{
			name:        "a later store holds this seat's work",
			rows:        map[string][]beads.Bead{"city": {boundA}, "riga": {unboundRoutedCandidate("r1-u")}},
			wantClaim:   "r1-u",
			wantListing: "r1-u",
			wantQuit:    []string{"left r1-a"},
		},
		{
			name: "no store holds this seat's work",
			// riga serves r1-a too, as two legs over one store would.
			rows:     map[string][]beads.Bead{"city": {boundA}, "riga": {boundA, boundB}},
			wantLeft: []string{"left r1-a to a session in its managed worktree /wt/r1/r1-a", "left r1-b to a session in its managed worktree /wt/r1/r1-b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stores := twoStoreFanOut()
			run := affinityStoreRunner(t, tc.rows)

			var claimed, reclaimed []string
			ops := affinityClaimOps(t, nil, &claimed, &reclaimed)
			var claimOut, claimErr bytes.Buffer
			claimCode := claimHookWorkWithRunner("query", "/rigs/r1", nil, stores, affinitySeatOpts(seatDir), ops, run, func(string, error) {}, &claimOut, &claimErr)
			if got := strings.Join(claimed, ","); got != tc.wantClaim {
				t.Fatalf("claim door claimed %q, want %q; exit %d stdout=%s stderr=%s", got, tc.wantClaim, claimCode, claimOut.String(), claimErr.String())
			}
			for _, line := range tc.wantLeft {
				if n := strings.Count(claimErr.String(), line); n != 1 {
					t.Fatalf("claim door stderr names %q %d times, want once:\n%s", line, n, claimErr.String())
				}
			}
			for _, line := range tc.wantQuit {
				if strings.Contains(claimErr.String(), line) {
					t.Fatalf("claim door stderr = %q; an invocation that claims must not report %q", claimErr.String(), line)
				}
			}

			visibility := affinitySeatVisibility(hookPoolSeat{WorkDir: seatDir}, false)
			var listOut, listErr bytes.Buffer
			listCode := doHook("query", "/rigs/r1", false, hookDiscoveryRunner(stores, run, visibility, func(string, error) {}, &listErr), &listOut, &listErr, visibility)
			wantCode := 1
			if tc.wantListing != "" {
				wantCode = 0
			}
			if listCode != wantCode {
				t.Fatalf("discovery door = exit %d, want %d; stdout=%s stderr=%s", listCode, wantCode, listOut.String(), listErr.String())
			}
			if got := listedHookIDs(t, listOut.String()); got != tc.wantListing {
				t.Fatalf("discovery door listed %q, want %q, what the claim door claims", got, tc.wantListing)
			}
		})
	}
}

// A pool whose configured work_dir is a managed worktree, or sits inside one,
// has its sessions for work bound to no managed worktree placed there: that is
// the directory the controller resolves for the work
// (verifiedPoolTriggerWorkDir). Such a session must take the work, though the
// real probe finds a managed worktree around it: leaving it would drain every
// session of the pool with the work still ready. Work the controller routes to
// another directory (another pack's) is still left, and named with where its
// session runs.
func TestHookClaimSeatInThePoolsOwnManagedDirectoryTakesUnboundWork(t *testing.T) {
	_, _, managed := ensureAffinityWorktree(t)
	cityPath := t.TempDir()
	three := 3
	packed := func(id, pack string) beads.Bead {
		b := unboundRoutedCandidate(id)
		b.Metadata[beadmeta.PackMetadataKey] = pack
		return b
	}
	for _, tc := range []struct {
		name      string
		workDir   string
		placedFor beads.Bead
		claimable beads.Bead
		wantClaim bool
		wantLeft  string
	}{
		{
			name: "configured directory is the managed worktree", workDir: managed,
			placedFor: unboundRoutedCandidate("r1-u"), claimable: unboundRoutedCandidate("r1-u"), wantClaim: true,
		},
		{
			name: "configured directory inside the managed worktree", workDir: filepath.Join(managed, "slots", "{{.AgentBase}}"),
			placedFor: unboundRoutedCandidate("r1-u"), claimable: unboundRoutedCandidate("r1-u"), wantClaim: true,
		},
		{
			name: "pack-routed work in the pack's directory", workDir: filepath.Join(managed, "slots", "{{.AgentBase}}"),
			placedFor: packed("r1-p", "review"), claimable: packed("r1-p", "review"), wantClaim: true,
		},
		{
			name: "another pack's work", workDir: filepath.Join(managed, "slots", "{{.AgentBase}}"),
			placedFor: unboundRoutedCandidate("r1-u"), claimable: packed("r1-q", "review"),
			wantLeft: "left r1-q, which names no managed worktree, to a session in the pool's own directory " + filepath.Join(managed, "slots", "review") + ";",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &config.Agent{Name: "dev", WorkDir: tc.workDir, MaxActiveSessions: &three}
			_, instance, slot := poolDesiredRequestIdentity(agent, 2)
			bp := &agentBuildParams{cityPath: cityPath, cityName: "fixture"}
			placed, err := verifiedPoolTriggerWorkDir(bp, agent, instance, SessionRequest{
				WorkBeadID: tc.placedFor.ID,
				WorkPack:   tc.placedFor.Metadata[beadmeta.PackMetadataKey],
			})
			if err != nil || placed == "" {
				t.Fatalf("controller placement = %q, %v", placed, err)
			}
			if err := os.MkdirAll(placed, 0o755); err != nil {
				t.Fatal(err)
			}
			if inManaged, err := hookClaimSeatInManagedWorktree(placed); err != nil || !inManaged {
				t.Fatalf("hookClaimSeatInManagedWorktree(%q) = %v, %v; the seat must run inside a managed worktree", placed, inManaged, err)
			}
			row := &session.Info{PoolSlot: strconv.Itoa(slot), AgentName: instance, WorkDirCanonical: placed, WorkDir: placed}
			opts := affinitySeatOpts("")
			opts.PoolSeat = hookClaimPoolSeatFor(cityPath, "fixture", nil, row, agent)

			var claimed, reclaimed []string
			ops := affinityClaimOps(t, []beads.Bead{tc.claimable}, &claimed, &reclaimed)
			ops.SeatInManagedWorktree = hookClaimSeatInManagedWorktree
			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", cityPath, opts, ops, &stdout, &stderr)
			if got := len(claimed) == 1 && claimed[0] == tc.claimable.ID; got != tc.wantClaim {
				t.Fatalf("seat placed in %q claimed %v, want claimed=%v; exit %d stdout=%s stderr=%s", placed, claimed, tc.wantClaim, code, stdout.String(), stderr.String())
			}
			if tc.wantLeft != "" && !strings.Contains(stderr.String(), tc.wantLeft) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tc.wantLeft)
			}
		})
	}
}

// End to end through both doors: a pool configured to run each session in its
// own directory inside a managed worktree takes, and lists, the work bound to
// no managed worktree the controller placed there.
func TestHookDoorsServeUnboundWorkToASeatInThePoolsOwnManagedDirectory(t *testing.T) {
	_, _, managed := ensureAffinityWorktree(t)
	seatDir := filepath.Join(managed, "slots", "dev-2")
	if err := os.MkdirAll(seatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(managed, "slots", "{{.AgentBase}}")
	t.Run("claim", func(t *testing.T) {
		affinityHookCityWithWorkDir(t, []beads.Bead{unboundRoutedCandidate("r1-u")}, affinityPoolSeatIn(seatDir), workDir)
		result, stderr := runHookClaimCommand(t)
		if result.Reason == hookClaimReasonNoWork || strings.Contains(stderr, "left r1-u") {
			t.Fatalf("result = %+v; want the claim of r1-u attempted, not left to the pool's own directory, where this seat already runs; stderr=%s", result, stderr)
		}
	})
	t.Run("discovery", func(t *testing.T) {
		affinityHookCityWithWorkDir(t, []beads.Bead{unboundRoutedCandidate("r1-u")}, affinityPoolSeatIn(seatDir), workDir)
		var stdout, stderr bytes.Buffer
		code := cmdHookWithOptions(nil, hookCommandOptions{}, &stdout, &stderr)
		if code != 0 || !strings.Contains(stdout.String(), `"r1-u"`) {
			t.Fatalf("discovery = exit %d stdout %s; want exit 0 listing r1-u; stderr=%s", code, stdout.String(), stderr.String())
		}
	})
}

// corruptAffinityProvenance overwrites the provenance record Ensure published
// for the managed worktree at dir with bytes that do not decode.
func corruptAffinityProvenance(t *testing.T, dir string) {
	t.Helper()
	gitDir := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "--absolute-git-dir"))
	records, err := filepath.Glob(filepath.Join(gitDir, "*provenance*.json"))
	if err != nil || len(records) != 1 {
		t.Fatalf("provenance records in %s = %v, %v; want exactly one", gitDir, records, err)
	}
	if err := os.WriteFile(records[0], []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.ReadProvenanceAt(gitDir); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadProvenanceAt after corrupting = %v, want a decode error", err)
	}
}

// A probe that could not tell whether the seat runs in a managed worktree is
// an error, not "not managed": a record that does not decode is one.
func TestHookClaimSeatInManagedWorktreeSaysWhenItCannotTell(t *testing.T) {
	_, _, managed := ensureAffinityWorktree(t)
	corruptAffinityProvenance(t, managed)
	inManaged, err := hookClaimSeatInManagedWorktree(managed)
	if err == nil || inManaged {
		t.Fatalf("hookClaimSeatInManagedWorktree(%q) = %v, %v; want false and an error", managed, inManaged, err)
	}
}

// Both doors name a probe that could not tell on stderr, and fail open: the
// seat takes, and lists, the work bound to no managed worktree, as it did
// before the check.
func TestHookDoorsNameASeatProbeThatCouldNotTell(t *testing.T) {
	t.Run("claim", func(t *testing.T) {
		var claimed, reclaimed []string
		ops := affinityClaimOps(t, []beads.Bead{unboundRoutedCandidate("r1-u")}, &claimed, &reclaimed)
		ops.SeatInManagedWorktree = func(string) (bool, error) { return false, errors.New("git gave no answer") }
		var stdout, stderr bytes.Buffer
		doHookClaim("query", "/rigs/r1", affinitySeatOpts("/wt/r1/r1-a"), ops, &stdout, &stderr)
		if got := strings.Join(claimed, ","); got != "r1-u" {
			t.Fatalf("claim CAS attempted on %q, want r1-u; stderr=%s", got, stderr.String())
		}
		want := "gc hook --claim: could not tell whether work dir /wt/r1/r1-a is a managed worktree: git gave no answer; treating it as not managed\n"
		if strings.Count(stderr.String(), want) != 1 {
			t.Fatalf("stderr = %q, want it to contain %q once", stderr.String(), want)
		}
	})
	t.Run("discovery", func(t *testing.T) {
		_, _, managed := ensureAffinityWorktree(t)
		corruptAffinityProvenance(t, managed)
		affinityHookCity(t, []beads.Bead{unboundRoutedCandidate("r1-u")}, affinityPoolSeatIn(managed))
		var stdout, stderr bytes.Buffer
		code := cmdHookWithOptions(nil, hookCommandOptions{}, &stdout, &stderr)
		if code != 0 || !strings.Contains(stdout.String(), `"r1-u"`) {
			t.Fatalf("discovery = exit %d stdout %s; want exit 0 listing r1-u; stderr=%s", code, stdout.String(), stderr.String())
		}
		want := "gc hook: could not tell whether work dir " + managed + " is a managed worktree: decoding "
		if !strings.Contains(stderr.String(), want) || !strings.Contains(stderr.String(), "; treating it as not managed\n") {
			t.Fatalf("stderr = %q, want it to name the probe that could not tell (%q)", stderr.String(), want)
		}
	})
}
