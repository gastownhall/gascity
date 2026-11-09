//go:build integration

package beads_test

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

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
// against a REAL bd binary in EMBEDDED mode.
//
// It is the authoritative guard for the two facts the unit tests can only
// assume about bd: that the flag spelling still exists, and that a rejected
// precondition is exit 13 with nothing written. A bd that renames the flags
// fails the capability leg here; a bd that changes the exit status fails the
// mismatch leg, loudly, instead of silently degrading gascity to "someone else
// holds it" on every release.
//
// It is homed on the contract matrix's "current" cell, whose bd is built from
// deps.env BD_CURRENT_REF. That is a choice, not a capability limit: deps.env
// BD_VERSION is v1.3.4, which carries the flags, so the shards that
// install BD_VERSION could run this row too. It stays here as the single home
// for the contract, and this cell sets requireConditionalReleaseEnv so every
// exit short of a full run is a failure (conditionalReleaseUnprovable). The
// floor (BD_PREV_VERSION, v1.3.0) carries the flags too; only an off-pin bd
// older than the supported floor skips.
func TestBdStoreReleaseIfCurrentAgainstRealBd(t *testing.T) {
	store, scope := newConditionalIntegrationBdStore(t)
	if !bdParsesConditionalReleaseFlags(t, scope) {
		conditionalReleaseUnprovable(t, "installed bd does not advertise --if-assignee/--if-status "+
			"(pre-beads#5008): ReleaseIfCurrent refuses it with ErrConditionalReleaseUnsupported, "+
			"so this row cannot exercise the verb at all. deps.env BD_PREV_VERSION, BD_VERSION and "+
			"BD_CURRENT_REF all carry the flags, so a bd this old is below the supported 1.3.0 "+
			"floor and off-pin; the row is homed on the BD_CURRENT_REF cell "+
			"(make test-bd-conditional-release-contract).")
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

// TestBdStoreLiveScopedListAndConditionalReleaseSkipDeferredRows drives, against
// a REAL bd, the three facts the release sweep's cost fix (ga-y0ft4d) rests on
// and the unit tests can only assume:
//
//  1. a live list scoped to one assignee and `open` returns that assignee's
//     open bead;
//  2. once the bead is deferred, the same list OMITS it (bd drops parked rows
//     from --status=open server side) although Get still reports Status
//     "open" — which is why the verification is a scoped list and not a Get;
//  3. a fenced release (UpdateIfAssignment on open + assignee) of the deferred
//     bead is refused by bd with nothing written, so a deferred claim that
//     slipped past the sweep's skip is still never released.
//
// If leg 3 fails on the installed bd, the sweep skip is no longer a pure
// optimisation over a safe write: stop and take it to the architect.
func TestBdStoreLiveScopedListAndConditionalReleaseSkipDeferredRows(t *testing.T) {
	store, scope := newConditionalIntegrationBdStore(t)
	if !bdParsesConditionalReleaseFlags(t, scope) {
		conditionalReleaseUnprovable(t, "installed bd does not advertise --if-assignee/--if-status")
	}
	const owner = "worker-1"

	created, err := store.Create(beads.Bead{Title: "deferred claim row", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Update(created.ID, beads.UpdateOpts{Assignee: strPtr(owner)}); err != nil {
		t.Fatalf("Update assignee: %v", err)
	}

	scoped := beads.ListQuery{Status: "open", Assignee: owner, Live: true, TierMode: beads.TierBoth}
	listed := func(t *testing.T) bool {
		t.Helper()
		rows, err := store.List(scoped)
		if err != nil {
			t.Fatalf("List(%+v): %v", scoped, err)
		}
		for _, r := range rows {
			if r.ID == created.ID {
				return true
			}
		}
		return false
	}

	t.Run("an open claim is listed under its assignee", func(t *testing.T) {
		if !listed(t) {
			t.Fatalf("scoped live List omitted open bead %s held by %s", created.ID, owner)
		}
	})

	deferUntil := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	if out, err := newConditionalIntegrationRunner(t, scope)(scope, "bd", "update", created.ID, "--defer", deferUntil); err != nil {
		t.Fatalf("bd update --defer %s: %v\n%s", deferUntil, err, out)
	}

	t.Run("a deferred claim drops out of the scoped list while Get still says open", func(t *testing.T) {
		if listed(t) {
			t.Fatalf("scoped live List still returned deferred bead %s; bd no longer parks it out of --status=open", created.ID)
		}
		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != "open" {
			t.Fatalf("Get status = %q, want open (the read-side collapse the scoped list guards against)", got.Status)
		}
	})

	t.Run("the fenced release of a deferred claim writes nothing", func(t *testing.T) {
		released, err := store.UpdateIfAssignment(created.ID, "open", owner, beads.UpdateOpts{
			Status:   strPtr("open"),
			Assignee: strPtr(""),
		})
		if err != nil {
			t.Fatalf("UpdateIfAssignment(deferred): %v", err)
		}
		if released {
			t.Fatal("released a deferred claim; the sweep's deferred-open skip would no longer be redundant with the write guard")
		}
		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get after refused release: %v", err)
		}
		if got.Assignee != owner {
			t.Fatalf("assignee = %q after a refused release, want %q", got.Assignee, owner)
		}
		if listed(t) {
			t.Fatal("bead is listed as open again: the refused release un-deferred it")
		}
	})
}

// bdParsesConditionalReleaseFlags reports whether the installed bd advertises
// both conditional-release preconditions.
//
// The signal is POSITIVE — the flags appear in `bd update --help`, the same
// shape the production four-verb probe uses for --if-revision
// (conditionalWritesCapable) — so the skip decision never rests on the code
// under test. Skipping on ReleaseIfCurrent's own
// ErrConditionalReleaseUnsupported would let a misread unknown-flag error skip
// the row this test exists to run.
func bdParsesConditionalReleaseFlags(t *testing.T, scope string) bool {
	t.Helper()
	out, err := newConditionalIntegrationRunner(t, scope)(scope, "bd", "update", "--help")
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
