package storebinding

import (
	"go/ast"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// graphPureReads is the closed set of GraphStore operations that cannot change
// what a later Get returns. EVERY other contract operation must run inside the
// cache fence, or the cache serves a value the store no longer holds.
//
// This list is the cache's whole correctness argument, so it is checked from
// both ends below: nothing in it may be missing from the contract, and nothing
// outside it may be missing from the wrapper.
var graphPureReads = map[string]bool{
	"Get":                     true,
	"List":                    true,
	"Ready":                   true,
	"ReadyContext":            true,
	"Children":                true,
	"DepList":                 true,
	"DepMetadata":             true,
	"Count":                   true,
	"WaitForParentProjection": true,
	"Ping":                    true,
}

// TestGraphCacheFencesEveryMutatingContractOperation is the census that keeps
// the cache honest as the contract grows.
//
// A wrapper embeds its contract interface, so an operation nobody overrode is
// forwarded silently and correctly — except when there is a cache behind it, in
// which case a forwarded MUTATION is a stale read. Adding a method to
// GraphStore therefore has to be a decision, and this census is where it gets
// made: the method is either a pure read (named above) or it is fenced.
//
// The operation set is resolved from the interface TYPE, and the overrides are
// resolved from the receiver declarations in the package's syntax tree. Neither
// is a text match, so renaming a wrapper or moving it to another file in this
// package cannot blind the census.
func TestGraphCacheFencesEveryMutatingContractOperation(t *testing.T) {
	contract := reflect.TypeOf((*GraphStore)(nil)).Elem()
	operations := map[string]bool{}
	for index := 0; index < contract.NumMethod(); index++ {
		operations[contract.Method(index).Name] = true
	}
	if len(operations) == 0 {
		t.Fatal("the Graph contract has no methods; this census has no subject")
	}
	for name := range graphPureReads {
		if !operations[name] {
			t.Errorf("graphPureReads names %q, which the Graph contract no longer has; the allowlist has drifted from the contract it describes", name)
		}
	}

	fenced, declared := packageReceiverMethods(t, "wrappedGraphStore")
	if len(declared) == 0 {
		t.Fatal("the census found no methods on the Graph wrapper; it is blind")
	}
	for name := range operations {
		if graphPureReads[name] {
			continue
		}
		if !declared[name] {
			t.Errorf("the Graph wrapper does not override %q, so the embedded contract forwards it and the cache never learns the store changed; override it or declare it a pure read", name)
			continue
		}
		if !fenced[name] {
			t.Errorf("the Graph wrapper overrides %q but its body never enters the cache fence; a mutation outside the fence leaves a stale cached bead", name)
		}
	}
}

// packageReceiverMethods returns, for one receiver type in this package, the
// set of declared method names and the subset whose body enters the cache
// fence. Both are resolved from the syntax tree: a method is found by its
// receiver TYPE and a fence entry by a call whose selector is the fence helper
// on that receiver.
func packageReceiverMethods(t *testing.T, receiverType string) (fenced, declared map[string]bool) {
	t.Helper()
	fenced = map[string]bool{}
	declared = map[string]bool{}
	for name, function := range packageReceiverFuncDecls(t, receiverType) {
		declared[name] = true
		if bodyEntersCacheFence(function) {
			fenced[name] = true
		}
	}
	return fenced, declared
}

func receiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	identifier, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return identifier.Name
}

// bodyEntersCacheFence reports whether a method calls the wrapper's own fence
// helper. Both helpers run the operation inside enterMutation/leaveMutation.
func bodyEntersCacheFence(function *ast.FuncDecl) bool {
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if selector.Sel.Name == "mutate" || selector.Sel.Name == "write" {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestGraphCacheInstallsNothingAcrossAnInvalidation is the generation fence in
// isolation: a read that started before a mutation must not install its result
// after it, however the two interleave.
func TestGraphCacheInstallsNothingAcrossAnInvalidation(t *testing.T) {
	cache := newGraphBeadCache()
	generation := cache.begin()

	cache.enterMutation()
	cache.leaveMutation()

	cache.install("gcg-1", beads.Bead{ID: "gcg-1", Title: "stale"}, generation)
	if _, found := cache.lookup("gcg-1"); found {
		t.Fatal("a read that started before a mutation installed its pre-mutation result; the cache now serves a value the store no longer holds")
	}
	if cache.Stats().Skipped != 1 {
		t.Errorf("the cache reports %d skipped installs, want 1; the skip is not observable", cache.Stats().Skipped)
	}

	fresh := cache.begin()
	cache.install("gcg-1", beads.Bead{ID: "gcg-1", Title: "fresh"}, fresh)
	got, found := cache.lookup("gcg-1")
	if !found || got.Title != "fresh" {
		t.Fatalf("lookup after a clean install = (%+v, %v), want the fresh bead", got, found)
	}
}

// TestGraphCacheInstallsNothingDuringAnInFlightMutation covers the other half
// of the race: the read finishes while a mutation is still open, so the
// generation has not moved yet and only the in-flight count can catch it.
func TestGraphCacheInstallsNothingDuringAnInFlightMutation(t *testing.T) {
	cache := newGraphBeadCache()
	generation := cache.begin()
	cache.enterMutation()
	cache.install("gcg-1", beads.Bead{ID: "gcg-1", Title: "stale"}, generation)
	cache.leaveMutation()
	if _, found := cache.lookup("gcg-1"); found {
		t.Fatal("a read installed its result while a mutation was in flight")
	}
}

// TestGraphCacheDetachesEveryReferenceField proves the cached value shares no
// mutable memory with either the writer or the reader.
func TestGraphCacheDetachesEveryReferenceField(t *testing.T) {
	priority := 2
	deferUntil := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	blocked := true
	original := beads.Bead{
		ID:           "gcg-1",
		Title:        "detached",
		Needs:        []string{"gcg-0"},
		Labels:       []string{"storage"},
		Metadata:     beads.StringMap{"plan_key": "wrapped"},
		Dependencies: []beads.Dep{{IssueID: "gcg-1", DependsOnID: "gcg-0", Type: "blocks"}},
		Priority:     &priority,
		DeferUntil:   &deferUntil,
		IsBlocked:    &blocked,
	}
	copied := deepCopyBead(original)

	// The fixture's own variables are what the pointers point at, so the
	// expected values are captured before anything is poisoned.
	wantDeferUntil := deferUntil

	original.Needs[0] = "poisoned"
	original.Labels[0] = "poisoned"
	original.Metadata["plan_key"] = "poisoned"
	original.Dependencies[0].Type = "poisoned"
	*original.Priority = 99
	*original.DeferUntil = deferUntil.Add(time.Hour)
	*original.IsBlocked = false

	if copied.Needs[0] != "gcg-0" {
		t.Errorf("Needs was shared: %q", copied.Needs[0])
	}
	if copied.Labels[0] != "storage" {
		t.Errorf("Labels was shared: %q", copied.Labels[0])
	}
	if copied.Metadata["plan_key"] != "wrapped" {
		t.Errorf("Metadata was shared: %q", copied.Metadata["plan_key"])
	}
	if copied.Dependencies[0].Type != "blocks" {
		t.Errorf("Dependencies was shared: %q", copied.Dependencies[0].Type)
	}
	if *copied.Priority != 2 {
		t.Errorf("Priority was shared: %d", *copied.Priority)
	}
	if !copied.DeferUntil.Equal(wantDeferUntil) {
		t.Errorf("DeferUntil was shared: %v", copied.DeferUntil)
	}
	if !*copied.IsBlocked {
		t.Errorf("IsBlocked was shared: %v", *copied.IsBlocked)
	}
}

// TestDeepCopyCoversEveryReferenceFieldOfABead is the completeness guard for
// the test above. Adding a slice, map or pointer field to beads.Bead adds a way
// for a caller to poison the cache, and this census fails until that field is
// covered by the detachment assertions.
func TestDeepCopyCoversEveryReferenceFieldOfABead(t *testing.T) {
	covered := map[string]bool{
		"Needs":        true,
		"Labels":       true,
		"Metadata":     true,
		"Dependencies": true,
		"Priority":     true,
		"DeferUntil":   true,
		"IsBlocked":    true,
	}
	beadType := reflect.TypeOf(beads.Bead{})
	var reference []string
	for index := 0; index < beadType.NumField(); index++ {
		field := beadType.Field(index)
		if !field.IsExported() {
			continue
		}
		switch field.Type.Kind() {
		case reflect.Slice, reflect.Map, reflect.Pointer:
			reference = append(reference, field.Name)
		case reflect.Interface, reflect.Chan, reflect.Func, reflect.UnsafePointer:
			t.Errorf("beads.Bead.%s is a %s; the reflect copier cannot detach one, so a cached bead would share it", field.Name, field.Type.Kind())
		}
	}
	if len(reference) == 0 {
		t.Fatal("beads.Bead has no reference-typed exported field; this census has no subject")
	}
	sort.Strings(reference)
	for _, name := range reference {
		if !covered[name] {
			t.Errorf("beads.Bead.%s is reference-typed and no detachment assertion covers it; a caller mutating it would poison the cache", name)
		}
	}
	for name := range covered {
		if _, found := beadType.FieldByName(name); !found {
			t.Errorf("the detachment assertions name %q, which beads.Bead no longer has", name)
		}
	}
}

// TestGraphCacheCountsHitsAndMisses pins the observable counters. Cache
// behavior that cannot be counted cannot be diagnosed in production.
func TestGraphCacheCountsHitsAndMisses(t *testing.T) {
	cache := newGraphBeadCache()
	if _, found := cache.lookup("gcg-1"); found {
		t.Fatal("an empty cache reported a hit")
	}
	cache.install("gcg-1", beads.Bead{ID: "gcg-1"}, cache.begin())
	if _, found := cache.lookup("gcg-1"); !found {
		t.Fatal("an installed bead was not found")
	}
	stats := cache.Stats()
	if stats.Misses != 1 || stats.Hits != 1 {
		t.Fatalf("cache stats = %+v, want exactly one hit and one miss", stats)
	}
	cache.enterMutation()
	cache.leaveMutation()
	if cache.Stats().Invalidations != 1 {
		t.Fatalf("cache reports %d invalidations after one mutation, want 1", cache.Stats().Invalidations)
	}
}

// TestGraphCacheStatsAreReadableThroughTheFrontDoor pins the status accessor.
// A cache whose counters cannot be read from the front door a consumer holds
// is a cache nobody can diagnose in production.
func TestGraphCacheStatsAreReadableThroughTheFrontDoor(t *testing.T) {
	inner := &staticGraphStore{bead: beads.Bead{ID: "gcg-1", Title: "counted"}}
	wrapping := ClassWrapping{Binding: "work", Capability: ClassCapability{Available: true}}

	uncached, err := WrapGraph(inner, wrapping)
	if err != nil {
		t.Fatalf("wrapping without a cache: %v", err)
	}
	if _, reported := GraphCacheStatsOf(uncached); reported {
		t.Error("a wrapper with no cache reports cache statistics; a status view cannot tell it apart from an idle cache")
	}
	if _, reported := GraphCacheStatsOf(inner); reported {
		t.Error("an unwrapped front door reports cache statistics")
	}

	wrapping.CacheReads = true
	cached, err := WrapGraph(inner, wrapping)
	if err != nil {
		t.Fatalf("wrapping with a cache: %v", err)
	}
	for round := 0; round < 3; round++ {
		if _, err := cached.Get("gcg-1"); err != nil {
			t.Fatalf("Get round %d: %v", round, err)
		}
	}
	stats, reported := GraphCacheStatsOf(cached)
	if !reported {
		t.Fatal("a wrapper with a cache reports no statistics")
	}
	if stats.Misses != 1 || stats.Hits != 2 {
		t.Fatalf("cache stats after three reads of one bead = %+v, want 1 miss and 2 hits", stats)
	}
}

// staticGraphStore answers one bead and nothing else.
type staticGraphStore struct {
	GraphStore
	bead beads.Bead
}

func (s *staticGraphStore) Get(string) (beads.Bead, error) { return s.bead, nil }
