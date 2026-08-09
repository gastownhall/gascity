//go:build integration

package beads_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBdStoreReleaseIfCurrentAgainstRealBd executes the conditional-release CAS
// against a REAL bd binary in EMBEDDED mode — the mode where `bd sql` is
// rejected outright ("'bd sql' is not yet supported in embedded mode"), so the
// raw-SQL path this conversion replaces could only reach it by shelling
// separately to `dolt sql`.
//
// It is the authoritative guard for the two facts the unit tests can only
// assume about bd: that the flag spelling still exists, and that a rejected
// precondition is exit 13 with nothing written. A bd that renames the flags
// fails the capability leg here; a bd that changes the exit status fails the
// mismatch leg, loudly, instead of silently degrading gascity to "someone else
// holds it" on every release.
func TestBdStoreReleaseIfCurrentAgainstRealBd(t *testing.T) {
	store, _ := newConditionalIntegrationBdStore(t)

	created, err := store.Create(beads.Bead{Title: "conditional release row", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Update(created.ID, beads.UpdateOpts{
		Status:   strPtr("in_progress"),
		Assignee: strPtr("worker-1"),
	}); err != nil {
		t.Fatalf("Update to in_progress: %v", err)
	}

	assertHeld := func(t *testing.T, wantStatus, wantAssignee string) {
		t.Helper()
		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != wantStatus || got.Assignee != wantAssignee {
			t.Fatalf("state = (%s, %q), want (%s, %q)", got.Status, got.Assignee, wantStatus, wantAssignee)
		}
	}
	assertHeld(t, "in_progress", "worker-1")

	t.Run("a wrong assignee writes nothing and is not an error", func(t *testing.T) {
		released, err := store.ReleaseIfCurrent(created.ID, "worker-2")
		if err != nil {
			// An old bd (pre-#5364) has no flags and falls back to bd sql, which
			// embedded mode rejects; that surfaces as unsupported, not a bug.
			if isUnsupportedOnThisBd(err) {
				t.Skipf("installed bd lacks the conditional-release flags and bd sql is embedded-rejected: %v", err)
			}
			t.Fatalf("ReleaseIfCurrent(wrong assignee): %v", err)
		}
		if released {
			t.Fatal("released a bead held by someone else")
		}
		assertHeld(t, "in_progress", "worker-1")
	})

	t.Run("the matching assignee releases", func(t *testing.T) {
		released, err := store.ReleaseIfCurrent(created.ID, "worker-1")
		if err != nil {
			if isUnsupportedOnThisBd(err) {
				t.Skipf("installed bd lacks the conditional-release flags: %v", err)
			}
			t.Fatalf("ReleaseIfCurrent(matching assignee): %v", err)
		}
		if !released {
			t.Fatal("did not release a matching in-progress assignment")
		}
		assertHeld(t, "open", "")
	})

	t.Run("a second release is a no-op, not an error", func(t *testing.T) {
		released, err := store.ReleaseIfCurrent(created.ID, "worker-1")
		if err != nil {
			if isUnsupportedOnThisBd(err) {
				t.Skipf("installed bd lacks the conditional-release flags: %v", err)
			}
			t.Fatalf("ReleaseIfCurrent(already released): %v", err)
		}
		if released {
			t.Fatal("released an already-open bead")
		}
	})

	// The replaced statement was `UPDATE issues SET ... WHERE id = ...`, so a
	// bead in the EPHEMERAL tier lived in the `wisps` table and matched zero
	// rows — the CAS reported "someone else holds it" for every wisp, forever.
	// bd's verb resolves the id across both tiers, so the conditional release
	// now covers ephemeral work. gascity runs formula steps as wisps, which is
	// where that silent no-op landed.
	t.Run("an ephemeral bead releases too", func(t *testing.T) {
		wisp, err := store.Create(beads.Bead{Title: "ephemeral release row", Type: "task", Ephemeral: true})
		if err != nil {
			t.Fatalf("Create ephemeral: %v", err)
		}
		if err := store.Update(wisp.ID, beads.UpdateOpts{
			Status:   strPtr("in_progress"),
			Assignee: strPtr("worker-1"),
		}); err != nil {
			t.Fatalf("Update ephemeral to in_progress: %v", err)
		}
		released, err := store.ReleaseIfCurrent(wisp.ID, "worker-1")
		if err != nil {
			if isUnsupportedOnThisBd(err) {
				t.Skipf("installed bd lacks the conditional-release flags: %v", err)
			}
			t.Fatalf("ReleaseIfCurrent(ephemeral): %v", err)
		}
		if !released {
			t.Fatal("did not release an ephemeral in-progress assignment")
		}
		got, err := store.Get(wisp.ID)
		if err != nil {
			t.Fatalf("Get ephemeral: %v", err)
		}
		if got.Status != "open" || got.Assignee != "" {
			t.Fatalf("ephemeral state = (%s, %q), want (open, \"\")", got.Status, got.Assignee)
		}
	})

	t.Run("an unresolvable id is not held", func(t *testing.T) {
		released, err := store.ReleaseIfCurrent("tst-nosuchbead", "worker-1")
		if err != nil {
			if isUnsupportedOnThisBd(err) {
				t.Skipf("installed bd lacks the conditional-release flags: %v", err)
			}
			t.Fatalf("ReleaseIfCurrent(missing id): %v", err)
		}
		if released {
			t.Fatal("released a bead that does not exist")
		}
	})
}

// isUnsupportedOnThisBd reports whether err is the old-bd path bottoming out:
// no conditional-release flags AND no usable bd sql (embedded mode rejects it).
// Only that combination is a legitimate skip.
func isUnsupportedOnThisBd(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, beads.ErrConditionalReleaseUnsupported) ||
		strings.Contains(err.Error(), "not yet supported in embedded mode")
}
