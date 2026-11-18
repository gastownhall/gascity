package main

// Tests for the controller containment lease (#9237 follow-up, slice 1):
// boot-check-design.md §4.7 T5 (controller) and T6 (quiescence).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// sideEffectOnFirstListStore runs before once, before its first List call,
// then delegates every call (including that one) to Store.
type sideEffectOnFirstListStore struct {
	beads.Store
	before func()
	ran    bool
}

func (s *sideEffectOnFirstListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if !s.ran {
		s.ran = true
		s.before()
	}
	return s.Store.List(q)
}

// rewriteInfraManifestInPlace rewrites target's manifest with the ids it
// already records, unchanged: a no-op in content, but a real temp+rename
// like any other rewrite (infra_copy_manifest.go), so it changes the
// manifest's inode and therefore its containment-basis stamp.
func rewriteInfraManifestInPlace(t *testing.T, target infraBindingTarget) {
	t.Helper()
	manifest, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil || !recorded {
		t.Fatalf("reading the manifest to rewrite it: recorded=%v err=%v", recorded, err)
	}
	ids := make([]string, 0, len(manifest.IDs))
	for id := range manifest.IDs {
		ids = append(ids, id)
	}
	if err := rewriteInfraCopyManifest(target, manifest, ids, manifest.Created); err != nil {
		t.Fatalf("rewriting the manifest: %v", err)
	}
}

// TestConfirmInfraContainmentCapturesTheBasisBeforeTheRead is T5's "basis
// taken before the read": if the manifest is rewritten during the
// work-store List call classifyInfraContainmentGap makes, the report
// carries the manifest's stamp from BEFORE that call, not after. Mutant 13
// ("basis taken after the read") moves the capture past the read and this
// test catches it directly, by comparing the two possible answers.
func TestConfirmInfraContainmentCapturesTheBasisBeforeTheRead(t *testing.T) {
	cityPath, _, source, target := convergedInfraCity(t)

	before, err := containmentBasisStamp(target.ManifestPath(), true)
	if err != nil {
		t.Fatalf("stamping the manifest before the check: %v", err)
	}

	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return &sideEffectOnFirstListStore{Store: source, before: func() {
			rewriteInfraManifestInPlace(t, target)
		}}, nil
	}

	report := confirmInfraContainment(cityPath, target, "gc start", io.Discard)

	after, err := containmentBasisStamp(target.ManifestPath(), true)
	if err != nil {
		t.Fatalf("stamping the manifest after the check: %v", err)
	}
	if after == before {
		t.Fatal("fixture: rewriting the manifest in place did not change its stamp")
	}
	if report.basis.Manifest != before {
		t.Errorf("report.basis.Manifest = %q, want the pre-read stamp %q (the post-read stamp is %q)", report.basis.Manifest, before, after)
	}
}

// TestStrandRecheckPublishesTheLeaseAndRefreshesItsAge is T5's remaining
// controller bullets: the boot verdict is published; a definitive recheck
// tick refreshes the lease's age; a transient failure does not. Mutant 12
// ("publish on a transient tick") is caught by the final assertion.
func TestStrandRecheckPublishesTheLeaseAndRefreshesItsAge(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	publishBootContainmentLease(routes)
	boot := routes.lease.Load()
	if boot == nil {
		t.Fatal("the boot verdict published no lease")
	}
	if boot.Outcome != infraMigrationConverged.String() {
		t.Errorf("the boot lease's outcome = %q, want %q", boot.Outcome, infraMigrationConverged.String())
	}

	rec := events.NewFake()
	s := newStrandRecheck(routes, cityPath, rec, io.Discard, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}

	s.tick()
	afterTick := routes.lease.Load()
	if afterTick == nil || !afterTick.TakenAt.After(boot.TakenAt) {
		t.Fatalf("a definitive tick did not refresh the lease: boot taken at %v, after tick %v", boot.TakenAt, afterTick.TakenAt)
	}

	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return nil, errors.New(`Get "https://hosted-bd/v1/issues": dial tcp: i/o timeout`)
	}
	s.tick()
	openInfraMigrationSource = prev

	stillAfterTick := routes.lease.Load()
	if !stillAfterTick.TakenAt.Equal(afterTick.TakenAt) {
		t.Fatalf("a transient recheck failure refreshed the lease: %v became %v", afterTick.TakenAt, stillAfterTick.TakenAt)
	}
}

// TestServedContainmentLeaseAnswersOnlyForItsOwnCity is T5's "with two
// cities, each handler answers only for its own" (mutant 14).
func TestServedContainmentLeaseAnswersOnlyForItsOwnCity(t *testing.T) {
	cityA, cfgA, sourceA, _ := convergedInfraCity(t)
	cityB, cfgB, sourceB, _ := convergedInfraCity(t)
	// convergedInfraCity points the work-store seam at whichever city it
	// just converged, so by the time both calls above return it names only
	// city B's store: a storageBootGate below for city A that still reads
	// through this seam (confirmInfraContainment's own re-check does) would
	// silently re-check city A's containment against city B's work store.
	// Dispatching by cityPath keeps each city on its own stub regardless of
	// call order (design review NIT).
	prevSource := openInfraMigrationSource
	openInfraMigrationSource = func(cityPath string) (beads.Store, error) {
		switch cityPath {
		case cityA:
			return sourceA, nil
		case cityB:
			return sourceB, nil
		default:
			return prevSource(cityPath)
		}
	}
	t.Cleanup(func() { openInfraMigrationSource = prevSource })

	routesA, err := storageBootGate(cityA, cfgA, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("city A refused: %v", err)
	}
	t.Cleanup(func() { _ = routesA.close() })
	routesB, err := storageBootGate(cityB, cfgB, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("city B refused: %v", err)
	}
	t.Cleanup(func() { _ = routesB.close() })

	publishBootContainmentLease(routesA)
	publishBootContainmentLease(routesB)

	registerResidencyRoutes(cityA, routesA, nil)
	t.Cleanup(func() { unregisterResidencyRoutes(cityA, routesA) })
	registerResidencyRoutes(cityB, routesB, nil)
	t.Cleanup(func() { unregisterResidencyRoutes(cityB, routesB) })

	leaseA, ok := servedContainmentLease(cityA)
	if !ok {
		t.Fatal("no lease served for city A")
	}
	leaseB, ok := servedContainmentLease(cityB)
	if !ok {
		t.Fatal("no lease served for city B")
	}
	if leaseA.Database == leaseB.Database {
		t.Fatal("fixture: both cities resolved the same binding database")
	}
	if leaseA.Database != routesA.target.Database {
		t.Errorf("city A's lease named database %q, want its own %q", leaseA.Database, routesA.target.Database)
	}
	if leaseB.Database != routesB.target.Database {
		t.Errorf("city B's lease named database %q, want its own %q", leaseB.Database, routesB.target.Database)
	}
}

// TestStrandRecheckSkipsTheReadWhileQuiescent is T6 (F3, mutant 17): no
// recheck runs while the city is quiescent, proved directly by counting
// work-store opens, not just by the absence of a published change.
func TestStrandRecheckSkipsTheReadWhileQuiescent(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	rec := events.NewFake()
	s := newStrandRecheck(routes, cityPath, rec, io.Discard, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}

	opens := 0
	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(path string) (beads.Store, error) {
		opens++
		return prev(path)
	}

	quiescent := true
	s.quiescent = func() bool { return quiescent }

	s.tick()
	if opens != 0 {
		t.Fatalf("a quiescent recheck opened the work store %d time(s), want 0", opens)
	}

	quiescent = false
	s.tick()
	if opens != 1 {
		t.Fatalf("a non-quiescent recheck opened the work store %d time(s), want exactly 1", opens)
	}
}

// TestContainmentLeaseWireDistinguishesServingFromServesDegraded is MUST 1:
// a refusing stranded verdict (HeldDifferent, Retained or an unfinished
// clear) and a degraded-serving one both name infraMigrationStranded and
// otherwise look alike on the wire, so a caller needs Serving and
// ServesDegraded themselves to tell which this is — Outcome and Stranded
// alone do not say.
func TestContainmentLeaseWireDistinguishesServingFromServesDegraded(t *testing.T) {
	basis := containmentBasis{Marker: "sha256:x"}
	target := infraBindingTarget{Binding: "infra", Database: "infra.db"}
	cases := []struct {
		name         string
		report       infraMigrationReport
		wantServing  bool
		wantDegraded bool
	}{
		{
			name:         "degraded: stranded with none of the serving-blockers",
			report:       infraMigrationReport{Outcome: infraMigrationStranded, Stranded: []string{"b1"}, basis: basis},
			wantServing:  false,
			wantDegraded: true,
		},
		{
			name: "refusing: stranded with an unfinished cleared note",
			report: infraMigrationReport{
				Outcome: infraMigrationStranded, Stranded: []string{"b1"}, ClearUnfinished: true, basis: basis,
			},
			wantServing:  false,
			wantDegraded: false,
		},
		{
			name: "refusing: stranded held different",
			report: infraMigrationReport{
				Outcome: infraMigrationStranded, Stranded: []string{"b1"}, HeldDifferent: []string{"b1"}, basis: basis,
			},
			wantServing:  false,
			wantDegraded: false,
		},
		{
			name: "refusing: stranded with a retained copy",
			report: infraMigrationReport{
				Outcome: infraMigrationStranded, Stranded: []string{"b1"}, Retained: []string{"b2"}, basis: basis,
			},
			wantServing:  false,
			wantDegraded: false,
		},
		{
			name:         "converged: serving, never degraded",
			report:       infraMigrationReport{Outcome: infraMigrationConverged, basis: basis},
			wantServing:  true,
			wantDegraded: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lease := newContainmentLease(target, c.report, 1000, time.Now())
			if lease == nil {
				t.Fatal("newContainmentLease returned nil for a leaseable outcome with a basis")
			}
			if lease.Serving != c.wantServing || lease.ServesDegraded != c.wantDegraded {
				t.Fatalf("Serving=%v ServesDegraded=%v, want Serving=%v ServesDegraded=%v",
					lease.Serving, lease.ServesDegraded, c.wantServing, c.wantDegraded)
			}
			wire := lease.wire(time.Now())
			if wire.Serving != c.wantServing || wire.ServesDegraded != c.wantDegraded {
				t.Fatalf("wire: serving=%v serves_degraded=%v, want serving=%v serves_degraded=%v",
					wire.Serving, wire.ServesDegraded, c.wantServing, c.wantDegraded)
			}
		})
	}
}

// TestStrandRecheckExtendsLeaseOnTransientFailure is MUST 2's Q3 path: a
// transient recheck failure must not drop the lease a definitive verdict
// already published. It re-stores it with RecheckFailing set and its
// freshness stretched to the full strandRecheckFreshCap, rather than let it
// age on the interval while this process cannot confirm it either way.
func TestStrandRecheckExtendsLeaseOnTransientFailure(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })
	publishBootContainmentLease(routes)
	before := routes.lease.Load()
	if before == nil {
		t.Fatal("no lease published at boot")
	}

	rec := events.NewFake()
	s := newStrandRecheck(routes, cityPath, rec, io.Discard, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}

	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return nil, errors.New(`Get "https://hosted-bd/v1/issues": dial tcp: i/o timeout`)
	}
	s.tick()

	after := routes.lease.Load()
	if after == nil {
		t.Fatal("a transient recheck failure withdrew the lease; it must only re-date it")
	}
	if after.Outcome != before.Outcome || after.TakenAt != before.TakenAt {
		t.Fatalf("a transient failure replaced the definitive verdict: outcome %q->%q, TakenAt %v->%v",
			before.Outcome, after.Outcome, before.TakenAt, after.TakenAt)
	}
	if !after.RecheckFailing {
		t.Error("RecheckFailing was not set on a transient failure")
	}
	if after.FreshMS != strandRecheckFreshCap.Milliseconds() {
		t.Errorf("FreshMS = %d, want the full cap %d once a recheck is failing",
			after.FreshMS, strandRecheckFreshCap.Milliseconds())
	}
	if !after.LastAttemptAt.After(before.LastAttemptAt) {
		t.Errorf("LastAttemptAt did not advance: before=%v after=%v", before.LastAttemptAt, after.LastAttemptAt)
	}
}

// TestStrandRecheckWithdrawsLeaseOnAStandingFault is MUST 2's other half
// (kills mutant d3_standing_fault_publishes): a standing, non-transient
// uncheckable means this process no longer knows whether the binding still
// holds, so the lease is withdrawn (lease.Store(nil)) rather than kept or
// republished — a caller must fall back to its own full check.
func TestStrandRecheckWithdrawsLeaseOnAStandingFault(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })
	publishBootContainmentLease(routes)
	if routes.lease.Load() == nil {
		t.Fatal("no lease published at boot")
	}

	rec := events.NewFake()
	s := newStrandRecheck(routes, cityPath, rec, io.Discard, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}

	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	// "disk i/o error" is a storeStandingMarkers entry: isTransientStoreFault
	// returns false for it, so this is a standing fault on the very first
	// tick (s.faulted starts false too).
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return nil, errors.New("disk i/o error")
	}
	s.tick()

	if lease := routes.lease.Load(); lease != nil {
		t.Fatalf("a standing fault left a lease published: %+v", lease)
	}
}

// TestHandleControllerConnStorageVerdict is SHOULD 6's socket-level test
// (reviewer probe P1): the storage-verdict case end to end over a real
// net.Pipe, covering the wrong city (no reply), v:1, and the served age
// advancing on every answer rather than being frozen at publish time.
// Must kill mutants c1 (serves the wrong city), c2 (freezes age_ms at
// zero) and w3 (hardcodes v:0).
func TestHandleControllerConnStorageVerdict(t *testing.T) {
	cityA, cfgA, _, _ := convergedInfraCity(t)
	routesA, err := storageBootGate(cityA, cfgA, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("city A refused: %v", err)
	}
	t.Cleanup(func() { _ = routesA.close() })
	publishBootContainmentLease(routesA)
	registerResidencyRoutes(cityA, routesA, nil)
	t.Cleanup(func() { unregisterResidencyRoutes(cityA, routesA) })

	verdict := func(cityPath string) []byte {
		t.Helper()
		server, client := net.Pipe()
		defer client.Close() //nolint:errcheck
		go handleControllerConn(server, cityPath, controllerHostingMode(""), func() {}, nil, nil, nil, nil, nil)
		if _, err := client.Write([]byte(storageVerdictCommand + "\n")); err != nil {
			t.Fatal(err)
		}
		client.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		data, _ := io.ReadAll(bufio.NewReader(client))
		return data
	}

	// The wrong city: nothing registered for it, so no reply at all —
	// never city A's lease answering for someone else's cityPath.
	if got := verdict(t.TempDir()); len(got) != 0 {
		t.Fatalf("an unregistered city got a reply: %q", got)
	}

	first := verdict(cityA)
	var m1 map[string]any
	if err := json.Unmarshal(first, &m1); err != nil {
		t.Fatalf("decode %q: %v", first, err)
	}
	if v, ok := m1["v"].(float64); !ok || v != 1 {
		t.Fatalf("v = %v, want 1", m1["v"])
	}
	if m1["database"] != routesA.target.Database || m1["outcome"] != "converged" {
		t.Fatalf("reply %v", m1)
	}
	age1, ok := m1["age_ms"].(float64)
	if !ok {
		t.Fatalf("no age_ms in %v", m1)
	}

	time.Sleep(20 * time.Millisecond)

	second := verdict(cityA)
	var m2 map[string]any
	if err := json.Unmarshal(second, &m2); err != nil {
		t.Fatalf("decode %q: %v", second, err)
	}
	age2, ok := m2["age_ms"].(float64)
	if !ok {
		t.Fatalf("no age_ms in %v", m2)
	}
	if age2 <= age1 {
		t.Fatalf("age_ms did not advance across two reads: %v then %v", age1, age2)
	}
}

// TestContainmentLeaseIsPublishedByCityRuntimeRun is SHOULD 6's wiring
// check for the boot publish itself (kills mutant w1_no_boot_publish_in_run):
// CityRuntime.run calls publishBootContainmentLease before it returns, not
// only something a test that calls publishBootContainmentLease directly
// would ever see.
func TestContainmentLeaseIsPublishedByCityRuntimeRun(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	cityCfg, err := config.Load(osFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		// No on-disk city.toml for this fixture: run with the same *City
		// confirmInfraContainment already converged against.
		cityCfg = cfg
	}

	sp := runtime.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr, runtimeErr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		Cfg:      cityCfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops: newDrainOps(sp),
		Rec:  events.Discard,
		OnStarted: func() {
			cancel()
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if runtimeErr != nil {
		t.Fatalf("building the city runtime: %v", runtimeErr)
	}
	cr.storageRoutes = routes
	cs := newControllerState(context.Background(), cityCfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	cr.run(ctx)

	if routes.lease.Load() == nil {
		t.Fatal("CityRuntime.run did not publish the boot containment lease")
	}
}

// TestStrandRecheckWiresQuiescenceFromCityRuntime is SHOULD 6's wiring
// check for quiescence (kills mutant q1_no_wiring):
// CityRuntime.startStrandRecheck sets s.quiescent to
// cr.beadsQuiescent.Load, so a quiescent city's recheck skips the
// work-store read the same way a directly-constructed strandRecheck's does
// (TestStrandRecheckSkipsTheReadWhileQuiescent) — proved here through the
// wiring method itself, not by setting s.quiescent by hand.
func TestStrandRecheckWiresQuiescenceFromCityRuntime(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	prevInterval := strandRecheckInterval
	strandRecheckInterval = 10 * time.Millisecond
	t.Cleanup(func() { strandRecheckInterval = prevInterval })

	var opens atomic.Int64
	prevOpen := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prevOpen })
	openInfraMigrationSource = func(p string) (beads.Store, error) {
		opens.Add(1)
		return prevOpen(p)
	}

	cr := &CityRuntime{storageRoutes: routes, cityPath: cityPath, rec: events.NewFake(), stderr: io.Discard, logPrefix: "gc start"}
	cr.beadsQuiescent.Store(true)

	stop := cr.startStrandRecheck(context.Background())
	time.Sleep(60 * time.Millisecond)
	stop()
	if n := opens.Load(); n != 0 {
		t.Fatalf("a quiescent city's wired-up recheck opened the work store %d time(s), want 0", n)
	}

	cr.beadsQuiescent.Store(false)
	stop = cr.startStrandRecheck(context.Background())
	time.Sleep(60 * time.Millisecond)
	stop()
	if n := opens.Load(); n == 0 {
		t.Fatal("a non-quiescent city's wired-up recheck never opened the work store")
	}
}

// TestContainmentLeaseRejectsNonDefinitiveOrBasislessReports is SHOULD 4:
// newContainmentLease itself rejects a non-definitive outcome, not only by
// basis emptiness — a standing fault confirmInfraContainment discovers
// mid-check (after its own work-store read) keeps its basis populated
// (that function's own comment on why), so the basis check alone would not
// catch it. Both guards are tested independently here.
func TestContainmentLeaseRejectsNonDefinitiveOrBasislessReports(t *testing.T) {
	target := infraBindingTarget{Binding: "infra", Database: "infra.db"}
	basis := containmentBasis{Marker: "sha256:x"}
	cases := []struct {
		name   string
		report infraMigrationReport
	}{
		{"a basisless definitive outcome", infraMigrationReport{Outcome: infraMigrationConverged}},
		{"a standing fault discovered after the read, basis populated", infraMigrationReport{Outcome: infraMigrationUncheckable, basis: basis}},
		{"an identity mismatch, basis populated", infraMigrationReport{Outcome: infraMigrationIdentityMismatch, basis: basis}},
		{"not configured", infraMigrationReport{Outcome: infraMigrationNotConfigured, basis: basis}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if lease := newContainmentLease(target, c.report, 1000, time.Now()); lease != nil {
				t.Fatalf("newContainmentLease published a lease for %v: %+v", c.report.Outcome, lease)
			}
		})
	}
	// Sanity: the positive case, same basis, a definitive outcome, does
	// publish — the two rejections above are not just everything refusing.
	if lease := newContainmentLease(target, infraMigrationReport{Outcome: infraMigrationConverged, basis: basis}, 1000, time.Now()); lease == nil {
		t.Fatal("a definitive outcome with a basis was not published")
	}
}

// TestContainmentBasisCapturesEveryField is SHOULD 7's T2-style basis
// coverage: each of marker, work_store, cleared, database and classifier
// is asserted at its true captured value, not the fixed wrong one each of
// mutants b1-b5 forces (an empty marker hash, an empty work-store string,
// "absent" regardless of a real cleared note, an empty database stamp, an
// empty classifier). A dev build's classifier also carries
// classifierBasisNonce, so two different nonces produce two different
// classifier strings (kills b8_nonce_dropped, which stops the nonce from
// ever reaching the string).
func TestContainmentBasisCapturesEveryField(t *testing.T) {
	cityPath, _, _, target := convergedInfraCity(t)
	if err := writeInfraClearedNote(cityPath, infraClearedNote{Binding: target.Binding, Database: target.Database, Complete: true}); err != nil {
		t.Fatalf("writing the cleared note: %v", err)
	}

	basis, err := captureContainmentBasis(cityPath, target)
	if err != nil {
		t.Fatalf("captureContainmentBasis: %v", err)
	}
	if basis.Marker == "sha256:" || basis.Marker == "" {
		t.Errorf("Marker = %q, want a real marker digest (b1)", basis.Marker)
	}
	if basis.WorkStore == "" {
		t.Errorf("WorkStore = %q, want a real identity (b2)", basis.WorkStore)
	}
	if basis.Cleared == "absent" || basis.Cleared == "" {
		t.Errorf("Cleared = %q, want a real stamp for the note just written (b3)", basis.Cleared)
	}
	if basis.Database == "" {
		t.Errorf("Database = %q, want a real stamp (b4)", basis.Database)
	}
	if basis.Classifier == "" {
		t.Errorf("Classifier = %q, want a real classifier digest (b5)", basis.Classifier)
	}

	prevNonce := classifierBasisNonce
	t.Cleanup(func() { classifierBasisNonce = prevNonce })
	classifierBasisNonce = "nonce-a"
	a := classifierBasisString()
	classifierBasisNonce = "nonce-b"
	b := classifierBasisString()
	if a == b {
		t.Fatalf("classifierBasisString() did not change with the nonce: %q == %q (b8)", a, b)
	}
}

// TestContainmentLeaseDropsStrandedAndDeclassifiableWhenOverCap is the NIT
// coverage (and kills w2_no_cap): a Stranded list over leaseListCap cannot
// be truncated and still vouch for itself, so newContainmentLease drops it
// (and, per the coupling fix at its call site, Declassifiable along with
// it) entirely rather than publish a partial list, and reports
// Complete = false.
func TestContainmentLeaseDropsStrandedAndDeclassifiableWhenOverCap(t *testing.T) {
	target := infraBindingTarget{Binding: "infra", Database: "infra.db"}
	basis := containmentBasis{Marker: "sha256:x"}

	stranded := make([]string, leaseListCap+5)
	for i := range stranded {
		stranded[i] = fmt.Sprintf("bead-%d", i)
	}
	declassifiable := map[string][]string{stranded[0]: {"cleared-by-note"}}

	report := infraMigrationReport{
		Outcome:        infraMigrationStranded,
		Stranded:       stranded,
		Declassifiable: declassifiable,
		basis:          basis,
	}
	lease := newContainmentLease(target, report, 1000, time.Now())
	if lease == nil {
		t.Fatal("newContainmentLease returned nil for a leaseable stranded report")
	}
	if lease.Stranded != nil {
		t.Fatalf("Stranded = %v, want nil once over leaseListCap (w2)", lease.Stranded)
	}
	if lease.Complete {
		t.Fatal("Complete = true for an over-cap Stranded list (w2)")
	}
	if lease.Declassifiable != nil {
		t.Fatalf("Declassifiable = %v, want nil once Stranded is dropped", lease.Declassifiable)
	}
}
