package main

// Tests for the controller containment lease (#9237 follow-up, slice 1):
// boot-check-design.md §4.7 T5 (controller) and T6 (quiescence).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
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
// test catches it directly, by comparing the two possible answers. The
// capture is a step of its own in confirmInfraContainment, ahead of the
// whole of checkInfraContainment, so the same order also covers the
// manifest read: checkInfraContainment has no point between that read and
// this List where the capture could move without being rewritten into it.
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

	// Back-date the published lease's TakenAt instead of a wall-clock sleep:
	// age_ms is now.Sub(TakenAt), so this advances it deterministically,
	// with no risk of a sub-millisecond gap between two fast net.Pipe
	// round trips leaving age_ms unchanged (c2's bug either way: it
	// hardcodes age_ms to 0 regardless of TakenAt).
	backdated := *routesA.lease.Load()
	backdated.TakenAt = backdated.TakenAt.Add(-50 * time.Millisecond)
	routesA.lease.Store(&backdated)

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
	opened := make(chan struct{}, 8)
	prevOpen := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prevOpen })
	openInfraMigrationSource = func(p string) (beads.Store, error) {
		opens.Add(1)
		opened <- struct{}{}
		return prevOpen(p)
	}

	cr := &CityRuntime{storageRoutes: routes, cityPath: cityPath, rec: events.NewFake(), stderr: io.Discard, logPrefix: "gc start"}
	cr.beadsQuiescent.Store(true)

	// A quiescent city's ticks skip the open entirely (no signal to wait
	// on for that): give the ticker a bounded, interruptible window to
	// fire — select on time.After rather than a fixed time.Sleep, so a
	// loaded machine only ever makes this slower, never flaky — then
	// check nothing opened.
	stop := cr.startStrandRecheck(context.Background())
	select {
	case <-opened:
		t.Fatal("a quiescent city's wired-up recheck opened the work store")
	case <-time.After(200 * time.Millisecond):
	}
	stop()
	if n := opens.Load(); n != 0 {
		t.Fatalf("a quiescent city's wired-up recheck opened the work store %d time(s), want 0", n)
	}

	cr.beadsQuiescent.Store(false)
	stop = cr.startStrandRecheck(context.Background())
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("a non-quiescent city's wired-up recheck never opened the work store")
	}
	stop()
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
// empty classifier). The classifier of a build with no commit stamp also
// carries classifierBasisNonce, so two different nonces produce two
// different classifier strings (kills b8_nonce_dropped, which stops the
// nonce from ever reaching the string).
func TestContainmentBasisCapturesEveryField(t *testing.T) {
	setGCBuildIdentity(t, "dev", "unknown", "(devel)")
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

// TestContainmentLeaseDropsPresumedReusedAndCapsLostCrossEdgesWhenOverCap is
// round 2's NIT fix: PresumedReused over leaseListCap is dropped to nil the
// same way Stranded is, not truncated to a partial list that could be read
// as the whole one, and LostCrossEdges gets the identical treatment. Either
// one being over cap, on its own, marks the lease incomplete.
func TestContainmentLeaseDropsPresumedReusedAndCapsLostCrossEdgesWhenOverCap(t *testing.T) {
	target := infraBindingTarget{Binding: "infra", Database: "infra.db"}
	basis := containmentBasis{Marker: "sha256:x"}

	overCap := make([]string, leaseListCap+5)
	for i := range overCap {
		overCap[i] = fmt.Sprintf("bead-%d", i)
	}

	t.Run("presumed reused", func(t *testing.T) {
		report := infraMigrationReport{Outcome: infraMigrationStranded, Stranded: []string{"b1"}, PresumedReused: overCap, basis: basis}
		lease := newContainmentLease(target, report, 1000, time.Now())
		if lease == nil {
			t.Fatal("newContainmentLease returned nil for a leaseable stranded report")
		}
		if lease.PresumedReused != nil {
			t.Fatalf("PresumedReused = %v, want nil once over leaseListCap, not a truncated list", lease.PresumedReused)
		}
		if lease.Complete {
			t.Fatal("Complete = true for an over-cap PresumedReused list")
		}
	})

	t.Run("lost cross edges", func(t *testing.T) {
		report := infraMigrationReport{Outcome: infraMigrationStranded, Stranded: []string{"b1"}, LostCrossEdges: overCap, basis: basis}
		lease := newContainmentLease(target, report, 1000, time.Now())
		if lease == nil {
			t.Fatal("newContainmentLease returned nil for a leaseable stranded report")
		}
		if lease.LostCrossEdges != nil {
			t.Fatalf("LostCrossEdges = %v, want nil once over leaseListCap", lease.LostCrossEdges)
		}
		if lease.Complete {
			t.Fatal("Complete = true for an over-cap LostCrossEdges list")
		}
	})
}

// basisFieldDiff names which containmentBasis fields differ between a and b.
func basisFieldDiff(a, b containmentBasis) []string {
	var d []string
	if a.Marker != b.Marker {
		d = append(d, "marker")
	}
	if a.Manifest != b.Manifest {
		d = append(d, "manifest")
	}
	if a.Cleared != b.Cleared {
		d = append(d, "cleared")
	}
	if a.Database != b.Database {
		d = append(d, "database")
	}
	if a.WorkStore != b.WorkStore {
		d = append(d, "work_store")
	}
	if a.Classifier != b.Classifier {
		d = append(d, "classifier")
	}
	return d
}

// TestCaptureContainmentBasisFieldSensitivity is round 2's SHOULD B,
// reviewer probe R10: changing one real underlying fact at a time changes
// exactly its own basis field and no other. A field read as a constant
// (k1-k5), from the wrong file (k6, k7) or the wrong path (k8, a directory
// that a database replacement never touches) never moves when its own fact
// does, and the manifest's stamp catches a temp+rename that keeps the same
// (dev, ino) (b6, already covered by
// TestConfirmInfraContainmentCapturesTheBasisBeforeTheRead but pinned again
// here end to end). The build has no commit stamp, so the classifier field
// carries the nonce the last step changes.
func TestCaptureContainmentBasisFieldSensitivity(t *testing.T) {
	setGCBuildIdentity(t, "dev", "unknown", "(devel)")
	cityPath, _, _, target := convergedInfraCity(t)
	capture := func() containmentBasis {
		b, err := captureContainmentBasis(cityPath, target)
		if err != nil {
			t.Fatalf("captureContainmentBasis: %v", err)
		}
		return b
	}
	prev := capture()
	step := func(name, want string, do func()) {
		t.Helper()
		do()
		now := capture()
		if got := basisFieldDiff(prev, now); len(got) != 1 || got[0] != want {
			t.Errorf("%s changed %v, want exactly [%s]", name, got, want)
		}
		prev = now
	}

	step("marker content", "marker", func() {
		data, err := os.ReadFile(target.MarkerPath())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target.MarkerPath(), append([]byte("# probe\n"), data...), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	step("manifest temp+rename", "manifest", func() { rewriteInfraManifestInPlace(t, target) })
	step("cleared note written", "cleared", func() {
		if err := writeInfraClearedNote(cityPath, infraClearedNote{Binding: target.Binding, Database: target.Database, Complete: true}); err != nil {
			t.Fatal(err)
		}
	})
	step("database replaced by rename", "database", func() {
		data, err := os.ReadFile(target.Database)
		if err != nil {
			t.Fatal(err)
		}
		tmp := target.Database + ".rewrite"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, target.Database); err != nil {
			t.Fatal(err)
		}
	})
	step("work-store identity", "work_store", func() { writeScopeProjectID(t, cityPath, "prj-r10") })
	step("classifier nonce", "classifier", func() {
		old := classifierBasisNonce
		t.Cleanup(func() { classifierBasisNonce = old })
		classifierBasisNonce = "r10-nonce"
	})
}

// TestConfirmInfraContainmentKeepsBasisWhenTheManifestWontRead is round 2's
// SHOULD B: a malformed manifest fails readInfraCopyManifestRecord, but the
// basis was captured before that read (this function's own comment on why)
// and stays on the uncheckable report it returns — never emptied just
// because the read after it failed.
func TestConfirmInfraContainmentKeepsBasisWhenTheManifestWontRead(t *testing.T) {
	cityPath, _, _, target := convergedInfraCity(t)
	if err := os.Remove(target.ManifestPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target.ManifestPath(), 0o755); err != nil {
		t.Fatal(err)
	}

	report := confirmInfraContainment(cityPath, target, "x", io.Discard)
	if report.Outcome != infraMigrationUncheckable {
		t.Fatalf("fixture: outcome = %v, want uncheckable with a directory where the manifest belongs", report.Outcome)
	}
	if report.basis.empty() {
		t.Fatal("a malformed manifest emptied the basis captured before the read (m13b)")
	}
}

// TestPublishBootContainmentLeaseTakenAtIsTheBootCheckedAt is round 2's
// SHOULD 2 (a1): the boot lease's TakenAt names when the boot check itself
// ran (routes.bootVerdict.CheckedAt), not whenever publishBootContainmentLease
// happens to be called — which can be later, once the rest of boot is done.
func TestPublishBootContainmentLeaseTakenAtIsTheBootCheckedAt(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	publishBootContainmentLease(routes)
	lease := routes.lease.Load()
	if lease == nil {
		t.Fatal("no lease published at boot")
	}
	if !lease.TakenAt.Equal(routes.bootVerdict.CheckedAt) {
		t.Fatalf("lease.TakenAt = %v, want bootVerdict.CheckedAt %v (a1)", lease.TakenAt, routes.bootVerdict.CheckedAt)
	}
}

// TestStorageBootGateChecksCheckedAtBeforeTheCheckRuns is round 2's SHOULD 2
// (a2): CheckedAt is stamped from a clock read before the check runs
// (storageBootGate's own comment on why), not after. Rather than sleep inside
// the check to manufacture a measurable gap, the stub itself is the clock:
// it records its own entry time, so this only needs CheckedAt to precede
// that recording — true on any schedule, not just a slow one.
func TestStorageBootGateChecksCheckedAtBeforeTheCheckRuns(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	prev := openInfraMigrationSource
	var checkEnteredAt time.Time
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(p string) (beads.Store, error) {
		checkEnteredAt = time.Now()
		return prev(p)
	}

	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	checkedAt := routes.bootVerdict.CheckedAt
	if checkEnteredAt.IsZero() {
		t.Fatal("fixture: the check never entered the stub")
	}
	if checkedAt.After(checkEnteredAt) {
		t.Fatalf("CheckedAt is %v, which is after the check itself started (%v) (a2)", checkedAt, checkEnteredAt)
	}
}

// TestStrandRecheckTickFreshMSMatchesTheAdaptiveFormula is slice 2's
// successor to round 2's i4 (which pinned FreshMS at the fixed-cadence cap,
// true only while interval was hardcoded at strandRecheckInterval). With
// the adaptive cadence wired up (design doc §4.3, carry-over 1), a fast
// definitive tick's lease gets
// strandRecheckFreshness(strandRecheckNextInterval(d), d) for its own
// measured duration d, not the 10-minute cap — a city whose
// check is cheap gets a shorter, not longer, freshness budget, because its
// own recheck comes back around sooner.
func TestStrandRecheckTickFreshMSMatchesTheAdaptiveFormula(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	s := newStrandRecheck(routes, cityPath, events.NewFake(), io.Discard, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}
	s.tick()

	lease := routes.lease.Load()
	if lease == nil {
		t.Fatal("a definitive tick published no lease")
	}
	want := strandRecheckFreshness(strandRecheckNextInterval(s.lastCheckDuration), s.lastCheckDuration).Milliseconds()
	if lease.FreshMS != want {
		t.Fatalf("FreshMS = %d, want %d from the adaptive formula (measured duration %s)", lease.FreshMS, want, s.lastCheckDuration)
	}
	if lease.FreshMS >= strandRecheckFreshCap.Milliseconds() {
		t.Fatalf("FreshMS = %d, want well under the cap %d for a fast fixture check", lease.FreshMS, strandRecheckFreshCap.Milliseconds())
	}
}

// TestHandleControllerConnStorageVerdictAfterATransientTick is round 2's
// MUST 2 socket-level pin (t8, t9): once a transient recheck failure has
// extended the lease, the wire reply actually carries recheck_failing (not
// hardcoded false) and last_attempt_ms measured from LastAttemptAt (which
// moved), not from TakenAt (which did not) — the two must read as
// different ages.
func TestHandleControllerConnStorageVerdictAfterATransientTick(t *testing.T) {
	cityA, cfgA, _, _ := convergedInfraCity(t)
	routesA, err := storageBootGate(cityA, cfgA, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("city A refused: %v", err)
	}
	t.Cleanup(func() { _ = routesA.close() })
	publishBootContainmentLease(routesA)
	registerResidencyRoutes(cityA, routesA, nil)
	t.Cleanup(func() { unregisterResidencyRoutes(cityA, routesA) })

	s := newStrandRecheck(routesA, cityA, events.NewFake(), io.Discard, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}

	// Back-date the boot lease's TakenAt rather than sleep for a real gap:
	// last_attempt_ms and age_ms both round to whole milliseconds, so the
	// two ages need a genuine difference, not merely two clock reads that
	// happen to land in the same millisecond.
	boot := routesA.lease.Load()
	if boot == nil {
		t.Fatal("fixture: no boot lease published")
	}
	backdated := *boot
	backdated.TakenAt = backdated.TakenAt.Add(-50 * time.Millisecond)
	backdated.LastAttemptAt = backdated.TakenAt
	routesA.lease.Store(&backdated)

	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return nil, errors.New(`Get "https://hosted-bd/v1/issues": dial tcp: i/o timeout`)
	}
	s.tick()
	openInfraMigrationSource = prev

	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck
	go handleControllerConn(server, cityA, controllerHostingMode(""), func() {}, nil, nil, nil, nil, nil)
	if _, err := client.Write([]byte(storageVerdictCommand + "\n")); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	data, _ := io.ReadAll(bufio.NewReader(client))

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	if failing, ok := m["recheck_failing"].(bool); !ok || !failing {
		t.Fatalf("recheck_failing = %v, want true after a transient tick (t8)", m["recheck_failing"])
	}
	lastAttemptMS, ok := m["last_attempt_ms"].(float64)
	if !ok {
		t.Fatalf("no last_attempt_ms in %v", m)
	}
	ageMS, ok := m["age_ms"].(float64)
	if !ok {
		t.Fatalf("no age_ms in %v", m)
	}
	if lastAttemptMS >= ageMS {
		t.Fatalf("last_attempt_ms = %v, want less than age_ms = %v: LastAttemptAt is more recent than TakenAt (t9)", lastAttemptMS, ageMS)
	}
}

// setGCBuildIdentity runs the test as a gc whose normalized version is v,
// built as built (setGCVersion), from commit stamp c.
func setGCBuildIdentity(t *testing.T, v, c, built string) {
	t.Helper()
	setGCVersion(t, v, built)
	prev := commit
	commit = c
	t.Cleanup(func() { commit = prev })
}

// TestClassifierBasisStringNonceFollowsTheBuildIdentity: a build whose
// commit stamp names its code — known, at least minAbbrevCommitLen long,
// from a clean tree, not a Go pseudo-version — gives the same classifier
// string in every process whatever its version, since an untagged `make
// build` stamps version=dev beside a clean commit on every Makefile-built
// fleet. A build with no such identity carries classifierBasisNonce, so no
// other process's basis ever matches it; the pseudo-version row has an
// ordinary release version and commit and still gets the nonce.
func TestClassifierBasisStringNonceFollowsTheBuildIdentity(t *testing.T) {
	cases := []struct {
		name                   string
		version, commit, built string
		trusted                bool
	}{
		{"an untagged make build of a clean commit", "dev", "c91f5744a2", "(devel)", true},
		{"a release build", "1.2.3", "c91f5744a2", "1.2.3", true},
		{"a full toolchain revision", "dev", "c91f5744a2b3c4d5e6f708192a3b4c5d6e7f8091", "(devel)", true},
		{"a dirty tree", "dev", "c91f5744a2" + dirtySuffix, "(devel)", false},
		{"no commit stamped", "dev", "unknown", "(devel)", false},
		{"an empty commit stamp", "dev", "", "(devel)", false},
		{"an abbreviation too short to name a commit", "dev", "c91f57", "(devel)", false},
		{"a Go pseudo-version", "1.2.3", "abc1234567", "v1.2.3-0.20210101000000-abcdef012345", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setGCBuildIdentity(t, c.version, c.commit, c.built)
			prevNonce := classifierBasisNonce
			t.Cleanup(func() { classifierBasisNonce = prevNonce })
			classifierBasisNonce = "nonce-a"
			a := classifierBasisString()
			classifierBasisNonce = "nonce-b"
			b := classifierBasisString()
			if c.trusted && a != b {
				t.Fatalf("two processes of one clean, known build disagree: %q != %q", a, b)
			}
			if !c.trusted && a == b {
				t.Fatalf("a build with no trustworthy identity matches another process: %q == %q", a, b)
			}
		})
	}

	// Trusting the commit stamp must not make two different commits agree.
	setGCBuildIdentity(t, "dev", "c91f5744a2", "(devel)")
	one := classifierBasisString()
	commit = "d02e6855b3"
	if other := classifierBasisString(); other == one {
		t.Fatalf("builds of two different commits share a classifier string: %q", one)
	}
}

// convergedInfraCityOfProject is convergedInfraCity for a work store with
// project id project, so its convergence marker records an identity that a
// recreated .beads (writeScopeProjectID again) contradicts.
func convergedInfraCityOfProject(t *testing.T, project string) (cityPath string, cfg *config.City, target infraBindingTarget) {
	t.Helper()
	cityPath = t.TempDir()
	writeScopeProjectID(t, cityPath, project)
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	cfg = infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	return cityPath, cfg, mustResolveInfraTarget(t, cityPath, cfg)
}

// TestStrandRecheckWithdrawsTheLeaseWhenTheWorkStoreIsRecreated is B15 for
// the lease. A city's .beads is recreated (a new project id) under a live
// controller. Boot refuses that city as identity-mismatch, but the recheck
// runs no identity hold of its own, so each tick would otherwise stamp the
// new identity into its basis and keep serving a fresh, serving, converged
// lease that matches the files. The tick withdraws the lease instead,
// prints why once rather than on every tick, and publishes again once the
// identity agrees with the marker again.
func TestStrandRecheckWithdrawsTheLeaseWhenTheWorkStoreIsRecreated(t *testing.T) {
	cityPath, cfg, target := convergedInfraCityOfProject(t, "prj-original")
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })
	publishBootContainmentLease(routes)
	if routes.lease.Load() == nil {
		t.Fatal("fixture: the boot verdict published no lease")
	}

	var stderr bytes.Buffer
	s := newStrandRecheck(routes, cityPath, events.NewFake(), &stderr, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}
	s.tick()
	if routes.lease.Load() == nil {
		t.Fatal("fixture: a definitive tick on the original work store published no lease")
	}

	writeScopeProjectID(t, cityPath, "prj-recreated")
	if boot := confirmInfraConvergence(cityPath, target, "gc start", io.Discard); boot.Outcome != infraMigrationIdentityMismatch {
		t.Fatalf("fixture: boot's own check of the recreated work store = %v, want %v", boot.Outcome, infraMigrationIdentityMismatch)
	}
	for tick := 1; tick <= 2; tick++ {
		s.tick()
		if lease := routes.lease.Load(); lease != nil {
			t.Fatalf("tick %d served a lease for a work store the marker's identity contradicts: outcome=%s serving=%v complete=%v work_store=%s",
				tick, lease.Outcome, lease.Serving, lease.Complete, lease.Basis.WorkStore)
		}
	}
	const withdrawn = "serves no containment lease"
	if got := strings.Count(stderr.String(), withdrawn); got != 1 {
		t.Errorf("two ticks printed the withdrawal %d times, want once:\n%s", got, stderr.String())
	}
	for _, want := range []string{"prj-original", "prj-recreated"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the withdrawal line does not name %q:\n%s", want, stderr.String())
		}
	}

	writeScopeProjectID(t, cityPath, "prj-original")
	s.tick()
	if routes.lease.Load() == nil {
		t.Fatal("a definitive tick on the original work store again published no lease")
	}
}

// TestCaptureContainmentBasisRefusesAContradictingWorkStoreIdentity pins
// the capture's own identity check directly: no basis for a work store the
// marker's recorded identity contradicts, nor for a marker whose identity
// line will not parse, while a legacy marker, which records no identity,
// contradicts nothing, as it does for boot's hold.
func TestCaptureContainmentBasisRefusesAContradictingWorkStoreIdentity(t *testing.T) {
	cityPath, _, target := convergedInfraCityOfProject(t, "prj-original")
	if _, err := captureContainmentBasis(cityPath, target); err != nil {
		t.Fatalf("fixture: the original work store's capture failed: %v", err)
	}

	writeScopeProjectID(t, cityPath, "prj-recreated")
	basis, err := captureContainmentBasis(cityPath, target)
	if err == nil || !basis.empty() {
		t.Fatalf("capturing a recreated work store = (%+v, %v), want an empty basis and an error", basis, err)
	}
	for _, want := range []string{target.MarkerPath(), "prj-original", "prj-recreated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the capture error %q does not name %q", err, want)
		}
	}

	data, err := os.ReadFile(target.MarkerPath())
	if err != nil {
		t.Fatal(err)
	}
	var legacy []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, workStoreIdentityLinePrefix) {
			legacy = append(legacy, line)
		}
	}
	if len(legacy) == len(strings.Split(string(data), "\n")) {
		t.Fatalf("fixture: the marker records no work-store line to remove:\n%s", data)
	}
	legacyMarker := strings.Join(legacy, "\n")
	if err := os.WriteFile(target.MarkerPath(), []byte(legacyMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := captureContainmentBasis(cityPath, target); err != nil {
		t.Fatalf("a legacy marker, which records no work-store identity, failed the capture: %v", err)
	}

	if err := os.WriteFile(target.MarkerPath(), []byte(workStoreIdentityLinePrefix+"project=%zz\n"+legacyMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	if basis, err := captureContainmentBasis(cityPath, target); err == nil || !basis.empty() {
		t.Fatalf("capturing a marker whose work-store line will not parse = (%+v, %v), want an empty basis and an error", basis, err)
	}
}

// TestLegacyMarkerBackfillPublishesNoBootLease is round 2's NIT (n3): the
// boot that backfills a legacy marker's work-store identity drops its own
// report's basis right after the write (confirmInfraConvergence's own
// comment on why: the basis it captured names a marker this process just
// rewrote underneath it), so that boot publishes no lease at all. The very
// next recheck tick captures a fresh basis and does publish one.
func TestLegacyMarkerBackfillPublishesNoBootLease(t *testing.T) {
	cityPath := t.TempDir()
	writeScopeProjectID(t, cityPath, "prj-legacy")
	cfg := infraSplitConfig(filepath.Join(cityPath, "store"))
	stubInfraMigrationSource(t)
	target := mustResolveInfraTarget(t, cityPath, cfg)

	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("seeding the marker: %v", err)
	}
	_ = routes.close()

	data, err := os.ReadFile(target.MarkerPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// A legacy marker carries only the write-time line, no work-store
	// identity line: what a pre-identity gc wrote.
	if err := os.WriteFile(target.MarkerPath(), []byte(lines[len(lines)-1]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	routes, err = storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the legacy-marker city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })
	if routes.bootVerdict.Outcome != infraMigrationConverged {
		t.Fatalf("fixture: legacy-marker boot outcome = %v, want converged", routes.bootVerdict.Outcome)
	}

	publishBootContainmentLease(routes)
	if lease := routes.lease.Load(); lease != nil {
		t.Fatalf("a legacy-marker backfill boot published a lease: %+v (n3)", lease)
	}

	s := newStrandRecheck(routes, cityPath, events.NewFake(), io.Discard, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}
	s.tick()
	if routes.lease.Load() == nil {
		t.Fatal("the first recheck tick after a legacy-marker backfill published no lease")
	}
}

// TestStrandRecheckWithdrawsStaleLeaseWhenTheBasisCaptureFails is round 2's
// SHOULD A, reviewer probe R6: a definitive tick whose basis capture failed
// gets no lease from newContainmentLease (nil), and publishLease must store
// that nil rather than silently leave an earlier, now possibly wrong,
// converged lease being served. The tick must be definitive (its stranded
// event says so), or the standing-fault branch would withdraw the lease and
// leave publishLease untested; and it says why it serves none.
func TestStrandRecheckWithdrawsStaleLeaseWhenTheBasisCaptureFails(t *testing.T) {
	cityPath, cfg, source, target := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	rec := events.NewFake()
	var stderr bytes.Buffer
	s := newStrandRecheck(routes, cityPath, rec, &stderr, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}
	s.tick()
	if routes.lease.Load() == nil {
		t.Fatal("fixture: the first tick on a converged city published no lease")
	}

	// Make the basis capture fail (no marker to hash) and, independently,
	// give the recheck a real stranded write plus an unfinished clear, so
	// the report that comes back is still DEFINITIVE — not uncheckable,
	// which already withdraws the lease on its own path (tick's standing-
	// fault branch) and would not exercise publishLease at all.
	if err := os.Remove(target.MarkerPath()); err != nil {
		t.Fatal(err)
	}
	mustCreateInfraBead(t, source, beads.Bead{Title: "finding", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-1"}})
	if err := writeInfraClearedNote(cityPath, infraClearedNote{Binding: target.Binding, Database: target.Database, Complete: false}); err != nil {
		t.Fatal(err)
	}

	s.tick()
	if lease := routes.lease.Load(); lease != nil {
		t.Fatalf("a definitive tick whose basis capture failed still served a stale lease: %+v (SHOULD A)", lease)
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.StorageBindingStranded {
		t.Fatalf("the second tick recorded %+v, want exactly one %s: the tick was not definitive", rec.Events, events.StorageBindingStranded)
	}
	if !strings.Contains(stderr.String(), "serves no containment lease") || !strings.Contains(stderr.String(), target.MarkerPath()) {
		t.Errorf("the tick did not say why it serves no lease:\n%s", stderr.String())
	}
}

// TestContainmentBasisStampDatabaseIgnoresOrdinaryWrites kills mutant B1:
// the database's own basis stamp is deliberately NOT change-sensitive
// (containmentBasisStamp(target.Database, false)) — it exists to catch a
// RESTORE (a new file swapped in, a new inode) rather than an ordinary
// write through the binding this process already has open, which changes
// size and mtime on every call and would invalidate every lease if the
// database stamp tracked them.
func TestContainmentBasisStampDatabaseIgnoresOrdinaryWrites(t *testing.T) {
	cityPath, _, _, target := convergedInfraCity(t)
	capture := func() string {
		b, err := captureContainmentBasis(cityPath, target)
		if err != nil {
			t.Fatalf("captureContainmentBasis: %v", err)
		}
		return b.Database
	}
	before := capture()

	// An ordinary binding write: content grows, mtime moves, inode does not.
	f, err := os.OpenFile(target.Database, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("opening %s for an ordinary write: %v", target.Database, err)
	}
	if _, err := f.Write([]byte("ordinary binding write\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	after := capture()
	if before != after {
		t.Errorf("captureContainmentBasis's Database field changed after an ordinary write that kept the same inode: before %q, after %q; the database's stamp must ignore content, size and mtime, only dev+ino", before, after)
	}
}

// TestContainmentBasisStampClearedNoteCatchesASameInodeRewrite kills mutant
// B3: the cleared note's basis stamp IS change-sensitive
// (containmentBasisStamp(notePath, true)) — a same-inode, content-only
// rewrite of the note must move the stamp, or a lease published just
// before such a rewrite would wrongly still look like it reflects the
// note's new content.
func TestContainmentBasisStampClearedNoteCatchesASameInodeRewrite(t *testing.T) {
	cityPath, _, _, target := convergedInfraCity(t)
	notePath := infraClearedNotePath(cityPath)
	if err := writeInfraClearedNote(cityPath, infraClearedNote{Binding: target.Binding, Database: target.Database, Complete: true}); err != nil {
		t.Fatal(err)
	}
	capture := func() string {
		b, err := captureContainmentBasis(cityPath, target)
		if err != nil {
			t.Fatalf("captureContainmentBasis: %v", err)
		}
		return b.Cleared
	}
	before := capture()

	// A same-inode, content-only rewrite: open in place (no temp+rename),
	// so dev and ino are unchanged but size and mtime move.
	f, err := os.OpenFile(notePath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("opening %s for an in-place rewrite: %v", notePath, err)
	}
	if _, err := f.Write([]byte("extra content, same inode\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	after := capture()
	if before == after {
		t.Errorf("captureContainmentBasis's Cleared field did not change after a same-inode, content-only rewrite: %q; this stamp must be change-sensitive (size/mtime/ctime), not dev+ino alone", before)
	}
}
