package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// byIDRawStoreBaseline is the shrink-only baseline TestByIDOpsDoNotTargetRawWorkStores
// ratchets against: file <TAB> function <TAB> rule <TAB> count, grouped under
// "## <reason>" headers.
const byIDRawStoreBaseline = "testdata/byid_raw_store.golden"

// byIDRawStoreUpdateEnv regenerates the baseline's counts in place. New keys
// land under an UNDOCUMENTED group the test refuses until someone moves each
// one under a reason.
const byIDRawStoreUpdateEnv = "UPDATE_BYID_RAW_STORE_GOLDEN"

// byIDUndocumentedGroup is the group regeneration files new keys under.
const byIDUndocumentedGroup = "UNDOCUMENTED — classify each row under a reason or fix it"

// byIDRawOpeners are the cmd/gc functions (and package-level opener vars) that
// hand back a RAW work or rig store: a beads.Store that answers only for the
// work class, so a by-id op on it misses (or patches a stale relic of) a bead a
// relocated class binding owns. A package var aliasing one of these inherits
// the classification without being listed.
var byIDRawOpeners = []string{
	"openCityStore", "openCityStoreWithPath", "openCityStoreAt", "openCityStoreAtWithConfig",
	"openCityStoreResultAt", "openStoreAtForCity", "openStoreAtForCityWithConfig",
	"openStoreAtForCityWithAuthority", "openAuthoritativeStoreAtForCity",
	"openOneShotStoreAtForCityWithConfig", "openScopeLocalFileStore", "openScopeLocalFileStoreForCity",
	"openExistingScopeLocalFileStore", "openCompatibleFileStore", "openControlStoreAtForCity",
	"openRigAwareStore", "openSlingCityStore", "openWispStepStore",
	"openCityStatusStore", "openStandaloneRigStores", "buildStandaloneRigStores",
	"buildStandaloneRigStoresWithConfig", "hookClaimBdStore", "hookClaimBdStoreContext",
	"openInfraMigrationSource", "openBdStoreAtScoped", "openBdStoreAtWithConfig",
	"openOneShotBdStoreAtWithConfig", "openControlBdStoreThroughFactory",
	"openExecStoreAtForCityWithConfig", "openExistingExecutionReemitStore",
	"openOptimizedDoltliteStore", "openPhysicalScopeStore", "openStoreResultAtForCity",
	"openStoreResultAtForCityScoped", "openStoreResultAtForCityWithAuthority",
	"openStoreResultAtForCityWithConfig", "openStoreResultAtForCityWithMode",
	"openWorkStoreResultAtForCityScoped", "openStoreForCity", "openStoreResultForCity",
	"openSlingStoreForSource", "openGitHubPRRepairStore", "openMailTargetStore",
	"openStoreFactoryForCity",
	// Per-id openers: they resolve the store that owns ONE id, so the handle
	// is raw for every other id. A same-id read carries a byid:allow marker.
	"openConvoyStoreByID", "openConvoyStoreByIDAt", "openSlingStore",
}

// byIDRawQualified are the raw openers outside cmd/gc, by qualified name.
var byIDRawQualified = []string{"beads.NewBdStore"}

// byIDRawAccessors are method calls that return a raw work or rig store (or a
// map of them): the controller's and city runtime's work-store accessors.
// Taking .Store off one is raw too.
var byIDRawAccessors = []string{"CityBeadStore", "cityBeadStore", "BeadStore", "BeadStores", "rigBeadStores", "Work", "cityWorkStore", "openRigStore"}

// byIDRawFields are struct fields holding raw work or rig stores.
var byIDRawFields = []string{"cityBeadStore", "beadStores", "standaloneCityStore", "standaloneRigStores"}

// byIDRoutedOpeners are the cmd/gc open*Store* functions that do NOT hand back
// a raw work store: each routes by class or residency, or returns a typed view
// whose consumers resolve ownership first. Listing one here clears an
// identifier assigned from it. The reason is the review.
var byIDRoutedOpeners = map[string]string{
	"openHookLaneSessionStore":             "returns cliSessionStore over the work store",
	"openLaneNudgeStore":                   "builds its journal through cliSessionStore",
	"openNudgeBeadStore":                   "returns cliStores(...).Nudges()",
	"openNudgeBeadStoreErr":                "returns cliStores(...).Nudges()",
	"openNudgeBeadStoreOwned":              "returns cliStores(...).Nudges(); the second result is the owned work handle, closed, never read by id",
	"openOwnedNudgeBeadStore":              "returns cliStores(...).Nudges(); the second result is the owned work handle, closed, never read by id",
	"openOrderStoreForOrder":               "returns an OrdersStore; its relabel is a baseline row (order_store.go)",
	"openConvergeStore":                    "returns a convergeScopeReader, which routes by convergeScopeStore",
	"openConvoyStores":                     "returns convoyStoreView directory scans behind byIDStoreOpener; convoys are work class",
	"openConvoyStoreDirs":                  "returns convoyStoreView directory scans; convoys are work class",
	"openConvoyCheckStoresAt":              "returns convoyStoreView directory scans; convoys are work class",
	"openAllConvoyStoresAt":                "returns convoyStoreView directory scans; by-id callers ask cliByIDBindingOwner first",
	"openEveryStoreAt":                     "returns convoyStoreView enumerations for listing, not by-id",
	"openSourceWorkflowStoreRef":           "returns a convoyStoreView of an explicit store ref; source workflows are work class",
	"openSourceWorkflowStores":             "returns convoyStoreView scans; source workflows are work class",
	"openSourceWorkflowStoresWith":         "returns convoyStoreView scans; source workflows are work class",
	"openSourceWorkflowStoresWithProvider": "returns convoyStoreView scans; source workflows are work class",
	"openSourceWorkflowStoresForCollect":   "test seam over openSourceWorkflowStores",
	"openInfraPurgeStores":                 "returns typed WorkStore/MailStore handles for the purge tool",
}

// byIDSanitizers are the class/residency helpers: an identifier assigned from
// one (or from a method chained off one) holds a routed store.
var byIDSanitizers = []string{
	"cliSessionStore", "cliSessionFrontDoor", "cliRelocatedSessionStore", "cliGraphStore", "cliMailStore",
	"cliNudgesStore", "cliStores", "scopeStores", "resolveClassStore", "resolveSessionStore", "resolveGraphStore",
	"resolveMailMessagesStore", "resolveOrderStore", "resolveNudgesStore", "classRoutedStoreForID",
	"classRoutedStoreForIDIn", "cliByIDOwner", "moleculeClassStore", "graphPlacementForScope", "controlGraphStore",
	"convergeScopeStore", "sessionsBeadStore", "SessionsBeadStore", "graphBeadStore", "GraphBeadStore", "mailBeadStore",
	"nudgesBeadStore", "NudgesBeadStore", "ordersBeadStore", "OrdersBeadStore", "ordersStoreFor", "storeFor",
}

// byIDMethods are the beads.Store methods that address one bead (or a batch)
// by id. Close counts only with an argument: a no-arg Close closes the handle.
var byIDMethods = []string{
	"Get", "Update", "Close", "Reopen", "CloseAll", "SetMetadata", "SetMetadataBatch", "Delete",
	"DepAdd", "DepRemove", "DepList", "Children", "UpdateIfMatch", "CloseIfMatch", "CloseWithMetadataIfMatch",
	"ReleaseIfCurrent", "TransferIfCurrent", "UpdateIfAssignment", "Claim", "CompareAndSetMetadataKey",
	"GetExactBatch", "DeleteBatch", "UpdateReporting", "SetLocalString", "GetLocalString",
}

// byIDFrontDoors are the cmd/gc constructors that turn a store into a class
// front door; handing one a raw work store relabels it without routing it.
// (session.NewStore, orders.NewStore and nudgequeue.NewStore take typed class
// handles, so a raw store reaches them only through a relabel, flagged there.)
var byIDFrontDoors = []string{"sessionFrontDoor", "nudgeFrontDoor"}

// byIDQualifiedSinks are by-id functions outside cmd/gc, by qualified name,
// with the index of their beads.Store argument.
var byIDQualifiedSinks = map[string]int{
	"beads.ApplyMetadataCAS": 0,
	"beads.RelicGet":         0,
	"sling.ProbeBeadInStore": 0,
}

// byIDAllowMarker exempts one flagged line: `// byid:allow <reason>` trailing
// the line of the method selector, callee name or Store: key. It is honored
// only at the site itself; a marker inside a helper does not exempt the
// helper's callers.
const byIDAllowMarker = "byid:allow"

// byIDClassHandles are the beads class-handle structs; beads.X{Store: raw}
// relabels a raw work store as a class store.
var byIDClassHandles = []string{"SessionStore", "MailStore", "NudgesStore", "OrdersStore", "GraphStore"}

// byIDOpenerName is the self-check's net: every function or package var this
// matches that returns a store must be classified raw or routed.
var byIDOpenerName = regexp.MustCompile(`^open[A-Za-z0-9_]*Store`)

// byIDHit is one flagged site.
type byIDHit struct {
	file, fn, rule, detail string
	line                   int
}

func (h byIDHit) key() string { return h.file + "\t" + h.fn + "\t" + h.rule }

// byIDFunc is one function declaration of the scanned package.
type byIDFunc struct {
	key, name, file string
	decl            *ast.FuncDecl
	storeParams     map[string]int // beads.Store parameter name -> index
}

// byIDScanner is the guard over one parsed package.
type byIDScanner struct {
	fset     *token.FileSet
	funcs    []*byIDFunc
	byName   map[string][]*byIDFunc
	allow    map[string]map[int]bool // file -> lines carrying byIDAllowMarker
	raw      map[string]bool         // raw opener names, aliases resolved
	routed   map[string]bool
	sanitize map[string]bool
	summary  map[string]map[int]bool // func key -> beads.Store param indexes reaching a sink
	rawPos   map[string]map[int]bool // raw function/var name -> raw result positions
	vars     map[string]ast.Expr     // package-level open*Store* vars -> value
	declared map[string]bool         // every function, method and package var name
	// derived are package functions classified raw only because a return
	// statement hands back a raw store (return-value summaries).
	derived map[string]bool
	// stale are list entries naming nothing in the package.
	stale []string
	used  map[string]map[int]bool // marker lines that suppressed a hit
	// unclassified are open*Store* functions/vars returning a store that are
	// classified neither raw nor routed.
	unclassified []string
}

func byIDSet(xs ...[]string) map[string]bool {
	m := map[string]bool{}
	for _, list := range xs {
		for _, x := range list {
			m[x] = true
		}
	}
	return m
}

// newByIDScanner indexes files (base name -> parsed file, comments included).
func newByIDScanner(fset *token.FileSet, files map[string]*ast.File) *byIDScanner {
	s := &byIDScanner{
		fset:     fset,
		byName:   map[string][]*byIDFunc{},
		allow:    map[string]map[int]bool{},
		raw:      byIDSet(byIDRawOpeners),
		routed:   map[string]bool{},
		declared: map[string]bool{},
		derived:  map[string]bool{},
		rawPos:   map[string]map[int]bool{},
		used:     map[string]map[int]bool{},
		sanitize: byIDSet(byIDSanitizers),
	}
	for name := range byIDRoutedOpeners {
		s.routed[name] = true
		s.sanitize[name] = true
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	type openerVar struct {
		name  string
		value ast.Expr
	}
	var vars []openerVar
	for _, base := range names {
		f := files[base]
		s.allow[base] = map[int]bool{}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if strings.Contains(c.Text, byIDAllowMarker) {
					s.allow[base][fset.Position(c.Pos()).Line] = true
				}
			}
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				fn := &byIDFunc{name: d.Name.Name, key: d.Name.Name, file: base, decl: d, storeParams: map[string]int{}}
				if d.Recv != nil && len(d.Recv.List) > 0 {
					fn.key = byIDRecvName(d.Recv.List[0].Type) + "." + d.Name.Name
				}
				i := 0
				for _, field := range d.Type.Params.List {
					isStore := byIDExprString(field.Type) == "beads.Store"
					if len(field.Names) == 0 {
						i++
						continue
					}
					for _, n := range field.Names {
						if isStore {
							fn.storeParams[n.Name] = i
						}
						i++
					}
				}
				s.funcs = append(s.funcs, fn)
				s.byName[fn.name] = append(s.byName[fn.name], fn)
				s.declared[fn.name] = true
			case *ast.GenDecl:
				ast.Inspect(d, func(n ast.Node) bool {
					spec, ok := n.(*ast.ValueSpec)
					if !ok {
						return true
					}
					for i, id := range spec.Names {
						s.declared[id.Name] = true
						if i < len(spec.Values) && byIDOpenerName.MatchString(id.Name) {
							vars = append(vars, openerVar{id.Name, spec.Values[i]})
						}
					}
					return false
				})
			}
		}
	}
	s.vars = map[string]ast.Expr{}
	for _, v := range vars {
		s.vars[v.name] = v.value
	}
	s.deriveRawReturns()
	seen := map[string]bool{}
	for _, fn := range s.funcs {
		if !byIDOpenerName.MatchString(fn.name) || !byIDReturnsStore(fn.decl.Type) || seen[fn.name] {
			continue
		}
		seen[fn.name] = true
		if !s.raw[fn.name] && !s.routed[fn.name] && (fn.decl.Recv == nil || !slices.Contains(byIDRawAccessors, fn.name)) {
			s.unclassified = append(s.unclassified, fn.file+": func "+fn.name)
		}
	}
	for _, v := range vars {
		if s.raw[v.name] || s.routed[v.name] || seen[v.name] {
			continue
		}
		returnsStore := false
		switch x := v.value.(type) {
		case *ast.FuncLit:
			returnsStore = byIDReturnsStore(x.Type)
		case *ast.Ident:
			for _, fn := range s.byName[x.Name] {
				returnsStore = returnsStore || byIDReturnsStore(fn.decl.Type)
			}
		case *ast.SelectorExpr:
			returnsStore = true // an opener outside the package: classify it explicitly
		}
		if returnsStore {
			seen[v.name] = true
			s.unclassified = append(s.unclassified, "var "+v.name)
		}
	}
	s.staleListEntries()
	slices.Sort(s.unclassified)
	s.summarize()
	return s
}

// deriveRawReturns classifies, to a fixed point, every package function whose
// return statement hands back a raw store, and every opener var aliasing a raw
// or routed function. A helper outside any naming convention that returns a
// raw store is therefore raw at its call sites.
func (s *byIDScanner) deriveRawReturns() {
	for name := range s.raw {
		s.rawPos[name] = s.declaredStorePositions(name, map[string]bool{})
	}
	for changed := true; changed; {
		changed = false
		for name, value := range s.vars {
			id, ok := value.(*ast.Ident)
			if !ok || s.raw[name] || s.routed[name] {
				continue
			}
			switch {
			case s.raw[id.Name]:
				s.raw[name] = true
				s.rawPos[name] = s.rawPos[id.Name]
				changed = true
			case s.routed[id.Name]:
				s.routed[name] = true
				s.sanitize[name] = true
				changed = true
			}
		}
		for _, fn := range s.funcs {
			if fn.decl.Recv != nil || s.sanitize[fn.name] || fn.decl.Type.Results == nil {
				continue
			}
			if !s.derived[fn.name] && s.raw[fn.name] {
				continue // listed: its positions come from its declared result types
			}
			for pos := range s.rawReturnPositions(fn.decl) {
				if s.rawPos[fn.name] == nil {
					s.rawPos[fn.name] = map[int]bool{}
				}
				if !s.rawPos[fn.name][pos] {
					s.rawPos[fn.name][pos] = true
					s.raw[fn.name] = true
					s.derived[fn.name] = true
					changed = true
				}
			}
		}
	}
}

// declaredStorePositions are the result positions of a listed opener whose
// declared type is a beads store type. An opener the package cannot resolve
// (or one declaring no such type) is taken as raw at position 0.
func (s *byIDScanner) declaredStorePositions(name string, seen map[string]bool) map[int]bool {
	if seen[name] {
		return map[int]bool{0: true}
	}
	seen[name] = true
	var ft *ast.FuncType
	for _, fn := range s.byName[name] {
		if fn.decl.Recv == nil {
			ft = fn.decl.Type
		}
	}
	if ft == nil {
		switch v := s.vars[name].(type) {
		case *ast.FuncLit:
			ft = v.Type
		case *ast.Ident:
			return s.declaredStorePositions(v.Name, seen)
		}
	}
	out := map[int]bool{}
	if ft != nil && ft.Results != nil {
		pos := 0
		for _, field := range ft.Results.List {
			n := max(len(field.Names), 1)
			for range n {
				if byIDIsBeadsStoreType(field.Type) {
					out[pos] = true
				}
				pos++
			}
		}
	}
	if len(out) == 0 {
		out[0] = true
	}
	return out
}

// byIDIsBeadsStoreType reports a type expression naming a beads store type
// (beads.Store, *beads.BdStore, beads.StoreOpenResult, map[string]beads.Store,
// func(...) (beads.Store, error), ...).
func byIDIsBeadsStoreType(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && byIDExprString(sel.X) == "beads" && strings.Contains(sel.Sel.Name, "Store") {
			found = true
		}
		return !found
	})
	return found
}

// rawReturnPositions are the result positions at which a return statement of
// fn (not of a closure inside it) hands back a raw store. A `return f()` that
// forwards a multi-result call forwards f's raw positions.
func (s *byIDScanner) rawReturnPositions(fn *ast.FuncDecl) map[int]bool {
	raw := s.rawIdents(fn.Body, nil)
	out := map[int]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			if len(x.Results) == 1 && fn.Type.Results.NumFields() > 1 {
				for pos, isRaw := range s.callResultRaw(x.Results[0], fn.Type.Results.NumFields(), raw) {
					if isRaw {
						out[pos] = true
					}
				}
				return true
			}
			for pos, r := range x.Results {
				if !s.isSanitized(r) && s.isRawSource(r, raw) {
					out[pos] = true
				}
			}
		}
		return true
	})
	return out
}

// callResultRaw reports, per position, whether the n values rhs produces are
// raw stores: a package function's call taints exactly its raw result
// positions; any other expression (a map index, a type assertion, a
// single-value call) taints at most position 0.
func (s *byIDScanner) callResultRaw(rhs ast.Expr, n int, raw map[string]bool) []bool {
	out := make([]bool, n)
	if s.isSanitized(rhs) {
		return out
	}
	if call, ok := rhs.(*ast.CallExpr); ok {
		if id, ok := call.Fun.(*ast.Ident); ok && s.raw[id.Name] {
			for pos := range s.rawPos[id.Name] {
				if pos < n {
					out[pos] = true
				}
			}
			return out
		}
	}
	if n > 0 {
		out[0] = s.isRawSource(rhs, raw)
	}
	return out
}

// staleListEntries records list entries that name nothing declared in the
// package, so a renamed or deleted helper cannot leave a dead classification.
// Method names (routed method resolvers) count as declared. The raw accessors
// are not checked: several are methods of types outside cmd/gc.
func (s *byIDScanner) staleListEntries() {
	check := func(list string, names []string) {
		for _, name := range names {
			if !s.declared[name] {
				s.stale = append(s.stale, list+": "+name)
			}
		}
	}
	check("byIDRawOpeners", byIDRawOpeners)
	check("byIDSanitizers", byIDSanitizers)
	routed := make([]string, 0, len(byIDRoutedOpeners))
	for name := range byIDRoutedOpeners {
		routed = append(routed, name)
	}
	slices.Sort(routed)
	check("byIDRoutedOpeners", routed)
	for _, fd := range byIDFrontDoors {
		if !strings.Contains(fd, ".") {
			check("byIDFrontDoors", []string{fd})
		}
	}
}

// byIDReturnsStore reports a result type that names a store.
func byIDReturnsStore(ft *ast.FuncType) bool {
	if ft.Results == nil {
		return false
	}
	found := false
	for _, r := range ft.Results.List {
		ast.Inspect(r.Type, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && strings.Contains(id.Name, "Store") {
				found = true
			}
			return !found
		})
	}
	return found
}

func byIDRecvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return byIDRecvName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return byIDRecvName(t.X)
	case *ast.IndexListExpr:
		return byIDRecvName(t.X)
	}
	return "?"
}

func byIDExprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return byIDExprString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + byIDExprString(t.X)
	}
	return ""
}

// byIDCallee returns a call's bare name and its qualified spelling.
func byIDCallee(c *ast.CallExpr) (name, qual string) {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name, f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name, byIDExprString(f)
	case *ast.IndexExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			return id.Name, id.Name
		}
	}
	return "", ""
}

func byIDIdent(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return byIDIdent(x.X)
	case *ast.ParenExpr:
		return byIDIdent(x.X)
	}
	return ""
}

// isRawSource reports whether e evaluates to a raw store given the function's
// raw identifiers.
func (s *byIDScanner) isRawSource(e ast.Expr, raw map[string]bool) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return raw[x.Name]
	case *ast.IndexExpr:
		return s.isRawSource(x.X, raw)
	case *ast.ParenExpr:
		return s.isRawSource(x.X, raw)
	case *ast.CallExpr:
		name, qual := byIDCallee(x)
		if slices.Contains(byIDRawQualified, qual) {
			return true
		}
		if _, ok := x.Fun.(*ast.Ident); ok && s.raw[name] && (s.rawPos[name] == nil || s.rawPos[name][0]) {
			return true
		}
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && slices.Contains(byIDRawAccessors, sel.Sel.Name) {
			return true
		}
	case *ast.SelectorExpr:
		if slices.Contains(byIDRawFields, x.Sel.Name) {
			return true
		}
		// .Store off a raw source: openCityStoreResultAt(...).Store,
		// res.Store, cs.cityWorkStore().Store.
		if x.Sel.Name == "Store" && s.isRawSource(x.X, raw) {
			return true
		}
	}
	return false
}

// isSanitized reports whether e is (a chain off) a class/residency helper call.
func (s *byIDScanner) isSanitized(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			e = x.X
			continue
		case *ast.CallExpr:
			name, _ := byIDCallee(x)
			if s.sanitize[name] {
				return true
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if inner, ok := sel.X.(*ast.CallExpr); ok {
					e = inner
					continue
				}
			}
			return false
		}
		return false
	}
}

// rawIdents computes, flow-insensitively, the identifiers in body holding a
// raw store. seed are identifiers raw by assumption (a summarized parameter).
// An identifier ever assigned from a class/residency helper is cleared.
func (s *byIDScanner) rawIdents(body *ast.BlockStmt, seed map[string]bool) map[string]bool {
	raw := map[string]bool{}
	for k := range seed {
		raw[k] = true
	}
	sanitized := map[string]bool{}
	for changed := true; changed; {
		changed = false
		mark := func(name string) {
			if name != "_" && !raw[name] {
				raw[name] = true
				changed = true
			}
		}
		ast.Inspect(body, func(n ast.Node) bool {
			switch st := n.(type) {
			case *ast.AssignStmt:
				if len(st.Rhs) == 1 && len(st.Lhs) > 1 {
					// A multi-value right-hand side taints each left-hand
					// variable by its result position.
					tainted := s.callResultRaw(st.Rhs[0], len(st.Lhs), raw)
					sanitizedRHS := s.isSanitized(st.Rhs[0])
					for i, lhs := range st.Lhs {
						id, ok := lhs.(*ast.Ident)
						if !ok {
							continue
						}
						switch {
						case sanitizedRHS && i == 0:
							sanitized[id.Name] = true
						case tainted[i]:
							mark(id.Name)
						}
					}
					break
				}
				for i, lhs := range st.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok || i >= len(st.Rhs) {
						continue
					}
					if s.isSanitized(st.Rhs[i]) {
						sanitized[id.Name] = true
					} else if s.isRawSource(st.Rhs[i], raw) {
						mark(id.Name)
					}
				}
			case *ast.RangeStmt:
				if s.isRawSource(st.X, raw) {
					if v, ok := st.Value.(*ast.Ident); ok {
						mark(v.Name)
					}
				}
			case *ast.ValueSpec:
				if len(st.Values) == 1 && len(st.Names) > 1 {
					for i, isRaw := range s.callResultRaw(st.Values[0], len(st.Names), raw) {
						if isRaw {
							mark(st.Names[i].Name)
						}
					}
					break
				}
				for i, id := range st.Names {
					if i < len(st.Values) && !s.isSanitized(st.Values[i]) && s.isRawSource(st.Values[i], raw) {
						mark(id.Name)
					}
				}
			}
			return true
		})
	}
	for k := range sanitized {
		if !seed[k] {
			delete(raw, k)
		}
	}
	return raw
}

// sinks returns the flagged sites in body for the raw identifiers.
func (s *byIDScanner) sinks(file string, body *ast.BlockStmt, raw map[string]bool, honorMarkers bool) []byIDHit {
	var out []byIDHit
	// add anchors a hit to the line of anchor (the method selector, the callee
	// name, the Store: key), which is the line a byid:allow marker must sit on.
	add := func(rule, detail string, anchor ast.Node) {
		line := s.fset.Position(anchor.Pos()).Line
		if honorMarkers && s.allow[file][line] {
			s.markUsed(file, line)
			return
		}
		out = append(out, byIDHit{file: file, rule: rule, detail: detail, line: line})
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			// A closure parameter shadows a same-named raw identifier of the
			// enclosing function: scan its body without it.
			shadowed := false
			inner := map[string]bool{}
			for k := range raw {
				inner[k] = true
			}
			for _, field := range x.Type.Params.List {
				for _, id := range field.Names {
					if inner[id.Name] {
						delete(inner, id.Name)
						shadowed = true
					}
				}
			}
			if shadowed {
				out = append(out, s.sinks(file, x.Body, inner, honorMarkers)...)
				return false
			}
		case *ast.CallExpr:
			name, qual := byIDCallee(x)
			anchor := ast.Node(x.Fun)
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				anchor = sel.Sel
				if slices.Contains(byIDMethods, name) && (name != "Close" || len(x.Args) != 0) && s.isRawSource(sel.X, raw) {
					add("byid", name+" on "+byIDExprString(sel.X), sel.Sel)
				}
			}
			if slices.Contains(byIDFrontDoors, name) {
				for _, a := range x.Args {
					if s.isRawSource(a, raw) {
						add("frontdoor", qual+"("+byIDIdent(a)+")", anchor)
					}
				}
			}
			if idx, ok := byIDQualifiedSinks[qual]; ok && idx < len(x.Args) && s.isRawSource(x.Args[idx], raw) {
				add("via", qual+"("+byIDIdent(x.Args[idx])+")", anchor)
			}
			if _, ok := x.Fun.(*ast.Ident); ok && !s.sanitize[name] {
				for _, cand := range s.byName[name] {
					if cand.decl.Recv != nil {
						continue
					}
					for idx := range s.summary[cand.key] {
						if idx < len(x.Args) && s.isRawSource(x.Args[idx], raw) {
							add("via", name+"("+byIDIdent(x.Args[idx])+")", anchor)
						}
					}
				}
			}
		case *ast.CompositeLit:
			if sel, ok := x.Type.(*ast.SelectorExpr); ok && byIDExprString(sel.X) == "beads" && slices.Contains(byIDClassHandles, sel.Sel.Name) {
				for _, el := range x.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Store" && s.isRawSource(kv.Value, raw) {
						add("relabel", "beads."+sel.Sel.Name+"{Store: "+byIDIdent(kv.Value)+"}", kv.Key)
					}
				}
			}
		}
		return true
	})
	return out
}

// summarize computes, to a fixed point, which beads.Store parameters of each
// package function reach a sink: a raw store handed to one is flagged at the
// call site.
func (s *byIDScanner) summarize() {
	s.summary = map[string]map[int]bool{}
	for changed := true; changed; {
		changed = false
		for _, fn := range s.funcs {
			for p, idx := range fn.storeParams {
				if s.summary[fn.key][idx] {
					continue
				}
				raw := s.rawIdents(fn.decl.Body, map[string]bool{p: true})
				if len(s.sinks(fn.file, fn.decl.Body, raw, false)) == 0 {
					continue
				}
				if s.summary[fn.key] == nil {
					s.summary[fn.key] = map[int]bool{}
				}
				s.summary[fn.key][idx] = true
				changed = true
			}
		}
	}
}

func (s *byIDScanner) markUsed(file string, line int) {
	if s.used[file] == nil {
		s.used[file] = map[int]bool{}
	}
	s.used[file][line] = true
}

// unusedMarkers are byid:allow lines that suppress no hit: a marker left on a
// line the guard no longer flags, or put on the wrong line of a multi-line
// call. Valid only after hits.
func (s *byIDScanner) unusedMarkers() []string {
	var out []string
	for file, lines := range s.allow {
		for line := range lines {
			if !s.used[file][line] {
				out = append(out, fmt.Sprintf("%s:%d", file, line))
			}
		}
	}
	slices.Sort(out)
	return out
}

// hits returns every flagged site in the package.
func (s *byIDScanner) hits() []byIDHit {
	var out []byIDHit
	for _, fn := range s.funcs {
		raw := s.rawIdents(fn.decl.Body, nil)
		for _, h := range s.sinks(fn.file, fn.decl.Body, raw, true) {
			h.fn = fn.key
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b byIDHit) int {
		if c := strings.Compare(a.key(), b.key()); c != 0 {
			return c
		}
		return a.line - b.line
	})
	return out
}

// parseByIDPackage parses every non-test Go file in dir.
func parseByIDPackage(t *testing.T, dir string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		files[filepath.Base(p)] = f
	}
	return fset, files
}

// byIDBaselineRow is one golden entry and the reason group it sits under.
type byIDBaselineRow struct {
	key, group string
	count      int
}

func readByIDBaseline(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// parseByIDBaseline parses the golden's rows, each with the "## <reason>"
// group it sits under.
func parseByIDBaseline(lines []string) ([]byIDBaselineRow, error) {
	var rows []byIDBaselineRow
	group := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "## "):
			group = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			continue
		case strings.HasPrefix(trimmed, "#"):
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			return nil, fmt.Errorf("malformed row %q: want file<TAB>function<TAB>rule<TAB>count", line)
		}
		n, err := strconv.Atoi(parts[3])
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("row %q: count must be a positive integer", line)
		}
		rows = append(rows, byIDBaselineRow{key: strings.Join(parts[:3], "\t"), group: group, count: n})
	}
	return rows, nil
}

// regenByIDBaseline is the regeneration path. A documented row is kept, or
// LOWERED in place when its count fell, or dropped when it reaches nothing. A
// row is never raised in place: a grown key is commented out where it was and
// filed, at its new count, in a fresh UNDOCUMENTED block with every new key and
// every row an earlier regeneration left undocumented. The checker refuses that
// block however many times regeneration runs.
func regenByIDBaseline(lines []string, got map[string]int) []string {
	var out, pending []string
	group := ""
	filed := map[string]bool{}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		parts := strings.Split(line, "\t")
		if strings.HasPrefix(trimmed, "## ") {
			group = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			if group == byIDUndocumentedGroup {
				continue
			}
		}
		if len(parts) != 4 || strings.HasPrefix(trimmed, "#") {
			if group == byIDUndocumentedGroup && trimmed == "" {
				continue
			}
			out = append(out, line)
			continue
		}
		key := strings.Join(parts[:3], "\t")
		n, _ := strconv.Atoi(parts[3])
		filed[key] = true
		switch {
		case got[key] == 0:
		case group == "" || group == byIDUndocumentedGroup:
			pending = append(pending, fmt.Sprintf("%s\t%d", key, got[key]))
		case got[key] > n:
			out = append(out, fmt.Sprintf("# grew from %d to %d; reclassify: %s", n, got[key], strings.ReplaceAll(key, "\t", " ")))
			pending = append(pending, fmt.Sprintf("%s\t%d", key, got[key]))
		default:
			out = append(out, fmt.Sprintf("%s\t%d", key, got[key]))
		}
	}
	for key, n := range got {
		if !filed[key] {
			pending = append(pending, fmt.Sprintf("%s\t%d", key, n))
		}
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(pending) > 0 {
		slices.Sort(pending)
		out = append(out, "", "## "+byIDUndocumentedGroup)
		out = append(out, pending...)
	}
	return out
}

// checkByIDBaseline reports every way the tree and the golden disagree.
func checkByIDBaseline(rows []byIDBaselineRow, got map[string]int, sites map[string][]string) []string {
	var problems []string
	want := map[string]int{}
	for _, r := range rows {
		if _, dup := want[r.key]; dup {
			problems = append(problems, fmt.Sprintf("duplicate row %q", r.key))
		}
		want[r.key] = r.count
		if r.group == "" || r.group == byIDUndocumentedGroup {
			problems = append(problems, fmt.Sprintf("row %q is undocumented: move it under a \"## <reason>\" header, or fix the site", strings.ReplaceAll(r.key, "\t", " ")))
		}
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if got[k] > want[k] {
			problems = append(problems, fmt.Sprintf("NEW by-id op on a raw work store: %s: %d site(s), baseline %d:\n\t%s\n"+
				"Route it through cliByIDOwner / cliByIDBindingOwner / classRoutedStoreForID / the class resolvers (cliSessionStore, cliStores(...).X(), resolve*Store),"+
				" or, if the bead is work-class by construction, add a trailing `// %s <reason>` on the flagged line.",
				strings.ReplaceAll(k, "\t", " "), got[k], want[k], strings.Join(sites[k], "\n\t"), byIDAllowMarker))
		}
	}
	for _, r := range rows {
		if got[r.key] < r.count {
			problems = append(problems, fmt.Sprintf("%s: %d site(s), below its baseline %d: lower the row (or remove it) in the same change (%s=1 rewrites counts)",
				strings.ReplaceAll(r.key, "\t", " "), got[r.key], r.count, byIDRawStoreUpdateEnv))
		}
	}
	return problems
}

// TestByIDOpsDoNotTargetRawWorkStores fails CI when a by-id bead operation is
// aimed at a RAW work or rig store.
//
// On a split city ([storage.classes] relocating graph, sessions, messaging,
// orders and nudges to a non-work binding) a coordination-class bead lives only
// in its binding, many under migrated work-shaped ids. A by-id Get, Update,
// SetMetadata, Close, ... issued on the raw work store for such a bead reads
// "not found" or patches the stale pre-migration copy — the class of bug behind
// #8137, ga-ylpbsy.36 and `gc session reset`.
//
// Per function (go/parser, flow-insensitive), an identifier is RAW when
// assigned from a raw opener (byIDRawOpeners, their package-var aliases, and
// any package function whose return statement hands back a raw store), a raw
// accessor or field, .Store off one, or a range value over one; it is cleared
// when assigned from a class/residency helper (byIDSanitizers,
// byIDRoutedOpeners). Flagged: a by-id method on a raw store (rule byid), a raw
// store handed to a front-door constructor (frontdoor) or relabeled as a class
// handle (relabel), and a raw store passed to a package function whose
// beads.Store parameter reaches any of those, or to a byIDQualifiedSinks
// function (via).
//
// Known residuals, by design of a name-based scan: identifiers are matched by
// NAME, not by scope, so a same-named variable in another block of the
// function is conflated with the raw one (closure parameters excepted); and the
// analysis is flow-insensitive, so a function that swaps one variable between
// a raw and a routed store is judged by whether it was EVER assigned a routed
// store. Methods, interfaces and other packages are not followed; that typed
// reachability is a separate guard. A raw store wrapped in a typed handle
// literal (beads.WorkStore{Store: raw}) is not tracked through the handle:
// operations on the handle, or on its .Store, are not attributed back to the
// raw store.
//
// The counts ratchet against byIDRawStoreBaseline: growth fails, and so does a
// row the tree no longer reaches. A self-check fails any open*Store* function
// or opener var returning a store that is classified neither raw nor routed,
// and any classification list entry that names nothing in the package.
func TestByIDOpsDoNotTargetRawWorkStores(t *testing.T) {
	fset, files := parseByIDPackage(t, ".")
	s := newByIDScanner(fset, files)
	for _, u := range s.unclassified {
		t.Errorf("%s returns a store but is classified neither raw (byIDRawOpeners) nor routed (byIDRoutedOpeners) in byid_raw_store_guard_test.go; classify it so a by-id op on what it returns is policed", u)
	}
	for _, e := range s.stale {
		t.Errorf("stale classification entry %s: it names nothing declared in cmd/gc; drop or rename it", e)
	}
	hits := s.hits()
	for _, m := range s.unusedMarkers() {
		t.Errorf("%s: a %s marker suppresses no hit; remove it, or move it to the line of the flagged selector/callee", m, byIDAllowMarker)
	}
	got := map[string]int{}
	sites := map[string][]string{}
	for _, h := range hits {
		got[h.key()]++
		sites[h.key()] = append(sites[h.key()], fmt.Sprintf("%s:%d %s", h.file, h.line, h.detail))
	}
	if len(hits) == 0 || len(s.funcs) < 1000 {
		t.Fatalf("flagged %d sites over %d functions; the scan missed the package", len(hits), len(s.funcs))
	}
	lines := readByIDBaseline(t, byIDRawStoreBaseline)
	if os.Getenv(byIDRawStoreUpdateEnv) != "" {
		out := regenByIDBaseline(lines, got)
		if err := os.WriteFile(byIDRawStoreBaseline, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s; document every UNDOCUMENTED row under a reason before committing", byIDRawStoreBaseline)
		return
	}
	rows, err := parseByIDBaseline(lines)
	if err != nil {
		t.Fatalf("%s: %v", byIDRawStoreBaseline, err)
	}
	for _, p := range checkByIDBaseline(rows, got, sites) {
		t.Errorf("%s: %s", byIDRawStoreBaseline, p)
	}
}

// TestByIDBaselineRegenerationNeverLaundersARow proves regeneration cannot
// document a row: a grown key and a new key land in the UNDOCUMENTED block,
// and a second regeneration leaves them there, so the check still fails.
func TestByIDBaselineRegenerationNeverLaundersARow(t *testing.T) {
	golden := []string{
		"# header",
		"",
		"## documented reason",
		"a.go\tf\tbyid\t1",
		"a.go\tg\tvia\t2",
		"a.go\tgone\tbyid\t1",
	}
	got := map[string]int{
		"a.go\tf\tbyid": 2, // grew
		"a.go\tg\tvia":  1, // fell
		"b.go\th\tbyid": 1, // new
	}
	once := regenByIDBaseline(golden, got)
	twice := regenByIDBaseline(once, got)
	if !slices.Equal(once, twice) {
		t.Errorf("regeneration is not idempotent:\nonce:\n%s\ntwice:\n%s", strings.Join(once, "\n"), strings.Join(twice, "\n"))
	}
	rows, err := parseByIDBaseline(twice)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]string{}
	for _, r := range rows {
		groups[r.key] = r.group
	}
	if g := groups["a.go\tf\tbyid"]; g != byIDUndocumentedGroup {
		t.Errorf("the grown row sits under %q, want the UNDOCUMENTED block", g)
	}
	if g := groups["b.go\th\tbyid"]; g != byIDUndocumentedGroup {
		t.Errorf("the new row sits under %q, want the UNDOCUMENTED block", g)
	}
	if g := groups["a.go\tg\tvia"]; g != "documented reason" {
		t.Errorf("the lowered row moved to %q; a fall keeps its reason", g)
	}
	if _, ok := groups["a.go\tgone\tbyid"]; ok {
		t.Error("a row reaching nothing survived regeneration")
	}
	problems := checkByIDBaseline(rows, got, nil)
	undocumented := 0
	for _, p := range problems {
		if strings.Contains(p, "is undocumented") {
			undocumented++
		}
	}
	if undocumented != 2 {
		t.Fatalf("after two regenerations the check reports %d undocumented row(s), want 2: %v", undocumented, problems)
	}
}

// byIDScanSnippet runs the guard over one in-memory source file.
func byIDScanSnippet(t *testing.T, src string) *byIDScanner {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse snippet: %v", err)
	}
	return newByIDScanner(fset, map[string]*ast.File{"snippet.go": f})
}

// TestByIDRawStoreGuardBites proves the guard flags the shapes it exists for
// and leaves the routed spellings alone.
func TestByIDRawStoreGuardBites(t *testing.T) {
	const src = `package main

func directGet(id string) {
	store, _ := openCityStore(nil, "gc x")
	store.Get(id)
}

func relabel() {
	raw, _ := openCityStore(nil, "gc x")
	_ = beads.SessionStore{Store: raw}
}

func frontDoor() {
	raw, _ := openCityStoreAt("/city")
	_ = sessionFrontDoor(raw)
}

func helper(store beads.Store, id string) {
	_ = store.SetMetadata(id, "k", "v")
}

func viaHelper(id string) {
	raw, _ := openCityStore(nil, "gc x")
	helper(raw, id)
}

func accessor(cs *controllerState, id string) {
	_, _ = cs.CityBeadStore().Get(id)
}

func rangeValue(cs *controllerState, id string) {
	for _, rig := range cs.beadStores {
		_, _ = rig.Get(id)
	}
}

func resultStore(id string) {
	res, _ := openCityStoreResultAt("/city")
	store := res.Store
	_ = store.Close(id)
}

func aliasOpener(id string) {
	store, _ := openAliasedStore("/city")
	_, _ = store.Get(id)
}

var openAliasedStore = openCityStoreAt

func sessionRouted(id string) {
	raw, _ := openCityStore(nil, "gc x")
	_, _ = cliSessionStore(raw, nil, "/city").Get(id)
	sessions := cliSessionStore(raw, nil, "/city")
	_, _ = sessions.Get(id)
}

func classRouted(id string) {
	raw, _ := openCityStore(nil, "gc x")
	_, _ = classRoutedStoreForID(raw, id).Get(id)
	routed := classRoutedStoreForID(raw, id)
	_, _ = routed.Get(id)
}

func handleClose() {
	raw, _ := openCityStore(nil, "gc x")
	_ = raw.Close()
}

func shadowedByClosure(id string) {
	store, _ := openCityStore(nil, "gc x")
	_ = store.Close()
	read := func(store beads.Store) { _, _ = store.Get(id) }
	read(nil)
}

func derivedHelper() beads.Store {
	store, _ := openCityStoreAt("/city")
	return store
}

func viaDerived(id string) {
	_, _ = derivedHelper().Get(id)
}

func qualifiedSink(id string) {
	raw, _ := openCityStoreAt("/city")
	_, _ = beads.ApplyMetadataCAS(raw, id, "k", "a", "b")
}

func markedHelper(store beads.Store, id string) {
	_, _ = store.Get(id) // byid:allow marked inside the helper
}

func callerOfMarkedHelper(id string) {
	raw, _ := openCityStoreAt("/city")
	markedHelper(raw, id)
}

func otherMarker(id string) {
	raw, _ := openCityStoreAt("/city")
	_, _ = raw.Get(id) // residency:allow a different guard's marker
}

func multiLineMarked(id string) {
	raw, _ := openCityStoreAt("/city")
	_ = raw.
		SetMetadata(id, "k", "v") // byid:allow anchored on the selector line
}

func pairOpener() (string, beads.Store) {
	store, _ := openCityStoreAt("/city")
	return "dir", store
}

func openSlingStore() (storeDir string, store beads.Store, errCode, errMsg string) {
	return "", nil, "", ""
}

func positionOneDerived(id string) {
	dir, store := pairOpener()
	_ = dir
	_, _ = store.Get(id)
}

func positionOneListed(id string) {
	storeDir, store, _, _ := openSlingStore()
	_ = storeDir
	_, _ = store.Get(id)
}

func positionOneVar(id string) {
	var dir, store = pairOpener()
	_ = dir
	_, _ = store.Get(id)
}

func positionZeroIsNotRaw(id string) {
	storeDir, _, _, _ := openSlingStore()
	_, _ = storeDir.Get(id) // position 0 is the directory, not the store
}

func staleMarker() {
	_ = 1 // byid:allow suppresses nothing
}

func allowed(id string) {
	raw, _ := openCityStore(nil, "gc x")
	_, _ = raw.Get(id) // byid:allow work bead by construction
}
`
	s := byIDScanSnippet(t, src)
	got := map[string]string{}
	for _, h := range s.hits() {
		got[h.fn] = h.rule
	}
	want := map[string]string{
		"directGet":            "byid",
		"relabel":              "relabel",
		"frontDoor":            "frontdoor",
		"viaHelper":            "via",
		"accessor":             "byid",
		"rangeValue":           "byid",
		"resultStore":          "byid",
		"aliasOpener":          "byid",
		"viaDerived":           "byid",
		"qualifiedSink":        "via",
		"callerOfMarkedHelper": "via",
		"otherMarker":          "byid",
		"positionOneDerived":   "byid",
		"positionOneListed":    "byid",
		"positionOneVar":       "byid",
	}
	for fn, rule := range want {
		if got[fn] != rule {
			t.Errorf("%s: flagged as %q, want %q", fn, got[fn], rule)
		}
	}
	for _, fn := range []string{"sessionRouted", "classRouted", "handleClose", "shadowedByClosure", "allowed", "multiLineMarked", "helper", "markedHelper", "derivedHelper", "positionZeroIsNotRaw", "staleMarker", "pairOpener"} {
		if rule, ok := got[fn]; ok {
			t.Errorf("%s: flagged as %q; a routed store, a handle Close, a closure parameter shadowing a raw name, a byid:allow line (on the selector's line) and a helper's own parameter are not hits", fn, rule)
		}
	}
	if len(got) != len(want) {
		t.Errorf("flagged %v, want exactly %v", got, want)
	}
	// A marker inside a helper suppresses nothing (the helper's parameter is
	// not a hit), and neither does one on an unflagged line: both are reported.
	if unused := s.unusedMarkers(); len(unused) != 2 {
		t.Errorf("unused markers = %v, want the helper's and staleMarker's", unused)
	}
	if len(s.unclassified) != 0 {
		t.Errorf("unclassified openers in the snippet: %v", s.unclassified)
	}
}

// TestByIDRawStoreGuardSelfCheckCatchesNewOpener proves a new open*Store*
// function or opener var cannot slip past the classification.
func TestByIDRawStoreGuardSelfCheckCatchesNewOpener(t *testing.T) {
	const src = `package main

func openShinyNewStore(path string) (beads.Store, error) { return nil, nil }

func openStoreLabels(path string) []string { return nil }

var openShinySeamStore = func(path string) (beads.Store, error) { return nil, nil }

var openRawAliasStore = openCityStoreAt

func openDerivedRawStore(path string) (beads.Store, error) {
	store, err := openCityStoreAt(path)
	return store, err
}
`
	s := byIDScanSnippet(t, src)
	want := []string{"snippet.go: func openShinyNewStore", "var openShinySeamStore"}
	if !slices.Equal(s.unclassified, want) {
		t.Fatalf("unclassified = %v, want %v (a non-store result, an alias of a raw opener and a function returning a raw store are classified)", s.unclassified, want)
	}
}
