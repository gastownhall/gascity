package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// fullyCapableBackingStore is a beads.Store test double that implements every
// optional capability route_clear_store.go must forward: beads.GraphApplyStore
// (via ApplyGraphPlan), beads.BatchDeleter, beads.Counter, beads.RowWitness,
// beads.ExactBatchGetter (via GetExactBatch) and beads.ContextReadyReader (via
// ReadyContext). None of the fast/hermetic test backends (FileStore, MemStore)
// implement ApplyGraphPlan, DeleteBatch, Count, SawRows or GetExactBatch -- only
// BdStore, NativeDoltStore and CachingStore implement any of them, and none of
// those are reachable from a cheap, hermetic unit test through the real
// openStoreAtForCity factory (it has no seam to inject a fake backing store,
// and the native/bd providers require a real preflight-gated store). A
// capability-bearing double standing in for that real backend is therefore
// the only way to prove the *forwarding* is correct in isolation from
// whether a given production backend happens to implement the capability.
//
// ReadyContext is different: the embedded beads.NewMemStore() genuinely
// implements it. But embedding is through the beads.Store INTERFACE field,
// which only promotes methods the Store interface itself declares --
// ContextReadyReader is an optional capability interface, not part of Store,
// so MemStore's ReadyContext is invisible through this double exactly like
// every other capability in ga-8q8z2w is invisible through
// routeChangeClearingStore. Re-declaring it here, forwarding to the same
// embedded MemStore, is this test double hitting the same bug it exists to
// catch -- which is why the forward below is a type-assert-and-call rather
// than relying on promotion.
type fullyCapableBackingStore struct {
	beads.Store

	graphPlanApplied *beads.GraphApplyPlan
	graphApplyErr    error
	deletedIDs       []string
	deleteBatchErr   error
	countCalls       int
	countErr         error
	sawRows          bool
	exactBatchIDs    []string
	exactBatchErr    error
}

func (s *fullyCapableBackingStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	s.graphPlanApplied = plan
	return &beads.GraphApplyResult{}, s.graphApplyErr
}

func (s *fullyCapableBackingStore) DeleteBatch(ids []string) error {
	s.deletedIDs = append(s.deletedIDs, ids...)
	return s.deleteBatchErr
}

func (s *fullyCapableBackingStore) Count(_ context.Context, _ beads.ListQuery, _ ...string) (int, error) {
	s.countCalls++
	return 7, s.countErr
}

func (s *fullyCapableBackingStore) SawRows() bool {
	return s.sawRows
}

func (s *fullyCapableBackingStore) GetExactBatch(ids []string) (map[string]beads.Bead, []string, error) {
	s.exactBatchIDs = append(s.exactBatchIDs, ids...)
	if len(ids) == 0 {
		return map[string]beads.Bead{}, nil, s.exactBatchErr
	}
	return map[string]beads.Bead{ids[0]: {ID: ids[0]}}, ids[1:], s.exactBatchErr
}

func (s *fullyCapableBackingStore) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	reader, ok := s.Store.(beads.ContextReadyReader)
	if !ok {
		return nil, fmt.Errorf("reading ready beads from backing store: %w", beads.ErrReadyContextUnsupported)
	}
	return reader.ReadyContext(ctx, query...)
}

// TestRouteClearForwardsOptionalCapabilitiesThroughProductionComposition
// composes a store exactly the way openStoreAtForCityWithConfig does
// (cmd/gc/main.go) -- wrapStoreWithBeadPolicies, then
// beads.WithRouteChangeClearing on top -- and asserts that
// beads.GraphApplyFor, beads.HandlesFor (tier expansion), beads.Counter,
// beads.BatchDeleter, beads.RowWitness and beads.ExactBatchGetter all still
// resolve through the outermost (route-clear) wrapper.
//
// Per ga-8q8z2w: routeChangeClearingStore embeds the Store INTERFACE, so it
// only promotes the capabilities it re-declares -- every one of these is
// invisible through it today even though the backing store (and the policy
// layer wrapping it) fully supports them.
//
// If a future change moves openStoreAtForCityWithConfig's composition order
// off wrapStoreWithBeadPolicies-then-WithRouteChangeClearing, update this
// test's setup to match; TestOpenStoreAtForCityForwardsReadyContextThroughRouteClear
// below exercises the real factory end to end as a cross-check.
func TestRouteClearForwardsOptionalCapabilitiesThroughProductionComposition(t *testing.T) {
	backing := &fullyCapableBackingStore{Store: beads.NewMemStore(), sawRows: true}
	policyWrapped := wrapStoreWithBeadPolicies(backing, &config.City{})
	store := beads.WithRouteChangeClearing(policyWrapped, func(target string) string {
		return target
	})

	t.Run("GraphApplyFor", func(t *testing.T) {
		applier, ok := beads.GraphApplyFor(store)
		if !ok {
			t.Fatal("GraphApplyFor(store) ok = false, want true: route-clear-wrapped store does not resolve GraphApplyStore")
		}
		plan := &beads.GraphApplyPlan{
			Nodes: []beads.GraphApplyNode{
				{Key: "root", Title: "Root", Metadata: map[string]string{"gc.kind": "wisp"}},
			},
		}
		if _, err := applier.ApplyGraphPlan(context.Background(), plan); err != nil {
			t.Fatalf("ApplyGraphPlan: %v", err)
		}
		if backing.graphPlanApplied == nil {
			t.Fatal("graph plan was not forwarded to the backing store")
		}
	})

	t.Run("BatchDeleter", func(t *testing.T) {
		deleter, ok := store.(beads.BatchDeleter)
		if !ok {
			t.Fatal("store.(beads.BatchDeleter) ok = false, want true: route-clear-wrapped store does not resolve BatchDeleter")
		}
		if err := deleter.DeleteBatch([]string{"bead-1", "bead-2"}); err != nil {
			t.Fatalf("DeleteBatch: %v", err)
		}
		if len(backing.deletedIDs) != 2 {
			t.Fatalf("backing.deletedIDs = %v, want 2 forwarded IDs", backing.deletedIDs)
		}
	})

	t.Run("Counter", func(t *testing.T) {
		counter, ok := store.(beads.Counter)
		if !ok {
			t.Fatal("store.(beads.Counter) ok = false, want true: route-clear-wrapped store does not resolve Counter")
		}
		n, err := counter.Count(context.Background(), beads.ListQuery{})
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 7 {
			t.Fatalf("Count = %d, want 7 (forwarded from backing store)", n)
		}
		if backing.countCalls != 1 {
			t.Fatalf("backing.countCalls = %d, want 1", backing.countCalls)
		}
	})

	t.Run("RowWitness", func(t *testing.T) {
		witness, ok := store.(beads.RowWitness)
		if !ok {
			t.Fatal("store.(beads.RowWitness) ok = false, want true: route-clear-wrapped store does not resolve RowWitness")
		}
		if !witness.SawRows() {
			t.Fatal("SawRows() = false, want true: backing store has seen rows")
		}
	})

	t.Run("HandlesForTierExpansion", func(t *testing.T) {
		handles := beads.HandlesFor(store)
		if _, ok := handles.Cached.(beadPolicyCachedReader); !ok {
			t.Fatalf("Handles().Cached = %T, want beadPolicyCachedReader: policy read-tier expansion is lost once route-clear wraps it", handles.Cached)
		}
		if _, ok := handles.Live.(beadPolicyLiveReader); !ok {
			t.Fatalf("Handles().Live = %T, want beadPolicyLiveReader: policy read-tier expansion is lost once route-clear wraps it", handles.Live)
		}
		// The outermost wrapper must own Handles().Writer, or a caller using
		// it to perform metadata writes silently bypasses route-clear's own
		// write-interception (the entire point of this decorator).
		if any(handles.Writer) != any(store) {
			t.Fatalf("Handles().Writer = %v, want the route-clear-wrapped store itself", handles.Writer)
		}
	})

	t.Run("ReadyContext", func(t *testing.T) {
		reader, ok := store.(beads.ContextReadyReader)
		if !ok {
			t.Fatal("store.(beads.ContextReadyReader) ok = false, want true: route-clear-wrapped store does not resolve ContextReadyReader")
		}
		if _, err := reader.ReadyContext(context.Background()); err != nil {
			t.Fatalf("ReadyContext: %v", err)
		}
	})

	t.Run("ExactBatchGetter", func(t *testing.T) {
		getter, ok := store.(beads.ExactBatchGetter)
		if !ok {
			t.Fatal("store.(beads.ExactBatchGetter) ok = false, want true: route-clear-wrapped store does not resolve ExactBatchGetter, so the gc bd bulk-mutation guard reads one bead per id instead of one batch")
		}
		found, unresolved, err := getter.GetExactBatch([]string{"bead-1", "bead-2"})
		if err != nil {
			t.Fatalf("GetExactBatch: %v", err)
		}
		if _, ok := found["bead-1"]; !ok || len(found) != 1 {
			t.Fatalf("found = %v, want exactly bead-1 (forwarded from backing store)", found)
		}
		if len(unresolved) != 1 || unresolved[0] != "bead-2" {
			t.Fatalf("unresolved = %v, want [bead-2] (forwarded from backing store)", unresolved)
		}
		if len(backing.exactBatchIDs) != 2 {
			t.Fatalf("backing.exactBatchIDs = %v, want 2 forwarded IDs", backing.exactBatchIDs)
		}

		// A backing failure must reach the caller unchanged: the gc bd guard
		// takes the per-id path on ANY batch error, which only works if the
		// forward neither swallows nor rewrites it.
		errBatch := errors.New("bd show failed")
		backing.exactBatchErr = errBatch
		if _, _, err := getter.GetExactBatch([]string{"bead-3"}); !errors.Is(err, errBatch) {
			t.Fatalf("GetExactBatch error = %v, want the backing store's error forwarded", err)
		}
	})
}

// TestRouteClearExactBatchWithoutBackingSupportReportsUnsupported pins the miss
// shape of the exact batch forward. A backing store with no exact batch read
// must answer beads.ErrExactBatchGetUnsupported -- the sentinel the gc bd
// mutation guard falls back to a per-id Get on -- rather than an empty, successful
// read that would make every requested id look absent.
func TestRouteClearExactBatchWithoutBackingSupportReportsUnsupported(t *testing.T) {
	store := beads.WithRouteChangeClearing(beads.NewMemStore(), func(target string) string { return target })

	getter, ok := store.(beads.ExactBatchGetter)
	if !ok {
		t.Fatal("store.(beads.ExactBatchGetter) ok = false, want true: the decorator must always answer the capability so a miss is explicit")
	}
	if _, _, err := getter.GetExactBatch([]string{"bead-1", "bead-2"}); !errors.Is(err, beads.ErrExactBatchGetUnsupported) {
		t.Fatalf("GetExactBatch error = %v, want errors.Is(err, beads.ErrExactBatchGetUnsupported)", err)
	}
}

// TestRouteClearCarriesTheProxiedStoreView pins the beads.ProxiedStoreCarrier
// seam (internal/beads/proxied_store_view.go) through route_clear_store.go.
//
// internal/doctor and beads.LiveProxiedDiagnostic ask a store whether the split
// store underneath it is still serving natively or has stood down, through
// beads.ProxiedStoreFrom. That walk only knows the wrappers it is told about and
// anything that declares ProxiedStore(); routeChangeClearingStore embeds the
// Store INTERFACE, so it promotes nothing outside that interface. It is also the
// OUTERMOST wrapper on every CLI/standalone open (cmd/gc/main.go composes
// wrapStoreWithBeadPolicies, then beads.WithRouteChangeClearing), which makes it
// the first thing the walk meets. Without a forward, a proxied city's CLI handle
// answers "not a split store" and `gc doctor` keeps reporting the account
// recorded at open for a handle that has since stood down -- the same defect
// class as ga-8q8z2w, one optional capability later.
func TestRouteClearCarriesTheProxiedStoreView(t *testing.T) {
	identity := func(target string) string { return target }

	t.Run("ThroughProductionComposition", func(t *testing.T) {
		view := &fakeProxiedStore{bdLeaf: beads.NewMemStore()}
		store := beads.WithRouteChangeClearing(
			wrapStoreWithBeadPolicies(beads.NewCachingStoreForTest(view, nil), &config.City{}),
			identity,
		)

		carried, ok := beads.ProxiedStoreFrom(store)
		if !ok {
			t.Fatal("ProxiedStoreFrom(routeClear(policy(cache(proxied)))) ok = false, want true: the outermost wrapper hides the split store")
		}
		if any(carried) != any(view) {
			t.Fatalf("carried view = %T %p, want the split store underneath (%p): a copy would stop tracking its stand-down", carried, carried, view)
		}

		// The view is live, not a snapshot: a stand-down AFTER the handle was
		// opened must show through the decorator, which is the one thing
		// `gc doctor` exists to tell an operator about a degraded city.
		if carried.Demoted() {
			t.Fatal("carried view reports demoted while the native leaf is serving")
		}
		view.demoted = true
		if !carried.Demoted() {
			t.Fatal("carried view did not observe the stand-down of the store underneath it")
		}
	})

	t.Run("LiveProxiedDiagnosticReachesTheSplitStore", func(t *testing.T) {
		view := &fakeProxiedStore{
			demoted: true,
			bdLeaf:  beads.NewMemStore(),
			report:  beads.ProxiedOpenReport{Demoted: true},
		}
		store := beads.WithRouteChangeClearing(wrapStoreWithBeadPolicies(view, &config.City{}), identity)

		got := beads.LiveProxiedDiagnostic(store, nil)
		if got == nil {
			t.Fatal("LiveProxiedDiagnostic(routeClear(policy(proxied)), nil) = nil, want the live account: the decorator hides the split store from the doctor's projection")
		}
		if !got.Demoted {
			t.Fatal("LiveProxiedDiagnostic did not carry the live demoted account through the decorator")
		}
	})

	t.Run("NonProxiedStoreStillAnswersFalse", func(t *testing.T) {
		store := beads.WithRouteChangeClearing(wrapStoreWithBeadPolicies(beads.NewMemStore(), &config.City{}), identity)

		if view, ok := beads.ProxiedStoreFrom(store); ok {
			t.Fatalf("a route-clear-wrapped MemStore answered as a proxied store: %T", view)
		}
	})
}

// routeClearIdentity is the route normalizer for tests that do not exercise
// pool-slot-suffix collapsing.
func routeClearIdentity(target string) string { return target }

// The route-change-clearing decorator is one of the layers a split city's
// binding is wrapped in: storage boot puts it on the engine it opens
// (openStorageRoutes), and the one-shot CLI's emitter and the controller's
// CachingStore then stack on top. Everything that asks "what engine is this?"
// or "what can this store do?" of a class store therefore reaches it, which is
// what the tests below hold it to.

// TestBindingEnginePeelsRouteClear pins the engine-identity walk. Questions
// about the engine itself -- whether the sessions ledger is the SQLite one,
// which store a snapshot cache should sit on -- are asked of bindingEngine, and
// a layer it does not know reads as an unrecognized store.
func TestBindingEnginePeelsRouteClear(t *testing.T) {
	engine := openBindingEngineForTest(t)
	guarded := beads.WithRouteChangeClearing(engine, routeClearIdentity)
	emitted := splitClassRoutes(guarded).withCLIEmission(t.TempDir()).stores[coordclass.ClassGraph]

	for name, store := range map[string]beads.Store{
		"the bare engine":                      engine,
		"the decorator over the engine":        guarded,
		"the controller's cache over it":       beads.NewCachingStoreForTest(guarded, nil),
		"the one-shot emitter over the engine": emitted,
	} {
		if got := bindingEngine(store); got != beads.Store(engine) {
			t.Errorf("bindingEngine(%s) = %T, want the bare engine", name, got)
		}
	}

	other := beads.NewMemStore()
	if got := bindingEngine(other); got != beads.Store(other) {
		t.Errorf("bindingEngine of a store that is none of the layers = %T, want it untouched", got)
	}
}

// TestIsRelocatedSQLiteSessionLedgerSeesThroughRouteClear is the same hazard one
// caller up: the closed-session purge asks whether the sessions class is served
// by the SQLite ledger through bindingEngine, and an answer of "no" is silent --
// the purge simply stops on every split city.
func TestIsRelocatedSQLiteSessionLedgerSeesThroughRouteClear(t *testing.T) {
	ledger := openSessionPurgeSQLiteStore(t)
	guarded := beads.WithRouteChangeClearing(ledger, routeClearIdentity)

	// The one-shot shape: the class store is the decorator over the ledger.
	if !isRelocatedSQLiteSessionLedger(wholeSplitRoutes(guarded), guarded, beads.NewMemStore()) {
		t.Fatal("isRelocatedSQLiteSessionLedger over the decorated ledger = false, want true: the decorator hid the engine")
	}

	// The controller's shape: its cache over the decorator over the ledger.
	routes := wholeSplitRoutes(guarded).withControllerCache(context.Background(), nil)
	sessions := routes.stores[coordclass.ClassSessions]
	if _, cached := sessions.(*beads.CachingStore); !cached {
		t.Fatalf("sessions class is %T, want the controller's cache", sessions)
	}
	if !isRelocatedSQLiteSessionLedger(routes, sessions, beads.NewMemStore()) {
		t.Fatal("isRelocatedSQLiteSessionLedger over the cached, decorated ledger = false, want true")
	}
}

func atomicCloserDiscovery(store beads.Store) bool {
	_, ok := beads.AtomicConditionalCloserFor(store)
	return ok
}

func conditionalWriterDiscovery(store beads.Store) bool {
	_, ok := beads.ConditionalWriterFor(store)
	return ok
}

// routeClearOmittedEngineCapabilities are the binding-engine methods the
// decorator does not carry structurally, each with why. A method listed with a
// discovery must still be found through its handle over a SQLite engine. The
// rest are engine-only: lifecycle and recovery open the engine themselves, so
// nothing reaches them through a class store.
var routeClearOmittedEngineCapabilities = map[string]func(beads.Store) bool{
	"ApplyGraphPlan": func(store beads.Store) bool {
		_, ok := beads.GraphApplyFor(store)
		return ok
	},
	"ApplyGraphPlanWithStorage": func(store beads.Store) bool {
		applier, ok := beads.GraphApplyFor(store)
		_, storage := applier.(beads.StorageGraphApplyStore)
		return ok && storage
	},
	// GraphApplyFor hands back the engine's own applier, so the engine's answer
	// to this question -- absence, for SQLite -- is what a caller gets.
	"SupportsEphemeralGraphApply": nil,
	// Resolved through ConditionalWritesResolveTarget to the terminal engine.
	"CloseWithMetadataIfMatch":      atomicCloserDiscovery,
	"AtomicConditionalCloserHandle": atomicCloserDiscovery,
	// The revision-fenced writers are discovered through handles that return the
	// route-clearing gate's wrapper over the backing's writer: UpdateIfMatch and
	// CompareAndSetMetadataKey run the clear after a gc.routed_to write lands
	// (TestRouteClearSeam_*), CloseIfMatch and DeleteIfMatch pass straight
	// through because neither can re-route a bead.
	"UpdateIfMatch": conditionalWriterDiscovery,
	"CloseIfMatch":  conditionalWriterDiscovery,
	"DeleteIfMatch": conditionalWriterDiscovery,
	"CompareAndSetMetadataKey": func(store beads.Store) bool {
		_, ok := beads.MetadataCASWriterFor(store)
		return ok
	},
	// ApplyGuarded is discovered through GuardedBatcherHandle, which answers the
	// route-clearing gate over the backing's batcher: a batch Update that writes
	// gc.routed_to runs the clear after the batch lands
	// (TestRouteClearGuardedBatch_*). A structural ApplyGuarded would answer for a
	// backing with no batcher.
	"ApplyGuarded": func(store beads.Store) bool {
		_, ok := beads.GuardedBatcherFor(store)
		return ok
	},
	// Discovered through NamespaceCensusHandle only. Carrying HasResidentOutside
	// structurally would advertise a census over a backing that has none, and the
	// boot verdict would retire a by-id probe on an answer nobody computed.
	"HasResidentOutside": func(store beads.Store) bool {
		_, ok := beads.NamespaceCensusFor(store)
		return ok
	},
	"CloseStore":           nil,
	"SequenceFloor":        nil,
	"SetSequenceFloor":     nil,
	"AdvanceSequenceFloor": nil,
	"StoreHealthPath":      nil,
}

// TestRouteClearCarriesEveryBindingEngineCapability holds the decorator to the
// same engine method sets the emitter and the controller's cache are held to.
// Optional capabilities are discovered by type assertion, so a method the
// decorator drops does not fail anywhere: the caller's assertion simply stops
// matching and it takes a slower or weaker path.
func TestRouteClearCarriesEveryBindingEngineCapability(t *testing.T) {
	// The decorator is one concrete type over a backing that declares edge
	// completeness and another over one that does not (ga-l69b9m, architect
	// Amendment 2); both are held to the engines' method sets.
	for _, wrapper := range []reflect.Type{
		reflect.TypeOf(beads.WithRouteChangeClearing(beads.NewMemStore(), routeClearIdentity)),
		reflect.TypeOf(beads.WithRouteChangeClearing(openBindingEngineForTest(t), routeClearIdentity)),
	} {
		for _, engine := range bindingEngineTypes {
			var missing []string
			for i := 0; i < engine.NumMethod(); i++ {
				method := engine.Method(i)
				got, ok := wrapper.MethodByName(method.Name)
				if !ok {
					if _, omitted := routeClearOmittedEngineCapabilities[method.Name]; !omitted {
						missing = append(missing, method.Name)
					}
					continue
				}
				// Compare the signatures without their receivers.
				if got.Type.String() != strings.Replace(method.Type.String(), engine.String(), wrapper.String(), 1) {
					missing = append(missing, method.Name+" (signature differs)")
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("the route-change-clearing decorator (%s) drops %v from %s; every one is a capability assertion that stops matching", wrapper, missing, engine)
			}
		}
		for name := range routeClearOmittedEngineCapabilities {
			if _, structural := wrapper.MethodByName(name); structural {
				t.Errorf("%s is listed as omitted but the decorator (%s) carries it; drop it from the list", name, wrapper)
			}
		}
	}

	carried := beads.WithRouteChangeClearing(openBindingEngineForTest(t), routeClearIdentity)
	for name, discover := range routeClearOmittedEngineCapabilities {
		if discover != nil && !discover(carried) {
			t.Errorf("%s is not discoverable through the decorator over a SQLite engine", name)
		}
	}
}

// TestRouteClearCarriesAnEdgePayload covers the decorator's edge-payload write,
// which the reflective audit forces to EXIST but says nothing about the
// behavior of. Without it the one-shot CLI over a split city refuses every
// payload-bearing dependency edge, which is what formula gating writes.
func TestRouteClearCarriesAnEdgePayload(t *testing.T) {
	leaf, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening the leaf store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(leaf) })
	from := seedClassBead(t, leaf, "gated")
	to := seedClassBead(t, leaf, "gates-it")
	guarded := beads.WithRouteChangeClearing(leaf, routeClearIdentity)

	writer, ok := guarded.(beads.DepMetadataWriter)
	if !ok {
		t.Fatalf("the decorator %T cannot carry an edge payload", guarded)
	}
	const payload = `{"gate":"waits_for"}`
	if err := writer.DepAddWithMetadata(from.ID, to.ID, "blocks", payload); err != nil {
		t.Fatalf("carrying an edge payload through the decorator: %v", err)
	}
	reader, ok := guarded.(beads.DepMetadataReader)
	if !ok {
		t.Fatalf("the decorator %T cannot report an edge payload", guarded)
	}
	got, carried, err := reader.DepMetadata(from.ID, to.ID)
	if err != nil {
		t.Fatalf("reading the payload back: %v", err)
	}
	if !carried || got != payload {
		t.Fatalf("the edge carries (%q, %v), want (%q, true): the decorator wrote the edge without its payload", got, carried, payload)
	}
}

// TestRouteClearRefusesAnEdgePayloadItsBackingCannotHold is the other half: a
// backing with no writer must produce an ERROR, never a silent plain DepAdd,
// which on a store keeping the payload in a sidecar would clear a payload the
// edge already had.
func TestRouteClearRefusesAnEdgePayloadItsBackingCannotHold(t *testing.T) {
	backing := beads.NewMemStore()
	if _, ok := beads.Store(backing).(beads.DepMetadataWriter); ok {
		t.Fatal("MemStore now carries edge payloads, so it is no longer the backing this test needs")
	}
	writer, ok := beads.WithRouteChangeClearing(backing, routeClearIdentity).(beads.DepMetadataWriter)
	if !ok {
		t.Fatal("the decorator does not expose the edge-payload write at all, so a caller cannot even be refused")
	}
	if err := writer.DepAddWithMetadata("gcg-1", "gcg-2", "blocks", `{"gate":"waits_for"}`); err == nil {
		t.Fatal("the decorator reported success writing a payload its backing cannot hold; the edge would be written without it")
	}
}

// storageSpyStore is a backing that supports storage-tier creates and records
// the class it was asked for.
type storageSpyStore struct {
	beads.Store
	got beads.StorageClass
}

func (s *storageSpyStore) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	s.got = storage
	return s.Create(b)
}

// TestRouteClearCreatesWithStorageWithoutChangingWhatCallersObserve pins both
// halves of the decorator's storage-tier create. Over a backing that supports it
// the class must reach the backing, or a policy-selected ephemeral bead is
// silently created in the history tier. Over one that does not, the method's
// presence must not turn the fallback every caller was owed into an error: the
// class becomes the bead's flags, exactly as CachingStore.CreateWithStorage does
// for the same absence.
func TestRouteClearCreatesWithStorageWithoutChangingWhatCallersObserve(t *testing.T) {
	t.Run("a supporting backing receives the class", func(t *testing.T) {
		spy := &storageSpyStore{Store: beads.NewMemStore()}
		creator, ok := beads.WithRouteChangeClearing(spy, routeClearIdentity).(beads.StorageCreateStore)
		if !ok {
			t.Fatal("the decorator cannot create with a storage class")
		}
		if _, err := creator.CreateWithStorage(beads.Bead{Title: "wisp", Type: "task"}, beads.StorageEphemeral); err != nil {
			t.Fatalf("CreateWithStorage: %v", err)
		}
		if spy.got != beads.StorageEphemeral {
			t.Fatalf("the backing was asked for storage class %q, want %q: the tier the caller selected was dropped", spy.got, beads.StorageEphemeral)
		}
	})

	t.Run("a backing without one gets the cache's flag translation", func(t *testing.T) {
		mem := beads.NewMemStore()
		if _, ok := beads.Store(mem).(beads.StorageCreateStore); ok {
			t.Fatal("MemStore now supports storage classes, so it is no longer the backing this test needs")
		}
		creator, ok := beads.WithRouteChangeClearing(mem, routeClearIdentity).(beads.StorageCreateStore)
		if !ok {
			t.Fatal("the decorator cannot create with a storage class")
		}
		created, err := creator.CreateWithStorage(beads.Bead{Title: "wisp", Type: "task"}, beads.StorageEphemeral)
		if err != nil {
			t.Fatalf("CreateWithStorage over a backing without the capability = %v, want the flag-translating fallback", err)
		}
		got, err := mem.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !got.Ephemeral {
			t.Fatalf("the created bead is %+v, want the ephemeral flag the storage class asked for", got)
		}
	})
}

// TestRouteClearReportsAtomicTxLikeItsBacking pins a capability the cache and
// the emitter both DERIVE from their backing: over the decorator they read
// false for an engine that rolls a failed Tx back, and a caller that needs an
// all-or-nothing swap then refuses the store or takes its recoverable path.
func TestRouteClearReportsAtomicTxLikeItsBacking(t *testing.T) {
	engine := openBindingEngineForTest(t)
	if !beads.StoreSupportsAtomicTx(engine) {
		t.Fatal("the SQLite engine no longer reports atomic Tx, so this fixture proves nothing")
	}
	for name, backing := range map[string]beads.Store{
		"a sqlite engine": engine,
		"a mem store":     beads.NewMemStore(),
	} {
		guarded := beads.WithRouteChangeClearing(backing, routeClearIdentity)
		if got, want := beads.StoreSupportsAtomicTx(guarded), beads.StoreSupportsAtomicTx(backing); got != want {
			t.Errorf("over %s the decorator reports AtomicTx = %v, its backing %v", name, got, want)
		}
	}
}

// TestRouteClearOnlyAdvertisesACensusItsBackingCanAnswer is the decorator's
// half of the census contract. Carrying the method is WORSE than dropping it,
// so the decorator answers only through the handle: a backing with no census
// must not be discovered as one, and a SQLite backing must be reached so the
// one-shot CLI does not go back to scanning the whole binding.
func TestRouteClearOnlyAdvertisesACensusItsBackingCanAnswer(t *testing.T) {
	const graphPrefix = "gcg"

	t.Run("a backing with no census is not advertised as one", func(t *testing.T) {
		guarded := beads.WithRouteChangeClearing(beads.NewMemStore(), routeClearIdentity)
		if census, ok := beads.NamespaceCensusFor(guarded); ok {
			t.Fatalf("a decorator over a backing with no census was discovered as one (%T); the boot verdict would retire the binding's probe on an answer nobody computed", census)
		}
		if _, structural := guarded.(beads.NamespaceCensus); structural {
			t.Fatal("the decorator carries HasResidentOutside structurally, so a bare assertion advertises a census over a backing that has none")
		}
	})

	t.Run("a sqlite backing is reached and agrees with the scan", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			seed []string
			want bool
		}{
			{name: "binding holding a relic", seed: []string{"gcg-1", "ga-relic"}, want: true},
			{name: "clean binding", seed: []string{"gcg-1", "gcg-2"}, want: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				leaf, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix(graphPrefix))
				if err != nil {
					t.Fatalf("opening the leaf store: %v", err)
				}
				t.Cleanup(func() { _ = closeBeadStoreHandle(leaf) })
				for _, id := range tc.seed {
					if _, err := leaf.Create(beads.Bead{ID: id, Title: id, Type: "task"}); err != nil {
						t.Fatalf("seeding %q: %v", id, err)
					}
				}

				census, ok := beads.NamespaceCensusFor(beads.WithRouteChangeClearing(leaf, routeClearIdentity))
				if !ok {
					t.Fatal("the decorator hid the sqlite leaf's census; every one-shot command on this city goes back to scanning the whole binding")
				}
				predicate, err := census.HasResidentOutside([]string{graphPrefix})
				if err != nil {
					t.Fatalf("predicate: %v", err)
				}
				relics, err := storeref.LegacyResidents(leaf, []string{graphPrefix})
				if err != nil {
					t.Fatalf("scan: %v", err)
				}
				if scanned := len(relics) > 0; scanned != tc.want || predicate != scanned {
					t.Fatalf("census through the decorator = %v, scan = %v, want %v (relics %v)", predicate, scanned, tc.want, relics)
				}
			})
		}
	})
}

// readOnlyFenceStore is a backing that refuses mutations and says so.
type readOnlyFenceStore struct{ beads.Store }

func (readOnlyFenceStore) ReadOnly() bool { return true }

// TestRouteClearForwardsTheMutationFence pins the one direction this answer
// must never be wrong in: a wrapper around a handle that refuses every write
// must not report itself writable.
func TestRouteClearForwardsTheMutationFence(t *testing.T) {
	fenced, ok := beads.WithRouteChangeClearing(readOnlyFenceStore{Store: beads.NewMemStore()}, routeClearIdentity).(beads.ReadOnlyReporter)
	if !ok || !fenced.ReadOnly() {
		t.Fatalf("the decorator over a read-only handle reports (%v, carried %v), want a read-only answer", ok && fenced.ReadOnly(), ok)
	}
	open, ok := beads.WithRouteChangeClearing(beads.NewMemStore(), routeClearIdentity).(beads.ReadOnlyReporter)
	if !ok || open.ReadOnly() {
		t.Fatalf("the decorator over a store with no fence reports (%v, carried %v), want a writable answer", ok && open.ReadOnly(), ok)
	}
}

// TestControllerBindingCacheKeepsTheReadyProjectionThroughRouteClear is the
// behavioral half of the layering rule in class_store_cache.go. The cache finds
// the engine's ready projection by type-asserting its backing, and over the
// decorator it does so through the decorator's own forward; this holds that
// forward to the answer rather than to the method's existence. Kills: dropping
// enrichReadyProjectionForCache from the decorator, which offers work the
// engine holds back.
func TestControllerBindingCacheKeepsTheReadyProjectionThroughRouteClear(t *testing.T) {
	engine := openBindingEngineForTest(t)
	blocked, err := engine.Create(beads.Bead{Title: "blocked on a missing bead", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := engine.DepAdd(blocked.ID, "gcg-missing", "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	live, err := engine.Ready()
	if err != nil {
		t.Fatalf("engine Ready: %v", err)
	}
	if containsBeadID(live, blocked.ID) {
		t.Fatalf("the engine serves %s as ready; this fixture no longer blocks", blocked.ID)
	}

	guarded := beads.WithRouteChangeClearing(engine, routeClearIdentity)
	cs, _ := newCachedSplitControllerState(context.Background(), t, splitClassRoutes(guarded), beads.NewMemStore())
	cache := bindingCacheOf(t, cs)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cached, err := beads.HandlesFor(cache).Cached.Ready()
	if err != nil {
		t.Fatalf("cached Ready: %v", err)
	}
	if containsBeadID(cached, blocked.ID) {
		t.Fatalf("the binding cache over the decorator offers %s, which the engine holds back", blocked.ID)
	}
}

// TestBindingCensusSurvivesRouteClear pins the boot census lookup over a
// decorated binding. The census keys its verdict by the store it read, which is
// the decorator here, and the class accessors hand out the cache over that same
// decorator, so peeling the cache reaches the key. Kills: a lookup that also
// peels the decorator, which misses and answers the pessimistic "relics" for a
// clean binding, keeping every by-id probe alive forever.
func TestBindingCensusSurvivesRouteClear(t *testing.T) {
	engine := openBindingEngineForTest(t)
	if _, err := engine.Create(beads.Bead{Title: "minted inside the namespace", Type: "task"}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	guarded := beads.WithRouteChangeClearing(engine, routeClearIdentity)
	routes := splitClassRoutes(guarded)
	censusBindingRelics(routes)
	if verdict, censused := routes.relics[guarded]; !censused || verdict {
		t.Fatalf("boot census over a clean, decorated binding = (%v, censused %v), want a clean verdict", verdict, censused)
	}

	cs, _ := newCachedSplitControllerState(context.Background(), t, routes, beads.NewMemStore())
	graph := cs.GraphBeadStore().Store
	if _, cached := graph.(*beads.CachingStore); !cached {
		t.Fatalf("graph class resolves to %T, want the binding cache", graph)
	}
	if cs.ClassBindingHasLegacyResidents(graph) {
		t.Fatal("a clean decorated binding reads as holding relics once cached")
	}
}

// TestOpenStoreAtForCityForwardsReadyContextThroughRouteClear opens a store
// through the real production entrypoint named in ga-8q8z2w's exit criteria
// (openStoreAtForCity, the terminal factory for every CLI/standalone open)
// rather than a hand-composed wrapper stack, and confirms ReadyContext --
// one of the capabilities route_clear_store.go must forward -- still
// resolves through it end to end.
//
// It is narrower than the table above only because FileStore (the fast,
// hermetic test provider every other openStoreAtForCity test in this
// package uses) is the one capability-forwarding target in ga-8q8z2w's list
// that a raw FileStore genuinely implements the interface for; FileStore has
// no ApplyGraphPlan, DeleteBatch, Count or SawRows to prove the rest against,
// real or forwarded. FileStore's own ReadyContext deliberately vetoes every
// call (see filestore.go) rather than succeeding -- refreshing the on-disk
// JSON is context-blind, so a promoted MemStore.ReadyContext would falsely
// promise cancellation -- so "resolves... end to end" here means the veto
// itself surfaces through both wrapper layers as
// beads.ErrReadyContextUnsupported, not that the call succeeds.
func TestOpenStoreAtForCityForwardsReadyContextThroughRouteClear(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}

	reader, ok := store.(beads.ContextReadyReader)
	if !ok {
		t.Fatal("store.(beads.ContextReadyReader) ok = false, want true: openStoreAtForCity's composed store does not resolve ContextReadyReader")
	}
	// FileStore always declines (see filestore.go); the fix under test is
	// that its veto reaches the caller as beads.ErrReadyContextUnsupported
	// through both wrapper layers rather than the type assertion above
	// failing outright -- not that FileStore starts answering successfully.
	if _, err := reader.ReadyContext(context.Background()); !errors.Is(err, beads.ErrReadyContextUnsupported) {
		t.Fatalf("ReadyContext error = %v, want errors.Is(err, beads.ErrReadyContextUnsupported): FileStore deliberately vetoes ReadyContext", err)
	}
}

// payloadReader answers every edge with one carried payload, standing in for a
// backing that can read an edge's payload one edge at a time but not in batch.
type payloadReader struct {
	beads.Store
}

func (payloadReader) DepMetadata(string, string) (string, bool, error) {
	return `{"gate":"x"}`, true, nil
}

// The migration's payload read falls back to per-edge reads when the batch read
// is refused with ErrDepMetadataUnsupported, so the decorator over a backing with
// no batch read must refuse with that sentinel and not fail the read outright
// (ga-l69b9m, architect Amendment 2).
func TestRouteClearEdgePayloadBatchOverASilentBackingFallsBackToPerEdgeReads(t *testing.T) {
	batch, ok := beads.WithRouteChangeClearing(payloadReader{Store: beads.NewMemStore()}, routeClearIdentity).(beads.DepMetadataBatchReader)
	if !ok {
		t.Fatal("the decorator does not carry the batch edge-payload read")
	}
	edges := map[string][]beads.Dep{"a": {{IssueID: "a", DependsOnID: "b", Type: "blocks"}}}
	got, err := readInfraPayloads(batch, payloadReader{Store: beads.NewMemStore()}, edges, func(beads.Dep) bool { return true })
	if err != nil {
		t.Fatalf("readInfraPayloads over the decorator over a backing with no batch read: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("readInfraPayloads = %v, want the one edge's payload read per edge", got)
	}
}
