package main

// The boot census, and the probe it is allowed to retire.
//
// C4 made the mint bit an observation and left the relic bit pessimistically
// true, so nothing retired. This is the commit where a plan shape actually
// changes, and these rows are the live pair the change is worth: a clean
// binding stops probing, and a binding holding one migrated bead does not.
//
// Both rows run against a REAL sqlite binding opened by openStorageRoutes,
// because the claim is about what a converged city's boot observes, and a
// hand-built topology could assert the bits without ever proving the boot sets
// them.

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// openSplitBindingRoutes opens the routes a converged whole-split city boots
// with, against a real sqlite binding on disk.
func openSplitBindingRoutes(t *testing.T) *storageRoutes {
	t.Helper()
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))
	plan, err := resolveCityStoragePlan(root, cfg)
	if err != nil {
		t.Fatalf("resolving the storage plan for a converged split city: %v", err)
	}
	routes, err := openStorageRoutes(plan, mustResolveInfraTarget(t, root, cfg))
	if err != nil {
		t.Fatalf("openStorageRoutes: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })
	return routes
}

// soleBinding is the one binding a whole-split city relocates every class onto.
func soleBinding(t *testing.T, routes *storageRoutes) storeref.ClassBinding {
	t.Helper()
	bindings, err := residencyBindingsFromRoutes(routes)
	if err != nil {
		t.Fatalf("building bindings: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("got %d bindings, want the one this build serves", len(bindings))
	}
	return bindings[0]
}

// bindingLegRead reports whether a by-id plan for id would read the binding.
func bindingLegRead(t *testing.T, routes *storageRoutes, id string) bool {
	t.Helper()
	topo := assembleResidencyTopology(nil, beads.NewMemStore(), nil, mustBindings(t, routes), nil)
	plan, err := storeref.Plan(storeref.ByID{ID: id}, topo)
	if err != nil {
		t.Fatalf("planning a by-id read of %q: %v", id, err)
	}
	ref := storeref.ClassRef(infrastructureClasses())
	for _, leg := range plan.Legs {
		if leg.Leg.Ref == ref {
			return true
		}
	}
	return false
}

func mustBindings(t *testing.T, routes *storageRoutes) []storeref.ClassBinding {
	t.Helper()
	bindings, err := residencyBindingsFromRoutes(routes)
	if err != nil {
		t.Fatalf("building bindings: %v", err)
	}
	return bindings
}

func TestBootCensusRetiresTheProbeOnACleanBinding(t *testing.T) {
	routes := openSplitBindingRoutes(t)
	censusBindingRelics(routes)

	binding := soleBinding(t, routes)
	if !binding.MintsReserved {
		t.Fatal("the binding does not mint inside its own namespaces, so this row is not testing retirement at all — the relic bit is dead weight while the mint bit is off")
	}
	if binding.HasLegacyResidents {
		t.Error("a freshly opened binding reported legacy residents; nothing has ever been written to it")
	}
	if bindingLegRead(t, routes, "ga-1") {
		t.Error("a work-shaped by-id read still probes the binding of a city that has never held a migrated bead; the probe is the per-read cost this census exists to remove")
	}
}

// The ga-axin6 pin, at the topology level: one bead the migration carried
// across is enough to keep every work-shaped read probing, because that bead
// is reachable no other way.
func TestBootCensusKeepsTheProbeForAMigratedBead(t *testing.T) {
	routes := openSplitBindingRoutes(t)
	store, ok := routes.storeFor(coordclass.ClassGraph)
	if !ok {
		t.Fatal("the split city relocated no graph store")
	}
	creator, ok := store.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("%T cannot create with a foreign id, so it cannot hold a bead the migration carried across", store)
	}
	if _, err := creator.CreateWithForeignID(beads.Bead{ID: "ga-relic", Title: "carried across", Type: "task"}); err != nil {
		t.Fatalf("seeding the relic the way `gc storage migrate` does: %v", err)
	}
	censusBindingRelics(routes)

	if !soleBinding(t, routes).HasLegacyResidents {
		t.Fatal("the census missed a bead sitting open in the binding under a work-shaped id")
	}
	if !bindingLegRead(t, routes, "ga-relic") {
		t.Error("the plan for the relic's own id never reads the binding it lives in; the bead is unreachable")
	}
}

// The ordering guard. C5 landing before C4's pessimistic default, or a plane
// building bindings from routes the boot never censused, must not retire
// anything: an unasked question is not a clean answer.
func TestUncensusedRoutesKeepTheirProbe(t *testing.T) {
	routes := openSplitBindingRoutes(t)

	if !soleBinding(t, routes).HasLegacyResidents {
		t.Error("routes that were never censused reported a clean binding")
	}
	if !bindingLegRead(t, routes, "ga-1") {
		t.Error("an uncensused city retired its probe")
	}
}

// convergedEmptyInfraCity is convergedInfraCity's clean twin: a city that cut
// over while its work store held no infrastructure at all, so the binding it
// boots on holds no relic.
//
// That is the only shape whose probe may retire, and it is the shape mc reaches
// on the day its last carried-across bead closes.
func convergedEmptyInfraCity(t *testing.T) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	stubInfraMigrationSource(t)
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	return cityPath, cfg
}

// The census's call site, through the real boot gate.
//
// The rows above call censusBindingRelics directly. That proves the census
// works and proves nothing about whether the boot ever takes it — with the call
// deleted from storageBootGate they would all still pass, and every city would
// quietly go back to probing forever. This row boots a converged city the way
// `gc start` does and asserts the routes it hands back already carry the clean
// verdict.
func TestBootGateTakesTheCensus(t *testing.T) {
	cityPath, cfg := convergedEmptyInfraCity(t)

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("booting a converged city: %v (stderr: %s)", err, stderr.String())
	}
	t.Cleanup(func() { _ = routes.close() })

	binding := soleBinding(t, routes)
	if !binding.MintsReserved {
		t.Fatal("the booted binding does not mint inside its own namespaces, so the relic bit decides nothing here and this row tests no retirement")
	}
	if binding.HasLegacyResidents {
		t.Error("the boot gate handed back a binding still marked as holding relics; the census the gate exists to take was never taken")
	}
	if bindingLegRead(t, routes, "ga-1") {
		t.Error("every work-shaped read of a converged, relic-free city still probes the binding")
	}
}

// The same gate, on the city shape that actually shipped: one bead carried
// across under its original work id. The boot must observe it and keep probing.
func TestBootGateKeepsTheProbeForACityThatMigratedWork(t *testing.T) {
	cityPath, cfg, source, _ := convergedInfraCity(t)
	carried := infraStoreFingerprint(t, source)
	if len(carried) == 0 {
		t.Fatal("the converged fixture migrated nothing, so this row cannot distinguish an observed relic from the pessimistic default")
	}

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("booting a converged city: %v (stderr: %s)", err, stderr.String())
	}
	t.Cleanup(func() { _ = routes.close() })

	if !soleBinding(t, routes).HasLegacyResidents {
		t.Fatalf("the boot retired the probe on a binding holding %v", carried)
	}
	if !bindingLegRead(t, routes, carried[0]) {
		t.Errorf("the plan for %s never reads the binding it was carried into; the bead is unreachable", carried[0])
	}
}

// The operator's view of the same count.
//
// The probe retires on its own, silently, on the day the last relic closes.
// Without a number an operator can watch, a city that has been paying the probe
// for weeks looks exactly like one that retired it, and there is nothing to
// point at when asking why every read costs two.
func TestStorageStatusCountsTheOpenRelics(t *testing.T) {
	cityPath, cfg, source, _ := convergedInfraCity(t)
	carried := infraStoreFingerprint(t, source)
	if len(carried) != 1 {
		t.Fatalf("the converged fixture carried %d beads across, want exactly 1 so the printed count is unambiguous", len(carried))
	}
	stubInfraControllerPing(t, 0)

	var stdout, stderr bytes.Buffer
	code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr)
	got := stdout.String()
	if code != 0 {
		t.Fatalf("status exited %d on a converged city; a carried-across bead is the migration working, not a fault (stdout: %s, stderr: %s)", code, got, stderr.String())
	}
	if !strings.Contains(got, "open relics: 1") {
		t.Errorf("status does not count the open relics:\n%s", got)
	}
	if !strings.Contains(got, "probe") {
		t.Errorf("status names a relic count but not what it blocks, so an operator cannot tell why it matters:\n%s", got)
	}
}

// The other end of the drain: a city that cut over clean prints zero, so the
// operator watching the count fall sees it land.
func TestStorageStatusReportsNoRelicsOnACleanCutover(t *testing.T) {
	cityPath, cfg := convergedEmptyInfraCity(t)
	stubInfraControllerPing(t, 0)

	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr); code != 0 {
		t.Fatalf("status exited %d on a clean converged city (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "open relics: 0") {
		t.Errorf("status does not report the retired probe:\n%s", got)
	}
}

// A city that relocates nothing has no binding to census, and the census must
// not care.
func TestCensusOfIdentityRoutesIsANoOp(t *testing.T) {
	censusBindingRelics(nil)
	if bindings := mustBindings(t, nil); len(bindings) != 0 {
		t.Errorf("nil routes produced %d bindings", len(bindings))
	}
}
