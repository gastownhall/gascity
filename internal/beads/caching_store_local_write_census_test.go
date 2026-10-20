package beads

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"testing"
)

// localWriteCensusExempt lists the exported methods of CachingStore and the
// handles that wrap it which take no local write of a cached row, so need no
// beginLocalWrite registration. Every other exported method is a write and
// must register, directly or through a method of the cache it calls.
var localWriteCensusExempt = map[string]string{
	// Reads.
	"CachingStore.CachedList":                            "read",
	"CachingStore.CachedReady":                           "read",
	"CachingStore.Children":                              "read",
	"CachingStore.Count":                                 "read",
	"CachingStore.DepList":                               "read",
	"CachingStore.DepMetadata":                           "read",
	"CachingStore.Get":                                   "read",
	"CachingStore.GetLocalString":                        "read",
	"CachingStore.IDPrefix":                              "read",
	"CachingStore.IsLive":                                "read",
	"CachingStore.List":                                  "read",
	"CachingStore.ListByAssignee":                        "read",
	"CachingStore.ListByLabel":                           "read",
	"CachingStore.ListByMetadata":                        "read",
	"CachingStore.ListOpen":                              "read",
	"CachingStore.ObservedList":                          "read",
	"CachingStore.OwnsBeadID":                            "read",
	"CachingStore.Ping":                                  "read",
	"CachingStore.Ready":                                 "read",
	"CachingStore.ReadyContext":                          "read",
	"CachingStore.ReadyOrdered":                          "read",
	"CachingStore.ReadyPreferCache":                      "read",
	"CachingStore.SawRows":                               "read",
	"CachingStore.Stats":                                 "read",
	"CachingStore.Stopped":                               "read",
	"CachingStore.WaitForParentProjection":               "read",
	"CachingStore.WriteRev":                              "read",
	"CachingStore.AtomicTx":                              "read: reports a backing capability",
	"CachingStore.Backing":                               "read: returns the backing store",
	"CachingStore.WithCurrentObservation":                "read: publishes under an observation, writes no row",
	"CacheObservation.CacheRev":                          "read",
	"cachingGraphApplyStore.SupportsEphemeralGraphApply": "read: reports a capability",
	"cachedStoreReader.DepList":                          "read",
	"cachedStoreReader.Get":                              "read",
	"cachedStoreReader.List":                             "read",
	"cachedStoreReader.Ready":                            "read",
	"liveStoreReader.DepList":                            "read",
	"liveStoreReader.Get":                                "read",
	"liveStoreReader.List":                               "read",
	"liveStoreReader.Ready":                              "read",
	// Handle constructors: the handles' own methods are in the census.
	"CachingStore.AtomicConditionalCloserHandle": "handle constructor",
	"CachingStore.GraphApplyHandle":              "handle constructor",
	"CachingStore.GuardedBatcherHandle":          "handle constructor",
	"CachingStore.Handles":                       "handle constructor",
	"CachingStore.ReadyGraphOnlyHandle":          "handle constructor",
	// Cache maintenance: these install what the backing already holds, under
	// the cache's own fences, and write nothing to the backing store.
	"CachingStore.ApplyEvent":         "cache maintenance",
	"CachingStore.ApplyEventSnapshot": "cache maintenance",
	"CachingStore.Prime":              "cache maintenance",
	"CachingStore.PrimeActive":        "cache maintenance",
	"CachingStore.RefreshRow":         "cache maintenance",
	"CachingStore.RefreshRowReport":   "cache maintenance",
	"CachingStore.RequestReconcile":   "cache maintenance",
	"CachingStore.StartReconciler":    "cache maintenance",
	"CachingStore.StopReconciler":     "cache maintenance",
	// Test hooks.
	"CachingStore.ReconcileIfDueForTest":     "test hook",
	"CachingStore.ReconcileNowForTest":       "test hook",
	"CachingStore.SetPrimeRetryDelayForTest": "test hook",
	// Clone-local data is never cached, so there is no cached row to fence.
	"CachingStore.SetLocalString": "clone-local, never cached",
}

// TestCachingStoreWritesRegisterLocalWrites is the local-write census: every
// exported write method of CachingStore, and of each handle type that wraps
// one, registers itself in flight with beginLocalWrite (directly or through
// a method of the cache it calls), so a refresh that yields to local writes
// (RowRefreshOptions.YieldToLocalWrites, the ledger feed's) never installs a
// row from between a write's backing commit and its absorb. A write method
// added without a registration, or a method the census has not classified,
// fails here.
func TestCachingStoreWritesRegisterLocalWrites(t *testing.T) {
	registers, exported := localWriteCensus(t, packageGoFiles(t))
	var unregistered []string
	for _, m := range exported {
		_, exempt := localWriteCensusExempt[m]
		switch {
		case exempt && registers[m]:
			t.Errorf("%s is exempt from the census as %q but registers a local write: drop the exemption", m, localWriteCensusExempt[m])
		case !exempt && !registers[m]:
			unregistered = append(unregistered, m)
		}
	}
	for _, m := range unregistered {
		t.Errorf("%s does not register beginLocalWrite: register the write's ids (or none, for a write whose rows its backing call decides), or exempt it with a reason if it writes no cached row", m)
	}
	for m := range localWriteCensusExempt {
		if !slices.Contains(exported, m) {
			t.Errorf("census exemption %s names no exported method: drop it", m)
		}
	}
}

// TestLocalWriteCensusCatchesAMissingRegistration proves the census rejects a
// write whose registration was removed, and follows a write that registers
// through another method of the cache.
func TestLocalWriteCensusCatchesAMissingRegistration(t *testing.T) {
	const src = `package beads

type CachingStore struct{}

type wrapper struct{ cache *CachingStore }

func (c *CachingStore) beginLocalWrite(ids ...string) (uint64, func()) { return 0, func() {} }
func (c *CachingStore) Registered(id string) { _, end := c.beginLocalWrite(id); defer end() }
func (c *CachingStore) Unregistered(id string) {}
func (c *CachingStore) Delegates(id string) { c.helper(func() { c.Registered(id) }) }
func (c *CachingStore) helper(fn func()) { fn() }
func (w wrapper) Apply() { _, end := w.cache.beginLocalWrite(); defer end() }
func (w wrapper) Forgets() {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	registers, exported := censusFiles([]*ast.File{f})
	want := map[string]bool{
		"CachingStore.Registered":   true,
		"CachingStore.Unregistered": false,
		"CachingStore.Delegates":    true,
		"wrapper.Apply":             true,
		"wrapper.Forgets":           false,
	}
	if len(exported) != len(want) {
		t.Fatalf("exported = %v, want %d methods", exported, len(want))
	}
	for m, w := range want {
		if registers[m] != w {
			t.Errorf("registers[%s] = %v, want %v", m, registers[m], w)
		}
	}
}

// localWriteCensus parses the package's source files and runs the census.
func localWriteCensus(t *testing.T, files []string) (map[string]bool, []string) {
	t.Helper()
	fset := token.NewFileSet()
	parsed := make([]*ast.File, 0, len(files))
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed = append(parsed, f)
	}
	registers, exported := censusFiles(parsed)
	if len(exported) == 0 {
		t.Fatal("the census found no exported CachingStore methods")
	}
	return registers, exported
}

// censusFiles reports, for every method of CachingStore and of each type with
// a *CachingStore field, whether it registers a local write, and lists the
// exported ones as "Type.Method". A method registers when it calls
// beginLocalWrite or beginLocalCreate, or calls a registering CachingStore
// method, on its receiver (a CachingStore) or its receiver's *CachingStore
// field (a handle).
func censusFiles(files []*ast.File) (map[string]bool, []string) {
	const cacheType = "CachingStore"
	cacheFields := map[string]map[string]bool{} // type -> its *CachingStore fields
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				star, ok := field.Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				if id, ok := star.X.(*ast.Ident); !ok || id.Name != cacheType {
					continue
				}
				for _, name := range field.Names {
					if cacheFields[ts.Name.Name] == nil {
						cacheFields[ts.Name.Name] = map[string]bool{}
					}
					cacheFields[ts.Name.Name][name.Name] = true
				}
			}
			return false
		})
	}

	// calls[m] are the CachingStore methods m calls on its cache.
	calls := map[string][]string{}
	var exported []string
	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || fd.Body == nil {
				continue
			}
			recvType := receiverTypeName(fd.Recv.List[0].Type)
			if recvType != cacheType && cacheFields[recvType] == nil {
				continue
			}
			recvName := ""
			if names := fd.Recv.List[0].Names; len(names) == 1 {
				recvName = names[0].Name
			}
			key := recvType + "." + fd.Name.Name
			if fd.Name.IsExported() {
				exported = append(exported, key)
			}
			calls[key] = nil
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || recvName == "" {
					return true
				}
				if onCache(sel.X, recvName, recvType == cacheType, cacheFields[recvType]) {
					calls[key] = append(calls[key], cacheType+"."+sel.Sel.Name)
				}
				return true
			})
		}
	}

	registers := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for m, callees := range calls {
			if registers[m] {
				continue
			}
			for _, callee := range callees {
				if callee == cacheType+".beginLocalWrite" || callee == cacheType+".beginLocalCreate" || registers[callee] {
					registers[m] = true
					changed = true
					break
				}
			}
		}
	}
	sort.Strings(exported)
	return registers, exported
}

// onCache reports whether x names the cache a method runs against: its
// receiver for a CachingStore method, or a *CachingStore field of its
// receiver for a handle.
func onCache(x ast.Expr, recvName string, recvIsCache bool, fields map[string]bool) bool {
	switch x := x.(type) {
	case *ast.Ident:
		return recvIsCache && x.Name == recvName
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && id.Name == recvName && fields[x.Sel.Name]
	}
	return false
}

// receiverTypeName returns the base type name of a method receiver.
func receiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
