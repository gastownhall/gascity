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
	"os"
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

// TestBootCensusIsLiveAndLeavesNothingOnDisk pins the shape of the census
// itself: it is computed from the binding, per process, and remembered only in
// the routes value the process already holds.
//
// The scan is expensive — the binding's whole history, 82ms at 10k rows and
// linear (internal/storeref/relic_census_bench_test.go) — and the obvious way to
// stop paying it per process is a note on disk. That note would be a status
// file: it goes stale the instant an operator rebuilds or re-points a binding,
// nothing clears it, and a `gc` that trusts it retires or keeps a probe on a
// verdict no store agrees with. AGENTS.md forbids exactly that, so the answer is
// re-derived, and this row is what a future memo has to argue with.
//
// A second, independent open of the SAME city must reach the same verdict from
// the store alone, and the city directory must gain no census artifact.
func TestBootCensusIsLiveAndLeavesNothingOnDisk(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))
	plan, err := resolveCityStoragePlan(root, cfg)
	if err != nil {
		t.Fatalf("resolving the storage plan for a converged split city: %v", err)
	}
	target := mustResolveInfraTarget(t, root, cfg)

	openAndCensus := func() bool {
		routes, err := openStorageRoutes(plan, target)
		if err != nil {
			t.Fatalf("openStorageRoutes: %v", err)
		}
		defer func() { _ = routes.close() }()
		censusBindingRelics(routes)
		return soleBinding(t, routes).HasLegacyResidents
	}

	if first := openAndCensus(); first {
		t.Fatalf("a binding with nothing carried across reported relics = %v", first)
	}

	store, err := openStorageRoutes(plan, target)
	if err != nil {
		t.Fatalf("openStorageRoutes: %v", err)
	}
	graph, ok := store.storeFor(coordclass.ClassGraph)
	if !ok {
		t.Fatal("the split city relocated no graph store")
	}
	creator, ok := graph.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("%T cannot create with a foreign id, so it cannot hold a bead the migration carried across", graph)
	}
	if _, err := creator.CreateWithForeignID(beads.Bead{ID: "ga-relic", Title: "carried across", Type: "task"}); err != nil {
		t.Fatalf("seeding the relic the way `gc storage migrate` does: %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("closing the seeding handle: %v", err)
	}

	if second := openAndCensus(); !second {
		t.Fatal("a fresh open censused the binding clean after a relic was carried into it; the verdict is not being read from the store")
	}

	for _, name := range []string{
		"storage-relic-census.json",
		"relic-census.json",
	} {
		if _, err := os.Stat(filepath.Join(root, ".gc", name)); err == nil {
			t.Errorf("the census wrote %s; a remembered verdict is a status file and goes stale the moment a binding is rebuilt", name)
		}
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

// The ga-qdt5y.19 incident shape, at the boot gate: the last relic CLOSES, and
// the probe must not retire.
//
// Closing a relic drains it from the operator's report and changes nothing
// about where it lives. `gc storage migrate` never deletes the work store's
// pre-migration copy, so if this city retired its probe the relic's own id
// would resolve to that copy — OPEN, with pre-migration fields, forever, with
// no error anywhere. A `show` would report a bead completed weeks ago as ready
// work, and an `update --claim` would claim it.
//
// This row fails against the open-only census that shipped before .19, which is
// the point: nothing in the tree closed a relic and then asked about it, so the
// bug lived in the gap between the fixtures that seed OPEN relics and the ones
// that hold none at all.
func TestBootCensusKeepsTheProbeForAClosedRelic(t *testing.T) {
	cityPath, cfg, source, _ := convergedInfraCity(t)
	carried := infraStoreFingerprint(t, source)
	if len(carried) != 1 {
		t.Fatalf("the converged fixture carried %d beads across, want exactly 1 so closing it empties the OPEN population outright", len(carried))
	}

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("booting a converged city: %v (stderr: %s)", err, stderr.String())
	}
	t.Cleanup(func() { _ = routes.close() })

	binding := soleBinding(t, routes)
	if err := binding.Leg.Store.Close(carried[0]); err != nil {
		t.Fatalf("closing the carried-across bead %s in the binding it lives in: %v", carried[0], err)
	}
	if open, err := storeref.OpenLegacyResidents(binding.Leg.Store, config.AllReservedClassPrefixes()); err != nil || len(open) != 0 {
		t.Fatalf("after the close the binding still reports %v open relics (err %v); this row cannot distinguish a widened census from an undrained one", open, err)
	}
	censusBindingRelics(routes)

	if !soleBinding(t, routes).HasLegacyResidents {
		t.Fatal("the boot certified a binding clean because its only relic had CLOSED; the probe retires and every read of that id is answered by the migration's frozen open copy")
	}
	if !bindingLegRead(t, routes, carried[0]) {
		t.Errorf("the plan for %s no longer reads the binding holding its live record", carried[0])
	}
}

// The divergence pin: the drain count kept its OPEN semantics when the
// retirement verdict widened past them.
//
// These are now two different questions over one binding — "how much is left to
// drain" (open) and "can this binding hold this id" (open or closed) — and the
// silent failure is someone tidying them back into one. Pointing
// reportBindingRelics at the widened list would print a count that can never
// fall, turning an operator's drain gauge into a constant; pointing the verdict
// back at the open list is the .19 bug. This row holds both ends apart on one
// city at one moment.
func TestClosingTheLastRelicDrainsTheCountAndKeepsTheProbe(t *testing.T) {
	cityPath, cfg, source, _ := convergedInfraCity(t)
	carried := infraStoreFingerprint(t, source)
	if len(carried) != 1 {
		t.Fatalf("the converged fixture carried %d beads across, want exactly 1", len(carried))
	}
	var bootErr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &bootErr)
	if err != nil {
		t.Fatalf("booting a converged city: %v (stderr: %s)", err, bootErr.String())
	}
	t.Cleanup(func() { _ = routes.close() })
	if err := soleBinding(t, routes).Leg.Store.Close(carried[0]); err != nil {
		t.Fatalf("closing the carried-across bead %s: %v", carried[0], err)
	}
	censusBindingRelics(routes)
	stubInfraControllerPing(t, 0)

	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr); code != 0 {
		t.Fatalf("status exited %d on a drained city (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "open relics: 0") {
		t.Errorf("the drain count did not reach zero after its last relic closed, so an operator watching it never sees the drain land:\n%s", got)
	}

	if !soleBinding(t, routes).HasLegacyResidents {
		t.Error("the drained city retired its residence probe; the relic is closed, not gone, and its id still resolves to the binding")
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

// Seeding a relic is an OUT-OF-ORDER act by construction, and this pins that the
// fixture puts the order back.
//
// The census runs when the funnel OPENS a binding, and no fixture can plant a
// bead in a binding it has not opened yet — so every relic a test seeds arrives
// after the verdict that describes it. A row that then asserts on residency is
// reading a verdict that was true when it was taken and is false by the time it
// is read, and the failure is silent in the worst direction: the probe retires,
// the binding stops being asked, and the row passes or fails for a reason that
// has nothing to do with what it claims to test.
//
// Production never produces that ordering. Relics are what `gc storage migrate`
// leaves behind, so every process that meets them starts after they exist and
// censuses once at boot — which is why the seed helper reopens the funnel rather
// than papering over the verdict. Deleting that reopen is what this row catches.
func TestSeedingARelicLeavesTheBindingCensusedAsRelicBearing(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	classResidentWorkShapedBead(t, cityPath, "gc-relic1", "an orphaned patrol root")

	binding := soleBinding(t, cliStorageRoutes(cityPath))
	if !binding.MintsReserved {
		t.Fatal("the fixture's binding does not mint inside its own namespaces, so the relic bit decides nothing and this row pins no ordering")
	}
	if !binding.HasLegacyResidents {
		t.Error("the binding holds a seeded relic and still reads as clean; the census verdict predates the relic, so every row that seeds one is asserting against a stale answer")
	}
}

// The `gc bd` by-id door reaches a relic the binding holds. That is the property
// this row has always pinned, and it now pins it against ONE probe.
//
// There used to be two. storeref.planByID skipped a binding whose census came
// back clean; bdByIDClassDoor.resolve asked the binding for every id
// unconditionally and had never heard of the census. ga-qdt5y.18 collapsed the
// door onto the plan, so the census verdict is now load-bearing on the CLI's
// hottest one-shot path rather than advisory to it: get retirement wrong and
// this read is the one that loses the bead.
//
// Which is why the seed here must recensus. classResidentWorkShapedBead does —
// see the warning on it — and without that the binding would still read as
// clean, the plan would retire the probe, and this row would fail for the
// fixture's reason instead of the code's.
func TestBdByIDDoorReachesASeededRelic(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	relic, _ := classResidentWorkShapedBead(t, cityPath, "gc-relic1", "an orphaned patrol root")

	door, relocated, err := openBdByIDClassFrontDoor(cityPath)
	if err != nil {
		t.Fatalf("opening the by-id class front door: %v", err)
	}
	if !relocated {
		t.Fatal("the fixture city resolved no class binding, so there is no door to test")
	}
	resolution, err := door.resolve(relic.ID)
	if err != nil {
		t.Fatalf("the door failed to decide ownership of %s: %v", relic.ID, err)
	}
	if !resolution.Found {
		t.Errorf("the door reported %s absent; the binding holds it, and a read that loses a relic is the root-loss shape this lane exists to prevent", relic.ID)
	}
}

// The same door, one lifecycle step on, against the REAL census rather than a
// stubbed verdict — the ga-qdt5y.19 incident at the surface an operator feels.
//
// TestBdByIDDoorSkipsTheProbeOnACensusCleanBinding pins that the door consumes
// the plan's retirement, and it does so from a hand-written verdict, so it is
// silent on whether the census produces the right one. This row lets the census
// run for itself: the binding's only relic is CLOSED, which drains the operator
// count to zero and changes nothing about where the bead lives. Under the
// open-only rule the recensus certifies the binding clean, the plan retires the
// probe, and `gc bd show` answers from the work store's frozen pre-migration
// copy — reporting a bead that was closed a moment ago as open, forever.
func TestBdByIDDoorReachesAClosedRelic(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	relic, classStore := classResidentWorkShapedBead(t, cityPath, "gc-relic1", "an orphaned patrol root")
	if err := classStore.Close(relic.ID); err != nil {
		t.Fatalf("closing the relic in the binding it lives in: %v", err)
	}
	if open, err := storeref.OpenLegacyResidents(classStore, config.AllReservedClassPrefixes()); err != nil || len(open) != 0 {
		t.Fatalf("the binding still reports %v open relics after the close (err %v); this row cannot tell a widened census from an undrained one", open, err)
	}
	recensusAfterSeedingARelic(t, cityPath)

	door, relocated, err := openBdByIDClassFrontDoor(cityPath)
	if err != nil {
		t.Fatalf("opening the by-id class front door: %v", err)
	}
	if !relocated {
		t.Fatal("the fixture city resolved no class binding, so there is no door to test")
	}
	resolution, err := door.resolve(relic.ID)
	if err != nil {
		t.Fatalf("the door failed to decide ownership of %s: %v", relic.ID, err)
	}
	if !resolution.Found {
		t.Fatalf("the door lost %s the moment it closed; the binding still holds the only live record, and the work store's answer is a frozen copy that reads open forever", relic.ID)
	}
	if resolution.Bead.Status != "closed" {
		t.Errorf("the door answered with a %q record for a bead that was just closed in the binding; that is the migration's retained pre-migration copy, not the live one", resolution.Bead.Status)
	}
}
