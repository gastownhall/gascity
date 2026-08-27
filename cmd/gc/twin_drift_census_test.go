package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Twin-drift census (ga-f7v2ft.174).
//
// The keyed reconciler is a second implementation of decisions the legacy
// fleet arm also makes. Where both sides drive a SHARED decider in
// internal/session, each carries its own copy of the fact-gathering loop and
// its own copy of the outcome switch. Nothing in Go ties those copies
// together: upstream can add a rung to the shared ladder, update its own
// consumer, and leave the lane's twin handling a strict subset — no conflict,
// no compile error, no red test.
//
// That is not hypothetical. ga-f7v2ft.166: origin/main's #5052 added
// TimerActionGatherMinFloor to session.DecideIdleTimeout and updated the fleet
// arm. decideExactSessionDeadline's gather loop looped on GatherPending and
// GatherAssignedWork only, so the ladder returned holding an action the loop
// did not understand, the handler recorded a defer and released the key, and
// EVERY keyed idle-deadline stop was silently dead in production.
//
// This file is the structural tie, in the TestAgentFieldSync spirit: compare
// the SETS, not the behavior. A new action constant, a new decision field, or
// a new builder field upstream fails the lane build instead of dying quietly.
//
// Every deliberate omission is recorded as a named exemption with a reason,
// never as a silent gap in coverage.

// ---------------------------------------------------------------------------
// Census 1: lifecycle-timer ladder arms
// ---------------------------------------------------------------------------

// timerLadderArm is one place in cmd/gc that drives a session.Decide* timer
// ladder: a gather loop plus the outcome handling around it. The arm is the
// innermost block containing the loop, which is the `if` body for each fleet
// arm and the per-deadline range body for the keyed twin.
type timerLadderArm struct {
	File     string
	Func     string
	Deciders []string
	// Handled is the gather-action set the LOOP CONDITION names. Only the
	// condition decides whether the ladder is asked again, so it — not the
	// switch inside the body — is what "handled" means. ga-f7v2ft.166 was a
	// condition that had not learned a rung.
	Handled map[string]bool
	// Cases is the gather-action set the loop body's switch names. A case
	// without a matching condition entry is unreachable.
	Cases map[string]bool
	// Named is every TimerAction constant the arm mentions anywhere, which is
	// how terminal actions are covered.
	Named map[string]bool
	Line  int
	// Block is the arm itself, so a per-arm census reads only that arm and not
	// its sibling arm in the same enclosing function.
	Block ast.Node
}

func (a timerLadderArm) key() string {
	return a.File + ":" + a.Func + "[" + strings.Join(a.Deciders, "+") + "]"
}

// timerLadderArmRegistry pins the arms that exist. A new lane function that
// drives one of these ladders shows up here as an unregistered arm rather than
// as an un-censused twin.
var timerLadderArmRegistry = map[string]string{
	"session_reconciler.go:reconcileSessionBeadsTracedWithNamedDemand[DecideMaxSessionAge]":                                       "legacy fleet max-session-age arm",
	"session_reconciler.go:reconcileSessionBeadsTracedWithNamedDemand[DecideAssignedWorkExhausted+DecideIdleTimeout]":             "legacy fleet idle-timeout arm, including the consecutive-defer backstop",
	"session_deadline_reconcile.go:decideExactSessionDeadline[DecideAssignedWorkExhausted+DecideIdleTimeout+DecideMaxSessionAge]": "keyed D-DEADLINE twin; one loop drives both ladders",
}

// timerLadderGatherExemptions records, per arm, a gather rung the arm's
// deciders can emit that the arm deliberately does not loop on. Empty today:
// every arm loops on exactly what its own deciders emit.
var timerLadderGatherExemptions = map[string]map[string]string{}

// timerLadderTerminalExemptions records, per arm, a terminal action the arm's
// deciders can emit that the arm deliberately does not name.
var timerLadderTerminalExemptions = map[string]map[string]string{
	"session_reconciler.go:reconcileSessionBeadsTracedWithNamedDemand[DecideMaxSessionAge]": {
		"TimerActionNone": "the not-triggered no-op: facts.Triggered is false, the loop is never entered, and the outcome switch matching nothing IS the correct no-op",
	},
	"session_reconciler.go:reconcileSessionBeadsTracedWithNamedDemand[DecideAssignedWorkExhausted+DecideIdleTimeout]": {
		"TimerActionNone": "the not-triggered no-op: facts.Triggered is false, the loop is never entered, and the outcome switch matching nothing IS the correct no-op",
	},
	"session_deadline_reconcile.go:decideExactSessionDeadline[DecideAssignedWorkExhausted+DecideIdleTimeout+DecideMaxSessionAge]": {
		"TimerActionNone": "unreachable by construction: the arm ranges over exactSessionDeadlineTriggered, so every evaluated deadline has Triggered=true",
	},
}

// TestTimerLadderTwinsHandleEveryRungTheirDecidersEmit is the ga-f7v2ft.166
// guard. For every cmd/gc arm that drives a lifecycle-timer ladder, the gather
// rungs the arm loops on must be EXACTLY the rungs its own deciders can emit.
//
// Equality, not containment, in both directions:
//   - a rung the decider emits and the arm does not loop on is the .166 kill —
//     the ladder returns an action the caller does not understand and the
//     effect silently never happens;
//   - a rung the arm loops on that no decider of its emits is a stale rung,
//     which is how an arm keeps looking correct after upstream retires a fact.
//
// The fleet max-age arm is the live example of why this must be per-decider:
// its loop handles two rungs because DecideMaxSessionAge emits two. The day
// upstream gives that ladder a floor rung — the type already declares
// TimerActionGatherMinFloor — this test goes red instead of the arm silently
// falling out of its loop holding an action its outcome switch cannot match.
func TestTimerLadderTwinsHandleEveryRungTheirDecidersEmit(t *testing.T) {
	emitted := timerDeciderEmissions(t)
	arms := discoverTimerLadderArms(t)

	if len(arms) == 0 {
		t.Fatal("census found no lifecycle-timer ladder arms in cmd/gc; the discovery is broken, not the tree")
	}

	seen := map[string]bool{}
	firedGather := map[string]bool{}
	firedTerminal := map[string]bool{}
	for _, arm := range arms {
		key := arm.key()
		seen[key] = true
		if _, registered := timerLadderArmRegistry[key]; !registered {
			t.Errorf("unregistered lifecycle-timer ladder arm %s (%s:%d)\n"+
				"A new twin of a shared decider must be added to timerLadderArmRegistry so the census covers it.",
				key, arm.File, arm.Line)
			continue
		}

		wantGather := map[string]bool{}
		wantTerminal := map[string]bool{}
		for _, decider := range arm.Deciders {
			for action := range emitted[decider] {
				if strings.HasPrefix(action, "TimerActionGather") {
					wantGather[action] = true
				} else {
					wantTerminal[action] = true
				}
			}
		}

		for action := range wantGather {
			if arm.Handled[action] {
				continue
			}
			if reason, exempt := timerLadderGatherExemptions[key][action]; exempt {
				firedGather[key+"/"+action] = true
				t.Logf("exemption: %s does not loop on %s — %s", key, action, reason)
				continue
			}
			t.Errorf("%s (%s:%d) does not loop on %s, which %s can emit.\n"+
				"The ladder returns holding that action, the arm falls out of its loop, and the effect silently never happens (ga-f7v2ft.166).\n"+
				"Handle the rung, or record an exemption with its reason in timerLadderGatherExemptions.",
				key, arm.File, arm.Line, action, strings.Join(arm.Deciders, "/"))
		}
		for action := range arm.Handled {
			if wantGather[action] {
				continue
			}
			t.Errorf("%s (%s:%d) loops on %s, which none of %s emits.\n"+
				"A stale rung makes the arm look current after upstream retires the fact.",
				key, arm.File, arm.Line, action, strings.Join(arm.Deciders, "/"))
		}
		for action := range arm.Cases {
			if arm.Handled[action] {
				continue
			}
			t.Errorf("%s (%s:%d) has a case for %s that its loop condition does not admit.\n"+
				"The gather is unreachable: the ladder returns that action, the condition is false, and the arm falls out of the loop having gathered nothing.",
				key, arm.File, arm.Line, action)
		}

		for action := range wantTerminal {
			if arm.Named[action] {
				continue
			}
			if reason, exempt := timerLadderTerminalExemptions[key][action]; exempt {
				firedTerminal[key+"/"+action] = true
				t.Logf("exemption: %s does not name %s — %s", key, action, reason)
				continue
			}
			t.Errorf("%s (%s:%d) never names %s, which %s can emit.\n"+
				"An unhandled terminal action is a decision the arm silently drops.\n"+
				"Handle it, or record an exemption with its reason in timerLadderTerminalExemptions.",
				key, arm.File, arm.Line, action, strings.Join(arm.Deciders, "/"))
		}
	}

	for key := range timerLadderArmRegistry {
		if !seen[key] {
			t.Errorf("registered lifecycle-timer ladder arm %s no longer exists; drop it from timerLadderArmRegistry in the commit that retires it", key)
		}
	}
	assertNoStaleExemptions(t, "timerLadderGatherExemptions", timerLadderGatherExemptions, firedGather)
	assertNoStaleExemptions(t, "timerLadderTerminalExemptions", timerLadderTerminalExemptions, firedTerminal)
}

// assertNoStaleExemptions fails on a recorded exemption the census no longer
// needs. An exemption that outlives its divergence is a standing license to
// re-introduce the gap: the next drift lands inside an entry that already says
// "this is fine", and the census goes quiet exactly where it should shout.
func assertNoStaleExemptions(t *testing.T, name string, exemptions map[string]map[string]string, fired map[string]bool) {
	t.Helper()
	for key, byMember := range exemptions {
		for member := range byMember {
			if !fired[key+"/"+member] {
				t.Errorf("%s has a stale entry: %s / %s no longer diverges.\n"+
					"Delete the exemption in the commit that closes the divergence.", name, key, member)
			}
		}
	}
}

// TestEveryDeclaredTimerActionIsEmittedBySomeDecider closes the census from
// the upstream side. A TimerAction constant that no decider in
// internal/session can return is either dead vocabulary or — the case that
// matters — a rung upstream added to the TYPE and wired into its own consumer
// without any lane-visible decider emitting it yet.
func TestEveryDeclaredTimerActionIsEmittedBySomeDecider(t *testing.T) {
	declared := declaredTimerActions(t)
	if len(declared) == 0 {
		t.Fatal("census found no TimerAction constants; the discovery is broken, not the tree")
	}
	emitted := timerDeciderEmissions(t)

	union := map[string]bool{}
	for _, actions := range emitted {
		for action := range actions {
			union[action] = true
		}
	}
	for _, action := range declared {
		if !union[action] {
			t.Errorf("TimerAction constant %s is declared but no session.Decide* function emits it.\n"+
				"Either a decider gained a rung the lane's twins have not learned, or the constant is dead and should be deleted.", action)
		}
	}
	for action := range union {
		if !censusContains(declared, action) {
			t.Errorf("decider emits %s, which is not in the TimerAction const block; the census discovery has drifted", action)
		}
	}
}

// TestTimerDecisionFieldsAreConsumedByEveryLadderArm censuses the other half of
// the ladder contract. TimerDecision carries side-effect fields beyond Action —
// CancelDrain and SkipWakePass are set only by DecideIdleTimeout's
// pending-interaction rung — and an arm that reads Action but ignores a field is
// the same silent-subset shape as an unhandled rung, with even less signal: a
// new field on the struct is a zero value everywhere, so nothing fails.
func TestTimerDecisionFieldsAreConsumedByEveryLadderArm(t *testing.T) {
	var fields []string
	dt := reflect.TypeOf(sessionpkg.TimerDecision{})
	for i := 0; i < dt.NumField(); i++ {
		fields = append(fields, dt.Field(i).Name)
	}
	sort.Strings(fields)

	// Fields an arm may legitimately not read, with the reason. Keyed by arm
	// key; "*" applies to every arm.
	exemptions := map[string]map[string]string{
		"session_reconciler.go:reconcileSessionBeadsTracedWithNamedDemand[DecideMaxSessionAge]": {
			"CancelDrain":  "DecideMaxSessionAge never sets it: only DecideIdleTimeout's PendingYes rung does",
			"SkipWakePass": "DecideMaxSessionAge never sets it: only DecideIdleTimeout's PendingYes rung does",
		},
		"session_deadline_reconcile.go:decideExactSessionDeadline[DecideAssignedWorkExhausted+DecideIdleTimeout+DecideMaxSessionAge]": {
			"TraceOutcome": "recordExactSessionDeadlineTrace takes the whole decision and reads it through timerTraceCodes",
			// ga-f7v2ft.181 adjudicated both pending-interaction side effects:
			// each is UNREACHABLE-as-effect for this family, and each is now
			// asserted rather than assumed. Consuming them at the keyed site
			// would be dead code, so the exemption is the honest record — but
			// only because a test fails when its premise stops holding.
			"CancelDrain":  "ga-f7v2ft.181: PROVED unreachable-as-effect. reconcileExactSessionDeadlineStop yields the row whenever DrainTracker holds an entry for it, and it does so BEFORE decideExactSessionDeadline — its only caller — so wherever CancelDrain can be set there is no drain to cancel and legacy's own cancelSessionDrainInfo returns false. Asserted by TestKeyedDeadlineCancelDrainIsUnreachableByConstruction, including that the rung still fires (a vacuous exemption fails there too)",
			"SkipWakePass": "ga-f7v2ft.181: PROVED structurally satisfied. The dispatch switch claims a fired-deadline row for D-DEADLINE above D-SLEEP so the draining family never runs on the key, and D-SLEEP independently defers a ready pending interaction on its A6 active-use rung. Asserted by TestKeyedDeadlineSkipWakePassIsStructural, whose third leg drains the same fixture once both are withdrawn",
			"SleepReason":  "read at the call site (persistExactSessionDeadlineSleep via SleepPatch), outside decideExactSessionDeadline's block",
		},
	}

	fired := map[string]bool{}
	for _, arm := range discoverTimerLadderArms(t) {
		key := arm.key()
		read := timerDecisionFieldsRead(t, arm)
		for _, field := range fields {
			if read[field] {
				continue
			}
			if reason, exempt := exemptions[key][field]; exempt {
				fired[key+"/"+field] = true
				t.Logf("exemption: %s does not read TimerDecision.%s — %s", key, field, reason)
				continue
			}
			t.Errorf("%s (%s:%d) never reads TimerDecision.%s.\n"+
				"A decision field an arm ignores is an effect that silently does not happen.\n"+
				"Consume it, or record an exemption with its reason.",
				key, arm.File, arm.Line, field)
		}
	}
	assertNoStaleExemptions(t, "TimerDecision field exemptions", exemptions, fired)
}

// ---------------------------------------------------------------------------
// Census 2: AwakeInput builder twins
// ---------------------------------------------------------------------------

// awakeInputBuilders are the two field-by-field constructions of AwakeInput:
// the legacy tick's bridge and the detector sweep's copy. Each entry names the
// builder's ENTRY function only. The helpers each one delegates population to —
// the detector's three Fill passes, and the shared
// fillAwakeNamedSessionWorkQueue derivation both call (ga-f7v2ft.180) — are
// discovered from the call graph rather than declared here, so a builder that
// stops CALLING a shared derivation goes red instead of continuing to claim its
// fields by listing it.
var awakeInputBuilders = map[string][]string{
	"legacy":   {"buildAwakeInputFromReconciler"},
	"detector": {"detectorAwakeSet"},
}

// awakeInputFieldExemptions records a field one builder populates and the other
// deliberately does not, with the reason.
//
// An entry here is a promise, not a shrug: the detector may leave a field empty
// only because every family that consumes the resulting projection RE-PAYS the
// missing observation handler-side, for its own key. Each reason therefore names
// the re-payment site per family, so a family added later without one is a
// missing citation rather than an invisible vacancy. Both entries below are the
// same shape — the sweep refuses fleet-wide provider I/O (DETECTOR.md §2), and
// the handler buys the one probe it needs for the one key it holds.
var awakeInputFieldExemptions = map[string]map[string]string{
	"detector": {
		"AttachedSessions": "ga-f7v2ft.161 (council B2): detectorAwakeSet leaves it empty because attachment is a per-session provider probe the sweep may not pay fleet-wide (DETECTOR.md §2; §3b D-SLEEP \"probe/pending arms unpredicted\"). Every consuming family re-pays it handler-side: D-SLEEP's drain arm and D-ORPHAN's drain arm through exactSessionActiveUseDeferralReason (session_sleep_reconcile.go, session_orphan_drain_reconcile.go), and D-DRAIN's third cancel arm through exactSessionUserAttached (session_drain_reconcile.go) — the arm that was reading a WakeAttached the projection can never carry. D-WAKE consumes the same view but only ever starts a session, which an attached row does not need rescuing from. Recordable only WITH that D-DRAIN re-pay: without it this exemption would certify the force-stop of an attached session",
		"PendingSessions":  "ga-f7v2ft.161 (council B2, same shape): detectorAwakeSet leaves it empty because pendingInteractionReady is per-session provider I/O with the same fleet-wide cost. Re-paid handler-side by D-SLEEP's and D-ORPHAN's drain arms through exactSessionActiveUseDeferralReason, and by D-DRAIN's FIRST cancel arm through pendingInteractionKeepsAwakeInfo (session_drain_reconcile.go) — which is why D-DRAIN's pending rescue was never reachable-only-by-view the way its attachment rescue was",
	},
}

// TestAwakeInputBuilderTwinsPopulateTheSameFields ties the detector's AwakeInput
// construction to the legacy bridge's. Both hand the SAME pure function
// (ComputeAwakeSet) its input; a field one builder sets and the other leaves at
// its zero value is a wake/sleep decision the two arms answer differently for
// the same row, with no compile error and no test — both are struct literals
// with named fields, so a new field is simply absent on one side.
func TestAwakeInputBuilderTwinsPopulateTheSameFields(t *testing.T) {
	var fields []string
	it := reflect.TypeOf(AwakeInput{})
	for i := 0; i < it.NumField(); i++ {
		fields = append(fields, it.Field(i).Name)
	}
	sort.Strings(fields)

	_, files := parseCmdGCPackage(t)
	populated := map[string]map[string]bool{}
	for builder, funcs := range awakeInputBuilders {
		populated[builder] = map[string]bool{}
		for _, name := range funcs {
			decl := censusFindFunc(files, name)
			if decl == nil {
				t.Fatalf("AwakeInput builder %q (%s) not found; the census registry has drifted from the tree", name, builder)
			}
			for _, fn := range append([]*ast.FuncDecl{decl}, awakeInputPopulationHelpers(files, decl)...) {
				for field := range structFieldsPopulated(fn, "AwakeInput", fields) {
					populated[builder][field] = true
				}
			}
		}
		if len(populated[builder]) == 0 {
			t.Fatalf("census found no AwakeInput fields populated by %q; the discovery is broken, not the tree", builder)
		}
	}

	firedAwake := map[string]bool{}
	for _, field := range fields {
		for builder := range awakeInputBuilders {
			if populated[builder][field] {
				continue
			}
			other := ""
			for candidate := range awakeInputBuilders {
				if candidate != builder && populated[candidate][field] {
					other = candidate
				}
			}
			if other == "" {
				// Neither side populates it: not a twin divergence. A field no
				// builder sets is covered by ComputeAwakeSet's own tests.
				continue
			}
			if reason, exempt := awakeInputFieldExemptions[builder][field]; exempt {
				firedAwake[builder+"/"+field] = true
				t.Logf("exemption: the %s builder does not populate AwakeInput.%s — %s", builder, field, reason)
				continue
			}
			t.Errorf("the %s AwakeInput builder does not populate %s, which the %s builder does.\n"+
				"Both feed the same ComputeAwakeSet, so the two arms answer wake/sleep differently for the same row.\n"+
				"Populate it, or record an exemption with its reason in awakeInputFieldExemptions.",
				builder, field, other)
		}
	}
	assertNoStaleExemptions(t, "awakeInputFieldExemptions", awakeInputFieldExemptions, firedAwake)
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

const lifecycleTimersPath = "../../internal/session/lifecycle_timers.go"

func parseLifecycleTimers(t *testing.T) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, lifecycleTimersPath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", lifecycleTimersPath, err)
	}
	return file
}

// declaredTimerActions returns the TimerAction const block's members, in
// declaration order.
func declaredTimerActions(t *testing.T) []string {
	t.Helper()
	file := parseLifecycleTimers(t)
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		block := false
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := value.Type.(*ast.Ident); ok && ident.Name == "TimerAction" {
				block = true
			}
			if !block {
				continue
			}
			for _, name := range value.Names {
				names = append(names, name.Name)
			}
		}
	}
	return names
}

// timerDeciderEmissions maps each exported session.Decide* timer function to the
// set of TimerAction constants it can return, following one level of same-file
// helper calls so deferDecision's TimerActionDefer is attributed to its callers.
func timerDeciderEmissions(t *testing.T) map[string]map[string]bool {
	t.Helper()
	file := parseLifecycleTimers(t)
	declared := map[string]bool{}
	for _, name := range declaredTimerActions(t) {
		declared[name] = true
	}

	bodies := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil {
			bodies[fn.Name.Name] = fn
		}
	}

	actionsIn := func(fn *ast.FuncDecl) map[string]bool {
		found := map[string]bool{}
		ast.Inspect(fn, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok && declared[ident.Name] {
				found[ident.Name] = true
			}
			return true
		})
		return found
	}

	emissions := map[string]map[string]bool{}
	for name, fn := range bodies {
		if !strings.HasPrefix(name, "Decide") {
			continue
		}
		actions := actionsIn(fn)
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			helper, ok := bodies[callee.Name]
			if !ok || helper == fn {
				return true
			}
			for action := range actionsIn(helper) {
				actions[action] = true
			}
			return true
		})
		if len(actions) > 0 {
			emissions[name] = actions
		}
	}
	if len(emissions) == 0 {
		t.Fatal("census found no timer deciders in internal/session; the discovery is broken, not the tree")
	}
	return emissions
}

// parseCmdGCPackage parses the production sources of cmd/gc once, keyed by
// base filename. Every result shares one FileSet so positions stay comparable.
func parseCmdGCPackage(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading cmd/gc: %v", err)
	}
	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files[filepath.Base(name)] = file
	}
	if len(files) == 0 {
		t.Fatal("census parsed no cmd/gc production sources; the discovery is broken, not the tree")
	}
	return fset, files
}

func censusFindFunc(files map[string]*ast.File, name string) *ast.FuncDecl {
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				return fn
			}
		}
	}
	return nil
}

// timerActionNames returns the TimerAction constants a node mentions, whatever
// package qualifier it uses.
func timerActionNames(node ast.Node) map[string]bool {
	found := map[string]bool{}
	if node == nil {
		return found
	}
	ast.Inspect(node, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && strings.HasPrefix(sel.Sel.Name, "TimerAction") {
			found[sel.Sel.Name] = true
			return false
		}
		if ident, ok := n.(*ast.Ident); ok && strings.HasPrefix(ident.Name, "TimerAction") {
			found[ident.Name] = true
		}
		return true
	})
	return found
}

// discoverTimerLadderArms finds every gather loop in cmd/gc that drives a timer
// ladder, and reports the block around it as the arm.
func discoverTimerLadderArms(t *testing.T) []timerLadderArm {
	t.Helper()
	fset, files := parseCmdGCPackage(t)

	var arms []timerLadderArm
	for base, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var stack []ast.Node
			ast.Inspect(fn, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				loop, ok := n.(*ast.ForStmt)
				if !ok || loop.Cond == nil {
					return true
				}
				handled := timerActionNames(loop.Cond)
				if len(handled) == 0 {
					return true
				}
				cases := map[string]bool{}
				for _, clause := range loop.Body.List {
					if sw, ok := clause.(*ast.SwitchStmt); ok {
						for _, item := range sw.Body.List {
							cc, ok := item.(*ast.CaseClause)
							if !ok {
								continue
							}
							for _, expr := range cc.List {
								for name := range timerActionNames(expr) {
									cases[name] = true
								}
							}
						}
					}
				}
				// The arm is the innermost enclosing block that is not the
				// loop's own body.
				var arm ast.Node = fn.Body
				for i := len(stack) - 2; i >= 0; i-- {
					if block, ok := stack[i].(*ast.BlockStmt); ok {
						arm = block
						break
					}
				}
				arms = append(arms, timerLadderArm{
					File:     base,
					Func:     fn.Name.Name,
					Deciders: deciderNamesIn(arm),
					Handled:  handled,
					Cases:    cases,
					Named:    timerActionNames(arm),
					Line:     fset.Position(loop.Pos()).Line,
					Block:    arm,
				})
				return true
			})
		}
	}
	sort.Slice(arms, func(i, j int) bool { return arms[i].key() < arms[j].key() })
	return arms
}

// deciderNamesIn returns the sorted set of session.Decide* functions referenced
// in a node, which is how an arm declares which ladders it drives — including
// the `decide := sessionpkg.DecideIdleTimeout` indirection the keyed twin uses.
func deciderNamesIn(node ast.Node) []string {
	found := map[string]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(sel.Sel.Name, "Decide") {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || (pkg.Name != "sessionpkg" && pkg.Name != "session") {
			return true
		}
		found[sel.Sel.Name] = true
		return false
	})
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// timerDecisionFieldsRead reports which TimerDecision fields an arm reads. A
// call that passes the whole decision to a helper counts as reading every field
// that helper reads, one level deep — timerTraceCodes and
// recordExactSessionDeadlineTrace are both used that way.
func timerDecisionFieldsRead(t *testing.T, arm timerLadderArm) map[string]bool {
	t.Helper()
	_, files := parseCmdGCPackage(t)

	dt := reflect.TypeOf(sessionpkg.TimerDecision{})
	isField := func(name string) bool {
		_, ok := dt.FieldByName(name)
		return ok
	}

	read := map[string]bool{}
	collect := func(node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			base, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if !strings.HasPrefix(base.Name, "dec") && !strings.HasPrefix(base.Name, "decision") {
				return true
			}
			if isField(sel.Sel.Name) {
				read[sel.Sel.Name] = true
			}
			return true
		})
	}
	collect(arm.Block)

	// Helpers handed the whole decision.
	ast.Inspect(arm.Block, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		passesDecision := false
		for _, arg := range call.Args {
			ident, ok := arg.(*ast.Ident)
			if ok && (strings.HasPrefix(ident.Name, "dec") || strings.HasPrefix(ident.Name, "decision")) {
				passesDecision = true
			}
		}
		if !passesDecision {
			return true
		}
		if helper := censusFindFunc(files, callee.Name); helper != nil {
			collect(helper)
		}
		return true
	})
	return read
}

// awakeInputPopulationHelpers returns the same-package functions a builder hands
// its AwakeInput to, one level deep. Following the CALL rather than trusting a
// registry entry is what makes a shared derivation count only for the builder
// that actually invokes it: a builder that drops the call loses the field and
// the census reports the divergence, which is exactly what a registry listing
// would have concealed.
func awakeInputPopulationHelpers(files map[string]*ast.File, fn *ast.FuncDecl) []*ast.FuncDecl {
	var helpers []*ast.FuncDecl
	seen := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee, ok := call.Fun.(*ast.Ident)
		if !ok || seen[callee.Name] {
			return true
		}
		if !censusPassesAwakeInput(call.Args) {
			return true
		}
		helper := censusFindFunc(files, callee.Name)
		if helper == nil || helper == fn {
			return true
		}
		seen[callee.Name] = true
		helpers = append(helpers, helper)
		return true
	})
	return helpers
}

// censusPassesAwakeInput reports whether a call hands over the ADDRESS of the
// builder's own AwakeInput. By-address is the only form that can populate the
// caller's struct — ComputeAwakeSet takes the same value by copy and only reads
// it — so requiring the `&` is what keeps a consumer out of the producer census.
func censusPassesAwakeInput(args []ast.Expr) bool {
	for _, arg := range args {
		unary, ok := arg.(*ast.UnaryExpr)
		if !ok || unary.Op != token.AND {
			continue
		}
		if ident, ok := unary.X.(*ast.Ident); ok && (ident.Name == "input" || ident.Name == "in") {
			return true
		}
	}
	return false
}

// structFieldsPopulated reports which of the named struct's fields a function
// puts CONTENT in, whether through a composite literal or a field assignment.
//
// Allocating an empty container is not content (censusEmptyContainer): an empty
// map answers every lookup exactly as a nil map does, so a builder that writes
// `AttachedSessions: map[string]bool{}` and never fills it has left the field at
// its effective zero value and the twin divergence is real. Counting that stub as
// population is how the detector's permanently-empty AttachedSessions passed this
// census while a downstream cancel arm read a verdict the projection could never
// carry (ga-f7v2ft.161, council finding F6). A field allocated empty here still
// counts once some index or assignment puts a value in it — which is how
// RunningSessions, allocated by both builders and filled by both, stays green.
func structFieldsPopulated(fn *ast.FuncDecl, structName string, fields []string) map[string]bool {
	known := map[string]bool{}
	for _, field := range fields {
		known[field] = true
	}
	populated := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			ident, ok := node.Type.(*ast.Ident)
			if !ok || ident.Name != structName {
				return true
			}
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok || censusEmptyContainer(kv.Value) {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && known[key.Name] {
					populated[key.Name] = true
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				// Pair only when the assignment is positional; a multi-value
				// call has one RHS for many LHS and nothing to inspect per field.
				if len(node.Rhs) == len(node.Lhs) && censusEmptyContainer(node.Rhs[i]) {
					continue
				}
				collectPopulatedField(lhs, known, populated)
			}
		}
		return true
	})
	return populated
}

// censusEmptyContainer reports whether an expression only ALLOCATES storage —
// `map[K]V{}`, `[]T{}`, or any `make(...)` — without putting a value in it. Both
// forms have to be recognized or the rule is only a ban on one spelling: the
// legacy bridge allocates its three session maps with `make`, so a detector that
// switched from the empty literal to `make` would re-open the same vacancy with
// the census still green.
func censusEmptyContainer(expr ast.Expr) bool {
	switch node := expr.(type) {
	case *ast.CompositeLit:
		return len(node.Elts) == 0
	case *ast.CallExpr:
		ident, ok := node.Fun.(*ast.Ident)
		return ok && ident.Name == "make"
	}
	return false
}

func collectPopulatedField(expr ast.Expr, known, populated map[string]bool) {
	switch node := expr.(type) {
	case *ast.IndexExpr:
		collectPopulatedField(node.X, known, populated)
	case *ast.SelectorExpr:
		base, ok := node.X.(*ast.Ident)
		if !ok {
			return
		}
		if base.Name != "input" && base.Name != "in" {
			return
		}
		if known[node.Sel.Name] {
			populated[node.Sel.Name] = true
		}
	}
}

func censusContains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
