package beadstest

import (
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// MetadataCASOptions controls legs of the narrow CAS suite that not every
// TEST FIXTURE can evaluate. Note the distinction from
// ConditionalWriterOptions, whose legs turn on what a STORE can express: the
// contention leg here is a claim about the backend's isolation, so a fixture
// that models a backend without modeling its isolation cannot judge it.
type MetadataCASOptions struct {
	// FixtureLacksIsolationReason, when non-empty, declares that this
	// factory's store cannot evaluate the contention leg because the fixture
	// behind it provides no isolation between concurrent transactions, and
	// records why. The leg is then reported as an explicit, named absence
	// rather than silently dropped — an unevaluatable gate must be visible in
	// the test output, not missing from it.
	//
	// Set this ONLY for a fixture whose non-isolation is a property of the
	// test double, never to quiet a store that genuinely admits two winners:
	// a store that loses the contention leg cannot carry a lease, which is
	// the whole reason callers want this capability. A fixture that opts out
	// here owes the contention property an integration-level test against the
	// real backend.
	FixtureLacksIsolationReason string
}

// RunMetadataCASConformance runs the store-agnostic beads.MetadataCASWriter
// contract suite against a capable store, with every leg enabled.
func RunMetadataCASConformance(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	RunMetadataCASConformanceWithOptions(t, name, open, MetadataCASOptions{})
}

// RunMetadataCASConformanceWithOptions is RunMetadataCASConformance with the
// fixture-dependent contention leg configurable. open must return a fresh,
// empty store that implements beads.MetadataCASWriter (verified via
// beads.MetadataCASWriterFor); name prefixes every subtest so multiple stores
// can run in one package.
//
// The subtests mirror the metadata-CAS legs of
// RunConditionalWriterConformance one-to-one, so a store that can only offer
// the narrow capability is held to exactly the same value-CAS contract as a
// full ConditionalWriter — the two suites can be diffed against each other.
// The revision legs are deliberately absent rather than skipped: a narrow
// store makes no revision claim at all, so there is nothing to assert (see
// the MetadataCASWriter doc comment for why no sound revision token exists at
// beads v1.1.0).
//
// Both contract traps that the in-tree implementations historically diverged
// on ride this suite: empty-expected matching absent OR present-and-empty,
// and a lost race reporting (false, nil) rather than an error.
func RunMetadataCASConformanceWithOptions(t *testing.T, name string, open func(t *testing.T) beads.Store, opts MetadataCASOptions) {
	t.Helper()

	// writerFor resolves the narrow writer or fails loudly: the suite is only
	// meaningful against a capable store.
	writerFor := func(t *testing.T, s beads.Store) beads.MetadataCASWriter {
		t.Helper()
		w, ok := beads.MetadataCASWriterFor(s)
		if !ok {
			t.Fatalf("store does not implement beads.MetadataCASWriter; "+
				"RunMetadataCASConformance requires a capable store (%T)", s)
		}
		return w
	}

	t.Run(name+"/cas_empty_expected_claims_absent_or_empty_only", func(t *testing.T) {
		s := open(t)
		w := writerFor(t, s)
		b, err := s.Create(beads.Bead{Title: "cas-empty"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID

		// Absent key: expected "" claims it.
		if ok, err := w.CompareAndSetMetadataKey(id, "k", "", "one"); err != nil || !ok {
			t.Fatalf("claim absent key: (%v, %v), want (true, nil)", ok, err)
		}
		// Empty-valued key: expected "" also claims it (the two states are
		// indistinguishable to callers).
		if err := s.SetMetadata(id, "k", ""); err != nil {
			t.Fatal(err)
		}
		if ok, err := w.CompareAndSetMetadataKey(id, "k", "", "two"); err != nil || !ok {
			t.Fatalf("claim empty-valued key: (%v, %v), want (true, nil)", ok, err)
		}
		// Non-empty key: expected "" must NOT claim it.
		if ok, err := w.CompareAndSetMetadataKey(id, "k", "", "three"); err != nil || ok {
			t.Fatalf("claim non-empty key with empty expected: (%v, %v), want (false, nil)", ok, err)
		}
		if got, _ := s.Get(id); got.Metadata["k"] != "two" {
			t.Fatalf("value after rejected empty-expected CAS = %q, want %q", got.Metadata["k"], "two")
		}
	})

	t.Run(name+"/cas_value_mismatch_is_false_nil_not_error", func(t *testing.T) {
		s := open(t)
		w := writerFor(t, s)
		b, err := s.Create(beads.Bead{Title: "cas-mismatch"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID
		if err := s.SetMetadata(id, "k", "A"); err != nil {
			t.Fatal(err)
		}
		ok, err := w.CompareAndSetMetadataKey(id, "k", "B", "C")
		if err != nil {
			t.Fatalf("value-mismatch CAS returned error: %v (want nil)", err)
		}
		if ok {
			t.Fatal("value-mismatch CAS returned true (want false)")
		}
		if got, _ := s.Get(id); got.Metadata["k"] != "A" {
			t.Fatalf("value mutated on a lost CAS: %q, want %q", got.Metadata["k"], "A")
		}
	})

	t.Run(name+"/cas_winner_value_visible_to_loser_reread", func(t *testing.T) {
		s := open(t)
		w := writerFor(t, s)
		b, err := s.Create(beads.Bead{Title: "cas-visible"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID
		if err := s.SetMetadata(id, "k", "start"); err != nil {
			t.Fatal(err)
		}
		if ok, err := w.CompareAndSetMetadataKey(id, "k", "start", "winner"); err != nil || !ok {
			t.Fatalf("winner CAS: (%v, %v), want (true, nil)", ok, err)
		}
		// A loser re-reads and must observe the winner's value.
		if got, _ := s.Get(id); got.Metadata["k"] != "winner" {
			t.Fatalf("loser re-read = %q, want %q (winner value not visible)", got.Metadata["k"], "winner")
		}
		// And a CAS from the old value now loses cleanly.
		if ok, err := w.CompareAndSetMetadataKey(id, "k", "start", "late"); err != nil || ok {
			t.Fatalf("stale-value CAS after a swap: (%v, %v), want (false, nil)", ok, err)
		}
	})

	t.Run(name+"/cas_does_not_disturb_sibling_keys", func(t *testing.T) {
		s := open(t)
		w := writerFor(t, s)
		b, err := s.Create(beads.Bead{Title: "cas-siblings"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID
		if err := s.SetMetadata(id, "sibling", "preserved"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMetadata(id, "k", "A"); err != nil {
			t.Fatal(err)
		}
		if ok, err := w.CompareAndSetMetadataKey(id, "k", "A", "B"); err != nil || !ok {
			t.Fatalf("CAS: (%v, %v), want (true, nil)", ok, err)
		}
		got, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Metadata["sibling"] != "preserved" {
			t.Fatalf("sibling metadata = %q, want %q (read-modify-write dropped it)", got.Metadata["sibling"], "preserved")
		}
		if got.Metadata["k"] != "B" {
			t.Fatalf("target metadata = %q, want %q", got.Metadata["k"], "B")
		}
	})

	// The exclusion property the lease/claim callers actually depend on:
	// under concurrency exactly ONE racer may win a claim from a single
	// starting value. A store that reports two winners has no mutual
	// exclusion and cannot carry a lease, however well it passes the
	// sequential legs above.
	t.Run(name+"/cas_contention_admits_exactly_one_winner", func(t *testing.T) {
		if reason := opts.FixtureLacksIsolationReason; reason != "" {
			// Named, visible absence: the store is not being excused, the
			// FIXTURE cannot evaluate the claim. The property is owed an
			// integration test against the real backend.
			t.Skipf("fixture cannot evaluate contention: %s", reason)
		}
		s := open(t)
		w := writerFor(t, s)
		b, err := s.Create(beads.Bead{Title: "cas-contention"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID
		if err := s.SetMetadata(id, "lease", ""); err != nil {
			t.Fatal(err)
		}

		const racers = 8
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			winners []string
			errs    []error
		)
		start := make(chan struct{})
		for i := range racers {
			wg.Add(1)
			go func(racer int) {
				defer wg.Done()
				holder := "holder-" + strconv.Itoa(racer)
				<-start
				ok, err := w.CompareAndSetMetadataKey(id, "lease", "", holder)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				if ok {
					winners = append(winners, holder)
				}
			}(i)
		}
		close(start)
		wg.Wait()

		for _, err := range errs {
			t.Fatalf("racer returned an error (a lost race must be (false, nil)): %v", err)
		}
		if len(winners) != 1 {
			t.Fatalf("winners = %d %v, want exactly 1 (no mutual exclusion)", len(winners), winners)
		}
		got, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Metadata["lease"] != winners[0] {
			t.Fatalf("stored lease = %q, want the sole winner %q", got.Metadata["lease"], winners[0])
		}
	})
}
