package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// TestFencedUpdateMetadataAndClosePreservesTerminalOutcome exercises the fenced
// close primitive directly: an already-terminal bead carrying a recorded outcome
// is left intact and the caller is told it was preserved.
func TestFencedUpdateMetadataAndClosePreservesTerminalOutcome(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{Title: "root"})
	closed := "closed"
	if err := store.Update(root.ID, beads.UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeCanceled},
	}); err != nil {
		t.Fatalf("close root as canceled: %v", err)
	}

	preserved, err := fencedUpdateMetadataAndClose(store, root.ID,
		map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if err != nil {
		t.Fatalf("fencedUpdateMetadataAndClose: %v", err)
	}
	if !preserved {
		t.Fatal("preserved = false, want true (an existing terminal outcome must survive)")
	}
	if got := mustGet(t, store, root.ID).Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomeCanceled {
		t.Fatalf("outcome = %q, want canceled", got)
	}
}

// TestFencedUpdateMetadataAndCloseHappyPath: with no concurrent close, an open
// bead is closed and stamped with the requested outcome, and preserved is false.
func TestFencedUpdateMetadataAndCloseHappyPath(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{
		Title:    "root",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})

	preserved, err := fencedUpdateMetadataAndClose(store, root.ID,
		map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if err != nil {
		t.Fatalf("fencedUpdateMetadataAndClose: %v", err)
	}
	if preserved {
		t.Fatal("preserved = true, want false (an open bead must be closed)")
	}
	got := mustGet(t, store, root.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("root = {status:%q outcome:%q}, want {closed pass}", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

// TestFencedUpdateMetadataAndCloseFallbackHappyPath drives the
// ErrConditionalWriteUnsupported fallback (a store that cannot fence): an open
// bead is still closed and stamped, via the narrowed unconditional path.
func TestFencedUpdateMetadataAndCloseFallbackHappyPath(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	store.DisableConditionalWrites = true // force the unsupported→fallback branch
	root := mustCreate(t, store, beads.Bead{
		Title:    "root",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})

	preserved, err := fencedUpdateMetadataAndClose(store, root.ID,
		map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if err != nil {
		t.Fatalf("fencedUpdateMetadataAndClose: %v", err)
	}
	if preserved {
		t.Fatal("preserved = true, want false")
	}
	got := mustGet(t, store, root.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("root = {status:%q outcome:%q}, want {closed pass}", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

// TestFencedUpdateMetadataAndCloseFallbackPreservesTerminalOutcome: even without
// conditional-write support, the pre-write terminal re-read must still refuse to
// overwrite an already-recorded FOREIGN outcome.
func TestFencedUpdateMetadataAndCloseFallbackPreservesTerminalOutcome(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	store.DisableConditionalWrites = true
	root := mustCreate(t, store, beads.Bead{Title: "root"})
	closed := "closed"
	if err := store.Update(root.ID, beads.UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeCanceled},
	}); err != nil {
		t.Fatalf("close root as canceled: %v", err)
	}

	preserved, err := fencedUpdateMetadataAndClose(store, root.ID,
		map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if err != nil {
		t.Fatalf("fencedUpdateMetadataAndClose: %v", err)
	}
	if !preserved {
		t.Fatal("preserved = false, want true (fallback must honor an existing terminal outcome)")
	}
	if got := mustGet(t, store, root.ID).Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomeCanceled {
		t.Fatalf("outcome = %q, want canceled", got)
	}
}

// TestFencedUpdateMetadataAndCloseIdempotentRetryNotPreserved guards the
// crash-retry idempotency contract: a bead already closed with the SAME outcome
// this pass requests is our own prior write, not a competing one. It must report
// preserved=false so the caller still runs its post-close cleanup — otherwise a
// dispatcher crash between the root close and the source-chain close would
// permanently orphan the parent source beads.
func TestFencedUpdateMetadataAndCloseIdempotentRetryNotPreserved(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{Title: "root"})
	closed := "closed"
	if err := store.Update(root.ID, beads.UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass},
	}); err != nil {
		t.Fatalf("pre-close root as pass: %v", err)
	}

	preserved, err := fencedUpdateMetadataAndClose(store, root.ID,
		map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if err != nil {
		t.Fatalf("fencedUpdateMetadataAndClose: %v", err)
	}
	if preserved {
		t.Fatal("preserved = true, want false (re-recording our own outcome is idempotent, not a foreign preserve)")
	}
}

// TestFencedUpdateMetadataAndCloseNoOutcomeCallerStillWrites guards the helper
// against a caller that closes with metadata carrying no gc.outcome: the
// preserve/idempotent decision is keyed on gc.outcome, so with none requested it
// must apply the write, not mistake an unrelated recorded outcome for a foreign
// preserve and silently skip it.
func TestFencedUpdateMetadataAndCloseNoOutcomeCallerStillWrites(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{Title: "root"})
	closed := "closed"
	if err := store.Update(root.ID, beads.UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass},
	}); err != nil {
		t.Fatalf("pre-close root as pass: %v", err)
	}

	// metadata with no gc.outcome key — requested == "".
	preserved, err := fencedUpdateMetadataAndClose(store, root.ID,
		map[string]string{"gc.some_other_key": "v"})
	if err != nil {
		t.Fatalf("fencedUpdateMetadataAndClose: %v", err)
	}
	if preserved {
		t.Fatal("preserved = true, want false (no gc.outcome requested; the write must not be skipped)")
	}
	got := mustGet(t, store, root.ID)
	if got.Metadata["gc.some_other_key"] != "v" {
		t.Fatalf("gc.some_other_key = %q, want \"v\" (the caller's write was silently skipped)", got.Metadata["gc.some_other_key"])
	}
	if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("gc.outcome = %q, want pass (existing outcome must be untouched)", got.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

// midFlightUnsupportedStore embeds a conditional-writes-capable MemStore but its
// UpdateIfMatch always latches ErrConditionalWriteUnsupported — simulating a
// store that passes the resolve-time capability probe, then loses the capability
// mid-write (a bd probe-miss). MemStore is not a ConditionalWritesResolveTargeter,
// so ResolveConditionalWriter resolves to this wrapper and exercises its
// UpdateIfMatch rather than the embedded store's.
type midFlightUnsupportedStore struct {
	*beads.MemStore
}

func (s *midFlightUnsupportedStore) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return beads.ErrConditionalWriteUnsupported
}

// TestFencedUpdateMetadataAndCloseMidFlightUnsupportedPropagates: a store that
// resolves to a live conditional writer but then latches
// ErrConditionalWriteUnsupported mid-write must PROPAGATE the error, not fall
// back to an unconditional close. Falling back would silently violate require
// mode; propagating (a transient class) keeps the finalizer open so the next
// resolve re-applies policy (auto degrades, require refuses fail-closed).
func TestFencedUpdateMetadataAndCloseMidFlightUnsupportedPropagates(t *testing.T) {
	t.Parallel()
	store := &midFlightUnsupportedStore{MemStore: newStampedDrainStore(t, gate.Require)}
	root := mustCreate(t, store, beads.Bead{Title: "root"})

	preserved, err := fencedUpdateMetadataAndClose(store, root.ID,
		map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if err == nil {
		t.Fatal("err = nil, want a propagated ErrConditionalWriteUnsupported (must not fall back to an unconditional close under require)")
	}
	if !beads.IsConditionalWriteUnsupported(err) {
		t.Fatalf("err = %v, want IsConditionalWriteUnsupported", err)
	}
	if preserved {
		t.Fatal("preserved = true, want false")
	}
	if got := mustGet(t, store, root.ID); got.Status == "closed" {
		t.Fatal("root was closed via an unconditional fallback; require mode must stay open (fail closed)")
	}
}

// TestFencedUpdateMetadataAndCloseCASFencePreservesForeignOutcome exercises the
// real conditional-write (CAS) fence path via a factory-stamped store, rather
// than the off-mode fallback the bare MemStore takes: a foreign terminal outcome
// is preserved, and the ordinary open→pass close still works.
func TestFencedUpdateMetadataAndCloseCASFencePreservesForeignOutcome(t *testing.T) {
	t.Parallel()
	store := newStampedDrainStore(t, gate.Require) // ResolveConditionalWriter → live CAS writer

	// Foreign preserve.
	canceled := mustCreate(t, store, beads.Bead{Title: "canceled root"})
	closed := "closed"
	if err := store.Update(canceled.ID, beads.UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeCanceled},
	}); err != nil {
		t.Fatalf("close root as canceled: %v", err)
	}
	preserved, err := fencedUpdateMetadataAndClose(store, canceled.ID,
		map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if err != nil {
		t.Fatalf("fencedUpdateMetadataAndClose (foreign): %v", err)
	}
	if !preserved {
		t.Fatal("preserved = false, want true (CAS path must preserve a foreign outcome)")
	}
	if got := mustGet(t, store, canceled.ID).Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomeCanceled {
		t.Fatalf("outcome = %q, want canceled", got)
	}

	// Happy path over the CAS fence.
	open := mustCreate(t, store, beads.Bead{Title: "open root"})
	preserved, err = fencedUpdateMetadataAndClose(store, open.ID,
		map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if err != nil {
		t.Fatalf("fencedUpdateMetadataAndClose (happy): %v", err)
	}
	if preserved {
		t.Fatal("preserved = true, want false")
	}
	got := mustGet(t, store, open.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("root = {status:%q outcome:%q}, want {closed pass}", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
}
