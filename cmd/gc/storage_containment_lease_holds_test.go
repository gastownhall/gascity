package main

// T3 (boot-check-design.md carry-over 2, design review R11): every hold that
// runs before a containment lease is ever consulted must refuse identically
// whether or not a lease is sitting there ready to be (wrongly) trusted. The
// lease these tests serve is always one a real strandRecheck tick published
// — never hand-built — because R11 is specifically about the recheck: it
// never runs markerWorkStoreIdentityHold itself (it has no marker to hold
// against), so the calling process's OWN hold, run unconditionally before
// confirmInfraConvergenceWith ever reaches the lease, is what must catch
// this, not the lease's own acceptance checks.

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// convergedCityWithRecheckLease boots a genesis city for real, then runs one
// real strandRecheck tick against it — exactly the controller's own
// publisher, never a hand-built lease — and returns the lease it published
// alongside the city's path, config and target. The returned lease claims
// Converged/Serving: a trustworthy-looking lease is the whole point, because
// every test in this file proves it is never reached.
func convergedCityWithRecheckLease(t *testing.T) (cityPath string, cfg *config.City, target infraBindingTarget, lease *containmentLease) {
	t.Helper()
	cityPath = t.TempDir()
	writeScopeProjectID(t, cityPath, "prj-original")
	cfg = infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	stubInfraMigrationSource(t)

	var boot bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &boot)
	if err != nil {
		t.Fatalf("genesis boot refused: %v\n%s", err, boot.String())
	}
	target = routes.target
	_ = routes.close()

	recheck := &strandRecheck{
		cityPath: cityPath, target: target, stderr: io.Discard, logPrefix: "gc start",
		routes: &storageRoutes{target: target},
	}
	recheck.tick()
	lease = recheck.routes.lease.Load()
	if lease == nil {
		t.Fatal("the recheck's own tick published no lease; the fixture never reached the scenario under test")
	}
	if lease.Outcome != infraMigrationConverged.String() || !lease.Serving {
		t.Fatalf("the recheck's lease = %+v, want a serving Converged lease to make the bypass worth proving", lease)
	}
	return cityPath, cfg, target, lease
}

// TestHoldBeforeLeaseIdentityMismatch is R11 itself: the work store is
// recreated (a different project) after the recheck already published a
// lease for the old one. The lease's basis will no longer match this city's
// current basis either, but that is not the hold under test — the point is
// that markerWorkStoreIdentityHold, which runs first and unconditionally in
// confirmInfraConvergenceWith, catches this before the lease is ever
// fetched, consulted, or given the chance to be rejected on its own terms.
func TestHoldBeforeLeaseIdentityMismatch(t *testing.T) {
	cityPath, cfg, _, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)

	writeScopeProjectID(t, cityPath, "prj-recreated")

	var log bytes.Buffer
	boot := checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &log, storageBootGateOpts{lease: true})
	if boot.Outcome != infraMigrationIdentityMismatch {
		t.Fatalf("leased re-check over a recreated work store = %s, want identity-mismatch; a lease published before the recreation must never paper over it: %s", boot.Outcome, log.String())
	}
	for _, want := range []string{"prj-original", "prj-recreated"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("refusal %q does not name %q", log.String(), want)
		}
	}
}

// TestHoldBeforeLeaseServedBindingNote covers the OTHER hold that runs
// ahead of the lease: servedBindingNoteHold, at the very top of
// inspectInfraConvergenceWith, unconditionally, before the marker is even
// read. A served-binding note naming a mismatched provider must still
// genesis-block a leased re-check exactly as it would an unleased one, even
// though the lease being served claims a clean Converged verdict for this
// same target.
func TestHoldBeforeLeaseServedBindingNote(t *testing.T) {
	cityPath, cfg, target, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)

	if err := writeBornSplitServedNote(cityPath, bornSplitServedNote{
		Binding: target.Binding, Provider: "outoftree-engine",
	}); err != nil {
		t.Fatalf("writing the served-binding note: %v", err)
	}

	var log bytes.Buffer
	boot := checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &log, storageBootGateOpts{lease: true})
	if boot.Outcome != infraMigrationGenesisBlocked {
		t.Fatalf("leased re-check with a mismatched served-binding note = %s, want genesis-blocked; the note's hold must run before the lease is ever consulted: %s", boot.Outcome, log.String())
	}
	if boot.ServedProvider != "outoftree-engine" {
		t.Errorf("the hold's report does not carry the previously served provider: %+v", boot)
	}
}

// TestHoldBeforeLeaseStillRefusesUnleased is the control for both cases
// above: the same two holds, with opts.lease left false (no socket served
// at all), must refuse identically — proving the lease option changes
// nothing about what these holds catch, only whether a lease is consulted
// once they do not fire.
func TestHoldBeforeLeaseStillRefusesUnleased(t *testing.T) {
	t.Run("identity mismatch", func(t *testing.T) {
		cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
		writeScopeProjectID(t, cityPath, "prj-recreated")
		var log bytes.Buffer
		if boot := checkInfraClassConvergence(cityPath, cfg, "gc start", &log); boot.Outcome != infraMigrationIdentityMismatch {
			t.Fatalf("unleased re-check over a recreated work store = %s, want identity-mismatch: %s", boot.Outcome, log.String())
		}
	})
	t.Run("served binding note", func(t *testing.T) {
		cityPath, cfg, target, _ := convergedCityWithRecheckLease(t)
		if err := writeBornSplitServedNote(cityPath, bornSplitServedNote{
			Binding: target.Binding, Provider: "outoftree-engine",
		}); err != nil {
			t.Fatalf("writing the served-binding note: %v", err)
		}
		var log bytes.Buffer
		if boot := checkInfraClassConvergence(cityPath, cfg, "gc start", &log); boot.Outcome != infraMigrationGenesisBlocked {
			t.Fatalf("unleased re-check with a mismatched served-binding note = %s, want genesis-blocked: %s", boot.Outcome, log.String())
		}
	})
}
