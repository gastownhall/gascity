package beads

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// countingStore is a store with an optional capability (Counter) that no
// typed handle promotes.
type countingStore struct {
	*MemStore
}

func (countingStore) Count(context.Context, ListQuery, ...string) (int, error) { return 7, nil }

// unhashableStore has a non-comparable dynamic type: == on two of them
// panics.
type unhashableStore struct {
	*MemStore
	tags []string
}

func TestAsSeesThroughEveryClassHandle(t *testing.T) {
	raw := countingStore{NewMemStore()}
	for name, h := range map[string]Store{
		"work":     WorkStore{raw},
		"graph":    GraphStore{raw},
		"sessions": SessionStore{raw},
		"mail":     MailStore{raw},
		"orders":   OrdersStore{raw},
		"nudges":   NudgesStore{raw},
		"nested":   GraphStore{WorkStore{raw}},
	} {
		if _, ok := h.(Counter); ok {
			t.Fatalf("%s: the handle itself is a Counter; the test no longer shows a hidden capability", name)
		}
		c, ok := As[Counter](h)
		if !ok {
			t.Fatalf("%s: As[Counter] through the handle = false", name)
		}
		if n, _ := c.Count(context.Background(), ListQuery{}); n != 7 {
			t.Fatalf("%s: As[Counter] reached a different store (count %d)", name, n)
		}
	}
}

func TestAsMissingCapability(t *testing.T) {
	if _, ok := As[Counter](GraphStore{NewMemStore()}); ok {
		t.Fatal("As[Counter] found a capability the store lacks")
	}
}

func TestAsIsNilSafe(t *testing.T) {
	if _, ok := As[Counter](nil); ok {
		t.Fatal("As on nil = true")
	}
	if _, ok := As[Counter](GraphStore{}); ok {
		t.Fatal("As on a zero handle = true")
	}
	if _, ok := As[ConditionalWriter](WorkStore{}); ok {
		t.Fatal("As on a zero handle for a provider-backed capability = true")
	}
	if s, ok := As[Store](WorkStore{}); ok {
		t.Fatalf("As[Store] on a zero handle = %v, true; an absent handle is no store", s)
	}
	if _, ok := As[Store](GraphStore{WorkStore{}}); ok {
		t.Fatal("As[Store] on a handle over a zero handle = true")
	}
}

// TestAsMatchesTheOutermostLayer: a capability the handle itself has is
// answered by the handle, so As[Store] keeps the class handle when no class
// guard is below it (TestAsThroughAClassGate has one).
func TestAsMatchesTheOutermostLayer(t *testing.T) {
	h := GraphStore{NewMemStore()}
	s, ok := As[Store](h)
	if !ok {
		t.Fatal("As[Store] = false")
	}
	if _, isGraph := s.(GraphStore); !isGraph {
		t.Fatalf("As[Store] = %T, want the GraphStore handle", s)
	}
}

// TestAsNeverPeelsTheCache: As walks class layers only. The cache's own
// capabilities are what a caller wants; its backing is not reachable.
func TestAsNeverPeelsTheCache(t *testing.T) {
	mem := NewMemStore()
	cache := NewCachingStoreForTest(mem, nil)
	h := GraphStore{cache}
	got, ok := As[*CachingStore](h)
	if !ok || got != cache {
		t.Fatalf("As[*CachingStore] = %v, %v; want the cache", got, ok)
	}
	if _, ok := As[*MemStore](h); ok {
		t.Fatal("As[*MemStore] peeled the cache")
	}
}

// TestAsHonorsProviders: for a capability with an XFor helper, As through a
// handle answers exactly what the helper answers on the handle's store.
func TestAsHonorsProviders(t *testing.T) {
	mem := NewMemStore()
	cache := NewCachingStoreForTest(mem, nil)
	// A cache over a store that strands nothing (bd/Dolt's case): the cache
	// has AbsentBlockedReader's methods, but its helper answers no.
	unstranding := NewCachingStoreForTest(struct{ Store }{NewMemStore()}, nil)
	if _, ok := Store(unstranding).(AbsentBlockedReader); !ok {
		t.Fatal("fixture: the cache is no AbsentBlockedReader by assertion; the test proves nothing")
	}
	if _, ok := ReadyBlockedByAbsentFor(unstranding); ok {
		t.Fatal("fixture: ReadyBlockedByAbsentFor answers yes for a cache over a store that strands nothing")
	}
	for name, raw := range map[string]Store{"mem": mem, "cache": cache, "cache over a store that strands nothing": unstranding} {
		h := SessionStore{raw}
		check := func(capability string, viaAs, viaFor bool) {
			t.Helper()
			if viaAs != viaFor {
				t.Errorf("%s %s: As = %v, XFor on the raw store = %v", name, capability, viaAs, viaFor)
			}
		}
		_, a := As[ConditionalWriter](h)
		_, f := ConditionalWriterFor(raw)
		check("ConditionalWriter", a, f)
		_, a = As[GuardedBatcher](h)
		_, f = GuardedBatcherFor(raw)
		check("GuardedBatcher", a, f)
		_, a = As[GraphApplyStore](h)
		_, f = GraphApplyFor(raw)
		check("GraphApplyStore", a, f)
		_, a = As[MetadataCASWriter](h)
		_, f = MetadataCASWriterFor(raw)
		check("MetadataCASWriter", a, f)
		_, a = As[AtomicConditionalCloser](h)
		_, f = AtomicConditionalCloserFor(raw)
		check("AtomicConditionalCloser", a, f)
		_, a = As[NamespaceCensus](h)
		_, f = NamespaceCensusFor(raw)
		check("NamespaceCensus", a, f)
		_, a = As[DependencyBatchLister](h)
		_, f = DepListBatchFor(raw)
		check("DependencyBatchLister", a, f)
		_, a = As[GraphOnlyReadyStore](h)
		_, f = GraphOnlyReadyFor(raw)
		check("GraphOnlyReadyStore", a, f)
		_, a = As[AssignmentGuardedUpdater](h)
		_, f = AssignmentGuardedUpdaterFor(raw)
		check("AssignmentGuardedUpdater", a, f)
		_, a = As[AbsentBlockedReader](h)
		_, f = ReadyBlockedByAbsentFor(raw)
		check("AbsentBlockedReader", a, f)
	}
	if _, ok := As[ConditionalWriter](GraphStore{mem}); !ok {
		t.Fatal("the fixture's MemStore is no ConditionalWriter: the test proves nothing")
	}
}

// TestAsRoutesEveryCapabilityHelper holds As to its doc for a helper that
// lands later: every exported func XFor(Store) (T, bool) of this package is a
// case *T of resolveCapability that calls it, and As's doc names it. Without
// the case, As[T] is a plain assertion, which answers yes where the helper
// gates the capability (ReadyBlockedByAbsentFor) or no where it follows a
// resolution target.
func TestAsRoutesEveryCapabilityHelper(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	helpers := map[string]string{} // helper -> the capability it returns
	routed := map[string]string{}  // helper -> the case of resolveCapability that calls it
	var asDoc string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			switch fn.Name.Name {
			case "As":
				asDoc = fn.Doc.Text()
			case "resolveCapability":
				resolveCapabilityRoutes(fn, routed)
			}
			if capability, ok := capabilityHelperOf(fn); ok {
				helpers[fn.Name.Name] = capability
			}
		}
	}
	if len(helpers) < 10 || len(routed) == 0 || asDoc == "" {
		t.Fatalf("parsed %d XFor helpers, %d resolveCapability cases and %d bytes of As doc; the guard would pass vacuously", len(helpers), len(routed), len(asDoc))
	}
	for helper, capability := range helpers {
		if got := routed[helper]; got != capability {
			t.Errorf("%s returns %s, but no case *%s of resolveCapability calls it (it is under %q): As[%s] is a plain assertion", helper, capability, capability, got, capability)
		}
		if !strings.Contains(asDoc, helper) {
			t.Errorf("As's doc does not list %s among the helpers it answers through", helper)
		}
	}
}

// capabilityHelperOf is the capability an exported func XFor(Store) (T, bool)
// returns.
func capabilityHelperOf(fn *ast.FuncDecl) (string, bool) {
	if !fn.Name.IsExported() || !strings.HasSuffix(fn.Name.Name, "For") {
		return "", false
	}
	params, results := fieldTypes(fn.Type.Params), fieldTypes(fn.Type.Results)
	if len(params) != 1 || len(results) != 2 || !identNamed(params[0], "Store") || !identNamed(results[1], "bool") {
		return "", false
	}
	capability, ok := results[0].(*ast.Ident)
	if !ok {
		return "", false
	}
	return capability.Name, true
}

// fieldTypes is fl's type once per name it declares: (a, b T) is two.
func fieldTypes(fl *ast.FieldList) []ast.Expr {
	if fl == nil {
		return nil
	}
	var out []ast.Expr
	for _, f := range fl.List {
		for range max(1, len(f.Names)) {
			out = append(out, f.Type)
		}
	}
	return out
}

func identNamed(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// resolveCapabilityRoutes records, for each case *T of resolveCapability,
// the helpers its body calls, as helper -> T.
func resolveCapabilityRoutes(fn *ast.FuncDecl, routed map[string]string) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		clause, ok := n.(*ast.CaseClause)
		if !ok || len(clause.List) != 1 {
			return true
		}
		star, ok := clause.List[0].(*ast.StarExpr)
		if !ok {
			return true
		}
		capability, ok := star.X.(*ast.Ident)
		if !ok {
			return true
		}
		for _, stmt := range clause.Body {
			ast.Inspect(stmt, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if helper, ok := call.Fun.(*ast.Ident); ok {
						routed[helper.Name] = capability.Name
					}
				}
				return true
			})
		}
		return true
	})
}

func TestRawReturnsTheWrappedStore(t *testing.T) {
	mem := NewMemStore()
	for name, got := range map[string]Store{
		"work": WorkStore{mem}.Raw(), "graph": GraphStore{mem}.Raw(), "sessions": SessionStore{mem}.Raw(),
		"mail": MailStore{mem}.Raw(), "orders": OrdersStore{mem}.Raw(), "nudges": NudgesStore{mem}.Raw(),
	} {
		if got != Store(mem) {
			t.Errorf("%s: Raw() = %v, want the wrapped store", name, got)
		}
	}
	if (GraphStore{}).Raw() != nil {
		t.Error("Raw() of a zero handle is not nil")
	}
}

func TestSameStore(t *testing.T) {
	a, b := NewMemStore(), NewMemStore()
	u := unhashableStore{MemStore: a, tags: []string{"x"}}
	for name, tc := range map[string]struct {
		x, y Store
		want bool
	}{
		"one store, two classes":       {GraphStore{a}, WorkStore{a}, true},
		"a handle and its store":       {a, SessionStore{a}, true},
		"nested handles":               {GraphStore{WorkStore{a}}, MailStore{a}, true},
		"two stores":                   {GraphStore{a}, GraphStore{b}, false},
		"zero handles":                 {GraphStore{}, WorkStore{}, false},
		"nil":                          {nil, a, false},
		"a non-comparable store":       {u, u, false},
		"a non-comparable under class": {GraphStore{u}, WorkStore{u}, false},
	} {
		if got := SameStore(tc.x, tc.y); got != tc.want {
			t.Errorf("%s: SameStore = %v, want %v", name, got, tc.want)
		}
	}
}

// testGate is a class-guard layer as T8 adds one: a class layer that vets
// class-write capabilities. It adapts ForeignIDCreator, and Store when it has
// a guarded one, and hides the rest.
type testGate struct {
	Store
	adapted ForeignIDCreator
	guarded Store
}

func (g testGate) unwrapClassLayer() Store { return g.Store }

func (g testGate) gateCapability(target any) bool {
	switch p := target.(type) {
	case *ForeignIDCreator:
		if g.adapted != nil {
			*p = g.adapted
			return true
		}
	case *Store:
		if g.guarded != nil {
			*p = g.guarded
			return true
		}
	}
	return false
}

type fakeForeignIDCreator struct{ name string }

func (fakeForeignIDCreator) CreateWithForeignID(Bead) (Bead, error) { return Bead{}, nil }

// rawForeignStore has class-write capabilities the gate must vet, so a
// capability that reaches the caller unvetted is visible.
type rawForeignStore struct {
	*MemStore
}

func (rawForeignStore) CreateWithForeignID(Bead) (Bead, error) { return Bead{}, nil }

func (rawForeignStore) UpdateReporting(string, UpdateOpts) (UpdatedRow, error) {
	return UpdatedRow{}, nil
}

// foreignIDShape has ForeignIDCreator's method set under another type
// identity.
type foreignIDShape interface {
	CreateWithForeignID(Bead) (Bead, error)
}

func TestAsThroughAClassGate(t *testing.T) {
	inner := rawForeignStore{NewMemStore()}
	if _, ok := Store(inner).(ForeignIDCreator); !ok {
		t.Fatal("fixture: the inner store is no ForeignIDCreator")
	}
	adapter := fakeForeignIDCreator{name: "guarded"}
	h := GraphStore{testGate{Store: inner, adapted: adapter}}

	got, ok := As[ForeignIDCreator](h)
	if !ok || got != ForeignIDCreator(adapter) {
		t.Fatalf("As[ForeignIDCreator] through a gate = %v, %v; want the gate's adapter", got, ok)
	}
	// A class-write capability the gate does not adapt is hidden, never the
	// unguarded inner one.
	if _, ok := As[ConditionalWriter](h); ok {
		t.Fatal("As[ConditionalWriter] leaked the unguarded writer through a gate")
	}
	declining := GraphStore{testGate{Store: inner}}
	if _, ok := As[ForeignIDCreator](declining); ok {
		t.Fatal("As[ForeignIDCreator] leaked the unguarded creator through a declining gate")
	}
	// The gate fails closed: a capability it was never told about is its to
	// answer too, so the store below it never answers one.
	if _, ok := Store(inner).(foreignIDShape); !ok {
		t.Fatal("fixture: the inner store does not have ForeignIDCreator's method set")
	}
	if _, ok := As[foreignIDShape](h); ok {
		t.Fatal("As leaked the unguarded creator through a gate under another interface's identity")
	}
	if _, ok := Store(inner).(UpdateReporter); !ok {
		t.Fatal("fixture: the inner store is no UpdateReporter")
	}
	if _, ok := As[UpdateReporter](h); ok {
		t.Fatal("As[UpdateReporter] leaked the unguarded updater through a gate")
	}
	// The handle above the gate is itself a Store and a
	// GuardedBatcherHandleProvider, but the gate answers both: the handle
	// never does.
	if _, ok := Store(h).(GuardedBatcherHandleProvider); !ok {
		t.Fatal("fixture: the handle is no GuardedBatcherHandleProvider")
	}
	if s, ok := As[Store](h); ok {
		t.Fatalf("As[Store] through a gate that declines it = %T, true; want nothing", s)
	}
	if p, ok := As[GuardedBatcherHandleProvider](h); ok {
		t.Fatalf("As[GuardedBatcherHandleProvider] through a gate = %T, true; want nothing", p)
	}
	guarded := NewMemStore()
	for name, over := range map[string]Store{
		"a handle over the gate":     GraphStore{testGate{Store: inner, guarded: guarded}},
		"two handles over the gate":  WorkStore{GraphStore{testGate{Store: inner, guarded: guarded}}},
		"the gate over a handle":     testGate{Store: GraphStore{inner}, guarded: guarded},
		"a handle, a gate, a handle": MailStore{testGate{Store: GraphStore{inner}, guarded: guarded}},
	} {
		if s, ok := As[Store](over); !ok || s != Store(guarded) {
			t.Errorf("%s: As[Store] = %T, %v; want the gate's guarded store", name, s, ok)
		}
	}
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	if _, ok := As[*CachingStore](GraphStore{cache}); !ok {
		t.Fatal("fixture: As[*CachingStore] does not find the cache without a gate")
	}
	if _, ok := As[*CachingStore](GraphStore{testGate{Store: cache}}); ok {
		t.Fatal("As[*CachingStore] leaked the raw cache through a gate")
	}
	// A capability that cannot change a class passes through.
	counted := GraphStore{testGate{Store: countingStore{NewMemStore()}}}
	if _, ok := As[Counter](counted); !ok {
		t.Fatal("As[Counter] did not pass through the gate")
	}
	// The gate is a class layer: identity sees through it.
	if !SameStore(h, WorkStore{inner}) {
		t.Fatal("SameStore did not see through the gate")
	}
}

// TestClassGateAllowlist: the class-neutral capabilities are an allowlist,
// and a gate vets every other T: the class writers, the whole store, a
// concrete store, a full updater, a provider, and an unnamed interface.
func TestClassGateAllowlist(t *testing.T) {
	for name, got := range map[string]bool{
		"Counter":                       passesClassGate[Counter](),
		"NamespaceCensus":               passesClassGate[NamespaceCensus](),
		"DependencyBatchLister":         passesClassGate[DependencyBatchLister](),
		"DepMetadataReader":             passesClassGate[DepMetadataReader](),
		"GraphOnlyReadyStore":           passesClassGate[GraphOnlyReadyStore](),
		"ContextReadyReader":            passesClassGate[ContextReadyReader](),
		"OrderedReadyReader":            passesClassGate[OrderedReadyReader](),
		"CachePreferredReadyReader":     passesClassGate[CachePreferredReadyReader](),
		"ChildrenBatchLister":           passesClassGate[ChildrenBatchLister](),
		"ExactBatchGetter":              passesClassGate[ExactBatchGetter](),
		"CachedReadExact":               passesClassGate[CachedReadExact](),
		"ReadOnlyReporter":              passesClassGate[ReadOnlyReporter](),
		"RowWitness":                    passesClassGate[RowWitness](),
		"ParentProjectionWaiter":        passesClassGate[ParentProjectionWaiter](),
		"ConditionalAssignmentReleaser": passesClassGate[ConditionalAssignmentReleaser](),
		"ConditionalAssigneeTransferer": passesClassGate[ConditionalAssigneeTransferer](),
		"AbsentBlockedReader":           passesClassGate[AbsentBlockedReader](),
	} {
		if !got {
			t.Errorf("%s is not class-neutral: a gate hides it", name)
		}
	}
	for name, got := range map[string]bool{
		"StorageCreateStore":              passesClassGate[StorageCreateStore](),
		"ForeignIDCreator":                passesClassGate[ForeignIDCreator](),
		"StorageGraphApplyStore":          passesClassGate[StorageGraphApplyStore](),
		"EphemeralGraphApplyStore":        passesClassGate[EphemeralGraphApplyStore](),
		"AssignmentGuardedUpdater":        passesClassGate[AssignmentGuardedUpdater](),
		"ConditionalWriter":               passesClassGate[ConditionalWriter](),
		"GuardedBatcher":                  passesClassGate[GuardedBatcher](),
		"GraphApplyStore":                 passesClassGate[GraphApplyStore](),
		"MetadataCASWriter":               passesClassGate[MetadataCASWriter](),
		"AtomicConditionalCloser":         passesClassGate[AtomicConditionalCloser](),
		"Store":                           passesClassGate[Store](),
		"*CachingStore":                   passesClassGate[*CachingStore](),
		"UpdateReporter":                  passesClassGate[UpdateReporter](),
		"ConditionalWriterHandleProvider": passesClassGate[ConditionalWriterHandleProvider](),
		"foreignIDShape":                  passesClassGate[foreignIDShape](),
	} {
		if got {
			t.Errorf("%s passes a class gate unvetted", name)
		}
	}
}

// TestXForThroughAHandleToday pins today's behavior, which T1 does not
// change: the XFor helpers that do not follow a declared resolution target
// answer "no" on a typed handle even when its store has the capability, and
// HandlesFor falls back to the logical wrappers. Provider forwarding on the
// handles (design §3.3.1-2) changes these answers, so it lands with its own
// behavior tests; until then As is the way through a handle.
func TestXForThroughAHandleToday(t *testing.T) {
	mem := NewMemStore()
	h := GraphStore{mem}
	if _, ok := ConditionalWriterFor(mem); !ok {
		t.Fatal("fixture: MemStore is no ConditionalWriter")
	}
	if _, ok := ConditionalWriterFor(h); ok {
		t.Error("ConditionalWriterFor now sees through a handle: provider forwarding landed; update this pin and its callers' tests")
	}
	if _, ok := As[ConditionalWriter](h); !ok {
		t.Error("As[ConditionalWriter] does not see through a handle")
	}
	if _, ok := GuardedBatcherFor(h); !ok {
		t.Error("GuardedBatcherFor no longer forwards through a handle")
	}
	if _, ok := MetadataCASWriterFor(h); !ok {
		t.Error("MetadataCASWriterFor no longer follows the handle's resolution target")
	}
	cache := NewCachingStoreForTest(mem, nil)
	if _, logical := HandlesFor(GraphStore{cache}).Cached.(logicalCachedStoreReader); !logical {
		t.Error("HandlesFor now sees through a handle to the cache: provider forwarding landed; update this pin")
	}
}
