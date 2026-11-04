package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Tests for the paths around the guarded store: aliasing seen from either
// seam, the resolver across reloads and passes, the ledger feed and the
// autoclose sweep, the control-plane store, and the value rules of each
// write.

// TestUnguardedRigOnAGuardedRigsStoreIsRefusedAtBothSeams covers an
// unguarded rig whose store is a guarded rig's, by redirect or by database:
// neither seam may open it, and the guarded rig is refused too.
func TestUnguardedRigOnAGuardedRigsStoreIsRefusedAtBothSeams(t *testing.T) {
	for _, variant := range []string{"redirect", "same-db"} {
		t.Run(variant, func(t *testing.T) {
			isolateRigNamespaceResolvers(t)
			city := rnsBdCity(t)
			beta := filepath.Join(city, "beta")
			rnsScope(t, beta, `{"backend":"dolt","dolt_database":"beta"}`)
			alpha := filepath.Join(city, "alpha")
			switch variant {
			case "redirect":
				rnsScope(t, alpha, "")
				if err := os.WriteFile(filepath.Join(alpha, ".beads", "redirect"), []byte("../beta/.beads\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "same-db":
				rnsScope(t, alpha, `{"backend":"dolt","dolt_database":"beta"}`)
			}
			prevOpen := controllerStateOpenRigStoreAtForCity
			t.Cleanup(func() { controllerStateOpenRigStoreAtForCity = prevOpen })
			var opened atomic.Int32
			controllerStateOpenRigStoreAtForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
				opened.Add(1)
				return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
			}
			cfg := &config.City{
				Workspace: config.Workspace{Name: "demo"},
				Rigs: []config.Rig{
					{Name: "beta", Path: beta, Prefix: "bt", Federation: config.RigFederationRig},
					{Name: "alpha", Path: alpha, Prefix: "al"},
				},
			}
			cs := &controllerState{cityPath: city, cfg: cfg, rigSessions: newRigNamespaceSessions(city)}
			stores := cs.buildStores(cfg)
			for _, name := range []string{"alpha", "beta"} {
				if _, err := stores[name].List(beads.ListQuery{AllowScan: true}); err == nil {
					t.Fatalf("controller opened rig %s, which shares a guarded rig's store", name)
				}
			}
			if n := opened.Load(); n != 0 {
				t.Fatalf("the controller opened %d engines for refused rigs", n)
			}
			for _, scope := range []string{alpha, beta} {
				if _, err := guardStoreOpenResult(beads.StoreOpenResult{Store: wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)}, cfg, city, scope); err == nil {
					t.Fatalf("the by-path seam opened %s", scope)
				}
			}
		})
	}
}

// TestReloadThatGuardsTheFirstRigFindsTheCityResolver builds a controller
// state with no guarded rig and reloads into one: the resolver is registered
// from construction and the reload points it at the sessions store.
func TestReloadThatGuardsTheFirstRigFindsTheCityResolver(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	city := t.TempDir()
	beta := filepath.Join(city, "beta")
	for _, dir := range []string{city, beta} {
		if err := ensurePersistedScopeLocalFileStore(dir); err != nil {
			t.Fatal(err)
		}
	}
	plain := &config.City{Workspace: config.Workspace{Name: "demo"}, Rigs: []config.Rig{{Name: "beta", Path: beta, Prefix: "bt"}}}
	guarded := &config.City{Workspace: config.Workspace{Name: "demo"}, Rigs: []config.Rig{{Name: "beta", Path: beta, Prefix: "bt", Federation: config.RigFederationRig}}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sp := runtime.NewFake()
	cs := newControllerState(ctx, plain, sp, events.NewFake(), "demo", city)
	if rigNamespaceOfStore(cs.BeadStore("beta")) != nil {
		t.Fatal("an unguarded rig opened behind a guard")
	}
	res := rigNamespaceSessionsForOpen(city, guarded).(rigNamespaceCityResolver)
	if res.current() != ownSessionResolver(cs.rigSessions) {
		t.Fatalf("an open by path got %T before any rig was guarded; the city's resolver is registered at construction", res.current())
	}

	cs.update(guarded, sp)
	g := guardOf(t, cs.BeadStore("beta"))
	if g.ns.sessions != ownSessionResolver(cs.rigSessions) {
		t.Fatal("the reloaded guard does not use the city's resolver")
	}
	cs.rigSessions.mu.Lock()
	store := cs.rigSessions.store
	cs.rigSessions.mu.Unlock()
	if noSessionStore(store) {
		t.Fatal("the reload that guarded the first rig left the resolver without the sessions store")
	}
}

// TestOpenedGuardFollowsTheCitysResolver opens a guarded store by path in a
// controller process before the city's resolver is registered: the guard
// must not keep the stand-in it saw at open.
func TestOpenedGuardFollowsTheCitysResolver(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	done := markRigNamespaceControllerProcess()
	res := rigNamespaceSessionsForOpen(cityPathB, nil)
	if _, _, err := res.lookupOwnSession(poolIDB); !errors.Is(err, errRigNamespaceSessionsUnavailable) {
		t.Fatalf("lookup before the city's resolver is registered = %v; want unavailable", err)
	}
	h := newRigNamespaceSessions(cityPathB)
	h.setSessionStore(beadstest.Sessions(rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))), []string{"gcx"})
	unregister := registerRigNamespaceSessions(cityPathB, h)
	t.Cleanup(unregister)
	if rec, ok, err := res.lookupOwnSession(poolIDB); err != nil || !ok || rec.Template != templateB {
		t.Fatalf("lookup after registration through the earlier handle = %+v, %v, %v", rec, ok, err)
	}
	done()
	done()
	rigNamespaceResolvers.mu.Lock()
	n := rigNamespaceResolvers.controllerProcess
	rigNamespaceResolvers.mu.Unlock()
	if n != 0 {
		t.Fatalf("controllerProcess = %d after the controller ended; it must not stick", n)
	}
	if _, ok := rigNamespaceSessionsForOpen(cityPathA, nil).(rigNamespaceCityResolver).current().(*citySessionResolver); !ok {
		t.Fatal("a one-shot process does not build the city's one-shot resolver")
	}
}

// TestTickBeginsAResolverPass puts the city's resolver into its error
// backoff and runs one controller tick: the tick ends the backoff.
func TestTickBeginsAResolverPass(t *testing.T) {
	cr, cs := rgsRuntimeWithGuardedRig(t)
	sessions := cs.cityBeadStore.(*rnsCountingStore)
	sessions.setErr(errors.New("down"))
	if _, _, err := cs.rigSessions.lookupOwnSession("gcx-session-" + strings.Repeat("a", 32)); err == nil {
		t.Fatal("precondition: a store failure was not reported")
	}
	sessions.setErr(nil)
	if _, _, err := cs.rigSessions.lookupOwnSession("gcx-session-" + strings.Repeat("b", 32)); err == nil {
		t.Fatal("precondition: the backoff did not hold")
	}
	var dirty atomic.Bool
	lastProviderName := ""
	prevPoolRunning := map[string]bool{}
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "test")
	if _, _, err := cs.rigSessions.lookupOwnSession("gcx-session-" + strings.Repeat("c", 32)); err != nil {
		t.Fatalf("the tick did not begin a resolver pass: %v", err)
	}
}

// TestAutocloseSweepIgnoresForeignClosesInAGuardedStore hands the sweep the
// closes of another city's rows in a guarded rig's shared store, as the
// ledger feed's deferClose hook does: nothing here may act on them.
func TestAutocloseSweepIgnoresForeignClosesInAGuardedStore(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	backing := beads.NewMemStore()
	convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID, Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "alpha/crew.worker")})
	if err != nil {
		t.Fatal(err)
	}
	fconvoy, err := backing.Create(beads.Bead{Title: "their batch", Type: "convoy", Metadata: rnsMD(rigNamespaceOwnerStampKey, "alpha/")})
	if err != nil {
		t.Fatal(err)
	}
	fmember, err := backing.Create(beads.Bead{Title: "their task", ParentID: fconvoy.ID, Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "alpha/crew.worker")})
	if err != nil {
		t.Fatal(err)
	}
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	guarded := wrapStoreWithBeadPolicies(newRigNamespaceStore(cached, ns), &config.City{})
	cs := &controllerState{beadStores: map[string]beads.Store{"beta": guarded}, eventProv: events.NewFake()}
	cs.runAutocloseSweepPass(sweepTestClock(0))
	for _, id := range []string{member.ID, fmember.ID} {
		if err := backing.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	sweep := cs.autocloseSweepOf()
	sweep.deferID(member.ID, sweepTestClock(1))
	sweep.deferID(fmember.ID, sweepTestClock(1))
	var ran int
	for i := 1; i <= 4; i++ {
		ran += cs.runAutocloseSweepPass(sweepTestClock(i)).Ran
	}
	if ran != 0 {
		t.Fatalf("the sweep ran autoclose %d times for another city's closes", ran)
	}
	for _, id := range []string{convoy.ID, fconvoy.ID} {
		if b, _ := backing.Get(id); b.Status == "closed" {
			t.Fatalf("convoy %s was closed because another city's member closed", id)
		}
	}
}

// TestLedgerFeedDeliversNoForeignRowOfAGuardedScope steps a guarded scope's
// feed over another city's close and this city's own: only this city's row
// is delivered to the sweep and the wake.
func TestLedgerFeedDeliversNoForeignRowOfAGuardedScope(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })
	ns, _ := rnsCityBNamespace(t)
	store := newJournalingStore()
	h := newFeedHarness(t, store)
	h.feed.ns = ns
	h.step()
	theirs, err := store.Create(beads.Bead{ID: "bt-theirs", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "alpha/crew.worker")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(theirs.ID); err != nil {
		t.Fatal(err)
	}
	h.pokes()
	h.step()
	if len(h.deferred) != 0 {
		t.Fatalf("deferred another city's close to the sweep: %v", h.deferred)
	}
	if poke, dispatch := h.pokes(); poke+dispatch != 0 {
		t.Fatalf("another city's close woke this one (%d/%d)", poke, dispatch)
	}
	ours, err := store.Create(beads.Bead{ID: "bt-ours", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(ours.ID); err != nil {
		t.Fatal(err)
	}
	h.step()
	if !slices.Equal(h.deferred, []string{ours.ID}) {
		t.Fatalf("deferred = %v, want this city's own close", h.deferred)
	}
}

func TestControlStoreOfAGuardedRigIsGuarded(t *testing.T) {
	isolateRigNamespaceResolvers(t)
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	city := t.TempDir()
	beta := filepath.Join(city, "beta")
	for _, dir := range []string{city, beta} {
		if err := ensurePersistedScopeLocalFileStore(dir); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}, Rigs: []config.Rig{{Name: "beta", Path: beta, Prefix: "bt", Federation: config.RigFederationRig}}}
	store, err := openControlStoreAtForCity(beta, city, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })
	if ns := rigNamespaceOfStore(store); ns == nil || ns.rigName() != "beta" {
		t.Fatalf("control store %T is not behind beta's guard", store)
	}
	cityStore, err := openControlStoreAtForCity(city, city, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(cityStore) })
	if rigNamespaceOfStore(cityStore) != nil {
		t.Fatal("the city's control store got a rig guard")
	}
}

func TestRigNamespaceStoreConditionalWritesAtAStaleRevisionWriteNothing(t *testing.T) {
	g, mem, _ := rgsGuard(t)
	own, _ := mem.Get("bt-own")
	stale := own.Revision + 1000
	cw, _ := g.ConditionalWriterHandle()
	closer, _ := g.AtomicConditionalCloserHandle()
	title := "x"
	for name, write := range map[string]func() error{
		"UpdateIfMatch":            func() error { return cw.UpdateIfMatch("bt-own", stale, beads.UpdateOpts{Title: &title}) },
		"CloseIfMatch":             func() error { return cw.CloseIfMatch("bt-own", stale) },
		"DeleteIfMatch":            func() error { return cw.DeleteIfMatch("bt-own", stale) },
		"CloseWithMetadataIfMatch": func() error { _, err := closer.CloseWithMetadataIfMatch("bt-own", stale, nil); return err },
	} {
		var pf *beads.PreconditionFailedError
		if err := write(); !errors.As(err, &pf) || pf.Current != own.Revision {
			t.Fatalf("%s at a stale revision = %v; want a precondition failure naming the current revision", name, err)
		}
	}
	after, _ := mem.Get("bt-own")
	if after.Revision != own.Revision || after.Status != own.Status {
		t.Fatal("a write at a stale revision changed the row")
	}
	if err := cw.UpdateIfMatch("bt-own", own.Revision, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("UpdateIfMatch at the current revision: %v", err)
	}
}

func TestRigNamespaceStoreGuardedBatchRefusesAMissingRow(t *testing.T) {
	g, _, _ := rgsGuard(t)
	spy := &rgsBatchSpy{}
	title := "x"
	_, err := rigNamespaceBatcher{s: g, inner: spy}.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: "bt-missing", Opts: beads.UpdateOpts{Title: &title}}}}})
	var item *beads.BatchItemError
	if !errors.As(err, &item) || !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("a batch updating a missing row = %v; want the item refused as not found", err)
	}
	if len(spy.got.Items) != 0 {
		t.Fatal("a batch with an unjudged update reached the inner batcher")
	}
}

func TestRigNamespaceStoreHolderAndStampRules(t *testing.T) {
	g, mem, _ := rgsGuard(t)
	session, err := mem.Create(beads.Bead{ID: "bt-sess", Title: "s", Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession}, Metadata: rnsMD("template", templateB)})
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]func() error{
		"deferred assignee outside on an own row": func() error {
			return g.SetMetadata("bt-own", beadmeta.DeferredAssigneeMetadataKey, "alice")
		},
		"stranded assignee outside on an own row": func() error {
			return g.SetMetadata("bt-own", beadmeta.StrandedAssigneeMetadataKey, shortIDB)
		},
		"another city's session id stamp": func() error {
			return g.SetMetadata("bt-unattr", beadmeta.SessionIDMetadataKey, poolIDA)
		},
		"another city's pool id stamp on an own row": func() error {
			return g.SetMetadata("bt-own", beadmeta.SessionIDMetadataKey, poolIDA)
		},
		"a short session id stamp": func() error {
			return g.SetMetadata("bt-unattr", beadmeta.SessionIDMetadataKey, shortIDB)
		},
		"a session template outside the rig": func() error {
			return g.SetMetadata(session.ID, "template", templateA)
		},
		"a session alias outside the rig": func() error {
			return g.SetMetadata(session.ID, "alias", "alice")
		},
		"a create stamped with another city's session": func() error {
			_, err := g.Create(beads.Bead{Title: "x", Metadata: rnsMD(beadmeta.SessionIDMetadataKey, poolIDA)})
			return err
		},
		"a session row created for another rig's template": func() error {
			_, err := g.Create(beads.Bead{Title: "x", Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession}, Metadata: rnsMD("template", templateA)})
			return err
		},
	}
	for name, write := range refused {
		if err := write(); !errors.Is(err, errRigNamespace) {
			t.Fatalf("%s: %v; want refused", name, err)
		}
	}
	allowed := map[string]func() error{
		"own pool session stamp": func() error { return g.SetMetadata("bt-unattr", beadmeta.SessionIDMetadataKey, poolIDB) },
		"own named session's short id on an own row": func() error {
			return g.SetMetadata("bt-own", beadmeta.StrandedSessionMetadataKey, shortIDB)
		},
		"cleared stamp":           func() error { return g.SetMetadata("bt-orphan", beadmeta.SessionIDMetadataKey, "") },
		"deferred assignee in ns": func() error { return g.SetMetadata("bt-own", beadmeta.DeferredAssigneeMetadataKey, "beta/crew.worker") },
		"session template in rig": func() error { return g.SetMetadata(session.ID, "template", "beta/crew.lead") },
	}
	for name, write := range allowed {
		if err := write(); err != nil {
			t.Fatalf("%s: %v; want allowed", name, err)
		}
	}
}

func TestRigNamespaceStoreStructuralReadsGateTheirIdentity(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := &rgsCountingStore{Store: beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}
	g := newRigNamespaceStore(inner, ns)
	rows, err := g.List(beads.ListQuery{IDs: []string{"bt-person"}, Assignee: "alice"})
	if err != nil || len(rows) != 0 || inner.lists.Load() != 0 {
		t.Fatalf("List{IDs, Assignee: alice} = %v, %v after %d reads; want empty without reading", rgsIDs(rows), err, inner.lists.Load())
	}
	if n, err := g.Count(context.Background(), beads.ListQuery{ParentID: "bt-x", Assignee: "alice"}); n != 0 || err != nil {
		t.Fatalf("Count{ParentID, Assignee: alice} = %d, %v; want 0", n, err)
	}
	rows, err = g.List(beads.ListQuery{IDs: []string{"bt-person"}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("List{IDs} of a Foreign row = %v, %v; reads by id pass through", rgsIDs(rows), err)
	}
}

func TestRigNamespaceStoreHandsOutNoProxiedLeaf(t *testing.T) {
	g, _, _ := rgsGuard(t)
	if view, ok := g.ProxiedStore(); ok || view != nil {
		t.Fatal("the guard handed out a proxied store view, whose leaves bypass it")
	}
}

// rgsBdBackedCache is a cache over a bd store: it answers cached reads from
// memory and names its bd backing, as a controller's cache over a bd rig does.
type rgsBdBackedCache struct {
	*beads.MemStore
	bd *beads.BdStore
}

func (c rgsBdBackedCache) CachedList(q beads.ListQuery) ([]beads.Bead, bool) {
	rows, err := c.List(q)
	return rows, err == nil
}

func (c rgsBdBackedCache) Backing() beads.Store { return c.bd }

// TestCloseTimeListerReportsAnUndecidableGuardedRead covers a bd-backed
// guarded leg, which has no live fallback: a cached read the guard could not
// decide must be an error, never "no late work".
func TestCloseTimeListerReportsAnUndecidableGuardedRead(t *testing.T) {
	inner := rgsBdBackedCache{MemStore: beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil), bd: beads.NewBdStore(t.TempDir(), noopBdRunner())}
	lister := func(ns *rigNamespace) func(beads.ListQuery) ([]beads.Bead, error) {
		list := closeTimeLister(wrapStoreWithBeadPolicies(newRigNamespaceStore(inner, ns), &config.City{}), false)
		if list == nil {
			t.Fatal("no lister for a cached bd leg")
		}
		return list
	}
	cleanNS, _ := rnsCityBNamespace(t)
	if rows, err := lister(cleanNS)(beads.ListQuery{AllowScan: true}); err != nil || len(rows) == 0 {
		t.Fatalf("clean read = %v, %v", rgsIDs(rows), err)
	}
	downNS, downSessions := rnsCityBNamespace(t)
	downSessions.setErr(errors.New("down"))
	if rows, err := lister(downNS)(beads.ListQuery{AllowScan: true}); err == nil {
		t.Fatalf("a read the guard could not decide answered %v with no error: it would read as no late work", rgsIDs(rows))
	}
}
