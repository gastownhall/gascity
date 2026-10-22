package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// rigNamespaceStoreReferenceTypes are the stores a guard can sit over or
// stand in for: every method one of them declares is a capability some
// caller discovers by type assertion.
var rigNamespaceStoreReferenceTypes = []reflect.Type{
	reflect.TypeOf(&beadPolicyStore{}),
	reflect.TypeOf(&beadPolicyGraphStore{}),
	reflect.TypeOf(&beads.CachingStore{}),
	reflect.TypeOf(&beads.BdStore{}),
	reflect.TypeOf(&beads.NativeDoltStore{}),
	reflect.TypeOf(&beads.SQLiteStore{}),
	reflect.TypeOf(&beads.MemStore{}),
	reflect.TypeOf(&beads.FileStore{}),
	reflect.TypeOf(&beads.ProxiedStore{}),
}

// rigNamespaceStoreNotCarried are the reference methods the guard
// deliberately does not carry, and why. Every other reference method must be
// carried with the same signature.
var rigNamespaceStoreNotCarried = map[string]string{
	// Must not carry: each would let a caller step around the guard.
	"ConditionalWritesResolveTarget": "a resolve target hands the inner store out as the writer; the guard declares a mode source instead",
	"Backing":                        "hands out the store under the guard",
	"UpdateIfMatch":                  "plain ConditionalWriter methods would make ConditionalWriterFor advertise the capability over any inner store; carried through ConditionalWriterHandle",
	"CloseIfMatch":                   "see UpdateIfMatch",
	"DeleteIfMatch":                  "see UpdateIfMatch",
	"CompareAndSetMetadataKey":       "see UpdateIfMatch; carried through MetadataCASWriterHandle",
	"CachedReadExact":                "the guard's cached reads are filtered; exactness is the cache's, judged under the guard (demandLegCache)",
	"CloseWithMetadataIfMatch":       "carried through AtomicConditionalCloserHandle, so it is advertised only when the inner store can honor it",
	"ApplyGuarded":                   "carried through GuardedBatcherHandle",
	"ApplyGraphPlan":                 "carried through GraphApplyHandle",
	"ApplyGraphPlanWithStorage":      "carried through GraphApplyHandle's applier",
	"ImportVerbatim":                 "copies rows verbatim, gc.owner_namespace included: an import into a guarded store goes through the creation stamp (Create), never a verbatim copy",
	// Cache internals: row-level access to the unfiltered cache, or
	// lifecycle owned by the controller through closeBeadStoreHandle.
	"ApplyEvent":                 "cache internal: feeds rows into the unfiltered cache",
	"ApplyEventSnapshot":         "cache internal",
	"ApplyResidentEventSnapshot": "cache internal",
	"CachedReady":                "cache internal: unfiltered ready rows",
	"ObservedList":               "cache internal: unfiltered rows",
	"OwnsBeadID":                 "cache internal",
	"Prime":                      "cache internal",
	"PrimeActive":                "cache internal",
	"RefreshRow":                 "cache internal: unfiltered row refresh",
	"RefreshRowReport":           "cache internal",
	"RequestReconcile":           "cache internal",
	"StartReconciler":            "cache internal",
	"StopReconciler":             "cache internal; CloseStore stops it",
	"Stopped":                    "cache internal",
	"Stats":                      "cache internal",
	"IsLive":                     "cache internal",
	"WriteRev":                   "cache internal",
	"WithCurrentObservation":     "cache internal",
	"ReconcileIfDueForTest":      "test hook",
	"ReconcileNowForTest":        "test hook",
	"SetPrimeRetryDelayForTest":  "test hook",
	// bd administration: unfenced writes or store setup a guarded rig
	// never routes through gc's store handles. A caller that asserts them
	// falls back to the fenced Store methods.
	"BDVersion":             "bd administration",
	"CloseAllWithReason":    "unfenced bd batch write; callers fall back to CloseAll",
	"CloseWithReason":       "unfenced bd write; callers fall back to Close",
	"ConfigGet":             "bd administration",
	"ConfigSet":             "bd administration",
	"Dir":                   "bd administration",
	"Init":                  "bd administration",
	"ListSkipLabelsEnabled": "bd administration",
	"Purge":                 "unfenced bd purge",
	"ReclaimStale":          "unfenced bd write",
	"RecomputeBlocked":      "derived state, the same whichever city runs it; not reached through a rig store handle",
	"SetPurgeRunner":        "bd administration",
	"UpdateAll":             "unfenced bd batch write; callers fall back to Update",
	// Engine plumbing a rig store handle never needs.
	"SetLocker":             "file store plumbing",
	"SetMintPrefixFunc":     "engine plumbing: id minting is the engine's",
	"SequenceFloor":         "binding sequence plumbing (relocated class stores)",
	"SetSequenceFloor":      "binding sequence plumbing",
	"AdvanceSequenceFloor":  "binding sequence plumbing",
	"StoreHealthPath":       "binding health plumbing",
	"EventsJournalActive":   "journal tail of the whole store; the cache's feed reads the engine and the event filter drops foreign rows",
	"ReadEventsJournalPage": "journal tail of the whole store, unfiltered",
	"BdLeaf":                "proxied split internals",
	"Demoted":               "proxied split internals; reachable through ProxiedStore for diagnostics",
	"Pin":                   "proxied split internals",
	"Report":                "proxied split internals",
	"StartGuard":            "proxied split internals",
	"Verdict":               "proxied split internals",
}

// rigNamespaceStoreMustNotCarry are the methods whose presence on the guard
// would bypass it.
var rigNamespaceStoreMustNotCarry = []string{
	"ConditionalWritesResolveTarget", "Backing", "UpdateIfMatch", "CloseIfMatch",
	"DeleteIfMatch", "CompareAndSetMetadataKey", "CachedReadExact", "ImportVerbatim",
}

func TestRigNamespaceStoreCapabilityParity(t *testing.T) {
	guard := reflect.TypeOf(&rigNamespaceStore{})
	var missing, unclassified []string
	seen := map[string]bool{}
	for _, ref := range rigNamespaceStoreReferenceTypes {
		for i := 0; i < ref.NumMethod(); i++ {
			m := ref.Method(i)
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			if _, skip := rigNamespaceStoreNotCarried[m.Name]; skip {
				continue
			}
			got, ok := guard.MethodByName(m.Name)
			if !ok {
				unclassified = append(unclassified, m.Name+" (from "+ref.String()+")")
				continue
			}
			if got.Type.String() != strings.Replace(m.Type.String(), ref.String(), guard.String(), 1) {
				missing = append(missing, m.Name+" (signature differs from "+ref.String()+")")
			}
		}
	}
	sort.Strings(missing)
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("reference methods the guard neither carries nor classifies: %v; carry each (fenced or filtered) or add it to rigNamespaceStoreNotCarried with the reason", unclassified)
	}
	if len(missing) > 0 {
		t.Errorf("guard methods whose signatures differ: %v", missing)
	}
	for _, name := range rigNamespaceStoreMustNotCarry {
		if _, ok := guard.MethodByName(name); ok {
			t.Errorf("the guard carries %s, which would hand a caller the inner store's unfenced capability", name)
		}
	}
	for name := range rigNamespaceStoreNotCarried {
		if !seen[name] {
			t.Errorf("rigNamespaceStoreNotCarried names %s, which no reference store declares any more; drop it", name)
		}
	}
}

// rigNamespaceStoreInterfaces classifies every exported interface of
// internal/beads: whether the guard implements it.
var rigNamespaceStoreInterfaces = map[string]struct {
	typ  reflect.Type
	want bool
}{
	"AbsentBlockedReader":                   {reflect.TypeOf((*beads.AbsentBlockedReader)(nil)).Elem(), true},
	"AssignmentGuardedUpdater":              {reflect.TypeOf((*beads.AssignmentGuardedUpdater)(nil)).Elem(), true},
	"AtomicConditionalCloser":               {reflect.TypeOf((*beads.AtomicConditionalCloser)(nil)).Elem(), false},
	"AtomicConditionalCloserHandleProvider": {reflect.TypeOf((*beads.AtomicConditionalCloserHandleProvider)(nil)).Elem(), true},
	"AtomicTxStore":                         {reflect.TypeOf((*beads.AtomicTxStore)(nil)).Elem(), true},
	"BatchDeleter":                          {reflect.TypeOf((*beads.BatchDeleter)(nil)).Elem(), true},
	"CachedReader":                          {reflect.TypeOf((*beads.CachedReader)(nil)).Elem(), true},
	"CachedReadExact":                       {reflect.TypeOf((*beads.CachedReadExact)(nil)).Elem(), false},
	"CachePreferredReadyReader":             {reflect.TypeOf((*beads.CachePreferredReadyReader)(nil)).Elem(), true},
	"ChildrenBatchLister":                   {reflect.TypeOf((*beads.ChildrenBatchLister)(nil)).Elem(), true},
	"ConditionalAssigneeTransferer":         {reflect.TypeOf((*beads.ConditionalAssigneeTransferer)(nil)).Elem(), true},
	"ConditionalAssignmentReleaser":         {reflect.TypeOf((*beads.ConditionalAssignmentReleaser)(nil)).Elem(), true},
	"ConditionalWriter":                     {reflect.TypeOf((*beads.ConditionalWriter)(nil)).Elem(), false},
	"ConditionalWriterHandleProvider":       {reflect.TypeOf((*beads.ConditionalWriterHandleProvider)(nil)).Elem(), true},
	"ConditionalWritesModeSourcer":          {reflect.TypeOf((*beads.ConditionalWritesModeSourcer)(nil)).Elem(), true},
	"ConditionalWritesResolveTargeter":      {reflect.TypeOf((*beads.ConditionalWritesResolveTargeter)(nil)).Elem(), false},
	"ContextReadyReader":                    {reflect.TypeOf((*beads.ContextReadyReader)(nil)).Elem(), true},
	"Counter":                               {reflect.TypeOf((*beads.Counter)(nil)).Elem(), true},
	"DependencyBatchLister":                 {reflect.TypeOf((*beads.DependencyBatchLister)(nil)).Elem(), true},
	"DepMetadataAbsentByProtocol":           {reflect.TypeOf((*beads.DepMetadataAbsentByProtocol)(nil)).Elem(), true},
	"DepMetadataReader":                     {reflect.TypeOf((*beads.DepMetadataReader)(nil)).Elem(), true},
	"DepMetadataWriter":                     {reflect.TypeOf((*beads.DepMetadataWriter)(nil)).Elem(), true},
	"EphemeralGraphApplyStore":              {reflect.TypeOf((*beads.EphemeralGraphApplyStore)(nil)).Elem(), true},
	"ExactBatchGetter":                      {reflect.TypeOf((*beads.ExactBatchGetter)(nil)).Elem(), true},
	"ForeignIDCreator":                      {reflect.TypeOf((*beads.ForeignIDCreator)(nil)).Elem(), true},
	"GraphApplyHandleProvider":              {reflect.TypeOf((*beads.GraphApplyHandleProvider)(nil)).Elem(), true},
	"GraphApplyStore":                       {reflect.TypeOf((*beads.GraphApplyStore)(nil)).Elem(), false},
	"GraphOnlyReadyProvider":                {reflect.TypeOf((*beads.GraphOnlyReadyProvider)(nil)).Elem(), true},
	"GraphOnlyReadyStore":                   {reflect.TypeOf((*beads.GraphOnlyReadyStore)(nil)).Elem(), false},
	"GuardedBatcher":                        {reflect.TypeOf((*beads.GuardedBatcher)(nil)).Elem(), false},
	"GuardedBatcherHandleProvider":          {reflect.TypeOf((*beads.GuardedBatcherHandleProvider)(nil)).Elem(), true},
	"LiveReader":                            {reflect.TypeOf((*beads.LiveReader)(nil)).Elem(), true},
	"Locker":                                {reflect.TypeOf((*beads.Locker)(nil)).Elem(), false},
	"MetadataCASWriter":                     {reflect.TypeOf((*beads.MetadataCASWriter)(nil)).Elem(), false},
	"MetadataCASWriterHandleProvider":       {reflect.TypeOf((*beads.MetadataCASWriterHandleProvider)(nil)).Elem(), true},
	"NamespaceCensus":                       {reflect.TypeOf((*beads.NamespaceCensus)(nil)).Elem(), true},
	"NamespaceCensusHandleProvider":         {reflect.TypeOf((*beads.NamespaceCensusHandleProvider)(nil)).Elem(), true},
	"OrderedReadyReader":                    {reflect.TypeOf((*beads.OrderedReadyReader)(nil)).Elem(), true},
	"ParentProjectionWaiter":                {reflect.TypeOf((*beads.ParentProjectionWaiter)(nil)).Elem(), true},
	"ProviderOps":                           {reflect.TypeOf((*beads.ProviderOps)(nil)).Elem(), false},
	"ProxiedStoreCarrier":                   {reflect.TypeOf((*beads.ProxiedStoreCarrier)(nil)).Elem(), true},
	"ProxiedStoreView":                      {reflect.TypeOf((*beads.ProxiedStoreView)(nil)).Elem(), false},
	"ReadOnlyReporter":                      {reflect.TypeOf((*beads.ReadOnlyReporter)(nil)).Elem(), true},
	"RowWitness":                            {reflect.TypeOf((*beads.RowWitness)(nil)).Elem(), true},
	"StorageCreateStore":                    {reflect.TypeOf((*beads.StorageCreateStore)(nil)).Elem(), true},
	"StorageGraphApplyStore":                {reflect.TypeOf((*beads.StorageGraphApplyStore)(nil)).Elem(), false},
	"Store":                                 {reflect.TypeOf((*beads.Store)(nil)).Elem(), true},
	"Tx":                                    {reflect.TypeOf((*beads.Tx)(nil)).Elem(), true},
	"UpdateReporter":                        {reflect.TypeOf((*beads.UpdateReporter)(nil)).Elem(), true},
	"VerbatimImporter":                      {reflect.TypeOf((*beads.VerbatimImporter)(nil)).Elem(), false},
	"Writer":                                {reflect.TypeOf((*beads.Writer)(nil)).Elem(), true},
}

// rigNamespaceDiscoveryHelpers are the internal/beads helpers that discover a
// capability on a store. TestRigNamespaceStoreDiscoveryParity checks each.
var rigNamespaceDiscoveryHelpers = map[string]bool{
	"AssignmentGuardedUpdaterFor": true,
	"AtomicConditionalCloserFor":  true,
	"ConditionalWriterFor":        true,
	"HandlesFor":                  true,
	"GraphApplyFor":               true,
	"DepListBatchFor":             true,
	"GuardedBatcherFor":           true,
	"NamespaceCensusFor":          true,
	"MetadataCASWriterFor":        true,
	"GraphOnlyReadyFor":           true,
	"ReadyBlockedByAbsentFor":     true,
	// LiveFor and IDPrefixFor are not capability discovery: they read the
	// store's live handle and its id prefix through any class handle.
	// TestRigNamespaceStoreLiveForAndIDPrefixForSeeTheGuard checks them.
	"LiveFor":     true,
	"IDPrefixFor": true,
}

// beadsExportedAPI parses internal/beads and returns its exported interface
// names and its exported "XFor(store Store)" helpers.
func beadsExportedAPI(t *testing.T) (interfaces, helpers []string) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "internal", "beads")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if f.Name.Name != "beads" {
			continue
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || !ts.Name.IsExported() {
						continue
					}
					if _, ok := ts.Type.(*ast.InterfaceType); ok {
						interfaces = append(interfaces, ts.Name.Name)
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil || !d.Name.IsExported() || !strings.HasSuffix(d.Name.Name, "For") {
					continue
				}
				params := d.Type.Params.List
				if len(params) == 1 && len(params[0].Names) <= 1 {
					if id, ok := params[0].Type.(*ast.Ident); ok && id.Name == "Store" {
						helpers = append(helpers, d.Name.Name)
					}
				}
			}
		}
	}
	return interfaces, helpers
}

func TestRigNamespaceStoreClassifiesEveryBeadsInterfaceAndHelper(t *testing.T) {
	interfaces, helpers := beadsExportedAPI(t)
	if len(interfaces) < 40 || len(helpers) < len(rigNamespaceDiscoveryHelpers) {
		t.Fatalf("parsed %d interfaces and %d discovery helpers from internal/beads; the scan did not see the package", len(interfaces), len(helpers))
	}
	guard := reflect.TypeOf(&rigNamespaceStore{})
	for _, name := range interfaces {
		c, ok := rigNamespaceStoreInterfaces[name]
		if !ok {
			t.Errorf("internal/beads exports interface %s, which rigNamespaceStoreInterfaces does not classify: decide whether the guard carries it (fenced, filtered or through a handle)", name)
			continue
		}
		if got := guard.Implements(c.typ); got != c.want {
			t.Errorf("guard implements %s = %v, want %v", name, got, c.want)
		}
	}
	for _, name := range helpers {
		if !rigNamespaceDiscoveryHelpers[name] {
			t.Errorf("internal/beads exports discovery helper %s, which TestRigNamespaceStoreDiscoveryParity does not check", name)
		}
	}
}

// TestRigNamespaceStoreDiscoveryParity checks that every discovery helper
// finds the same capabilities through policy(guard(cache(engine))) as
// through policy(cache(engine)), and that what it finds is the guard's.
func TestRigNamespaceStoreDiscoveryParity(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	leaf, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("bt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(leaf) })
	cache := beads.NewCachingStore(leaf, nil)
	plain := wrapStoreWithBeadPolicies(cache, nil)
	guarded := wrapStoreWithBeadPolicies(newRigNamespaceStore(cache, ns), nil)

	isGuards := func(v any) bool {
		switch v.(type) {
		case *rigNamespaceStore, rigNamespaceConditionalWriter, rigNamespaceMetadataCAS, rigNamespaceAtomicCloser,
			rigNamespaceBatcher, rigNamespaceGraphApplier, rigNamespaceGraphOnlyReady:
			return true
		}
		return false
	}
	type probe struct {
		name     string
		find     func(beads.Store) (any, bool)
		innerOK  bool // a found capability must be the guard's
		rowsFree bool // the capability carries no rows (a census, a dep batch): the inner's is fine
	}
	probes := []probe{
		{"ConditionalWriterFor", func(s beads.Store) (any, bool) { return beads.ConditionalWriterForTarget(s) }, true, false},
		{"MetadataCASWriterFor", func(s beads.Store) (any, bool) { return beads.MetadataCASWriterFor(s) }, true, false},
		{"AtomicConditionalCloserFor", func(s beads.Store) (any, bool) { return beads.AtomicConditionalCloserFor(s) }, true, false},
		{"AssignmentGuardedUpdaterFor", func(s beads.Store) (any, bool) { return beads.AssignmentGuardedUpdaterFor(s) }, true, false},
		{"GuardedBatcherFor", func(s beads.Store) (any, bool) {
			base, _, _ := unwrapBeadPolicyStore(s)
			return beads.GuardedBatcherFor(base)
		}, true, false},
		{"GraphApplyFor", func(s beads.Store) (any, bool) {
			base, _, _ := unwrapBeadPolicyStore(s)
			return beads.GraphApplyFor(base)
		}, true, false},
		{"NamespaceCensusFor", func(s beads.Store) (any, bool) {
			base, _, _ := unwrapBeadPolicyStore(s)
			return beads.NamespaceCensusFor(base)
		}, false, true},
		{"DepListBatchFor", func(s beads.Store) (any, bool) { return beads.DepListBatchFor(s) }, false, true},
		{"ReadyBlockedByAbsentFor", func(s beads.Store) (any, bool) {
			base, _, _ := unwrapBeadPolicyStore(s)
			return beads.ReadyBlockedByAbsentFor(base)
		}, true, false},
		{"HandlesFor", func(s beads.Store) (any, bool) {
			base, _, _ := unwrapBeadPolicyStore(s)
			return beads.HandlesFor(base).Writer, true
		}, true, false},
	}
	for _, p := range probes {
		_, wantOK := p.find(plain)
		got, ok := p.find(guarded)
		if ok != wantOK {
			t.Errorf("%s: found = %v through the guard, %v without it", p.name, ok, wantOK)
			continue
		}
		if ok && p.innerOK && !isGuards(got) {
			t.Errorf("%s through the guard returned %T, the inner store's capability: its writes bypass the guard", p.name, got)
		}
	}
}

// TestRigNamespaceStoreLiveForAndIDPrefixForSeeTheGuard: through a class
// handle over policy(guard(store)), beads.LiveFor reads the guard's live
// handle, so a listing drops another city's row, and beads.IDPrefixFor
// finds the inner store's prefix, as it does without the guard.
func TestRigNamespaceStoreLiveForAndIDPrefixForSeeTheGuard(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	leaf, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("bt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(leaf) })
	own, err := leaf.Create(beads.Bead{Title: "own", Type: "task", Assignee: "beta/crew.worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Create(beads.Bead{Title: "foreign", Type: "task", Assignee: "alpha/crew.worker"}); err != nil {
		t.Fatal(err)
	}
	plain := beadstest.Work(wrapStoreWithBeadPolicies(leaf, nil))
	guarded := beadstest.Work(wrapStoreWithBeadPolicies(newRigNamespaceStore(leaf, ns), nil))

	rows, err := beads.LiveFor(guarded).List(beads.ListQuery{AllowScan: true})
	if ids := rgsIDs(rows); err != nil || len(ids) != 1 || ids[0] != own.ID {
		t.Fatalf("LiveFor through the guard listed %v, %v; want only %s", ids, err, own.ID)
	}
	if rows, err := beads.LiveFor(plain).List(beads.ListQuery{AllowScan: true}); err != nil || len(rows) != 2 {
		t.Fatalf("fixture: LiveFor without the guard listed %v, %v; want both rows", rgsIDs(rows), err)
	}
	if got, want := beads.IDPrefixFor(guarded), beads.IDPrefixFor(plain); got != want || got != "bt" {
		t.Fatalf("IDPrefixFor through the guard = %q, without it %q; want bt for both", got, want)
	}
}

// parityGraphOnlyMem is a mem store with a graph-only ready read.
type parityGraphOnlyMem struct{ *beads.MemStore }

func (s parityGraphOnlyMem) ReadyGraphOnly(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return s.Ready(query...)
}

// TestRigNamespaceStoreDiscoversGraphOnlyReadyGuarded is the graph-only
// probe of TestRigNamespaceStoreDiscoveryParity over a store that has the
// capability (the SQLite and cache stores do not).
func TestRigNamespaceStoreDiscoversGraphOnlyReadyGuarded(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := parityGraphOnlyMem{beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}
	if _, ok := beads.GraphOnlyReadyFor(inner); !ok {
		t.Fatal("precondition: the inner store has no graph-only ready read")
	}
	got, ok := beads.GraphOnlyReadyFor(newRigNamespaceStore(inner, ns))
	if !ok {
		t.Fatal("the guard hid the graph-only ready read")
	}
	if _, guarded := got.(rigNamespaceGraphOnlyReady); !guarded {
		t.Fatalf("GraphOnlyReadyFor through the guard returned %T, the inner store's unfiltered read", got)
	}
	if _, ok := beads.GraphOnlyReadyFor(newRigNamespaceStore(beads.NewMemStore(), ns)); ok {
		t.Fatal("the guard advertised a graph-only ready read its inner store lacks")
	}
}

// TestRigNamespaceStoreResolvesConditionalWritesUnderRequire pins the mode
// source: under a require deployment the resolver reads the stamp from the
// store under the guard and returns the guard's writer, never the inner one
// and never a refusal.
func TestRigNamespaceStoreResolvesConditionalWritesUnderRequire(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	for _, overCache := range []bool{false, true} {
		result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
			ScopeRoot:         t.TempDir(),
			Provider:          "file",
			ConditionalWrites: gate.Require,
			OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		inner := result.Store
		if overCache {
			inner = beads.NewCachingStore(inner, nil)
		}
		stack := wrapStoreWithBeadPolicies(newRigNamespaceStore(inner, ns), nil)
		writer, diag, err := beads.ResolveConditionalWriter(stack)
		if err != nil || diag != nil {
			t.Fatalf("overCache=%v: resolve = diag %v err %v; the guard must source the inner store's require stamp", overCache, diag, err)
		}
		if _, ok := writer.(rigNamespaceConditionalWriter); !ok {
			t.Fatalf("overCache=%v: resolved writer %T, want the guard's", overCache, writer)
		}
		created, err := stack.Create(beads.Bead{Title: "x", Type: "task", Assignee: "alice"})
		if err == nil {
			t.Fatalf("overCache=%v: created %s for a foreign assignee", overCache, created.ID)
		}
	}
}

// payloadBlindStore declares, as the generic exec store does, that its
// protocol carries no dependency payload, and has no edge-payload read.
type payloadBlindStore struct{ beads.Store }

func (payloadBlindStore) DepMetadataAbsentByProtocol() (string, bool) { return "exec", true }

// TestRigNamespaceStoreForwardsThePayloadDeclaration pins the two answers the
// migration reads through the guard: the inner store's no-payload
// declaration, and ErrDepMetadataUnsupported for an inner store that cannot
// be asked, both as the bead-policy wrapper gives them.
func TestRigNamespaceStoreForwardsThePayloadDeclaration(t *testing.T) {
	guard := newRigNamespaceStore(payloadBlindStore{beads.NewMemStore()}, nil)
	if protocol, absent := beads.DepMetadataAbsent(guard); protocol != "exec" || !absent {
		t.Fatalf("DepMetadataAbsent(guard) = (%q, %v), want (\"exec\", true)", protocol, absent)
	}
	if protocol, absent := beads.DepMetadataAbsent(newRigNamespaceStore(beads.NewMemStore(), nil)); protocol != "" || absent {
		t.Fatalf("DepMetadataAbsent(guard over a store that declares nothing) = (%q, %v), want (\"\", false)", protocol, absent)
	}
	if _, _, err := guard.DepMetadata("a", "b"); !errors.Is(err, beads.ErrDepMetadataUnsupported) {
		t.Fatalf("DepMetadata through the guard over a store without the read: err = %v, want ErrDepMetadataUnsupported", err)
	}
}
