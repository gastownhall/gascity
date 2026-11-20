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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestLegacyMarkerAlwaysTakesTheFullCheckEvenLeased is boot-check-design.md
// §4.8 mutant 16 ("accept a legacy marker"): confirmInfraConvergenceWith's
// lease shortcut is gated on storeRecorded, not only on opts.lease, because
// a legacy marker (recorded==false) is exactly what the full check's own
// backfill (confirmInfraConvergenceWith, just past the lease branch) exists
// to fix. A lease published against this same, still-legacy city can have a
// basis that matches perfectly — nothing about storeRecorded rides on the
// wire — so if the gate ever drops to opts.lease alone, this city takes the
// leased shortcut forever and the backfill never runs.
func TestLegacyMarkerAlwaysTakesTheFullCheckEvenLeased(t *testing.T) {
	cityPath := t.TempDir()
	writeScopeProjectID(t, cityPath, "prj-legacy")
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	stubInfraMigrationSource(t)
	target := mustResolveInfraTarget(t, cityPath, cfg)

	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("genesis boot refused: %v", err)
	}
	_ = routes.close()

	// Strip the marker back to its legacy, pre-identity one-line shape.
	data, err := os.ReadFile(target.MarkerPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	legacy := lines[len(lines)-1] + "\n"
	if err := os.WriteFile(target.MarkerPath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, recorded, err := readMarkerWorkStoreIdentity(target.MarkerPath()); err != nil || recorded {
		t.Fatalf("fixture: marker = (recorded=%v, err=%v), want a legacy marker with no identity recorded", recorded, err)
	}

	// A real recheck tick against this still-legacy city: its basis is
	// whatever the city looks like right now, which a later re-check of the
	// same, unchanged city will capture identically.
	recheck := &strandRecheck{
		cityPath: cityPath, target: target, stderr: io.Discard, logPrefix: "gc start",
		routes: &storageRoutes{target: target},
	}
	recheck.tick()
	lease := recheck.routes.lease.Load()
	if lease == nil || lease.Outcome != infraMigrationConverged.String() || !lease.Serving {
		t.Fatalf("the recheck's lease = %+v, want a serving Converged lease to make the bypass worth proving", lease)
	}
	bornSplitLeaseListener(t, cityPath, lease)

	var log bytes.Buffer
	report := checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &log, storageBootGateOpts{lease: true})
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("leased re-check of a legacy-marker city = %s, want converged: %s", report.Outcome, log.String())
	}
	if _, recorded, err := readMarkerWorkStoreIdentity(target.MarkerPath()); err != nil || !recorded {
		t.Fatalf("after a leased re-check the marker = (recorded=%v, err=%v), want the full check's backfill to have run; a legacy marker must never take the lease shortcut", recorded, err)
	}
}

// TestHoldBeforeLeaseStillRefusesUnleased is the control for every case in
// this file: the same holds, with opts.lease left false (no socket served
// at all), must refuse identically — proving the lease option changes
// nothing about what these holds catch, only whether a lease is consulted
// once they do not fire.
func TestHoldBeforeLeaseStillRefusesUnleased(t *testing.T) {
	t.Run("identity mismatch", func(t *testing.T) {
		cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
		writeScopeProjectID(t, cityPath, "prj-recreated")
		var log bytes.Buffer
		if boot := checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &log, storageBootGateOpts{}); boot.Outcome != infraMigrationIdentityMismatch {
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
		if boot := checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &log, storageBootGateOpts{}); boot.Outcome != infraMigrationGenesisBlocked {
			t.Fatalf("unleased re-check with a mismatched served-binding note = %s, want genesis-blocked: %s", boot.Outcome, log.String())
		}
	})
	t.Run("re-copy record", func(t *testing.T) {
		cityPath, cfg, target, _ := convergedCityWithRecheckLease(t)
		if err := writeInfraRecopyRecord(target, infraMigrationFromBackup); err != nil {
			t.Fatalf("writing the re-copy record: %v", err)
		}
		var log bytes.Buffer
		if boot := checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &log, storageBootGateOpts{}); boot.Outcome != infraMigrationUncheckable {
			t.Fatalf("unleased re-check with an open re-copy record = %s, want uncheckable: %s", boot.Outcome, log.String())
		}
	})
	t.Run("restore record", func(t *testing.T) {
		cityPath, cfg, target, _ := convergedCityWithRecheckLease(t)
		if err := writeInfraRestoreRecord(target, infraRestoreRecord{
			Snapshot: "snap1", To: target.Database, Database: target.Database,
			AsideSuffix: ".aside-1", Command: "gc storage snapshot restore",
		}); err != nil {
			t.Fatalf("writing the restore record: %v", err)
		}
		var log bytes.Buffer
		if boot := checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &log, storageBootGateOpts{}); boot.Outcome != infraMigrationUncheckable {
			t.Fatalf("unleased re-check with an open restore record = %s, want uncheckable: %s", boot.Outcome, log.String())
		}
	})
	t.Run("reverse journal", func(t *testing.T) {
		cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
		if err := writeStorageReverseJournal(cityPath, storageReverseJournal{
			Format: storageReverseJournalFormat, Mode: reverseToWork, Phase: reverseStarted,
		}); err != nil {
			t.Fatalf("writing the reverse journal: %v", err)
		}
		var log bytes.Buffer
		if _, err := storageBootGateWith(cityPath, cfg, "gc start", nil, &log, storageBootGateOpts{}); err == nil || !strings.Contains(err.Error(), "reverse migration") {
			t.Fatalf("unleased boot with an open reverse journal = %v, want a reverse-migration refusal: %s", err, log.String())
		}
	})
}

// TestHoldBeforeLeaseRecopyRecord is T3's re-copy case (design review R11):
// an open re-copy record (infra.recopying) holds the marker stale
// (readInfraConvergenceState) even with a trustworthy-looking Converged
// lease served for the same target. markerWorkStoreIdentityHold's own
// identity check passes — nothing about this city changed — so the catch
// here is readInfraConvergenceState itself, read unconditionally before
// confirmInfraConvergenceWith (and so the lease) is ever reached: this kills
// a "lease before the marker-state read" reorder, which would let this same
// city serve Converged from the lease instead of refusing as stale.
func TestHoldBeforeLeaseRecopyRecord(t *testing.T) {
	cityPath, cfg, target, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)

	if err := writeInfraRecopyRecord(target, infraMigrationFromBackup); err != nil {
		t.Fatalf("writing the re-copy record: %v", err)
	}

	var log bytes.Buffer
	boot := checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &log, storageBootGateOpts{lease: true})
	if boot.Outcome != infraMigrationUncheckable {
		t.Fatalf("leased re-check with an open re-copy record = %s, want uncheckable; the marker-state read must run before the lease is ever consulted: %s", boot.Outcome, log.String())
	}
	if !strings.Contains(log.String(), "re-copy") {
		t.Errorf("refusal %q does not name the re-copy as the cause", log.String())
	}
}

// TestHoldBeforeLeaseRestoreRecord is T3's restore case: an open restore
// record (infra.restoring) holds the marker stale the same way the re-copy
// record does, and must refuse a leased re-check identically.
func TestHoldBeforeLeaseRestoreRecord(t *testing.T) {
	cityPath, cfg, target, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)

	if err := writeInfraRestoreRecord(target, infraRestoreRecord{
		Snapshot: "snap1", To: target.Database, Database: target.Database,
		AsideSuffix: ".aside-1", Command: "gc storage snapshot restore",
	}); err != nil {
		t.Fatalf("writing the restore record: %v", err)
	}

	var log bytes.Buffer
	boot := checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &log, storageBootGateOpts{lease: true})
	if boot.Outcome != infraMigrationUncheckable {
		t.Fatalf("leased re-check with an open restore record = %s, want uncheckable; the marker-state read must run before the lease is ever consulted: %s", boot.Outcome, log.String())
	}
	if !strings.Contains(log.String(), "restore") {
		t.Errorf("refusal %q does not name the restore as the cause", log.String())
	}
}

// TestHoldBeforeLeaseReverseJournal is T3's reverse-journal case: an open
// reverse migration holds boot (storageReverseRefusal), run unconditionally
// at the very top of storageBootGateWith — before resolveInfraBindingTarget,
// let alone checkInfraClassConvergenceWith and the lease it may consult, is
// ever reached. A trustworthy-looking Converged lease already served for
// this same target changes nothing: this test proves that directly, by
// calling storageBootGateWith itself (not checkInfraClassConvergenceWith,
// which an open reverse never even gets called into) with opts.lease true,
// and getting the identical reverse-migration refusal the unleased control
// in TestHoldBeforeLeaseStillRefusesUnleased gets.
func TestHoldBeforeLeaseReverseJournal(t *testing.T) {
	cityPath, cfg, _, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)

	if err := writeStorageReverseJournal(cityPath, storageReverseJournal{
		Format: storageReverseJournalFormat, Mode: reverseToWork, Phase: reverseStarted,
	}); err != nil {
		t.Fatalf("writing the reverse journal: %v", err)
	}

	var log bytes.Buffer
	routes, err := storageBootGateWith(cityPath, cfg, "gc start", nil, &log, storageBootGateOpts{lease: true})
	if err == nil {
		_ = routes.close()
		t.Fatal("leased boot with an open reverse journal was not refused; the reverse hold must run before the lease is ever consulted")
	}
	if !strings.Contains(err.Error(), "reverse migration") {
		t.Fatalf("refusal %q does not name the reverse migration as the cause", err)
	}
}

// TestHoldBeforeLeaseMinGCVersion kills mutant HF: a city's gc-version
// floor (minGCVersionRefusal) holds boot before the lease is ever
// consulted, exactly like every other hold in this file. A trustworthy
// Converged lease already served for this target changes nothing.
func TestHoldBeforeLeaseMinGCVersion(t *testing.T) {
	cityPath, cfg, _, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)

	stubGCVersion(t, "1.0.0")
	if err := writeMinGCVersion(cityPath, "9.9.9"); err != nil {
		t.Fatalf("writing the gc-version floor: %v", err)
	}

	var log bytes.Buffer
	routes, err := storageBootGateWith(cityPath, cfg, "gc start", nil, &log, storageBootGateOpts{lease: true})
	if err == nil {
		_ = routes.close()
		t.Fatal("leased boot below the city's gc-version floor was not refused; the version hold must run before the lease is ever consulted")
	}
	if !strings.Contains(err.Error(), "requires gc") {
		t.Fatalf("refusal %q does not name the gc-version floor as the cause", err)
	}
}

// TestHoldBeforeLeaseCityRestoreJournal kills mutant HC: an unfinished city
// restore (storageCityRestoreBootRefusal) holds boot before the lease is
// ever consulted, exactly like every other hold in this file.
func TestHoldBeforeLeaseCityRestoreJournal(t *testing.T) {
	cityPath, cfg, _, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)

	if err := writeStorageCityRestoreJournal(cityPath, storageCityRestoreJournal{
		Format: storageCityRestoreFormat, Snapshot: "snap1", To: cityPath, Suffix: ".aside-1",
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("writing the city restore journal: %v", err)
	}

	var log bytes.Buffer
	routes, err := storageBootGateWith(cityPath, cfg, "gc start", nil, &log, storageBootGateOpts{lease: true})
	if err == nil {
		_ = routes.close()
		t.Fatal("leased boot with an unfinished city restore was not refused; the restore hold must run before the lease is ever consulted")
	}
	if !strings.Contains(err.Error(), "restore") {
		t.Fatalf("refusal %q does not name the city restore as the cause", err)
	}
}

// TestHoldBeforeLeaseRemoteStage kills mutant HS: a remote stage record
// (remoteStageStorageRefusal) holds boot before the lease is ever
// consulted, exactly like every other hold in this file.
func TestHoldBeforeLeaseRemoteStage(t *testing.T) {
	cityPath, cfg, _, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)

	if err := writeRemoteStage(cityPath, remoteStage{ControllerCity: "controller-city", ControllerPath: "/srv/controller-city"}); err != nil {
		t.Fatalf("writing the remote stage record: %v", err)
	}

	var log bytes.Buffer
	routes, err := storageBootGateWith(cityPath, cfg, "gc start", nil, &log, storageBootGateOpts{lease: true})
	if err == nil {
		_ = routes.close()
		t.Fatal("leased boot of a remotely-staged city was not refused; the remote-stage hold must run before the lease is ever consulted")
	}
	if !strings.Contains(err.Error(), "staged") && !strings.Contains(err.Error(), "controller-city") {
		t.Fatalf("refusal %q does not name the remote stage as the cause", err)
	}
}

// TestHoldBeforeLeaseRigImportInFlight kills mutant HG: an in-flight rig
// import (rigBootRefusal) holds boot before the lease is ever consulted,
// exactly like every other hold in this file. convergedCityWithRecheckLease's
// own target already names a non-empty binding root, which is all
// rigBootRefusal needs to be invoked.
func TestHoldBeforeLeaseRigImportInFlight(t *testing.T) {
	cityPath, cfg, target, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)
	if target.Root == "" {
		t.Fatal("fixture: target.Root is empty; rigBootRefusal is never invoked with no binding root")
	}

	inFlight := filepath.Join(target.Root, rigGraphInFlightName("packs"))
	if err := os.WriteFile(inFlight, []byte("{}"), 0o644); err != nil {
		t.Fatalf("writing the in-flight rig import marker: %v", err)
	}

	var log bytes.Buffer
	routes, err := storageBootGateWith(cityPath, cfg, "gc start", nil, &log, storageBootGateOpts{lease: true})
	if err == nil {
		_ = routes.close()
		t.Fatal("leased boot with an in-flight rig import was not refused; the rig hold must run before the lease is ever consulted")
	}
	if !strings.Contains(err.Error(), rigImportInstruction("packs")) {
		t.Fatalf("refusal %q does not name the rig import command as the cause", err)
	}
}
