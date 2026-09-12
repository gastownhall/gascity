package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// hookClaimWorkBranchStoreDir is the directory a federated work query was
// answered from. For a rig-scoped worker this is the SHARED rig checkout, which
// sits on whatever branch someone last left it on and which the worker never
// commits to.
const hookClaimWorkBranchStoreDir = "/rigs/shared-checkout"

// hookClaimBranchByDir builds a ResolveWorkBranch seam that answers per
// directory, so a case can put the store and the worker checkout on different
// branches without running git. A directory absent from the map resolves to no
// branch, which is how a missing path or a detached HEAD presents.
// hookClaimTreeFor resolves dir the way the claim path does, with no store to
// exclude, so a case can hand the production resolver the same tree the production
// caller would hand it. The resolver reads HEAD out of the repository the tree
// carries, so a case that only had the path would be testing nothing.
func hookClaimTreeFor(t *testing.T, dir string) hookClaimWorkTree {
	t.Helper()
	tree, admitted := (hookClaimStoreHead{}).Admit(dir)
	if !admitted {
		t.Fatalf("Admit(%s) refused the directory with no store to exclude", dir)
	}
	if tree.RepoDir == "" {
		t.Fatalf("Admit(%s) identified no repository; the fixture is not a checkout", dir)
	}
	return tree
}

func hookClaimBranchByDir(branches map[string]string) hookResolveWorkBranchFunc {
	return func(tree hookClaimWorkTree) string { return branches[tree.Dir] }
}

// TestHookClaimIdentityPatchResolvesWorkerBranchNotStoreBranch pins the rule
// that gc.work_branch names the tree the WORKER used, never the store directory
// the claim was answered from (gc-j4sr).
//
// The cases deliberately include two where the recorded checkout is a perfectly
// valid repo that merely happens to BE the store: a chain that only rejects
// MALFORMED candidates (missing path, detached HEAD) passes those while still
// stamping the shared branch, which is the live gc-ber59 shape.
func TestHookClaimIdentityPatchResolvesWorkerBranchNotStoreBranch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		branches map[string]string
		want     string // expected gc.work_branch; "" means the key must be absent
	}{
		{
			name:     "worker checkout on its own branch is stamped, not the store branch",
			metadata: map[string]string{beadmeta.WorkDirMetadataKey: "/worktrees/worker-1"},
			branches: map[string]string{
				hookClaimWorkBranchStoreDir: "someone-elses-branch",
				"/worktrees/worker-1":       "bd-gc-worker",
			},
			want: "bd-gc-worker",
		},
		{
			name:     "no recorded checkout leaves the key unset rather than inferring from the store",
			metadata: map[string]string{},
			branches: map[string]string{hookClaimWorkBranchStoreDir: "someone-elses-branch"},
			want:     "",
		},
		{
			name:     "canonical work dir that IS the store dir is refused",
			metadata: map[string]string{beadmeta.WorkDirMetadataKey: hookClaimWorkBranchStoreDir},
			branches: map[string]string{hookClaimWorkBranchStoreDir: "someone-elses-branch"},
			want:     "",
		},
		{
			name: "legacy work dir recovers the branch when the canonical key does not resolve",
			metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       "/worktrees/never-created",
				beadmeta.LegacyWorkDirMetadataKey: "/worktrees/worker-2",
			},
			branches: map[string]string{
				hookClaimWorkBranchStoreDir: "someone-elses-branch",
				"/worktrees/worker-2":       "bd-gc-legacy",
			},
			want: "bd-gc-legacy",
		},
		{
			name: "legacy work dir that IS the store dir is refused too",
			metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       "/worktrees/never-created",
				beadmeta.LegacyWorkDirMetadataKey: hookClaimWorkBranchStoreDir,
			},
			branches: map[string]string{hookClaimWorkBranchStoreDir: "someone-elses-branch"},
			want:     "",
		},
		{
			name: "a trailing separator does not smuggle the store dir past the refusal",
			metadata: map[string]string{
				beadmeta.WorkDirMetadataKey: hookClaimWorkBranchStoreDir + "/",
			},
			branches: map[string]string{
				hookClaimWorkBranchStoreDir:       "someone-elses-branch",
				hookClaimWorkBranchStoreDir + "/": "someone-elses-branch",
			},
			want: "",
		},
		{
			name: "a branch already current issues no write",
			metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:    "/worktrees/worker-1",
				beadmeta.WorkBranchMetadataKey: "bd-gc-worker",
			},
			branches: map[string]string{"/worktrees/worker-1": "bd-gc-worker"},
			want:     "",
		},
	} {
		// Every case runs twice. "session fallback unwired" is the isolated
		// predicate. "session fallback wired" is PRODUCTION: applyDefaults always
		// installs ResolveSessionWorkDir and a claiming session always exports
		// GC_SESSION_ID, so a case that leaves both unset cannot reach the fallback
		// at all and therefore cannot say anything about the shipped path.
		//
		// What the wired arm adds here is reachability, not discrimination. Only the
		// no-recorded-checkout case actually enters the fallback, and the
		// store-excluded cases do not fail under a wrong precondition through this
		// table, because their branch fixtures hold no entry for the session dir and
		// their legacy key is empty. The two shapes that do discriminate have their
		// own tests below: TestHookClaimIdentityPatchDoesNotManufactureWorkDirConflict
		// for a bead recording the store under both keys, and
		// TestHookClaimIdentityPatchLeavesACanonicalOnlyStoreAlone for the canonical
		// key alone. Keep both; this table does not cover them.
		for _, arm := range []struct {
			name string
			opts hookClaimOptions
			ops  func(hookClaimOps) hookClaimOps
		}{
			{
				name: "session fallback unwired",
				opts: hookClaimOptions{},
				ops:  func(o hookClaimOps) hookClaimOps { return o },
			},
			{
				name: "session fallback wired (production shape)",
				opts: hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1"}},
				ops: func(o hookClaimOps) hookClaimOps {
					o.ResolveSessionWorkDir = func(string) string { return hookClaimWorkBranchSessionDir }
					return o
				},
			},
		} {
			t.Run(tc.name+"/"+arm.name, func(t *testing.T) {
				bead := beads.Bead{ID: "gc-work-1", Metadata: beads.StringMap{}}
				for k, v := range tc.metadata {
					bead.Metadata[k] = v
				}
				ops := arm.ops(hookClaimOps{ResolveWorkBranch: hookClaimBranchByDir(tc.branches)})

				patch := hookClaimIdentityPatch(bead, arm.opts, ops, hookClaimWorkBranchStoreDir)

				// The branch verdict must not depend on whether the fallback is
				// available: the fallback exists for a bead that recorded NO
				// checkout, and every case here records one.
				got, ok := patch[beadmeta.WorkBranchMetadataKey]
				switch {
				case tc.want == "" && ok:
					t.Errorf("stamped gc.work_branch=%q; expected no stamp. A branch resolved from %q is the shared rig checkout, not this worker's tree, and stamping it makes the close gate validate unrelated commits.",
						got, hookClaimWorkBranchStoreDir)
				case tc.want != "" && !ok:
					t.Errorf("no gc.work_branch stamped; expected %q resolved from the bead's own recorded checkout", tc.want)
				case tc.want != "" && got != tc.want:
					t.Errorf("gc.work_branch = %q; want %q", got, tc.want)
				}

				// And it must never manufacture a canonical/legacy disagreement:
				// worktreeSpecForBead treats that as a hard error and the bead is
				// then starved of a session entirely.
				assertNoWorkDirConflict(t, bead, patch)
			})
		}
	}
}

// hookClaimWorkBranchSessionDir is the checkout the claiming session reports. It
// is neither the store nor any tree a case records, so a stamp of this value is
// unambiguously the session fallback having fired.
const hookClaimWorkBranchSessionDir = "/worktrees/claiming-session"

// assertNoWorkDirConflict fails when applying patch to bead would leave the
// canonical and legacy work-dir keys naming different trees. That combination is
// not merely untidy: worktreeSpecForBead (pool_desired_state.go) returns an error
// for it rather than picking a side, which surfaces as a pool trigger refusal and
// leaves the bead without a session. workDirStampWouldClobberEvidence refuses to
// create the same shape on the reconciler side.
func assertNoWorkDirConflict(t *testing.T, bead beads.Bead, patch map[string]string) {
	t.Helper()
	conflict := func(canonical, legacy string) bool {
		canonical, legacy = strings.TrimSpace(canonical), strings.TrimSpace(legacy)
		return canonical != "" && legacy != "" && canonical != legacy
	}
	effective := func(key string) string {
		if v, ok := patch[key]; ok {
			return v
		}
		return bead.Metadata[key]
	}
	// Only a conflict the PATCH introduces is a defect. Some fixtures hand in a
	// bead that already disagrees with itself (a canonical path that was never
	// created alongside a real legacy tree), and reporting that would be blaming
	// the patch for its input.
	before := conflict(bead.Metadata[beadmeta.WorkDirMetadataKey], bead.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	after := conflict(effective(beadmeta.WorkDirMetadataKey), effective(beadmeta.LegacyWorkDirMetadataKey))
	if !before && after {
		t.Errorf("patch manufactures a work-dir conflict: %s=%q vs %s=%q. worktreeSpecForBead hard-errors on this and the bead never gets a session.",
			beadmeta.WorkDirMetadataKey, effective(beadmeta.WorkDirMetadataKey),
			beadmeta.LegacyWorkDirMetadataKey, effective(beadmeta.LegacyWorkDirMetadataKey))
	}
}

// TestHookClaimIdentityPatchDoesNotManufactureWorkDirConflict is the regression
// for the case the candidate-list-empty precondition got wrong. A bead that
// recorded the store under BOTH keys has named a checkout; it merely named an
// unusable one. Gating the session fallback on the post-exclusion candidate list
// being empty treats that bead as if it had recorded nothing, stamps the session
// dir over the canonical key, and leaves the legacy key on the store path. The
// result is the conflict worktreeSpecForBead fails closed on, so the bead is
// starved rather than helped.
func TestHookClaimIdentityPatchDoesNotManufactureWorkDirConflict(t *testing.T) {
	bead := beads.Bead{ID: "gc-work-conflict", Metadata: beads.StringMap{
		beadmeta.WorkDirMetadataKey:       hookClaimWorkBranchStoreDir,
		beadmeta.LegacyWorkDirMetadataKey: hookClaimWorkBranchStoreDir,
	}}
	ops := hookClaimOps{
		ResolveWorkBranch:     hookClaimBranchByDir(map[string]string{hookClaimWorkBranchStoreDir: "someone-elses-branch"}),
		ResolveSessionWorkDir: func(string) string { return hookClaimWorkBranchSessionDir },
	}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1"}}

	patch := hookClaimIdentityPatch(bead, opts, ops, hookClaimWorkBranchStoreDir)

	if got, ok := patch[beadmeta.WorkDirMetadataKey]; ok {
		t.Errorf("stamped %s=%q over a bead that already records a checkout under both keys; the session fallback is for a bead that records NONE",
			beadmeta.WorkDirMetadataKey, got)
	}
	if got, ok := patch[beadmeta.WorkBranchMetadataKey]; ok {
		t.Errorf("stamped gc.work_branch=%q; the only checkout this bead records is the store, so the honest outcome is no branch", got)
	}
	assertNoWorkDirConflict(t, bead, patch)
}

// TestHookClaimRecordsNoWorkDirAsksTheRawQuestion pins that the fallback
// precondition is about what the bead RECORDS, not about what survived the
// store-dir exclusion. The two differ exactly for the bead above, and conflating
// them is what manufactured the conflict.
func TestHookClaimRecordsNoWorkDirAsksTheRawQuestion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		want     bool
	}{
		{name: "no keys at all", metadata: nil, want: true},
		{name: "blank values only", metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:       "   ",
			beadmeta.LegacyWorkDirMetadataKey: "",
		}, want: true},
		{name: "canonical records the store; still a record", metadata: map[string]string{
			beadmeta.WorkDirMetadataKey: hookClaimWorkBranchStoreDir,
		}, want: false},
		{name: "legacy records the store; still a record", metadata: map[string]string{
			beadmeta.LegacyWorkDirMetadataKey: hookClaimWorkBranchStoreDir,
		}, want: false},
		{name: "canonical records a real worker tree", metadata: map[string]string{
			beadmeta.WorkDirMetadataKey: "/worktrees/worker-7",
		}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bead := beads.Bead{ID: "gc-raw-1", Metadata: beads.StringMap{}}
			for k, v := range tc.metadata {
				bead.Metadata[k] = v
			}
			if got := hookClaimRecordsNoWorkDir(bead); got != tc.want {
				t.Errorf("hookClaimRecordsNoWorkDir = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHookClaimWorkerTreesExcludesTheStore pins the candidate list itself: the
// store directory is passed in only to be excluded, so the negative predicate
// stays expressible at the point of decision.
func TestHookClaimWorkerTreesExcludesTheStore(t *testing.T) {
	bead := beads.Bead{Metadata: beads.StringMap{
		beadmeta.WorkDirMetadataKey:       hookClaimWorkBranchStoreDir,
		beadmeta.LegacyWorkDirMetadataKey: "/worktrees/worker-3",
	}}

	got := hookClaimWorkerTrees(bead, hookClaimResolveStoreHead(hookClaimWorkBranchStoreDir))

	if len(got) != 1 || got[0].Dir != "/worktrees/worker-3" {
		t.Fatalf("hookClaimWorkerTrees = %v; want only the non-store checkout %q", got, "/worktrees/worker-3")
	}
}

// TestHookClaimWorkerTreesDedupesIdenticalKeys keeps a bead that records the same
// tree under both keys from producing two identical probes.
func TestHookClaimWorkerTreesDedupesIdenticalKeys(t *testing.T) {
	bead := beads.Bead{Metadata: beads.StringMap{
		beadmeta.WorkDirMetadataKey:       "/worktrees/worker-4",
		beadmeta.LegacyWorkDirMetadataKey: "/worktrees/worker-4",
	}}

	if got := hookClaimWorkerTrees(bead, hookClaimResolveStoreHead(hookClaimWorkBranchStoreDir)); len(got) != 1 {
		t.Fatalf("hookClaimWorkerTrees = %v; want one candidate, the duplicate collapsed", got)
	}
}

// TestHookClaimIdentityPatchFallsBackToSessionWorkDir covers gc-2n4c: a
// pool-routed bead records no checkout, because no slot is chosen until the claim
// happens, so the claim takes the checkout of the claiming session and stamps
// BOTH gc.work_dir and the branch resolved from it in the same patch.
func TestHookClaimIdentityPatchFallsBackToSessionWorkDir(t *testing.T) {
	bead := beads.Bead{ID: "hw-pool-routed", Metadata: beads.StringMap{}}
	ops := hookClaimOps{
		ResolveWorkBranch:     hookClaimBranchByDir(map[string]string{"/worktrees/sess-1": "bd-from-session"}),
		ResolveSessionWorkDir: func(string) string { return "/worktrees/sess-1" },
	}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1"}}

	patch := hookClaimIdentityPatch(bead, opts, ops, hookClaimWorkBranchStoreDir)

	if got := patch[beadmeta.WorkDirMetadataKey]; got != "/worktrees/sess-1" {
		t.Errorf("gc.work_dir = %q; want the checkout of the claiming session", got)
	}
	if got := patch[beadmeta.WorkBranchMetadataKey]; got != "bd-from-session" {
		t.Errorf("gc.work_branch = %q; want bd-from-session resolved from that checkout", got)
	}
}

// TestHookClaimIdentityPatchRefusesSessionWorkDirThatIsTheStore pins that the
// store refusal covers the session fallback too. A rig-scoped session can be
// running IN the shared rig checkout, and that tree is no more the workspace of
// this bead than it was when the branch was read from the store directly.
func TestHookClaimIdentityPatchRefusesSessionWorkDirThatIsTheStore(t *testing.T) {
	bead := beads.Bead{ID: "hw-rig-scoped", Metadata: beads.StringMap{}}
	ops := hookClaimOps{
		ResolveWorkBranch:     hookClaimBranchByDir(map[string]string{hookClaimWorkBranchStoreDir: "someone-elses-branch"}),
		ResolveSessionWorkDir: func(string) string { return hookClaimWorkBranchStoreDir },
	}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1"}}

	patch := hookClaimIdentityPatch(bead, opts, ops, hookClaimWorkBranchStoreDir)

	if got, ok := patch[beadmeta.WorkDirMetadataKey]; ok {
		t.Errorf("stamped gc.work_dir=%q; the shared rig checkout is not a per-bead workspace", got)
	}
	if got, ok := patch[beadmeta.WorkBranchMetadataKey]; ok {
		t.Errorf("stamped gc.work_branch=%q; expected no branch once the store is refused", got)
	}
}

// TestHookClaimIdentityPatchPrefersRecordedWorkDirOverSession keeps the session
// value a FALLBACK rather than a rewrite: a recorded checkout is the declared
// intent of the bead and the tree the work-record close gate resolves from.
func TestHookClaimIdentityPatchPrefersRecordedWorkDirOverSession(t *testing.T) {
	bead := beads.Bead{ID: "hw-recorded", Metadata: beads.StringMap{
		beadmeta.WorkDirMetadataKey: "/worktrees/recorded",
	}}
	ops := hookClaimOps{
		ResolveWorkBranch: hookClaimBranchByDir(map[string]string{
			"/worktrees/recorded": "bd-recorded",
			"/worktrees/sess-1":   "bd-from-session",
		}),
		ResolveSessionWorkDir: func(string) string {
			t.Error("ResolveSessionWorkDir must not be consulted when the bead records a checkout")
			return "/worktrees/sess-1"
		},
	}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1"}}

	patch := hookClaimIdentityPatch(bead, opts, ops, hookClaimWorkBranchStoreDir)

	if got := patch[beadmeta.WorkBranchMetadataKey]; got != "bd-recorded" {
		t.Errorf("gc.work_branch = %q; want bd-recorded from the checkout the bead records", got)
	}
	if _, ok := patch[beadmeta.WorkDirMetadataKey]; ok {
		t.Error("rewrote gc.work_dir; a recorded checkout must be left alone")
	}
}

// TestHookClaimIdentityPatchSkipsSessionFallbackForControlBead holds the
// control-bead boundary the session back-reference keys already observe: control
// steps stay session-free by the graphroute design, so a control claim must not
// acquire a session-derived workspace either.
func TestHookClaimIdentityPatchSkipsSessionFallbackForControlBead(t *testing.T) {
	bead := beads.Bead{ID: "hc-check", Metadata: beads.StringMap{beadmeta.KindMetadataKey: "check"}}
	ops := hookClaimOps{
		ResolveWorkBranch:     hookClaimBranchByDir(map[string]string{"/worktrees/sess-1": "bd-from-session"}),
		ResolveSessionWorkDir: func(string) string { return "/worktrees/sess-1" },
	}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1"}}

	patch := hookClaimIdentityPatch(bead, opts, ops, hookClaimWorkBranchStoreDir)

	if _, ok := patch[beadmeta.WorkDirMetadataKey]; ok {
		t.Error("stamped gc.work_dir on a control bead; control steps stay session-free")
	}
}

// TestSessionStampableWorkDirRefusesPoolManaged covers gc-j0cfh: the WorkDir of a
// pool-managed session is the slot label, a directory shared by every bead the
// slot ever runs, so stamping it would manufacture worktree-ownership evidence
// for a tree nobody owns.
func TestSessionStampableWorkDirRefusesPoolManaged(t *testing.T) {
	pooled := session.Info{PoolManaged: true, WorkDir: "/rigs/worker-slots/worker-3"}
	if got := sessionStampableWorkDir(pooled); got != "" {
		t.Errorf("sessionStampableWorkDir(pool-managed) = %q; want empty, a slot label is not a worktree", got)
	}

	owned := session.Info{WorkDir: "/worktrees/owned-1"}
	if got := sessionStampableWorkDir(owned); got != "/worktrees/owned-1" {
		t.Errorf("sessionStampableWorkDir(non-pool) = %q; want the recorded checkout", got)
	}
}

// hookClaimFixtureStoreBranch and the branches beside it are what each checkout in
// the fixture sits on. They all differ so a stamped branch names which tree it came
// from: the defect under test is the shared branch reaching a bead, and only a
// distinguishable name can show that it did.
const (
	hookClaimFixtureStoreBranch  = "shared-branch"
	hookClaimFixtureWorkerBranch = "worker-branch"
	hookClaimFixtureNestedBranch = "nested-branch"
	hookClaimFixtureLinkedBranch = "linked-branch"
)

// hookClaimStoreFixture is real filesystem and git state for the store exclusion.
// Every defect this file pins there exists only below the level of a path string
// (an alias, an enclosing repository, an administrative directory), so invented
// paths cannot exhibit any of them, and a predicate that only ever sees invented
// paths cannot be shown to handle one.
type hookClaimStoreFixture struct {
	root    string // holds the store and the worker checkout side by side
	store   string // the shared checkout, a repository on hookClaimFixtureStoreBranch
	alias   string // a symlink naming the store
	worker  string // an independent repository beside the store
	subdir  string // a plain directory INSIDE the store, no repository of its own
	gitDir  string // the store's own .git directory
	objects string // a directory inside the store's .git
	nested  string // an independent repository inside the store
	linked  string // a linked worktree cut from the store's repository
}

// newHookClaimStoreFixture builds that state. Every repository gets a commit, so a
// branch can actually be read out of it.
func newHookClaimStoreFixture(t *testing.T) hookClaimStoreFixture {
	t.Helper()
	f := hookClaimStoreFixture{root: t.TempDir()}
	f.store = filepath.Join(f.root, "shared-checkout")
	f.worker = filepath.Join(f.root, "worker-1")
	hookClaimInitRepo(t, f.store, hookClaimFixtureStoreBranch)
	hookClaimInitRepo(t, f.worker, hookClaimFixtureWorkerBranch)

	f.alias = filepath.Join(f.root, "store-by-another-name")
	if err := os.Symlink(f.store, f.alias); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}

	f.subdir = filepath.Join(f.store, "worker-slots", "worker-1")
	if err := os.MkdirAll(f.subdir, 0o755); err != nil {
		t.Fatalf("creating a subdirectory of the store: %v", err)
	}
	f.gitDir = filepath.Join(f.store, ".git")
	f.objects = filepath.Join(f.gitDir, "objects")
	f.nested = filepath.Join(f.store, "nested-checkout")
	hookClaimInitRepo(t, f.nested, hookClaimFixtureNestedBranch)
	f.linked = filepath.Join(f.root, "linked-worktree")
	mustGit(t, f.store, "worktree", "add", "-b", hookClaimFixtureLinkedBranch, f.linked)
	return f
}

// hookClaimInitRepo makes dir a repository on branch with one commit, so rev-parse
// can answer both --absolute-git-dir and --abbrev-ref HEAD from it.
func hookClaimInitRepo(t *testing.T, dir, branch string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	mustGit(t, "", "-c", "init.defaultBranch="+branch, "init", dir)
	mustGit(t, dir, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
}

// TestHookClaimStoreHeadComparesRepositoriesNotPaths covers the ways a candidate's
// name can disagree with the repository its branch would come from. Each refusal
// here is a way the exclusion was observed to fail, and each admission is a tree
// that reports a branch of its own and must keep it.
//
// The branch is read by running git inside the candidate, and git answers that read
// from the repository it discovers, so the comparison is between repositories. That
// is also why the cases inside the store's own directory land on opposite sides: a
// subdirectory and the .git directory are both answered by the store, while a
// nested checkout and a linked worktree answer for themselves.
func TestHookClaimStoreHeadComparesRepositoriesNotPaths(t *testing.T) {
	t.Run("refuses every spelling the store answers for", func(t *testing.T) {
		f := newHookClaimStoreFixture(t)
		store := hookClaimResolveStoreHead(f.store)
		for _, tc := range []struct {
			name      string
			candidate string
		}{
			{"a symlink naming the store", f.alias},
			{"a plain subdirectory of the store", f.subdir},
			{"the store's own .git directory", f.gitDir},
			{"a directory inside the store's .git", f.objects},
		} {
			if !store.Covers(tc.candidate) {
				t.Errorf("%s was admitted; a branch read there is answered by the store, so it re-stamps the shared branch", tc.name)
			}
		}
		// The alias is the one case where direction is meaningful.
		if !hookClaimResolveStoreHead(f.alias).Covers(f.store) {
			t.Errorf("the refusal depends on which side carries the alias")
		}
	})

	t.Run("admits every tree that answers for itself", func(t *testing.T) {
		f := newHookClaimStoreFixture(t)
		store := hookClaimResolveStoreHead(f.store)
		for _, tc := range []struct {
			name      string
			candidate string
		}{
			{"a checkout beside the store", f.worker},
			{"an independent checkout nested inside the store", f.nested},
			{"a linked worktree cut from the store's repository", f.linked},
		} {
			if store.Covers(tc.candidate) {
				t.Errorf("%s was refused; it reports a branch of its own, and refusing it costs the bead a branch it legitimately had", tc.name)
			}
		}
	})

	t.Run("a relative name for the store is the store", func(t *testing.T) {
		f := newHookClaimStoreFixture(t)
		t.Chdir(f.root)
		if !hookClaimResolveStoreHead(f.store).Covers("shared-checkout") {
			t.Errorf("a relative spelling of the store was admitted; it resolves to the same directory")
		}
	})

	t.Run("a store with no repository refuses a usable candidate", func(t *testing.T) {
		f := newHookClaimStoreFixture(t)
		plain := filepath.Join(f.root, "not-a-repository")
		if err := os.MkdirAll(plain, 0o755); err != nil {
			t.Fatalf("creating %s: %v", plain, err)
		}
		// Nothing can be compared against a store whose repository was never
		// identified, and the premise of the change is that the store's branch is
		// worse than no branch, so the refusal holds when the answer is unknown. A
		// store that genuinely holds no repository hands out no branch either, so
		// this costs only the claim-time convenience stamp.
		if !hookClaimResolveStoreHead(plain).Covers(f.worker) {
			t.Errorf("a candidate was admitted against a store whose repository was never identified")
		}
	})

	t.Run("a store whose probe never answered refuses a usable candidate", func(t *testing.T) {
		f := newHookClaimStoreFixture(t)
		// The arm a timed-out or unrunnable git lands in. It is reached by state
		// rather than by a real slow mount, because a deadline cannot be made to
		// expire on demand here; hookClaimClassifyGitOutput is where the mapping from
		// a real timeout to this state is pinned.
		unavailable := hookClaimStoreHead{dir: f.store, probe: hookClaimProbeUnavailable}
		if !unavailable.Covers(f.worker) {
			t.Errorf("a candidate was admitted while the store's probe had established nothing")
		}
	})

	t.Run("dot-dot across a symlink does not fold to the store", func(t *testing.T) {
		f := newHookClaimStoreFixture(t)

		// The candidate has to be a path that cleans to the store and really names a
		// DIFFERENT EXISTING repository. Both halves matter. If it cleans to something
		// else the lexical shortcut was never in play, and if the real target is not a
		// repository the case returns early on that instead, which proves nothing
		// about the fold: the negative reading would be manufactured by a missing
		// value rather than by the comparison under test.
		//
		// So: "hop" is a symlink to <elsewhere>/inner. "<root>/hop/../shared-checkout"
		// cleans to "<root>/shared-checkout", the store. Followed for real it is
		// <elsewhere>/inner/.. which is <elsewhere>, then <elsewhere>/shared-checkout,
		// a checkout that exists and is not the store.
		elsewhere := t.TempDir()
		target := filepath.Join(elsewhere, "shared-checkout")
		hookClaimInitRepo(t, target, "elsewhere-branch")
		if err := os.MkdirAll(filepath.Join(elsewhere, "inner"), 0o755); err != nil {
			t.Fatalf("creating the symlink target: %v", err)
		}
		hop := filepath.Join(f.root, "hop")
		if err := os.Symlink(filepath.Join(elsewhere, "inner"), hop); err != nil {
			t.Skipf("this filesystem does not support symlinks: %v", err)
		}
		// Concatenated, not filepath.Join: Join cleans its result and would fold the
		// ".." away here, handing the predicate the store path itself.
		candidate := hop + "/../shared-checkout"

		// The fixture's own preconditions, asserted rather than assumed.
		if got, want := filepath.Clean(candidate), filepath.Clean(f.store); got != want {
			t.Fatalf("fixture does not exercise the fold: Clean(candidate) = %q, want the store %q", got, want)
		}
		candRepo, probe := hookClaimHeadRepoDir(candidate)
		if probe != hookClaimProbeAnswered {
			t.Fatalf("fixture candidate resolves no repository; the case would return early and prove nothing about the fold")
		}
		if !hookClaimSameDir(candRepo, filepath.Join(target, ".git")) {
			t.Fatalf("fixture candidate resolves repository %s, not the one at %s", candRepo, target)
		}
		if hookClaimSameDir(candRepo, filepath.Join(f.store, ".git")) {
			t.Fatalf("fixture candidate IS the store's repository; the case cannot distinguish anything")
		}

		if hookClaimResolveStoreHead(f.store).Covers(candidate) {
			t.Errorf("a candidate that merely cleans to the store was refused; it names the checkout at %s", target)
		}
	})
}

// TestHookClaimClassifyGitOutputSeparatesAnsweredFromNotAsked pins the distinction
// the store exclusion rests on. A git that ran and exited nonzero has ANSWERED for
// the queries here: "not a git repository" and "cannot change to <dir>" both mean no
// repository covers the directory, and the branch read will fail the same way. A
// query that never completed has established nothing, and treating it as an answer
// admits the shared checkout whenever a probe times out or git cannot be run.
//
// The deadline has to be checked before the run error, because a query the context
// kills still surfaces as an ExitError.
func TestHookClaimClassifyGitOutputSeparatesAnsweredFromNotAsked(t *testing.T) {
	exitErr := &exec.ExitError{ProcessState: &os.ProcessState{}}
	startErr := &exec.Error{Name: "git", Err: exec.ErrNotFound}

	for _, tc := range []struct {
		name      string
		ctxErr    error
		runErr    error
		out       string
		wantValue string
		wantProbe hookClaimProbe
	}{
		{"a value came back", nil, nil, "/repo/.git\n", "/repo/.git", hookClaimProbeAnswered},
		{"git ran and reported nothing here", nil, exitErr, "", "", hookClaimProbeAbsent},
		{"the deadline expired", context.DeadlineExceeded, exitErr, "", "", hookClaimProbeUnavailable},
		{"the caller canceled", context.Canceled, nil, "/repo/.git\n", "", hookClaimProbeUnavailable},
		{"git could not be started", nil, startErr, "", "", hookClaimProbeUnavailable},
		{"git exited cleanly with nothing to say", nil, nil, "  \n", "", hookClaimProbeUnavailable},
	} {
		value, probe := hookClaimClassifyGitOutput(tc.ctxErr, tc.runErr, tc.out)
		if value != tc.wantValue || probe != tc.wantProbe {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, value, probe, tc.wantValue, tc.wantProbe)
		}
	}
}

// TestHookClaimBranchLookupIgnoresALeakedGitDir pins the environment both git
// queries run under. gc can be invoked from a pre-commit hook or from nested
// worktree tooling, both of which export GIT_DIR and GIT_WORK_TREE, and git reads
// those ahead of its own -C argument. A leaked pair would make every query answer
// about the leaking repository instead of the directory the bead recorded: the
// resolver would report a branch from a tree the bead never named, and the exclusion
// would compare that repository against itself and refuse every candidate.
// internal/git.SanitizedEnv exists for exactly this and documents that callers
// shelling out to git should assign it; both queries here go through one runner that
// does.
func TestHookClaimBranchLookupIgnoresALeakedGitDir(t *testing.T) {
	f := newHookClaimStoreFixture(t)
	t.Setenv("GIT_DIR", f.gitDir)
	t.Setenv("GIT_WORK_TREE", f.store)

	if got := hookResolveWorkBranch(hookClaimTreeFor(t, f.worker)); got != hookClaimFixtureWorkerBranch {
		t.Errorf("hookResolveWorkBranch read %q from the worker checkout, want %q; a leaked GIT_DIR redirected the lookup at the store",
			got, hookClaimFixtureWorkerBranch)
	}
	store := hookClaimResolveStoreHead(f.store)
	if store.Covers(f.worker) {
		t.Errorf("the worker checkout was refused as the store; a leaked GIT_DIR made both sides resolve to the same repository")
	}
	if !store.Covers(f.subdir) {
		t.Errorf("a subdirectory of the store was admitted while GIT_DIR was set; the refusal must not depend on the ambient environment")
	}
}

// TestHookClaimWorkerTreesExcludesEverySpellingOfTheStore pins the refusal at the
// candidate list, not only at the predicate, because the list is what the claim path
// calls.
func TestHookClaimWorkerTreesExcludesEverySpellingOfTheStore(t *testing.T) {
	f := newHookClaimStoreFixture(t)
	store := hookClaimResolveStoreHead(f.store)

	for _, tc := range []struct {
		name      string
		candidate string
	}{
		{"an alias of the store", f.alias},
		{"a subdirectory of the store", f.subdir},
		{"the store's .git directory", f.gitDir},
	} {
		bead := beads.Bead{Metadata: beads.StringMap{beadmeta.WorkDirMetadataKey: tc.candidate}}
		if trees := hookClaimWorkerTrees(bead, store); len(trees) != 0 {
			t.Errorf("hookClaimWorkerTrees kept %v for a bead recording %s", trees, tc.name)
		}
	}

	recorded := beads.Bead{Metadata: beads.StringMap{beadmeta.WorkDirMetadataKey: f.worker}}
	trees := hookClaimWorkerTrees(recorded, store)
	if len(trees) != 1 || trees[0].Dir != f.worker {
		t.Fatalf("hookClaimWorkerTrees returned %v; a real worker checkout must survive", trees)
	}
	if trees[0].RepoDir == "" {
		t.Errorf("the surviving checkout carries no repository; the branch read has nothing to pin to")
	}
}

// TestHookClaimIdentityPatchReadsTheBranchFromTheWorkerNotTheStore drives the whole
// identity patch with the PRODUCTION branch resolver over real repositories. Every
// round of this fix was caught one layer below here, against a fake resolver or the
// predicate alone, and the cases inside the store's own directory survived all of
// them because a fake resolver cannot reproduce the one behavior that makes them
// defects: git answers a branch read from the repository it discovers.
//
// The arms that expect a branch are the positive control. Without them an empty
// branch would be indistinguishable from a resolver that answers nothing at all,
// and the refusals would be unproven.
func TestHookClaimIdentityPatchReadsTheBranchFromTheWorkerNotTheStore(t *testing.T) {
	f := newHookClaimStoreFixture(t)
	ops := hookClaimOps{ResolveWorkBranch: hookResolveWorkBranch}

	for _, tc := range []struct {
		name       string
		recorded   string
		wantBranch string
	}{
		{"the store itself", f.store, ""},
		{"an alias of the store", f.alias, ""},
		{"a plain subdirectory of the store", f.subdir, ""},
		{"the store's own .git directory", f.gitDir, ""},
		{"a directory inside the store's .git", f.objects, ""},
		{"the worker's own checkout", f.worker, hookClaimFixtureWorkerBranch},
		{"a checkout nested inside the store", f.nested, hookClaimFixtureNestedBranch},
		{"a linked worktree of the store's repository", f.linked, hookClaimFixtureLinkedBranch},
	} {
		bead := beads.Bead{
			ID:       "gc-branch-source",
			Metadata: beads.StringMap{beadmeta.WorkDirMetadataKey: tc.recorded},
		}
		patch := hookClaimIdentityPatch(bead, hookClaimOptions{}, ops, f.store)
		if got := patch[beadmeta.WorkBranchMetadataKey]; got != tc.wantBranch {
			t.Errorf("a bead recording %s was stamped %s = %q, want %q",
				tc.name, beadmeta.WorkBranchMetadataKey, got, tc.wantBranch)
		}
		assertNoWorkDirConflict(t, bead, patch)
	}
}

// TestHookClaimIdentityPatchLeavesACanonicalOnlyStoreAlone covers the case a
// contributor check found unpinned: a bead naming the store under the canonical key
// with the legacy key empty. The session fallback must not fire for it, because the
// bead does record a checkout, it just records an unusable one. Nothing else in the
// suite fails when the precondition is reverted for this shape.
func TestHookClaimIdentityPatchLeavesACanonicalOnlyStoreAlone(t *testing.T) {
	bead := beads.Bead{
		ID: "gc-canonical-only",
		Metadata: beads.StringMap{
			beadmeta.WorkDirMetadataKey: hookClaimWorkBranchStoreDir,
		},
	}
	ops := hookClaimOps{
		ResolveWorkBranch:     func(hookClaimWorkTree) string { return "" },
		ResolveSessionWorkDir: func(string) string { return hookClaimWorkBranchSessionDir },
	}
	patch := hookClaimIdentityPatch(bead, hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1"}}, ops, hookClaimWorkBranchStoreDir)

	if got, ok := patch[beadmeta.WorkDirMetadataKey]; ok {
		t.Errorf("rewrote %s to %q over a bead that already records a checkout, even though the recorded one is the store",
			beadmeta.WorkDirMetadataKey, got)
	}
	if got := patch[beadmeta.WorkBranchMetadataKey]; got != "" {
		t.Errorf("stamped %s = %q; the only recorded checkout is the store, so the honest result is no branch",
			beadmeta.WorkBranchMetadataKey, got)
	}
	assertNoWorkDirConflict(t, bead, patch)
}

// hookClaimFixtureGitFile is a directory OUTSIDE the store whose .git is a file
// naming the store's git directory, which is how a linked worktree is spelled. git
// resolves it to the store's own repository and answers the store's branch from it,
// so it is a candidate that no path comparison can catch: it is not under the store,
// it is not a symlink to it, and it cleans to a name of its own.
const hookClaimFixtureGitFile = "worker-by-gitfile"

// hookClaimGitFileToStore builds that candidate and returns its path.
func hookClaimGitFileToStore(t *testing.T, f hookClaimStoreFixture) string {
	t.Helper()
	dir := filepath.Join(f.root, hookClaimFixtureGitFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+f.gitDir+"\n"), 0o644); err != nil {
		t.Fatalf("writing the gitfile: %v", err)
	}
	repo, probe := hookClaimHeadRepoDir(dir)
	if probe != hookClaimProbeAnswered || !hookClaimSameDir(repo, f.gitDir) {
		t.Fatalf("fixture resolves (%q, %v); it has to answer for the store's repository or it proves nothing", repo, probe)
	}
	if !hookClaimPathOutside(dir, f.store) {
		t.Fatalf("fixture does not lie outside the store; the case cannot show what the path comparison misses")
	}
	return dir
}

// hookClaimGitShim puts a fake git first on PATH that runs kill -TERM on itself for
// the queries matchArg selects, and delegates everything else to the real git. A
// signal is not a deadline and not an exit status: the process never answers.
//
// The shim delegates through an absolute PATH captured before the override, so the
// test needs no lookup of its own and cannot recurse into itself.
func hookClaimGitShim(t *testing.T, matchArg string) {
	t.Helper()
	realPath := os.Getenv("PATH")
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = '" + matchArg + "' ]; then kill -TERM $$; fi\n" +
		"done\n" +
		"PATH='" + realPath + "' exec git \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the git shim: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+realPath)
}

// hookClaimGitStallShim puts a fake git first on PATH that outlives the probe
// deadline, so a case can observe a query the context really killed rather than a
// state a test constructed. It costs one probe timeout of wall clock.
func hookClaimGitStallShim(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\nexec sleep " + strconv.Itoa(int(hookClaimGitProbeTimeout.Seconds())*4) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the stalling git shim: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestHookClaimProbeDeadlineIsNotAnAnswer proves the UNAVAILABLE classification from
// a real subprocess. The table above constructs its error states, which pins the
// ordering of the branches but cannot show that a query the deadline kills reaches
// the one that refuses: a killed process surfaces as an ExitError, the same type a
// git that answered "no repository here" surfaces as.
func TestHookClaimProbeDeadlineIsNotAnAnswer(t *testing.T) {
	f := newHookClaimStoreFixture(t)
	// Resolved before the shim, so the refusal below is attributable to the
	// candidate's identity being unknown and not to the store's.
	store := hookClaimResolveStoreHead(f.store)
	if store.probe != hookClaimProbeAnswered {
		t.Fatalf("the store probe is %v; this case needs a store that IS identified", store.probe)
	}
	hookClaimGitStallShim(t)

	if value, probe := hookClaimHeadRepoDir(f.worker); probe != hookClaimProbeUnavailable {
		t.Errorf("a probe the deadline killed classified as (%q, %v), want %v", value, probe, hookClaimProbeUnavailable)
	}
	if _, admitted := store.Admit(f.worker); admitted {
		t.Error("admitted a candidate whose identity could not be established; an unknown repository has to be refused")
	}
}

// TestHookClaimSignalKilledProbeIsNotAnAnswer pins the difference between a git that
// ran and reported nothing and a git that never reported. Both surface as an
// ExitError. Only the first one is an answer.
//
// A process killed by a signal has established nothing about which repository covers
// a directory, and reading that as "no repository here" admits the shared checkout
// under any spelling no path comparison can catch -- here a directory whose .git file
// names the store's repository, which is outside the store and answers the store's
// branch. The session fallback is what makes that costly: it writes the admitted
// directory onto the bead as the workspace this work happened in.
//
// The arm where the probe is NOT killed is the positive control. Without it a refusal
// would be indistinguishable from a shim that breaks git altogether.
func TestHookClaimSignalKilledProbeIsNotAnAnswer(t *testing.T) {
	f := newHookClaimStoreFixture(t)
	candidate := hookClaimGitFileToStore(t, f)
	hookClaimGitShim(t, "--absolute-git-dir")

	if _, probe := hookClaimHeadRepoDir(candidate); probe != hookClaimProbeUnavailable {
		t.Errorf("a signal-killed identity probe classified as %v, want %v", probe, hookClaimProbeUnavailable)
	}
	if branch, probe := hookClaimRunGit(f.worker, "rev-parse", "--abbrev-ref", "HEAD"); probe != hookClaimProbeAnswered ||
		branch != hookClaimFixtureWorkerBranch {
		t.Fatalf("the shim also broke the queries it was not told to kill: (%q, %v); the refusals below would prove nothing", branch, probe)
	}

	bead := beads.Bead{ID: "gc-signaled", Metadata: beads.StringMap{}}
	ops := hookClaimOps{
		ResolveWorkBranch:     hookResolveWorkBranch,
		ResolveSessionWorkDir: func(string) string { return candidate },
	}
	patch := hookClaimIdentityPatch(bead, hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sig1"}}, ops, f.store)

	if got, ok := patch[beadmeta.WorkDirMetadataKey]; ok {
		t.Errorf("stamped %s = %q from a probe that was killed before it answered", beadmeta.WorkDirMetadataKey, got)
	}
	if got := patch[beadmeta.WorkBranchMetadataKey]; got != "" {
		t.Errorf("stamped %s = %q; the candidate resolves the store's repository", beadmeta.WorkBranchMetadataKey, got)
	}
	assertNoWorkDirConflict(t, bead, patch)
}

// TestHookClaimRefusesTheStoreWhenGitDeclinesToAnswer pins the one comparison that
// needs no cooperation from git.
//
// git reports "no repository covers this directory" and "I will not look at this
// repository" with the same exit status. A shared checkout owned by another user is
// refused for dubious ownership, which is the common way this happens: the store IS a
// repository, the claiming session cannot ask about it, and every query inside it
// fails the same way. Reading that as "no repository" admits the store's own
// subdirectories, and the session fallback then records one of them as the workspace
// this bead's work happened in -- the exact false provenance this change exists to
// stop, reached without a branch ever being read.
//
// The fixture asserts git really did refuse, so the case cannot pass by not reaching
// the condition.
func TestHookClaimRefusesTheStoreWhenGitDeclinesToAnswer(t *testing.T) {
	f := newHookClaimStoreFixture(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	store := hookClaimResolveStoreHead(f.store)
	if store.probe != hookClaimProbeAbsent {
		t.Fatalf("the store probe is %v; this case needs git to refuse it with an exit status", store.probe)
	}
	if _, probe := hookClaimHeadRepoDir(f.subdir); probe != hookClaimProbeAbsent {
		t.Fatalf("the subdirectory probe is %v; git has to refuse it the same way", probe)
	}

	if _, admitted := store.Admit(f.subdir); admitted {
		t.Errorf("admitted %s, a directory inside the shared checkout, because git declined to identify either one", f.subdir)
	}

	bead := beads.Bead{ID: "gc-dubious", Metadata: beads.StringMap{}}
	ops := hookClaimOps{
		ResolveWorkBranch:     hookResolveWorkBranch,
		ResolveSessionWorkDir: func(string) string { return f.subdir },
	}
	patch := hookClaimIdentityPatch(bead, hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-dub1"}}, ops, f.store)
	if got, ok := patch[beadmeta.WorkDirMetadataKey]; ok {
		t.Errorf("stamped %s = %q, a directory inside the shared checkout", beadmeta.WorkDirMetadataKey, got)
	}

	// Outside the store directory is refused too, and that is not over-reach. A
	// directory anywhere on disk can reach the store's repository through a .git file
	// or an administrative path, and under this refusal it reads exactly like an
	// unrelated directory: both answer ABSENT, and no comparison of paths separates
	// them. So while the store holds a repository that will not identify itself,
	// nothing can be cleared.
	outside := filepath.Join(f.root, "not-in-the-store")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("creating %s: %v", outside, err)
	}
	if _, admitted := store.Admit(outside); admitted {
		t.Errorf("admitted %s while the store holds a repository git refused to identify; an outside directory can still reach that repository through a .git file", outside)
	}

	// The refusal is about a repository that declined, not about git being unhappy in
	// general. A store directory that holds no repository at all has no branch to
	// leak, so the path comparison is the whole question there and an outside
	// candidate is still admitted.
	bare := t.TempDir()
	plain := hookClaimResolveStoreHead(bare)
	if plain.probe == hookClaimProbeAnswered {
		t.Fatalf("the no-repository store probe is %v; this half needs a directory git finds nothing in", plain.probe)
	}
	if hookClaimDirLooksLikeRepo(bare) {
		t.Fatalf("%s carries a repository on disk; the two readings are not separated", bare)
	}
	usable := filepath.Join(bare, "..", "usable-checkout")
	if err := os.MkdirAll(filepath.Clean(usable), 0o755); err != nil {
		t.Fatalf("creating %s: %v", usable, err)
	}
	if _, admitted := plain.Admit(filepath.Clean(usable)); !admitted {
		t.Errorf("refused %s against a store that holds no repository; there is nothing to protect there", filepath.Clean(usable))
	}
}

// TestHookClaimBranchComesFromTheValidatedRepository pins that the branch is read out
// of the repository the exclusion was decided against, rather than out of whatever
// the path resolves to when the read happens.
//
// Those are two different answers to the same question, and the gap between them is
// writable: a recorded path can be a symlink, and repointing it at the shared
// checkout between the two answers stamps the shared branch onto the bead with the
// refusal having seen a different repository entirely. Reading HEAD from the
// repository by name closes the gap -- there is no path left to repoint.
func TestHookClaimBranchComesFromTheValidatedRepository(t *testing.T) {
	f := newHookClaimStoreFixture(t)
	workerRepo, probe := hookClaimHeadRepoDir(f.worker)
	if probe != hookClaimProbeAnswered {
		t.Fatalf("the worker checkout resolves no repository: %v", probe)
	}

	t.Run("the repository decides, not the directory", func(t *testing.T) {
		crossed := hookClaimWorkTree{Dir: f.store, RepoDir: workerRepo}
		if got := hookResolveWorkBranch(crossed); got != hookClaimFixtureWorkerBranch {
			t.Errorf("read %q from a tree naming the store with the worker's repository, want %q; the read followed the path",
				got, hookClaimFixtureWorkerBranch)
		}
	})

	t.Run("a tree with no repository yields no branch", func(t *testing.T) {
		if got := hookResolveWorkBranch(hookClaimWorkTree{Dir: f.worker}); got != "" {
			t.Errorf("read %q from a tree that identified no repository, want no branch", got)
		}
	})

	t.Run("repointing the recorded path after it is admitted does not move the branch", func(t *testing.T) {
		candidate := filepath.Join(f.root, "moving-worker")
		if err := os.Symlink(f.worker, candidate); err != nil {
			t.Skipf("this filesystem does not support symlinks: %v", err)
		}
		retargeted := false
		ops := hookClaimOps{ResolveWorkBranch: func(tree hookClaimWorkTree) string {
			if !retargeted {
				if err := os.Remove(candidate); err != nil {
					t.Fatalf("removing the candidate symlink: %v", err)
				}
				if err := os.Symlink(f.store, candidate); err != nil {
					t.Fatalf("repointing the candidate at the store: %v", err)
				}
				retargeted = true
			}
			return hookResolveWorkBranch(tree)
		}}
		bead := beads.Bead{
			ID:       "gc-retargeted",
			Metadata: beads.StringMap{beadmeta.WorkDirMetadataKey: candidate},
		}
		patch := hookClaimIdentityPatch(bead, hookClaimOptions{}, ops, f.store)
		if !retargeted {
			t.Fatal("the resolver was never called, so nothing was raced")
		}
		if branch := hookResolveWorkBranch(hookClaimTreeFor(t, candidate)); branch != hookClaimFixtureStoreBranch {
			t.Fatalf("after the retarget the candidate resolves %q; the race did not reach the store", branch)
		}
		if got := patch[beadmeta.WorkBranchMetadataKey]; got != hookClaimFixtureWorkerBranch {
			t.Errorf("stamped %s = %q, want %q: the branch was re-resolved from the path after the refusal had cleared a different repository",
				beadmeta.WorkBranchMetadataKey, got, hookClaimFixtureWorkerBranch)
		}
		assertNoWorkDirConflict(t, bead, patch)
	})
}

// TestHookClaimRefusesTheCandidatesItCannotCompare pins that the containment check
// admits only on a reading it established, and refuses everything it could not.
//
// The earlier shape asked whether the candidate was inside the store and admitted
// whenever the answer was not yes. Three readings are not "no": a relative candidate
// means nothing without the directory it was written against, a relative store gives
// the comparison two different origins, and a name folding ".." does not name the
// directory Clean produces once a symlink precedes it. Each of those was measured to
// pass the old check and take a false workspace stamp while git was declining to
// identify the store.
func TestHookClaimRefusesTheCandidatesItCannotCompare(t *testing.T) {
	f := newHookClaimStoreFixture(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	store := hookClaimResolveStoreHead(f.store)
	if store.probe != hookClaimProbeAbsent {
		t.Fatalf("the store probe is %v; this case needs git to refuse it with an exit status", store.probe)
	}
	if !hookClaimDirLooksLikeRepo(f.store) {
		t.Fatalf("%s does not carry a repository on disk; the refusal would be about the wrong thing", f.store)
	}

	rel, err := filepath.Rel(f.root, f.subdir)
	if err != nil {
		t.Fatalf("relating %s to %s: %v", f.subdir, f.root, err)
	}
	dotdot := filepath.Join(f.store, "worker-slots", "..", "worker-slots", "worker-1")

	for _, tc := range []struct {
		name      string
		candidate string
	}{
		{"a relative candidate naming a directory inside the store", rel},
		{"a candidate that folds a .. back into the store", dotdot},
		{"a bare name with no directory at all", "worker-slots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, admitted := store.Admit(tc.candidate); admitted {
				t.Errorf("admitted %q; containment was never established for it", tc.candidate)
			}
		})
	}

	// The same asymmetry on the store side: a comparison with two origins is not a
	// comparison, so it cannot clear a candidate either.
	relStore := hookClaimStoreHead{dir: "shared-checkout", probe: hookClaimProbeAbsent}
	if _, admitted := relStore.Admit(f.subdir); admitted {
		t.Errorf("admitted %s against the relative store name %q", f.subdir, relStore.dir)
	}
}

// TestHookClaimPinnedBranchReadKeepsRelativeConfigSemantics pins that naming the
// repository did not move where git resolves relative paths from.
//
// --git-dir alone runs from the claiming process's own directory, and a relative
// GIT_CONFIG_GLOBAL -- which survives the sanitizer -- is then read out of that
// directory instead of the checkout being read. A seat whose own directory holds an
// unreadable config loses every branch stamp, with the failure looking exactly like
// "this worktree has no branch". The read carries -C as well so the pin chooses the
// repository and the directory chooses nothing else.
func TestHookClaimPinnedBranchReadKeepsRelativeConfigSemantics(t *testing.T) {
	f := newHookClaimStoreFixture(t)
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "rel.gitconfig"), []byte("[core]\n\tbare = not-a-boolean\n"), 0o644); err != nil {
		t.Fatalf("writing the unreadable config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.worker, "rel.gitconfig"), []byte("[user]\n\tname = fixture\n"), 0o644); err != nil {
		t.Fatalf("writing the worker config: %v", err)
	}
	t.Chdir(elsewhere)
	t.Setenv("GIT_CONFIG_GLOBAL", "rel.gitconfig")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	tree := hookClaimTreeFor(t, f.worker)
	if got := hookResolveWorkBranch(tree); got != hookClaimFixtureWorkerBranch {
		t.Errorf("branch = %q, want %q; the pinned read resolved its relative config against the process directory", got, hookClaimFixtureWorkerBranch)
	}
}
