package main

// Wiring tests for the ralph finalize verdict. A loop whose earlier iteration
// failed but whose final iteration passed has recovered: its superseded members
// must not fail the workflow root, while a loop that never passes still does.
// Each test compiles a real formula, drives the workflow through the real
// control dispatcher (dispatch.ProcessControl) over a beads.MemStore, and asserts
// on the closed beads and on the source bead the finalizer stamps.
//
// The tests mutate package-level seams (slingPokeController, the feature flags),
// so none of them is parallel.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/runtime"
)

const wiringFormula = "ralph-finalize-wiring"

// wiringBodyChildrenTOML is the body of every loop: m1; m2 needs m1; m3 is
// independent; m4 needs m2 and m3.
const wiringBodyChildrenTOML = `[[steps.children]]
id = "m1"
title = "Member one"

[[steps.children]]
id = "m2"
title = "Member two"
needs = ["m1"]

[[steps.children]]
id = "m3"
title = "Member three"

[[steps.children]]
id = "m4"
title = "Member four"
needs = ["m2", "m3"]
`

// wiringScopeMetadata declares the enclosing plain scope's membership on a loop,
// as mol-scoped-work declares it for its members (minus the on_fail default).
const wiringScopeMetadata = `{ "gc.scope_ref" = "body", "gc.scope_role" = "member" }`

func wiringTOMLList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = fmt.Sprintf("%q", item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func wiringCheckTOML(id string, maxAttempts int) string {
	return fmt.Sprintf("[steps.check]\nmax_attempts = %d\n\n[steps.check.check]\nmode = \"exec\"\npath = \".gc/scripts/%s-check.sh\"\ntimeout = \"30s\"\n\n", maxAttempts, id)
}

// wiringRalphLoopTOML is the ralph-with-children step "loop". metadata is a raw TOML
// inline table for the step itself ("" = none).
func wiringRalphLoopTOML(maxAttempts int, metadata string) string {
	const id = "loop"
	var b strings.Builder
	fmt.Fprintf(&b, "[[steps]]\nid = %q\ntitle = %q\n", id, id)
	if metadata != "" {
		fmt.Fprintf(&b, "metadata = %s\n", metadata)
	}
	b.WriteString("\n")
	b.WriteString(wiringCheckTOML(id, maxAttempts))
	b.WriteString(wiringBodyChildrenTOML)
	return b.String()
}

func wiringPlainStepTOML(id string, needs []string, metadata string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[[steps]]\nid = %q\ntitle = %q\n", id, id)
	if len(needs) > 0 {
		fmt.Fprintf(&b, "needs = %s\n", wiringTOMLList(needs))
	}
	if metadata != "" {
		fmt.Fprintf(&b, "metadata = %s\n", metadata)
	}
	return b.String()
}

func wiringFormulaTOML(steps ...string) string {
	return fmt.Sprintf("formula = %q\ndescription = \"ralph finalize wiring\"\n\n[requires]\nformula_compiler = \">=2.0.0\"\n\n", wiringFormula) + strings.Join(steps, "\n")
}

// wiringTopologyFormula builds the loop plus what follows it: topology (ii) is a
// downstream plain step, topology (iii) an enclosing plain scope whose body
// latches on the loop.
func wiringTopologyFormula(topology string, maxAttempts int) string {
	if topology == "in_scope" {
		return wiringFormulaTOML(
			wiringRalphLoopTOML(maxAttempts, wiringScopeMetadata),
			wiringPlainStepTOML("body", []string{"loop"}, `{ "gc.kind" = "scope", "gc.scope_name" = "worktree", "gc.scope_role" = "body" }`),
		)
	}
	return wiringFormulaTOML(
		wiringRalphLoopTOML(maxAttempts, ""),
		wiringPlainStepTOML("after", []string{"loop"}, ""),
	)
}

// wiringScenario is one run. failMembers maps "<loop>.<member>@<iteration>" to a
// failure class ("" = none). checkPassIter maps a loop id to the first
// GC_ITERATION on which its check script exits 0 (0 = never); every ralph loop of
// the formula needs an entry.
type wiringScenario struct {
	formula       string
	checkPassIter map[string]int
	failMembers   map[string]string
}

// wiringControl is one ProcessControl result that did work, in call order.
type wiringControl struct {
	id     string
	kind   string
	ref    string
	action string
}

type wiringResult struct {
	store       *beads.MemStore
	rootID      string
	sourceID    string
	finalizerID string
	trace       []wiringControl
	all         []beads.Bead
}

var wiringWorkerRef = regexp.MustCompile(`^(.*)\.iteration\.([0-9]+)\.([^.]+)$`)

// wiringFailKey derives "<loop>.<member>@<iteration>" from a worker bead's step
// ref, so a key names its loop as well as its member.
func wiringFailKey(ref string) string {
	m := wiringWorkerRef.FindStringSubmatch(ref)
	if m == nil {
		return ref
	}
	loop := m[1]
	if i := strings.LastIndex(loop, "."); i >= 0 {
		loop = loop[i+1:]
	}
	return loop + "." + m[3] + "@" + m[2]
}

// startWiringWorkflow slings the run's formula into a fresh MemStore and
// creates the source bead the finalizer stamps its failure diagnostics onto. Sling
// clears gc.source_bead_id on graph roots, so the test sets it after doSling.
func startWiringWorkflow(t *testing.T, sc wiringScenario) (*beads.MemStore, string, string, string) {
	t.Helper()
	cityPath := t.TempDir()
	scriptDir := filepath.Join(cityPath, ".gc", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for loop, pass := range sc.checkPassIter {
		script := fmt.Sprintf("#!/bin/sh\n[ %d -gt 0 ] && [ \"$GC_ITERATION\" -ge %d ] && exit 0\nexit 1\n", pass, pass)
		if err := os.WriteFile(filepath.Join(scriptDir, loop+"-check.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The exec check resolves its script against the city root, which it finds by
	// city.toml.
	cityToml := "[workspace]\nname = \"test-city\"\nprovider = \"claude\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n\n[[agent]]\nname = \"worker\"\nmax_active_sessions = 1\n\n[[agent]]\nname = \"control-dispatcher\"\nmax_active_sessions = 1\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, wiringFormula+".toml"), []byte(sc.formula), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := newFakeRunner()
	cfg := &config.City{
		Daemon:        config.DaemonConfig{FormulaV2: boolPtr(true)},
		Workspace:     config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{City: []string{formulaDir}},
		Agents:        []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}},
	}
	addTestControlDispatcherAgents(cfg, "")
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })

	store := beads.NewMemStore()
	item, err := store.Create(beads.Bead{Title: "wiring item", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	convoy, err := store.Create(beads.Bead{Title: "wiring convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoycore.TrackItem(store, convoy.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	deps, stdout, stderr := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = store
	deps.CityPath = cityPath
	worker, ok := resolveAgentIdentity(cfg, "worker", "")
	if !ok {
		t.Fatal("resolveAgentIdentity(worker) failed")
	}
	oldPoke := slingPokeController
	slingPokeController = func(string) error { return nil }
	t.Cleanup(func() { slingPokeController = oldPoke })

	opts := testOpts(worker, convoy.ID)
	opts.OnFormula = wiringFormula
	if code := doSling(opts, deps, store, stdout, stderr); code != 0 {
		t.Fatalf("doSling returned %d; stderr=%s", code, stderr.String())
	}
	roots, err := store.ListByMetadata(map[string]string{"gc.input_convoy_id": convoy.ID, "gc.kind": "workflow"}, 1)
	if err != nil || len(roots) != 1 {
		t.Fatalf("workflow roots = %d err=%v", len(roots), err)
	}
	source, err := store.Create(beads.Bead{Title: "wiring source bead", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(roots[0].ID, beadmeta.SourceBeadIDMetadataKey, source.ID); err != nil {
		t.Fatal(err)
	}
	return store, roots[0].ID, source.ID, cityPath
}

// runWiringWorkflow drives the workflow to completion: it processes every ready
// control bead, then plays the worker, closing each worker bead per failMembers.
func runWiringWorkflow(t *testing.T, sc wiringScenario) *wiringResult {
	t.Helper()
	store, rootID, sourceID, cityPath := startWiringWorkflow(t, sc)
	res := &wiringResult{store: store, rootID: rootID, sourceID: sourceID}
	workerSession := "worker"
	for step := 0; step < 400; step++ {
		if mustGetMemBead(t, store, rootID).Status == "closed" {
			break
		}
		progressed := false
		for _, bead := range memGraphReady(t, store) {
			if !graphroute.IsControlDispatcherKind(bead.Metadata["gc.kind"]) {
				continue
			}
			result, err := dispatch.ProcessControl(store, bead, dispatch.ProcessOptions{CityPath: cityPath})
			if err != nil {
				if errors.Is(err, dispatch.ErrControlPending) {
					continue
				}
				t.Fatalf("ProcessControl(%s %s): %v", bead.ID, beadRef(bead), err)
			}
			if !result.Processed {
				continue
			}
			progressed = true
			res.trace = append(res.trace, wiringControl{id: bead.ID, kind: bead.Metadata["gc.kind"], ref: beadRef(bead), action: result.Action})
			if bead.Metadata["gc.kind"] == "workflow-finalize" {
				res.finalizerID = bead.ID
			}
		}
		ready := memGraphReady(t, store)
		for {
			if i, ok := firstClaimableGraphWorkerBead(ready, workerSession); ok {
				candidate := ready[i]
				writer, _ := beads.ConditionalWriterFor(store)
				if err := writer.UpdateIfMatch(candidate.ID, candidate.Revision, beads.UpdateOpts{Assignee: &workerSession}); err != nil {
					t.Fatalf("claim %s: %v", candidate.ID, err)
				}
				ready[i] = mustGetMemBead(t, store, candidate.ID)
			}
			bead, ok, err := selectExecutableGraphWorkerBead(ready, workerSession)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			class, fail := sc.failMembers[wiringFailKey(beadRef(bead))]
			if fail {
				md := map[string]string{"gc.outcome": "fail", "gc.failure_reason": "wiring-test"}
				if class != "" {
					md["gc.failure_class"] = class
				}
				if err := store.SetMetadataBatch(bead.ID, md); err != nil {
					t.Fatal(err)
				}
			} else if err := store.SetMetadata(bead.ID, "gc.outcome", "pass"); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(bead.ID); err != nil {
				t.Fatal(err)
			}
			progressed = true
			ready = memGraphReady(t, store)
		}
		if progressed {
			continue
		}
		var summary []string
		for _, bead := range ready {
			summary = append(summary, fmt.Sprintf("%s kind=%s ref=%s assignee=%s", bead.ID, bead.Metadata["gc.kind"], beadRef(bead), bead.Assignee))
		}
		all, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
		for _, b := range all {
			t.Logf("bead %s ref=%s kind=%q status=%s outcome=%q assignee=%q routed_to=%q scope_ref=%q scope_role=%q ralph_step_id=%q attempt=%q logical=%q", b.ID, beadRef(b), b.Metadata["gc.kind"], b.Status, b.Metadata["gc.outcome"], b.Assignee, b.Metadata["gc.routed_to"], b.Metadata["gc.scope_ref"], b.Metadata["gc.scope_role"], b.Metadata["gc.ralph_step_id"], b.Metadata["gc.attempt"], b.Metadata["gc.logical_bead_id"])
		}
		for _, c := range res.trace {
			t.Logf("trace %s kind=%s ref=%s action=%s", c.id, c.kind, c.ref, c.action)
		}
		t.Fatalf("workflow made no progress; ready=%v", summary)
	}
	if mustGetMemBead(t, store, rootID).Status != "closed" {
		t.Fatal("workflow root not closed within the step budget")
	}
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.ID == rootID || b.Metadata["gc.root_bead_id"] == rootID {
			res.all = append(res.all, b)
		}
	}
	return res
}

func (r *wiringResult) ref(suffix string) string { return wiringFormula + "." + suffix }

// find returns the root-view bead whose step ref is wiringFormula.<suffix>.
func (r *wiringResult) find(suffix string) (beads.Bead, bool) {
	for _, b := range r.all {
		if beadRef(b) == r.ref(suffix) {
			return b, true
		}
	}
	return beads.Bead{}, false
}

func (r *wiringResult) get(t *testing.T, suffix string) beads.Bead {
	t.Helper()
	b, ok := r.find(suffix)
	if !ok {
		t.Fatalf("no bead with step ref %s", r.ref(suffix))
	}
	return b
}

func (r *wiringResult) root(t *testing.T) beads.Bead {
	t.Helper()
	return mustGetMemBead(t, r.store, r.rootID)
}

func (r *wiringResult) source(t *testing.T) beads.Bead {
	t.Helper()
	return mustGetMemBead(t, r.store, r.sourceID)
}

// lastAction is the final recorded ProcessControl action for a bead id and its
// position in the trace. A ralph control returns "retry" before its final result.
func (r *wiringResult) lastAction(id string) (string, int) {
	for i := len(r.trace) - 1; i >= 0; i-- {
		if r.trace[i].id == id {
			return r.trace[i].action, i
		}
	}
	return "", -1
}

func (r *wiringResult) refOfID(t *testing.T, id string) string {
	t.Helper()
	return beadRef(mustGetMemBead(t, r.store, id))
}

// finalizerBlockers lists the step refs of the finalizer's direct blocks
// dependencies, sorted.
func (r *wiringResult) finalizerBlockers(t *testing.T) []string {
	t.Helper()
	deps, err := r.store.DepList(r.finalizerID, "down")
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, d := range deps {
		if d.Type == "blocks" {
			refs = append(refs, r.refOfID(t, d.DependsOnID))
		}
	}
	sort.Strings(refs)
	return refs
}

// sourceSubjectRef is the step ref of the bead the source bead's
// gc.failure_subject names ("" when the source carries no subject).
func (r *wiringResult) sourceSubjectRef(t *testing.T) string {
	t.Helper()
	subject := r.source(t).Metadata[beadmeta.FailureSubjectMetadataKey]
	if subject == "" {
		return ""
	}
	return r.refOfID(t, subject)
}

// logMemberOrder logs the root view's creation order (what the finalize scan
// walks: beads.DirectMembers is the root, then List order).
func (r *wiringResult) logMemberOrder(t *testing.T, label string) {
	t.Helper()
	members, err := beads.DirectMembers(r.store, r.rootID)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]string, 0, len(members))
	for _, m := range members {
		refs = append(refs, strings.TrimPrefix(beadRef(m), wiringFormula+"."))
	}
	t.Logf("%s rootViewMembers order: %v", label, refs)
}

func (r *wiringResult) assertSourceFailureKeysEmpty(t *testing.T) {
	t.Helper()
	src := r.source(t)
	if src.Status != "closed" {
		t.Errorf("source bead status = %q, want closed", src.Status)
	}
	for _, key := range []string{beadmeta.FailureReasonMetadataKey, beadmeta.FailureClassMetadataKey, beadmeta.FailureSubjectMetadataKey} {
		if v := src.Metadata[key]; v != "" {
			t.Errorf("source bead %s = %q, want empty", key, v)
		}
	}
}

// assertControlPassedBeforeFinalizer checks the ordering contract: the ralph
// control's final recorded result is "pass", and it precedes the finalizer's.
func (r *wiringResult) assertControlPassedBeforeFinalizer(t *testing.T, control beads.Bead) {
	t.Helper()
	action, at := r.lastAction(control.ID)
	if action != "pass" {
		t.Errorf("ralph control %s final action = %q, want pass", control.ID, action)
	}
	finAction, finAt := r.lastAction(r.finalizerID)
	if at < 0 || finAt < 0 || at > finAt {
		t.Errorf("ralph control result index %d, finalizer result index %d, want control first", at, finAt)
	}
	if finAction != "workflow-pass" {
		t.Errorf("finalizer action = %q, want workflow-pass", finAction)
	}
}

func ralphAssertOutcome(t *testing.T, what string, b beads.Bead, want string) {
	t.Helper()
	if b.Status != "closed" {
		t.Errorf("%s status = %q, want closed", what, b.Status)
	}
	if got := b.Metadata["gc.outcome"]; got != want {
		t.Errorf("%s gc.outcome = %q, want %q", what, got, want)
	}
}

// bodyMembers returns the worker members of a loop's iteration (refs
// <loop>.iteration.<n>.<member> with no further dot segment, excluding
// scope-checks).
func (r *wiringResult) bodyMembers(loop string) []beads.Bead {
	var out []beads.Bead
	prefix := r.ref(loop + ".iteration.")
	for _, b := range r.all {
		ref := beadRef(b)
		if !strings.HasPrefix(ref, prefix) || b.Metadata["gc.kind"] == "scope-check" || b.Metadata["gc.kind"] == "scope" {
			continue
		}
		if wiringWorkerRef.MatchString(ref) {
			out = append(out, b)
		}
	}
	return out
}

func wiringAssertNoLogicalID(t *testing.T, members []beads.Bead) {
	t.Helper()
	if len(members) == 0 {
		t.Fatal("no body members found")
	}
	for _, m := range members {
		if v := m.Metadata["gc.logical_bead_id"]; v != "" {
			t.Errorf("%s carries gc.logical_bead_id=%q, want none", beadRef(m), v)
		}
	}
}

// ralphMetadataDump renders a bead's full metadata as sorted key=value pairs.
func ralphMetadataDump(b beads.Bead) string {
	pairs := make([]string, 0, len(b.Metadata))
	for k, v := range b.Metadata {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, " ")
}

// assertRecoveredLoopIdentity asserts the identity keys the compile and runtime
// stamp on the beads of a loop that failed iteration 1 and passed iteration 2,
// and logs each bead's full key set. It also covers the part of the fix's
// constraint that holds on every build: the control gains no identity key, and
// no iteration-1 bead carries a logical id.
func (r *wiringResult) assertRecoveredLoopIdentity(t *testing.T, control beads.Bead) {
	t.Helper()
	body1 := r.get(t, "loop.iteration.1")
	member1 := r.get(t, "loop.iteration.1.m1")
	body2 := r.get(t, "loop.iteration.2")
	for _, d := range []struct {
		label string
		b     beads.Bead
	}{{"iteration-1 scope body", body1}, {"ralph control", control}, {"iteration-1 member m1", member1}, {"iteration-2 scope body", body2}} {
		t.Logf("identity dump %s %s: %s", d.label, d.b.ID, ralphMetadataDump(d.b))
	}
	want := func(what string, b beads.Bead, key, value string) {
		t.Helper()
		if got := b.Metadata[key]; got != value {
			t.Errorf("%s %s = %q, want %q", what, key, got, value)
		}
	}
	// The iteration scope body: scope kind/role, the loop's identity, the bare
	// step id as control_for, no logical id and no on_fail in iteration 1.
	want("iteration-1 scope body", body1, "gc.kind", "scope")
	want("iteration-1 scope body", body1, "gc.scope_role", "body")
	want("iteration-1 scope body", body1, "gc.ralph_step_id", "loop")
	want("iteration-1 scope body", body1, "gc.attempt", "1")
	want("iteration-1 scope body", body1, "gc.control_for", "loop")
	want("iteration-1 scope body", body1, "gc.logical_bead_id", "")
	want("iteration-1 scope body", body1, "gc.on_fail", "")
	// The control is not a body member: it carries none of the member identity.
	want("ralph control", control, "gc.kind", "ralph")
	want("ralph control", control, "gc.step_id", "loop")
	want("ralph control", control, "gc.logical_bead_id", "")
	want("ralph control", control, "gc.ralph_step_id", "")
	want("ralph control", control, "gc.attempt", "")
	// An iteration-1 body member names its loop and iteration, and is not a
	// logical-id carrier.
	want("iteration-1 member m1", member1, "gc.ralph_step_id", "loop")
	want("iteration-1 member m1", member1, "gc.attempt", "1")
	want("iteration-1 member m1", member1, "gc.scope_ref", "loop.iteration.1")
	want("iteration-1 member m1", member1, "gc.on_fail", "abort_scope")
	want("iteration-1 member m1", member1, "gc.logical_bead_id", "")
	// Iteration 2+: both pointers are the control's BEAD id, not the step id.
	want("iteration-2 scope body", body2, "gc.kind", "scope")
	want("iteration-2 scope body", body2, "gc.scope_role", "body")
	want("iteration-2 scope body", body2, "gc.logical_bead_id", control.ID)
	want("iteration-2 scope body", body2, "gc.control_for", control.ID)
	want("iteration-2 scope body", body2, "gc.ralph_step_id", "loop")
	want("iteration-2 scope body", body2, "gc.attempt", "2")
}

// TestRalphFinalizeRecoveredLoopRootPasses: m1 fails in iteration 1 and the check
// passes on iteration 2, so the loop recovers and the root must finalize pass. The
// failed member stays recorded as failed. The subtest pins that the failed member
// still aborts its own iteration.
func TestRalphFinalizeRecoveredLoopRootPasses(t *testing.T) {
	res := runWiringWorkflow(t, wiringScenario{
		formula:       wiringFormulaTOML(wiringRalphLoopTOML(3, "")),
		checkPassIter: map[string]int{"loop": 2},
		failMembers:   map[string]string{"loop.m1@1": ""},
	})
	res.logMemberOrder(t, "recovered loop")
	for _, c := range res.trace {
		t.Logf("control trace %s kind=%s ref=%s action=%s", c.id, c.kind, c.ref, c.action)
	}
	control := res.get(t, "loop")
	ralphAssertOutcome(t, "ralph control", control, "pass")
	res.assertControlPassedBeforeFinalizer(t, control)
	ralphAssertOutcome(t, "workflow root", res.root(t), "pass")
	res.assertSourceFailureKeysEmpty(t)
	ralphAssertOutcome(t, "iteration 1 m1", res.get(t, "loop.iteration.1.m1"), "fail")
	wiringAssertNoLogicalID(t, res.bodyMembers("loop"))
	res.assertRecoveredLoopIdentity(t, control)

	t.Run("iteration_abort_unchanged", func(t *testing.T) {
		ralphAssertOutcome(t, "iteration 1 m1", res.get(t, "loop.iteration.1.m1"), "fail")
		check := res.get(t, "loop.iteration.1.m1-scope-check")
		if action, _ := res.lastAction(check.ID); action != "scope-fail" {
			t.Errorf("m1 scope-check action = %q, want scope-fail", action)
		}
		const skipReason = "scope member skipped: an earlier member of the same scope failed"
		for _, member := range []string{"m2", "m4"} {
			m := res.get(t, "loop.iteration.1."+member)
			ralphAssertOutcome(t, "iteration 1 "+member, m, "skipped")
			if got := m.Metadata["close_reason"]; got != skipReason {
				t.Errorf("iteration 1 %s close_reason = %q, want %q", member, got, skipReason)
			}
			ralphAssertOutcome(t, "iteration 1 "+member+"-scope-check", res.get(t, "loop.iteration.1."+member+"-scope-check"), "skipped")
		}
		ralphAssertOutcome(t, "iteration 1 m3", res.get(t, "loop.iteration.1.m3"), "pass")
		body := res.get(t, "loop.iteration.1")
		ralphAssertOutcome(t, "iteration 1 scope body", body, "fail")
		// The aborted iteration's scope body carries the members' skip reason as
		// its close_reason; the fix must leave that unchanged.
		const wantBodyCloseReason = "scope member skipped: an earlier member of the same scope failed"
		if got := body.Metadata["close_reason"]; got != wantBodyCloseReason {
			t.Errorf("iteration 1 scope body close_reason = %q, want %q", got, wantBodyCloseReason)
		}
	})
}

// TestRalphFinalizeEveryIterationFailsRootFails: m4 fails in every iteration, so
// the loop exhausts and the root fails through the ralph control, which is a
// direct blocker of the finalizer.
func TestRalphFinalizeEveryIterationFailsRootFails(t *testing.T) {
	res := runWiringWorkflow(t, wiringScenario{
		formula:       wiringFormulaTOML(wiringRalphLoopTOML(3, "")),
		checkPassIter: map[string]int{"loop": 0},
		failMembers:   map[string]string{"loop.m4@1": "", "loop.m4@2": "", "loop.m4@3": ""},
	})
	control := res.get(t, "loop")
	ralphAssertOutcome(t, "ralph control", control, "fail")
	if v := control.Metadata["gc.on_fail"]; v != "" {
		t.Errorf("ralph control gc.on_fail = %q, want none", v)
	}
	if blockers := res.finalizerBlockers(t); !containsString(blockers, res.ref("loop")) {
		t.Errorf("finalizer blocks = %v, want the ralph control %s among them", blockers, res.ref("loop"))
	}
	ralphAssertOutcome(t, "workflow root", res.root(t), "fail")
	if got := res.sourceSubjectRef(t); got != res.ref("loop") {
		t.Errorf("source gc.failure_subject ref = %q, want the ralph control %q", got, res.ref("loop"))
	}
	if st := res.source(t).Status; st == "closed" {
		t.Errorf("source bead status = %q, want open", st)
	}
}

// TestRalphFinalizeCheckRejectsEveryIterationRootFails: no member fails but the
// check rejects every iteration, so the control and the root fail and the source
// bead's failure subject is the control.
func TestRalphFinalizeCheckRejectsEveryIterationRootFails(t *testing.T) {
	res := runWiringWorkflow(t, wiringScenario{
		formula:       wiringFormulaTOML(wiringRalphLoopTOML(2, "")),
		checkPassIter: map[string]int{"loop": 0},
	})
	members := res.bodyMembers("loop")
	if len(members) != 8 {
		t.Errorf("body members = %d, want 8 (4 members x 2 iterations)", len(members))
	}
	for _, m := range members {
		ralphAssertOutcome(t, beadRef(m), m, "pass")
	}
	// A loop whose check never passes fails: control fail, root fail, and the
	// source bead's failure subject is the ralph control.
	ralphAssertOutcome(t, "ralph control", res.get(t, "loop"), "fail")
	ralphAssertOutcome(t, "workflow root", res.root(t), "fail")
	if got := res.sourceSubjectRef(t); got != res.ref("loop") {
		t.Errorf("source gc.failure_subject ref = %q, want the ralph control %q", got, res.ref("loop"))
	}
}

// TestRalphFinalizeCheckRejectsThenPassesRootPasses: the check rejects iteration 1
// but no member fails, so nothing is superseded and the root passes.
func TestRalphFinalizeCheckRejectsThenPassesRootPasses(t *testing.T) {
	res := runWiringWorkflow(t, wiringScenario{
		formula:       wiringFormulaTOML(wiringRalphLoopTOML(3, "")),
		checkPassIter: map[string]int{"loop": 2},
	})
	ralphAssertOutcome(t, "ralph control", res.get(t, "loop"), "pass")
	for _, m := range res.bodyMembers("loop") {
		if m.Metadata["gc.outcome"] == "fail" {
			t.Errorf("%s closed fail, want no failed member", beadRef(m))
		}
	}
	ralphAssertOutcome(t, "workflow root", res.root(t), "pass")
	res.assertSourceFailureKeysEmpty(t)
}

// wiringObservation is what a topology test records about one run; the
// per-topology tests compare it against the expected values field by field.
type wiringObservation struct {
	RootOutcome       string
	ControlOutcome    string
	ControlOnFail     string
	AfterOutcome      string // topology (ii): the downstream step; "" when absent or open
	BodyOutcome       string // topology (iii): the enclosing scope body; "" when absent
	FinalizerBlockers string // sorted step refs, comma-joined
	SubjectRef        string // step ref of SB's gc.failure_subject
	SourceStatus      string
}

func observeWiring(t *testing.T, res *wiringResult) wiringObservation {
	t.Helper()
	obs := wiringObservation{
		RootOutcome:       res.root(t).Metadata["gc.outcome"],
		FinalizerBlockers: strings.TrimPrefix(strings.ReplaceAll(strings.Join(res.finalizerBlockers(t), ","), ","+wiringFormula+".", ","), wiringFormula+"."),
		SubjectRef:        strings.TrimPrefix(res.sourceSubjectRef(t), wiringFormula+"."),
		SourceStatus:      res.source(t).Status,
	}
	control := res.get(t, "loop")
	obs.ControlOutcome = control.Metadata["gc.outcome"]
	obs.ControlOnFail = control.Metadata["gc.on_fail"]
	if b, ok := res.find("after"); ok {
		obs.AfterOutcome = b.Status + "/" + b.Metadata["gc.outcome"]
	}
	if b, ok := res.find("body"); ok {
		obs.BodyOutcome = b.Status + "/" + b.Metadata["gc.outcome"]
	}
	t.Logf("observed %+v", obs)
	return obs
}

func assertObservation(t *testing.T, got, want wiringObservation) {
	t.Helper()
	g, w := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := 0; i < g.NumField(); i++ {
		if g.Field(i).String() != w.Field(i).String() {
			t.Errorf("%s = %q, want %q", g.Type().Field(i).Name, g.Field(i).String(), w.Field(i).String())
		}
	}
}

// TestRalphFinalizeNeverPassesMemberFailsTopologies: m4 fails in every iteration
// of a loop followed by a plain step (downstream_step) and of a loop inside a
// plain scope (in_scope). The loop never passes, so the root fails in both.
func TestRalphFinalizeNeverPassesMemberFailsTopologies(t *testing.T) {
	failEvery := map[string]string{"loop.m4@1": "", "loop.m4@2": "", "loop.m4@3": ""}
	rows := []struct {
		name string
		want wiringObservation
	}{
		{"downstream_step", wiringObservation{
			RootOutcome: "fail", ControlOutcome: "fail", AfterOutcome: "closed/pass",
			FinalizerBlockers: "after,loop,loop.iteration.1.m4-scope-check", SubjectRef: "loop", SourceStatus: "open",
		}},
		{"in_scope", wiringObservation{
			RootOutcome: "fail", ControlOutcome: "fail", BodyOutcome: "closed/fail",
			FinalizerBlockers: "body,loop,loop-scope-check,loop.iteration.1.m4-scope-check", SubjectRef: "loop", SourceStatus: "open",
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			res := runWiringWorkflow(t, wiringScenario{
				formula:       wiringTopologyFormula(row.name, 3),
				checkPassIter: map[string]int{"loop": 0},
				failMembers:   failEvery,
			})
			assertObservation(t, observeWiring(t, res), row.want)
		})
	}
}

// TestRalphFinalizeNeverPassesCheckRejectsTopologies: no member fails but the
// check rejects every iteration, for a loop followed by a plain step
// (downstream_step) and a loop inside a plain scope (in_scope). A control that
// never passes is never exempted, so the verdict must not depend on the topology.
func TestRalphFinalizeNeverPassesCheckRejectsTopologies(t *testing.T) {
	rows := []struct {
		name string
		want wiringObservation
	}{
		{"downstream_step", wiringObservation{
			RootOutcome: "fail", ControlOutcome: "fail", AfterOutcome: "closed/pass",
			FinalizerBlockers: "after,loop,loop.iteration.1.m4-scope-check", SubjectRef: "loop", SourceStatus: "open",
		}},
		{"in_scope", wiringObservation{
			RootOutcome: "fail", ControlOutcome: "fail", BodyOutcome: "closed/fail",
			FinalizerBlockers: "body,loop,loop-scope-check,loop.iteration.1.m4-scope-check", SubjectRef: "loop", SourceStatus: "open",
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			res := runWiringWorkflow(t, wiringScenario{
				formula:       wiringTopologyFormula(row.name, 2),
				checkPassIter: map[string]int{"loop": 0},
			})
			assertObservation(t, observeWiring(t, res), row.want)
		})
	}
}

// TestRalphFinalizeRecoveredWithDownstreamStep: a recovered loop followed by a
// plain step. Every finalizer blocker passes, so the root must finalize pass.
func TestRalphFinalizeRecoveredWithDownstreamStep(t *testing.T) {
	res := runWiringWorkflow(t, wiringScenario{
		formula:       wiringTopologyFormula("downstream_step", 3),
		checkPassIter: map[string]int{"loop": 2},
		failMembers:   map[string]string{"loop.m1@1": ""},
	})
	control := res.get(t, "loop")
	ralphAssertOutcome(t, "ralph control", control, "pass")
	res.assertControlPassedBeforeFinalizer(t, control)
	ralphAssertOutcome(t, "after", res.get(t, "after"), "pass")
	ralphAssertOutcome(t, "workflow root", res.root(t), "pass")
	res.assertSourceFailureKeysEmpty(t)
	ralphAssertOutcome(t, "iteration 1 m1", res.get(t, "loop.iteration.1.m1"), "fail")
}

// TestRalphFinalizeRecoveredInsidePlainScope: a recovered loop that is a member
// of an enclosing plain scope. The iteration abort stays inside the iteration
// scope, so the enclosing scope is not aborted and the root passes.
func TestRalphFinalizeRecoveredInsidePlainScope(t *testing.T) {
	res := runWiringWorkflow(t, wiringScenario{
		formula:       wiringTopologyFormula("in_scope", 3),
		checkPassIter: map[string]int{"loop": 2},
		failMembers:   map[string]string{"loop.m1@1": ""},
	})
	control := res.get(t, "loop")
	ralphAssertOutcome(t, "ralph control", control, "pass")
	res.assertControlPassedBeforeFinalizer(t, control)
	body := res.get(t, "body")
	if got := body.Metadata["gc.outcome"]; got == "fail" {
		t.Errorf("enclosing scope body gc.outcome = %q, want not aborted by the iteration-1 failure", got)
	}
	ralphAssertOutcome(t, "workflow root", res.root(t), "pass")
	res.assertSourceFailureKeysEmpty(t)
	t.Logf("ralph control gc.on_fail = %q", control.Metadata["gc.on_fail"])
}

// TestRalphFinalizeRecoveredAfterTwoFailedIterations: m1 fails in iterations 1 and
// 2 and everything passes in iteration 3, so the loop recovers on its last
// attempt. Both superseded members stay recorded as failed and the root passes.
func TestRalphFinalizeRecoveredAfterTwoFailedIterations(t *testing.T) {
	res := runWiringWorkflow(t, wiringScenario{
		formula:       wiringFormulaTOML(wiringRalphLoopTOML(3, "")),
		checkPassIter: map[string]int{"loop": 3},
		failMembers:   map[string]string{"loop.m1@1": "", "loop.m1@2": ""},
	})
	control := res.get(t, "loop")
	ralphAssertOutcome(t, "ralph control", control, "pass")
	res.assertControlPassedBeforeFinalizer(t, control)
	ralphAssertOutcome(t, "workflow root", res.root(t), "pass")
	res.assertSourceFailureKeysEmpty(t)
	ralphAssertOutcome(t, "iteration 1 m1", res.get(t, "loop.iteration.1.m1"), "fail")
	ralphAssertOutcome(t, "iteration 2 m1", res.get(t, "loop.iteration.2.m1"), "fail")
	ralphAssertOutcome(t, "iteration 3 m1", res.get(t, "loop.iteration.3.m1"), "pass")
}

// TestRalphFinalizeOuterExhaustsInnerPasses is skipped on purpose: a ralph with
// children nested inside a ralph body does not run today. The compile stamps the
// inner iteration scope and its members with the OUTER iteration's scope_ref,
// ralph_step_id and attempt, so the inner member's scope check evaluates the
// outer scope and answers "continue", the inner scope never closes, and the
// workflow makes no progress.
func TestRalphFinalizeOuterExhaustsInnerPasses(t *testing.T) {
	t.Skip("nested ralph-with-children deadlocks before the finalizer: the inner scope carries the outer iteration's identity")
}
