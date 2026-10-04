package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
)

// wakeKeySets are the key sets a keyed trigger can carry.
var wakeKeySets = [][]reconcilekey.Key{
	nil,
	{reconcilekey.Allocator()},
	{{}},
	{reconcilekey.Session("gc-1")},
	{reconcilekey.SessionNamed("worker-1")},
	{reconcilekey.Session("gc-1"), reconcilekey.SessionNamed("worker-2")},
	{reconcilekey.ControlDispatch()},
	{reconcilekey.Session("gc-1"), reconcilekey.ControlDispatch()},
}

// wakeSignalState is one starting state of the two legacy signals: a pending
// signal is what coalesces a later one.
type wakeSignalState struct{ pokePending, dispatchPending bool }

var wakeSignalStates = []wakeSignalState{{}, {pokePending: true}, {dispatchPending: true}, {true, true}}

func (s wakeSignalState) String() string {
	return fmt.Sprintf("poke_pending=%v/dispatch_pending=%v", s.pokePending, s.dispatchPending)
}

// signalPair returns fresh cap-1 signals in state s.
func (s wakeSignalState) signalPair() (pokeCh, dispatchCh chan struct{}) {
	pokeCh, dispatchCh = make(chan struct{}, 1), make(chan struct{}, 1)
	if s.pokePending {
		pokeCh <- struct{}{}
	}
	if s.dispatchPending {
		dispatchCh <- struct{}{}
	}
	return pokeCh, dispatchCh
}

func TestLegacyWakeMatchesLegacyEnqueue(t *testing.T) {
	for _, keys := range wakeKeySets {
		for _, st := range wakeSignalStates {
			t.Run(fmt.Sprintf("%v/%s", keys, st), func(t *testing.T) {
				wantPoke, wantDispatch := st.signalPair()
				wantLanded := legacyEnqueue(wantPoke, wantDispatch, keys...)
				gotPoke, gotDispatch := st.signalPair()
				gotLanded := newLegacyWake(gotPoke, gotDispatch).Enqueue(wakeReasonAPI, keys...)
				if gotLanded != wantLanded {
					t.Errorf("landed = %v, want %v", gotLanded, wantLanded)
				}
				if len(gotPoke) != len(wantPoke) || len(gotDispatch) != len(wantDispatch) {
					t.Errorf("signals (poke, dispatch) = (%d, %d), want (%d, %d)", len(gotPoke), len(gotDispatch), len(wantPoke), len(wantDispatch))
				}
			})
		}
	}
	for _, st := range wakeSignalStates {
		t.Run("maintenance/"+st.String(), func(t *testing.T) {
			wantPoke, wantDispatch := st.signalPair()
			legacyEnqueue(wantPoke, nil)
			gotPoke, gotDispatch := st.signalPair()
			newLegacyWake(gotPoke, gotDispatch).WakeMaintenance()
			if len(gotPoke) != len(wantPoke) || len(gotDispatch) != len(wantDispatch) {
				t.Errorf("signals (poke, dispatch) = (%d, %d), want (%d, %d)", len(gotPoke), len(gotDispatch), len(wantPoke), len(wantDispatch))
			}
		})
	}
	t.Run("nil wake", func(t *testing.T) {
		var w *controllerWake
		if w.Enqueue(wakeReasonAPI, reconcilekey.Allocator()) {
			t.Error("a nil wake reported a landed enqueue")
		}
		w.WakeMaintenance()
		w.OnBeadEvent(events.Event{}, false, false)
		w.OnEventGap()
	})
}

func TestLegacyWakeOnBeadEventPokesOnlyForNonSnapshot(t *testing.T) {
	pokeCh, dispatchCh := make(chan struct{}, 1), make(chan struct{}, 1)
	w := newLegacyWake(pokeCh, dispatchCh)

	w.OnBeadEvent(events.Event{}, true, false)
	if drainSignal(pokeCh) || drainSignal(dispatchCh) {
		t.Fatal("a cache-reconcile replay woke the reconciler: every controller write would echo into a tick (ga-yoix1)")
	}
	w.OnBeadEvent(events.Event{}, false, false)
	if !drainSignal(pokeCh) {
		t.Fatal("a bead event did not poke the reconciler")
	}
	if drainSignal(dispatchCh) {
		t.Fatal("a bead event signaled the control dispatcher; it is allocator work")
	}
}

// TestControllerWiringWakesItsOwnSignals pins what both entry points rely on:
// the wake newControllerWiring builds, once installed on the API state,
// signals exactly the wiring's channels, which the runtime's run loop selects
// on (assertWakeSignalsWired checks the runtime half).
func TestControllerWiringWakesItsOwnSignals(t *testing.T) {
	w, err := newControllerWiring(&config.City{}, nil, io.Discard)
	if err != nil {
		t.Fatalf("newControllerWiring: %v", err)
	}
	cs := &controllerState{}
	wireControllerWakeSignals(cs, w.wake)

	cs.Enqueue(reconcilekey.Session("gc-1"))
	if poke, dispatch := drainSignal(w.pokeCh), drainSignal(w.controlDispatcherCh); !poke || dispatch {
		t.Errorf("session enqueue signaled (poke, dispatch) = (%v, %v), want (true, false)", poke, dispatch)
	}
	cs.Enqueue(reconcilekey.ControlDispatch())
	if poke, dispatch := drainSignal(w.pokeCh), drainSignal(w.controlDispatcherCh); poke || !dispatch {
		t.Errorf("control-dispatch enqueue signaled (poke, dispatch) = (%v, %v), want (false, true)", poke, dispatch)
	}
	w.wake.WakeMaintenance()
	if poke, dispatch := drainSignal(w.pokeCh), drainSignal(w.controlDispatcherCh); !poke || dispatch {
		t.Errorf("maintenance wake signaled (poke, dispatch) = (%v, %v), want (true, false)", poke, dispatch)
	}
}

// TestControllerWakeRouterSeam pins the seam the v2 switch uses: with a
// router installed, keyed enqueues and event gaps reach the router and never
// the legacy signals, while a maintenance wake still pokes the tick.
func TestControllerWakeRouterSeam(t *testing.T) {
	var allocWakes []routeReason
	var resyncs []string
	router := newReconcileRouter("sessions", routerSink{
		addSession:    func(rowKey, routeReason) {},
		wakeAllocator: func(r routeReason) { allocWakes = append(allocWakes, r) },
		requestResync: func(reason string) { resyncs = append(resyncs, reason) },
	}, io.Discard)
	pokeCh, dispatchCh := make(chan struct{}, 1), make(chan struct{}, 1)
	w := &controllerWake{pokeCh: pokeCh, controlDispatcherCh: dispatchCh, router: router}

	if !w.Enqueue(routeReasonSupervisor, reconcilekey.Allocator()) {
		t.Error("a routed enqueue reported nothing landed")
	}
	w.Enqueue(wakeReasonSocket, reconcilekey.ControlDispatch())
	w.Enqueue(wakeReasonAPI)
	w.Enqueue(wakeReasonFollowUp, reconcilekey.Allocator())
	w.OnEventGap()
	if drainSignal(pokeCh) || drainSignal(dispatchCh) {
		t.Fatal("a routed enqueue also signaled the legacy reconciler")
	}
	// The operator intents (api, socket) and the supervisor reload bypass the
	// backoff gate (amendment A2); the reason kinds are what the router logs.
	want := []routeReason{
		{Kind: routeReasonSupervisor, Urgent: true},
		{Kind: routeReasonControlDispatch, Detail: "socket", Urgent: true},
		{Kind: "api", Urgent: true},
		{Kind: "follow-up"},
	}
	if fmt.Sprint(allocWakes) != fmt.Sprint(want) {
		t.Errorf("router allocator wakes = %+v, want %+v (the caller's reason and its urgency must reach the router)", allocWakes, want)
	}
	if want := []string{routeReasonSupervisor, "bead-event-gap"}; strings.Join(resyncs, ",") != strings.Join(want, ",") {
		t.Errorf("router resyncs = %v, want %v", resyncs, want)
	}
	w.WakeMaintenance()
	if !drainSignal(pokeCh) {
		t.Error("a maintenance wake did not poke the tick with a router installed")
	}
}

// wakeInput is one trigger at a site: the keys it carries, or whether a bead
// event is a cache-reconcile replay.
type wakeInput struct {
	keys     []reconcilekey.Key
	snapshot bool
}

func keyedWakeInputs() []wakeInput {
	in := make([]wakeInput, len(wakeKeySets))
	for i, keys := range wakeKeySets {
		in[i] = wakeInput{keys: keys}
	}
	return in
}

// wakeSource is one production enqueue site: the wake call it makes now,
// and the exact send it made before the wake existed (origin/main at
// d140d3835c).
type wakeSource struct {
	site   string // file.go:enclosing function
	call   string // the wake call as written there, receiver omitted
	count  int    // identical calls at the site; 0 means 1
	inputs []wakeInput
	before func(pokeCh, dispatchCh chan<- struct{}, in wakeInput)
	after  func(w *controllerWake, in wakeInput)
}

var allocatorKey = []reconcilekey.Key{reconcilekey.Allocator()}

// wakeSources lists every production enqueue. Each "before" is the site's
// pre-wake send, transcribed from the line it replaced.
var wakeSources = []wakeSource{
	{
		site:   "api_state.go:Enqueue",
		call:   "Enqueue(wakeReasonAPI, keys...)",
		inputs: keyedWakeInputs(),
		before: func(p, d chan<- struct{}, in wakeInput) { legacyEnqueue(p, d, in.keys...) },
		after:  func(w *controllerWake, in wakeInput) { w.Enqueue(wakeReasonAPI, in.keys...) },
	},
	{
		site:   "api_state.go:applyBeadEventToStores",
		call:   "OnBeadEvent(evt, snapshot, appliedToSessions)",
		inputs: []wakeInput{{snapshot: false}, {snapshot: true}},
		before: func(p, d chan<- struct{}, in wakeInput) {
			if !in.snapshot {
				legacyEnqueue(p, d, allocatorKey...) // was cs.Enqueue(reconcilekey.Allocator())
			}
		},
		after: func(w *controllerWake, in wakeInput) { w.OnBeadEvent(events.Event{}, in.snapshot, false) },
	},
	{
		site:   "api_state.go:mutateAndPoke",
		call:   "WakeMaintenance()",
		before: func(p, d chan<- struct{}, _ wakeInput) { legacyEnqueue(p, d, allocatorKey...) }, // was cs.Enqueue(reconcilekey.Allocator())
		after:  func(w *controllerWake, _ wakeInput) { w.WakeMaintenance() },
	},
	{
		site:   "api_state.go:startBeadEventWatcher",
		call:   "OnEventGap()",
		count:  5,                                          // no provider, unresolved cursor, watch error, regressed seq, broken tail
		before: func(_, _ chan<- struct{}, _ wakeInput) {}, // new: no legacy send
		after:  func(w *controllerWake, _ wakeInput) { w.OnEventGap() },
	},
	{
		site:   "controller.go:handleControllerConn",
		call:   "Enqueue(wakeReasonSocket, key)",
		inputs: keyedWakeInputs(),
		before: func(p, d chan<- struct{}, in wakeInput) { legacyEnqueue(p, d, in.keys...) },
		after:  func(w *controllerWake, in wakeInput) { w.Enqueue(wakeReasonSocket, in.keys...) },
	},
	{
		site:   "controller.go:handleControllerConn",
		call:   "WakeMaintenance()",
		before: func(p, _ chan<- struct{}, _ wakeInput) { legacyEnqueue(p, nil, allocatorKey...) },
		after:  func(w *controllerWake, _ wakeInput) { w.WakeMaintenance() },
	},
	{
		site:   "controller.go:handleControllerConn",
		call:   "Enqueue(wakeReasonSocket, reconcilekey.ControlDispatch())",
		before: func(_, d chan<- struct{}, _ wakeInput) { legacyEnqueue(nil, d, reconcilekey.ControlDispatch()) },
		after:  func(w *controllerWake, _ wakeInput) { w.Enqueue(wakeReasonSocket, reconcilekey.ControlDispatch()) },
	},
	{
		site:   "controller.go:handleControllerConn",
		call:   "Enqueue(wakeReasonTrace, reconcilekey.Allocator())",
		count:  2, // trace-arm, trace-stop
		before: func(p, _ chan<- struct{}, _ wakeInput) { legacyEnqueue(p, nil, allocatorKey...) },
		after:  func(w *controllerWake, _ wakeInput) { w.Enqueue(wakeReasonTrace, reconcilekey.Allocator()) },
	},
	{
		site:   "controller.go:watchConfigTargets",
		call:   "WakeMaintenance()",
		before: func(p, _ chan<- struct{}, _ wakeInput) { legacyEnqueue(p, nil, allocatorKey...) },
		after:  func(w *controllerWake, _ wakeInput) { w.WakeMaintenance() },
	},
	{
		site:   "cmd_supervisor.go:runSupervisor",
		call:   "Enqueue(routeReasonSupervisor, reconcilekey.Allocator())",
		before: func(p, d chan<- struct{}, _ wakeInput) { legacyEnqueue(p, d, allocatorKey...) }, // was v.cs.Enqueue(reconcilekey.Allocator())
		after:  func(w *controllerWake, _ wakeInput) { w.Enqueue(routeReasonSupervisor, reconcilekey.Allocator()) },
	},
	{
		site:   "city_runtime.go:handleReloadRequest",
		call:   "WakeMaintenance()",
		before: func(p, _ chan<- struct{}, _ wakeInput) { legacyEnqueue(p, nil, allocatorKey...) },
		after:  func(w *controllerWake, _ wakeInput) { w.WakeMaintenance() },
	},
	{
		site:   "city_runtime.go:requestDeferredDrainFollowUpTick",
		call:   "Enqueue(wakeReasonFollowUp, reconcilekey.Allocator())",
		before: func(p, _ chan<- struct{}, _ wakeInput) { legacyEnqueue(p, nil, allocatorKey...) },
		after:  func(w *controllerWake, _ wakeInput) { w.Enqueue(wakeReasonFollowUp, reconcilekey.Allocator()) },
	},
	{
		site:   "city_runtime.go:requestAsyncStartFollowUpTick",
		call:   "Enqueue(wakeReasonFollowUp, reconcilekey.Allocator())",
		before: func(p, _ chan<- struct{}, _ wakeInput) { legacyEnqueue(p, nil, allocatorKey...) },
		after:  func(w *controllerWake, _ wakeInput) { w.Enqueue(wakeReasonFollowUp, reconcilekey.Allocator()) },
	},
	{
		site:   "service_runtime.go:Poke",
		call:   "Enqueue(wakeReasonService, reconcilekey.Allocator())",
		before: func(p, _ chan<- struct{}, _ wakeInput) { legacyEnqueue(p, nil, allocatorKey...) },
		after:  func(w *controllerWake, _ wakeInput) { w.Enqueue(wakeReasonService, reconcilekey.Allocator()) },
	},
	{
		site: "runtime_inventory_ondeath.go:detectPoolDeaths",
		call: "Enqueue(wakeReasonLaneGone, keys...)",
		inputs: []wakeInput{
			{keys: []reconcilekey.Key{reconcilekey.SessionNamed("worker-1")}},
			{keys: []reconcilekey.Key{reconcilekey.SessionNamed("worker-1"), reconcilekey.SessionNamed("worker-2")}},
		},
		before: func(p, _ chan<- struct{}, in wakeInput) { legacyEnqueue(p, nil, in.keys...) },
		after:  func(w *controllerWake, in wakeInput) { w.Enqueue(wakeReasonLaneGone, in.keys...) },
	},
	{
		site:   "runtime_inventory_ondeath.go:startOnDeathWorker",
		call:   "Enqueue(wakeReasonOnDeath, reconcilekey.SessionNamed(e.name))",
		before: func(p, _ chan<- struct{}, _ wakeInput) { legacyEnqueue(p, nil, reconcilekey.SessionNamed("worker-1")) },
		after: func(w *controllerWake, _ wakeInput) {
			w.Enqueue(wakeReasonOnDeath, reconcilekey.SessionNamed("worker-1"))
		},
	},
	{
		site:   "session_event_pump.go:poke",
		call:   "Enqueue(wakeReasonProviderEvent, reconcilekey.SessionNamed(session))",
		before: func(p, _ chan<- struct{}, _ wakeInput) { legacyEnqueue(p, nil, reconcilekey.SessionNamed("worker-1")) },
		after: func(w *controllerWake, _ wakeInput) {
			w.Enqueue(wakeReasonProviderEvent, reconcilekey.SessionNamed("worker-1"))
		},
	},
}

// TestEveryEnqueueSourceKeepsItsLegacyWake is the equivalence proof: for
// every production enqueue site, from every starting state of the two
// signals, the wake call leaves the poke and control-dispatcher signals
// exactly as the site's pre-wake send did, so the tick and the
// control-dispatcher tick wake as before. The inventory half parses cmd/gc
// so the table cannot drift from the code: a new wake call, a removed one,
// or a call whose arguments changed fails here.
func TestEveryEnqueueSourceKeepsItsLegacyWake(t *testing.T) {
	want := map[string]int{}
	for _, src := range wakeSources {
		n := src.count
		if n == 0 {
			n = 1
		}
		want[src.site+" "+src.call] += n
		inputs := src.inputs
		if inputs == nil {
			inputs = []wakeInput{{}}
		}
		for _, in := range inputs {
			for _, st := range wakeSignalStates {
				t.Run(fmt.Sprintf("%s/%s/%v/%s", src.site, src.call, in, st), func(t *testing.T) {
					beforePoke, beforeDispatch := st.signalPair()
					src.before(beforePoke, beforeDispatch, in)
					afterPoke, afterDispatch := st.signalPair()
					src.after(newLegacyWake(afterPoke, afterDispatch), in)
					if len(afterPoke) != len(beforePoke) || len(afterDispatch) != len(beforeDispatch) {
						t.Errorf("signals (poke, dispatch) = (%d, %d), want (%d, %d) as before the wake", len(afterPoke), len(afterDispatch), len(beforePoke), len(beforeDispatch))
					}
				})
			}
		}
	}
	got := productionWakeCalls(t)
	var diffs []string
	for k, n := range got {
		if want[k] != n {
			diffs = append(diffs, fmt.Sprintf("in code %d, in table %d: %s", n, want[k], k))
		}
	}
	for k, n := range want {
		if _, ok := got[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("in code 0, in table %d: %s", n, k))
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Fatalf("production wake calls and wakeSources disagree; add or fix the row (with the send it replaces):\n  %s", strings.Join(diffs, "\n  "))
	}
}

// productionWakeCalls returns every wake method call in cmd/gc's non-test
// files, keyed "file.go:declaration call", where the receiver is a wake:
// wakeOf(), a wake variable, or a wake field. The walk covers whole files,
// so a call in a func literal, package-level or not, counts too.
func productionWakeCalls(t *testing.T) map[string]int {
	t.Helper()
	calls := map[string]int{}
	fset := token.NewFileSet()
	for name, file := range parseCmdGCProductionFiles(t, fset) {
		if name == "reconcile_wake.go" {
			continue
		}
		inspectTopLevel(file, func(decl string, n ast.Node) {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isWakeMethod(sel.Sel.Name) || !isWakeReceiver(sel.X) {
				return
			}
			var args []string
			for _, a := range call.Args {
				args = append(args, renderNode(t, fset, a))
			}
			text := sel.Sel.Name + "(" + strings.Join(args, ", ")
			if call.Ellipsis.IsValid() {
				text += "..."
			}
			if i := strings.LastIndex(decl, ")."); i >= 0 {
				decl = decl[i+2:] // the table names methods without the receiver
			}
			calls[name+":"+decl+" "+text+")"]++
		})
	}
	return calls
}

// inspectTopLevel walks the whole file, telling visit which top-level
// declaration each node sits in: "f", "(T).m" or "(*T).m" for functions, and
// the first declared name for a const, var, type or import block.
func inspectTopLevel(file *ast.File, visit func(decl string, n ast.Node)) {
	top := map[ast.Node]string{}
	for _, d := range file.Decls {
		top[d] = topLevelDeclName(d)
	}
	var cur string
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		if name, ok := top[n]; ok {
			cur = name
		}
		visit(cur, n)
		return true
	})
}

func topLevelDeclName(d ast.Decl) string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil || len(d.Recv.List) == 0 {
			return d.Name.Name
		}
		switch recv := d.Recv.List[0].Type.(type) {
		case *ast.StarExpr:
			if id, ok := recv.X.(*ast.Ident); ok {
				return "(*" + id.Name + ")." + d.Name.Name
			}
		case *ast.Ident:
			return "(" + recv.Name + ")." + d.Name.Name
		}
		return d.Name.Name
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				return "type " + s.Name.Name
			case *ast.ValueSpec:
				return d.Tok.String() + " " + s.Names[0].Name
			case *ast.ImportSpec:
				return "import"
			}
		}
	}
	return "?"
}

func isWakeMethod(name string) bool {
	switch name {
	case "Enqueue", "WakeMaintenance", "OnBeadEvent", "OnEventGap":
		return true
	}
	return false
}

func isWakeReceiver(x ast.Expr) bool {
	switch x := x.(type) {
	case *ast.Ident:
		return x.Name == "wake"
	case *ast.SelectorExpr:
		return x.Sel.Name == "wake"
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "wakeOf"
	}
	return false
}

func renderNode(t *testing.T, fset *token.FileSet, n ast.Node) string {
	t.Helper()
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, n); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func parseCmdGCProductionFiles(t *testing.T, fset *token.FileSet) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading cmd/gc: %v", err)
	}
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files[name] = file
	}
	return files
}

// wakeSignalMentions are the only declarations in cmd/gc's non-test files
// that may name the reconciler's two wake signals (pokeCh,
// controlDispatcherCh, and the params' PokeCh, ControlDispatcherCh): the
// fields that hold them, the code that makes them and hands them to the
// runtime and the wake, the run loop that selects on them, and legacyEnqueue,
// which signals them. Any other mention can send behind the wake's back
// (directly, through an alias, or through a helper), so it fails
// TestEveryReconcileEnqueueGoesThroughTheWake. Adding a row needs a reason.
var wakeSignalMentions = map[string]bool{
	"api_state.go:type controllerState":                         true, // wakeOf's fallback fields
	"city_runtime.go:type CityRuntime":                          true, // the run loop's signals
	"city_runtime.go:type CityRuntimeParams":                    true, // handed in by the entry points
	"city_runtime.go:newCityRuntime":                            true, // made when not handed in
	"city_runtime.go:(*CityRuntime).run":                        true, // the run loop selects on them
	"city_runtime_v2.go:(*CityRuntime).controlDispatcherSignal": true, // the run loop's control arm, nil under v2
	"controller.go:controllerLoop":                              true, // the test shim's runtime makes its own
	"reconcile_enqueue.go:legacyEnqueue":                        true, // the one fold that signals them
	"reconcile_wake.go:type controllerWake":                     true, // the wake's signals
	"reconcile_wake.go:newLegacyWake":                           true,
	"reconcile_wake.go:(*controllerState).wakeOf":               true,
	"reconcile_wake.go:(*controllerWake).Enqueue":               true,
	"reconcile_wake.go:(*controllerWake).WakeMaintenance":       true,
	"reconcile_wake.go:(*controllerWake).OnBeadEvent":           true,
	"reconcile_wake.go:(*CityRuntime).initWake":                 true,
	"reconcile_wiring.go:type controllerWiring":                 true,
	"reconcile_wiring.go:newControllerWiring":                   true,
	"reconcile_wiring.go:(*controllerWiring).runtimeParams":     true, // hands the wiring's signals to the runtime
}

// wakeFiles are the only files that may build a controllerWake.
var wakeFiles = map[string]bool{"reconcile_wake.go": true, "reconcile_wiring.go": true}

// TestEveryReconcileEnqueueGoesThroughTheWake pins the single sink over whole
// files, func literals included: only the controllerWake's methods call
// legacyEnqueue, only legacyEnqueue sends on the reconciler's wake signals,
// only the declarations in wakeSignalMentions name those signals at all, and
// only the wake files build a wake. A send, fold or ad hoc wake anywhere else
// is a trigger the v2 router would never see.
func TestEveryReconcileEnqueueGoesThroughTheWake(t *testing.T) {
	fset := token.NewFileSet()
	var bypasses []string
	mentioned := map[string]bool{}
	flag := func(n ast.Node, decl, format string, args ...any) {
		bypasses = append(bypasses, fmt.Sprintf("%s: %s %s", fset.Position(n.Pos()), decl, fmt.Sprintf(format, args...)))
	}
	for name, file := range parseCmdGCProductionFiles(t, fset) {
		inspectTopLevel(file, func(decl string, n ast.Node) {
			switch n := n.(type) {
			case *ast.Ident:
				switch {
				case n.Name == "legacyEnqueue":
					if (name != "reconcile_enqueue.go" || decl != "legacyEnqueue") &&
						(name != "reconcile_wake.go" || !strings.HasPrefix(decl, "(*controllerWake).")) {
						flag(n, decl, "names legacyEnqueue")
					}
				case n.Name == "newLegacyWake":
					if !wakeFiles[name] {
						flag(n, decl, "builds a wake with newLegacyWake")
					}
				case isWakeSignalName(n.Name):
					mentioned[name+":"+decl] = true
					if !wakeSignalMentions[name+":"+decl] {
						flag(n, decl, "names wake signal %s", n.Name)
					}
				}
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok && id.Name == "controllerWake" && !wakeFiles[name] {
					flag(n, decl, "builds a wake with a controllerWake literal")
				}
			case *ast.SendStmt:
				if isWakeSignal(n.Chan) && decl != "legacyEnqueue" {
					flag(n, decl, "sends on %s", renderNode(t, fset, n.Chan))
				}
			}
		})
	}
	for decl := range wakeSignalMentions {
		if !mentioned[decl] {
			bypasses = append(bypasses, decl+": in wakeSignalMentions but names no wake signal; drop the row")
		}
	}
	if len(bypasses) > 0 {
		sort.Strings(bypasses)
		t.Fatalf("reconcile enqueues bypass the controller wake:\n  %s", strings.Join(bypasses, "\n  "))
	}
}

// isWakeSignal reports whether ch names one of the reconciler's two wake
// signals, whatever it is reached through.
func isWakeSignal(ch ast.Expr) bool {
	switch ch := ch.(type) {
	case *ast.Ident:
		return isWakeSignalName(ch.Name)
	case *ast.SelectorExpr:
		return isWakeSignalName(ch.Sel.Name)
	}
	return false
}

func isWakeSignalName(name string) bool {
	return name == "pokeCh" || name == "controlDispatcherCh" || name == "PokeCh" || name == "ControlDispatcherCh"
}

// scriptedEventProvider is an events.Provider whose Watch calls play a
// script: each call returns the next scripted watcher or error, and every
// call after the script blocks until ctx ends. Once ctx is done every call
// fails with ctx.Err(), so a watcher that ignores its stop fails fast.
type scriptedEventProvider struct {
	events.Provider // unused methods panic
	watches         []scriptedWatch

	mu        sync.Mutex // the watcher goroutine calls Watch; the test reads cursors
	calls     int
	afterSeqs []uint64 // every Watch call's cursor
}

// cursors returns every Watch call's cursor so far.
func (p *scriptedEventProvider) cursors() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.afterSeqs)
}

type scriptedWatch struct {
	err    error
	events []events.Event // then Next fails: the tail broke
}

func (p *scriptedEventProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.afterSeqs = append(p.afterSeqs, afterSeq)
	if p.calls >= len(p.watches) {
		return &blockingWatcher{ctx: ctx}, nil
	}
	w := p.watches[p.calls]
	p.calls++
	if w.err != nil {
		return nil, w.err
	}
	return &scriptedWatcher{events: w.events}, nil
}

type scriptedWatcher struct{ events []events.Event }

func (w *scriptedWatcher) Next() (events.Event, error) {
	if len(w.events) == 0 {
		return events.Event{}, errors.New("tail broke")
	}
	e := w.events[0]
	w.events = w.events[1:]
	return e, nil
}

func (*scriptedWatcher) Close() error { return nil }

type blockingWatcher struct{ ctx context.Context }

func (w *blockingWatcher) Next() (events.Event, error) {
	<-w.ctx.Done()
	return events.Event{}, w.ctx.Err()
}

func (*blockingWatcher) Close() error { return nil }

// Kills: a broken tail reported as a gap although the re-watch resumed it
// (every FileRecorder hiccup forced a full resync), the re-watch started from
// anywhere but the last seq read, or a resume that failed left unreported: a
// failed Watch, or a watcher that broke before reading past its cursor; and a
// resume that made no progress re-watching at once (a hot loop on a tail that
// keeps breaking at its cursor) instead of after the retry delay.
func TestBeadEventWatcherResumesBrokenTailFromCursor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gapCount atomic.Int32
		gaps := gapCount.Load
		router := newReconcileRouter("sessions", routerSink{
			requestResync: func(reason string) {
				if reason == "bead-event-gap" {
					gapCount.Add(1)
				}
			},
		}, io.Discard)
		other := func(seq uint64) events.Event { return events.Event{Seq: seq, Type: "test.other"} }
		ep := &scriptedEventProvider{watches: []scriptedWatch{
			{events: []events.Event{other(5), other(6)}}, // the tail breaks after 6: resumed
			{}, // the resume breaks at its cursor: a gap
			{events: []events.Event{other(7), other(4)}}, // seq regresses (a gap), then the tail breaks: resumed
			{err: errors.New("watch failed")},            // the resume fails: a gap
		}}
		cs := &controllerState{eventProv: ep, beadEventStartSeq: 3, beadEventStartSeqOK: true, wake: &controllerWake{router: router}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cs.startBeadEventWatcher(ctx)
		synctest.Wait()
		if want := []uint64{3, 6}; !slices.Equal(ep.cursors(), want) || gaps() != 1 {
			t.Fatalf("watch cursors = %v, gaps = %d; want %v, 1 (the resume broke at its cursor and waits to retry)", ep.cursors(), gaps(), want)
		}
		<-time.After(beadEventWatcherRetryDelay) // fake time
		synctest.Wait()
		if want := []uint64{3, 6, 6, 4}; !slices.Equal(ep.cursors(), want) || gaps() != 3 {
			t.Fatalf("watch cursors = %v, gaps = %d; want %v, 3 (then a regressed seq, a resume and a failed watch)", ep.cursors(), gaps(), want)
		}
		<-time.After(beadEventWatcherRetryDelay) // fake time: the retry watch blocks in Next
		synctest.Wait()
		if want := []uint64{3, 6, 6, 4, 4}; !slices.Equal(ep.cursors(), want) || gaps() != 3 {
			t.Fatalf("after the retry: watch cursors = %v, gaps = %d; want %v, 3", ep.cursors(), gaps(), want)
		}
		cancel()
		synctest.Wait()
		if gaps() != 3 {
			t.Fatalf("gaps reported = %d after stop, want 3: a stopping watcher is not a gap", gaps())
		}
	})
}

// withLegacyWake gives a directly-constructed runtime the wake newCityRuntime
// builds over its signals.
func withLegacyWake(cr *CityRuntime) *CityRuntime {
	cr.initWake(nil)
	return cr
}
