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
	"github.com/gastownhall/gascity/internal/beads/beadstest"
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
	// Both controls are persisted open drains: the loser's reservation read
	// may already see the winner's value, and only an open owner conflicts.
	controls := []beads.Bead{
		createExclusiveDrainControl(t, store, "drain-a"),
		createExclusiveDrainControl(t, store, "drain-b"),
	}
	results := make([]error, len(controls))
	var wg sync.WaitGroup
	for i := range controls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = reserveDrainMember(beadstest.Graph(store), controls[i], member, ProcessOptions{Work: beadstest.Work(store)})
		}()
	}
	wg.Wait()

	owner := reservationOwner(t, store, member.ID)
	if owner != controls[0].ID && owner != controls[1].ID {
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

// TestReserveDrainMemberTakesOverMissingOwner covers a reservation whose
// owner no longer resolves anywhere in the drain's residency frame — a drain
// control purged or deleted without releasing it. The slot is dead and the
// reserving drain takes it, through the legacy write and through the CAS.
func TestReserveDrainMemberTakesOverMissingOwner(t *testing.T) {
	for _, tc := range []struct {
		name       string
		store      func(t *testing.T) *beads.MemStore
		wantWriter bool
	}{
		{name: "legacy write", store: func(*testing.T) *beads.MemStore { return beads.NewMemStore() }},
		{name: "conditional writer", store: func(t *testing.T) *beads.MemStore { return newStampedDrainStore(t, gate.Auto) }, wantWriter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.store(t)
			writer, _, err := beads.ResolveConditionalWriter(store)
			if err != nil {
				t.Fatalf("ResolveConditionalWriter: %v", err)
			}
			if got := writer != nil; got != tc.wantWriter {
				t.Fatalf("conditional writer resolved = %v, want %v", got, tc.wantWriter)
			}
			control, member := newDrainReservationFixtures(t, store)
			const purged = "gc-purged-drain"
			if _, err := store.Get(purged); !errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("Get(%s) error = %v, want not found", purged, err)
			}
			if err := store.SetMetadata(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, purged); err != nil {
				t.Fatalf("stamp stale reservation: %v", err)
			}

			if err := reserveDrainMember(beadstest.Graph(store), control, member, ProcessOptions{Work: beadstest.Work(store)}); err != nil {
				t.Fatalf("reserveDrainMember over a missing owner: %v", err)
			}
			if owner := reservationOwner(t, store, member.ID); owner != control.ID {
				t.Fatalf("owner = %q, want taken over by %s", owner, control.ID)
			}
		})
	}
}

// TestReserveDrainMemberOwnerLookupFaultIsAnErrorNeverARelease is the fault
// control for the owner check: a lookup that could not be completed reaches
// the drain as an error, never as a dead owner whose member it may take. The
// reservation stays with its owner, and a transient fault re-enters on a later
// pass instead of closing the drain.
func TestReserveDrainMemberOwnerLookupFaultIsAnErrorNeverARelease(t *testing.T) {
	mem := beads.NewMemStore()
	owner := createExclusiveDrainControl(t, mem, "reservation owner")
	control, member := newDrainReservationFixtures(t, mem)
	if err := mem.SetMetadata(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, owner.ID); err != nil {
		t.Fatalf("stamp reservation: %v", err)
	}
	fault := errors.New("dial tcp: i/o timeout")
	store := drainOwnerGetFaultStore{Store: mem, ownerID: owner.ID, err: fault}

	err := reserveDrainMember(beadstest.Graph(store), control, member, ProcessOptions{Work: beadstest.Work(store)})
	if !errors.Is(err, fault) {
		t.Fatalf("reserveDrainMember error = %v, want the owner lookup fault %v", err, fault)
	}
	if !retryableDrainReservationError(err) {
		t.Fatalf("retryableDrainReservationError(%v) = false, want a transient owner lookup fault to re-enter on a later pass", err)
	}
	if got := reservationOwner(t, mem, member.ID); got != owner.ID {
		t.Fatalf("reservation = %q, want it left with %s", got, owner.ID)
	}
}

// drainOwnerGetFaultStore fails every point read of one bead, the reservation
// owner, and serves every other read from the store it wraps.
type drainOwnerGetFaultStore struct {
	beads.Store
	ownerID string
	err     error
}

func (s drainOwnerGetFaultStore) Get(id string) (beads.Bead, error) {
	if id == s.ownerID {
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

// TestReserveDrainMemberItemWorkFaultIsAnErrorNeverARelease is the fault
// control for the check on a dead owner's item work: a lookup, or a park on
// the open item root, that could not be completed reaches the drain as an
// error, never as an owner with no item work left whose member it may take.
// The reservation stays with the dead owner, and a transient fault re-enters
// on a later pass instead of closing the drain.
func TestReserveDrainMemberItemWorkFaultIsAnErrorNeverARelease(t *testing.T) {
	fault := errors.New("dial tcp: i/o timeout")
	for _, tc := range []struct {
		name  string
		fault func(mem *beads.MemStore, owner, root beads.Bead) beads.Store
	}{
		{name: "listing the owner's item roots", fault: func(mem *beads.MemStore, owner, _ beads.Bead) beads.Store {
			return drainItemWorkFaultStore{Store: mem, listOwnerID: owner.ID, err: fault}
		}},
		{name: "re-reading an item root", fault: func(mem *beads.MemStore, _, root beads.Bead) beads.Store {
			return drainItemWorkFaultStore{Store: mem, getID: root.ID, err: fault}
		}},
		{name: "parking on the open item root", fault: func(mem *beads.MemStore, _, root beads.Bead) beads.Store {
			return drainItemWorkFaultStore{Store: mem, depAddOnID: root.ID, err: fault}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			control, member := newDrainReservationFixtures(t, mem)
			owner, root := seedClosedOwnerWithOpenItemRoot(t, mem, member.ID)
			store := tc.fault(mem, owner, root)

			err := reserveDrainMember(beadstest.Graph(store), control, member, ProcessOptions{Work: beadstest.Work(store)})
			if !errors.Is(err, fault) {
				t.Fatalf("reserveDrainMember error = %v, want the item-work fault %v", err, fault)
			}
			if !retryableDrainReservationError(err) {
				t.Fatalf("retryableDrainReservationError(%v) = false, want a transient item-work fault to re-enter on a later pass", err)
			}
			if got := reservationOwner(t, mem, member.ID); got != owner.ID {
				t.Fatalf("reservation = %q, want it left with the dead owner %s while its item workflow %s is open", got, owner.ID, root.ID)
			}
		})
	}
}

// seedClosedOwnerWithOpenItemRoot persists what a drain control closed from
// outside leaves behind: the control, closed and still named by the member's
// reservation, and the item workflow root it launched for the member, open.
func seedClosedOwnerWithOpenItemRoot(t *testing.T, store beads.Store, memberID string) (owner, root beads.Bead) {
	t.Helper()
	owner = createExclusiveDrainControl(t, store, "closed drain")
	root, err := store.Create(beads.Bead{
		Title: "item workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.DrainControlIDMetadataKey:  owner.ID,
			beadmeta.DrainMemberIDMetadataKey:   memberID,
		},
	})
	if err != nil {
		t.Fatalf("create item root: %v", err)
	}
	if err := store.SetMetadata(memberID, beadmeta.ExclusiveDrainReservationMetadataKey, owner.ID); err != nil {
		t.Fatalf("stamp the owner's reservation: %v", err)
	}
	if err := store.Close(owner.ID); err != nil {
		t.Fatalf("close the owner: %v", err)
	}
	return owner, root
}

// drainItemWorkFaultStore fails one kind of call on a dead owner's item work
// and serves every other call from the store it wraps: the list of the
// owner's item roots (listOwnerID), the point read of one root (getID), or
// the edge parking a drain on one root (depAddOnID).
type drainItemWorkFaultStore struct {
	beads.Store
	listOwnerID string
	getID       string
	depAddOnID  string
	err         error
}

func (s drainItemWorkFaultStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.listOwnerID != "" && query.Metadata[beadmeta.DrainControlIDMetadataKey] == s.listOwnerID {
		return nil, s.err
	}
	return s.Store.List(query)
}

func (s drainItemWorkFaultStore) Get(id string) (beads.Bead, error) {
	if s.getID != "" && id == s.getID {
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

func (s drainItemWorkFaultStore) DepAdd(issueID, dependsOnID, depType string) error {
	if s.depAddOnID != "" && dependsOnID == s.depAddOnID {
		return s.err
	}
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

// TestReserveDrainMemberCASTakeoverContention races two open drains that both
// see a member reserved by a closed drain. The takeover is fenced on the dead
// owner's value, so exactly one drain owns the member and the other conflicts
// on the winner.
func TestReserveDrainMemberCASTakeoverContention(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	member, err := store.Create(beads.Bead{Title: "member"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	dead := createExclusiveDrainControl(t, store, "closed drain")
	if err := store.SetMetadata(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, dead.ID); err != nil {
		t.Fatalf("stamp stale reservation: %v", err)
	}
	if err := store.Close(dead.ID); err != nil {
		t.Fatalf("close stale owner: %v", err)
	}
	controls := []beads.Bead{
		createExclusiveDrainControl(t, store, "drain-a"),
		createExclusiveDrainControl(t, store, "drain-b"),
	}

	start := make(chan struct{})
	results := make([]error, len(controls))
	var wg sync.WaitGroup
	for i := range controls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = reserveDrainMember(beadstest.Graph(store), controls[i], member, ProcessOptions{Work: beadstest.Work(store)})
		}()
	}
	close(start)
	wg.Wait()

	owner := reservationOwner(t, store, member.ID)
	if owner != controls[0].ID && owner != controls[1].ID {
		t.Fatalf("owner = %q, want exactly one of the racing drains", owner)
	}
	var wins, conflicts int
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
				t.Fatalf("loser observed owner %q, want the winner %q", re.Owner, owner)
			}
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d, want exactly one of each", wins, conflicts)
	}
}

func TestReserveDrainMemberCASReentryIsIdempotent(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, member := newDrainReservationFixtures(t, store)
	for i := range 2 {
		if err := reserveDrainMember(beadstest.Graph(store), control, member, ProcessOptions{Work: beadstest.Work(store)}); err != nil {
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

	err := reserveDrainMember(beadstest.Graph(store), control, member, ProcessOptions{Work: beadstest.Work(store)})
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

	if err := claimDrainReservationCAS(store, writer, control, member, ""); err != nil {
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

	if err := claimDrainReservationCAS(store, writer, control, member, ""); err != nil {
		t.Fatalf("spurious conflict = %v, want one bounded re-issue to succeed", err)
	}
	if writer.casCalls != 2 {
		t.Fatalf("cas calls = %d, want exactly 2 (one bounded re-issue)", writer.casCalls)
	}
	if owner := reservationOwner(t, store, member.ID); owner != "drain-a" {
		t.Fatalf("owner = %q, want drain-a", owner)
	}
}

// newStaleDrainReservation stamps a reservation naming a released owner and
// returns the scripted writer a takeover claim runs through.
func newStaleDrainReservation(t *testing.T, store *beads.MemStore, member beads.Bead, staleOwner string) *scriptedDrainWriter {
	t.Helper()
	if err := store.SetMetadata(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, staleOwner); err != nil {
		t.Fatalf("stamp stale reservation: %v", err)
	}
	inner, ok := beads.ConditionalWriterFor(store)
	if !ok {
		t.Fatal("MemStore must supply a conditional writer")
	}
	return &scriptedDrainWriter{inner: inner}
}

func TestClaimDrainReservationCASTakeoverSpuriousConflictRetriesOnce(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, member := newDrainReservationFixtures(t, store)
	writer := newStaleDrainReservation(t, store, member, "drain-dead")
	writer.failPreconditionOnce = true

	if err := claimDrainReservationCAS(store, writer, control, member, "drain-dead"); err != nil {
		t.Fatalf("spurious conflict on a takeover = %v, want one bounded re-issue to succeed", err)
	}
	if writer.casCalls != 2 {
		t.Fatalf("cas calls = %d, want exactly 2 (one bounded re-issue)", writer.casCalls)
	}
	if owner := reservationOwner(t, store, member.ID); owner != control.ID {
		t.Fatalf("owner = %q, want %s", owner, control.ID)
	}
}

func TestClaimDrainReservationCASTakeoverOfReleasedSlotRetriesEmpty(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, member := newDrainReservationFixtures(t, store)
	writer := newStaleDrainReservation(t, store, member, "drain-dead")
	// The slot is released after the stale value was observed and before the
	// takeover's CAS lands.
	if err := store.SetMetadata(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, ""); err != nil {
		t.Fatalf("release slot: %v", err)
	}

	if err := claimDrainReservationCAS(store, writer, control, member, "drain-dead"); err != nil {
		t.Fatalf("takeover of a concurrently released slot = %v, want the re-issue against the empty slot to win", err)
	}
	if writer.casCalls != 2 {
		t.Fatalf("cas calls = %d, want exactly 2", writer.casCalls)
	}
	if owner := reservationOwner(t, store, member.ID); owner != control.ID {
		t.Fatalf("owner = %q, want %s", owner, control.ID)
	}
}

func TestClaimDrainReservationCASTakeoverOfRestampedSlotConflicts(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, member := newDrainReservationFixtures(t, store)
	writer := newStaleDrainReservation(t, store, member, "drain-dead")
	// A third drain took the slot over after the stale value was observed.
	if err := store.SetMetadata(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, "drain-c"); err != nil {
		t.Fatalf("restamp slot: %v", err)
	}

	err := claimDrainReservationCAS(store, writer, control, member, "drain-dead")
	var re drainReservationError
	if !errors.As(err, &re) || re.Owner != "drain-c" {
		t.Fatalf("takeover of a restamped slot = %v, want drainReservationError naming drain-c", err)
	}
	if writer.casCalls != 1 {
		t.Fatalf("cas calls = %d, want 1 (no write over a value never observed)", writer.casCalls)
	}
	if owner := reservationOwner(t, store, member.ID); owner != "drain-c" {
		t.Fatalf("owner = %q, want drain-c", owner)
	}
}

func TestClaimDrainReservationCASPersistentSpuriousConflictSurfaces(t *testing.T) {
	store := newStampedDrainStore(t, gate.Auto)
	control, member := newDrainReservationFixtures(t, store)
	writer := &alwaysPreconditionWriter{}

	err := claimDrainReservationCAS(store, writer, control, member, "")
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
		if err := reserveDrainMember(beadstest.Graph(store), control, member, ProcessOptions{Work: beadstest.Work(store)}); err != nil {
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
			errs[i] = syncControlEpochToAttempt(beadstest.Graph(store), ctl, attempt)
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
		{"a dead owner's open item work re-enters once it closes", fmt.Errorf("%w: item workflow still open", ErrControlPending), true},
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
	if retryable := markControllerSpawnError(beadstest.Graph(store), control.ID, fenceLoss, ProcessOptions{Work: beadstest.Work(store)}); !retryable {
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
