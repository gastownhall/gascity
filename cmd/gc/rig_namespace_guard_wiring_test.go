package main

// Tests that pin the guarded store's reads, the controller and runtime wiring
// of its resolver, and its refusal of stores that are not a guarded rig's
// own. Each one fails if the behavior its comment names is dropped.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

func rgsForeignThenOwn(foreign, own int) []beads.Bead {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var seed []beads.Bead
	for i := 0; i < foreign; i++ {
		seed = append(seed, beads.Bead{
			ID: fmt.Sprintf("bt-f%03d", i), Title: "foreign", Type: "task", Status: "open",
			CreatedAt: base.Add(time.Duration(i) * time.Second), Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "alpha/crew.intake"),
		})
	}
	for i := 0; i < own; i++ {
		seed = append(seed, beads.Bead{
			ID: fmt.Sprintf("bt-m%03d", i), Title: "mine", Type: "task", Status: "open",
			CreatedAt: base.Add(time.Hour + time.Duration(i)*time.Second), Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.intake"),
		})
	}
	return seed
}

func rgsAssertNoForeign(t *testing.T, ns *rigNamespace, what string, rows []beads.Bead) {
	t.Helper()
	for _, b := range rows {
		if ns.classify(b) == rowForeign {
			t.Fatalf("%s returned Foreign row %s", what, b.ID)
		}
	}
}

// Fails if refill returns kept, not kept[:limit].
func TestRigNamespaceGuardRefillTruncatesToLimit(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	g := newRigNamespaceStore(beads.NewMemStoreFrom(1000, rgsForeignThenOwn(50, 2), nil), ns)
	rows, err := g.List(beads.ListQuery{Status: "open", Limit: 1, Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "bt-m000" {
		t.Fatalf("List{Limit: 1} = %v; want exactly [bt-m000]", rgsIDs(rows))
	}
}

// Fails if ReadyOrdered always reports ordered=true. An inner store with no
// ordered read returns its whole set; the guard must not claim a cut page.
func TestRigNamespaceGuardReadyOrderedNeverClaimsAnOrderItLacks(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	g := newRigNamespaceStore(beads.NewMemStoreFrom(1000, rgsForeignThenOwn(3, 3), nil), ns)
	rows, ordered, err := g.ReadyOrdered(beads.ReadyQuery{Limit: 1}, beads.ReadyOrderOldest)
	if err != nil {
		t.Fatal(err)
	}
	if ordered && len(rows) > 1 {
		t.Fatalf("ReadyOrdered{Limit: 1} over an unordered store = %d rows, ordered=true; a cut page longer than its limit", len(rows))
	}
}

// Fails if the unbounded fallback reports ordered=true.
func TestRigNamespaceGuardReadyOrderedFallbackIsNotACutPage(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := &rgsOrderedStore{MemStore: beads.NewMemStoreFrom(1000, rgsForeignThenOwn(70, 2), nil)}
	g := newRigNamespaceStore(inner, ns)
	rows, ordered, err := g.ReadyOrdered(beads.ReadyQuery{Limit: 1}, beads.ReadyOrderOldest)
	if err != nil {
		t.Fatal(err)
	}
	if ordered && (len(rows) != 1 || rows[0].ID != "bt-m000") {
		t.Fatalf("ReadyOrdered{Limit: 1} after the refill rounds = %v, ordered=true; want a correct one-row prefix or ordered=false", rgsIDs(rows))
	}
	if !ordered && len(rows) != 2 {
		t.Fatalf("ReadyOrdered fallback = %v, ordered=false; want the whole kept set", rgsIDs(rows))
	}
}

// rgsCountingMem is a mem store that counts like a backend Counter.
type rgsCountingMem struct{ *beads.MemStore }

func (s rgsCountingMem) Count(_ context.Context, q beads.ListQuery, _ ...string) (int, error) {
	rows, err := s.List(q)
	return len(rows), err
}

// Fails if Count forwards a non-structural query to the inner Counter.
func TestRigNamespaceGuardCountNeverForwardsAFilteredQuery(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	g := newRigNamespaceStore(rgsCountingMem{beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}, ns)
	n, err := g.Count(context.Background(), beads.ListQuery{AllowScan: true})
	if !errors.Is(err, beads.ErrCountUnsupported) {
		t.Fatalf("Count over an inner Counter = %d, %v; want ErrCountUnsupported (the inner count includes Foreign rows)", n, err)
	}
}

// Fails if Count answers 0, nil when the assignee could not be decided.
func TestRigNamespaceGuardCountReportsAnUndecidableAssignee(t *testing.T) {
	g, _, sessions := rgsGuard(t)
	sessions.setErr(errors.New("sessions store down"))
	n, err := g.Count(context.Background(), beads.ListQuery{Assignee: poolIDB2})
	if err == nil {
		t.Fatalf("Count{undecidable pool assignee} = %d, nil; want an error, never a conclusive count", n)
	}
}

// Fails if the read handles' Ready is unfiltered.
func TestRigNamespaceGuardHandleReadyIsFiltered(t *testing.T) {
	g, _, _ := rgsGuard(t)
	for name, r := range map[string]beads.CachedReader{"Cached": g.Handles().Cached, "Live": g.Handles().Live} {
		rows, err := r.Ready()
		if err != nil {
			t.Fatal(err)
		}
		rgsAssertNoForeign(t, g.ns, "Handles()."+name+".Ready", rows)
		if !containsString(rgsIDs(rows), "bt-own") {
			t.Fatalf("Handles().%s.Ready dropped an own row: %v", name, rgsIDs(rows))
		}
	}
}

// Fails if CachedList answers a partial read as complete.
func TestRigNamespaceGuardCachedListDeclinesAPartialRead(t *testing.T) {
	ns, sessions := rnsCityBNamespace(t)
	cache := beads.NewCachingStore(beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := newRigNamespaceStore(cache, ns)
	if _, ok := g.CachedList(beads.ListQuery{AllowScan: true}); !ok {
		t.Fatal("precondition: the guard over a primed cache declined a clean read")
	}
	ns2, sessions2 := rnsCityBNamespace(t)
	_ = sessions
	g2 := newRigNamespaceStore(cache, ns2)
	sessions2.setErr(errors.New("sessions store down"))
	if rows, ok := g2.CachedList(beads.ListQuery{AllowScan: true}); ok {
		t.Fatalf("CachedList with an unreadable sessions store answered %v as complete", rgsIDs(rows))
	}
}

// Fails if ListByLabel is unfiltered.
func TestRigNamespaceGuardListByLabelIsFiltered(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	seed := rgsSeedBeads()
	for i := range seed {
		seed[i].Labels = append(seed[i].Labels, "lbl")
	}
	g := newRigNamespaceStore(beads.NewMemStoreFrom(1000, seed, nil), ns)
	rows, err := g.ListByLabel("lbl", 0)
	if err != nil {
		t.Fatal(err)
	}
	rgsAssertNoForeign(t, ns, "ListByLabel", rows)
	if !containsString(rgsIDs(rows), "bt-own") {
		t.Fatalf("ListByLabel dropped an own row: %v", rgsIDs(rows))
	}
}

// Fails if ReadyContext is unfiltered.
func TestRigNamespaceGuardReadyContextIsFiltered(t *testing.T) {
	g, _, _ := rgsGuard(t)
	rows, err := g.ReadyContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rgsAssertNoForeign(t, g.ns, "ReadyContext", rows)
	if !containsString(rgsIDs(rows), "bt-own") {
		t.Fatalf("ReadyContext dropped an own row: %v", rgsIDs(rows))
	}
}

// Fails if ReadyPreferCache is unfiltered.
func TestRigNamespaceGuardReadyPreferCacheIsFiltered(t *testing.T) {
	g, _, _ := rgsGuard(t)
	rows, err := g.ReadyPreferCache(beads.ReadyQuery{})
	if err != nil {
		t.Fatal(err)
	}
	rgsAssertNoForeign(t, g.ns, "ReadyPreferCache", rows)
	if !containsString(rgsIDs(rows), "bt-own") {
		t.Fatalf("ReadyPreferCache dropped an own row: %v", rgsIDs(rows))
	}
}

// rgsCensusMem is a mem store with a namespace census.
type rgsCensusMem struct {
	*beads.MemStore
	resident bool
	err      error
}

func (s rgsCensusMem) HasResidentOutside([]string) (bool, error) { return s.resident, s.err }

// Fails if a store with no census gets a verdict, or a store with one does not have it forwarded.
func TestRigNamespaceGuardCensusIsForwardedOrRefused(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	none := newRigNamespaceStore(beads.NewMemStore(), ns)
	if got, err := none.HasResidentOutside([]string{"bt"}); !errors.Is(err, beads.ErrNamespaceCensusUnsupported) {
		t.Fatalf("census over a store without one = %v, %v; want ErrNamespaceCensusUnsupported", got, err)
	}
	with := newRigNamespaceStore(rgsCensusMem{MemStore: beads.NewMemStore(), resident: true}, ns)
	if got, err := with.HasResidentOutside([]string{"bt"}); err != nil || !got {
		t.Fatalf("census = %v, %v; want the inner store's unfiltered true", got, err)
	}
}

// Fails if a guarded rig on the legacy shared file store is opened.
func TestRigNamespaceGuardLegacySharedFileStoreRefusesAGuardedRig(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "beta")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Create(beads.Bead{Title: "city row", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "beta", Path: rigDir, Federation: config.RigFederationRig}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs := newControllerState(ctx, cfg, runtime.NewFake(), events.NewFake(), "test-city", cityDir)
	store := cs.BeadStore("beta")
	if store == nil {
		t.Fatal("BeadStore(beta) = nil")
	}
	rows, err := store.List(beads.ListQuery{AllowScan: true})
	if err == nil || !strings.Contains(err.Error(), "shared legacy file store") {
		t.Fatalf("guarded rig on the legacy shared file store read %d rows, err %v; want the refusal", len(rows), err)
	}
}

// Fails if scopedStoreLike drops the guard from the bd clone.
func TestRigNamespaceGuardScopedCloneOfAGuardedRigKeepsTheGuard(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	rigDir := filepath.Join(cityDir, "rigs", "beta")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "beta", Path: "rigs/beta", Federation: config.RigFederationRig}}}
	backing := beads.NewBdStore(rigDir, noopBdRunner())
	existing := wrapStoreWithBeadPolicies(newRigNamespaceStore(beads.NewCachingStoreForTest(backing, nil), ns), cfg)
	scoped, err := scopedStoreLike(context.Background(), cityDir, cfg, existing)
	if err != nil {
		t.Fatalf("scopedStoreLike: %v", err)
	}
	if scoped == nil {
		t.Fatal("scopedStoreLike returned no clone for a bd-backed guarded rig")
	}
	if rigNamespaceOfStore(scoped) != ns {
		t.Fatalf("the scoped clone %T lost the rig's guard: a bare bd store over the shared ledger", scoped)
	}
}

// Fails if two rigs whose metadata names one project are not one store.
func TestRigNamespaceGuardSameProjectIDIsOneStore(t *testing.T) {
	city := rnsBdCity(t)
	mk := func(name, metadata string, guarded bool) config.Rig {
		dir := filepath.Join(city, name)
		rnsScope(t, dir, metadata)
		r := config.Rig{Name: name, Path: dir}
		if guarded {
			r.Federation = config.RigFederationRig
		}
		return r
	}
	pa := mk("pa", `{"backend":"dolt","project_id":"prj_same","dolt_database":"pa"}`, true)
	pb := mk("pb", `{"backend":"dolt","project_id":"prj_same","dolt_database":"pb"}`, false)
	cfg := &config.City{Rigs: []config.Rig{pa, pb}}
	if err := checkGuardedRigStore(cfg, city, pa); err == nil || !strings.Contains(err.Error(), `rig "pb"'s`) {
		t.Fatalf("a guarded rig whose project id is another rig's opened: %v", err)
	}
}

// rgsRuntimeWithGuardedRig is a city runtime with a guarded rig whose
// controller state's city (and so sessions) store is a counting store
// holding one of city B's pool session records.
func rgsRuntimeWithGuardedRig(t *testing.T) (*CityRuntime, *controllerState) {
	t.Helper()
	isolateRigNamespaceResolvers(t)
	t.Setenv("GC_BEADS", "file")
	// Not t.TempDir: the controller state's background prime may still be
	// writing when the test ends, and TempDir fails on a busy directory.
	cityPath, err := os.MkdirTemp("", "mkrt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cityPath) })
	tomlPath := filepath.Join(cityPath, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "beta", Path: filepath.Join(cityPath, "beta"), Federation: config.RigFederationRig}},
	}
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs := newControllerState(ctx, cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	cr.setControllerState(cs)
	return cr, cs
}

// Fails if setControllerState does not point the resolver at the runtime's
// sessions store.
func TestRigNamespaceGuardSetControllerStatePointsTheResolverAtTheSessionsStore(t *testing.T) {
	_, cs := rgsRuntimeWithGuardedRig(t)
	rec, ok, err := cs.rigSessions.lookupOwnSession(poolIDB)
	if err != nil || !ok || rec.Template != templateB {
		t.Fatalf("lookup through the runtime's sessions store = %+v, %v, %v", rec, ok, err)
	}
}

// Fails if a snapshot loaded from the sessions store is not published.
func TestRigNamespaceGuardSnapshotLoadsArePublishedToTheResolver(t *testing.T) {
	cr, cs := rgsRuntimeWithGuardedRig(t)
	if _, partial := cr.loadSessionBeadSnapshotWithPartial(); partial {
		t.Fatal("precondition: the session snapshot load was partial")
	}
	if cs.rigSessions.snap.Load() == nil {
		t.Fatal("the session snapshot was not published to the guard resolver")
	}
}

// rgsGraphOnlyMem is a mem store with a graph-only ready read, as the
// doltlite read store has.
type rgsGraphOnlyMem struct{ *beads.MemStore }

func (s rgsGraphOnlyMem) ReadyGraphOnly(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return s.Ready(query...)
}

// Fails if the graph-only ready handle is the inner store's, unfiltered.
func TestRigNamespaceGuardGraphOnlyReadyIsFiltered(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	g := newRigNamespaceStore(rgsGraphOnlyMem{beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}, ns)
	h, ok := g.ReadyGraphOnlyHandle()
	if !ok {
		t.Fatal("the guard hid the inner store's graph-only ready read")
	}
	rows, err := h.ReadyGraphOnly()
	if err != nil {
		t.Fatal(err)
	}
	rgsAssertNoForeign(t, ns, "ReadyGraphOnly", rows)
	if !containsString(rgsIDs(rows), "bt-own") {
		t.Fatalf("ReadyGraphOnly dropped an own row: %v", rgsIDs(rows))
	}
}

// rgsProxiedInner is a store that carries a proxied split view, whose BdLeaf
// would hand out a raw store.
type rgsProxiedInner struct{ *beads.MemStore }

func (rgsProxiedInner) Demoted() bool                       { return false }
func (rgsProxiedInner) Verdict() *beads.ProxiedVerdictError { return nil }
func (rgsProxiedInner) Report() beads.ProxiedOpenReport     { return beads.ProxiedOpenReport{} }
func (p rgsProxiedInner) BdLeaf() beads.Store               { return p.MemStore }

// the guard must not forward the proxied view of a proxied inner store.
func TestRigNamespaceGuardProxiedStoreHandsOutNoLeafOverAProxiedInner(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := rgsProxiedInner{MemStore: beads.NewMemStore()}
	if _, ok := beads.ProxiedStoreFrom(inner); !ok {
		t.Fatal("precondition: the inner store carries no proxied view")
	}
	g := newRigNamespaceStore(inner, ns)
	if view, ok := g.ProxiedStore(); ok || view != nil {
		t.Fatalf("the guard handed out the inner store's proxied view (%T): its BdLeaf bypasses the guard", view)
	}
	if view, ok := beads.ProxiedStoreFrom(wrapStoreWithBeadPolicies(g, nil)); ok {
		t.Fatalf("ProxiedStoreFrom(policy(guard(proxied))) found %T", view)
	}
}

// rgsCountingInner is an inner store with a Counter.
type rgsCountingInner struct {
	*beads.MemStore
	counts atomic.Int64
	err    error
}

func (s *rgsCountingInner) Count(context.Context, beads.ListQuery, ...string) (int, error) {
	s.counts.Add(1)
	return 7, s.err
}

// a structural count that also selects by an unadmitted identity is
// answered 0 without asking the inner Counter.
func TestRigNamespaceGuardCountGatesStructuralIdentityBeforeForwarding(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := &rgsCountingInner{MemStore: beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}
	g := newRigNamespaceStore(inner, ns)
	n, err := g.Count(context.Background(), beads.ListQuery{ParentID: "bt-x", Assignee: "alice"})
	if err != nil || n != 0 || inner.counts.Load() != 0 {
		t.Fatalf("Count{ParentID, Assignee: alice} = %d, %v after %d inner counts; want 0, nil without counting", n, err, inner.counts.Load())
	}
	if n, err := g.Count(context.Background(), beads.ListQuery{ParentID: "bt-x"}); err != nil || n != 7 {
		t.Fatalf("Count{ParentID} = %d, %v; want the inner count 7 (structural reads are counted by the inner store)", n, err)
	}
}

// a guarded city whose sessions store has no id prefix is warned once;
// one whose store has a prefix is not.
func TestRigNamespaceGuardNoMintPrefixWarns(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}, Rigs: []config.Rig{rnsGuardedRig("beta")}}
	cs := &controllerState{cityPath: t.TempDir(), cityName: "demo", rigSessions: newRigNamespaceSessions("/cities/mk2-x")}
	cs.setRigNamespaceSessionStore(beadstest.Sessions(beads.NewMemStore()), cfg)
	if !cs.rigSessions.warned {
		t.Fatal("a guarded city whose sessions store mints no pinned pool ids was not warned")
	}
	ok := &controllerState{cityPath: t.TempDir(), cityName: "demo", rigSessions: newRigNamespaceSessions("/cities/mk2-y")}
	ok.setRigNamespaceSessionStore(beadstest.Sessions(rnsNewCountingStore()), cfg)
	if ok.rigSessions.warned {
		t.Fatal("warned although the sessions store mints pool ids under gcx")
	}
}

// a close the sweep cannot classify (the sessions store is down)
// is retried, never concluded: not acted on (N13) and not dropped (N12).
func TestRigNamespaceGuardSweepRetriesAnUndecidableRowAndNeverActsOnIt(t *testing.T) {
	res, sessions := rnsCityBResolver(t)
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	backing := beads.NewMemStore()
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		out, err := backing.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ownConvoy := mk(beads.Bead{Title: "ours", Type: "convoy"})
	ownMember := mk(beads.Bead{Title: "our task", ParentID: ownConvoy.ID, Assignee: poolIDB})
	theirConvoy := mk(beads.Bead{Title: "unattributed", Type: "convoy"})
	theirMember := mk(beads.Bead{Title: "their task", ParentID: theirConvoy.ID, Assignee: poolIDA})
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	guarded := wrapStoreWithBeadPolicies(newRigNamespaceStore(cached, ns), &config.City{})
	cs := &controllerState{beadStores: map[string]beads.Store{"beta": guarded}, eventProv: events.NewFake()}

	sessions.setErr(errors.New("sessions store down"))
	for _, id := range []string{ownMember.ID, theirMember.ID} {
		if err := backing.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	sweep := cs.autocloseSweepOf()
	sweep.deferID(ownMember.ID, sweepTestClock(1))
	sweep.deferID(theirMember.ID, sweepTestClock(1))
	for i := 1; i <= 3; i++ {
		r := cs.runAutocloseSweepPass(sweepTestClock(i))
		if r.Ran != 0 || r.Refuted != 0 {
			t.Fatalf("pass %d during the sessions outage: ran=%d refuted=%d; an undecidable row is retried, never concluded", i, r.Ran, r.Refuted)
		}
	}
	for _, id := range []string{ownConvoy.ID, theirConvoy.ID} {
		if b, _ := backing.Get(id); b.Status == "closed" {
			t.Fatalf("convoy %s closed during the sessions outage", id)
		}
	}

	sessions.setErr(nil)
	res.beginPass()
	for i := 4; i <= 6; i++ {
		cs.runAutocloseSweepPass(sweepTestClock(i))
	}
	if b, _ := backing.Get(ownConvoy.ID); b.Status != "closed" {
		t.Fatalf("this city's convoy %s is %s after the outage: its member's close was dropped instead of retried", ownConvoy.ID, b.Status)
	}
	if b, _ := backing.Get(theirConvoy.ID); b.Status == "closed" {
		t.Fatal("another city's pool member closed an unattributed convoy here")
	}
}

// the ledger feed of a guarded rig's cache, built by
// wrapWithCachingStoreFeed, carries the rig's namespace.
func TestRigNamespaceGuardGuardedRigFeedCarriesTheNamespace(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	ctx, cancel := context.WithCancel(context.Background())
	set := newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{work: func() func() { return func() {} }, deferClose: func(string) {}})
	req := &ledgerFeedRequest{set: set, scope: ledgerFeedRigScope("beta"), mode: config.BeadsLedgerFeedAuto, admit: func() bool { return true }}
	store := wrapWithCachingStoreFeed(ctx, wrapStoreWithBeadPolicies(newRigNamespaceStore(newJournalingStore(), ns), &config.City{}), nil, true, req)
	if store == nil {
		cancel()
		t.Fatal("wrapWithCachingStoreFeed returned no store")
	}
	t.Cleanup(func() {
		cancel()
		_ = closeBeadStoreHandle(store)
	})
	set.mu.Lock()
	defer set.mu.Unlock()
	feeds := 0
	for _, e := range set.entries {
		if e.feed == nil {
			continue
		}
		feeds++
		if e.feed.ns != ns {
			t.Fatalf("the guarded rig's ledger feed has namespace %p, want beta's %p: another city's journal rows would wake this city", e.feed.ns, ns)
		}
	}
	if feeds != 1 {
		t.Fatalf("%d ledger feeds attached, want 1", feeds)
	}
}

// TestRigNamespaceGuardWarnsOnTheMintPathsOwnCheck covers an unsplit city's
// sessions store (the policy layer hides IDPrefix from the mint path, which
// then pins no pool ids) and a split one's (the cache exposes it).
func TestRigNamespaceGuardWarnsOnTheMintPathsOwnCheck(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "demo", Prefix: "gcx"}, Rigs: []config.Rig{rnsGuardedRig("beta")}}
	for _, tc := range []struct {
		name     string
		sessions beads.Store
		wantWarn bool
	}{
		{"unsplit", wrapStoreWithBeadPolicies(beads.NewCachingStore(rnsNewCountingStore(), nil), cfg), true},
		{"split", beads.NewCachingStore(rnsNewCountingStore(), nil), false},
	} {
		cs := &controllerState{cityName: "demo", rigSessions: newRigNamespaceSessions(t.TempDir())}
		cs.setRigNamespaceSessionStore(beadstest.Sessions(tc.sessions), cfg)
		minted := poolSessionExplicitBeadID(tc.sessions, strings.Repeat("a", 32))
		if (minted == "") != tc.wantWarn {
			t.Fatalf("%s: precondition: the mint path pins %q", tc.name, minted)
		}
		if cs.rigSessions.warned != tc.wantWarn {
			t.Fatalf("%s: warned = %v, want %v (the mint path pins %q)", tc.name, cs.rigSessions.warned, tc.wantWarn, minted)
		}
	}
}

// TestControlReadyScanReturnsTheGuardsRefusal opens the control ready scan
// over a guarded rig whose store an unguarded rig shares: the scan returns the
// refusal instead of falling back to an unguarded bd read.
func TestControlReadyScanReturnsTheGuardsRefusal(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	city := rnsBdCity(t)
	beta := filepath.Join(city, "beta")
	rnsScope(t, beta, `{"backend":"dolt","dolt_database":"beta"}`)
	alias := filepath.Join(city, "alias")
	rnsScope(t, alias, `{"backend":"dolt","dolt_database":"beta"}`)
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "beta", Path: beta, Prefix: "bt", Federation: config.RigFederationRig},
		{Name: "alias", Path: alias, Prefix: "al"},
	}}
	_, opened, err := controlReadyLegsReady(beta, city, cfg)
	if !errors.Is(err, errRigNamespace) || !opened {
		t.Fatalf("control ready scan over a refused store = opened %v, err %v; want the refusal, never the unguarded fallback", opened, err)
	}
}

func TestRigNamespaceAliasRefusalsWrapTheSentinel(t *testing.T) {
	if err := guardedRigAliasError("beta", "its bead store is the city's"); !errors.Is(err, errRigNamespace) {
		t.Fatalf("alias refusal %v does not wrap errRigNamespace", err)
	}
	city := rnsBdCity(t)
	beta := filepath.Join(city, "beta")
	rnsScope(t, beta, `{"backend":"dolt","dolt_database":"shared"}`)
	gamma := filepath.Join(city, "gamma")
	rnsScope(t, gamma, `{"backend":"dolt","dolt_database":"shared"}`)
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "beta", Path: beta, Federation: config.RigFederationRig},
		{Name: "gamma", Path: gamma, Federation: config.RigFederationRig},
	}}
	if _, err := guardedRigForScope(cfg, city, beta); !errors.Is(err, errRigNamespace) {
		t.Fatalf("two guarded rigs on one store = %v; want an errRigNamespace refusal", err)
	}
}
