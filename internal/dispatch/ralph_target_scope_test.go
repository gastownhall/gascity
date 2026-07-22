package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/convergence"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// The envelope Ralph runs under in these tests. A declared worktree must sit
// under one of these roots or the scoped read fails closed.
func testEnvelope() targetscope.Envelope {
	return targetscope.Envelope{CityPath: "/city", StorePath: "/city/rig"}
}

func scopedBead(t *testing.T, store *beads.MemStore, meta map[string]string) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{Title: "ralph check subject", Metadata: meta})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return bead
}

func declaredMeta(t *testing.T, scope targetscope.Scope, extra map[string]string) map[string]string {
	t.Helper()
	blob, err := targetscope.Marshal(scope)
	if err != nil {
		t.Fatalf("marshalling scope: %v", err)
	}
	meta := map[string]string{beadmeta.TargetScopeMetadataKey: blob}
	for k, v := range extra {
		meta[k] = v
	}
	return meta
}

// The headline: a declared worktree wins over the claim-stamped gc.work_dir.
// gc.work_dir is written from whatever cwd the claiming session held, so a
// check that trusts it runs where the worker stood rather than where the work
// was targeted.
func TestRalphWorkDirPrefersDeclaredScopeOverClaimStampedWorkDir(t *testing.T) {
	store := beads.NewMemStore()
	bead := scopedBead(t, store, declaredMeta(t,
		targetscope.Scope{V: 1, Branch: "release", Worktree: "/city/rig/declared"},
		map[string]string{beadmeta.WorkDirMetadataKey: "/city/rig/claimant-cwd"},
	))

	workDir, violation := ralphWorkDir(store, bead, testEnvelope())
	if violation != "" {
		t.Fatalf("violation = %q, want none", violation)
	}
	if workDir != "/city/rig/declared" {
		t.Fatalf("workDir = %q, want the declared worktree", workDir)
	}
}

// §2c field-empty: "unknown" is an answer, not an absence. It must fall to the
// store root (empty here — the caller's no-work_dir base), never reach for the
// flat key it was declared to override.
func TestRalphWorkDirUnknownScopeDoesNotFallBackToFlatKey(t *testing.T) {
	store := beads.NewMemStore()
	bead := scopedBead(t, store, declaredMeta(t,
		targetscope.Unknown(),
		map[string]string{beadmeta.WorkDirMetadataKey: "/city/rig/claimant-cwd"},
	))

	workDir, violation := ralphWorkDir(store, bead, testEnvelope())
	if violation != "" {
		t.Fatalf("violation = %q, want none", violation)
	}
	if workDir != "" {
		t.Fatalf("workDir = %q, want empty so the caller uses the store root", workDir)
	}
}

// A corrupt object must not degrade into the cwd value it exists to override.
func TestRalphWorkDirInvalidScopeIsAViolationNotAFallback(t *testing.T) {
	store := beads.NewMemStore()
	bead := scopedBead(t, store, map[string]string{
		beadmeta.TargetScopeMetadataKey: "{not json",
		beadmeta.WorkDirMetadataKey:     "/city/rig/claimant-cwd",
	})

	workDir, violation := ralphWorkDir(store, bead, testEnvelope())
	if violation == "" {
		t.Fatal("an unusable scope produced no violation, want a refusal")
	}
	if workDir != "" {
		t.Fatalf("workDir = %q, want empty on a refused scoped read", workDir)
	}
	if !strings.Contains(violation, beadmeta.TargetScopeMetadataKey) {
		t.Fatalf("violation %q does not name the offending key", violation)
	}
}

// §14(iv): an escaping worktree is present-invalid for the reader. Ralph must
// fail the scoped read rather than re-anchor it or fall through to flat.
func TestRalphWorkDirEscapingWorktreeFailsClosed(t *testing.T) {
	store := beads.NewMemStore()
	bead := scopedBead(t, store, declaredMeta(t,
		targetscope.Scope{V: 1, Worktree: "/etc/elsewhere"},
		map[string]string{beadmeta.WorkDirMetadataKey: "/city/rig/claimant-cwd"},
	))

	workDir, violation := ralphWorkDir(store, bead, testEnvelope())
	if violation == "" {
		t.Fatal("a worktree outside the envelope produced no violation, want a refusal")
	}
	if workDir != "" {
		t.Fatalf("workDir = %q, want empty on a refused scoped read", workDir)
	}
}

// Absent is every bead in the existing population: the legacy inherited flat
// read must be untouched, or this change regresses every ralph check that
// ships today.
func TestRalphWorkDirAbsentScopeKeepsTheLegacyFlatRead(t *testing.T) {
	store := beads.NewMemStore()
	bead := scopedBead(t, store, map[string]string{
		beadmeta.WorkDirMetadataKey: "/city/rig/legacy",
	})

	workDir, violation := ralphWorkDir(store, bead, testEnvelope())
	if violation != "" {
		t.Fatalf("violation = %q, want none", violation)
	}
	if workDir != "/city/rig/legacy" {
		t.Fatalf("workDir = %q, want the legacy flat value", workDir)
	}
}

// The legacy key is still honoured on the absent path.
func TestRalphWorkDirAbsentScopeHonoursTheLegacyKey(t *testing.T) {
	store := beads.NewMemStore()
	bead := scopedBead(t, store, map[string]string{
		beadmeta.LegacyWorkDirMetadataKey: "/city/rig/very-legacy",
	})

	workDir, violation := ralphWorkDir(store, bead, testEnvelope())
	if violation != "" {
		t.Fatalf("violation = %q, want none", violation)
	}
	if workDir != "/city/rig/very-legacy" {
		t.Fatalf("workDir = %q, want the legacy flat value", workDir)
	}
}

// The symbolic-root-reference class the design exists for: a stage carrying
// only gc.root_bead_id must reach the root's declared scope, because that is
// the shape a formula stage actually has.
func TestRalphWorkDirInheritsScopeThroughTheRootChain(t *testing.T) {
	store := beads.NewMemStore()
	root := scopedBead(t, store, declaredMeta(t,
		targetscope.Scope{V: 1, Branch: "release", Worktree: "/city/rig/declared"},
		nil,
	))
	stage := scopedBead(t, store, map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.WorkDirMetadataKey:    "/city/rig/claimant-cwd",
	})

	workDir, violation := ralphWorkDir(store, stage, testEnvelope())
	if violation != "" {
		t.Fatalf("violation = %q, want none", violation)
	}
	if workDir != "/city/rig/declared" {
		t.Fatalf("workDir = %q, want the root's declared worktree inherited by the stage", workDir)
	}
}

// The mayor's standing condition for this build: a seam wired into production
// gets a test that exercises the PRODUCTION path, not only the helper with
// hand-built collaborators. Session 1 shipped a guard that passed seven
// hand-injected tests and did nothing when constructed for real.
//
// runRalphCheck is the production caller. The check script exists ONLY in the
// declared worktree, so the scoped read is what makes this check runnable at
// all: honour gc.work_dir instead and the script is not there to run.
func TestRunRalphCheckRunsInTheDeclaredWorktreeNotTheClaimStampedWorkDir(t *testing.T) {
	cityPath := t.TempDir()

	declaredWorktree := filepath.Join(cityPath, "worktrees", "declared")
	if err := os.MkdirAll(declaredWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(declaredWorktree, "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The cwd a claiming session happened to hold. It carries no check script.
	claimantCwd := filepath.Join(cityPath, "worktrees", "claimant-cwd")
	if err := os.MkdirAll(claimantCwd, 0o755); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	control := mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: declaredMeta(t,
			targetscope.Scope{V: 1, Branch: "release", Worktree: declaredWorktree},
			map[string]string{
				"gc.kind":         "ralph",
				"gc.root_bead_id": root.ID,
				"gc.check_path":   "check.sh",
				"gc.work_dir":     claimantCwd,
				"gc.max_attempts": "3",
			},
		),
	})
	subject := mustCreate(t, store, beads.Bead{
		Title:    "review loop iteration 1",
		Metadata: map[string]string{"gc.kind": "scope", "gc.root_bead_id": root.ID},
	})

	result, err := runRalphCheck(store, control, subject, 1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("runRalphCheck: %v (the declared worktree holds the check script; gc.work_dir does not)", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("Outcome = %q (stderr=%q), want pass from the declared worktree", result.Outcome, result.Stderr)
	}
}

// An unusable scope must abort the production check rather than quietly run it
// against the claim-time cwd.
func TestRunRalphCheckRefusesAnUnusableScope(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	control := mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":                       "ralph",
			"gc.root_bead_id":               root.ID,
			"gc.check_path":                 "check.sh",
			"gc.work_dir":                   cityPath,
			"gc.max_attempts":               "3",
			beadmeta.TargetScopeMetadataKey: "{not json",
		},
	})
	subject := mustCreate(t, store, beads.Bead{
		Title:    "review loop iteration 1",
		Metadata: map[string]string{"gc.kind": "scope", "gc.root_bead_id": root.ID},
	})

	if _, err := runRalphCheck(store, control, subject, 1, ProcessOptions{CityPath: cityPath}); err == nil {
		t.Fatal("runRalphCheck accepted an unusable target scope, want a refusal")
	}
}
