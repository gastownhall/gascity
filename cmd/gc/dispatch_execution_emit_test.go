package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/runtime"
)

// These tests pin the drain-pass execution-fact projection (ga-vnycm2.18, D2):
// the serve drain projects a workflow root once after its last queued control
// instead of after every control, and still projects synchronously when a
// control materialized new steps.

// emitDrainHarness runs the real serve drain and the real control dispatch
// over an in-memory scoped workflow, recording the order in which controls
// are processed and roots are projected.
type emitDrainHarness struct {
	t        *testing.T
	store    *beads.MemStore
	cityPath string
	rootID   string
	convoyID string
	log      []string
	// failFirstAttemptOf, when set, makes the worker close the first attempt
	// of the step whose ref contains it without an outcome, so its retry
	// control classifies the attempt transient and mints a new attempt.
	failFirstAttemptOf string
	failedOnce         bool
}

var createdCountPattern = regexp.MustCompile(`created=(\d+)`)

func newEmitDrainHarness(t *testing.T) *emitDrainHarness {
	t.Helper()
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)
	store, convoyID, rootID := startMemScopedWorkflow(t)
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	h := &emitDrainHarness{t: t, store: store, cityPath: cityPath, rootID: rootID, convoyID: convoyID}

	prevOpen := openControlStoreForDispatch
	prevList := workflowServeList
	prevServe := controlDispatcherServe
	prevEmit := executionEmitCurrent
	prevProvider := dispatchControlSessionProvider
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	t.Cleanup(func() {
		openControlStoreForDispatch = prevOpen
		workflowServeList = prevList
		controlDispatcherServe = prevServe
		executionEmitCurrent = prevEmit
		dispatchControlSessionProvider = prevProvider
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	fakeProvider := runtime.NewFake()
	dispatchControlSessionProvider = func() (runtime.Provider, error) { return fakeProvider, nil }
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) { return store, nil }
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		var queue []hookBead
		for _, b := range memGraphReady(t, store) {
			if graphroute.IsControlDispatcherKind(b.Metadata[beadmeta.KindMetadataKey]) {
				queue = append(queue, hookBead{ID: b.ID, Metadata: hookBeadMetadata(b.Metadata)})
			}
		}
		return queue, nil
	}
	controlDispatcherServe = func(cityPath, storePath, beadID string, _, stderr io.Writer, emits *executionEmitDeferral) error {
		// The process line goes where the dispatch started, ahead of any
		// projection the dispatch itself ran.
		at := len(h.log)
		h.log = append(h.log, "")
		var stdout bytes.Buffer
		err := runControlDispatcherInStore(cityPath, storePath, beadID, &stdout, stderr, emits)
		created := "0"
		if m := createdCountPattern.FindStringSubmatch(stdout.String()); m != nil {
			created = m[1]
		}
		h.log[at] = fmt.Sprintf("process %s created=%s", beadID, created)
		return err
	}
	executionEmitCurrent = func(recorder events.Recorder, graphStore beads.GraphStore, workStore beads.WorkStore, rootID, actor string) error {
		h.log = append(h.log, "emit "+rootID)
		return executionevent.EmitCurrent(recorder, graphStore, workStore, rootID, actor)
	}
	return h
}

// drain runs one serve drain pass and returns the log lines it produced.
func (h *emitDrainHarness) drain() []string {
	h.t.Helper()
	start := len(h.log)
	if _, err := drainWorkflowServeWork(testControlDispatcherAgent(""), h.cityPath, h.cityPath, "", nil, io.Discard); err != nil {
		h.t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	return slices.Clone(h.log[start:])
}

// work runs every worker step that is ready, like the fixture worker does
// between dispatcher passes. It reports whether any step ran.
func (h *emitDrainHarness) work() bool {
	h.t.Helper()
	ran := false
	for {
		ready := memGraphReady(h.t, h.store)
		if i, ok := firstClaimableGraphWorkerBead(ready, "worker"); ok {
			worker := "worker"
			if err := h.store.Update(ready[i].ID, beads.UpdateOpts{Assignee: &worker}); err != nil {
				h.t.Fatalf("claim %s: %v", ready[i].ID, err)
			}
			ready[i] = mustGetMemBead(h.t, h.store, ready[i].ID)
		}
		bead, ok, err := selectExecutableGraphWorkerBead(ready, "worker")
		if err != nil {
			h.t.Fatal(err)
		}
		if !ok {
			return ran
		}
		ran = true
		if h.failFirstAttemptOf != "" && !h.failedOnce && strings.Contains(beadRef(bead), h.failFirstAttemptOf) {
			h.failedOnce = true
			if err := h.store.Close(bead.ID); err != nil {
				h.t.Fatalf("close %s without outcome: %v", bead.ID, err)
			}
			continue
		}
		executeMemGraphWorkerBead(h.t, h.store, bead, h.convoyID, h.cityPath, "success")
	}
}

// runToCompletion alternates worker steps and drain passes until the
// workflow root closes, returning the log of every drain pass.
func (h *emitDrainHarness) runToCompletion() [][]string {
	h.t.Helper()
	var passes [][]string
	for i := 0; i < 100; i++ {
		if mustGetMemBead(h.t, h.store, h.rootID).Status == "closed" {
			return passes
		}
		pass := h.drain()
		ran := h.work()
		if len(pass) == 0 && !ran {
			h.t.Fatalf("workflow %s made no progress; log=%v", h.rootID, h.log)
		}
		if len(pass) > 0 {
			passes = append(passes, pass)
		}
	}
	h.t.Fatalf("workflow %s did not finish; log=%v", h.rootID, h.log)
	return nil
}

func (h *emitDrainHarness) recordedStepDefined() map[string]bool {
	h.t.Helper()
	all, err := events.ReadAll(filepath.Join(h.cityPath, citylayout.RuntimeRoot, "events.jsonl"))
	if err != nil {
		h.t.Fatalf("read events: %v", err)
	}
	got := map[string]bool{}
	for _, e := range all {
		if e.Type == events.ExecutionStepDefined && e.RunID == h.rootID {
			got[e.Subject] = true
		}
	}
	return got
}

// assertEveryStepDefined is the end-state check: every physical step of the
// root carries a recorded step_defined fact and the durable marker, which is
// what the per-control projection produced.
func (h *emitDrainHarness) assertEveryStepDefined() {
	h.t.Helper()
	projection, err := executionevent.ProjectCurrent(beads.GraphStore{Store: h.store}, beads.WorkStore{Store: h.store}, h.rootID)
	if err != nil {
		h.t.Fatalf("ProjectCurrent: %v", err)
	}
	if len(projection.Steps) == 0 {
		h.t.Fatal("premise: the workflow projects no steps")
	}
	recorded := h.recordedStepDefined()
	for _, step := range projection.Steps {
		if !step.DefinedEmitted {
			h.t.Errorf("step %s (%s) is not marked step_defined-emitted", step.BeadID, step.StepID)
		}
		if !recorded[step.BeadID] {
			h.t.Errorf("no execution.step_defined recorded for step %s (%s)", step.BeadID, step.StepID)
		}
	}
}

// processedCreated parses a harness "process" line, reporting whether the
// control created steps.
func processedCreated(line string) (created bool, ok bool) {
	var id string
	var n int
	if _, err := fmt.Sscanf(line, "process %s created=%d", &id, &n); err != nil {
		return false, false
	}
	return n > 0, true
}

// TestServeDrainProjectsEachRootOnceAfterItsLastControl: a drain pass that
// processes several controls of one root, none of which create steps,
// projects that root exactly once, after the last of them.
func TestServeDrainProjectsEachRootOnceAfterItsLastControl(t *testing.T) {
	h := newEmitDrainHarness(t)
	passes := h.runToCompletion()

	multiControlPasses := 0
	for _, pass := range passes {
		processed, emits := 0, 0
		for i, line := range pass {
			if created, ok := processedCreated(line); ok {
				processed++
				if created {
					t.Fatalf("premise: success path control created steps in pass %v", pass)
				}
				continue
			}
			emits++
			if line != "emit "+h.rootID {
				t.Fatalf("unexpected projection %q in pass %v", line, pass)
			}
			if i != len(pass)-1 {
				t.Fatalf("pass %v projected the root before its last control was processed", pass)
			}
		}
		if processed > 0 && emits != 1 {
			t.Fatalf("pass %v projected the root %d times, want exactly once after its controls", pass, emits)
		}
		if processed > 1 {
			multiControlPasses++
		}
	}
	if multiControlPasses == 0 {
		t.Fatalf("premise: no drain pass processed more than one control of the root; passes=%v", passes)
	}
	h.assertEveryStepDefined()
}

// TestServeDrainProjectsSynchronouslyWhenAControlCreatesSteps: a retry that
// mints a new attempt is projected before the drain moves on, so the new
// attempt's step_defined fact cannot trail the worker's claim of it.
func TestServeDrainProjectsSynchronouslyWhenAControlCreatesSteps(t *testing.T) {
	h := newEmitDrainHarness(t)
	h.failFirstAttemptOf = ".load-context"
	passes := h.runToCompletion()
	if !h.failedOnce {
		t.Fatal("premise: the first load-context attempt was never failed")
	}

	sawCreated := false
	for _, pass := range passes {
		for i, line := range pass {
			created, ok := processedCreated(line)
			if !ok || !created {
				continue
			}
			sawCreated = true
			if i+1 >= len(pass) || pass[i+1] != "emit "+h.rootID {
				t.Fatalf("pass %v: control %q created steps but the root was not projected before the drain moved on", pass, line)
			}
		}
	}
	if !sawCreated {
		t.Fatalf("premise: no control created steps; passes=%v", passes)
	}
	h.assertEveryStepDefined()
}

// TestServeDrainCrashBeforeDeferredProjectionSelfHeals: a dispatcher that
// dies after processing a control but before the drain projects its root
// leaves the root's steps unmarked, and the next projection of that root
// records every missing step_defined fact.
func TestServeDrainCrashBeforeDeferredProjectionSelfHeals(t *testing.T) {
	h := newEmitDrainHarness(t)

	// Run worker steps until a control is ready, then process exactly that
	// control with a deferral that never flushes: the process died.
	var control hookBead
	for i := 0; i < 20 && control.ID == ""; i++ {
		queue, err := workflowServeList("", h.cityPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(queue) > 0 {
			control = queue[0]
			break
		}
		if !h.work() {
			t.Fatal("no control became ready")
		}
	}
	if control.ID == "" {
		t.Fatal("no control became ready")
	}
	crashed := newExecutionEmitDeferral(h.cityPath, h.cityPath, io.Discard)
	if err := controlDispatcherServe(h.cityPath, h.cityPath, control.ID, io.Discard, io.Discard, crashed); err != nil {
		t.Fatalf("process %s: %v", control.ID, err)
	}
	if got := mustGetMemBead(t, h.store, control.ID).Status; got != "closed" {
		t.Fatalf("premise: control %s status = %q, want closed", control.ID, got)
	}
	if len(crashed.pending) != 1 || crashed.pending[0].rootID != h.rootID {
		t.Fatalf("deferred roots = %+v, want [%s]", crashed.pending, h.rootID)
	}
	if got := len(h.recordedStepDefined()); got != 0 {
		t.Fatalf("premise: %d step_defined facts recorded before any projection ran, want 0", got)
	}

	h.runToCompletion()
	h.assertEveryStepDefined()
}

// TestOneShotControlDispatchStillProjectsInline: `gc convoy control <id>`
// has no drain pass to defer to, so it projects before it returns.
func TestOneShotControlDispatchStillProjectsInline(t *testing.T) {
	h := newEmitDrainHarness(t)
	for i := 0; i < 20; i++ {
		queue, err := workflowServeList("", h.cityPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(queue) > 0 {
			if err := runControlDispatcherInStore(h.cityPath, h.cityPath, queue[0].ID, io.Discard, io.Discard, nil); err != nil {
				t.Fatalf("dispatch %s: %v", queue[0].ID, err)
			}
			if want := []string{"emit " + h.rootID}; !slices.Equal(h.log, want) {
				t.Fatalf("log = %v, want %v", h.log, want)
			}
			return
		}
		if !h.work() {
			break
		}
	}
	t.Fatal("no control became ready")
}

// TestExecutionEmitDeferralFlushOpensAndClosesOneStore pins the handle
// discipline of the deferred projection (#6255): a flush opens one scope store
// for every pending root and closes it before returning, and a flush with
// nothing pending opens nothing.
func TestExecutionEmitDeferralFlushOpensAndClosesOneStore(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	var opened []*closeCountingStore
	prevOpen := openControlStoreForDispatch
	prevEmit := executionEmitCurrent
	t.Cleanup(func() {
		openControlStoreForDispatch = prevOpen
		executionEmitCurrent = prevEmit
	})
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		s := newCloseCountingStore(t, false)
		opened = append(opened, s)
		return s, nil
	}
	var projected []string
	executionEmitCurrent = func(_ events.Recorder, graphStore beads.GraphStore, _ beads.WorkStore, rootID, _ string) error {
		if _, err := graphStore.List(beads.ListQuery{AllowScan: true}); err != nil {
			t.Errorf("projection of %s read a closed store: %v", rootID, err)
		}
		projected = append(projected, rootID)
		return nil
	}

	d := newExecutionEmitDeferral(cityDir, cityDir, io.Discard)
	d.flush()
	if len(opened) != 0 {
		t.Fatalf("empty flush opened %d stores, want 0", len(opened))
	}
	d.add("gc-root-a", false)
	d.add("gc-root-b", false)
	d.add("gc-root-a", false)
	d.flushSettled([]hookBead{{ID: "gc-ctl", Metadata: hookBeadMetadata{beadmeta.RootBeadIDMetadataKey: "gc-root-b"}}})
	if want := []string{"gc-root-a"}; !slices.Equal(projected, want) {
		t.Fatalf("flushSettled projected %v, want %v (gc-root-b still has a queued control)", projected, want)
	}
	d.flush()
	if want := []string{"gc-root-a", "gc-root-b"}; !slices.Equal(projected, want) {
		t.Fatalf("projected %v, want %v", projected, want)
	}
	if len(opened) != 2 {
		t.Fatalf("two non-empty flushes opened %d stores, want 2", len(opened))
	}
	for i, s := range opened {
		if got := s.closes(); got != 1 {
			t.Fatalf("store %d closed %d times, want 1", i, got)
		}
	}
	d.forget("gc-root-a")
	d.flush()
	if len(opened) != 2 {
		t.Fatalf("flush after everything projected opened a store")
	}
}
