package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// claimedAtNoWorktreeOps is a minimal hookClaimOps for exercising
// hookClaimIdentityPatch directly: no worktree, no session-identity plumbing
// beyond what the patch function itself reads.
func claimedAtNoWorktreeOps() hookClaimOps {
	return hookClaimOps{ResolveWorkBranch: func(string) string { return "" }}
}

// requireRecentRFC3339 fails the test unless raw parses as RFC3339 and lands
// within the last 10 seconds — proving the patch stamped "now", not a
// hardcoded or stale value.
func requireRecentRFC3339(t *testing.T, raw string) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("gc.claimed_at = %q is not RFC3339: %v", raw, err)
	}
	if age := time.Since(ts); age < 0 || age > 10*time.Second {
		t.Fatalf("gc.claimed_at = %q is not a recent timestamp (age %s)", raw, age)
	}
}

// TestHookClaimIdentityPatchStampsClaimedAtWhenAbsent covers a bead's first
// claim: gc.claimed_at is absent from its metadata, so the patch must set it
// to a fresh, valid RFC3339 UTC timestamp.
func TestHookClaimIdentityPatchStampsClaimedAtWhenAbsent(t *testing.T) {
	bead := beads.Bead{ID: "hw-first", Status: "open", Metadata: map[string]string{
		beadmeta.KindMetadataKey: "worker",
	}}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1", "GC_SESSION_NAME=gc__role-mc-sess1"}}

	patch := hookClaimIdentityPatch(bead, opts, claimedAtNoWorktreeOps(), "/tmp/work")

	raw, ok := patch[beadmeta.ClaimedAtMetadataKey]
	if !ok {
		t.Fatalf("patch = %v, want gc.claimed_at present on a bead with no prior claim", patch)
	}
	requireRecentRFC3339(t, raw)
}

// TestHookClaimIdentityPatchClaimedAtWriteOnce is the flood-class regression
// test: a bead that already carries gc.claimed_at (simulating the metadata
// state after the FIRST call's patch landed) must NOT get the key re-issued
// on a second call, even though time.Now() always differs from the stored
// value. This is the specific trap OBS-001 calls out — a naive
// claimed_at = now() would defeat hookClaimIdentityPatch's compare-and-skip
// by construction and flood bead.updated on every hook tick for every
// in-progress bead. Absence from the returned map (not equality of value) is
// the thing under test, since equality would trivially pass even for a
// unconditional re-stamp that happened to land in the same second.
func TestHookClaimIdentityPatchClaimedAtWriteOnce(t *testing.T) {
	bead := beads.Bead{ID: "hw-second", Status: "open", Metadata: map[string]string{
		beadmeta.KindMetadataKey: "worker",
	}}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1", "GC_SESSION_NAME=gc__role-mc-sess1"}}
	ops := claimedAtNoWorktreeOps()

	first := hookClaimIdentityPatch(bead, opts, ops, "/tmp/work")
	stamped, ok := first[beadmeta.ClaimedAtMetadataKey]
	if !ok {
		t.Fatalf("first call patch = %v, want gc.claimed_at present", first)
	}

	// Simulate the store now holding the value the first call decided.
	bead.Metadata[beadmeta.ClaimedAtMetadataKey] = stamped

	second := hookClaimIdentityPatch(bead, opts, ops, "/tmp/work")
	if _, ok := second[beadmeta.ClaimedAtMetadataKey]; ok {
		t.Fatalf("second call patch = %v, want gc.claimed_at ABSENT (write-once; no re-stamp once set)", second)
	}
}

// TestHookClaimIdentityPatchClaimedAtForControlBead pins the deliberate
// design choice that gc.claimed_at is unconditional: unlike
// gc.session_id/gc.session_name (skipped for control-kind beads because
// control steps stay session-free by graphroute's design), a claim timestamp
// is meaningful for every claimed bead regardless of kind — the bead's own
// description frames it purely as "when was this claimed", with no
// control-bead carve-out. A control bead with no prior gc.claimed_at must
// still receive one.
func TestHookClaimIdentityPatchClaimedAtForControlBead(t *testing.T) {
	bead := beads.Bead{ID: "hc-check", Status: "open", Metadata: map[string]string{
		beadmeta.KindMetadataKey: "check",
	}}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1", "GC_SESSION_NAME=gc__role-mc-sess1"}}

	patch := hookClaimIdentityPatch(bead, opts, claimedAtNoWorktreeOps(), "/tmp/work")

	raw, ok := patch[beadmeta.ClaimedAtMetadataKey]
	if !ok {
		t.Fatalf("patch = %v, want gc.claimed_at present on a control bead's first claim", patch)
	}
	requireRecentRFC3339(t, raw)
	if _, ok := patch[beadmeta.SessionIDMetadataKey]; ok {
		t.Fatalf("patch = %v, want gc.session_id absent on a control bead (unaffected by the claimed_at change)", patch)
	}
}

// TestHookClaimIdentityPatchClaimedAtWithoutSessionOrWorktree proves
// gc.claimed_at is not gated on session identity or a resolvable worktree
// branch: a session-less, worktree-less claim is still a genuine claim and
// must still get a claim timestamp.
func TestHookClaimIdentityPatchClaimedAtWithoutSessionOrWorktree(t *testing.T) {
	bead := beads.Bead{ID: "hw-bare", Status: "open", Metadata: map[string]string{
		beadmeta.KindMetadataKey: "worker",
	}}
	opts := hookClaimOptions{} // no GC_SESSION_ID, no GC_SESSION_NAME

	patch := hookClaimIdentityPatch(bead, opts, claimedAtNoWorktreeOps(), "/tmp/work")

	if len(patch) != 1 {
		t.Fatalf("patch = %v, want exactly gc.claimed_at (no session, no branch)", patch)
	}
	raw, ok := patch[beadmeta.ClaimedAtMetadataKey]
	if !ok {
		t.Fatalf("patch = %v, want gc.claimed_at present", patch)
	}
	requireRecentRFC3339(t, raw)
}

// TestHookClaimIdentityPatchClaimedAtDoesNotDisturbExistingKeys guards the
// three pre-existing compare-and-skip keys against interference from the new
// write-once key: a bead needing a branch update and carrying a stale session
// name, but already carrying gc.claimed_at, must patch exactly the stale keys
// and nothing else.
func TestHookClaimIdentityPatchClaimedAtDoesNotDisturbExistingKeys(t *testing.T) {
	bead := beads.Bead{ID: "hw-mixed", Status: "open", Metadata: map[string]string{
		beadmeta.KindMetadataKey:        "worker",
		beadmeta.WorkBranchMetadataKey:  "bd-old",
		beadmeta.SessionIDMetadataKey:   "mc-sess1",
		beadmeta.SessionNameMetadataKey: "gc__role-mc-sess1",
		beadmeta.ClaimedAtMetadataKey:   "2026-01-01T00:00:00Z",
	}}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1", "GC_SESSION_NAME=gc__role-mc-sess1"}}
	ops := hookClaimOps{ResolveWorkBranch: func(string) string { return "bd-new" }}

	patch := hookClaimIdentityPatch(bead, opts, ops, "/tmp/work")

	want := map[string]string{beadmeta.WorkBranchMetadataKey: "bd-new"}
	if len(patch) != len(want) || patch[beadmeta.WorkBranchMetadataKey] != want[beadmeta.WorkBranchMetadataKey] {
		t.Fatalf("patch = %v, want %v (claimed_at already set must stay out of the patch)", patch, want)
	}
	if _, ok := patch[beadmeta.ClaimedAtMetadataKey]; ok {
		t.Fatalf("patch = %v, want gc.claimed_at absent (already current)", patch)
	}
}
