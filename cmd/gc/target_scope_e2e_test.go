package main

// END-TO-END REGRESSION MATRIX for gc.target_scope.v1 (DESIGN §11).
//
// Every unit/boundary test in this build proves one production function in
// isolation against a hand-built bead. This file proves they COMPOSE along the
// real path: a real launch stamps a scope, and the real claim / reconcile /
// close-gate readers then honour it — from a claiming cwd whose branch and path
// deliberately differ from the declared scope, asserting no functional reader
// observes the cwd values (§11 preamble).
//
// This is the layer that catches composition bugs a unit test structurally
// cannot: "the launch never stamped the bead the close gate loads", "the guard
// is installed in tests but not in production". Sessions 2/4/5 each found one of
// exactly that class; the matrix is the standing net for it.
//
// The claim step drives the production hookClaimIdentityPatch /
// hookClaimMayStampCwdBranch with a resolver that is the production resolver's
// own body — targetscope.ResolveInherited(store, bead) — fed the REAL launched
// store; that production installs it is pinned separately by
// TestApplyDefaultsInstallsTargetScopeResolver. The reconcile and close-gate
// steps call the production functions directly.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// The claimant is parked on a shared checkout whose branch and path have
// nothing to do with any declared scope — the poison shape the whole design
// exists to keep out of functional reads.
const (
	e2ePoisonBranch  = "parked-shared-branch"
	e2ePoisonWorkDir = "/shared/parked/root"
	e2ePoisonDir     = "/shared/parked/checkout"
)

// e2eScopedFormulaDir writes a formula that DECLARES base_branch with a default,
// which is what makes base_branch a carrier this formula consumes: an explicit
// gc_target_branch then reaches BOTH scope.branch and the substituted step
// title, the equality the close gate later validates against.
func e2eScopedFormulaDir(t *testing.T, name string) string {
	t.Helper()
	return e2eWriteFormula(t, name, `formula = "`+name+`"
version = 1

[vars.base_branch]
default = "main"

[[steps]]
id = "work"
title = "Work on {{base_branch}}"
`)
}

// e2eWriteFormula writes a single formula file into a fresh temp dir and returns
// that dir (a formula search path).
func e2eWriteFormula(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing formula %s: %v", name, err)
	}
	return dir
}

// e2eSlingDeps builds SlingDeps talking to a fresh MemStore, mirroring the
// production wiring cmd/gc installs. Notify is nil (skip) so no controller poke
// is attempted; the runner is a no-op.
func e2eSlingDeps(t *testing.T, formulaDir string) (slingDeps, config.Agent) {
	t.Helper()
	agent := config.Agent{Name: "worker", Dir: "rig", MaxActiveSessions: intPtr(1)}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{agent}}
	cfg.FormulaLayers.City = []string{formulaDir}
	deps := slingDeps{
		CityName: "test-city",
		CityPath: t.TempDir(),
		Cfg:      cfg,
		Store:    beads.NewMemStore(),
		StoreRef: "city:test-city",
		Resolver: cliAgentResolver{},
		Runner:   func(_, _ string, _ map[string]string) (string, error) { return "", nil },
	}
	return deps, agent
}

// e2eLaunchScopedSling launches a REAL sling formula and returns the store plus
// every bead the launch materialized. The formula consumes base_branch, so vars
// like gc_target_branch=release drive the stamped scope end to end.
func e2eLaunchScopedSling(t *testing.T, deps slingDeps, agent config.Agent, formulaName string, vars ...string) []beads.Bead {
	t.Helper()
	opts := sling.SlingOpts{
		Target:        agent,
		BeadOrFormula: formulaName,
		IsFormula:     true,
		SkipPoke:      true,
		Vars:          vars,
	}
	if _, err := sling.DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling(%s): %v", formulaName, err)
	}
	return e2eStoreBeads(t, deps.Store)
}

func e2eStoreBeads(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("listing store: %v", err)
	}
	return all
}

// e2eFormulaRoot returns the one bead the launch stamped with gc.formula_name.
func e2eFormulaRoot(t *testing.T, all []beads.Bead) beads.Bead {
	t.Helper()
	var roots []beads.Bead
	for _, b := range all {
		if b.Metadata[beadmeta.FormulaNameMetadataKey] != "" {
			roots = append(roots, b)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("found %d formula roots after launch, want exactly one", len(roots))
	}
	return roots[0]
}

// e2eWorkStep returns the materialized non-root step, located by its title
// prefix (the step the scoped formula titles "Work on <branch>").
func e2eWorkStep(t *testing.T, all []beads.Bead) beads.Bead {
	t.Helper()
	var steps []beads.Bead
	for _, b := range all {
		if strings.HasPrefix(b.Title, "Work on ") {
			steps = append(steps, b)
		}
	}
	if len(steps) != 1 {
		t.Fatalf("found %d work steps after launch, want exactly one", len(steps))
	}
	return steps[0]
}

// e2eClaimPatch drives the production claim identity patch against the launched
// store from a poison cwd. The resolver is the production resolver's own body
// (ResolveInherited) fed the real store; ResolveWorkBranch always returns the
// poison branch, so a non-empty patch[gc.work_branch] means the cwd leaked in.
func e2eClaimPatch(store beads.Store, bead beads.Bead) map[string]string {
	ops := hookClaimOps{
		ResolveWorkBranch: func(string) string { return e2ePoisonBranch },
		ResolveTargetScope: func(_ string, _ []string, b beads.Bead) targetscope.Resolution {
			return targetscope.ResolveInherited(store, b)
		},
	}
	return hookClaimIdentityPatch(bead, hookClaimOptions{}, ops, e2ePoisonDir)
}

// e2ePoisonFlatKeys returns a copy of bead with a shipped work record whose flat
// gc.work_* values are all poison — the state a pool session parked on a shared
// checkout would leave. The close gate must read the declared scope, never these.
func e2ePoisonFlatKeys(bead beads.Bead) beads.Bead {
	md := beads.StringMap{}
	for k, v := range bead.Metadata {
		md[k] = v
	}
	md[beadmeta.WorkOutcomeMetadataKey] = beadmeta.WorkOutcomeShipped
	md[beadmeta.WorkCommitMetadataKey] = "cafef00d"
	md[beadmeta.WorkBranchMetadataKey] = e2ePoisonBranch
	md[beadmeta.WorkDirMetadataKey] = e2ePoisonWorkDir
	out := bead
	out.Metadata = md
	return out
}

// e2eEnvelope returns the scopeRoot and Envelope both anchored at the city
// path — the shape the production close gate is handed.
func e2eEnvelope(deps slingDeps) (string, targetscope.Envelope) {
	return deps.CityPath, targetscope.Envelope{CityPath: deps.CityPath, StorePath: deps.CityPath}
}

// e2eAssertClaimStampsNothing asserts the production claim path, driven from the
// poison cwd, leaves gc.work_branch unwritten — the declared scope wins.
func e2eAssertClaimStampsNothing(t *testing.T, store beads.Store, bead beads.Bead) {
	t.Helper()
	if patch := e2eClaimPatch(store, bead); patch[beadmeta.WorkBranchMetadataKey] != "" {
		t.Fatalf("claim stamped %s=%q from the cwd over a declared scope on %s",
			beadmeta.WorkBranchMetadataKey, patch[beadmeta.WorkBranchMetadataKey], bead.ID)
	}
}

// e2eAssertReconcileStandsDown asserts the reconcile work-dir writer refuses to
// stamp the session cwd over the (possibly inherited) declared scope.
func e2eAssertReconcileStandsDown(t *testing.T, store beads.Store, bead beads.Bead) {
	t.Helper()
	if mayStampCwdWorkDir(store, bead, nil) {
		t.Fatalf("reconcile would stamp the session cwd work_dir over the declared scope on %s", bead.ID)
	}
}

// e2eCloseGateLocation poisons the bead's flat keys (the shipped-record shape a
// parked pool session leaves) and runs the production close-gate location
// resolver, returning what the gate's git probe would receive.
func e2eCloseGateLocation(deps slingDeps, bead beads.Bead) (repoDir, declaredBranch, violation string) {
	scopeRoot, envelope := e2eEnvelope(deps)
	return workRecordCloseLocation(deps.Store, e2ePoisonFlatKeys(bead), scopeRoot, envelope)
}

// §11 #1 — Fresh claim of a scoped formula stage. The launched stage's
// gc.work_branch must NOT become the claiming cwd branch, and the close gate
// must validate against scope.branch, not the parked flat value.
func TestE2EFreshClaimOfScopedStageHonoursDeclaredScope(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-fresh")
	deps, agent := e2eSlingDeps(t, dir)
	all := e2eLaunchScopedSling(t, deps, agent, "e2e-fresh", "gc_target_branch=release")

	root := e2eFormulaRoot(t, all)
	rootScope := targetscope.Parse(root.Metadata[beadmeta.TargetScopeMetadataKey])
	if !rootScope.Valid() || rootScope.Scope.Branch != "release" {
		t.Fatalf("root scope = %+v, want valid branch=release", rootScope)
	}

	step := e2eWorkStep(t, all)
	e2eAssertClaimStampsNothing(t, deps.Store, step)
	e2eAssertReconcileStandsDown(t, deps.Store, step)

	// CLOSE GATE — the poisoned copy keeps its parent link, so the reader still
	// inherits the root's scope through the store; the branch comes from there.
	_, declaredBranch, violation := e2eCloseGateLocation(deps, step)
	if violation != "" {
		t.Fatalf("unexpected close-gate violation: %s", violation)
	}
	if declaredBranch != "release" {
		t.Fatalf("close gate declaredBranch = %q, want release (not the parked flat value)", declaredBranch)
	}
}

// §11 #2 — Existing-assignment and adopted-assignment claims run the same
// stampHookClaimIdentity seam, so a bead that already carries an assignee (or is
// being adopted by a different session) must have its cwd branch suppressed just
// like a fresh claim. Assignment state is orthogonal to the scope guard.
func TestE2EExistingAndAdoptedAssignmentClaimsStillSuppressCwd(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-adopt")
	deps, agent := e2eSlingDeps(t, dir)
	all := e2eLaunchScopedSling(t, deps, agent, "e2e-adopt", "gc_target_branch=release")
	step := e2eWorkStep(t, all)

	// Existing assignment: the step already carries a claiming session.
	step.Metadata[beadmeta.SessionIDMetadataKey] = "prior-session"
	step.Assignee = "prior-session"
	e2eAssertClaimStampsNothing(t, deps.Store, step)

	// Adopted assignment: a different session claims from a different poison cwd.
	ops := hookClaimOps{
		ResolveWorkBranch: func(string) string { return "another-parked-branch" },
		ResolveTargetScope: func(_ string, _ []string, b beads.Bead) targetscope.Resolution {
			return targetscope.ResolveInherited(deps.Store, b)
		},
	}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=adopting-session", "GC_SESSION_NAME=worker-2"}}
	patch := hookClaimIdentityPatch(step, opts, ops, "/some/other/checkout")
	if patch[beadmeta.WorkBranchMetadataKey] != "" {
		t.Fatalf("adopted-assignment claim stamped %s=%q over a declared scope",
			beadmeta.WorkBranchMetadataKey, patch[beadmeta.WorkBranchMetadataKey])
	}
	// The session back-reference is orthogonal and must still be recorded.
	if patch[beadmeta.SessionIDMetadataKey] != "adopting-session" {
		t.Fatalf("adopted claim dropped the session back-reference: %v", patch)
	}
}

// §11 #3 — Repeated claim from a different cwd. Re-claiming a scoped stage from
// a second, differently-parked checkout must still suppress the stamp: the
// scope is authoritative every time, and nothing drifts across claims.
func TestE2ERepeatedClaimFromDifferentCwdDoesNotDrift(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-reclaim")
	deps, agent := e2eSlingDeps(t, dir)
	all := e2eLaunchScopedSling(t, deps, agent, "e2e-reclaim", "gc_target_branch=release")
	step := e2eWorkStep(t, all)

	for _, cwd := range []struct{ branch, dir string }{
		{e2ePoisonBranch, e2ePoisonDir},
		{"second-parked-branch", "/second/parked/checkout"},
	} {
		ops := hookClaimOps{
			ResolveWorkBranch: func(string) string { return cwd.branch },
			ResolveTargetScope: func(_ string, _ []string, b beads.Bead) targetscope.Resolution {
				return targetscope.ResolveInherited(deps.Store, b)
			},
		}
		if patch := hookClaimIdentityPatch(step, hookClaimOptions{}, ops, cwd.dir); patch[beadmeta.WorkBranchMetadataKey] != "" {
			t.Fatalf("re-claim from %s stamped %s=%q, want the declared scope to keep winning",
				cwd.dir, beadmeta.WorkBranchMetadataKey, patch[beadmeta.WorkBranchMetadataKey])
		}
	}
}

// §11 #4 — Work-dir reconcile + root propagation. A scope declaring a worktree
// must suppress the cwd work_dir stamp on BOTH the root and the inheriting
// stage, and the stage's close gate must resolve the declared worktree (through
// inheritance), never the parked path. The worktree is persisted absolute.
func TestE2EWorkDirReconcileAndRootPropagation(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-worktree")
	deps, agent := e2eSlingDeps(t, dir)
	all := e2eLaunchScopedSling(t, deps, agent, "e2e-worktree",
		"gc_target_branch=release", "gc_target_worktree=worktrees/T")

	root := e2eFormulaRoot(t, all)
	rootScope := targetscope.Parse(root.Metadata[beadmeta.TargetScopeMetadataKey])
	if !rootScope.Valid() || rootScope.Scope.Worktree == "" {
		t.Fatalf("root scope = %+v, want a valid worktree", rootScope)
	}
	if !filepath.IsAbs(rootScope.Scope.Worktree) {
		t.Fatalf("root scope worktree %q is not absolute; normalization must happen at the boundary", rootScope.Scope.Worktree)
	}

	step := e2eWorkStep(t, all)
	// Neither the root nor the inheriting stage may take the session cwd work_dir.
	e2eAssertReconcileStandsDown(t, deps.Store, root)
	e2eAssertReconcileStandsDown(t, deps.Store, step)
	e2eAssertClaimStampsNothing(t, deps.Store, step)

	// The stage inherits the declared worktree; the gate probes there, not the
	// parked root.
	repoDir, declaredBranch, violation := e2eCloseGateLocation(deps, step)
	if violation != "" {
		t.Fatalf("unexpected close-gate violation: %s", violation)
	}
	if declaredBranch != "release" {
		t.Fatalf("declaredBranch = %q, want release", declaredBranch)
	}
	if repoDir != rootScope.Scope.Worktree {
		t.Fatalf("close gate repoDir = %q, want the inherited declared worktree %q (not the parked path)", repoDir, rootScope.Scope.Worktree)
	}
	if repoDir == e2ePoisonWorkDir {
		t.Fatal("close gate resolved the parked work_dir")
	}
}

// §11 #5 — The symbolic-ref class the whole design exists for. A formula stage
// carries only a gc.root_bead_id pointer, not a copy of root metadata. The
// inherited-scope resolver must reach the root's scope through that pointer so
// the claim guard suppresses cwd and the close gate reads the declared branch.
func TestE2ESymbolicRootRefStageInheritsScope(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-symbolic")
	deps, agent := e2eSlingDeps(t, dir)
	all := e2eLaunchScopedSling(t, deps, agent, "e2e-symbolic", "gc_target_branch=release")
	root := e2eFormulaRoot(t, all)

	// A stage that references the root ONLY through gc.root_bead_id — no parent
	// link, no scope of its own. This is the graph-stage shape.
	stage, err := deps.Store.Create(beads.Bead{
		Title:    "symbolic stage",
		Metadata: beads.StringMap{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("creating symbolic-ref stage: %v", err)
	}

	if got := targetscope.ResolveInherited(deps.Store, stage); !got.Valid() || got.Scope.Branch != "release" {
		t.Fatalf("inherited resolution = %+v, want valid branch=release through the root ref", got)
	}
	e2eAssertClaimStampsNothing(t, deps.Store, stage)
	e2eAssertReconcileStandsDown(t, deps.Store, stage)

	_, declaredBranch, violation := e2eCloseGateLocation(deps, stage)
	if violation != "" || declaredBranch != "release" {
		t.Fatalf("close gate on symbolic-ref stage = (%q, %q), want (release, no violation)", declaredBranch, violation)
	}
}

// §11 #6 — Attempt 2 (a retry stage) resolves the ORIGINAL root's scope. A
// retry materializes a fresh attempt bead that points back at the original root;
// it must inherit the same declared scope rather than resolving a new one from
// the cwd it happens to be claimed in.
func TestE2ERetryAttemptResolvesOriginalRootScope(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-retry")
	deps, agent := e2eSlingDeps(t, dir)
	all := e2eLaunchScopedSling(t, deps, agent, "e2e-retry", "gc_target_branch=release")
	root := e2eFormulaRoot(t, all)
	original := e2eWorkStep(t, all)

	// The retry attempt references the original root (the inherit-don't-copy
	// contract: a retry is safe by lineage, §9).
	retry, err := deps.Store.Create(beads.Bead{
		Title:    original.Title + " (attempt 2)",
		Metadata: beads.StringMap{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("creating retry attempt: %v", err)
	}

	got := targetscope.ResolveInherited(deps.Store, retry)
	if !got.Valid() || got.Scope.Branch != "release" {
		t.Fatalf("retry inherited scope = %+v, want the original root's release", got)
	}
	e2eAssertClaimStampsNothing(t, deps.Store, retry)
}

// §11 #10 / #13 — Poisoned-input-member and no-trusted-source. A launch with no
// branch carrier of any kind resolves a present-valid FIELD-EMPTY object, never
// absent. Absence is the only state that re-enables the cwd writers, so a
// carrierless launch must still lock them out: claim and reconcile stand down,
// and both readings the close gate makes refuse the parked flat keys.
func TestE2ECarrierlessLaunchStampsUnknownNotAbsent(t *testing.T) {
	dir := e2eWriteFormula(t, "e2e-carrierless",
		"formula = \"e2e-carrierless\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n")
	deps, agent := e2eSlingDeps(t, dir)
	all := e2eLaunchScopedSling(t, deps, agent, "e2e-carrierless")

	root := e2eFormulaRoot(t, all)
	got := targetscope.Parse(root.Metadata[beadmeta.TargetScopeMetadataKey])
	if !got.Valid() {
		t.Fatalf("carrierless root scope state = %v, want present-valid field-empty, never absent", got.State)
	}
	if got.Scope.Branch != "" {
		t.Fatalf("carrierless scope carried a branch %q, want field-empty", got.Scope.Branch)
	}

	// The step inherits the unknown scope; the cwd writers must still stand down
	// (unknown ≠ absent).
	step := all[0]
	for _, b := range all {
		if b.Title == "Work" {
			step = b
		}
	}
	e2eAssertClaimStampsNothing(t, deps.Store, step)
	e2eAssertReconcileStandsDown(t, deps.Store, step)

	// Close gate: unknown scope does NOT fall back to the flat work_branch, and
	// its repoDir falls to the scope root, never the parked work_dir.
	repoDir, declaredBranch, violation := e2eCloseGateLocation(deps, step)
	if violation != "" {
		t.Fatalf("unknown scope produced a violation: %s", violation)
	}
	if declaredBranch != "" {
		t.Fatalf("unknown scope leaked a declaredBranch %q — it must not fall back to the flat key", declaredBranch)
	}
	if repoDir == e2ePoisonWorkDir {
		t.Fatal("unknown scope fell back to the parked flat work_dir")
	}
}

// §11 #16 — the consumed-carrier equality invariant (DECISION 3), re-asserted on
// the FINISHED assembly through the whole chain. Explicit gc_target_branch
// outranks the formula default; the reconciled winner is written back into the
// consumed base_branch carrier so template substitution and the close gate read
// the SAME branch. This is the poison class the entire bead exists to kill:
// scope.branch == what the work runs against == what the gate validates.
func TestE2EConsumedCarrierEqualsScopeEndToEnd(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-equality")
	deps, agent := e2eSlingDeps(t, dir)
	all := e2eLaunchScopedSling(t, deps, agent, "e2e-equality", "gc_target_branch=release")

	root := e2eFormulaRoot(t, all)
	scope := targetscope.Parse(root.Metadata[beadmeta.TargetScopeMetadataKey])
	if !scope.Valid() || scope.Scope.Branch != "release" {
		t.Fatalf("root scope = %+v, want branch=release", scope)
	}

	// The substituted step title is the observable proof the consumed carrier
	// equals the scope — substitution used the winner, not the stale default.
	step := e2eWorkStep(t, all)
	if step.Title != "Work on release" {
		t.Fatalf("step title = %q, want %q — substitution and the declared scope disagree", step.Title, "Work on release")
	}

	// And the close gate validates against that SAME release.
	_, declaredBranch, violation := e2eCloseGateLocation(deps, step)
	if violation != "" || declaredBranch != "release" {
		t.Fatalf("close gate = (%q, %q), want (release, no violation) — the gate must validate the executed branch", declaredBranch, violation)
	}
}

// e2eAttachScopedFormula attaches a scoped formula to a pre-existing source bead
// and returns the source bead after the launch. An attached launch routes and
// closes the SOURCE, so §5b declares the resolved scope on the source bead
// itself — the bead the close gate later loads directly.
func e2eAttachScopedFormula(t *testing.T, deps slingDeps, agent config.Agent, formulaName, sourceID string, vars ...string) beads.Bead {
	t.Helper()
	opts := sling.SlingOpts{
		Target:        agent,
		BeadOrFormula: sourceID,
		OnFormula:     formulaName,
		SkipPoke:      true,
		Vars:          vars,
	}
	if _, err := sling.DoSling(opts, deps, deps.Store); err != nil {
		t.Fatalf("DoSling --on %s: %v", formulaName, err)
	}
	got, err := deps.Store.Get(sourceID)
	if err != nil {
		t.Fatalf("reloading source %s: %v", sourceID, err)
	}
	return got
}

// §11 #9 — Legacy claimable source. An attached formula routes and closes the
// source bead; that bead carries the declared scope on its OWN key (not through
// inheritance), and a claim from a mismatching cwd must not overwrite it.
func TestE2ELegacyClaimableSourceCarriesScopeAndResistsCwd(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-attach")
	deps, agent := e2eSlingDeps(t, dir)
	src, err := deps.Store.Create(beads.Bead{Title: "attach source", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("creating source: %v", err)
	}

	src = e2eAttachScopedFormula(t, deps, agent, "e2e-attach", src.ID, "gc_target_branch=release")

	// The scope is declared ON the source bead, readable without inheritance.
	own := targetscope.Parse(src.Metadata[beadmeta.TargetScopeMetadataKey])
	if !own.Valid() || own.Scope.Branch != "release" {
		t.Fatalf("source own scope = %+v, want valid branch=release declared on the source", own)
	}
	e2eAssertClaimStampsNothing(t, deps.Store, src)
	e2eAssertReconcileStandsDown(t, deps.Store, src)

	_, declaredBranch, violation := e2eCloseGateLocation(deps, src)
	if violation != "" || declaredBranch != "release" {
		t.Fatalf("close gate on legacy source = (%q, %q), want (release, no violation)", declaredBranch, violation)
	}
}

// §11 #12 — THE HEADLINE. A member T whose three gc.work_* flat values are all
// poisoned, closed through the close gate. Launch the targeted formula against T
// with an explicit clean branch + worktree, claim + reconcile from a THIRD
// mismatching cwd, then close T (the ORIGINAL member). The gate's git probe must
// receive the DECLARED branch and the NORMALIZED declared worktree — not any
// poisoned flat value and not the claimant cwd. This exercises the §5b member
// stamp specifically: the gate loads T, so reading a stage's scope is not enough.
func TestE2EPoisonedMemberClosedThroughGateReadsDeclaredScope(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-poison-member")
	deps, agent := e2eSlingDeps(t, dir)

	// A member T whose THREE flat work_* values are poison before the launch.
	src, err := deps.Store.Create(beads.Bead{
		Title:  "poisoned member",
		Type:   "task",
		Status: "open",
		Metadata: beads.StringMap{
			beadmeta.WorkBranchMetadataKey: e2ePoisonBranch,
			beadmeta.WorkDirMetadataKey:    e2ePoisonWorkDir,
			beadmeta.WorkCommitMetadataKey: "deadbeef",
		},
	})
	if err != nil {
		t.Fatalf("creating poisoned member: %v", err)
	}

	src = e2eAttachScopedFormula(t, deps, agent, "e2e-poison-member", src.ID,
		"gc_target_branch=release", "gc_target_worktree=worktrees/T")

	declared := targetscope.Parse(src.Metadata[beadmeta.TargetScopeMetadataKey])
	if !declared.Valid() || declared.Scope.Branch != "release" || declared.Scope.Worktree == "" {
		t.Fatalf("member T declared scope = %+v, want valid branch=release with a worktree", declared)
	}
	if !filepath.IsAbs(declared.Scope.Worktree) {
		t.Fatalf("declared worktree %q is not absolute; the boundary must normalize before persisting", declared.Scope.Worktree)
	}

	// Claim + reconcile from a THIRD cwd (distinct from the poison and the
	// declared scope): both writers stand down.
	e2eAssertClaimStampsNothing(t, deps.Store, src)
	e2eAssertReconcileStandsDown(t, deps.Store, src)

	// Close T through the gate. Its git probe is handed the declared branch and
	// the normalized declared worktree, NOT the three poisoned flat values.
	// Note the bead here keeps its own poisoned flat keys — no re-poisoning: this
	// is the real shape the gate loads.
	scopeRoot, envelope := e2eEnvelope(deps)
	repoDir, declaredBranch, violation := workRecordCloseLocation(deps.Store, src, scopeRoot, envelope)
	if violation != "" {
		t.Fatalf("close gate on poisoned member T reported a violation: %s", violation)
	}
	if declaredBranch != "release" {
		t.Fatalf("gate probe branch = %q, want the declared release (not the poisoned %q)", declaredBranch, e2ePoisonBranch)
	}
	if repoDir != declared.Scope.Worktree {
		t.Fatalf("gate probe repoDir = %q, want the normalized declared worktree %q (not the poisoned %q)", repoDir, declared.Scope.Worktree, e2ePoisonWorkDir)
	}
	if repoDir == e2ePoisonWorkDir {
		t.Fatal("gate probe took the poisoned flat work_dir")
	}
}

// §11 #14(ii) — CROSS-STORE worktree anchor (DECISION 2). With a distinct
// GraphStore (the root) and Store (the member T) rooted at different paths, the
// single persisted ABSOLUTE worktree is authority across the split: the reader
// that loads T (close gate) and the reader that loads the root/stage (Ralph)
// must consume the SAME absolute worktree — the graph-store vs work-store anchor
// split does not produce two different paths. This asserts the cmd/gc close-gate
// half and the cross-store equality of the persisted value; ralph_target_scope_test
// covers the Ralph reader against the same ResolveForReader contract.
func TestE2ECrossStoreWorktreeIsOneAbsolutePathForBothReaders(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-xstore")
	deps, agent := e2eSlingDeps(t, dir)
	// The graph root lands in a SEPARATE store from the source member T.
	graphStore := beads.NewMemStore()
	deps.GraphStore = graphStore

	src, err := deps.Store.Create(beads.Bead{Title: "xstore member", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("creating cross-store member: %v", err)
	}
	src = e2eAttachScopedFormula(t, deps, agent, "e2e-xstore", src.ID,
		"gc_target_branch=release", "gc_target_worktree=worktrees/T")

	memberScope := targetscope.Parse(src.Metadata[beadmeta.TargetScopeMetadataKey])
	if !memberScope.Valid() || memberScope.Scope.Worktree == "" {
		t.Fatalf("member T scope = %+v, want a valid worktree", memberScope)
	}
	if !filepath.IsAbs(memberScope.Scope.Worktree) {
		t.Fatalf("member worktree %q is not absolute", memberScope.Scope.Worktree)
	}

	// The root landed in the GRAPH store; find its scope there.
	graphBeads := e2eStoreBeads(t, graphStore)
	root := e2eFormulaRoot(t, graphBeads)
	rootScope := targetscope.Parse(root.Metadata[beadmeta.TargetScopeMetadataKey])
	if !rootScope.Valid() {
		t.Fatalf("graph-store root scope state = %v, want valid", rootScope.State)
	}
	if rootScope.Scope.Worktree != memberScope.Scope.Worktree {
		t.Fatalf("cross-store worktree split: root=%q member=%q — the persisted absolute value must be identical across stores",
			rootScope.Scope.Worktree, memberScope.Scope.Worktree)
	}

	// The close gate loading T (envelope anchored at the work store) resolves the
	// one absolute worktree, not a re-anchored per-store path.
	scopeRoot, envelope := e2eEnvelope(deps)
	repoDir, _, violation := workRecordCloseLocation(deps.Store, src, scopeRoot, envelope)
	if violation != "" {
		t.Fatalf("close gate on cross-store member reported a violation: %s", violation)
	}
	if repoDir != memberScope.Scope.Worktree {
		t.Fatalf("close gate repoDir = %q, want the single absolute worktree %q", repoDir, memberScope.Scope.Worktree)
	}
}

// §11 #14(iv) — an ESCAPING worktree is present-invalid at the close gate, which
// refuses to validate and does NOT fall back to the flat keys. The scope is a
// well-formed absolute path that simply escapes both the city and store roots.
func TestE2EEscapingWorktreeFailsClosedAtTheGate(t *testing.T) {
	dir := e2eScopedFormulaDir(t, "e2e-escape")
	deps, agent := e2eSlingDeps(t, dir)
	all := e2eLaunchScopedSling(t, deps, agent, "e2e-escape", "gc_target_branch=release")
	step := e2eWorkStep(t, all)

	// Pin an escaping absolute worktree directly on the step (a well-formed scope
	// whose worktree is outside every envelope root).
	escaping, err := targetscope.Marshal(targetscope.Scope{V: 1, Branch: "release", Worktree: "/etc/outside-every-root"})
	if err != nil {
		t.Fatalf("marshal escaping scope: %v", err)
	}
	if err := deps.Store.SetMetadata(step.ID, beadmeta.TargetScopeMetadataKey, escaping); err != nil {
		t.Fatalf("pinning escaping scope: %v", err)
	}
	step, err = deps.Store.Get(step.ID)
	if err != nil {
		t.Fatalf("reloading step: %v", err)
	}

	scopeRoot, envelope := e2eEnvelope(deps)
	repoDir, declaredBranch, violation := workRecordCloseLocation(deps.Store, e2ePoisonFlatKeys(step), scopeRoot, envelope)
	if violation == "" {
		t.Fatal("an escaping worktree must be a close-gate violation, not a silent pass")
	}
	if declaredBranch != "" || repoDir != "" {
		t.Fatalf("escaping worktree fell back to a location (%q, %q); the gate must refuse, not use flat keys", repoDir, declaredBranch)
	}
}

// §11 #15 — graph-v2 order boundary. An order materializes with empty
// molecule.Options, so its scope is resolved from the formula defaults alone
// (D4) — but that scope is real and must lock the cwd writers out just like a
// sling launch: a claim of the order step from a mismatching cwd is suppressed,
// and the gate reads the order's declared branch.
func TestE2EOrderStepHonoursDeclaredScope(t *testing.T) {
	cityDir := t.TempDir()
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"
[daemon]
formula_v2 = true
[[rigs]]
name = "fixture"
path = "fixture"
[[agent]]
name = "worker"
dir = "fixture"
max_active_sessions = 2
[[agent]]
name = "control-dispatcher"
dir = "fixture"
max_active_sessions = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "ord.toml"), []byte(`
formula = "ord"
version = 2
contract = "graph.v2"

[vars.base_branch]
default = "order-branch"

[[steps]]
id = "work"
title = "Work on {{base_branch}}"
metadata = { "gc.run_target" = "worker" }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	a := orders.Order{Name: "probe", Rig: "fixture", Formula: "ord", Trigger: "manual", FormulaLayer: formulaDir}
	store := beads.NewMemStore()
	var stdout, stderr bytes.Buffer
	if code := doOrderRunWithJSON([]orders.Order{a}, a.Name, a.Rig, cityDir, beads.OrdersStore{Store: store}, nil, false, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("doOrderRunWithJSON = %d, want 0; stderr: %s", code, stderr.String())
	}

	all := e2eStoreBeads(t, store)
	step := e2eWorkStep(t, all)
	if step.Title != "Work on order-branch" {
		t.Fatalf("order step title = %q, want the substituted default", step.Title)
	}

	// Claim of the order step from the poison cwd is suppressed by the scope the
	// order boundary stamped.
	if patch := e2eClaimPatch(store, step); patch[beadmeta.WorkBranchMetadataKey] != "" {
		t.Fatalf("order step claim stamped %s=%q over the declared scope",
			beadmeta.WorkBranchMetadataKey, patch[beadmeta.WorkBranchMetadataKey])
	}
	if mayStampCwdWorkDir(store, step, nil) {
		t.Fatal("reconcile would stamp cwd work_dir over the order's declared scope")
	}

	scopeRoot := cityDir
	_, declaredBranch, violation := workRecordCloseLocation(store, e2ePoisonFlatKeys(step), scopeRoot, targetscope.Envelope{CityPath: cityDir, StorePath: cityDir})
	if violation != "" || declaredBranch != "order-branch" {
		t.Fatalf("order close gate = (%q, %q), want (order-branch, no violation)", declaredBranch, violation)
	}
}

// §11 #7 / #8 — Direct graph-v2 `gc formula cook` (which is also the no-convoy
// standalone graph launch — the shape whose convoy early-return once left the
// root unscoped). Cook stamps the scope, and the generated stages are not
// cwd-poisonable: a claim of a cooked stage from a mismatching cwd is suppressed
// and the close gate reads the cooked branch. This drives the REAL `gc formula
// cook` end to end against a real store, then chains the readers on the stage it
// materialized.
func TestE2ECookedGraphStageIsNotCwdPoisonable(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	cityDir := t.TempDir()
	formulaDir := writeCookScopeCity(t, cityDir)
	writeCookFormula(t, formulaDir, "e2e-cook", `
formula = "e2e-cook"
version = 2
contract = "graph.v2"

[vars.base_branch]
default = "FORMULA-DEFAULT"

[[steps]]
id = "work"
title = "Work on {{base_branch}}"
`)
	formulatest.SetupHermeticCookEnv(t, cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaCookCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"e2e-cook", "--var", "base_branch=release", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formula cook: %v\nstderr=%s", err, stderr.String())
	}

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	all := e2eStoreBeads(t, store)
	step := e2eWorkStep(t, all)
	if step.Title != "Work on release" {
		t.Fatalf("cooked step title = %q, want the substituted release", step.Title)
	}

	// The generated stage inherits the cooked scope; the cwd writers stand down
	// and the gate reads the cooked branch, not the parked flat keys.
	if patch := e2eClaimPatch(store, step); patch[beadmeta.WorkBranchMetadataKey] != "" {
		t.Fatalf("cooked stage claim stamped %s=%q over the cooked scope",
			beadmeta.WorkBranchMetadataKey, patch[beadmeta.WorkBranchMetadataKey])
	}
	if mayStampCwdWorkDir(store, step, nil) {
		t.Fatal("reconcile would stamp cwd work_dir over the cooked scope")
	}
	_, declaredBranch, violation := workRecordCloseLocation(store, e2ePoisonFlatKeys(step), cityDir, targetscope.Envelope{CityPath: cityDir, StorePath: cityDir})
	if violation != "" || declaredBranch != "release" {
		t.Fatalf("cooked-stage close gate = (%q, %q), want (release, no violation)", declaredBranch, violation)
	}
}

// SCENARIO MAP (DESIGN §11, 17 scenarios). The cross-cutting claim/reconcile/
// close-gate chains are the new integration proofs in THIS file; the scenarios
// whose whole contract is a single reader/boundary property are pinned by the
// unit/boundary tests cited alongside. Every scenario is covered:
//
//	#1  fresh claim ................ TestE2EFreshClaimOfScopedStageHonoursDeclaredScope
//	#2  existing/adopted claim ..... TestE2EExistingAndAdoptedAssignmentClaimsStillSuppressCwd
//	#3  repeated claim ............. TestE2ERepeatedClaimFromDifferentCwdDoesNotDrift
//	#4  reconcile + propagation .... TestE2EWorkDirReconcileAndRootPropagation
//	#5  symbolic root ref .......... TestE2ESymbolicRootRefStageInheritsScope
//	#6  attempt 2 (retry) .......... TestE2ERetryAttemptResolvesOriginalRootScope
//	#7  graph-v2 cook .............. TestE2ECookedGraphStageIsNotCwdPoisonable
//	                                 (+ formula_cook_scope_test.go, real cook boundary)
//	#8  no-convoy standalone graph . TestE2ECookedGraphStageIsNotCwdPoisonable
//	                                 (+ TestFormulaCookStandaloneGraphScopeEqualsSubstitutedBranch)
//	#9  legacy claimable source .... TestE2ELegacyClaimableSourceCarriesScopeAndResistsCwd
//	#10 poisoned member vs empty ... TestE2ECarrierlessLaunchStampsUnknownNotAbsent
//	                                 (+ TestDeclareTreatsPoisonedFlatKeysAsAbsent, TestDeclareUnknownPersistsAsValidNotAbsent)
//	#11 heterogeneous drain ........ internal/dispatch/drain_target_scope_test.go (3 tests)
//	#12 poisoned member via gate ... TestE2EPoisonedMemberClosedThroughGateReadsDeclaredScope (HEADLINE)
//	#13 poison-only / no source .... TestE2ECarrierlessLaunchStampsUnknownNotAbsent
//	                                 (+ TestNoTrustedSourceYieldsValidUnknown, TestCloseGateUnknownScopeDoesNotFallBackToFlatKeys, TestRalphWorkDirUnknownScopeDoesNotFallBackToFlatKey)
//	#14 worktree / cross-store:
//	   (i)   absolute persisted .... TestE2EWorkDirReconcileAndRootPropagation (+ TestWorktreeNormalizedAtBoundary)
//	   (ii)  cross-store ........... TestE2ECrossStoreWorktreeIsOneAbsolutePathForBothReaders
//	   (iii) persisted-relative .... TestParseRelativeWorktreeIsInvalidNotReanchored (+ TestCloseGateRefusesFlatFallbackWhenScopeIsCorrupt, TestRalphWorkDirInvalidScopeIsAViolationNotAFallback)
//	   (iv)  escaping .............. TestE2EEscapingWorktreeFailsClosedAtTheGate
//	                                 (+ TestCloseGateRefusesFlatFallbackWhenScopeEscapesEnvelope, TestRalphWorkDirEscapingWorktreeFailsClosed)
//	#15 order + legacy cook attach . TestE2EOrderStepHonoursDeclaredScope
//	                                 (+ TestFormulaCookLegacyInlineStampsScope, TestFormulaCookAttachGraphV2DeclaresTargetMemberScope)
//	#16 alias / consumed equality .. TestE2EConsumedCarrierEqualsScopeEndToEnd
//	                                 (+ resolver_test.go, sling_targetscope_test.go)
//	#17 declaration race ........... internal/targetscope/declare_test.go
//	                                 (TestDeclareRaceHasExactlyOneWinner, TestDeclareRaceWithEqualScopesConverges, TestDeclareRejectsStoreWithoutCAS)
