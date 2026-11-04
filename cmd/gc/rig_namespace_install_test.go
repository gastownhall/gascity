package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// isolateRigNamespaceResolvers gives a test its own resolver registry.
func isolateRigNamespaceResolvers(t *testing.T) {
	t.Helper()
	rigNamespaceResolvers.mu.Lock()
	saved := rigNamespaceResolvers.controllers
	savedCLI := rigNamespaceResolvers.cli
	savedProc := rigNamespaceResolvers.controllerProcess
	rigNamespaceResolvers.controllers = nil
	rigNamespaceResolvers.cli = nil
	rigNamespaceResolvers.controllerProcess = 0
	rigNamespaceResolvers.mu.Unlock()
	t.Cleanup(func() {
		rigNamespaceResolvers.mu.Lock()
		rigNamespaceResolvers.controllers = saved
		rigNamespaceResolvers.cli = savedCLI
		rigNamespaceResolvers.controllerProcess = savedProc
		rigNamespaceResolvers.mu.Unlock()
	})
}

func guardOf(t *testing.T, store beads.Store) *rigNamespaceStore {
	t.Helper()
	base, _, ok := unwrapBeadPolicyStore(splitStrictFrontOf(store))
	if !ok {
		t.Fatalf("store %T has no policy layer outermost", store)
	}
	g, ok := base.(*rigNamespaceStore)
	if !ok {
		t.Fatalf("store under the policy layer is %T, want the rig namespace guard", base)
	}
	return g
}

func TestGuardRigStoreSitsUnderThePolicyLayer(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	mem := beads.NewMemStore()
	cfg := &config.City{}

	if got := guardRigStore(wrapStoreWithBeadPolicies(mem, cfg), nil); guardIsPresent(got) {
		t.Fatal("an unguarded rig got a guard")
	}
	guarded := guardRigStore(wrapStoreWithBeadPolicies(mem, cfg), ns)
	if g := guardOf(t, guarded); g.inner != mem {
		t.Fatalf("guard inner = %T, want the engine", g.inner)
	}
	again := guardRigStore(guarded, ns)
	if guardOf(t, again).inner != mem {
		t.Fatal("guarding a guarded store stacked a second guard")
	}
	borrowed := guardRigStore(borrowedStoreWithBeadPolicies(mem, cfg), ns)
	if _, policy, _ := unwrapBeadPolicyStore(borrowed); !policy.borrowed {
		t.Fatal("guarding a borrowed handle dropped its borrowed flag; closing it would close the memo's store")
	}
	if _, ok := beads.GraphApplyFor(wrapStoreWithBeadPolicies(newRigNamespaceStore(&rgsSpyStore{MemStore: mem}, ns), cfg)); !ok {
		t.Fatal("the policy layer over the guard lost graph apply")
	}
}

// TestGuardRigStoreLeavesNoStoreAlone: nil and a typed nil are no store, so
// the guard is not wrapped around one.
func TestGuardRigStoreLeavesNoStoreAlone(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	if got := guardRigStore(nil, ns); got != nil {
		t.Fatalf("guardRigStore(nil) = %T, want nil", got)
	}
	var typedNil *beads.MemStore
	got := guardRigStore(typedNil, ns)
	if m, ok := got.(*beads.MemStore); !ok || m != nil {
		t.Fatalf("guardRigStore(typed nil) = %T, want the typed nil back", got)
	}
}

func guardIsPresent(store beads.Store) bool {
	return rigNamespaceOfStore(store) != nil
}

// TestGuardedRigCacheStaysUnfilteredAndItsEventsAreFiltered pins the
// controller's placement: policy(guard(cache(engine))). The cache holds every
// row; reads through the store are filtered; another city's change wakes
// nothing.
func TestGuardedRigCacheStaysUnfilteredAndItsEventsAreFiltered(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	mem := beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)
	ep := events.NewFake()
	opened := guardRigStore(wrapStoreWithBeadPolicies(mem, &config.City{}), ns)

	store := wrapWithCachingStoreFeed(context.Background(), opened, ep, false, nil)
	g := guardOf(t, store)
	cache, ok := g.inner.(*beads.CachingStore)
	if !ok {
		t.Fatalf("guard inner = %T, want the cache", g.inner)
	}
	if cache.Backing() != mem {
		t.Fatalf("cache backing = %T, want the engine", cache.Backing())
	}
	all, err := cache.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(rgsSeedBeads()) {
		t.Fatalf("the cache holds %d rows, want every row (%d): it must stay unfiltered", len(all), len(rgsSeedBeads()))
	}
	rows, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "List through the guarded stack", rows, rgsWantIDs())

	// Another city's write lands in the shared cache: no event here.
	foreign, err := cache.Create(beads.Bead{Title: "theirs", Type: "task", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "alpha/crew.intake")})
	if err != nil {
		t.Fatal(err)
	}
	own, err := store.Create(beads.Bead{Title: "ours", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, e := range ep.Events {
		subjects = append(subjects, e.Subject)
	}
	if containsString(subjects, foreign.ID) {
		t.Fatalf("a Foreign row's create was recorded: %v", subjects)
	}
	if !containsString(subjects, own.ID) {
		t.Fatalf("this city's own create was not recorded: %v", subjects)
	}
}

func TestRigNamespaceEventFilter(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	var got []string
	filter := rigNamespaceEventFilter(ns, func(_ beads.ChangeSource, eventType, beadID, _, _, _ string, _ *[]string, _ json.RawMessage) {
		got = append(got, eventType+" "+beadID)
	})
	emit := func(eventType string, b beads.Bead) {
		payload, err := beads.EncodeBeadEventPayload(b)
		if err != nil {
			t.Fatal(err)
		}
		filter(beads.ChangeLocal, eventType, b.ID, "", "", "", nil, payload)
	}
	for _, r := range rgsSeed() {
		for _, et := range []string{events.BeadCreated, events.BeadUpdated, events.BeadClosed, events.BeadDeleted} {
			emit(et, r.bead)
		}
	}
	filter(beads.ChangeLocal, events.BeadUpdated, "bt-garbled", "", "", "", nil, []byte("not json"))
	filter(beads.ChangeLocal, "cache.snapshot", "", "", "", "", nil, nil)
	want := 0
	for _, r := range rgsSeed() {
		if r.keep {
			want += 4
		}
		for _, line := range got {
			if strings.HasSuffix(line, " "+r.bead.ID) && !r.keep {
				t.Fatalf("Foreign row's notification passed: %s", line)
			}
		}
	}
	if len(got) != want+1 || got[len(got)-1] != "cache.snapshot " {
		t.Fatalf("passed %v; want the %d notifications of this city's rows and the non-bead event", got, want)
	}
}

// closeTrackingStore records CloseStore and answers it with err.
type closeTrackingStore struct {
	*beads.MemStore
	closed int
	err    error
}

func (s *closeTrackingStore) CloseStore() error {
	s.closed++
	return s.err
}

func TestPeelSitesHandOutTheGuard(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	mem := beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)
	cache := beads.NewCachingStore(mem, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := wrapStoreWithBeadPolicies(newRigNamespaceStore(cache, ns), &config.City{})

	t.Run("demand leg", func(t *testing.T) {
		view, _ := demandLegCache(store)
		if view == nil {
			t.Fatal("demandLegCache found no cache under the guard: a guarded leg would block creates")
		}
		if _, ok := view.(*rigNamespaceStore); !ok {
			t.Fatalf("demandLegCache handed out %T, want the guard", view)
		}
		rows, err := view.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, "demand view List", rows, rgsWantIDs())
		cached, ok := view.CachedList(beads.ListQuery{AllowScan: true})
		if !ok {
			t.Fatal("the demand view declined a primed cached read")
		}
		assertIDs(t, "demand view CachedList", cached, rgsWantIDs())
	})

	t.Run("autoclose census", func(t *testing.T) {
		cs := &controllerState{beadStores: map[string]beads.Store{"beta": store}}
		caches := cs.sweepCaches()
		if len(caches) != 1 {
			t.Fatalf("sweepCaches = %d caches, want the guarded rig's", len(caches))
		}
		if _, ok := caches[0].(*rigNamespaceStore); !ok {
			t.Fatalf("sweepCaches handed out %T, want the guard", caches[0])
		}
		rows, ok := caches[0].CachedList(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
		if !ok {
			t.Fatal("census declined")
		}
		assertIDs(t, "autoclose census", rows, rgsWantIDs())
	})

	t.Run("seat work close-time lister", func(t *testing.T) {
		list := closeTimeLister(store, true)
		if list == nil {
			t.Fatal("closeTimeLister found no way to read the guarded leg")
		}
		rows, err := list(beads.ListQuery{AllowScan: true})
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, "close-time lister", rows, rgsWantIDs())
	})

	t.Run("uncached projection store", func(t *testing.T) {
		uncached := uncachedBeadStore(store)
		g, ok := uncached.(*rigNamespaceStore)
		if !ok {
			t.Fatalf("uncachedBeadStore = %T, want the guard over the uncached store", uncached)
		}
		if g.inner != mem {
			t.Fatalf("uncached guard inner = %T, want the engine", g.inner)
		}
	})

	t.Run("close handle", func(t *testing.T) {
		tracked := &closeTrackingStore{MemStore: beads.NewMemStore()}
		if err := closeBeadStoreHandle(wrapStoreWithBeadPolicies(newRigNamespaceStore(tracked, ns), nil)); err != nil {
			t.Fatal(err)
		}
		if tracked.closed != 1 {
			t.Fatalf("closing the guarded handle closed the engine %d times, want 1", tracked.closed)
		}
		borrowed := &closeTrackingStore{MemStore: beads.NewMemStore()}
		if err := closeBeadStoreHandle(guardRigStore(borrowedStoreWithBeadPolicies(borrowed, nil), ns)); err != nil {
			t.Fatal(err)
		}
		if borrowed.closed != 0 {
			t.Fatal("closing a borrowed guarded handle closed the memo's store")
		}
	})

	t.Run("scoped bd clone keeps the guard", func(t *testing.T) {
		bd := beads.NewBdStore(t.TempDir(), nil)
		stack := wrapStoreWithBeadPolicies(newRigNamespaceStore(beads.NewCachingStore(bd, nil), ns), &config.City{})
		if got, ok := bdStoreBacking(stack); !ok || got != bd {
			t.Fatalf("bdStoreBacking through the guard = %v, %v", got, ok)
		}
		if _, ok := beadPolicyConfig(stack); !ok {
			t.Fatal("beadPolicyConfig lost the policy layer")
		}
		if rigNamespaceOfStore(stack) != ns {
			t.Fatal("rigNamespaceOfStore did not find the guard")
		}
	})
}

func TestReloadRebuildsStoresWhenFederationChanges(t *testing.T) {
	city := t.TempDir()
	base := &config.City{Rigs: []config.Rig{{Name: "beta", Path: "beta", Prefix: "bt"}}}
	guarded := &config.City{Rigs: []config.Rig{{Name: "beta", Path: "beta", Prefix: "bt", Federation: config.RigFederationRig}}}
	exclusive := &config.City{Rigs: []config.Rig{{Name: "beta", Path: "beta", Prefix: "bt", Federation: config.RigFederationRig, Namespace: config.RigNamespaceExclusive}}}
	if !sameStoreTopology(city, base, base) {
		t.Fatal("identical configs differ in topology")
	}
	if sameStoreTopology(city, base, guarded) {
		t.Fatal("a reload that only sets federation = \"rig\" reuses the unguarded store handles")
	}
	if sameStoreTopology(city, guarded, base) {
		t.Fatal("a reload that only clears federation keeps the guarded store handles")
	}
	if sameStoreTopology(city, guarded, exclusive) {
		t.Fatal("a reload that only changes the namespace reuses the store handles")
	}
}

func TestRigNamespaceForOpenFindsTheRigByStoreIdentity(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	city := rnsBdCity(t)
	beta := filepath.Join(city, "beta")
	rnsScope(t, beta, `{"backend":"dolt","dolt_database":"beta"}`)
	gamma := filepath.Join(city, "gamma")
	rnsScope(t, gamma, `{"backend":"dolt","dolt_database":"gamma"}`)
	worktree := filepath.Join(t.TempDir(), "wt")
	rnsScope(t, worktree, "")
	if err := os.WriteFile(filepath.Join(worktree, ".beads", "redirect"), []byte(filepath.Join(beta, ".beads")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(beta, "src", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "beta", Path: beta, Prefix: "bt", Federation: config.RigFederationRig},
			{Name: "gamma", Path: gamma, Prefix: "gm"},
		},
	}
	for _, tc := range []struct {
		scope string
		want  string
	}{
		{beta, "beta"},
		{sub, "beta"},
		{worktree, "beta"},
		{gamma, ""},
		{city, ""},
	} {
		ns, err := rigNamespaceForOpen(cfg, city, resolveStoreScopeRoot(city, tc.scope))
		if err != nil {
			t.Fatalf("scope %s: %v", tc.scope, err)
		}
		if ns.rigName() != tc.want {
			t.Fatalf("scope %s guarded by %q, want %q", tc.scope, ns.rigName(), tc.want)
		}
	}

	unguarded := &config.City{Rigs: []config.Rig{{Name: "beta", Path: beta}}}
	if ns, err := rigNamespaceForOpen(unguarded, city, beta); ns != nil || err != nil {
		t.Fatalf("a city without a guarded rig got %v, %v", ns, err)
	}

	shared := &config.City{Rigs: []config.Rig{
		{Name: "beta", Path: beta, Federation: config.RigFederationRig},
		{Name: "delta", Path: worktree, Federation: config.RigFederationRig},
	}}
	if _, err := rigNamespaceForOpen(shared, city, beta); err == nil {
		t.Fatal("two guarded rigs on one store opened")
	}
}

func TestOpenBodyGuardsAGuardedRigFreshAndBorrowed(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	opened := stubInvocationStoreFactory(t)
	city := t.TempDir()
	beta := filepath.Join(city, "beta")
	plain := filepath.Join(city, "plain")
	for _, dir := range []string{city, beta, plain} {
		if err := ensurePersistedScopeLocalFileStore(dir); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "beta", Path: beta, Prefix: "bt", Federation: config.RigFederationRig},
			{Name: "plain", Path: plain, Prefix: "pl"},
		},
	}
	open := func(scope string) beads.Store {
		t.Helper()
		result, err := openStoreResultAtForCityWithConfig(scope, city, cfg, gate.ModeUnset, false, false, false, nil)
		if err != nil {
			t.Fatalf("open %s: %v", scope, err)
		}
		return result.Store
	}

	fresh := open(beta)
	if g := guardOf(t, fresh); g.ns.rigName() != "beta" {
		t.Fatalf("fresh open guarded by %q", g.ns.rigName())
	}
	if guardIsPresent(open(plain)) || guardIsPresent(open(city)) {
		t.Fatal("an unguarded rig or the city store got a guard")
	}

	end := beginInvocationStoreMemo()
	t.Cleanup(func() { _ = end() })
	first := open(beta)
	second := open(beta)
	for _, s := range []beads.Store{first, second} {
		guardOf(t, s)
		if _, policy, _ := unwrapBeadPolicyStore(splitStrictFrontOf(s)); !policy.borrowed {
			t.Fatal("a memoized guarded handle is not borrowed")
		}
	}
	if guardOf(t, first).inner != guardOf(t, second).inner {
		t.Fatal("the memo opened the guarded scope twice")
	}
	if n := len(*opened); n != 4 {
		t.Fatalf("opened %d stores, want 4 (three fresh, one memoized)", n)
	}

	refused := &config.City{Rigs: []config.Rig{{Name: "beta", Path: city, Federation: config.RigFederationRig}}}
	if _, err := openStoreResultAtForCityWithConfig(filepath.Join(city, "sub"), city, refused, gate.ModeUnset, false, false, false, nil); err != nil {
		t.Fatalf("a scope that is no guarded rig's store was refused: %v", err)
	}
}

func TestControllerOpensAGuardedRigBehindItsGuard(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	city := rnsBdCity(t)
	beta := filepath.Join(city, "beta")
	rnsScope(t, beta, `{"backend":"dolt","dolt_database":"beta"}`)
	plain := filepath.Join(city, "plain")
	rnsScope(t, plain, `{"backend":"dolt","dolt_database":"plain"}`)
	aliased := filepath.Join(city, "aliased")
	rnsScope(t, aliased, `{"backend":"dolt","dolt_database":"hq"}`)

	prevOpen := controllerStateOpenRigStoreAtForCity
	t.Cleanup(func() { controllerStateOpenRigStoreAtForCity = prevOpen })
	engines := map[string]beads.Store{}
	controllerStateOpenRigStoreAtForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		s := beads.NewMemStore()
		engines[opts.ScopeRoot] = s
		return beads.StoreOpenResult{Store: s}, nil
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "beta", Path: beta, Prefix: "bt", Federation: config.RigFederationRig},
			{Name: "plain", Path: plain, Prefix: "pl"},
			{Name: "aliased", Path: aliased, Prefix: "al", Federation: config.RigFederationRig},
		},
	}
	cs := &controllerState{cityPath: city, cfg: cfg, rigSessions: newRigNamespaceSessions(city)}
	stores := cs.buildStores(cfg)

	g := guardOf(t, stores["beta"])
	cache, ok := g.inner.(*beads.CachingStore)
	if !ok || cache.Backing() != engines[resolveStoreScopeRoot(city, beta)] {
		t.Fatalf("beta guard inner = %T, want the cache over beta's engine", g.inner)
	}
	if g.ns.sessions != cs.rigSessions {
		t.Fatal("the guard does not use the city's one resolver")
	}
	if guardIsPresent(stores["plain"]) {
		t.Fatal("an unguarded rig got a guard")
	}
	if _, err := stores["aliased"].List(beads.ListQuery{AllowScan: true}); err == nil || !strings.Contains(err.Error(), "the city's") {
		t.Fatalf("a guarded rig on the city's store opened: %v", err)
	}
	if _, opened := engines[resolveStoreScopeRoot(city, aliased)]; opened {
		t.Fatal("the aliased guarded rig's store was opened before it was refused")
	}
}

func TestRigNamespaceSessionsIsPerCityAndFollowsTheSessionsStore(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	b := newRigNamespaceSessions(cityPathB)
	a := newRigNamespaceSessions(cityPathA)
	if b.res.Load() == a.res.Load() {
		t.Fatal("two cities share a resolver")
	}
	if _, _, err := b.lookupOwnSession(poolIDB); !errors.Is(err, errRigNamespaceSessionsUnavailable) {
		t.Fatalf("lookup before the sessions store is known = %v; want unavailable (fail closed)", err)
	}

	storeB := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	b.setSessionStore(beadstest.Sessions(storeB), []string{"gcx"})
	first := b.res.Load()
	if rec, ok, err := b.lookupOwnSession(poolIDB); err != nil || !ok || rec.Template != templateB {
		t.Fatalf("lookup through the sessions store = %v, %v, %v", rec, ok, err)
	}
	b.setSessionStore(beadstest.Sessions(storeB), []string{"gcx"})
	if b.res.Load() != first {
		t.Fatal("re-setting the same sessions store rebuilt the resolver")
	}
	reopened := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	b.setSessionStore(beadstest.Sessions(reopened), []string{"gcx"})
	if b.res.Load() == first {
		t.Fatal("a reopened sessions store kept the old resolver")
	}
	if _, ok, _ := b.lookupOwnSession(poolIDB); !ok || reopened.gets.Load() == 0 {
		t.Fatal("the rebuilt resolver did not read the new sessions store")
	}

	snap := newSessionBeadSnapshot([]beads.Bead{rnsSessionBead(poolIDB2, templateB, "crew.worker-"+poolIDB2)})
	b.publishSnapshot(snap)
	before := reopened.gets.Load()
	if _, ok, err := b.lookupOwnSession(poolIDB2); !ok || err != nil {
		t.Fatalf("published snapshot not consulted: %v, %v", ok, err)
	}
	if reopened.gets.Load() != before {
		t.Fatal("a snapshot hit read the store")
	}

	reopened.setErr(errors.New("down"))
	if _, _, err := b.lookupOwnSession("gcx-session-" + strings.Repeat("a", 32)); err == nil {
		t.Fatal("store failure not reported")
	}
	reopened.setErr(nil)
	if _, _, err := b.lookupOwnSession("gcx-session-" + strings.Repeat("b", 32)); err == nil {
		t.Fatal("the backoff did not hold after a store error")
	}
	b.beginPass()
	if _, _, err := b.lookupOwnSession("gcx-session-" + strings.Repeat("c", 32)); err != nil {
		t.Fatalf("beginPass did not end the backoff: %v", err)
	}

	unregister := registerRigNamespaceSessions(cityPathB, b)
	current := func(cityPath string) ownSessionResolver {
		return rigNamespaceSessionsForOpen(cityPath, nil).(rigNamespaceCityResolver).current()
	}
	if got := current(cityPathB); got != ownSessionResolver(b) {
		t.Fatalf("an open in this process for city B got %T, want B's controller resolver", got)
	}
	done := markRigNamespaceControllerProcess()
	if got := current(cityPathA); got == ownSessionResolver(b) {
		t.Fatal("city A got city B's resolver")
	} else if _, ok := got.(unavailableSessionResolver); !ok {
		t.Fatalf("a controller process built %T for a city without a controller resolver; it must never build a one-shot resolver", got)
	}
	done()
	unregister()
	if got := current(cityPathB); got == ownSessionResolver(b) {
		t.Fatal("unregister left the resolver registered")
	}
}

func TestOneShotProcessBuildsOneCLIResolverPerCity(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	current := func(cityPath string) ownSessionResolver {
		return rigNamespaceSessionsForOpen(cityPath, nil).(rigNamespaceCityResolver).current()
	}
	first := current(cityPathB)
	if _, ok := first.(*citySessionResolver); !ok {
		t.Fatalf("one-shot resolver = %T", first)
	}
	if current(cityPathB) != first {
		t.Fatal("the one-shot resolver was rebuilt within the process")
	}
	if current(cityPathA) == first {
		t.Fatal("two cities share a one-shot resolver")
	}
}

// TestGuardedStoreConcurrentUseWithResolverSwaps reads and writes a guarded
// cache from many goroutines while the resolver is swapped, fed snapshots and
// started on new passes, as a reload and the controller loop do. Run under
// -race.
func TestGuardedStoreConcurrentUseWithResolverSwaps(t *testing.T) {
	sessions := newRigNamespaceSessions(cityPathB)
	storeB := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	sessions.setSessionStore(beadstest.Sessions(storeB), []string{"gcx"})
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), sessions)
	mem := beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)
	ep := events.NewFake()
	store := wrapWithCachingStoreFeed(context.Background(), guardRigStore(wrapStoreWithBeadPolicies(mem, &config.City{}), ns), ep, false, nil)

	done := make(chan struct{})
	errs := make(chan error, 64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rows, err := store.List(beads.ListQuery{AllowScan: true})
				if err != nil && !beads.IsPartialResult(err) {
					errs <- err
					return
				}
				for _, b := range rows {
					if ns.classify(b) == rowForeign {
						errs <- errors.New("a Foreign row came through: " + b.ID)
						return
					}
				}
				if _, err := store.Create(beads.Bead{Title: "w", Type: "task"}); err != nil {
					errs <- err
					return
				}
				if err := store.SetMetadata("bt-person", "k", "v"); !errors.Is(err, errRigNamespace) {
					errs <- errors.New("a write to a Foreign row was not refused")
					return
				}
			}
		}()
	}
	go func() {
		defer close(done)
		for k := 0; k < 50; k++ {
			sessions.setSessionStore(beadstest.Sessions(rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))), []string{"gcx"})
			sessions.publishSnapshot(newSessionBeadSnapshot([]beads.Bead{rnsSessionBead(poolIDB, templateB, poolNameB)}))
			sessions.beginPass()
		}
	}()
	wg.Wait()
	<-done
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestScopeInitProbeDoesNotClassifyRows is the supervisor start regression:
// the supervisor marks itself a controller process and starts the bead
// lifecycle before the city's controller state registers its resolver, so a
// guarded rig opened by path then has a resolver that admits no session. The
// rig init's liveness probe must still pass when the first row is held by
// one of this city's pool sessions (a row whose class needs a session
// lookup); a discovery read through the guard fails on it.
func TestScopeInitProbeDoesNotClassifyRows(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	done := markRigNamespaceControllerProcess()
	defer done()
	cfg := &config.City{Rigs: []config.Rig{{Name: "beta", Path: "/srv/beta", Prefix: "bt", Federation: config.RigFederationRig}}}
	ns, ok := newRigNamespace(cfg.Rigs[0], rigNamespaceSessionsForOpen(cityPathB, cfg))
	if !ok {
		t.Fatal("no namespace for the guarded rig")
	}
	engine := beads.NewMemStore()
	if _, err := engine.Create(beads.Bead{Title: "held by this city's pool session", Assignee: poolIDB}); err != nil {
		t.Fatal(err)
	}
	guarded := guardRigStore(wrapStoreWithBeadPolicies(engine, cfg), ns)
	if _, err := guarded.List(beads.ListQuery{AllowScan: true, Limit: 1}); err == nil {
		t.Fatal("a discovery read classified a pool session's row with no resolver registered; the probe below would not be needed")
	}
	sleeps := 0
	if err := verifyCanonicalBdScopeStoreReady(rigNamespaceProbeStore(guarded), func(time.Duration) { sleeps++ }); err != nil || sleeps != 0 {
		t.Fatalf("init probe = %v after %d retries; want the store reported live at once", err, sleeps)
	}
	if rigNamespaceProbeStore(guarded) != beads.Store(engine) {
		t.Fatalf("probe store = %T, want the engine under the guard", rigNamespaceProbeStore(guarded))
	}
	if plain := beads.NewMemStore(); rigNamespaceProbeStore(plain) != beads.Store(plain) {
		t.Fatal("an unguarded store was not probed as itself")
	}
}

// TestRigNamespaceSessionsReloadComparesTheUnderlyingStore: a reload re-mints
// the city's class handles over the same stores. The controller's resolver is
// rebuilt (and its memo dropped) only when the store under the sessions
// handle changes, judged by beads.SameStore: a handle re-minted over the same
// store, even through another class layer, keeps the resolver and its memo; a
// handle over another store, or no store, rebuilds it.
func TestRigNamespaceSessionsReloadComparesTheUnderlyingStore(t *testing.T) {
	h := newRigNamespaceSessions(cityPathB)
	store := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	h.setSessionStore(beadstest.Sessions(store), []string{"gcx"})
	first := h.res.Load()
	if _, ok, err := h.lookupOwnSession(poolIDB); err != nil || !ok {
		t.Fatalf("first lookup = %v, %v", ok, err)
	}
	gets := store.gets.Load()

	reminted := beads.SessionStore{Store: beadstest.Work(store)}
	if reminted == beadstest.Sessions(store) {
		t.Fatal("fixture: the re-minted handle must be a different handle value")
	}
	h.setSessionStore(reminted, []string{"gcx"})
	if h.res.Load() != first {
		t.Fatal("a handle re-minted over the same sessions store rebuilt the resolver, dropping its memo")
	}
	if _, ok, err := h.lookupOwnSession(poolIDB); err != nil || !ok || store.gets.Load() != gets {
		t.Fatalf("lookup after the re-mint = %v, %v with %d store reads (was %d); want the memo's answer", ok, err, store.gets.Load(), gets)
	}

	other := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	h.setSessionStore(beadstest.Sessions(other), []string{"gcx"})
	second := h.res.Load()
	if second == first {
		t.Fatal("a handle over another sessions store kept the old resolver")
	}
	h.setSessionStore(beads.SessionStore{}, []string{"gcx"})
	if h.res.Load() == second {
		t.Fatal("dropping the sessions store kept the old resolver")
	}
	zero := h.res.Load()
	h.setSessionStore(beads.SessionStore{}, []string{"gcx"})
	if h.res.Load() != zero {
		t.Fatal("re-setting no sessions store rebuilt the resolver")
	}
}

// TestRigNamespaceCLIResolverReadsTheSessionsBinding: the one-shot resolver
// of a guarded store opened by path opens the city's sessions handle,
// cliStores(cityPath, cityStore).Sessions(). On a split city that is the
// sessions binding, never the city's work store and never a rig store; on an
// unsplit city it is the city's own store.
func TestRigNamespaceCLIResolverReadsTheSessionsBinding(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\nprefix = \"hq\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "demo", Prefix: "hq"}, Rigs: []config.Rig{rnsGuardedRig("beta")}}

	t.Run("split", func(t *testing.T) {
		isolateRigNamespaceResolvers(t)
		binding := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
		seedSplitRoutes(t, cityPath, binding)
		handle, err := rigNamespaceOpenCityStore(cityPath, cfg)
		if err != nil {
			t.Fatalf("opening the city's sessions handle: %v", err)
		}
		if !beads.SameStore(handle, binding) {
			t.Fatalf("the CLI resolver's sessions handle is over %T, want the sessions binding", handle.Raw())
		}
		res := rigNamespaceSessionsForOpen(cityPath, cfg)
		rec, ok, err := res.lookupOwnSession(poolIDB)
		if err != nil || !ok || rec.Template != templateB {
			t.Fatalf("lookup through the CLI resolver = %+v, %v, %v; want the binding's record", rec, ok, err)
		}
		if binding.gets.Load() == 0 {
			t.Fatal("the CLI resolver answered without reading the sessions binding")
		}
	})
	t.Run("unsplit", func(t *testing.T) {
		isolateRigNamespaceResolvers(t)
		seedNoRoutes(t, cityPath)
		handle, err := rigNamespaceOpenCityStore(cityPath, cfg)
		if err != nil {
			t.Fatalf("opening the city's sessions handle: %v", err)
		}
		if _, isCityStore := beads.As[*beadPolicyStore](splitStrictFrontOf(handle.Raw())); !isCityStore {
			t.Fatalf("the CLI resolver's sessions handle on an unsplit city is over %T, want the city's own store (behind its policy layer)", handle.Raw())
		}
	})
}

// The cache-fed views the demand reads and the autoclose sweep take with
// beads.As: a cache, or the guard over one.
var (
	_ demandLegView        = (*beads.CachingStore)(nil)
	_ demandLegView        = (*rigNamespaceStore)(nil)
	_ autocloseCensusCache = (*beads.CachingStore)(nil)
	_ autocloseCensusCache = (*rigNamespaceStore)(nil)
)
