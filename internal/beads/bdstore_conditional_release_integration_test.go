//go:build integration

package beads_test

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// requireConditionalReleaseEnv is set by the one CI cell that installs a bd
// built from deps.env BD_CURRENT_REF, which carries the flags. There every exit
// that is not "the CAS ran and passed" is a FAILURE: the cell exists to guard
// the CAS, so a green run that skipped it has guarded nothing — exactly the
// state this row was in while every job installed the flagless BD_VERSION
// release. Unset (every other shard, and local runs on a stock bd) it still
// skips.
const requireConditionalReleaseEnv = "GC_REQUIRE_BD_CONDITIONAL_RELEASE"

// conditionalReleaseRequired reports whether this run promised a bd that can
// prove the CAS. Any set, non-false value counts. An equality test against "1"
// would make GC_REQUIRE_BD_CONDITIONAL_RELEASE=true quietly mean "not
// required" — a green path bought with a typo, which is the exact disease this
// knob exists to cure.
func conditionalReleaseRequired() bool {
	value := strings.TrimSpace(os.Getenv(requireConditionalReleaseEnv))
	if value == "" {
		return false
	}
	if parsed, err := strconv.ParseBool(value); err == nil {
		return parsed
	}
	return true
}

// conditionalReleaseUnprovable ends the caller at an exit that did NOT prove the
// CAS — the capability probe itself failed, bd lacks the flags, or a leg is not
// drivable against this bd. It skips normally and FAILS under
// requireConditionalReleaseEnv.
//
// Every such exit routes through here on purpose. Honoring the promise only on
// the "flags absent" branch would leave the others green: a bd whose `update`
// verb disappeared makes the probe ERROR rather than report the flags missing,
// and skipping there passes a cell that proved nothing while claiming a
// flag-capable bd. Under the env there is exactly one green path — the row ran
// end to end. (bd being absent outright needs no branch: the scope builder
// shells `bd init` and t.Fatals when it cannot, on every run, env or not.)
func conditionalReleaseUnprovable(t *testing.T, format string, args ...any) {
	t.Helper()
	if conditionalReleaseRequired() {
		t.Fatalf("%s [%s is set: this run promised a bd that can prove the CAS (deps.env "+
			"BD_CURRENT_REF); either that pin regressed, bd changed under it, or the row is being "+
			"run somewhere it cannot run]", fmt.Sprintf(format, args...), requireConditionalReleaseEnv)
	}
	t.Skipf(format, args...)
}

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
//
// It runs on the contract matrix's "current" cell, whose bd is built from
// deps.env BD_CURRENT_REF. That is not a preference: the only release carrying
// the flags is the v1.2.1 PRERELEASE, below the published bar BD_VERSION holds,
// so on every shard that installs BD_VERSION this row can only skip. There the
// cell sets requireConditionalReleaseEnv and every exit short of a full run is a
// failure (conditionalReleaseUnprovable).
func TestBdStoreReleaseIfCurrentAgainstRealBd(t *testing.T) {
	store, scope := newConditionalIntegrationBdStore(t)
	if !bdParsesConditionalReleaseFlags(t, scope) {
		conditionalReleaseUnprovable(t, "installed bd does not advertise --if-assignee/--if-status "+
			"(pre-beads#5008): the store latches to the raw-SQL fallback, which embedded mode rejects "+
			"outright, so this row cannot exercise the verb at all. deps.env BD_VERSION is a published "+
			"release that predates the flags, and the only release carrying them is a prerelease "+
			"(v1.2.1), so this row runs on the source-built BD_CURRENT_REF cell "+
			"(make test-bd-conditional-release-contract), not on the shards that install BD_VERSION.")
	}

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
			t.Fatalf("ReleaseIfCurrent(missing id): %v", err)
		}
		if released {
			t.Fatal("released a bead that does not exist")
		}
	})

	// The statement this verb replaced matched `WHERE id = <literal>`. bd's
	// resolver prefix-matches instead, and it evaluates the CAS preconditions
	// against whatever it resolved — so an id one character short of a held bead
	// releases that bead and reports success. This leg drives the real resolver.
	t.Run("a non-exact id refuses instead of releasing a different bead", func(t *testing.T) {
		held, err := store.Create(beads.Bead{Title: "collision target", Type: "task"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.Update(held.ID, beads.UpdateOpts{
			Status:   strPtr("in_progress"),
			Assignee: strPtr("worker-1"),
		}); err != nil {
			t.Fatalf("Update to in_progress: %v", err)
		}
		truncated := held.ID[:len(held.ID)-1]
		if _, getErr := store.Get(truncated); !errors.Is(getErr, beads.ErrIDCollision) {
			conditionalReleaseUnprovable(t, "bd did not prefix-resolve %q to another bead (Get: %v); "+
				"the collision cell is not drivable here", truncated, getErr)
		}

		released, err := store.ReleaseIfCurrent(truncated, "worker-1")
		if !errors.Is(err, beads.ErrIDCollision) {
			t.Fatalf("ReleaseIfCurrent(%q) = released=%v err=%v, want a refusal wrapping ErrIDCollision", truncated, released, err)
		}
		if released {
			t.Fatal("released = true for an id that resolved to a different bead")
		}
		got, err := store.Get(held.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", held.ID, err)
		}
		if got.Status != "in_progress" || got.Assignee != "worker-1" {
			t.Fatalf("a bead the caller never named was released: state = (%s, %q), want (in_progress, \"worker-1\")", got.Status, got.Assignee)
		}
	})
}

// bdParsesConditionalReleaseFlags reports whether the installed bd advertises
// both conditional-release preconditions.
//
// The signal is POSITIVE — the flags appear in `bd update --help`, the same
// shape the production four-verb probe uses for --if-revision
// (conditionalWritesCapable). Deciding from an error string instead cannot work
// here: on a flagless bd the store latches to the raw-SQL path, embedded mode
// rejects `bd sql`, and the dolt fallback then SUCCEEDS with released=false for
// a wisp (it names the `issues` table), so no error ever surfaces to classify
// and the row hard-fails instead of skipping.
func bdParsesConditionalReleaseFlags(t *testing.T, scope string) bool {
	t.Helper()
	out, err := newConditionalIntegrationRunner(scope)(scope, "bd", "update", "--help")
	if err != nil {
		// The probe's OWN failure is not evidence about the flags — a bd that
		// dropped the `update` verb errors here rather than reporting the flags
		// absent — so it is unprovable, not skippable, under the require-env.
		conditionalReleaseUnprovable(t, "bd update --help failed, so the capability probe could not "+
			"run at all (bd broken, or the update verb is gone?): %v", err)
	}
	help := string(out)
	return strings.Contains(help, "--if-assignee") && strings.Contains(help, "--if-status")
}
