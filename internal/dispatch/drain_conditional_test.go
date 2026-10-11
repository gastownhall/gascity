package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// newStampedDrainStore opens a MemStore through the beads factory so it
// carries a real conditional-writes stamp — the only sanctioned way for a
// consumer-package test to obtain a moded store.
func newStampedDrainStore(t *testing.T, mode gate.Mode) *beads.MemStore {
	t.Helper()
	mem := beads.NewMemStore()
	_, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: mode,
		OpenFileStore:     func() (beads.Store, error) { return mem, nil },
	})
	if err != nil {
		t.Fatalf("factory open: %v", err)
	}
	return mem
}

func newDrainReservationFixtures(t *testing.T, store beads.Store) (control, member beads.Bead) {
	t.Helper()
	member, err := store.Create(beads.Bead{Title: "member"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	control = beads.Bead{
		ID:       "drain-a",
		Metadata: map[string]string{beadmeta.DrainMemberAccessMetadataKey: beadmeta.DrainMemberAccessExclusive},
	}
	return control, member
}

func reservationOwner(t *testing.T, store beads.Store, memberID string) string {
	t.Helper()
	member, err := store.Get(memberID)
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	return strings.TrimSpace(member.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey])
}

// scriptedDrainWriter wraps a real ConditionalWriter with one-shot fault
// overrides for the CAS-decision cells real stores cannot express
// deterministically (committed-but-ambiguous, spurious conflict).
type scriptedDrainWriter struct {
	inner                beads.ConditionalWriter
	casCalls             int
	commitThenErr        error // apply the swap, then return this error (once)
	failPreconditionOnce bool  // report a conflict without touching state (once)
}

func (w *scriptedDrainWriter) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	return w.inner.UpdateIfMatch(id, rev, opts)
}

func (w *scriptedDrainWriter) CloseIfMatch(id string, rev int64) error {
	return w.inner.CloseIfMatch(id, rev)
}

func (w *scriptedDrainWriter) DeleteIfMatch(id string, rev int64) error {
	return w.inner.DeleteIfMatch(id, rev)
}

func (w *scriptedDrainWriter) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	w.casCalls++
	if w.commitThenErr != nil {
		err := w.commitThenErr
		w.commitThenErr = nil
		if _, casErr := w.inner.CompareAndSetMetadataKey(id, key, expected, next); casErr != nil {
			return false, casErr
		}
		return false, err
	}
	if w.failPreconditionOnce {
		w.failPreconditionOnce = false
		return false, &beads.PreconditionFailedError{ID: id, Expected: 1, Current: 2}
	}
	return w.inner.CompareAndSetMetadataKey(id, key, expected, next)
}

func TestReserveDrainMemberCASContention(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	member, err := store.Create(beads.Bead{Title: "member"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	controls := []beads.Bead{
		{ID: "drain-a", Metadata: map[string]string{beadmeta.DrainMemberAccessMetadataKey: beadmeta.DrainMemberAccessExclusive}},
		{ID: "drain-b", Metadata: map[string]string{beadmeta.DrainMemberAccessMetadataKey: beadmeta.DrainMemberAccessExclusive}},
	}
	results := make([]error, len(controls))
	var wg sync.WaitGroup
	for i := range controls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = reserveDrainMember(store, controls[i], member, ProcessOptions{})
		}()
	}
	wg.Wait()

	owner := reservationOwner(t, store, member.ID)
	if owner != "drain-a" && owner != "drain-b" {
		t.Fatalf("owner = %q, want exactly one of the racing drains", owner)
	}
	var wins, skips int
	for i, err := range results {
		switch {
		case err == nil && controls[i].ID == owner:
			wins++
		case err == nil:
			t.Fatalf("control %s reported success but %s owns the member", controls[i].ID, owner)
		default:
			var re drainReservationError
			if !errors.As(err, &re) {
				t.Fatalf("loser error = %v, want drainReservationError", err)
			}
			if re.Owner != owner {
				t.Fatalf("loser observed owner %q, want %q", re.Owner, owner)
			}
			skips++
		}
	}
	if wins != 1 || skips != 1 {
		t.Fatalf("wins=%d skips=%d, want exactly one of each", wins, skips)
	}
}

func TestReserveDrainMemberCASReentryIsIdempotent(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, member := newDrainReservationFixtures(t, store)
	for i := range 2 {
		if err := reserveDrainMember(store, control, member, ProcessOptions{}); err != nil {
			t.Fatalf("reserve #%d: %v (re-entry must be success, not skip)", i+1, err)
		}
	}
	if owner := reservationOwner(t, store, member.ID); owner != "drain-a" {
		t.Fatalf("owner = %q, want drain-a", owner)
	}
}

func TestReserveDrainMemberRequireIncapableFailsClosed(t *testing.T) {
	store := newStampedDrainStore(t, gate.Require)
	store.DisableConditionalWrites = true
	control, member := newDrainReservationFixtures(t, store)

	err := reserveDrainMember(store, control, member, ProcessOptions{})
	if !beads.IsConditionalWritesRequired(err) {
		t.Fatalf("err = %v, want the typed require refusal", err)
	}
	if owner := reservationOwner(t, store, member.ID); owner != "" {
		t.Fatalf("owner = %q after refusal, want empty (no unconditional fallback write)", owner)
	}
}

func TestClaimDrainReservationCASAmbiguousCommitSelfWins(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, member := newDrainReservationFixtures(t, store)
	inner, ok := beads.ConditionalWriterFor(store)
	if !ok {
		t.Fatal("MemStore must supply a conditional writer")
	}
	writer := &scriptedDrainWriter{inner: inner, commitThenErr: errors.New("i/o timeout")}

	if err := claimDrainReservationCAS(store, writer, control, member); err != nil {
		t.Fatalf("ambiguous committed claim = %v, want self-win nil (§9.3: our own committed write)", err)
	}
	if owner := reservationOwner(t, store, member.ID); owner != "drain-a" {
		t.Fatalf("owner = %q, want drain-a", owner)
	}
}

func TestClaimDrainReservationCASSpuriousConflictRetriesOnce(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, member := newDrainReservationFixtures(t, store)
	inner, _ := beads.ConditionalWriterFor(store)
	writer := &scriptedDrainWriter{inner: inner, failPreconditionOnce: true}

	if err := claimDrainReservationCAS(store, writer, control, member); err != nil {
		t.Fatalf("spurious conflict = %v, want one bounded re-issue to succeed", err)
	}
	if writer.casCalls != 2 {
		t.Fatalf("cas calls = %d, want exactly 2 (one bounded re-issue)", writer.casCalls)
	}
	if owner := reservationOwner(t, store, member.ID); owner != "drain-a" {
		t.Fatalf("owner = %q, want drain-a", owner)
	}
}

func TestClaimDrainReservationCASPersistentSpuriousConflictSurfaces(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, member := newDrainReservationFixtures(t, store)
	writer := &alwaysPreconditionWriter{}

	err := claimDrainReservationCAS(store, writer, control, member)
	if err == nil {
		t.Fatal("persistent spurious conflict must surface an error for the next level-triggered pass")
	}
	var re drainReservationError
	if errors.As(err, &re) {
		t.Fatalf("err = %v; a spurious (empty-owner) conflict must NOT read as a genuine reservation conflict", err)
	}
}

type alwaysPreconditionWriter struct{}

func (alwaysPreconditionWriter) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return beads.ErrConditionalWriteUnsupported
}

func (alwaysPreconditionWriter) CloseIfMatch(string, int64) error {
	return beads.ErrConditionalWriteUnsupported
}

func (alwaysPreconditionWriter) DeleteIfMatch(string, int64) error {
	return beads.ErrConditionalWriteUnsupported
}

func (alwaysPreconditionWriter) CompareAndSetMetadataKey(id, _, _, _ string) (bool, error) {
	return false, &beads.PreconditionFailedError{ID: id, Expected: 1, Current: 2}
}

func TestReleaseDrainReservationCAS(t *testing.T) {
	t.Run("owner clears its own reservation", func(t *testing.T) {
		store := newStampedDrainStore(t, gate.Auto)
		control, member := newDrainReservationFixtures(t, store)
		if err := reserveDrainMember(store, control, member, ProcessOptions{}); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if err := releaseDrainReservation(store, "drain-a", member.ID); err != nil {
			t.Fatalf("release: %v", err)
		}
		if owner := reservationOwner(t, store, member.ID); owner != "" {
			t.Fatalf("owner = %q after release, want empty", owner)
		}
	})
	t.Run("losing the release CAS is the correct outcome", func(t *testing.T) {
		store := newStampedDrainStore(t, gate.Auto)
		_, member := newDrainReservationFixtures(t, store)
		// A successor drain already re-claimed the member: clearing now would
		// clobber its reservation. The lost CAS is success-by-loss.
		if err := store.SetMetadata(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, "drain-successor"); err != nil {
			t.Fatalf("seed successor: %v", err)
		}
		if err := releaseDrainReservation(store, "drain-a", member.ID); err != nil {
			t.Fatalf("release after successor re-claim = %v, want nil (loss is correct)", err)
		}
		if owner := reservationOwner(t, store, member.ID); owner != "drain-successor" {
			t.Fatalf("owner = %q, want the successor's reservation intact", owner)
		}
	})
}

// TestSyncControlEpochToAttemptCASNeverRegresses races the attempt-recovery
// epoch sync on a fenced store: concurrent syncs (and a competing higher
// advance) must land the epoch at the highest attempt, never a lost update.
func TestSyncControlEpochToAttemptCASNeverRegresses(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, err := store.Create(beads.Bead{Title: "control", Metadata: map[string]string{
		beadmeta.ControlEpochMetadataKey: "1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	attempt := beads.Bead{Metadata: map[string]string{beadmeta.AttemptMetadataKey: "3"}}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctl, getErr := store.Get(control.ID)
			if getErr != nil {
				errs[i] = getErr
				return
			}
			errs[i] = syncControlEpochToAttempt(store, ctl, attempt)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("sync %d: %v (losing the sync race is benign, never an error)", i, err)
		}
	}
	updated, _ := store.Get(control.ID)
	if got := updated.Metadata[beadmeta.ControlEpochMetadataKey]; got != "3" {
		t.Fatalf("epoch = %q, want 3", got)
	}
}

func TestRetryableDrainReservationErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"genuine competing owner is terminal", drainReservationError{ControlID: "a", MemberID: "m", Owner: "b"}, false},
		{"require refusal is terminal fail-closed", &beads.ConditionalWritesRequiredError{StoreKind: "BdStore", Reason: "r"}, false},
		{"CAS exhaustion re-enters", &beads.CASRetriesExhaustedError{ID: "m", Key: "k", Attempts: 4}, true},
		{"runtime unsupported latch re-enters (next resolve degrades)", beads.ErrConditionalWriteUnsupported, true},
		{"transport transient re-enters", errors.New("dial tcp: i/o timeout"), true},
		{"plain store error stays terminal (pre-fence behavior)", errors.New("corrupt manifest"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableDrainReservationError(tc.err); got != tc.retryable {
				t.Fatalf("retryable(%v) = %v, want %v", tc.err, got, tc.retryable)
			}
		})
	}
}

// TestFenceLossClassifiesTransientAndKeepsControlOpen pins the CRITICAL
// review finding: a routine CAS-last fence loser (molecule.ErrEpochConflict)
// must be a retryable convergence signal — classified transient by
// markControllerSpawnError with the control left OPEN — never routed into
// the partial-attach hard path that terminally closes the shared control and
// makes the promised next-pass convergence impossible.
func TestFenceLossClassifiesTransientAndKeepsControlOpen(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, err := store.Create(beads.Bead{Title: "control"})
	if err != nil {
		t.Fatal(err)
	}

	fenceLoss := markTransientControllerBoundaryError(
		fmt.Errorf("attach epoch conflict on %s attempt 2 (fence lost; converging next pass): %w", control.ID, molecule.ErrEpochConflict))
	if !IsTransientControllerError(fenceLoss) {
		t.Fatal("fence-loss error not classified transient")
	}
	if retryable := markControllerSpawnError(store, control.ID, fenceLoss, ProcessOptions{}); !retryable {
		t.Fatal("markControllerSpawnError treated the fence loss as hard")
	}
	after, err := store.Get(control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status == "closed" {
		t.Fatal("control was closed on a routine fence loss — convergence impossible")
	}
	if after.Metadata[beadmeta.ControllerRetryableMetadataKey] != "true" {
		t.Fatalf("control not marked retryable: %+v", after.Metadata)
	}

	// The typed CAS contention classes re-enter too.
	if !IsTransientControllerError(&beads.CASRetriesExhaustedError{ID: "x", Key: "k", Attempts: 4}) {
		t.Fatal("CAS exhaustion not transient")
	}
	if !IsTransientControllerError(fmt.Errorf("wrapped: %w", beads.ErrConditionalWriteUnsupported)) {
		t.Fatal("runtime unsupported latch not transient")
	}
	if IsTransientControllerError(&beads.ConditionalWritesRequiredError{StoreKind: "BdStore", Reason: "r"}) {
		t.Fatal("require refusal must stay hard/fail-closed")
	}
}
