//go:build integration

package beads_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBdStoreClaimAgainstRealBd runs BdStore.Claim against the pinned bd for
// each answer `bd update <id> --claim` gives, and pins the verdict gc's claim
// path reads from it.
//
// BdStore.Claim tells bd's refusals from its failures by their text
// (isBdClaimConflictMessage), and the unit fixtures in bdstore_test.go only copy
// that text. This row is what fails when a bd bump rewords a refusal. Every
// refusal below must stay a claim this caller did not win (ok=false, nil error)
// and write nothing; otherwise a raced claim drains `gc hook --claim` as
// claims_errored instead of a lost claim. A missing bead must stay ErrNotFound,
// the one error the split-city claim route escalates on.
//
// The scope is embedded, so this drives bd's direct update path. The spelling
// bd's proxied-server path prints ("Error claiming") stays a unit fixture
// (bdNotClaimableRefusal in bdstore_test.go).
func TestBdStoreClaimAgainstRealBd(t *testing.T) {
	setup, scope := newConditionalIntegrationBdStore(t)
	setupRunner := newConditionalIntegrationRunner(t, scope)
	worker := beads.NewBdStore(scope, newConditionalIntegrationRunnerAs(t, scope, "worker-1"))
	rival := beads.NewBdStore(scope, newConditionalIntegrationRunnerAs(t, scope, "worker-2"))
	bd := func(t *testing.T, args ...string) {
		t.Helper()
		if out, err := setupRunner(scope, "bd", args...); err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	create := func(t *testing.T, title string) string {
		t.Helper()
		created, err := setup.Create(beads.Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatalf("Create %q: %v", title, err)
		}
		return created.ID
	}

	t.Run("an open, unassigned bead is claimed", func(t *testing.T) {
		id := create(t, "claimable")
		claimed, ok, err := worker.Claim(id)
		if err != nil || !ok {
			t.Fatalf("Claim(%s) = (ok=%v err=%v), want the claim won", id, ok, err)
		}
		if claimed.Status != "in_progress" || claimed.Assignee != "worker-1" {
			t.Fatalf("claimed bead = (%s, %q), want (in_progress, \"worker-1\")", claimed.Status, claimed.Assignee)
		}
	})

	refusals := []struct {
		name    string
		prepare func(t *testing.T, id string)
	}{
		{"closed", func(t *testing.T, id string) { bd(t, "close", id) }},
		{"deferred", func(t *testing.T, id string) { bd(t, "update", id, "--status", "deferred") }},
		{"blocked status", func(t *testing.T, id string) { bd(t, "update", id, "--status", "blocked") }},
		{"in_progress with no assignee", func(t *testing.T, id string) { bd(t, "update", id, "--status", "in_progress") }},
		{"open and assigned to another actor", func(t *testing.T, id string) { bd(t, "update", id, "--assignee", "worker-2") }},
		{"claimed by another actor", func(t *testing.T, id string) {
			if _, ok, err := rival.Claim(id); err != nil || !ok {
				t.Fatalf("rival Claim(%s) = (ok=%v err=%v), want worker-2 to hold it", id, ok, err)
			}
		}},
	}
	for _, tc := range refusals {
		t.Run("refused when "+tc.name, func(t *testing.T) {
			id := create(t, tc.name)
			tc.prepare(t, id)
			before, err := setup.Get(id)
			if err != nil {
				t.Fatalf("Get(%s) before the claim: %v", id, err)
			}
			_, ok, err := worker.Claim(id)
			if err != nil {
				t.Fatalf("Claim(%s) error = %v, want nil: bd refused the claim, and a refusal is a claim not won "+
					"(if bd reworded it, teach isBdClaimConflictMessage the new text)", id, err)
			}
			if ok {
				t.Fatalf("Claim(%s) ok = true, want false", id)
			}
			after, err := setup.Get(id)
			if err != nil {
				t.Fatalf("Get(%s) after the claim: %v", id, err)
			}
			if after.Status != before.Status || after.Assignee != before.Assignee {
				t.Fatalf("the refused claim wrote: (%s, %q) became (%s, %q)", before.Status, before.Assignee, after.Status, after.Assignee)
			}
		})
	}

	t.Run("a missing bead is not found", func(t *testing.T) {
		_, ok, err := worker.Claim("tst-nosuchbead")
		if ok || !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("Claim(missing) = (ok=%v err=%v), want ErrNotFound", ok, err)
		}
	})
}
