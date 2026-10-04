//go:build integration

package main

import (
	"encoding/json"
	"go/types"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
	"golang.org/x/tools/go/types/typeutil"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// The call-graph half of the decide's purity guard (P3 spec §4.4; see
// allocator_decide_purity_test.go for the fast half).
//
// The walk is a rapid type analysis rooted at decideAllocation, computed over
// the reached code only:
//   - static calls, methods included, are followed into every package (the
//     module, its internal and non-internal packages, third-party modules
//     and the standard library), except the Go runtime;
//   - an interface call reaches the method of every type converted to an
//     interface in reached code, plus every type that implements an
//     interface found in allocInputs (values the caller built);
//   - a call through a function value (a func-valued field, parameter or
//     local) reaches every function whose address reached code takes, with
//     the call's signature, plus every function of a func type found in
//     allocInputs;
//   - a dynamic call from the standard library to the standard library is
//     not followed (fmt's Stringer dispatch, errors.As's Unwrap). Anything
//     the decide hands the standard library that could do I/O is either
//     made by a flagged call or names a flagged global (os.Stderr), so the
//     cut loses no reach the decide controls.
//
// Every call edge is kept, and a forbidden function is charged to each
// function outside the standard library that reaches it directly or through
// the standard library alone, so one allowlisted reach cannot mask another.
//
// Reused code reaches some flagged functions only on a branch the decide
// never takes: those edges are cut below, each with the guard and the test
// that pins it; the few flags the decide does reach are allowlisted with why.

// decideCut are calls the walk does not follow, "caller → callee" (SSA
// names, module prefix trimmed): the decide never takes them.
var decideCut = map[string]string{
	// configWakeSuppressedInfo gets no provider, so its idle reference is
	// detached_at alone (TestAllocator_ConfigSleepSuppressionDeadUsesDetachedAtLiveNeedsActivity).
	"cmd/gc.sessionIdleReferenceInfoWithError → cmd/gc.workerSessionTargetLastActivityWithConfig": "sp == nil",
}

// decideAllowed are flagged reaches the decide makes, "function: flag",
// with why none is a per-pass read or a shared lock.
var decideAllowed = map[string]string{
	// Legacy reads the wall clock and discards it for the injected clock
	// (clk != nil): the decide passes clock.Fake at in.Now.
	"cmd/gc.pendingCreateStartInFlightInfo: time.Now":            "discarded for clk",
	"cmd/gc.pendingCreateNeverStartedLeaseExpiredInfo: time.Now": "discarded for clk",
	// Zero-Now fallbacks: the lifecycle input's Now is the pass's clock
	// (newAwakeInputFromSnapshot), and the decide refuses a zero Now.
	"internal/session.ProjectLifecycle: time.Now":     "input.Now set",
	"internal/session.creatingStateIsStale: time.Now": "input.Now set",
	// A misconfiguration warning legacy writes to the process log every
	// tick (a malformed session_template). Not a read; P3-8 moves it to the
	// trace.
	"internal/agent.SessionNameFor: os.Stderr": "misconfiguration warning",
	// Known gap (inherited, reported at P3-5a): matching an agent's dir to
	// a rig path canonicalizes both (filepath.Abs, EvalSymlinks), which
	// stats config-declared paths whenever the dir is not a rig name.
	// Legacy does the same every tick; P3-7 resolves rig names per
	// environment generation (obligation recorded in the P3 spec).
	"internal/pathutil.NormalizePathForCompare: os.Getwd":           "config path canonicalization",
	"internal/pathutil.NormalizePathForCompare: os.Lstat":           "config path canonicalization",
	"internal/pathutil.NormalizePathForCompare: os.Readlink":        "config path canonicalization",
	"internal/pathutil.ResolveNearestExistingAncestor: os.Lstat":    "config path canonicalization",
	"internal/pathutil.ResolveNearestExistingAncestor: os.Readlink": "config path canonicalization",
}

// decideMustReach are functions the walk must reach, or it proves nothing:
// the reused legacy code, and the ledger, census and observation readers
// the decide calls through methods.
var decideMustReach = []string{
	"cmd/gc.computeAwakeSetKeyed", "cmd/gc.filterAssignedWorkBeadsForSessionWakeOn", "cmd/gc.newAwakeInputFromSnapshot",
	"cmd/gc.observeCensus", "cmd/gc.readRuntimeName", "cmd/gc.configWakeSuppressedInfo",
	"(cmd/gc.ledgerEntry).lagging", "(cmd/gc.ledgerEntry).markerVisible", "(cmd/gc.censusLeg).complete",
}

// decideMustNotReach are the I/O-bound legacy entry points the decide
// replaced with inputs.
var decideMustNotReach = []string{
	"cmd/gc.newAgentBuildParams", "cmd/gc.effectiveCitySuspended", "cmd/gc.assignedWorkRelocatedClaimRefs",
	"cmd/gc.assignedWorkClaimRefs", "cmd/gc.validateAgentSessionTransportForBuild", "cmd/gc.loadSuspensionState",
	"cmd/gc.loadProviderHealthSnapshot", "cmd/gc.realizePoolDesiredSessionsAt", "cmd/gc.ensureDependencyOnlyTemplate",
	"cmd/gc.buildAwakeInputFromReconcilerWithObservationErrors",
}

// decidePureOS are os functions that only inspect their argument; the walk
// neither flags nor enters them.
var decidePureOS = map[string]bool{
	"IsNotExist": true, "IsExist": true, "IsPermission": true, "IsTimeout": true, "IsPathSeparator": true,
}

// decideClockFuncs are the time package's clock and timer functions.
var decideClockFuncs = map[string]bool{
	"Now": true, "Since": true, "Until": true, "Sleep": true, "After": true, "AfterFunc": true,
	"Tick": true, "NewTimer": true, "NewTicker": true,
}

const decideModulePrefix = "github.com/gastownhall/gascity/"

func trimModule(s string) string { return strings.ReplaceAll(s, decideModulePrefix, "") }

// pkgPathOf is a function's package path, through its enclosing function
// for a closure; "" for a synthetic wrapper with no package.
func pkgPathOf(f *ssa.Function) string {
	for f.Pkg == nil && f.Parent() != nil {
		f = f.Parent()
	}
	if f.Pkg == nil {
		if recv := f.Signature.Recv(); recv != nil {
			if n, ok := derefNamed(recv.Type()); ok && n.Obj().Pkg() != nil {
				return n.Obj().Pkg().Path()
			}
		}
		return ""
	}
	return f.Pkg.Pkg.Path()
}

func derefNamed(t types.Type) (*types.Named, bool) {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	return n, ok
}

// isStd reports a standard library package path.
func isStd(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return path != "" && !strings.Contains(first, ".")
}

// decideWalk is one reachability walk from decideAllocation.
type decideWalk struct {
	prog     *ssa.Program
	reached  map[*ssa.Function]*ssa.Function // → the caller it was reached from
	edges    map[*ssa.Function]map[*ssa.Function]bool
	queue    []*ssa.Function
	addr     typeutil.Map // signature → []*ssa.Function whose address reached code takes
	addrSeen map[*ssa.Function]bool
	rtypes   []types.Type // types converted to an interface in reached code
	seeded   []types.Type // types that implement an interface found in allocInputs
	rtSeen   typeutil.Map
	sites    []*decideSite
	// effect are the interfaces whose every call is a store, provider or
	// filesystem access, by name.
	effect map[string]*types.Interface
	flags  map[string]string // "function: flag" → the chain that reached it
	cutHit map[string]bool
}

// decideSite is a dynamic call site, with how many of the runtime types or
// address-taken functions known so far it has resolved against.
type decideSite struct {
	caller           *ssa.Function
	call             *ssa.CallCommon
	seen, seenSeeded int
}

func (w *decideWalk) chain(f *ssa.Function) string {
	var path []string
	for g := f; g != nil; g = w.reached[g] {
		path = append(path, trimModule(g.String()))
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return strings.Join(path, " → ")
}

// flag charges what to f, outside the standard library, reached by chain.
func (w *decideWalk) flag(f *ssa.Function, what, chain string) {
	key := trimModule(f.String()) + ": " + what
	if _, ok := w.flags[key]; !ok {
		w.flags[key] = chain
	}
}

// forbidden names a function that is I/O, the environment, logging or the
// clock, or "".
func forbidden(f *ssa.Function) string {
	path := pkgPathOf(f)
	switch {
	case decideIOPackages[path]:
		return trimModule(f.String())
	case path == "time" && f.Signature.Recv() == nil && decideClockFuncs[f.Name()]:
		return "time." + f.Name()
	case path == "fmt" && strings.HasPrefix(f.Name(), "Print"):
		return "fmt." + f.Name()
	}
	return ""
}

// charge flags, for every reached function outside the standard library,
// each forbidden function it calls directly or through the standard
// library alone. Every path is charged, not only the first that reached a
// function: one allowlisted reach of time.Now must not hide another.
func (w *decideWalk) charge() {
	for f := range w.reached {
		if isStd(pkgPathOf(f)) {
			continue
		}
		seen := map[*ssa.Function]bool{}
		var walk func(g *ssa.Function, path []string)
		walk = func(g *ssa.Function, path []string) {
			for callee := range w.edges[g] {
				next := append(slices.Clip(path), trimModule(callee.String()))
				if what := forbidden(callee); what != "" {
					w.flag(f, what, w.chain(f)+strings.Join(next, " → "))
					continue
				}
				if isStd(pkgPathOf(callee)) && !seen[callee] {
					seen[callee] = true
					walk(callee, next)
				}
			}
		}
		walk(f, []string{""})
	}
}

// edge follows a call from caller to callee, unless it is cut.
func (w *decideWalk) edge(caller, callee *ssa.Function, dynamic bool) {
	if callee == nil || pkgPathOf(callee) == "runtime" {
		return
	}
	if dynamic && isStd(pkgPathOf(caller)) && isStd(pkgPathOf(callee)) {
		return
	}
	for _, cut := range []string{trimModule(caller.String()) + " → " + trimModule(callee.String()), "* → " + trimModule(callee.String())} {
		if _, ok := decideCut[cut]; ok {
			w.cutHit[cut] = true
			return
		}
	}
	if pkgPathOf(callee) == "os" && decidePureOS[callee.Name()] {
		return
	}
	if w.edges[caller] == nil {
		w.edges[caller] = map[*ssa.Function]bool{}
	}
	w.edges[caller][callee] = true
	if _, seen := w.reached[callee]; seen || forbidden(callee) != "" {
		return
	}
	w.reached[callee] = caller
	w.queue = append(w.queue, callee)
}

func (w *decideWalk) addRuntimeType(t types.Type) {
	if w.rtSeen.At(t) != nil {
		return
	}
	w.rtSeen.Set(t, true)
	w.rtypes = append(w.rtypes, t)
}

func (w *decideWalk) addSeeded(t types.Type) {
	for _, s := range w.seeded {
		if types.Identical(s, t) {
			return
		}
	}
	w.seeded = append(w.seeded, t)
}

func (w *decideWalk) addAddrTaken(f *ssa.Function) {
	if w.addrSeen[f] {
		return
	}
	w.addrSeen[f] = true
	fns, _ := w.addr.At(f.Signature).([]*ssa.Function)
	w.addr.Set(f.Signature, append(fns, f))
}

// visit scans a newly reached function: flags, calls, function values and
// interface conversions.
func (w *decideWalk) visit(f *ssa.Function) {
	std := isStd(pkgPathOf(f))
	for _, anon := range f.AnonFuncs {
		w.addAddrTaken(anon)
	}
	for _, b := range f.Blocks {
		for _, ins := range b.Instrs {
			if mi, ok := ins.(*ssa.MakeInterface); ok {
				w.addRuntimeType(mi.X.Type())
			}
			var static *ssa.Function
			if call, ok := ins.(ssa.CallInstruction); ok {
				c := call.Common()
				static = c.StaticCallee()
				switch {
				case static != nil:
					w.checkStatic(f, std, static)
					w.edge(f, static, false)
				case c.IsInvoke() || c.Value != nil:
					if _, builtin := c.Value.(*ssa.Builtin); !builtin {
						if !std && c.IsInvoke() {
							w.checkInvoke(f, c)
						}
						w.sites = append(w.sites, &decideSite{caller: f, call: c})
					}
				}
			}
			for _, op := range ins.Operands(nil) {
				switch v := (*op).(type) {
				case *ssa.Function:
					if v != static {
						w.addAddrTaken(v)
					}
				case *ssa.Global:
					if !std && v.Pkg != nil && decideIOPackages[v.Pkg.Pkg.Path()] {
						w.flag(f, v.Pkg.Pkg.Path()+"."+v.Name(), w.chain(f))
					}
				}
			}
		}
	}
}

// checkStatic flags a lock taken, or a store, provider or filesystem
// method called directly, outside the standard library.
func (w *decideWalk) checkStatic(caller *ssa.Function, std bool, callee *ssa.Function) {
	if std {
		return
	}
	recv := callee.Signature.Recv()
	if recv == nil {
		return
	}
	if n, ok := derefNamed(recv.Type()); ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "sync" &&
		(n.Obj().Name() == "Mutex" || n.Obj().Name() == "RWMutex") && strings.Contains(callee.Name(), "Lock") && !strings.HasPrefix(callee.Name(), "Un") {
		w.flag(caller, "lock "+trimModule(callee.String()), w.chain(caller))
		return
	}
	for name, iface := range w.effect {
		t := recv.Type()
		if types.Implements(t, iface) || types.Implements(types.NewPointer(t), iface) {
			w.flag(caller, name+" "+trimModule(callee.String()), w.chain(caller))
		}
	}
}

// checkInvoke flags an interface call on a store, provider or filesystem.
func (w *decideWalk) checkInvoke(caller *ssa.Function, c *ssa.CallCommon) {
	for name, iface := range w.effect {
		if types.Identical(c.Value.Type().Underlying(), iface) {
			w.flag(caller, name+"."+c.Method.Name(), w.chain(caller))
		}
	}
}

// resolve adds the callees of every dynamic site against the runtime types
// and address-taken functions it has not yet resolved against.
func (w *decideWalk) resolve() {
	for _, s := range w.sites {
		if s.call.IsInvoke() {
			iface := s.call.Value.Type().Underlying().(*types.Interface)
			candidates := w.rtypes[s.seen:]
			s.seen = len(w.rtypes)
			// Only the decide's own code can hold a value the caller built;
			// what reaches the standard library was converted in reached code.
			if !isStd(pkgPathOf(s.caller)) {
				candidates = append(slices.Clip(candidates), w.seeded[s.seenSeeded:]...)
				s.seenSeeded = len(w.seeded)
			}
			for _, t := range candidates {
				if types.IsInterface(t) || !types.Implements(t, iface) {
					continue
				}
				w.edge(s.caller, w.prog.LookupMethod(t, s.call.Method.Pkg(), s.call.Method.Name()), true)
			}
			continue
		}
		fns, _ := w.addr.At(s.call.Signature()).([]*ssa.Function)
		for _, fn := range fns[s.seen:] {
			w.edge(s.caller, fn, true)
		}
		s.seen = len(fns)
	}
}

// seedInputs adds what the caller builds into allocInputs: every type that
// implements an interface found in its type graph, and every function of a
// func type found there.
func (w *decideWalk) seedInputs(in types.Type, all []*ssa.Function, named []types.Type) {
	seen := map[types.Type]bool{}
	var walk func(types.Type)
	walk = func(t types.Type) {
		if seen[t] {
			return
		}
		seen[t] = true
		switch u := t.Underlying().(type) {
		case *types.Interface:
			for _, nt := range named {
				for _, c := range []types.Type{nt, types.NewPointer(nt)} {
					if !types.IsInterface(c) && types.Implements(c, u) {
						w.addSeeded(c)
					}
				}
			}
		case *types.Signature:
			for _, fn := range all {
				if types.Identical(fn.Signature, u) {
					w.addAddrTaken(fn)
				}
			}
		case *types.Pointer:
			walk(u.Elem())
		case *types.Slice:
			walk(u.Elem())
		case *types.Array:
			walk(u.Elem())
		case *types.Map:
			walk(u.Key())
			walk(u.Elem())
		case *types.Struct:
			for i := 0; i < u.NumFields(); i++ {
				walk(u.Field(i).Type())
			}
		}
	}
	walk(in)
}

// goflagsOverlay honors a `-overlay` in GOFLAGS, as a mutation run passes
// it, so the walk analyzes the mutated source the test binary was built
// from: go/packages hands `go list` the flag but parses files from disk.
func goflagsOverlay(t *testing.T) map[string][]byte {
	t.Helper()
	var file string
	for _, f := range strings.Fields(os.Getenv("GOFLAGS")) {
		if v, ok := strings.CutPrefix(f, "-overlay="); ok {
			file = v
		}
	}
	if file == "" {
		return nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading overlay: %v", err)
	}
	var ov struct{ Replace map[string]string }
	if err := json.Unmarshal(raw, &ov); err != nil {
		t.Fatalf("parsing overlay: %v", err)
	}
	out := make(map[string][]byte, len(ov.Replace))
	for path, repl := range ov.Replace {
		if out[path], err = os.ReadFile(repl); err != nil {
			t.Fatalf("reading overlay file: %v", err)
		}
	}
	return out
}

// decideReach loads the module and walks from decideAllocation.
func decideReach(t *testing.T) *decideWalk {
	t.Helper()
	start := time.Now()
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: cmdGCDir(t), Overlay: goflagsOverlay(t)}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		t.Fatalf("loading cmd/gc: %v", err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("loading cmd/gc: %d errors", n)
	}
	t.Logf("loaded in %v", time.Since(start).Round(time.Millisecond))
	prog, ssaPkgs := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	t.Logf("built SSA in %v", time.Since(start).Round(time.Millisecond))
	main := ssaPkgs[0]
	lookup := func(path, name string) *types.Interface {
		for _, p := range prog.AllPackages() {
			if p.Pkg.Path() == path {
				if obj := p.Pkg.Scope().Lookup(name); obj != nil {
					return obj.Type().Underlying().(*types.Interface)
				}
			}
		}
		t.Fatalf("no %s.%s in the program", path, name)
		return nil
	}
	w := &decideWalk{
		prog: prog, reached: map[*ssa.Function]*ssa.Function{}, edges: map[*ssa.Function]map[*ssa.Function]bool{},
		addrSeen: map[*ssa.Function]bool{},
		flags:    map[string]string{}, cutHit: map[string]bool{},
		effect: map[string]*types.Interface{
			"beads.Store":      lookup(decideModulePrefix+"internal/beads", "Store"),
			"runtime.Provider": lookup(decideModulePrefix+"internal/runtime", "Provider"),
			"fsys.FS":          lookup(decideModulePrefix+"internal/fsys", "FS"),
		},
	}
	var named []types.Type
	for _, p := range prog.AllPackages() {
		for _, m := range p.Members {
			if ty, ok := m.(*ssa.Type); ok {
				if _, generic := ty.Type().(*types.Named); generic && ty.Type().(*types.Named).TypeParams().Len() > 0 {
					continue
				}
				named = append(named, ty.Type())
			}
		}
	}
	var all []*ssa.Function
	for fn := range ssautil.AllFunctions(prog) {
		all = append(all, fn)
	}
	in := main.Pkg.Scope().Lookup("allocInputs")
	if in == nil {
		t.Fatal("no allocInputs")
	}
	w.seedInputs(in.Type(), all, named)
	t.Logf("seeded %d types from the inputs in %v", len(w.seeded), time.Since(start).Round(time.Millisecond))
	root := main.Func("decideAllocation")
	w.reached[root] = nil
	w.queue = append(w.queue, root)
	for len(w.queue) > 0 {
		for len(w.queue) > 0 {
			f := w.queue[0]
			w.queue = w.queue[1:]
			w.visit(f)
		}
		w.resolve()
	}
	w.charge()
	return w
}

// TestDecideReachesNoIO is the type-checked call-graph half of the purity
// guard (owner decision at P3-5a review). It loads and type-checks the whole
// module with go/packages (a `go list` subprocess, about 12s and several
// GB), so it lives in the integration tier (make test-integration) rather
// than the fast suite, and skips under bazel, whose sandbox has no go
// toolchain.
//
// Kills: a store, provider, filesystem, environment, clock, logging or lock
// access added to the pass, directly, through a method, an interface or a
// function value, or through a reused legacy helper; G1-G3 of the P3-5a
// review (time.Now in ledgerEntry.lagging, createVeto.live and
// censusLeg.complete, which the AST walk missed).
func TestDecideReachesNoIO(t *testing.T) {
	if bazeltest.IsBazel() {
		t.Skip("go/packages needs the go toolchain, which the bazel sandbox lacks")
	}
	start := time.Now()
	w := decideReach(t)
	t.Logf("walked %d functions in %v", len(w.reached), time.Since(start).Round(time.Millisecond))
	byName := map[string]bool{}
	for f := range w.reached {
		byName[trimModule(f.String())] = true
	}
	var bad []string
	for _, fn := range decideMustReach {
		if !byName[fn] {
			bad = append(bad, "the walk did not reach "+fn)
		}
	}
	for _, fn := range decideMustNotReach {
		if byName[fn] {
			bad = append(bad, "the decide reaches "+fn)
		}
	}
	for key, chain := range w.flags {
		if _, ok := decideAllowed[key]; !ok {
			bad = append(bad, key+"\n      via "+chain)
		}
	}
	for key := range decideAllowed {
		if _, ok := w.flags[key]; !ok {
			bad = append(bad, "stale allowlist entry (no longer reached): "+key)
		}
	}
	for cut := range decideCut {
		if !w.cutHit[cut] {
			bad = append(bad, "stale cut (no longer reached): "+cut)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("the decide reaches I/O, the clock, the environment, logging or a lock:\n  %s", strings.Join(bad, "\n  "))
	}
}
