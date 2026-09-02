package molecule

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
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

	got, outcome, err := ClaimExact(store, b.ID, want, "", onSuccess)
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

	got, outcome, err := ClaimExact(store, b.ID, want, "", onSuccess)
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b := mustCreateExactClaimBead(t, store)

			got, outcome, err := ClaimExact(store, b.ID, tc.want, "", beads.UpdateOpts{Assignee: strp("worker-9")})
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

	got, outcome, err := ClaimExact(store, b.ID, ClaimExactPreconditions{}, "", onSuccess)
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

			got, outcome, err := ClaimExact(store, b.ID, ClaimExactPreconditions{}, bad, beads.UpdateOpts{Assignee: strp("worker-9")})
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

	first, outcome, err := ClaimExact(store, b.ID, ClaimExactPreconditions{}, "", beads.UpdateOpts{Assignee: strp("worker-1")})
	if err != nil || outcome != ClaimExactClaimed {
		t.Fatalf("setup claim failed: outcome=%q err=%v", outcome, err)
	}
	firstGen := first.Metadata[beadmeta.ClaimGenerationMetadataKey]
	if firstGen != "1" {
		t.Fatalf("first generation = %q, want 1", firstGen)
	}

	// Retry with the now-stale fromGeneration ("") instead of the current "1".
	got, outcome, err := ClaimExact(store, b.ID, ClaimExactPreconditions{}, "", beads.UpdateOpts{Assignee: strp("worker-2")})
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
	first, outcome, err := ClaimExact(store, b.ID, ClaimExactPreconditions{}, "", onSuccess)
	if err != nil || outcome != ClaimExactClaimed {
		t.Fatalf("setup claim failed: outcome=%q err=%v", outcome, err)
	}
	firstGen := first.Metadata[beadmeta.ClaimGenerationMetadataKey]

	got, outcome, err := ClaimExact(store, b.ID, ClaimExactPreconditions{}, "", onSuccess)
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

	got, outcome, err := ClaimExact(store, b.ID, ClaimExactPreconditions{}, "", beads.UpdateOpts{Assignee: strp("worker-9")})
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
	_, _, err := ClaimExact(store, "does-not-exist", ClaimExactPreconditions{}, "", beads.UpdateOpts{})
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
			_, outcome, err := ClaimExact(store, b.ID, ClaimExactPreconditions{}, "", beads.UpdateOpts{Assignee: strp(assignee)})
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

// raceOnFirstUpdateStore wraps a *beads.MemStore and, on the first call to
// Update for the watched bead, injects a second full ClaimExact-shaped claim
// (its own generation CAS plus its own unconditional effects write) before
// letting the real caller's Update proceed. It simulates a second caller that
// observed the just-advanced generation and raced in during the window
// between this call's CAS and its own effects write.
type raceOnFirstUpdateStore struct {
	*beads.MemStore
	targetID     string
	racedFromGen string
	racedToGen   string
	raced        bool
}

func (s *raceOnFirstUpdateStore) Update(id string, opts beads.UpdateOpts) error {
	if !s.raced && id == s.targetID {
		s.raced = true
		outcome, err := beads.ApplyMetadataCAS(s.MemStore, id, beadmeta.ClaimGenerationMetadataKey, s.racedFromGen, s.racedToGen)
		if err != nil {
			return fmt.Errorf("test setup: racer CAS: %w", err)
		}
		if outcome != beads.MetadataCASSwapped {
			return fmt.Errorf("test setup: racer CAS did not swap: %s", outcome)
		}
		if err := s.MemStore.Update(id, beads.UpdateOpts{Assignee: strp("worker-racer")}); err != nil {
			return fmt.Errorf("test setup: racer effects write: %w", err)
		}
	}
	return s.MemStore.Update(id, opts)
}

// TestClaimExact_EffectsRaceAfterGenerationCASIsDetected proves the
// documented, narrower guarantee: ClaimExact cannot make its own CAS-then-
// Update sequence fully atomic, but it does detect (and refuse to falsely
// report ClaimExactClaimed for) the case where a second claimant's CAS lands
// during the window between this call's own CAS and its effects write.
func TestClaimExact_EffectsRaceAfterGenerationCASIsDetected(t *testing.T) {
	inner := beads.NewMemStore()
	b := mustCreateExactClaimBead(t, inner)

	racer := &raceOnFirstUpdateStore{
		MemStore:     inner,
		targetID:     b.ID,
		racedFromGen: "1", // the generation this call's own CAS will advance to
		racedToGen:   "2",
	}

	got, outcome, err := ClaimExact(racer, b.ID, ClaimExactPreconditions{}, "", beads.UpdateOpts{Assignee: strp("worker-A")})
	if !errors.Is(err, ErrClaimEffectsRaced) {
		t.Fatalf("err = %v, want ErrClaimEffectsRaced", err)
	}
	if outcome != "" {
		t.Fatalf("outcome = %q, want empty on race detection", outcome)
	}
	if got.ID == "" {
		t.Fatal("returned bead is the zero value; caller cannot inspect the raced state")
	}
	if got.Metadata[beadmeta.ClaimGenerationMetadataKey] != "2" {
		t.Fatalf("generation = %q, want 2 (the racer's own further advance)", got.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
}
