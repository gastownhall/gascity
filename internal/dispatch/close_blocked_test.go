package dispatch

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// blockedCloseUpdateStore models the beads >= v63 (bump #5210) close semantics:
// a non-force `bd update --status closed` on a bead that is still blocked by an
// open dependency is REJECTED (storage.ErrCloseBlocked) instead of silently
// leaving the bead open, while the dedicated force close path (store.Close,
// which shells out to `bd close --force`) still succeeds. Older beads did not
// gate the update-close path, so the dispatcher's convergence closes — which
// legitimately close a scope body or control bead still blocked by a peer
// control bead being closed in the same pass — used to succeed via the update
// path alone.
type blockedCloseUpdateStore struct {
	*beads.MemStore
	blockClose bool
}

// Update rejects a status→closed transition while blockClose is set, mirroring
// the real bd subprocess error surfaced through BdStore.Update as a generic
// wrapped error (only ErrNotFound is extracted as a sentinel, so the dispatcher
// cannot classify this one). A metadata-only update is unaffected.
func (s *blockedCloseUpdateStore) Update(id string, opts beads.UpdateOpts) error {
	if s.blockClose && opts.Status != nil && *opts.Status == "closed" {
		return fmt.Errorf("updating bead %q: cannot close blocked issue: %s is blocked by [ctrl] (use --force to override)", id, id)
	}
	return s.MemStore.Update(id, opts)
}

// TestUpdateMetadataAndCloseForcesThroughBlockedClose is the regression guard
// for the beads-v63 bump: convergence must still write the outcome metadata and
// close the bead by falling back to the force close path when the non-force
// update-close is rejected. Without the fix, updateMetadataAndClose returns the
// blocked-close error and the bead stays open, deadlocking scope-check
// convergence (TestGraphWorkflowSuccessPath's downstream close-timeout).
func TestUpdateMetadataAndCloseForcesThroughBlockedClose(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	body := mustCreate(t, mem, beads.Bead{Title: "scope-body", Status: "open"})
	store := &blockedCloseUpdateStore{MemStore: mem, blockClose: true}

	if err := updateMetadataAndClose(store, body.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}); err != nil {
		t.Fatalf("updateMetadataAndClose: %v", err)
	}

	got, err := store.Get(body.ID)
	if err != nil {
		t.Fatalf("get %s: %v", body.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed (must force-close through beads-v63 blocked-close enforcement)", got.Status)
	}
	if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("outcome metadata = %q, want %q", got.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomePass)
	}
}

// TestUpdateMetadataAndCloseCommonPathUnblocked locks in that the ordinary
// (unblocked) close still succeeds — the fix must not regress the path every
// existing convergence close takes.
func TestUpdateMetadataAndCloseCommonPathUnblocked(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	body := mustCreate(t, mem, beads.Bead{Title: "scope-body", Status: "open"})
	store := &blockedCloseUpdateStore{MemStore: mem, blockClose: false}

	if err := updateMetadataAndClose(store, body.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}); err != nil {
		t.Fatalf("updateMetadataAndClose: %v", err)
	}

	got, err := store.Get(body.ID)
	if err != nil {
		t.Fatalf("get %s: %v", body.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("outcome metadata = %q, want %q", got.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomePass)
	}
}
