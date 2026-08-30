package beadstest

import (
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// ConditionalWriterOptions controls optional legs of the ConditionalWriter
// conformance suite that not every store can express.
type ConditionalWriterOptions struct {
	// RowBackedMutationFlavors declares that direct whole-row mutation flavors
	// expose their fresh revision through the Store read surface. MemStore,
	// FileStore, and real BdStore do; wrappers whose cache projection cannot
	// preserve every unconditional flavor leave this false.
	RowBackedMutationFlavors bool

	// RestrictedUpdateFields declares that this store persists parent and
	// labels through writes it cannot fold into a revision-guarded update, so
	// UpdateIfMatch must reject those options with
	// *beads.ConditionalUpdateFieldUnsupportedError instead of applying them.
	// bd-backed and Dolt-backed stores do; a store that keeps the whole bead in
	// one row can apply them and leaves this false.
	RestrictedUpdateFields bool

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
//
// The body is a flat table of contents: each contract leg lives in its own
// conformance* helper so this dispatcher stays a readable manifest of the
// suite, and the optional legs (opts.RestrictedUpdateFields,
// opts.RowBackedMutationFlavors, opts.OpenDisabled) are the only branches here.
func RunConditionalWriterConformanceWithOptions(t *testing.T, name string, open func(t *testing.T) beads.Store, opts ConditionalWriterOptions) {
	t.Helper()

	t.Run(name, func(t *testing.T) { runEmptyUpdateContract(t, open) })
	conformanceWholeBeadWriteChangesRevision(t, name, open)
	if opts.RestrictedUpdateFields {
		conformanceRestrictedUpdateFieldsRejected(t, name, open)
	}
	conformanceReadsNeverBump(t, name, open)
	conformanceRevisionTokensNeverReused(t, name, open)
	conformanceReleaseIfCurrentMintsRevision(t, name, open)
	if opts.RowBackedMutationFlavors {
		conformanceRowBackedMutationFlavors(t, name, open)
	}
	conformanceStaleRevisionIsPreconditionFailed(t, name, open, opts)
	conformanceConditionalSuccessPaths(t, name, open)
	conformanceCASEmptyExpectedClaimsAbsentOrEmpty(t, name, open)
	conformanceCASValueMismatchIsFalseNil(t, name, open)
	conformanceCASWinnerValueVisibleToLoser(t, name, open)
	conformanceUpdateIfMatchContentionPair(t, name, open)
	conformanceContention(t, name, open)
	if opts.OpenDisabled != nil {
		conformanceDisableToggle(t, name, opts)
	}
}

// conformanceWriterFor resolves the ConditionalWriter for a store or fails
// loudly: the suite is only meaningful against a capable store.
func conformanceWriterFor(t *testing.T, s beads.Store) beads.ConditionalWriter {
	t.Helper()
	w, ok := beads.ConditionalWriterFor(s)
	if !ok {
		t.Fatalf("store does not implement beads.ConditionalWriter; "+
			"RunConditionalWriterConformance requires a capable store (got %T)", s)
	}
	return w
}

// conformanceRevOf reads the current revision of id through the plain Store
// surface.
func conformanceRevOf(t *testing.T, s beads.Store, id string) int64 {
	t.Helper()
	b, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return b.Revision
}

// conformanceStrPtr returns a pointer to s, for the *string UpdateOpts fields.
func conformanceStrPtr(s string) *string { return &s }

// conformanceWholeBeadWriteChangesRevision asserts a conditional whole-bead
// write moves the revision to a fresh nonzero token.
func conformanceWholeBeadWriteChangesRevision(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/whole_bead_conditional_write_changes_revision", func(t *testing.T) {
		s := open(t)
		w := conformanceWriterFor(t, s)
		b, err := s.Create(beads.Bead{Title: "orig"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID

		before := conformanceRevOf(t, s, id)
		if err := w.UpdateIfMatch(id, before, beads.UpdateOpts{Title: conformanceStrPtr("renamed")}); err != nil {
			t.Fatalf("UpdateIfMatch: %v", err)
		}
		after := conformanceRevOf(t, s, id)
		if after == 0 || after == before {
			t.Fatalf("UpdateIfMatch did not change to a nonzero revision: %d -> %d", before, after)
		}
	})
}

// conformanceRestrictedUpdateFieldsRejected asserts a store that cannot fold
// parent/labels into a revision-guarded update rejects them with the typed
// unsupported error and mutates nothing.
func conformanceRestrictedUpdateFieldsRejected(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/restricted_update_fields_are_rejected_without_mutation", func(t *testing.T) {
		s := open(t)
		w := conformanceWriterFor(t, s)
		parentBefore, err := s.Create(beads.Bead{Title: "restricted-update-parent"})
		if err != nil {
			t.Fatalf("Create parent fixture: %v", err)
		}
		parent := "parent-after"
		tests := []struct {
			name string
			opts beads.UpdateOpts
		}{
			{name: "parent", opts: beads.UpdateOpts{ParentID: &parent}},
			{name: "add_labels", opts: beads.UpdateOpts{Labels: []string{"added"}}},
			{name: "remove_labels", opts: beads.UpdateOpts{RemoveLabels: []string{"remove"}}},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				created, err := s.Create(beads.Bead{
					Title:    "restricted-update",
					ParentID: parentBefore.ID,
					Labels:   []string{"keep", "remove"},
				})
				if err != nil {
					t.Fatal(err)
				}
				before, err := s.Get(created.ID)
				if err != nil {
					t.Fatal(err)
				}

				err = w.UpdateIfMatch(created.ID, before.Revision, tt.opts)
				var unsupported *beads.ConditionalUpdateFieldUnsupportedError
				if !errors.As(err, &unsupported) {
					t.Fatalf("UpdateIfMatch(%s) = %v, want *ConditionalUpdateFieldUnsupportedError", tt.name, err)
				}
				after, err := s.Get(created.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("restricted conditional update mutated bead: before=%#v after=%#v", before, after)
				}
			})
		}
	})
}

// conformanceReadsNeverBump asserts a spread of read paths leaves the revision
// unchanged.
func conformanceReadsNeverBump(t *testing.T, name string, open func(t *testing.T) beads.Store) {
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
		before := conformanceRevOf(t, s, id)

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

		if after := conformanceRevOf(t, s, id); after != before {
			t.Fatalf("reads bumped the revision: %d -> %d", before, after)
		}
	})
}

// conformanceRevisionTokensNeverReused asserts each mutation mints a nonzero
// token never seen before in the bead's lifetime.
func conformanceRevisionTokensNeverReused(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/revision_tokens_are_never_reused_during_a_bead_lifetime", func(t *testing.T) {
		s := open(t)
		b, err := s.Create(beads.Bead{Title: "mono"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID

		// A never-mutated bead may legitimately carry the zero token on a
		// counter-backed store, so seed the seen set only from a real token.
		last := conformanceRevOf(t, s, id)
		seen := map[int64]struct{}{}
		if last != 0 {
			seen[last] = struct{}{}
		}
		for i := 0; i < 8; i++ {
			if err := s.SetMetadata(id, "counter", string(rune('a'+i))); err != nil {
				t.Fatal(err)
			}
			cur := conformanceRevOf(t, s, id)
			if cur == 0 || cur == last {
				t.Fatalf("revision did not change to a nonzero token at step %d: %d -> %d", i, last, cur)
			}
			if _, exists := seen[cur]; exists {
				t.Fatalf("revision token %d was reused at step %d", cur, i)
			}
			seen[cur] = struct{}{}
			last = cur
		}
	})
}

// conformanceReleaseIfCurrentMintsRevision asserts ReleaseIfCurrent no-ops for a
// wrong assignee, releases for the matching one, and mints a fresh token that
// stales the pre-release snapshot.
func conformanceReleaseIfCurrentMintsRevision(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/release_if_current_mints_revision_and_stales_prior_token", func(t *testing.T) {
		s := open(t)
		w := conformanceWriterFor(t, s)
		releaser, ok := s.(beads.ConditionalAssignmentReleaser)
		if !ok {
			t.Fatalf("%T does not implement ConditionalAssignmentReleaser", s)
		}
		created, err := s.Create(beads.Bead{
			Title:    "release-fence",
			Assignee: "worker-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		inProgress := "in_progress"
		if err := s.Update(created.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatal(err)
		}
		before, err := s.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}

		released, err := releaser.ReleaseIfCurrent(created.ID, "worker-2")
		if err != nil {
			t.Fatalf("ReleaseIfCurrent wrong assignee: %v", err)
		}
		if released {
			t.Fatal("ReleaseIfCurrent wrong assignee = true, want false")
		}
		afterNoop, err := s.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if afterNoop.Revision != before.Revision {
			t.Fatalf("no-op ReleaseIfCurrent minted revision %d -> %d", before.Revision, afterNoop.Revision)
		}

		released, err = releaser.ReleaseIfCurrent(created.ID, "worker-1")
		if err != nil {
			t.Fatalf("ReleaseIfCurrent matching assignee: %v", err)
		}
		if !released {
			t.Fatal("ReleaseIfCurrent matching assignee = false, want true")
		}
		afterRelease, err := s.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if afterRelease.Revision == 0 || afterRelease.Revision == before.Revision {
			t.Fatalf("ReleaseIfCurrent did not mint a fresh nonzero revision: %d -> %d", before.Revision, afterRelease.Revision)
		}
		if afterRelease.Status != "open" || afterRelease.Assignee != "" {
			t.Fatalf("released bead = %+v, want open and unassigned", afterRelease)
		}

		title := "stale overwrite"
		err = w.UpdateIfMatch(created.ID, before.Revision, beads.UpdateOpts{Title: &title})
		if !beads.IsPreconditionFailed(err) {
			t.Fatalf("UpdateIfMatch with pre-release revision = %v, want precondition failure", err)
		}
		final, err := s.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Title != "release-fence" {
			t.Fatalf("stale pre-release write changed title to %q", final.Title)
		}
	})
}

// conformanceRowBackedMutationFlavors asserts every whole-row mutation flavor
// (Update fields, SetMetadata(Batch), the *IfMatch verbs, CAS) mints a fresh
// nonzero token visible through the Store read surface.
func conformanceRowBackedMutationFlavors(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/row_backed_mutation_flavors_mint_fresh_tokens", func(t *testing.T) {
		s := open(t)
		w := conformanceWriterFor(t, s)
		strPtr := func(s string) *string { return &s }
		row, err := s.Create(beads.Bead{Title: "row-backed-mutations"})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[int64]struct{}{conformanceRevOf(t, s, row.ID): {}}
		priority := 2

		assertFresh := func(label string, mutate func() error) {
			t.Helper()
			before := conformanceRevOf(t, s, row.ID)
			if err := mutate(); err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			after := conformanceRevOf(t, s, row.ID)
			if after == 0 || after == before {
				t.Fatalf("%s did not mint a fresh nonzero revision: %d -> %d", label, before, after)
			}
			if _, exists := seen[after]; exists {
				t.Fatalf("%s reused revision token %d", label, after)
			}
			seen[after] = struct{}{}
		}

		updates := []struct {
			name string
			opts beads.UpdateOpts
		}{
			{name: "Update(title)", opts: beads.UpdateOpts{Title: strPtr("updated")}},
			{name: "Update(status)", opts: beads.UpdateOpts{Status: strPtr("in_progress")}},
			{name: "Update(type)", opts: beads.UpdateOpts{Type: strPtr("task")}},
			{name: "Update(priority)", opts: beads.UpdateOpts{Priority: &priority}},
			{name: "Update(description)", opts: beads.UpdateOpts{Description: strPtr("description")}},
			{name: "Update(assignee)", opts: beads.UpdateOpts{Assignee: strPtr("agent")}},
			{name: "Update(metadata)", opts: beads.UpdateOpts{Metadata: map[string]string{"update_metadata": "value"}}},
		}
		for _, update := range updates {
			assertFresh(update.name, func() error { return s.Update(row.ID, update.opts) })
		}

		assertFresh("SetMetadata", func() error {
			return s.SetMetadata(row.ID, "key", "value")
		})

		assertFresh("SetMetadataBatch", func() error {
			return s.SetMetadataBatch(row.ID, map[string]string{"left": "one", "right": "two"})
		})

		assertFresh("UpdateIfMatch", func() error {
			return w.UpdateIfMatch(row.ID, conformanceRevOf(t, s, row.ID), beads.UpdateOpts{Title: strPtr("conditional-updated")})
		})

		assertFresh("CompareAndSetMetadataKey", func() error {
			ok, err := w.CompareAndSetMetadataKey(row.ID, "conditional_metadata", "", "value")
			if err != nil {
				return err
			}
			if !ok {
				t.Fatal("CompareAndSetMetadataKey claiming an absent key returned (false, nil)")
			}
			return nil
		})

		assertFresh("Close", func() error { return s.Close(row.ID) })
		assertFresh("Reopen", func() error { return s.Reopen(row.ID) })

		conditionalClose, err := s.Create(beads.Bead{Title: "conditional-close"})
		if err != nil {
			t.Fatal(err)
		}
		before := conformanceRevOf(t, s, conditionalClose.ID)
		if err := w.CloseIfMatch(conditionalClose.ID, before); err != nil {
			t.Fatalf("CloseIfMatch: %v", err)
		}
		after := conformanceRevOf(t, s, conditionalClose.ID)
		if after == 0 || after == before {
			t.Fatalf("CloseIfMatch did not mint a fresh nonzero revision: %d -> %d", before, after)
		}
	})
}

// conformanceStaleRevisionIsPreconditionFailed asserts each *IfMatch verb
// rejects a stale revision with a typed PreconditionFailedError carrying the
// caller's Expected (and Current when opts.SuppliesCurrent) without mutating.
func conformanceStaleRevisionIsPreconditionFailed(t *testing.T, name string, open func(t *testing.T) beads.Store, opts ConditionalWriterOptions) {
	t.Run(name+"/stale_revision_is_precondition_failed", func(t *testing.T) {
		s := open(t)
		w := conformanceWriterFor(t, s)
		b, err := s.Create(beads.Bead{Title: "stale"})
		if err != nil {
			t.Fatal(err)
		}
		id := b.ID
		stale := conformanceRevOf(t, s, id)
		// Move the revision on so the caller's snapshot is out of date.
		if err := s.SetMetadata(id, "k", "moved"); err != nil {
			t.Fatal(err)
		}
		current := conformanceRevOf(t, s, id)

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

		assertPrecondition("UpdateIfMatch", w.UpdateIfMatch(id, stale, beads.UpdateOpts{Title: conformanceStrPtr("x")}))
		assertPrecondition("CloseIfMatch", w.CloseIfMatch(id, stale))
		assertPrecondition("DeleteIfMatch", w.DeleteIfMatch(id, stale))
		// The bead must still exist (every stale write was rejected).
		if _, err := s.Get(id); err != nil {
			t.Fatalf("bead vanished after rejected conditional writes: %v", err)
		}
	})
}

// conformanceConditionalSuccessPaths asserts the matching-revision (success) leg
// of each *IfMatch verb actually applies and removes, so a store whose gated
// verbs always fail — or return nil without applying — cannot pass by omission.
func conformanceConditionalSuccessPaths(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/conditional_success_paths", func(t *testing.T) {
		// The matching-revision (success) leg of each *IfMatch verb — without this,
		// a store whose gated verbs always return PreconditionFailedError, or that
		// returns nil without applying anything, passes the whole suite.
		s := open(t)
		w := conformanceWriterFor(t, s)

		// UpdateIfMatch at the current revision applies opts and bumps.
		a, err := s.Create(beads.Bead{Title: "upd"})
		if err != nil {
			t.Fatal(err)
		}
		aRev := conformanceRevOf(t, s, a.ID)
		if err := w.UpdateIfMatch(a.ID, aRev, beads.UpdateOpts{Title: conformanceStrPtr("applied")}); err != nil {
			t.Fatalf("UpdateIfMatch at current revision: %v", err)
		}
		got, err := s.Get(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "applied" {
			t.Fatalf("UpdateIfMatch did not apply opts: title = %q, want %q", got.Title, "applied")
		}
		if got.Revision == 0 || got.Revision == aRev {
			t.Fatalf("UpdateIfMatch did not change to a nonzero revision: %d -> %d", aRev, got.Revision)
		}

		// CloseIfMatch at the current revision succeeds.
		b, err := s.Create(beads.Bead{Title: "cls"})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.CloseIfMatch(b.ID, conformanceRevOf(t, s, b.ID)); err != nil {
			t.Fatalf("CloseIfMatch at current revision: %v", err)
		}

		// DeleteIfMatch at the current revision removes the bead.
		c, err := s.Create(beads.Bead{Title: "del"})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.DeleteIfMatch(c.ID, conformanceRevOf(t, s, c.ID)); err != nil {
			t.Fatalf("DeleteIfMatch at current revision: %v", err)
		}
		if _, err := s.Get(c.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("DeleteIfMatch left the bead present: Get returned %v, want ErrNotFound", err)
		}
	})
}

// conformanceCASEmptyExpectedClaimsAbsentOrEmpty asserts CompareAndSetMetadataKey
// with an empty expected value claims an absent or empty-valued key but not a
// populated one.
func conformanceCASEmptyExpectedClaimsAbsentOrEmpty(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/cas_empty_expected_claims_absent_or_empty_only", func(t *testing.T) {
		s := open(t)
		w := conformanceWriterFor(t, s)
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
}

// conformanceCASValueMismatchIsFalseNil asserts a value-mismatched CAS returns
// (false, nil) — a lost race, not an error — and mutates nothing.
func conformanceCASValueMismatchIsFalseNil(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/cas_value_mismatch_is_false_nil_not_error", func(t *testing.T) {
		s := open(t)
		w := conformanceWriterFor(t, s)
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
}

// conformanceCASWinnerValueVisibleToLoser asserts a CAS winner's value is
// visible to a loser's re-read and that a CAS from the pre-swap value then loses
// cleanly.
func conformanceCASWinnerValueVisibleToLoser(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/cas_winner_value_visible_to_loser_reread", func(t *testing.T) {
		s := open(t)
		w := conformanceWriterFor(t, s)
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
}

// conformanceUpdateIfMatchContentionPair asserts that under contention exactly
// one UpdateIfMatch racer commits its complete metadata pair, losers commit
// nothing partial, and an unrelated sibling key is preserved.
func conformanceUpdateIfMatchContentionPair(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/update_if_match_contention_commits_one_complete_metadata_pair", func(t *testing.T) {
		s := open(t)
		w := conformanceWriterFor(t, s)
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
		if after.Revision == 0 || after.Revision == before.Revision {
			t.Fatalf("sole successful UpdateIfMatch did not change to a nonzero revision: %d -> %d", before.Revision, after.Revision)
		}
	})
}

// conformanceContention asserts that under contention exactly one
// CompareAndSetMetadataKey racer wins (never an error) and the store persists
// precisely that winner's value.
func conformanceContention(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/contention", func(t *testing.T) {
		s := open(t)
		w := conformanceWriterFor(t, s)
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
}

// conformanceDisableToggle asserts an instance whose conditional writes are
// disabled returns ErrConditionalWriteUnsupported from the four CAS methods
// while still claiming the interface and keeping its other optional interfaces.
func conformanceDisableToggle(t *testing.T, name string, opts ConditionalWriterOptions) {
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
		assertUnsupported("UpdateIfMatch", w.UpdateIfMatch(id, 1, beads.UpdateOpts{Title: conformanceStrPtr("x")}))
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
