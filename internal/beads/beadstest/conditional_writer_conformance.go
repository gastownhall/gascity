package beadstest

import (
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// ConditionalWriterOptions controls optional legs of the ConditionalWriter
// conformance suite that not every store can express.
type ConditionalWriterOptions struct {
	// OpenDisabled returns a fresh store of the same kind whose conditional
	// writes are turned off at the instance level (e.g. MemStore/FileStore with
	// DisableConditionalWrites=true). When non-nil, the disable_toggle subtest
	// asserts the four CAS methods return beads.ErrConditionalWriteUnsupported
	// while the store's other optional interfaces stay intact. When nil the
	// subtest does not run — a store with no instance toggle (BdStore latches
	// instead) legitimately has nothing to assert here, so this is an absent
	// leg, not a skipped one (no ledger entry needed).
	OpenDisabled func(t *testing.T) beads.Store

	// SuppliesCurrent declares that this store populates
	// PreconditionFailedError.Current (the live revision) on a stale write. The
	// stale_revision subtest then asserts Current equals the live revision.
	// Stores that cannot recover the live revision from the backend (some bd
	// error bodies) leave it false; Expected is always asserted regardless — it
	// is the caller's own argument, which every implementation has in hand.
	SuppliesCurrent bool
}

// RunConditionalWriterConformance runs the store-agnostic ConditionalWriter
// contract suite against a capable store. open must return a fresh, empty store
// that implements beads.ConditionalWriter (verified via beads.ConditionalWriterFor);
// name prefixes every subtest so multiple stores can run in one package.
//
// The suite mirrors the revision + granularity contract on beads.ConditionalWriter
// one-to-one (a reviewer can diff the subtest list against the doc comment) and
// exercises ONLY the caller-visible result surface — no subtest asserts
// cross-key interference timing, which the granularity contract leaves undefined
// (so BdStore's --if-revision emulation and sqlite's value-CAS both pass the same
// table).
func RunConditionalWriterConformance(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	RunConditionalWriterConformanceWithOptions(t, name, open, ConditionalWriterOptions{})
}

// RunConditionalWriterConformanceWithOptions is RunConditionalWriterConformance
// with the optional disable-toggle leg wired.
func RunConditionalWriterConformanceWithOptions(t *testing.T, name string, open func(t *testing.T) beads.Store, opts ConditionalWriterOptions) {
	t.Helper()

	// writerFor resolves the ConditionalWriter for a store or fails loudly: the
	// suite is only meaningful against a capable store.
	writerFor := func(t *testing.T, s beads.Store) beads.ConditionalWriter {
		t.Helper()
		w, ok := beads.ConditionalWriterFor(s)
		if !ok {
			t.Fatalf("store does not implement beads.ConditionalWriter; "+
				"RunConditionalWriterConformance requires a capable store (got %T)", s)
		}
		return w
	}

	// revOf reads the current revision of id through the plain Store surface.
	revOf := func(t *testing.T, s beads.Store, id string) int64 {
		t.Helper()
		b, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
		return b.Revision
	}

	strPtr := func(s string) *string { return &s }

	t.Run(name, func(t *testing.T) { runEmptyUpdateContract(t, open) })

	t.Run(name+"/every_mutation_bumps_revision", func(t *testing.T) {
		s := open(t)
		w := writerFor(t, s)
		b, err := s.Create(beads.Bead{Title: "orig"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID

		prev := revOf(t, s, id)
		bump := func(label string, mutate func() error) {
			t.Helper()
			if err := mutate(); err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			cur := revOf(t, s, id)
			if cur <= prev {
				t.Fatalf("%s did not bump revision: %d -> %d (want strictly greater)", label, prev, cur)
			}
			prev = cur
		}

		// Every UpdateOpts field flavor is exercised separately: MemStore bumps
		// once regardless, but BdStore fans different opts to different bd
		// subcommands, so a per-field missed bump is exactly what this catches.
		bump("Update(title)", func() error { return s.Update(id, beads.UpdateOpts{Title: strPtr("renamed")}) })
		bump("Update(labels)", func() error { return s.Update(id, beads.UpdateOpts{Labels: []string{"alpha"}}) })
		bump("Update(status)", func() error { return s.Update(id, beads.UpdateOpts{Status: strPtr("in_progress")}) })
		bump("Update(description)", func() error { return s.Update(id, beads.UpdateOpts{Description: strPtr("desc")}) })
		bump("Update(priority)", func() error { p := 2; return s.Update(id, beads.UpdateOpts{Priority: &p}) })
		bump("Update(metadata-opt)", func() error { return s.Update(id, beads.UpdateOpts{Metadata: map[string]string{"mo": "1"}}) })
		bump("Update(removeLabels)", func() error { return s.Update(id, beads.UpdateOpts{RemoveLabels: []string{"alpha"}}) })
		bump("SetMetadata", func() error { return s.SetMetadata(id, "k", "v") })
		bump("Update(assignee)", func() error { return s.Update(id, beads.UpdateOpts{Assignee: strPtr("agent")}) })
		// Close then Reopen. Isolate each verb's bump where the store allows a Get
		// on a closed bead (MemStore, FileStore, bd); for stores that return
		// ErrNotFound from Get on a closed bead (CachingStore), fall back to
		// asserting only that the Close+Reopen pair bumped.
		if err := s.Close(id); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if closed, err := s.Get(id); err == nil {
			if closed.Revision <= prev {
				t.Fatalf("Close did not bump revision: %d -> %d", prev, closed.Revision)
			}
			prev = closed.Revision
		}
		bump("Reopen", func() error { return s.Reopen(id) })
		bump("CompareAndSetMetadataKey", func() error {
			ok, err := w.CompareAndSetMetadataKey(id, "casKey", "", "first")
			if err != nil {
				return err
			}
			if !ok {
				t.Fatal("CompareAndSetMetadataKey claiming an absent key returned (false, nil)")
			}
			return nil
		})
	})

	t.Run(name+"/reads_never_bump", func(t *testing.T) {
		s := open(t)
		b, err := s.Create(beads.Bead{Title: "read-target", Labels: []string{"l"}})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID
		if err := s.SetMetadata(id, "k", "v"); err != nil {
			t.Fatal(err)
		}
		before := revOf(t, s, id)

		// A spread of read paths — none may bump the revision.
		if _, err := s.Get(id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.List(beads.ListQuery{AllowScan: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListByMetadata(map[string]string{"k": "v"}, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Children(id); err != nil {
			t.Fatal(err)
		}

		if after := revOf(t, s, id); after != before {
			t.Fatalf("reads bumped the revision: %d -> %d", before, after)
		}
	})

	t.Run(name+"/revision_monotonic_never_reused", func(t *testing.T) {
		s := open(t)
		b, err := s.Create(beads.Bead{Title: "mono"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID

		seen := map[int64]bool{}
		last := revOf(t, s, id)
		seen[last] = true
		for i := 0; i < 8; i++ {
			if err := s.SetMetadata(id, "counter", string(rune('a'+i))); err != nil {
				t.Fatal(err)
			}
			cur := revOf(t, s, id)
			if cur <= last {
				t.Fatalf("revision not monotonic at step %d: %d -> %d", i, last, cur)
			}
			if seen[cur] {
				t.Fatalf("revision %d reused at step %d", cur, i)
			}
			seen[cur] = true
			last = cur
		}
	})

	t.Run(name+"/stale_revision_is_precondition_failed", func(t *testing.T) {
		s := open(t)
		w := writerFor(t, s)
		b, err := s.Create(beads.Bead{Title: "stale"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID
		stale := revOf(t, s, id)
		// Move the revision on so the caller's snapshot is out of date.
		if err := s.SetMetadata(id, "k", "moved"); err != nil {
			t.Fatal(err)
		}
		current := revOf(t, s, id)

		assertPrecondition := func(verb string, err error) {
			t.Helper()
			var pfe *beads.PreconditionFailedError
			if !errors.As(err, &pfe) {
				t.Fatalf("%s with stale revision: got %v, want *PreconditionFailedError", verb, err)
			}
			// Expected is the caller's own argument — every store has it in hand,
			// so it is asserted unconditionally (a zero here is a real regression).
			if pfe.Expected != stale {
				t.Fatalf("%s: PreconditionFailedError.Expected = %d, want %d (the stale revision)", verb, pfe.Expected, stale)
			}
			// Current is asserted only for stores that declare they supply it, so
			// a store that regressed to Current=0 cannot pass by omission.
			if opts.SuppliesCurrent && pfe.Current != current {
				t.Fatalf("%s: PreconditionFailedError.Current = %d, want %d", verb, pfe.Current, current)
			}
		}

		assertPrecondition("UpdateIfMatch", w.UpdateIfMatch(id, stale, beads.UpdateOpts{Title: strPtr("x")}))
		assertPrecondition("CloseIfMatch", w.CloseIfMatch(id, stale))
		assertPrecondition("DeleteIfMatch", w.DeleteIfMatch(id, stale))
		// The bead must still exist (every stale write was rejected).
		if _, err := s.Get(id); err != nil {
			t.Fatalf("bead vanished after rejected conditional writes: %v", err)
		}
	})

	t.Run(name+"/conditional_success_paths", func(t *testing.T) {
		// The matching-revision (success) leg of each *IfMatch verb — without this,
		// a store whose gated verbs always return PreconditionFailedError, or that
		// returns nil without applying anything, passes the whole suite.
		s := open(t)
		w := writerFor(t, s)

		// UpdateIfMatch at the current revision applies opts and bumps.
		a, err := s.Create(beads.Bead{Title: "upd"})
		if err != nil {
			t.Fatal(err)
		}
		aRev := revOf(t, s, a.ID)
		if err := w.UpdateIfMatch(a.ID, aRev, beads.UpdateOpts{Title: strPtr("applied")}); err != nil {
			t.Fatalf("UpdateIfMatch at current revision: %v", err)
		}
		got, err := s.Get(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "applied" {
			t.Fatalf("UpdateIfMatch did not apply opts: title = %q, want %q", got.Title, "applied")
		}
		if got.Revision <= aRev {
			t.Fatalf("UpdateIfMatch did not bump revision: %d -> %d", aRev, got.Revision)
		}

		// CloseIfMatch at the current revision succeeds.
		b, err := s.Create(beads.Bead{Title: "cls"})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.CloseIfMatch(b.ID, revOf(t, s, b.ID)); err != nil {
			t.Fatalf("CloseIfMatch at current revision: %v", err)
		}

		// DeleteIfMatch at the current revision removes the bead.
		c, err := s.Create(beads.Bead{Title: "del"})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.DeleteIfMatch(c.ID, revOf(t, s, c.ID)); err != nil {
			t.Fatalf("DeleteIfMatch at current revision: %v", err)
		}
		if _, err := s.Get(c.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("DeleteIfMatch left the bead present: Get returned %v, want ErrNotFound", err)
		}
	})

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

	t.Run(name+"/update_if_match_contention_commits_one_complete_metadata_pair", func(t *testing.T) {
		s := open(t)
		w := writerFor(t, s)
		b, err := s.Create(beads.Bead{Title: "fenced-metadata-pair"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID
		if err := s.SetMetadata(id, "sibling", "preserved"); err != nil {
			t.Fatal(err)
		}
		before, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}

		const racers = 16
		type updateResult struct {
			racer int
			err   error
		}
		results := make(chan updateResult, racers)
		start := make(chan struct{})
		var ready, done sync.WaitGroup
		ready.Add(racers)
		done.Add(racers)
		for i := 0; i < racers; i++ {
			go func(racer int) {
				defer done.Done()
				ready.Done()
				<-start
				value := strconv.Itoa(racer)
				results <- updateResult{
					racer: racer,
					err: w.UpdateIfMatch(id, before.Revision, beads.UpdateOpts{Metadata: map[string]string{
						"pair_left_" + value:  "left-" + value,
						"pair_right_" + value: "right-" + value,
					}}),
				}
			}(i)
		}
		ready.Wait()
		close(start)
		done.Wait()
		close(results)

		winner := -1
		for result := range results {
			switch {
			case result.err == nil:
				if winner != -1 {
					t.Fatalf("multiple UpdateIfMatch winners: racers %d and %d", winner, result.racer)
				}
				winner = result.racer
			case !beads.IsPreconditionFailed(result.err):
				t.Fatalf("losing racer %d returned %v, want PreconditionFailedError", result.racer, result.err)
			}
		}
		if winner == -1 {
			t.Fatal("no UpdateIfMatch racer won")
		}

		after, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		for racer := 0; racer < racers; racer++ {
			value := strconv.Itoa(racer)
			leftKey := "pair_left_" + value
			rightKey := "pair_right_" + value
			left, hasLeft := after.Metadata[leftKey]
			right, hasRight := after.Metadata[rightKey]
			if racer == winner {
				if want := "left-" + value; !hasLeft || left != want {
					t.Fatalf("winner metadata %s = (%q, %v), want (%q, true)", leftKey, left, hasLeft, want)
				}
				if want := "right-" + value; !hasRight || right != want {
					t.Fatalf("winner metadata %s = (%q, %v), want (%q, true)", rightKey, right, hasRight, want)
				}
				continue
			}
			if hasLeft {
				t.Fatalf("losing racer %d left partial metadata %s=%q", racer, leftKey, left)
			}
			if hasRight {
				t.Fatalf("losing racer %d left partial metadata %s=%q", racer, rightKey, right)
			}
		}
		if got := after.Metadata["sibling"]; got != "preserved" {
			t.Fatalf("unrelated sibling metadata = %q, want %q", got, "preserved")
		}
		if after.Revision == before.Revision {
			t.Fatalf("sole successful UpdateIfMatch did not bump revision %d", before.Revision)
		}
	})

	t.Run(name+"/contention", func(t *testing.T) {
		s := open(t)
		w := writerFor(t, s)
		b, err := s.Create(beads.Bead{Title: "contention"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID
		if err := s.SetMetadata(id, "k", "start"); err != nil {
			t.Fatal(err)
		}

		const racers = 16
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			winners []string
			errs    []error
		)
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			val := "racer-" + strconv.Itoa(i)
			go func(val string) {
				defer wg.Done()
				<-start
				ok, err := w.CompareAndSetMetadataKey(id, "k", "start", val)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					errs = append(errs, err)
				case ok:
					winners = append(winners, val)
				}
			}(val)
		}
		close(start)
		wg.Wait()

		if len(errs) != 0 {
			t.Fatalf("contention must resolve to true/false, not error: %v", errs)
		}
		if len(winners) != 1 {
			t.Fatalf("exactly one racer must win the CAS, got %d winners: %v", len(winners), winners)
		}
		// The store must persist exactly the sole winner's write — a store could
		// otherwise report one winner while committing a loser's value.
		if got, _ := s.Get(id); got.Metadata["k"] != winners[0] {
			t.Fatalf("final value %q does not match the sole winner %q", got.Metadata["k"], winners[0])
		}
	})

	if opts.OpenDisabled != nil {
		t.Run(name+"/disable_toggle_returns_typed_unsupported_with_interfaces_intact", func(t *testing.T) {
			s := opts.OpenDisabled(t)
			// The store still CLAIMS the interface — disabling is a runtime toggle,
			// not interface-stripping (no hiding wrapper, per the class_store lesson).
			w, ok := beads.ConditionalWriterFor(s)
			if !ok {
				t.Fatal("disabled store must still implement ConditionalWriter (toggle is runtime, not interface-stripping)")
			}
			b, err := s.Create(beads.Bead{Title: "disabled"})
			if err != nil {
				t.Fatal(err)
			}
			id := b.ID

			assertUnsupported := func(verb string, err error) {
				t.Helper()
				if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
					t.Fatalf("%s on disabled store: got %v, want ErrConditionalWriteUnsupported", verb, err)
				}
			}
			assertUnsupported("UpdateIfMatch", w.UpdateIfMatch(id, 1, beads.UpdateOpts{Title: strPtr("x")}))
			assertUnsupported("CloseIfMatch", w.CloseIfMatch(id, 1))
			assertUnsupported("DeleteIfMatch", w.DeleteIfMatch(id, 1))
			_, casErr := w.CompareAndSetMetadataKey(id, "k", "", "v")
			assertUnsupported("CompareAndSetMetadataKey", casErr)

			// Other optional interfaces stay intact on the disabled store.
			if _, ok := s.(beads.ConditionalAssignmentReleaser); !ok {
				t.Fatal("disabled store lost ConditionalAssignmentReleaser (interface set must stay intact)")
			}
		})
	}
}

// runEmptyUpdateContract asserts the pinned empty-fenced-update contract: an
// UpdateIfMatch with no fields is invalid input on EVERY store — it neither
// evaluates the fence nor bumps the revision.
func runEmptyUpdateContract(t *testing.T, open func(t *testing.T) beads.Store) {
	t.Run("empty_update_opts_is_invalid_and_never_bumps", func(t *testing.T) {
		store := open(t)
		writer, ok := beads.ConditionalWriterFor(store)
		if !ok {
			t.Fatal("store lost ConditionalWriter")
		}
		created, err := store.Create(beads.Bead{Title: "empty-opts"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		before, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		err = writer.UpdateIfMatch(created.ID, before.Revision, beads.UpdateOpts{})
		if !errors.Is(err, beads.ErrEmptyConditionalUpdate) {
			t.Fatalf("empty fenced update = %v, want ErrEmptyConditionalUpdate", err)
		}
		after, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get after: %v", err)
		}
		if after.Revision != before.Revision {
			t.Fatalf("revision %d -> %d on an invalid empty update, want unchanged", before.Revision, after.Revision)
		}
	})
}
