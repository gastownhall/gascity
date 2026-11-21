package molecule

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func strp(s string) *string { return &s }

func mustCreateExactClaimBead(t *testing.T, store *beads.MemStore) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "exact claim target",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:        "root-77",
			beadmeta.ContinuationGroupMetadataKey: "cg-42",
			beadmeta.RoutedToMetadataKey:          "session-alpha",
		},
	})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}
	return b
}

func TestClaimExact_FirstClaimFromEmptyGeneration(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)

	want := ClaimExactPreconditions{
		Status:            strp("open"),
		RoutedTo:          strp("session-alpha"),
		RootBeadID:        strp("root-77"),
		ContinuationGroup: strp("cg-42"),
	}
	onSuccess := beads.UpdateOpts{
		Status:   strp("in_progress"),
		Assignee: strp("worker-9"),
	}

	got, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, want, "", onSuccess)
	if err != nil {
		t.Fatalf("ClaimExact: %v", err)
	}
	if outcome != ClaimExactClaimed {
		t.Fatalf("outcome = %q, want %q", outcome, ClaimExactClaimed)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "worker-9" {
		t.Fatalf("assignee = %q, want worker-9", got.Assignee)
	}
	gen := got.Metadata[beadmeta.ClaimGenerationMetadataKey]
	if gen != "1" {
		t.Fatalf("claim generation = %q, want %q", gen, "1")
	}
	// Onlooker fields untouched by onSuccess must survive the merge.
	if got.Metadata[beadmeta.RootBeadIDMetadataKey] != "root-77" {
		t.Fatalf("root bead id metadata clobbered: %q", got.Metadata[beadmeta.RootBeadIDMetadataKey])
	}
}

func TestClaimExact_PreconditionMismatchFailsClosedWithoutWrite(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)

	want := ClaimExactPreconditions{
		RoutedTo: strp("session-bravo"), // does not match "session-alpha"
	}
	onSuccess := beads.UpdateOpts{Assignee: strp("worker-9")}

	got, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, want, "", onSuccess)
	if err != nil {
		t.Fatalf("ClaimExact: %v", err)
	}
	if outcome != ClaimExactPreconditionFailed {
		t.Fatalf("outcome = %q, want %q", outcome, ClaimExactPreconditionFailed)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee mutated on precondition failure: %q", got.Assignee)
	}
	if _, ok := got.Metadata[beadmeta.ClaimGenerationMetadataKey]; ok {
		t.Fatalf("claim generation written on precondition failure: %q", got.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
}

// TestClaimExact_EachPreconditionFieldMismatchesIndependently covers the three
// fields the original test suite left unexercised: a mismatch on any single
// field must fail closed on its own, not just when RoutedTo is wrong.
func TestClaimExact_EachPreconditionFieldMismatchesIndependently(t *testing.T) {
	cases := []struct {
		name string
		want ClaimExactPreconditions
	}{
		{"status", ClaimExactPreconditions{Status: strp("in_progress")}},
		{"routed_to", ClaimExactPreconditions{RoutedTo: strp("session-bravo")}},
		{"root_bead_id", ClaimExactPreconditions{RootBeadID: strp("root-99")}},
		{"continuation_group", ClaimExactPreconditions{ContinuationGroup: strp("cg-99")}},
		{"assignee", ClaimExactPreconditions{Assignee: strp("worker-9")}}, // bead is unassigned
		{"step_id", ClaimExactPreconditions{StepID: strp("build")}},       // bead carries no step id
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b := mustCreateExactClaimBead(t, store)

			got, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, tc.want, "", beads.UpdateOpts{Assignee: strp("worker-9")})
			if err != nil {
				t.Fatalf("ClaimExact: %v", err)
			}
			if outcome != ClaimExactPreconditionFailed {
				t.Fatalf("outcome = %q, want %q", outcome, ClaimExactPreconditionFailed)
			}
			if got.Assignee != "" {
				t.Fatalf("assignee mutated on precondition failure: %q", got.Assignee)
			}
			if _, ok := got.Metadata[beadmeta.ClaimGenerationMetadataKey]; ok {
				t.Fatalf("claim generation written on precondition failure: %q", got.Metadata[beadmeta.ClaimGenerationMetadataKey])
			}
		})
	}
}

func TestClaimExact_ReservedGenerationKeyInOnSuccessIsRejected(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)

	onSuccess := beads.UpdateOpts{
		Assignee: strp("worker-9"),
		Metadata: map[string]string{beadmeta.ClaimGenerationMetadataKey: "999"},
	}

	got, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, ClaimExactPreconditions{}, "", onSuccess)
	if !errors.Is(err, ErrClaimGenerationReserved) {
		t.Fatalf("err = %v, want ErrClaimGenerationReserved", err)
	}
	if outcome != "" {
		t.Fatalf("outcome = %q, want empty on rejection", outcome)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee mutated despite rejected call: %q", got.Assignee)
	}

	// Nothing was written: the bead in the store is untouched.
	fresh, getErr := store.Get(b.ID)
	if getErr != nil {
		t.Fatalf("get: %v", getErr)
	}
	if _, ok := fresh.Metadata[beadmeta.ClaimGenerationMetadataKey]; ok {
		t.Fatalf("claim generation written despite rejected call: %q", fresh.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
}

func TestClaimExact_NonPositiveStoredGenerationFailsClosed(t *testing.T) {
	for _, bad := range []string{"0", "-1", "not-a-number"} {
		t.Run(bad, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{
				Title:  "exact claim target",
				Type:   "task",
				Status: "open",
				Metadata: map[string]string{
					beadmeta.ClaimGenerationMetadataKey: bad,
				},
			})
			if err != nil {
				t.Fatalf("create bead: %v", err)
			}

			got, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, ClaimExactPreconditions{}, bad, beads.UpdateOpts{Assignee: strp("worker-9")})
			if err == nil {
				t.Fatalf("expected an error for stored generation %q", bad)
			}
			if outcome != "" {
				t.Fatalf("outcome = %q, want empty on error", outcome)
			}
			if got.Assignee != "" {
				t.Fatalf("assignee mutated despite invalid generation: %q", got.Assignee)
			}
			if got.Metadata[beadmeta.ClaimGenerationMetadataKey] != bad {
				t.Fatalf("generation mutated: %q -> %q", bad, got.Metadata[beadmeta.ClaimGenerationMetadataKey])
			}
		})
	}
}

func TestClaimExact_StaleGenerationFailsClosedWithoutFallback(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)

	first, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, ClaimExactPreconditions{}, "", beads.UpdateOpts{Assignee: strp("worker-1")})
	if err != nil || outcome != ClaimExactClaimed {
		t.Fatalf("setup claim failed: outcome=%q err=%v", outcome, err)
	}
	firstGen := first.Metadata[beadmeta.ClaimGenerationMetadataKey]
	if firstGen != "1" {
		t.Fatalf("first generation = %q, want 1", firstGen)
	}

	// Retry with the now-stale fromGeneration ("") instead of the current "1".
	got, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, ClaimExactPreconditions{}, "", beads.UpdateOpts{Assignee: strp("worker-2")})
	if err != nil {
		t.Fatalf("ClaimExact: %v", err)
	}
	if outcome != ClaimExactStale {
		t.Fatalf("outcome = %q, want %q", outcome, ClaimExactStale)
	}
	if got.Assignee != "worker-1" {
		t.Fatalf("stale claim must not overwrite the winner: assignee = %q", got.Assignee)
	}
	if got.Metadata[beadmeta.ClaimGenerationMetadataKey] != firstGen {
		t.Fatalf("stale claim bumped generation: %q -> %q", firstGen, got.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
}

// TestClaimExact_RetryAtLandedGenerationIsStaleNotReclaimed documents the
// deliberate tradeoff: ClaimExact cannot tell "I already won this" apart from
// "a different caller landed on the same next value", so a retry that finds
// its target generation already reached is always Stale, never a silent
// re-application of onSuccess. A caller doing crash recovery must inspect the
// returned bead itself (here: its own assignee survived) rather than lean on
// ClaimExact to re-apply effects.
func TestClaimExact_RetryAtLandedGenerationIsStaleNotReclaimed(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)

	onSuccess := beads.UpdateOpts{Assignee: strp("worker-1")}
	first, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, ClaimExactPreconditions{}, "", onSuccess)
	if err != nil || outcome != ClaimExactClaimed {
		t.Fatalf("setup claim failed: outcome=%q err=%v", outcome, err)
	}
	firstGen := first.Metadata[beadmeta.ClaimGenerationMetadataKey]

	got, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, ClaimExactPreconditions{}, "", onSuccess)
	if err != nil {
		t.Fatalf("ClaimExact retry: %v", err)
	}
	if outcome != ClaimExactStale {
		t.Fatalf("retry outcome = %q, want %q", outcome, ClaimExactStale)
	}
	if got.Metadata[beadmeta.ClaimGenerationMetadataKey] != firstGen {
		t.Fatalf("retry changed generation: %q -> %q", firstGen, got.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
	if got.Assignee != "worker-1" {
		t.Fatalf("assignee = %q, want worker-1 (the winning attempt's own effect, still visible)", got.Assignee)
	}
}

func TestClaimExact_UnsupportedStoreFailsClosedNoFallback(t *testing.T) {
	store := beads.NewMemStore()
	store.DisableConditionalWrites = true
	b := mustCreateExactClaimBead(t, store)

	got, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, ClaimExactPreconditions{}, "", beads.UpdateOpts{Assignee: strp("worker-9")})
	if err == nil {
		t.Fatal("expected an error when the store cannot support conditional writes")
	}
	if !beads.IsConditionalWriteUnsupported(err) {
		t.Fatalf("error = %v, want IsConditionalWriteUnsupported", err)
	}
	if outcome != "" {
		t.Fatalf("outcome = %q, want empty on error", outcome)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee mutated despite unsupported store: %q", got.Assignee)
	}
}

func TestClaimExact_UnknownBeadReturnsError(t *testing.T) {
	store := beads.NewMemStore()
	_, _, err := ClaimExact(beadstest.GraphMolecule(store), "does-not-exist", ClaimExactPreconditions{}, "", beads.UpdateOpts{})
	if err == nil {
		t.Fatal("expected an error for an unknown bead id")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("error = %v, want wrapping beads.ErrNotFound", err)
	}
}

// TestClaimExact_ConcurrentExactClaimsExactlyOneWinner is the race regression
// this bead exists for: two scheduler-bound goroutines racing an exact claim
// on the same bead from the same observed generation must not both win.
func TestClaimExact_ConcurrentExactClaimsExactlyOneWinner(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)

	const attempts = 20
	var wg sync.WaitGroup
	claimed := make([]bool, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			assignee := "worker-" + strconv.Itoa(i)
			_, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, ClaimExactPreconditions{}, "", beads.UpdateOpts{Assignee: strp(assignee)})
			if err != nil {
				t.Errorf("attempt %d: ClaimExact: %v", i, err)
				return
			}
			claimed[i] = outcome == ClaimExactClaimed
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, ok := range claimed {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}

	final, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.Metadata[beadmeta.ClaimGenerationMetadataKey] != "1" {
		t.Fatalf("final generation = %q, want 1 (exactly one CAS should ever succeed from \"\")",
			final.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
}

// raceBeforeUpdateIfMatchStore wraps a *beads.MemStore and, on the first call
// to UpdateIfMatch for the watched bead, injects a second full claim (its own
// ClaimExact call, winning from the same fromGeneration this call observed)
// before letting the real caller's UpdateIfMatch proceed. It simulates a
// second, later claimant B racing in between this call's (A's) Get and its
// UpdateIfMatch.
type raceBeforeUpdateIfMatchStore struct {
	*beads.MemStore
	targetID     string
	racedFromGen string
	raced        bool
}

func (s *raceBeforeUpdateIfMatchStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	if !s.raced && id == s.targetID {
		s.raced = true
		_, outcome, err := ClaimExact(beadstest.GraphMolecule(s.MemStore), id, ClaimExactPreconditions{}, s.racedFromGen, beads.UpdateOpts{Assignee: strp("worker-B")})
		if err != nil {
			return fmt.Errorf("test setup: racer B claim: %w", err)
		}
		if outcome != ClaimExactClaimed {
			return fmt.Errorf("test setup: racer B claim did not win: %s", outcome)
		}
	}
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
}

// TestClaimExact_StaleWriterCannotOverwriteLaterWinnersEffects is the atomicity
// regression this fix exists for: a stale caller A observes fromGeneration,
// then before A's own UpdateIfMatch runs, a second caller B independently wins
// a full claim from that same fromGeneration (its ClaimExact call reads the
// bead, matches preconditions, and its own UpdateIfMatch commits first). A's
// UpdateIfMatch is fenced on the revision A observed at its own Get, so B's
// intervening commit must make A's write lose the fence (PreconditionFailed),
// not silently overwrite B's assignee/generation with A's stale effects.
func TestClaimExact_StaleWriterCannotOverwriteLaterWinnersEffects(t *testing.T) {
	inner := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, inner)

	racer := &raceBeforeUpdateIfMatchStore{
		MemStore:     inner,
		targetID:     b.ID,
		racedFromGen: "", // the generation both A and B observe before either writes
	}

	gotA, outcomeA, errA := ClaimExact(beadstest.GraphMolecule(racer), b.ID, ClaimExactPreconditions{}, "", beads.UpdateOpts{Assignee: strp("worker-A")})
	if errA != nil {
		t.Fatalf("ClaimExact (A): %v", errA)
	}
	if outcomeA != ClaimExactStale {
		t.Fatalf("A outcome = %q, want %q (B's commit must fence A out)", outcomeA, ClaimExactStale)
	}

	final, err := inner.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.Assignee != "worker-B" {
		t.Fatalf("assignee = %q, want worker-B: A's stale write overwrote B's committed effects", final.Assignee)
	}
	if final.Metadata[beadmeta.ClaimGenerationMetadataKey] != "1" {
		t.Fatalf("generation = %q, want 1 (B's own advance, untouched by A)", final.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
	if gotA.Assignee != "worker-B" {
		t.Fatalf("A's returned bead assignee = %q, want worker-B (A must see B's committed state, not its own)", gotA.Assignee)
	}
}

func TestClaimExact_MaxInt64GenerationFailsClosed(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:  "exact claim target",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.ClaimGenerationMetadataKey: strconv.FormatInt(math.MaxInt64, 10),
		},
	})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	got, outcome, err := ClaimExact(beadstest.GraphMolecule(store), b.ID, ClaimExactPreconditions{}, strconv.FormatInt(math.MaxInt64, 10), beads.UpdateOpts{Assignee: strp("worker-9")})
	if err == nil {
		t.Fatal("expected an error advancing math.MaxInt64")
	}
	if outcome != "" {
		t.Fatalf("outcome = %q, want empty on error", outcome)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee mutated despite overflow rejection: %q", got.Assignee)
	}
	if got.Metadata[beadmeta.ClaimGenerationMetadataKey] != strconv.FormatInt(math.MaxInt64, 10) {
		t.Fatalf("generation mutated: got %q", got.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
}

// exactReaderFunc adapts a Get function to ClaimExactReader so tests can
// script reads (and read failures) without a full store.
type exactReaderFunc func(id string) (beads.Bead, error)

func (f exactReaderFunc) Get(id string) (beads.Bead, error) { return f(id) }

// exactRevisionWriterStub records the single UpdateIfMatch a claim issued and
// answers a scripted error, so the tier's drop-to-assignment behavior can be
// exercised without a store that actually lacks revision support.
type exactRevisionWriterStub struct {
	err        error
	calls      int
	revision   int64
	opts       beads.UpdateOpts
	appliedIDs []string
}

func (w *exactRevisionWriterStub) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	w.calls++
	w.revision = expectedRevision
	w.opts = opts
	if w.err != nil {
		return w.err
	}
	w.appliedIDs = append(w.appliedIDs, id)
	return nil
}

func (w *exactRevisionWriterStub) CloseIfMatch(_ string, _ int64) error {
	return fmt.Errorf("stub: CloseIfMatch not expected in exact-claim tests")
}

func (w *exactRevisionWriterStub) DeleteIfMatch(_ string, _ int64) error {
	return fmt.Errorf("stub: DeleteIfMatch not expected in exact-claim tests")
}

func (w *exactRevisionWriterStub) CompareAndSetMetadataKey(_, _, _, _ string) (bool, error) {
	return false, fmt.Errorf("stub: CompareAndSetMetadataKey not expected in exact-claim tests")
}

// exactAssignmentWriterStub records the guarded write a claim issued and can
// simulate the straddle the assignment tier exists for: the guard passes and
// the write lands while the row's step identity has already moved. When
// landed and store is set it applies the effects the way a real guarded
// write would, so the confirmation read observes the claim's own result.
type exactAssignmentWriterStub struct {
	landed         bool
	err            error
	calls          int
	expectedStat   string
	expectedAssign string
	opts           beads.UpdateOpts
	store          *beads.MemStore
	onLanded       func(id string)
}

func (w *exactAssignmentWriterStub) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts beads.UpdateOpts) (bool, error) {
	w.calls++
	w.expectedStat = expectedStatus
	w.expectedAssign = expectedAssignee
	w.opts = opts
	if w.err != nil {
		return false, w.err
	}
	if !w.landed {
		return false, nil
	}
	if w.store != nil {
		if err := w.store.Update(id, opts); err != nil {
			return false, err
		}
	}
	if w.onLanded != nil {
		w.onLanded(id)
	}
	return true, nil
}

func exactClaimRequest(id string, want ClaimExactPreconditions, onSuccess beads.UpdateOpts) ClaimExactRequest {
	return ClaimExactRequest{ID: id, Want: want, OnSuccess: onSuccess}
}

// TestClaimExactWithWriters_FencesObservedAssigneeAndStepID pins the two
// preconditions the hook's exact-claim plan adds over the scheduler launch
// path: the observed assignee (a pointer to "" requires the bead to be
// unassigned) and the observed gc.step_id. Either moving before the claim
// must fail closed with no write on any tier.
func TestClaimExactWithWriters_FencesObservedAssigneeAndStepID(t *testing.T) {
	cases := []struct {
		name string
		want ClaimExactPreconditions
	}{
		{"assignee", ClaimExactPreconditions{Assignee: strp("worker-9")}},
		{"step_id", ClaimExactPreconditions{StepID: strp("build")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b := mustCreateExactClaimBead(t, store)

			res, err := ClaimExactWithWriters(store, ClaimExactWriters{Revision: store}, exactClaimRequest(b.ID, tc.want, beads.UpdateOpts{Assignee: strp("worker-9")}))
			if err != nil {
				t.Fatalf("ClaimExactWithWriters: %v", err)
			}
			if res.Outcome != ClaimExactPreconditionFailed {
				t.Fatalf("outcome = %q, want %q", res.Outcome, ClaimExactPreconditionFailed)
			}
			if res.Bead.Assignee != "" {
				t.Fatalf("assignee mutated on precondition failure: %q", res.Bead.Assignee)
			}
			if _, ok := res.Bead.Metadata[beadmeta.ClaimGenerationMetadataKey]; ok {
				t.Fatalf("claim generation written on precondition failure: %q", res.Bead.Metadata[beadmeta.ClaimGenerationMetadataKey])
			}
		})
	}
}

// TestClaimExactWithWriters_RevisionTierClaimsAndFencesOnRevision covers the
// writer-injected shape of the original strict claim: a real MemStore reader
// plus its own conditional writer claims once, reports the revision fence, and
// folds the generation bump into the same write.
func TestClaimExactWithWriters_RevisionTierClaimsAndFencesOnRevision(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)

	res, err := ClaimExactWithWriters(store, ClaimExactWriters{Revision: store}, exactClaimRequest(b.ID, ClaimExactPreconditions{Status: strp("open")}, beads.UpdateOpts{Status: strp("in_progress"), Assignee: strp("worker-9")}))
	if err != nil {
		t.Fatalf("ClaimExactWithWriters: %v", err)
	}
	if res.Outcome != ClaimExactClaimed {
		t.Fatalf("outcome = %q, want %q", res.Outcome, ClaimExactClaimed)
	}
	if res.Fence != ClaimExactFenceRevision {
		t.Fatalf("fence = %q, want %q", res.Fence, ClaimExactFenceRevision)
	}
	if res.Bead.Status != "in_progress" || res.Bead.Assignee != "worker-9" {
		t.Fatalf("claimed bead = %s/%q, want in_progress/worker-9", res.Bead.Status, res.Bead.Assignee)
	}
	if gen := res.Bead.Metadata[beadmeta.ClaimGenerationMetadataKey]; gen != "1" {
		t.Fatalf("claim generation = %q, want 1", gen)
	}
}

// TestClaimExactWithWriters_NeitherWriterFailsClosed: with no usable tier the
// claim must fail closed with beads.ErrConditionalWriteUnsupported and write
// nothing — there is no unconditional fallback, whatever the effects promise.
func TestClaimExactWithWriters_NeitherWriterFailsClosed(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)

	res, err := ClaimExactWithWriters(store, ClaimExactWriters{}, exactClaimRequest(b.ID, ClaimExactPreconditions{}, beads.UpdateOpts{Assignee: strp("worker-9")}))
	if err == nil {
		t.Fatal("expected an error with no conditional writer")
	}
	if !beads.IsConditionalWriteUnsupported(err) {
		t.Fatalf("error = %v, want IsConditionalWriteUnsupported", err)
	}
	if res.Outcome != "" {
		t.Fatalf("outcome = %q, want empty on error", res.Outcome)
	}
	fresh, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.Assignee != "" || fresh.Metadata[beadmeta.ClaimGenerationMetadataKey] != "" {
		t.Fatalf("unsupported store wrote anyway: assignee=%q generation=%q", fresh.Assignee, fresh.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
}

// TestClaimExactWithWriters_RevisionUnsupportedDropsToAssignmentTier covers
// the BdStore-without---if-revision shape: the revision tier reports the
// capability missing, and the claim re-fences the same observed facts on the
// status+assignee guard instead of failing or degrading to an unconditional
// write.
func TestClaimExactWithWriters_RevisionUnsupportedDropsToAssignmentTier(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)
	assignment := &exactAssignmentWriterStub{landed: true, store: store}

	res, err := ClaimExactWithWriters(store, ClaimExactWriters{
		Revision:   &exactRevisionWriterStub{err: beads.ErrConditionalWriteUnsupported},
		Assignment: assignment,
	}, exactClaimRequest(b.ID, ClaimExactPreconditions{Status: strp("open"), Assignee: strp("")}, beads.UpdateOpts{Status: strp("in_progress"), Assignee: strp("worker-9")}))
	if err != nil {
		t.Fatalf("ClaimExactWithWriters: %v", err)
	}
	if res.Outcome != ClaimExactClaimed {
		t.Fatalf("outcome = %q, want %q", res.Outcome, ClaimExactClaimed)
	}
	if res.Fence != ClaimExactFenceAssignment {
		t.Fatalf("fence = %q, want %q", res.Fence, ClaimExactFenceAssignment)
	}
	if assignment.calls != 1 {
		t.Fatalf("assignment writer calls = %d, want 1", assignment.calls)
	}
	if assignment.expectedStat != "open" || assignment.expectedAssign != "" {
		t.Fatalf("guard = %q/%q, want open/\"\" (the observed row)", assignment.expectedStat, assignment.expectedAssign)
	}
	if gen := assignment.opts.Metadata[beadmeta.ClaimGenerationMetadataKey]; gen != "1" {
		t.Fatalf("assignment-tier effects generation = %q, want 1", gen)
	}
}

// TestClaimExactWithWriters_AssignmentGuardRejectsIsStale: a guard that does
// not pass (another claimant landed first) is stale with nothing written, the
// same definitive loss the revision tier reports.
func TestClaimExactWithWriters_AssignmentGuardRejectsIsStale(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)

	res, err := ClaimExactWithWriters(store, ClaimExactWriters{
		Assignment: &exactAssignmentWriterStub{landed: false},
	}, exactClaimRequest(b.ID, ClaimExactPreconditions{Status: strp("open"), Assignee: strp("")}, beads.UpdateOpts{Status: strp("in_progress"), Assignee: strp("worker-9")}))
	if err != nil {
		t.Fatalf("ClaimExactWithWriters: %v", err)
	}
	if res.Outcome != ClaimExactStale {
		t.Fatalf("outcome = %q, want %q", res.Outcome, ClaimExactStale)
	}
	if res.Fence != ClaimExactFenceAssignment {
		t.Fatalf("fence = %q, want %q", res.Fence, ClaimExactFenceAssignment)
	}
}

// TestClaimExactWithWriters_AssignmentTierReadbackSeesMovedObservation is the
// straddle the assignment tier cannot fence inside its write: the guard passes
// and the effects land while the row's step identity has already moved. The
// readback must catch it and report observation_moved so the caller gives the
// bead back rather than execute a step it never observed.
func TestClaimExactWithWriters_AssignmentTierReadbackSeesMovedObservation(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:  "moved step target",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: "root-77",
			beadmeta.StepIDMetadataKey:     "build",
		},
	})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}
	// The guarded write lands, then an unrelated writer moves the step before
	// the readback — the exact race the outcome exists for.
	assignment := &exactAssignmentWriterStub{landed: true, store: store, onLanded: func(id string) {
		if err := store.Update(id, beads.UpdateOpts{Metadata: map[string]string{beadmeta.StepIDMetadataKey: "test"}}); err != nil {
			t.Errorf("move step: %v", err)
		}
	}}

	res, err := ClaimExactWithWriters(store, ClaimExactWriters{Assignment: assignment}, exactClaimRequest(b.ID,
		ClaimExactPreconditions{Status: strp("open"), Assignee: strp(""), StepID: strp("build"), RootBeadID: strp("root-77")},
		beads.UpdateOpts{Status: strp("in_progress"), Assignee: strp("worker-9")}))
	if err != nil {
		t.Fatalf("ClaimExactWithWriters: %v", err)
	}
	if res.Outcome != ClaimExactObservationMoved {
		t.Fatalf("outcome = %q, want %q", res.Outcome, ClaimExactObservationMoved)
	}
	if res.Bead.Metadata[beadmeta.StepIDMetadataKey] != "test" {
		t.Fatalf("moved readback bead = %q, want the moved snapshot", res.Bead.Metadata[beadmeta.StepIDMetadataKey])
	}
}

// TestClaimExactWithWriters_ReadbackFailureAfterCommittedWriteIsClaimed: a
// write that committed but cannot be re-read is a claimed outcome paired with
// ErrClaimExactReadback — the caller must stop and reconcile, never drain as
// if nothing was minted.
func TestClaimExactWithWriters_ReadbackFailureAfterCommittedWriteIsClaimed(t *testing.T) {
	store := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, store)
	calls := 0
	reader := exactReaderFunc(func(id string) (beads.Bead, error) {
		calls++
		if calls > 1 {
			return beads.Bead{}, fmt.Errorf("store evaporated")
		}
		return store.Get(id)
	})

	res, err := ClaimExactWithWriters(reader, ClaimExactWriters{Assignment: &exactAssignmentWriterStub{landed: true}}, exactClaimRequest(b.ID,
		ClaimExactPreconditions{Status: strp("open"), Assignee: strp("")},
		beads.UpdateOpts{Status: strp("in_progress"), Assignee: strp("worker-9")}))
	if err == nil {
		t.Fatal("expected the readback error to surface")
	}
	if !errors.Is(err, ErrClaimExactReadback) {
		t.Fatalf("error = %v, want ErrClaimExactReadback", err)
	}
	if res.Outcome != ClaimExactClaimed {
		t.Fatalf("outcome = %q, want %q (the write committed)", res.Outcome, ClaimExactClaimed)
	}
}

// TestClaimExactWithWriters_FromGenerationNilAdvancesWhateverIsRead: a nil
// FromGeneration means "the caller never observed a generation" — the claim
// still bumps whatever counter the row carries (never resets it) and fences
// only on the identity preconditions plus the guard.
func TestClaimExactWithWriters_FromGenerationNilAdvancesWhateverIsRead(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:    "previously claimed target",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{beadmeta.ClaimGenerationMetadataKey: "5"},
	})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	res, err := ClaimExactWithWriters(store, ClaimExactWriters{Revision: store}, ClaimExactRequest{
		ID:        b.ID,
		Want:      ClaimExactPreconditions{Status: strp("open")},
		OnSuccess: beads.UpdateOpts{Status: strp("in_progress"), Assignee: strp("worker-9")},
	})
	if err != nil {
		t.Fatalf("ClaimExactWithWriters: %v", err)
	}
	if res.Outcome != ClaimExactClaimed {
		t.Fatalf("outcome = %q, want %q", res.Outcome, ClaimExactClaimed)
	}
	if gen := res.Bead.Metadata[beadmeta.ClaimGenerationMetadataKey]; gen != "6" {
		t.Fatalf("claim generation = %q, want 6 (advanced, not reset)", gen)
	}
}

// TestClaimExactWithWriters_FromGenerationMismatchIsStale: an explicit
// FromGeneration that no longer matches the row is stale before any write,
// on the writer-injected shape exactly as on the strict one.
func TestClaimExactWithWriters_FromGenerationMismatchIsStale(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:    "raced target",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{beadmeta.ClaimGenerationMetadataKey: "5"},
	})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}
	stale := "4"

	res, err := ClaimExactWithWriters(store, ClaimExactWriters{Revision: store}, ClaimExactRequest{
		ID:             b.ID,
		Want:           ClaimExactPreconditions{Status: strp("open")},
		FromGeneration: &stale,
		OnSuccess:      beads.UpdateOpts{Status: strp("in_progress"), Assignee: strp("worker-9")},
	})
	if err != nil {
		t.Fatalf("ClaimExactWithWriters: %v", err)
	}
	if res.Outcome != ClaimExactStale {
		t.Fatalf("outcome = %q, want %q", res.Outcome, ClaimExactStale)
	}
	if res.Bead.Metadata[beadmeta.ClaimGenerationMetadataKey] != "5" {
		t.Fatalf("generation = %q, want 5 (no write)", res.Bead.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
}
