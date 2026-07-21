package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// claimOpsWithScope builds the minimal ops needed to exercise the identity
// patch: a cwd that always resolves to a "parked" branch (the poison shape),
// plus a scope resolver returning the given resolution.
func claimOpsWithScope(res targetscope.Resolution, resolverSet bool) hookClaimOps {
	ops := hookClaimOps{
		ResolveWorkBranch: func(string) string { return "parked-shared-branch" },
	}
	if resolverSet {
		ops.ResolveTargetScope = func(string, []string, beads.Bead) targetscope.Resolution { return res }
	}
	return ops
}

// The legacy path is preserved exactly: with no scope reachable, the claim hook
// still stamps the claiming worktree's branch.
func TestClaimStampsCwdBranchWhenScopeAbsent(t *testing.T) {
	bead := beads.Bead{ID: "bd-1", Metadata: beads.StringMap{}}
	patch := hookClaimIdentityPatch(bead, hookClaimOptions{}, claimOpsWithScope(targetscope.Resolution{State: targetscope.StateAbsent}, true), "/some/dir")

	if patch[beadmeta.WorkBranchMetadataKey] != "parked-shared-branch" {
		t.Fatalf("work_branch = %q, want the cwd branch (absent scope keeps legacy behavior)",
			patch[beadmeta.WorkBranchMetadataKey])
	}
}

// A nil resolver (no store available) must also preserve legacy behavior, so
// the guard can never make a claim path worse than it was.
func TestClaimStampsCwdBranchWhenNoResolverConfigured(t *testing.T) {
	bead := beads.Bead{ID: "bd-1", Metadata: beads.StringMap{}}
	patch := hookClaimIdentityPatch(bead, hookClaimOptions{}, claimOpsWithScope(targetscope.Resolution{}, false), "/some/dir")

	if patch[beadmeta.WorkBranchMetadataKey] != "parked-shared-branch" {
		t.Fatalf("work_branch = %q, want the cwd branch", patch[beadmeta.WorkBranchMetadataKey])
	}
}

// THE DEFECT, INVERTED. A bead governed by a declared scope must not have its
// branch overwritten from wherever the claimant happens to be standing.
func TestClaimDoesNotStampCwdBranchWhenScopeValid(t *testing.T) {
	bead := beads.Bead{
		ID: "bd-1",
		Metadata: beads.StringMap{
			beadmeta.WorkBranchMetadataKey: "release",
		},
	}
	res := targetscope.Resolution{State: targetscope.StateValid, Scope: targetscope.Scope{V: 1, Branch: "release"}}
	patch := hookClaimIdentityPatch(bead, hookClaimOptions{}, claimOpsWithScope(res, true), "/some/dir")

	if _, ok := patch[beadmeta.WorkBranchMetadataKey]; ok {
		t.Fatalf("claim wrote %s=%q over a declared scope; the cwd must never win",
			beadmeta.WorkBranchMetadataKey, patch[beadmeta.WorkBranchMetadataKey])
	}
}

// WHOLE-SCOPE SEMANTICS. A scope whose branch field is unknown still suppresses
// the cwd stamp — an absent FIELD means unknown, not "fill it from cwd".
func TestClaimDoesNotStampCwdBranchWhenScopeFieldEmpty(t *testing.T) {
	bead := beads.Bead{ID: "bd-1", Metadata: beads.StringMap{}}
	res := targetscope.Resolution{State: targetscope.StateValid, Scope: targetscope.Unknown()}
	patch := hookClaimIdentityPatch(bead, hookClaimOptions{}, claimOpsWithScope(res, true), "/some/dir")

	if _, ok := patch[beadmeta.WorkBranchMetadataKey]; ok {
		t.Fatalf("a field-empty scope must still suppress the cwd stamp, got %s=%q",
			beadmeta.WorkBranchMetadataKey, patch[beadmeta.WorkBranchMetadataKey])
	}
}

// An unusable object is NOT permission to stamp cwd. Treating present-invalid
// as absent is precisely how the defect would reopen.
func TestClaimDoesNotStampCwdBranchWhenScopeInvalid(t *testing.T) {
	bead := beads.Bead{ID: "bd-1", Metadata: beads.StringMap{}}
	res := targetscope.Parse(`{"v":99}`)
	if res.State != targetscope.StateInvalid {
		t.Fatalf("fixture is not invalid: %v", res.State)
	}
	patch := hookClaimIdentityPatch(bead, hookClaimOptions{}, claimOpsWithScope(res, true), "/some/dir")

	if _, ok := patch[beadmeta.WorkBranchMetadataKey]; ok {
		t.Fatalf("an unusable scope must not fall through to a cwd stamp, got %s=%q",
			beadmeta.WorkBranchMetadataKey, patch[beadmeta.WorkBranchMetadataKey])
	}
}

// The session back-reference is orthogonal to scope and must still be stamped:
// suppressing the branch must not suppress session identity.
func TestClaimStillStampsSessionIdentityUnderValidScope(t *testing.T) {
	bead := beads.Bead{ID: "bd-1", Metadata: beads.StringMap{}}
	res := targetscope.Resolution{State: targetscope.StateValid, Scope: targetscope.Scope{V: 1, Branch: "release"}}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=sess-1", "GC_SESSION_NAME=worker-1"}}

	patch := hookClaimIdentityPatch(bead, opts, claimOpsWithScope(res, true), "/some/dir")

	if patch[beadmeta.SessionIDMetadataKey] != "sess-1" {
		t.Fatalf("session id = %q, want sess-1 — scope must not suppress session identity",
			patch[beadmeta.SessionIDMetadataKey])
	}
	if _, ok := patch[beadmeta.WorkBranchMetadataKey]; ok {
		t.Fatal("branch must still be suppressed")
	}
}

// The resolver must be handed the bead being claimed, since the inherited walk
// starts there.
func TestClaimResolverReceivesClaimedBead(t *testing.T) {
	bead := beads.Bead{ID: "bd-target", Metadata: beads.StringMap{}}
	var sawID string
	ops := hookClaimOps{
		ResolveWorkBranch: func(string) string { return "parked" },
		ResolveTargetScope: func(_ string, _ []string, b beads.Bead) targetscope.Resolution {
			sawID = b.ID
			return targetscope.Resolution{State: targetscope.StateAbsent}
		},
	}
	hookClaimIdentityPatch(bead, hookClaimOptions{}, ops, "/some/dir")

	if sawID != "bd-target" {
		t.Fatalf("resolver saw bead %q, want bd-target", sawID)
	}
}

// Guards the doc claim that the key name is the one the beadmeta package owns.
func TestTargetScopeKeyName(t *testing.T) {
	if beadmeta.TargetScopeMetadataKey != "gc.target_scope" {
		t.Fatalf("key = %q, want gc.target_scope", beadmeta.TargetScopeMetadataKey)
	}
	if !strings.HasPrefix(beadmeta.TargetScopeMetadataKey, "gc.") {
		t.Fatal("target scope key must live in the gc. namespace")
	}
}

// THE WIRING ASSERTION. Every test above injects a resolver by hand, so they
// all pass even if production never installs one — and production did not:
// claimHookWork builds a bare hookClaimOps{}, so a nil ResolveTargetScope made
// hookClaimMayStampCwdBranch permit the cwd stamp unconditionally and the whole
// claim-side guard was dead code in the only configuration that ships.
//
// The guard is only real if applyDefaults installs the store-backed resolver,
// exactly as it installs every other production op.
func TestApplyDefaultsInstallsTargetScopeResolver(t *testing.T) {
	ops := hookClaimOps{}
	ops.applyDefaults()

	if ops.ResolveTargetScope == nil {
		t.Fatal("applyDefaults left ResolveTargetScope nil; the claim-time cwd guard is inert in production")
	}
}

// applyDefaults must not clobber an injected resolver — tests and callers that
// supply one keep it.
func TestApplyDefaultsKeepsInjectedTargetScopeResolver(t *testing.T) {
	called := false
	ops := hookClaimOps{
		ResolveTargetScope: func(string, []string, beads.Bead) targetscope.Resolution {
			called = true
			return targetscope.Resolution{State: targetscope.StateAbsent}
		},
	}
	ops.applyDefaults()
	ops.ResolveTargetScope("/dir", nil, beads.Bead{})

	if !called {
		t.Fatal("applyDefaults replaced an injected ResolveTargetScope")
	}
}
