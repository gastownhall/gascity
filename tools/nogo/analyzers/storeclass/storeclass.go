// Package storeclass is the typed-store lint (typed-stores design §3.8, phase
// T1). A coordination class's store reaches code as a typed class handle
// (beads.WorkStore, GraphStore, SessionStore, MailStore, OrdersStore,
// NudgesStore), and only the residency layer decides which physical store a
// handle wraps. Outside the allowlisted packages it reports, by type rather
// than by name:
//
//   - raw-param, raw-result, raw-field, raw-var, raw-type: a parameter,
//     result, struct field, package variable or declared type whose type is
//     or holds a raw beads.Store (through a pointer, slice, array, map,
//     channel, function signature or type argument; an interface embedding
//     it);
//   - mint: a typed handle built outside the mint, by a composite literal
//     with fields or a conversion, and any reference to a mint function;
//   - unwrap: a handle's embedded store read back (.Store, through any
//     embedding) or an unwrap method (Raw) named;
//   - cap-assert: a type assertion or type switch on a store value (a
//     beads.Store, an interface that is one, or a store, concrete store or
//     handle boxed by a conversion): use beads.As or an XFor helper, which
//     see through the handle;
//   - test-mint-import: production code importing a test-only minting
//     package;
//   - read-marker: a federated or relic read marker named (D9 freezes them:
//     a new federation is a storeref intent change).
//
// A raw store is beads.Store itself, an interface that is one, or a concrete
// store type (an engine such as *beads.CachingStore, or a wrapper); a struct
// that is a store only through an embedded typed handle is not. A type
// parameter constrained by a raw store, or by a union or ~ term over one, is
// a raw-type finding, and so is an interface element that holds one. As[T]
// with a store for T is an unwrap, and so is ConditionalWritesResolveTarget.
//
// Out of scope, by design: assertions laundered through a value that is no
// longer typed as a store (var x any = s; x.(T)), through reflect, or through
// a helper outside the linted packages. A conversion that boxes a store at the
// assertion (any(s).(T)) is caught. The runtime class guard (T8) is the
// backstop for what a type check cannot see. Also out of scope: debt moved
// into a package the allowlist exempts whole (internal/beads, the
// internal/storebinding packages, internal/storeref). Such a move changes
// that package's API, which its review sees, but no count does.
//
// Every finding counts against a generated baseline keyed by file, enclosing
// function and rule (baseline.txt). A count above its pin reports every site
// of that key; a pin the tree no longer reaches reports too, so the baseline
// shrinks in the commit that retires a site. A package compiled under several
// build-tag sets is checked in each, against the rows for the files that set
// compiles. A Sealed package has no baseline at all.
//
// The allowlist exempts a package entry's findings outright, and names each
// package: a package added below an allowlisted one is linted until an entry
// names it. An entry for one file (cmd/gc's storage_boot.go and
// infra_class_*.go) exempts its findings from nogo only. The generator counts
// them in allowed.txt, by the baseline's key, and the growth gate judges
// allowed.txt like the baseline, so new raw-store code in such a file, or
// debt moved into it, is a new or raised allowed.txt row that needs the
// label.
//
// Test files are not linted. Generated files are: a _gen.go name or a
// generated-code header does not take a file the compiler builds out of the
// lint, or its rows out of the baseline (tools/nogo/config.json gives
// storeclass its own scope, _base's without the _gen.go exclusion). A
// //nolint:storeclass directive is itself a finding: the baseline is the
// only exemption.
//
// # Raising the baseline
//
// New raw-store debt is refused, not baselined. A change that cannot avoid
// it (a slice that lands before the router or the retyping phase for its
// package: S3b, S4a and S7 are expected to) raises the baseline this way:
//
//  1. Regenerate: from tools/nogo, go run ./analyzers/storeclass/cmd/storeclass-baseline -write.
//     It refuses baseline growth, so raise ceiling.txt by hand to the totals
//     it reports, and rerun. allowed.txt has no ceiling: -write rewrites it,
//     and the gate judges its rows.
//  2. List the new rows and why each cannot use a typed handle yet in the
//     pull request description.
//  3. The typed-store programme's coordinator reviews the rows and applies
//     the storeclass/baseline-raise label. Without it the storeclass
//     baseline workflow fails on any new or raised baseline or allowed.txt
//     row or ceiling against the merge-base. CODEOWNERS (gascity-admin)
//     gates this directory, tools/nogo/analyzers.bzl, tools/nogo/BUILD.bazel
//     and the two rules_go nogo patches.
//
// The same label approves a change that could weaken the lint rather than
// raise its baseline: to this directory, the analyzer set and nogo target, the
// two rules_go nogo patches, or the nogo wiring in files the gate does not
// protect whole (MODULE.bazel's rules_go override and go_sdk.nogo
// registration, tools/nogo/config.json's _base and storeclass scopes; see
// cmd/storeclass-baseline).
//
// Shrinking needs no label: regenerate with -write, which lowers the
// ceiling, in the commit that retires the sites. A shrink still edits
// baseline.txt and ceiling.txt (or allowed.txt), so once code-owner review is
// enforced on main (an admin setting #8435 asks for), it needs a
// gascity-admin review too.
//
// # What row-level strictness costs
//
// The growth gate is strict per row on purpose: raw-store debt in a function
// that had none is new debt, even when its package's total holds. Every
// change to raw-store code pays for that, not only a raise:
//
//   - Concurrent shrinks collide. Two changes that retire sites of one
//     package and rule each lower the same ceiling.txt line. Different
//     amounts conflict; equal amounts merge cleanly to a ceiling above the
//     tree, which TestBaselineOnlyShrinks and the generator's check refuse,
//     so the merge queue ejects the later change: it rebases and reruns
//     -write.
//   - A rename, a move or a split of a function or file that holds debt is a
//     new row, so the growth gate counts it as a raise that needs the label,
//     though no package's total grows. A move into an allowlisted file is a
//     new allowed.txt row, so it needs the label too.
//   - Every shrink edits baseline.txt and ceiling.txt, which CODEOWNERS
//     assigns to gascity-admin (above).
//   - baseline.txt is an input of every main-repository nogo action, so any
//     edit to it re-runs every analyzer over every gascity package.
//
// Judging package/rule totals against the merge-base's baseline, with no
// committed ceiling, would remove the first two costs, and a per-package
// baseline the last; revisit both before the phases that retire debt (T2
// onward) make shrinks common.
//
// The gate protects tools/nogo/analyzers.bzl and tools/nogo/BUILD.bazel
// whole, so a change that cannot weaken storeclass still needs the label
// there: adding another analyzer, or a staticcheck bump that edits the check
// lists. Guarding only the storeclass wiring in those two files (storeclass
// in WRAPPED, WRAPPED in NOGO_ANALYZERS, the nogo target and main_repo_data),
// as MODULE.bazel and config.json are guarded, would remove that cost.
//
// # The read markers have two ledgers
//
// The split-strict gate (#8335) reviews every production use of a read marker
// in its own table, reviewedMarkers in internal/beads/splittest
// (TestReadMarkersAreReviewed), by file and function, internal/storeref's
// uses included. read-marker pins the uses outside internal/storeref a second
// time, as baseline rows under this gate's approval. Until T7 replaces the
// markers with plan roles, a change that adds, moves or removes a marker
// updates both: its reviewedMarkers entry, and the baseline (regenerate).
package storeclass

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/analysis"
)

// Rule names one kind of finding; it is a baseline row's third column.
type Rule string

// The rules.
const (
	RawParam       Rule = "raw-param"
	RawResult      Rule = "raw-result"
	RawField       Rule = "raw-field"
	RawVar         Rule = "raw-var"
	RawType        Rule = "raw-type"
	Mint           Rule = "mint"
	Unwrap         Rule = "unwrap"
	CapAssert      Rule = "cap-assert"
	TestMintImport Rule = "test-mint-import"
	ReadMarker     Rule = "read-marker"
)

// Rules is every rule, in reporting order.
var Rules = []Rule{RawParam, RawResult, RawField, RawVar, RawType, Mint, Unwrap, CapAssert, TestMintImport, ReadMarker}

// AllRulesBut is every rule except rs, for an Allow entry.
func AllRulesBut(rs ...Rule) []Rule {
	var out []Rule
	for _, r := range Rules {
		if !slices.Contains(rs, r) {
			out = append(out, r)
		}
	}
	return out
}

// Allow exempts a package (a module-relative path, exactly: a subpackage
// needs its own entry, so a new package is linted until the allowlist names
// it), or one file of it (File, a base name or a path.Match glob), from Rules
// (nil: every rule), for a reviewed Reason. A package entry exempts its
// findings outright. A File entry exempts them from nogo only: the generator
// still counts them in allowed.txt, and the growth gate judges that file
// like the baseline, so debt moved into an allowlisted file is a raise. A
// File entry that matches no file of its package is stale and reports, and
// the generator refuses an entry whose package the module does not have.
type Allow struct {
	Package string
	File    string
	Rules   []Rule
	Reason  string
}

// Config is what the lint guards, by fully qualified name ("importpath.Name").
type Config struct {
	Module           string   // module path prefix with a trailing "/": only its packages are linted
	Store            string   // the raw store interface
	Handles          []string // the typed class handle types
	HandleField      string   // the handles' embedded store field
	UnwrapMethods    []string // handle methods that return the raw store
	AsFuncs          []string // capability helpers (beads.As): an instance whose T is a store unwraps
	MintFuncs        []string // functions that mint handles
	TestMintPackages []string // packages production code may not import
	Markers          []string // federated/relic read marker functions
	Allow            []Allow
	Sealed           []string    // module-relative packages with no baseline
	Baseline         map[Key]int // the pinned counts, or nil to load them
	// LoadBaseline, when Baseline is nil, loads the pinned counts the first
	// time a module package is checked.
	LoadBaseline func() (map[Key]int, error)
}

// Finding is one site: its baseline key, where it is, and what it is.
// Allowed marks a site an Allow entry for its file exempts: nogo does not
// report it, and the generator counts it in allowed.txt, not the baseline.
type Finding struct {
	Key     Key
	Pos     token.Pos
	Desc    string
	Allowed bool
}

// New builds the analyzer for cfg.
func New(cfg Config) *analysis.Analyzer {
	var once sync.Once
	var loaded map[Key]int
	var loadErr error
	return &analysis.Analyzer{
		Name: "storeclass",
		Doc:  "reports raw beads.Store signatures, handle mints, unwraps and capability assertions outside the residency layer, against a shrink-only baseline (typed-stores T1)",
		Run: func(pass *analysis.Pass) (any, error) {
			c := cfg
			if _, ok := relPackage(c, pass.Pkg.Path()); ok && c.Baseline == nil && c.LoadBaseline != nil {
				once.Do(func() { loaded, loadErr = c.LoadBaseline() })
				if loadErr != nil {
					return nil, loadErr
				}
				c.Baseline = loaded
			}
			return nil, run(pass, c, nil)
		},
	}
}

// NewCollector builds an analyzer that hands every finding to collect,
// unfiltered by the baseline and marked Allowed where an Allow entry for its
// file exempts it, and reports nothing: the baseline generator. collect may
// be called from several goroutines' passes; it is serialized.
func NewCollector(cfg Config, collect func(Finding)) *analysis.Analyzer {
	var mu sync.Mutex
	serial := func(f Finding) {
		mu.Lock()
		defer mu.Unlock()
		collect(f)
	}
	return &analysis.Analyzer{
		Name: "storeclass",
		Doc:  "collects storeclass findings for the baseline generator",
		Run:  func(pass *analysis.Pass) (any, error) { return nil, run(pass, cfg, serial) },
	}
}

// relPackage is pkgPath relative to the module, "." for its root, and
// whether it is in the module at all.
func relPackage(cfg Config, pkgPath string) (string, bool) {
	root := strings.TrimSuffix(cfg.Module, "/")
	switch {
	case pkgPath == root:
		return ".", true
	case strings.HasPrefix(pkgPath, cfg.Module):
		return strings.TrimPrefix(pkgPath, cfg.Module), true
	}
	return "", false
}

func fileMatches(a Allow, base string) bool {
	if a.File == "" {
		return true
	}
	ok, err := path.Match(a.File, base)
	return err == nil && ok
}

// exemption is what the allowlist does with one finding.
type exemption int

const (
	notExempt     exemption = iota
	exemptCounted           // an entry for its file: nogo skips it, allowed.txt counts it
	exemptWhole             // an entry for its package: nothing counts it
)

func (c Config) exempt(rel, base string, r Rule) exemption {
	e := notExempt
	for _, a := range c.Allow {
		if a.Package != rel || !fileMatches(a, base) || a.Rules != nil && !slices.Contains(a.Rules, r) {
			continue
		}
		if a.File == "" {
			return exemptWhole
		}
		e = exemptCounted
	}
	return e
}

// linted reports whether a file of the pass is linted: any file but a test
// file. A generated one is linted too: were a _gen.go name or a generated-code
// header to skip a file, renaming a baselined file or adding the header would
// drop its rows from the census, a shrink that needs no label, and hide every
// later raw store in it.
func linted(filename string) bool {
	return !strings.HasSuffix(path.Base(strings.ReplaceAll(filename, "\\", "/")), "_test.go")
}

func run(pass *analysis.Pass, cfg Config, collect func(Finding)) error {
	rel, ok := relPackage(cfg, pass.Pkg.Path())
	if !ok {
		return nil
	}
	c := &checker{cfg: cfg, pass: pass, rel: rel}
	c.storeIface = c.findStoreInterface()
	var first *ast.File
	var bases []string
	for _, file := range pass.Files {
		// The file's own name: a //line directive renames the positions after
		// it, and would otherwise make it a test file or another file's rows.
		name := pass.Fset.PositionFor(file.Pos(), false).Filename
		if !linted(name) {
			continue
		}
		if first == nil {
			first = file
		}
		base := path.Base(strings.ReplaceAll(name, "\\", "/"))
		bases = append(bases, base)
		c.file(file, base)
	}
	if first == nil {
		return nil // a test-only pass
	}
	if collect != nil {
		for _, f := range slices.Concat(c.found, c.allowed) {
			collect(f)
		}
		return nil
	}
	c.report(first, bases)
	return nil
}

type checker struct {
	cfg        Config
	pass       *analysis.Pass
	rel        string
	storeIface *types.Interface
	found      []Finding // judged against the baseline
	allowed    []Finding // exempt by an entry for their file: only the generator sees them
	nolint     []token.Pos
	concrete   map[*types.Named]bool
}

// report checks the pass's findings against the baseline.
func (c *checker) report(first *ast.File, bases []string) {
	sealed := slices.Contains(c.cfg.Sealed, c.rel)
	byKey := map[Key][]Finding{}
	var keys []Key
	for _, f := range c.found {
		if byKey[f.Key] == nil {
			keys = append(keys, f.Key)
		}
		byKey[f.Key] = append(byKey[f.Key], f)
	}
	sortKeys(keys)
	for _, k := range keys {
		pinned := c.cfg.Baseline[k]
		if sealed {
			pinned = 0
		}
		fs := byKey[k]
		if len(fs) <= pinned {
			continue
		}
		for _, f := range fs {
			c.pass.Reportf(f.Pos, "storeclass[%s] %s (%s %s: %d here, baseline %d)", k.Rule, f.Desc, k.Path, k.Func, len(fs), pinned)
		}
	}
	var stale []string
	for k, pinned := range c.cfg.Baseline {
		// A row for a file this pass does not compile is another build-tag
		// variant's (nogo analyzes a package once per configuration that
		// compiles it), or a deleted file's, which the generator's check
		// reports: it is not this pass's to judge.
		if k.Package() != c.rel || !slices.Contains(bases, path.Base(k.Path)) {
			continue
		}
		switch have := len(byKey[k]); {
		case sealed:
			stale = append(stale, fmt.Sprintf("storeclass[baseline] %s is sealed: drop its baseline row %s", c.rel, k))
		case have < pinned:
			stale = append(stale, fmt.Sprintf("storeclass[baseline] %s pins %d, the tree has %d: regenerate the baseline (it may only shrink)", k, pinned, have))
		}
	}
	for _, a := range c.cfg.Allow {
		if a.File == "" || a.Package != c.rel {
			continue
		}
		if !slices.ContainsFunc(bases, func(b string) bool { return fileMatches(a, b) }) {
			stale = append(stale, fmt.Sprintf("storeclass[allowlist] allowlisted file %s/%s names nothing: drop it", c.rel, a.File))
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		c.pass.Report(analysis.Diagnostic{Pos: first.Package, Message: s})
	}
	for _, pos := range c.nolint {
		c.pass.Reportf(c.fileOf(pos).Package, "storeclass[nolint] a //nolint:storeclass directive at %s: the baseline is the only exemption", c.pass.Fset.Position(pos))
	}
}

func (c *checker) fileOf(pos token.Pos) *ast.File {
	for _, f := range c.pass.Files {
		if f.FileStart <= pos && pos <= f.FileEnd {
			return f
		}
	}
	return c.pass.Files[0]
}

func (c *checker) add(base, fn string, r Rule, pos token.Pos, desc string) {
	ex := c.cfg.exempt(c.rel, base, r)
	if ex == exemptWhole {
		return
	}
	p := base
	if c.rel != "." {
		p = c.rel + "/" + base
	}
	f := Finding{Key: Key{Path: p, Func: fn, Rule: r}, Pos: pos, Desc: desc, Allowed: ex == exemptCounted}
	if f.Allowed {
		c.allowed = append(c.allowed, f)
		return
	}
	c.found = append(c.found, f)
}

// file collects one file's findings.
func (c *checker) file(file *ast.File, base string) {
	for _, cg := range file.Comments {
		for _, cm := range cg.List {
			if names, ok := strings.CutPrefix(cm.Text, "//nolint:"); ok {
				names, _, _ = strings.Cut(names, " ")
				if slices.Contains(strings.Split(names, ","), "storeclass") {
					c.nolint = append(c.nolint, cm.Pos())
				}
			}
		}
	}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, "`\"")
		if slices.Contains(c.cfg.TestMintPackages, p) {
			c.add(base, "(import)", TestMintImport, imp.Pos(), "production code imports the test-only package "+p)
		}
	}
	info := c.pass.TypesInfo
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		fn := enclosing(stack)
		switch v := n.(type) {
		case *ast.FuncDecl:
			c.signature(base, fn, v.Type)
		case *ast.FuncLit:
			c.signature(base, fn, v.Type)
		case *ast.StructType:
			for _, f := range v.Fields.List {
				c.fieldList(base, fn, RawField, "field", f)
			}
		case *ast.InterfaceType:
			for _, m := range v.Methods.List {
				if ft, ok := m.Type.(*ast.FuncType); ok {
					c.signature(base, fn, ft)
					continue
				}
				if c.holdsElement(info.TypeOf(m.Type)) {
					c.add(base, fn, RawType, m.Pos(), "an interface embeds or is satisfied by a raw store: take a typed class handle")
				}
			}
		case *ast.TypeSpec:
			c.typeParams(base, fn, v.TypeParams)
			switch v.Type.(type) {
			case *ast.StructType, *ast.InterfaceType:
			default:
				if t := info.TypeOf(v.Type); c.holds(t) {
					c.add(base, fn, RawType, v.Name.Pos(), fmt.Sprintf("type %s is or holds a raw beads.Store (%s): take a typed class handle", v.Name.Name, c.typeString(t)))
				}
			}
		case *ast.ValueSpec:
			if len(stack) >= 3 {
				if _, top := stack[len(stack)-3].(*ast.File); top {
					for _, name := range v.Names {
						if obj := info.Defs[name]; obj != nil && c.holds(obj.Type()) {
							c.add(base, name.Name, RawVar, name.Pos(), fmt.Sprintf("package variable %s is or holds a raw beads.Store (%s): hold a typed class handle", name.Name, c.typeString(obj.Type())))
						}
					}
				}
			}
		case *ast.CompositeLit:
			if len(v.Elts) > 0 && c.isHandle(info.TypeOf(v)) {
				c.add(base, fn, Mint, v.Pos(), fmt.Sprintf("a %s minted by a literal: handles come from the residency layer (storeref), or beadstest in tests", c.typeString(info.TypeOf(v))))
			}
		case *ast.CallExpr:
			if tv, ok := info.Types[v.Fun]; ok && tv.IsType() && c.isHandle(tv.Type) {
				c.add(base, fn, Mint, v.Pos(), fmt.Sprintf("a conversion to %s: a handle is never relabeled to another class", c.typeString(tv.Type)))
			}
		case *ast.Ident:
			if f, ok := info.Uses[v].(*types.Func); ok && f.Pkg() != nil && f.Type().(*types.Signature).Recv() == nil {
				full := f.Pkg().Path() + "." + f.Name()
				switch {
				case slices.Contains(c.cfg.MintFuncs, full):
					c.add(base, fn, Mint, v.Pos(), fmt.Sprintf("%s mints handles: only the residency layer may call it", f.Name()))
				case slices.Contains(c.cfg.Markers, full):
					c.add(base, fn, ReadMarker, v.Pos(), fmt.Sprintf("the read marker %s: markers are frozen (D9); a new federation is a storeref intent change", f.Name()))
				case slices.Contains(c.cfg.AsFuncs, full):
					if inst, ok := info.Instances[v]; ok && inst.TypeArgs.Len() == 1 {
						if ta := inst.TypeArgs.At(0); c.isStoreType(ta) && !c.isHandle(ta) {
							c.add(base, fn, Unwrap, v.Pos(), fmt.Sprintf("%s[%s] extracts a raw store from a handle: ask for a capability, not a store", f.Name(), c.typeString(ta)))
						}
					}
				}
			}
		case *ast.SelectorExpr:
			c.selector(base, fn, v)
		case *ast.TypeAssertExpr:
			if c.isStoreValue(v.X) {
				what := "a type switch"
				if v.Type != nil {
					what = "a type assertion to " + c.typeString(info.TypeOf(v.Type))
				}
				c.add(base, fn, CapAssert, v.Pos(), what+" on a store: use beads.As, which sees through a typed handle")
			}
		}
		return true
	})
}

// signature collects a function type's raw parameters and results.
func (c *checker) signature(base, fn string, ft *ast.FuncType) {
	c.typeParams(base, fn, ft.TypeParams)
	if ft.Params != nil {
		for _, f := range ft.Params.List {
			c.fieldList(base, fn, RawParam, "parameter", f)
		}
	}
	if ft.Results != nil {
		for _, f := range ft.Results.List {
			c.fieldList(base, fn, RawResult, "result", f)
		}
	}
}

// typeParams collects type parameters constrained by a raw store: a store
// type, or a union or ~ term over one ([S *beads.CachingStore | int],
// [S ~*beads.CachingStore], whose type is a *types.Union). A constraint
// written as an interface literal reports where the literal is.
func (c *checker) typeParams(base, fn string, tps *ast.FieldList) {
	if tps == nil {
		return
	}
	for _, f := range tps.List {
		if _, lit := f.Type.(*ast.InterfaceType); lit {
			continue
		}
		t := c.pass.TypesInfo.TypeOf(f.Type)
		if !c.holdsElement(t) {
			continue
		}
		for _, name := range f.Names {
			c.add(base, fn, RawType, name.Pos(), fmt.Sprintf("type parameter %s is constrained by a raw store (%s): take a typed class handle", name.Name, c.typeString(t)))
		}
	}
}

// holdsElement reports whether a constraint, or an interface's embedded
// element, is or holds a raw store: a type, or a union of plain and ~ terms.
func (c *checker) holdsElement(t types.Type) bool {
	if u, ok := t.(*types.Union); ok {
		for i := range u.Len() {
			if c.holds(u.Term(i).Type()) {
				return true
			}
		}
		return false
	}
	return c.holds(t)
}

// fieldList collects one field of a parameter, result or field list: a
// finding per name, or one for an unnamed or embedded field.
func (c *checker) fieldList(base, fn string, r Rule, what string, f *ast.Field) {
	t := c.pass.TypesInfo.TypeOf(f.Type)
	if !c.holds(t) {
		return
	}
	desc := func(name string) string {
		return fmt.Sprintf("%s %s is or holds a raw beads.Store (%s): take a typed class handle", what, name, c.typeString(t))
	}
	if len(f.Names) == 0 {
		c.add(base, fn, r, f.Pos(), desc("(unnamed)"))
		return
	}
	for _, name := range f.Names {
		c.add(base, fn, r, name.Pos(), desc(name.Name))
	}
}

func (c *checker) selector(base, fn string, sel *ast.SelectorExpr) {
	s := c.pass.TypesInfo.Selections[sel]
	if s == nil {
		return
	}
	switch s.Kind() {
	case types.FieldVal:
		if s.Obj().Name() != c.cfg.HandleField {
			return
		}
		owner := s.Recv()
		idx := s.Index()
		for _, i := range idx[:len(idx)-1] {
			st, ok := deref(owner).Underlying().(*types.Struct)
			if !ok {
				return
			}
			owner = st.Field(i).Type()
		}
		if c.isHandle(owner) {
			c.add(base, fn, Unwrap, sel.Sel.Pos(), fmt.Sprintf("%s.%s unwraps a typed handle to its raw store: keep the handle, or use beads.As for a capability", c.typeString(deref(owner)), c.cfg.HandleField))
		}
	case types.MethodVal, types.MethodExpr:
		m, ok := s.Obj().(*types.Func)
		if !ok || !slices.Contains(c.cfg.UnwrapMethods, m.Name()) {
			return
		}
		if recv := m.Type().(*types.Signature).Recv(); recv != nil && c.isHandle(recv.Type()) {
			c.add(base, fn, Unwrap, sel.Sel.Pos(), fmt.Sprintf("%s unwraps a typed handle to its raw store: keep the handle, or use beads.As for a capability", m.Name()))
		}
	}
}

// enclosing is the baseline's function column for the innermost node of
// stack: the function declaration ("Func", "Recv.Method"), else the
// package-level type or variable, else "(package)".
func enclosing(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if fn, ok := stack[i].(*ast.FuncDecl); ok {
			return funcName(fn)
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		switch s := stack[i].(type) {
		case *ast.TypeSpec:
			return s.Name.Name
		case *ast.ValueSpec:
			if len(s.Names) > 0 {
				return s.Names[0].Name
			}
		}
	}
	return "(package)"
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.ParenExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		case *ast.Ident:
			return x.Name + "." + fn.Name.Name
		}
		return fn.Name.Name
	}
}

func deref(t types.Type) types.Type {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return types.Unalias(p.Elem())
	}
	return t
}

func fullName(t types.Type) string {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return ""
	}
	return n.Obj().Pkg().Path() + "." + n.Obj().Name()
}

// isStore reports whether t is the raw store interface itself.
func (c *checker) isStore(t types.Type) bool { return t != nil && fullName(t) == c.cfg.Store }

// isHandle reports whether t, or what it points to, is a typed handle.
func (c *checker) isHandle(t types.Type) bool {
	return t != nil && slices.Contains(c.cfg.Handles, fullName(deref(t)))
}

// holds reports whether t is or holds the raw store through type
// constructors and type arguments. It does not enter other named types
// (their own declarations report) or anonymous structs and interfaces
// (their fields and methods report where they are written).
func (c *checker) holds(t types.Type) bool {
	switch t := types.Unalias(t).(type) {
	case nil:
		return false
	case *types.Named:
		if c.isStore(t) || c.isConcreteStore(t) {
			return true
		}
		for a := range t.TypeArgs().Types() {
			if c.holds(a) {
				return true
			}
		}
	case *types.Pointer:
		return c.holds(t.Elem())
	case *types.Slice:
		return c.holds(t.Elem())
	case *types.Array:
		return c.holds(t.Elem())
	case *types.Chan:
		return c.holds(t.Elem())
	case *types.Map:
		return c.holds(t.Key()) || c.holds(t.Elem())
	case *types.Signature:
		for v := range t.Params().Variables() {
			if c.holds(v.Type()) {
				return true
			}
		}
		for v := range t.Results().Variables() {
			if c.holds(v.Type()) {
				return true
			}
		}
	}
	return false
}

// isStoreType reports whether t is the store interface or implements it.
func (c *checker) isStoreType(t types.Type) bool {
	if c.isStore(t) {
		return true
	}
	return c.storeIface != nil && t != nil && (types.Implements(t, c.storeIface) || types.Implements(types.NewPointer(t), c.storeIface))
}

// isConcreteStore reports whether named type t is a store other than the
// store interface and the typed handles: an engine (*beads.CachingStore,
// *beads.MemStore), a wrapper, or an interface that is a store. A struct
// that is a store only through an embedded typed handle is the handle's.
func (c *checker) isConcreteStore(t *types.Named) bool {
	if c.storeIface == nil || c.isHandle(t) || c.isStore(t) {
		return false
	}
	if c.concrete == nil {
		c.concrete = map[*types.Named]bool{}
	}
	if v, ok := c.concrete[t]; ok {
		return v
	}
	v := types.Implements(t, c.storeIface) || types.Implements(types.NewPointer(t), c.storeIface)
	if st, ok := t.Underlying().(*types.Struct); v && ok {
		for i := range st.NumFields() {
			if f := st.Field(i); f.Embedded() && c.isHandle(f.Type()) {
				v = false
			}
		}
	}
	c.concrete[t] = v
	return v
}

// isStoreValue reports whether a type assertion's operand is a store: a raw
// store, an interface type that is one, or a store, a concrete store (an
// engine or a wrapper, through a pointer) or a handle boxed into an interface
// by a conversion (any(h), any(cache)).
func (c *checker) isStoreValue(x ast.Expr) bool {
	info := c.pass.TypesInfo
	t := info.TypeOf(x)
	if c.isStore(t) {
		return true
	}
	if t != nil && c.storeIface != nil {
		if _, isIface := t.Underlying().(*types.Interface); isIface && types.Implements(t, c.storeIface) {
			return true
		}
	}
	call, ok := ast.Unparen(x).(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	if tv, ok := info.Types[call.Fun]; !ok || !tv.IsType() {
		return false
	}
	if n, ok := deref(info.TypeOf(call.Args[0])).(*types.Named); ok && (c.isHandle(n) || c.isConcreteStore(n)) {
		return true
	}
	return c.isStoreValue(call.Args[0])
}

// findStoreInterface is the store interface as the pass's imports show it,
// or nil when the package cannot see it.
func (c *checker) findStoreInterface() *types.Interface {
	pkgPath, name, ok := cutLast(c.cfg.Store, ".")
	if !ok {
		return nil
	}
	seen := map[*types.Package]bool{}
	var walk func(p *types.Package) *types.Interface
	walk = func(p *types.Package) *types.Interface {
		if p == nil || seen[p] {
			return nil
		}
		seen[p] = true
		if p.Path() == pkgPath {
			if tn, ok := p.Scope().Lookup(name).(*types.TypeName); ok {
				if iface, ok := tn.Type().Underlying().(*types.Interface); ok {
					return iface
				}
			}
			return nil
		}
		for _, imp := range p.Imports() {
			if i := walk(imp); i != nil {
				return i
			}
		}
		return nil
	}
	return walk(c.pass.Pkg)
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func (c *checker) typeString(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string {
		if p == c.pass.Pkg {
			return ""
		}
		return p.Name()
	})
}
