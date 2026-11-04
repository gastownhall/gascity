package main

import (
	"bufio"
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
	"openRigStore", "openRigAwareStore", "openSlingCityStore", "openWispStepStore",
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
}

// byIDRawQualified are the raw openers outside cmd/gc, by qualified name.
var byIDRawQualified = []string{"beads.NewBdStore"}

// byIDRawAccessors are method calls that return a raw work or rig store (or a
// map of them): the controller's and city runtime's work-store accessors.
// Taking .Store off one is raw too.
var byIDRawAccessors = []string{"CityBeadStore", "cityBeadStore", "BeadStore", "BeadStores", "rigBeadStores", "Work", "cityWorkStore"}

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
	"openSlingStore":                       "consults the binding residence (slingResidenceBinding) for a source bead it holds",
	"openConvoyStoreByID":                  "resolves the convoy's store by id (resolveConvoyStore); convoys are work class",
	"openConvoyStoreByIDAt":                "resolves the convoy's store by id (resolveConvoyStore); convoys are work class",
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

// byIDFrontDoors are the constructors that turn a store into a class front door;
// handing one a raw work store relabels it without routing it.
var byIDFrontDoors = []string{"sessionFrontDoor", "nudgeFrontDoor", "session.NewStore", "orders.NewStore", "nudgequeue.NewStore"}

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
	allow    map[string]map[int]bool // file -> lines carrying residency:allow
	raw      map[string]bool         // raw opener names, aliases resolved
	routed   map[string]bool
	sanitize map[string]bool
	summary  map[string]map[int]bool // func key -> beads.Store param indexes reaching a sink
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
				if strings.Contains(c.Text, "residency:allow") {
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
			case *ast.GenDecl:
				ast.Inspect(d, func(n ast.Node) bool {
					spec, ok := n.(*ast.ValueSpec)
					if !ok {
						return true
					}
					for i, id := range spec.Names {
						if i < len(spec.Values) && byIDOpenerName.MatchString(id.Name) {
							vars = append(vars, openerVar{id.Name, spec.Values[i]})
						}
					}
					return false
				})
			}
		}
	}
	// An opener var aliasing a classified function inherits its class.
	for changed := true; changed; {
		changed = false
		for _, v := range vars {
			id, ok := v.value.(*ast.Ident)
			if !ok || s.raw[v.name] || s.routed[v.name] {
				continue
			}
			switch {
			case s.raw[id.Name]:
				s.raw[v.name] = true
				changed = true
			case s.routed[id.Name]:
				s.routed[v.name] = true
				s.sanitize[v.name] = true
				changed = true
			}
		}
	}
	seen := map[string]bool{}
	for _, fn := range s.funcs {
		if !byIDOpenerName.MatchString(fn.name) || !byIDReturnsStore(fn.decl.Type) || seen[fn.name] {
			continue
		}
		seen[fn.name] = true
		if !s.raw[fn.name] && !s.routed[fn.name] {
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
	slices.Sort(s.unclassified)
	s.summarize()
	return s
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
		if _, ok := x.Fun.(*ast.Ident); ok && s.raw[name] {
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
				for i, lhs := range st.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok {
						continue
					}
					var rhs ast.Expr
					switch {
					case len(st.Rhs) == len(st.Lhs):
						rhs = st.Rhs[i]
					case len(st.Rhs) == 1 && i == 0:
						rhs = st.Rhs[0]
					}
					if rhs == nil {
						continue
					}
					if s.isSanitized(rhs) {
						sanitized[id.Name] = true
					} else if s.isRawSource(rhs, raw) {
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
func (s *byIDScanner) sinks(file string, body *ast.BlockStmt, raw map[string]bool) []byIDHit {
	var out []byIDHit
	add := func(rule, detail string, n ast.Node) {
		line := s.fset.Position(n.Pos()).Line
		if s.allow[file][line] {
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
				out = append(out, s.sinks(file, x.Body, inner)...)
				return false
			}
		case *ast.CallExpr:
			name, qual := byIDCallee(x)
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && slices.Contains(byIDMethods, name) &&
				(name != "Close" || len(x.Args) != 0) && s.isRawSource(sel.X, raw) {
				add("byid", name+" on "+byIDExprString(sel.X), x)
			}
			if slices.Contains(byIDFrontDoors, qual) || slices.Contains(byIDFrontDoors, name) {
				for _, a := range x.Args {
					if s.isRawSource(a, raw) {
						add("frontdoor", qual+"("+byIDIdent(a)+")", x)
					}
				}
			}
			if _, ok := x.Fun.(*ast.Ident); ok && !s.sanitize[name] {
				for _, cand := range s.byName[name] {
					if cand.decl.Recv != nil {
						continue
					}
					for idx := range s.summary[cand.key] {
						if idx < len(x.Args) && s.isRawSource(x.Args[idx], raw) {
							add("via", name+"("+byIDIdent(x.Args[idx])+")", x)
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
						add("relabel", "beads."+sel.Sel.Name+"{Store: "+byIDIdent(kv.Value)+"}", x)
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
				if len(s.sinks(fn.file, fn.decl.Body, raw)) == 0 {
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

// hits returns every flagged site in the package.
func (s *byIDScanner) hits() []byIDHit {
	var out []byIDHit
	for _, fn := range s.funcs {
		raw := s.rawIdents(fn.decl.Body, nil)
		for _, h := range s.sinks(fn.file, fn.decl.Body, raw) {
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

func readByIDBaseline(t *testing.T, path string) (rows []byIDBaselineRow, lines []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	group := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		lines = append(lines, line)
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
			t.Fatalf("%s: malformed row %q: want file<TAB>function<TAB>rule<TAB>count", path, line)
		}
		n, err := strconv.Atoi(parts[3])
		if err != nil || n <= 0 {
			t.Fatalf("%s: row %q: count must be a positive integer", path, line)
		}
		rows = append(rows, byIDBaselineRow{key: strings.Join(parts[:3], "\t"), group: group, count: n})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows, lines
}

// rewriteByIDBaseline updates counts in place, drops stale rows and files new
// keys under the UNDOCUMENTED group.
func rewriteByIDBaseline(t *testing.T, path string, lines []string, got map[string]int) {
	t.Helper()
	var out []string
	kept := map[string]bool{}
	for _, line := range lines {
		parts := strings.Split(line, "\t")
		trimmed := strings.TrimSpace(line)
		if len(parts) != 4 || strings.HasPrefix(trimmed, "#") {
			if trimmed == "## "+byIDUndocumentedGroup {
				continue
			}
			out = append(out, line)
			continue
		}
		key := strings.Join(parts[:3], "\t")
		if got[key] == 0 {
			continue
		}
		kept[key] = true
		out = append(out, fmt.Sprintf("%s\t%d", key, got[key]))
	}
	var fresh []string
	for key, n := range got {
		if !kept[key] {
			fresh = append(fresh, fmt.Sprintf("%s\t%d", key, n))
		}
	}
	if len(fresh) > 0 {
		slices.Sort(fresh)
		out = append(out, "", "## "+byIDUndocumentedGroup)
		out = append(out, fresh...)
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
// assigned from a raw opener (byIDRawOpeners and their package-var aliases), a
// raw accessor or field, .Store off one, or a range value over one; it is
// cleared when assigned from a class/residency helper (byIDSanitizers,
// byIDRoutedOpeners). Flagged: a by-id method on a raw store (rule byid), a raw
// store handed to a front-door constructor (frontdoor) or relabeled as a class
// handle (relabel), and a raw store passed to a package function whose
// beads.Store parameter reaches any of those (via).
//
// The counts ratchet against byIDRawStoreBaseline: growth fails, and so does a
// row the tree no longer reaches. A self-check fails any open*Store* function
// or opener var returning a store that is classified neither raw nor routed.
func TestByIDOpsDoNotTargetRawWorkStores(t *testing.T) {
	fset, files := parseByIDPackage(t, ".")
	s := newByIDScanner(fset, files)
	for _, u := range s.unclassified {
		t.Errorf("%s returns a store but is classified neither raw (byIDRawOpeners) nor routed (byIDRoutedOpeners) in byid_raw_store_guard_test.go; classify it so a by-id op on what it returns is policed", u)
	}
	hits := s.hits()
	got := map[string]int{}
	sites := map[string][]string{}
	for _, h := range hits {
		got[h.key()]++
		sites[h.key()] = append(sites[h.key()], fmt.Sprintf("%s:%d %s", h.file, h.line, h.detail))
	}
	if len(hits) == 0 || len(s.funcs) < 1000 {
		t.Fatalf("flagged %d sites over %d functions; the scan missed the package", len(hits), len(s.funcs))
	}
	rows, lines := readByIDBaseline(t, byIDRawStoreBaseline)
	if os.Getenv(byIDRawStoreUpdateEnv) != "" {
		rewriteByIDBaseline(t, byIDRawStoreBaseline, lines, got)
		t.Logf("rewrote %s; document every row under an UNDOCUMENTED header before committing", byIDRawStoreBaseline)
		return
	}
	want := map[string]int{}
	for _, r := range rows {
		if _, dup := want[r.key]; dup {
			t.Errorf("%s: duplicate row %q", byIDRawStoreBaseline, r.key)
		}
		want[r.key] = r.count
		if r.group == "" || r.group == byIDUndocumentedGroup {
			t.Errorf("%s: row %q has no reason: move it under a \"## <reason>\" header, or fix the site", byIDRawStoreBaseline, r.key)
		}
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if got[k] > want[k] {
			t.Errorf("NEW by-id op on a raw work store: %s: %d site(s), baseline %d:\n\t%s\n"+
				"Route it through cliByIDOwner / classRoutedStoreForID / the class resolvers (cliSessionStore, cliStores(...).X(), resolve*Store),"+
				" or, if the bead is work-class by construction, add a trailing `// residency:allow <reason>` on the line.",
				strings.ReplaceAll(k, "\t", " "), got[k], want[k], strings.Join(sites[k], "\n\t"))
		}
	}
	for _, r := range rows {
		if got[r.key] < r.count {
			t.Errorf("%s: %s: %d site(s), below its baseline %d: lower the row (or remove it) in the same change (%s=1 rewrites counts)",
				byIDRawStoreBaseline, strings.ReplaceAll(r.key, "\t", " "), got[r.key], r.count, byIDRawStoreUpdateEnv)
		}
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

func allowed(id string) {
	raw, _ := openCityStore(nil, "gc x")
	_, _ = raw.Get(id) // residency:allow work bead by construction
}
`
	s := byIDScanSnippet(t, src)
	got := map[string]string{}
	for _, h := range s.hits() {
		got[h.fn] = h.rule
	}
	want := map[string]string{
		"directGet":   "byid",
		"relabel":     "relabel",
		"frontDoor":   "frontdoor",
		"viaHelper":   "via",
		"accessor":    "byid",
		"rangeValue":  "byid",
		"resultStore": "byid",
		"aliasOpener": "byid",
	}
	for fn, rule := range want {
		if got[fn] != rule {
			t.Errorf("%s: flagged as %q, want %q", fn, got[fn], rule)
		}
	}
	for _, fn := range []string{"sessionRouted", "classRouted", "handleClose", "shadowedByClosure", "allowed", "helper"} {
		if rule, ok := got[fn]; ok {
			t.Errorf("%s: flagged as %q; a routed store, a handle Close, a closure parameter shadowing a raw name, a residency:allow line and a helper's own parameter are not hits", fn, rule)
		}
	}
	if len(got) != len(want) {
		t.Errorf("flagged %v, want exactly %v", got, want)
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
`
	s := byIDScanSnippet(t, src)
	want := []string{"snippet.go: func openShinyNewStore", "var openShinySeamStore"}
	if !slices.Equal(s.unclassified, want) {
		t.Fatalf("unclassified = %v, want %v (a non-store result and an alias of a raw opener are classified)", s.unclassified, want)
	}
}
