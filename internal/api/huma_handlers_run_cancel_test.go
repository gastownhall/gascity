package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// closeFailStore delegates every read to an embedded store but fails CloseAll,
// standing in for a transient bd/Dolt write failure during cancel.
type closeFailStore struct {
	beads.Store
}

func (closeFailStore) CloseAll([]string, map[string]string) (int, error) {
	return 0, errors.New("simulated store write failure")
}

// txCloseFailStore models a store whose Tx commits atomically (AtomicTx=true):
// writes buffered inside the callback persist only when the callback returns nil.
// Its transactional Close fails for failCloseID, standing in for a run root whose
// close fails AFTER its cancel-marker metadata was written in the same Tx. Because
// the Tx is atomic, that failure must roll the marker back, so the root never
// lingers open carrying gc.cancel_requested / gc.outcome=canceled.
type txCloseFailStore struct {
	beads.Store
	failCloseID string
}

func (txCloseFailStore) AtomicTx() bool { return true }

func (s txCloseFailStore) Tx(_ string, fn func(beads.Tx) error) error {
	buf := &bufferingTx{base: s.Store, failCloseID: s.failCloseID}
	if err := fn(buf); err != nil {
		return err // atomic rollback: buffered writes are discarded
	}
	return buf.flush()
}

// bufferingTx records writes and applies them to the base store only on flush, so
// a callback error leaves the base store untouched (atomic-rollback semantics).
type bufferingTx struct {
	base        beads.Store
	failCloseID string
	metaWrites  []bufferedMeta
	closes      []string
}

type bufferedMeta struct {
	id  string
	kvs map[string]string
}

func (b *bufferingTx) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, errors.New("bufferingTx.Create unused in this test")
}

func (b *bufferingTx) Update(string, beads.UpdateOpts) error {
	return errors.New("bufferingTx.Update unused in this test")
}

func (b *bufferingTx) SetMetadataBatch(id string, kvs map[string]string) error {
	b.metaWrites = append(b.metaWrites, bufferedMeta{id: id, kvs: kvs})
	return nil
}

func (b *bufferingTx) Close(id string) error {
	if id == b.failCloseID {
		return errors.New("simulated root close failure")
	}
	b.closes = append(b.closes, id)
	return nil
}

func (b *bufferingTx) flush() error {
	for _, m := range b.metaWrites {
		if err := b.base.SetMetadataBatch(m.id, m.kvs); err != nil {
			return err
		}
	}
	for _, id := range b.closes {
		if _, err := b.base.CloseAll([]string{id}, nil); err != nil {
			return err
		}
	}
	return nil
}

// newWorkflowRun seeds a graph-workflow run (root + one open child step) in the
// rig store and returns the server plus the store-assigned run root id.
func newWorkflowRun(t *testing.T) (*Server, beads.Store, string) {
	t.Helper()
	fs := newFakeState(t)
	store := fs.stores["myrig"]
	root, err := store.Create(beads.Bead{
		Title: "run root",
		Type:  "molecule",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "step 1",
		Type:     "task",
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	}); err != nil {
		t.Fatal(err)
	}
	return &Server{state: fs}, store, root.ID
}

func TestRunCancelClosesRun(t *testing.T) {
	s, store, runID := newWorkflowRun(t)

	out, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     runID,
	})
	if err != nil {
		t.Fatalf("humaHandleRunCancel error: %v", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("status = %q, want canceled", out.Body.Status)
	}
	if out.Body.Closed != 2 {
		t.Errorf("closed = %d, want 2 (root + one step)", out.Body.Closed)
	}

	// The root is closed with a canceled outcome and carries the intent marker.
	root, err := store.Get(runID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "closed" {
		t.Errorf("root status = %q, want closed", root.Status)
	}
	if root.Metadata["gc.outcome"] != "canceled" {
		t.Errorf("root gc.outcome = %q, want canceled", root.Metadata["gc.outcome"])
	}
	if root.Metadata["gc.cancel_requested"] != "true" {
		t.Errorf("root gc.cancel_requested = %q, want true", root.Metadata["gc.cancel_requested"])
	}

	// The cancel-intent marker is root-only: members close as canceled but must
	// not be smeared with gc.cancel_requested.
	members, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{"gc.root_bead_id": runID},
		IncludeClosed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(members) == 0 {
		t.Fatal("expected at least one member step under the run root")
	}
	for _, m := range members {
		if m.Metadata["gc.outcome"] != "canceled" {
			t.Errorf("member %s gc.outcome = %q, want canceled", m.ID, m.Metadata["gc.outcome"])
		}
		if got := m.Metadata["gc.cancel_requested"]; got != "" {
			t.Errorf("member %s gc.cancel_requested = %q, want empty (root-only marker)", m.ID, got)
		}
	}
}

// TestRunCancelLeavesCompletedStepsUntouched guards the data-loss finding: a
// member that already completed keeps its recorded outcome — cancel closes only
// the still-open work.
func TestRunCancelLeavesCompletedStepsUntouched(t *testing.T) {
	fs := newFakeState(t)
	store := fs.stores["myrig"]
	root, err := store.Create(beads.Bead{
		Title:    "run root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := store.Create(beads.Bead{Title: "done step", Type: "task", Metadata: map[string]string{"gc.root_bead_id": root.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CloseAll([]string{done.ID}, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{Title: "open step", Type: "task", Metadata: map[string]string{"gc.root_bead_id": root.ID}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{state: fs}

	out, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     root.ID,
	})
	if err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	// Only the root + the one open step are closed by the cancel; the done step is not.
	if out.Body.Closed != 2 {
		t.Errorf("closed = %d, want 2 (root + open step; the completed step is untouched)", out.Body.Closed)
	}
	doneAfter, err := store.Get(done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doneAfter.Metadata["gc.outcome"] != "pass" {
		t.Errorf("completed step outcome = %q, want it left as pass (not rewritten to canceled)", doneAfter.Metadata["gc.outcome"])
	}
}

// TestRunCancelStoreFailureReports503 guards the false-success finding: a store
// write failure must surface as a 5xx, never a phantom 202 canceled.
func TestRunCancelStoreFailureReports503(t *testing.T) {
	fs := newFakeState(t)
	mem := fs.stores["myrig"]
	root, err := mem.Create(beads.Bead{
		Title:    "run root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fs.stores["myrig"] = closeFailStore{mem} // reads still work; CloseAll fails
	s := &Server{state: fs}

	_, err = s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     root.ID,
	})
	if err == nil {
		t.Fatal("cancel with a failing store returned nil error, want a 5xx (no phantom success)")
	}
	if !strings.Contains(err.Error(), "run cancel failed") {
		t.Errorf("error = %q, want a cancel-failed 5xx", err.Error())
	}
	// The run must remain open — nothing was canceled.
	after, err := mem.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status == "closed" {
		t.Error("root was closed despite the reported failure")
	}
}

// TestRunCancelAtomicRootCloseFailureLeavesRunCancelingForRetry pins what a
// root close that fails after its metadata write leaves behind on an atomic
// store. The cancel marks the run root canceling (gc.cancel_requested) as its
// first write, before it interrupts anyone or closes anything, so that claims
// and controls stop while the rest of the wind-down runs. The failed close's
// transaction rolls back its own writes, so the root stays open without a
// canceled outcome, still marked canceling, and the caller gets a retryable
// 5xx. Retrying the cancel completes the close.
func TestRunCancelAtomicRootCloseFailureLeavesRunCancelingForRetry(t *testing.T) {
	fs := newFakeState(t)
	mem := fs.stores["myrig"]
	root, err := mem.Create(beads.Bead{
		Title:    "run root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Create(beads.Bead{Title: "open step", Type: "task", Metadata: map[string]string{"gc.root_bead_id": root.ID}}); err != nil {
		t.Fatal(err)
	}
	// Reads and the descendant close batch use the real store; the root's
	// transactional close fails after its metadata write, and AtomicTx=true
	// forces the whole Tx to roll back.
	fs.stores["myrig"] = txCloseFailStore{Store: mem, failCloseID: root.ID}
	s := &Server{state: fs}
	input := &RunCancelInput{CityScope: CityScope{CityName: "test-city"}, RunID: root.ID}

	_, err = s.humaHandleRunCancel(context.Background(), input)
	if err == nil {
		t.Fatal("cancel with a failing atomic root close returned nil error, want a retryable 5xx")
	}
	if !strings.Contains(err.Error(), "run cancel failed") {
		t.Errorf("error = %q, want a cancel-failed 5xx", err.Error())
	}

	after, err := mem.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status == "closed" {
		t.Error("root was closed despite the reported close failure")
	}
	if got := after.Metadata["gc.outcome"]; got == "canceled" {
		t.Error("root gc.outcome was written as canceled despite its close rolling back")
	}
	if got := after.Metadata["gc.cancel_requested"]; got != "true" {
		t.Errorf("root gc.cancel_requested = %q, want true: the cancel intent is recorded before the close, so the run stays stopped while the client retries", got)
	}

	fs.stores["myrig"] = mem
	out, err := s.humaHandleRunCancel(context.Background(), input)
	if err != nil {
		t.Fatalf("retried cancel error: %v, want it to complete the wind-down", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("retried cancel status = %q, want canceled", out.Body.Status)
	}
	after, err = mem.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "closed" || after.Metadata["gc.outcome"] != "canceled" {
		t.Errorf("root after retry = status %q outcome %q, want closed canceled", after.Status, after.Metadata["gc.outcome"])
	}
}

// TestRunCancelGraphV2OnlyRoot guards the false-404 finding: a run root marked
// only by gc.formula_contract=graph.v2 (no gc.kind=workflow) is still cancellable.
func TestRunCancelGraphV2OnlyRoot(t *testing.T) {
	fs := newFakeState(t)
	store := fs.stores["myrig"]
	root, err := store.Create(beads.Bead{
		Title:    "graph-only root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.formula_contract": "graph.v2"}, // NO gc.kind=workflow
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{state: fs}

	out, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     root.ID,
	})
	if err != nil {
		t.Fatalf("cancel of a graph.v2-only root errored: %v (want it cancellable, not a 404)", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("status = %q, want canceled", out.Body.Status)
	}
}

func TestRunCancelNotFound(t *testing.T) {
	s, _, _ := newWorkflowRun(t)
	_, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     "ghost",
	})
	if err == nil {
		t.Fatal("cancel(ghost) = nil error, want run-not-found")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Errorf("error = %q, want run-not-found detail", err.Error())
	}
}

func TestRunCancelRejectsNonWorkflowBead(t *testing.T) {
	fs := newFakeState(t)
	store := fs.stores["myrig"]
	plain, err := store.Create(beads.Bead{Title: "not a run", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{state: fs}

	_, err = s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     plain.ID,
	})
	if err == nil || !strings.Contains(err.Error(), "run not found") {
		t.Fatalf("cancel(plain bead) err = %v, want run-not-found", err)
	}
}

// TestRunCancelWireRoute drives the cancel through the real HTTP router: POST
// routing + CSRF, the 202 default status, and the run-not-found 404.
func TestRunCancelWireRoute(t *testing.T) {
	fs := newFakeState(t)
	store := fs.stores["myrig"]
	root, err := store.Create(beads.Bead{
		Title:    "run root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestCityHandler(t, fs)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/runs/"+root.ID+"/cancel"), nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rec.Body.String())
	}
	if body.RunID != root.ID || body.Status != string(RunStatusCanceled) {
		t.Errorf("body = %+v, want run_id=%s status=canceled", body, root.ID)
	}

	rec404 := httptest.NewRecorder()
	h.ServeHTTP(rec404, newPostRequest(cityURL(fs, "/runs/ghost/cancel"), nil))
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("cancel(ghost) status = %d, want 404; body = %s", rec404.Code, rec404.Body.String())
	}
}

// TestRunCancelLeavesTeardownStepOpenToExecute guards the finding that run
// cancellation force-closed a workflow's always-run cleanup step as canceled
// instead of letting it run: cancelRun's blanket subtree close swept the
// teardown tail along with ordinary open work, so a step whose job is to
// release the run's external resources (e.g. tearing down a sandbox) never
// executed. The teardown step must stay open after cancel, and still be
// closeable with a real outcome afterward, so the dispatcher can drive it to
// completion — its pass condition may read ROOT_OUTCOME, which only exists
// once the root itself is closed, which cancelRun already does.
func TestRunCancelLeavesTeardownStepOpenToExecute(t *testing.T) {
	fs := newFakeState(t)
	store := fs.stores["myrig"]
	root, err := store.Create(beads.Bead{
		Title:    "run root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	openStep, err := store.Create(beads.Bead{
		Title:    "ordinary step",
		Type:     "task",
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	teardown, err := store.Create(beads.Bead{
		Title: "cleanup",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.scope_role":   "teardown",
			"gc.step_id":      "cleanup",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{state: fs}

	out, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     root.ID,
	})
	if err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("status = %q, want canceled", out.Body.Status)
	}
	// Root + the one ordinary step close; the teardown step is excluded from
	// the sweep and stays open.
	if out.Body.Closed != 2 {
		t.Errorf("closed = %d, want 2 (root + ordinary step; the teardown step stays open)", out.Body.Closed)
	}

	afterCancel, err := store.Get(teardown.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterCancel.Status == "closed" {
		t.Fatal("teardown step was force-closed by run cancel; it must stay open so it can still execute")
	}
	if got := afterCancel.Metadata["gc.outcome"]; got == "canceled" {
		t.Fatal("teardown step was stamped canceled by run cancel; it never ran")
	}

	// The ordinary step DID close as canceled — cancel still winds down the
	// rest of the run, it only exempts the teardown tail.
	openAfter, err := store.Get(openStep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if openAfter.Status != "closed" || openAfter.Metadata["gc.outcome"] != "canceled" {
		t.Errorf("ordinary step = %+v, want closed with gc.outcome=canceled", openAfter)
	}

	// The teardown step is still executable: it can now run to completion and
	// record a real outcome, proving cancel did not strand it half-wound-down.
	if _, err := store.CloseAll([]string{teardown.ID}, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("closing teardown step after cancel: %v", err)
	}
	executed, err := store.Get(teardown.ID)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Status != "closed" || executed.Metadata["gc.outcome"] != "pass" {
		t.Errorf("executed teardown step = %+v, want closed with gc.outcome=pass", executed)
	}
}

func TestRunCancelAlreadyTerminalConflict(t *testing.T) {
	s, store, runID := newWorkflowRun(t)
	// Close the root before canceling — the run is already terminal.
	if _, err := store.CloseAll([]string{runID}, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatal(err)
	}

	_, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     runID,
	})
	if err == nil {
		t.Fatal("cancel(terminal run) = nil error, want 409 conflict")
	}
	if !strings.Contains(err.Error(), "already terminal") {
		t.Errorf("error = %q, want already-terminal conflict", err.Error())
	}
}

// TestRunCancelReachesNestedDrainItemRoots guards the nested-drain cancel gap: a
// drain item workflow is its own root, linked to the run only through its
// gc.drain_control_id, and its steps carry the item root's id rather than the
// run's. A cancel that winds down only gc.root_bead_id == run left those item
// roots and their steps open and claimable after the run was reported canceled.
// The cancel must reach every level of nesting, keep the teardown-tail
// exemption at each level, and leave completed work untouched.
func TestRunCancelReachesNestedDrainItemRoots(t *testing.T) {
	fs := newFakeState(t)
	store := fs.stores["myrig"]
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	workflowRoot := func(title, drainControlID string) beads.Bead {
		meta := map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}
		if drainControlID != "" {
			meta["gc.drain_control_id"] = drainControlID
		}
		return create(beads.Bead{Title: title, Type: "molecule", Metadata: meta})
	}
	member := func(title, rootID string, extra map[string]string) beads.Bead {
		meta := map[string]string{"gc.root_bead_id": rootID}
		for k, v := range extra {
			meta[k] = v
		}
		return create(beads.Bead{Title: title, Type: "task", Metadata: meta})
	}

	run := workflowRoot("implement run", "")
	drain := member("drain work", run.ID, map[string]string{"gc.kind": "drain"})
	item := workflowRoot("do-work item", drain.ID)
	itemStep := member("item step", item.ID, nil)
	itemDone := member("item done step", item.ID, nil)
	if _, err := store.CloseAll([]string{itemDone.ID}, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatal(err)
	}
	itemTeardown := member("item cleanup", item.ID, map[string]string{"gc.scope_role": "teardown", "gc.step_id": "cleanup"})
	nestedDrain := member("nested drain", item.ID, map[string]string{"gc.kind": "drain"})
	grandchild := workflowRoot("nested item", nestedDrain.ID)
	grandchildStep := member("nested item step", grandchild.ID, nil)
	s := &Server{state: fs}

	out, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     run.ID,
	})
	if err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("status = %q, want canceled", out.Body.Status)
	}

	for _, id := range []string{item.ID, grandchild.ID} {
		root, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if root.Status != "closed" || root.Metadata["gc.outcome"] != "canceled" {
			t.Errorf("nested item root %s = status %q outcome %q, want closed canceled", id, root.Status, root.Metadata["gc.outcome"])
		}
		if root.Metadata["gc.cancel_requested"] != "true" {
			t.Errorf("nested item root %s gc.cancel_requested = %q, want true so its controls stop as canceled", id, root.Metadata["gc.cancel_requested"])
		}
	}
	for _, id := range []string{itemStep.ID, nestedDrain.ID, grandchildStep.ID} {
		step, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if step.Status != "closed" || step.Metadata["gc.outcome"] != "canceled" {
			t.Errorf("nested step %s = status %q outcome %q, want closed canceled", id, step.Status, step.Metadata["gc.outcome"])
		}
	}
	done, err := store.Get(itemDone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Metadata["gc.outcome"] != "pass" {
		t.Errorf("completed nested step outcome = %q, want it left as pass", done.Metadata["gc.outcome"])
	}
	teardown, err := store.Get(itemTeardown.ID)
	if err != nil {
		t.Fatal(err)
	}
	if teardown.Status == "closed" {
		t.Error("nested teardown step was force-closed by run cancel; it must stay open so it can still execute")
	}
	// run + drain + item + item step + nested drain + grandchild + its step.
	if out.Body.Closed != 7 {
		t.Errorf("closed = %d, want 7 (both workflow levels below the run, minus completed and teardown work)", out.Body.Closed)
	}
}

// runCancelInterruptFixture is a run whose steps are being worked by live
// sessions, alongside sessions that hold no canceled work.
type runCancelInterruptFixture struct {
	fs             *fakeState
	runID          string
	stepWorker     session.Info // holds an in-progress step of the run
	itemWorker     session.Info // holds an in-progress step of a nested drain item
	teardownWorker session.Info // holds only the run's spared teardown step
	otherRunWorker session.Info // holds an in-progress step of a different run
	goneWorker     session.Info // holds a step of the run, but its runtime is gone
}

func newRunCancelInterruptFixture(t *testing.T) runCancelInterruptFixture {
	t.Helper()
	fs := newSessionFakeState(t)
	store := fs.stores["myrig"]
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	workflowRoot := func(title, drainControlID string) beads.Bead {
		meta := map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}
		if drainControlID != "" {
			meta["gc.drain_control_id"] = drainControlID
		}
		return create(beads.Bead{Title: title, Type: "molecule", Metadata: meta})
	}
	member := func(title, rootID string, extra map[string]string) beads.Bead {
		meta := map[string]string{"gc.root_bead_id": rootID}
		for k, v := range extra {
			meta[k] = v
		}
		return create(beads.Bead{Title: title, Type: "task", Metadata: meta})
	}
	claim := func(b beads.Bead, assignee string) {
		t.Helper()
		status := "in_progress"
		if err := store.Update(b.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatal(err)
		}
	}

	f := runCancelInterruptFixture{
		fs:             fs,
		stepWorker:     createTestSession(t, fs.cityBeadStore, fs.sp, "run-step-worker"),
		itemWorker:     createTestSession(t, fs.cityBeadStore, fs.sp, "item-step-worker"),
		teardownWorker: createTestSession(t, fs.cityBeadStore, fs.sp, "teardown-worker"),
		otherRunWorker: createTestSession(t, fs.cityBeadStore, fs.sp, "other-run-worker"),
		goneWorker:     createTestSession(t, fs.cityBeadStore, fs.sp, "gone-worker"),
	}

	run := workflowRoot("implement run", "")
	f.runID = run.ID
	// A pool worker's claim is recorded under its session bead ID.
	claim(member("implement step", run.ID, nil), f.stepWorker.ID)
	claim(member("cleanup", run.ID, map[string]string{"gc.scope_role": "teardown", "gc.step_id": "cleanup"}), f.teardownWorker.ID)
	claim(member("stale step", run.ID, nil), f.goneWorker.ID)
	member("queued step", run.ID, nil) // open and unclaimed: nobody is working it
	drain := member("drain work", run.ID, map[string]string{"gc.kind": "drain"})
	item := workflowRoot("do-work item", drain.ID)
	// A manual session's claim is recorded under its session name.
	claim(member("item step", item.ID, nil), f.itemWorker.SessionName)

	other := workflowRoot("other run", "")
	claim(member("other step", other.ID, nil), f.otherRunWorker.ID)

	if err := fs.sp.Stop(f.goneWorker.SessionName); err != nil {
		t.Fatal(err)
	}
	return f
}

func interruptCount(sp *runtime.Fake, sessionName string) int {
	n := 0
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" && call.Name == sessionName {
			n++
		}
	}
	return n
}

// TestRunCancelInterruptsSessionsWorkingTheRun guards the live gap where a
// canceled run's graph closed durably but the session executing one of its
// steps kept running its turn, foreground tool included. The cancel must
// interrupt the current turn of every live session holding an in-progress step
// the cancel closed, nested drain items included, exactly once each, and leave
// alone sessions working other runs, sessions holding only the spared teardown
// tail, and sessions whose runtime is already gone. Nothing is killed: a pool
// session may serve other work.
func TestRunCancelInterruptsSessionsWorkingTheRun(t *testing.T) {
	f := newRunCancelInterruptFixture(t)
	s := &Server{state: f.fs}

	out, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     f.runID,
	})
	if err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("status = %q, want canceled", out.Body.Status)
	}

	for _, owner := range []session.Info{f.stepWorker, f.itemWorker} {
		if got := interruptCount(f.fs.sp, owner.SessionName); got != 1 {
			t.Errorf("session %s (%s) interrupts = %d, want 1: it was executing a step the cancel closed", owner.ID, owner.Title, got)
		}
		if !f.fs.sp.IsRunning(owner.SessionName) {
			t.Errorf("session %s (%s) is no longer running; cancel must interrupt the turn, not kill the session", owner.ID, owner.Title)
		}
	}
	for _, bystander := range []session.Info{f.teardownWorker, f.otherRunWorker, f.goneWorker} {
		if got := interruptCount(f.fs.sp, bystander.SessionName); got != 0 {
			t.Errorf("session %s (%s) interrupts = %d, want 0", bystander.ID, bystander.Title, got)
		}
	}

	wantInterrupted := []string{f.stepWorker.ID, f.itemWorker.ID}
	slices.Sort(wantInterrupted)
	gotInterrupted := slices.Clone(out.Body.Interrupted)
	slices.Sort(gotInterrupted)
	if !slices.Equal(gotInterrupted, wantInterrupted) {
		t.Errorf("interrupted = %v, want %v", out.Body.Interrupted, wantInterrupted)
	}
	if len(out.Body.InterruptFailures) != 0 {
		t.Errorf("interrupt_failures = %+v, want none", out.Body.InterruptFailures)
	}
}

// interruptFailProvider is the fake runtime with Interrupt failing for one
// session, standing in for a runtime that cannot reach that session's pane.
type interruptFailProvider struct {
	*runtime.Fake
	failName string
}

func (p interruptFailProvider) Interrupt(name string) error {
	if name == p.failName {
		return errors.New("simulated interrupt failure")
	}
	return p.Fake.Interrupt(name)
}

// TestRunCancelSucceedsWhenInterruptFails guards the best-effort contract: the
// graph cancel is already durable when the interrupts run, so a session that
// cannot be interrupted is reported rather than turned into a failed cancel,
// and the other holders are still interrupted.
func TestRunCancelSucceedsWhenInterruptFails(t *testing.T) {
	f := newRunCancelInterruptFixture(t)
	f.fs.sessionProvider = interruptFailProvider{Fake: f.fs.sp, failName: f.stepWorker.SessionName}
	s := &Server{state: f.fs}

	out, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     f.runID,
	})
	if err != nil {
		t.Fatalf("cancel error: %v, want success: the graph cancel is durable even when an interrupt fails", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("status = %q, want canceled", out.Body.Status)
	}
	root, err := f.fs.stores["myrig"].Get(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "closed" || root.Metadata["gc.outcome"] != "canceled" {
		t.Errorf("run root = status %q outcome %q, want closed canceled", root.Status, root.Metadata["gc.outcome"])
	}

	if len(out.Body.InterruptFailures) != 1 {
		t.Fatalf("interrupt_failures = %+v, want exactly the session whose interrupt failed", out.Body.InterruptFailures)
	}
	failure := out.Body.InterruptFailures[0]
	if failure.Session != f.stepWorker.ID || !strings.Contains(failure.Error, "simulated interrupt failure") {
		t.Errorf("interrupt failure = %+v, want session %s with the runtime's error", failure, f.stepWorker.ID)
	}
	if !slices.Equal(out.Body.Interrupted, []string{f.itemWorker.ID}) {
		t.Errorf("interrupted = %v, want [%s]: one failure must not stop the other interrupts", out.Body.Interrupted, f.itemWorker.ID)
	}
	if got := interruptCount(f.fs.sp, f.itemWorker.SessionName); got != 1 {
		t.Errorf("item worker interrupts = %d, want 1", got)
	}
}

// cancelOrderLog records, in order, the operations of a run cancel that decide
// how soon the run's work stops: the durable canceling mark on the run root,
// each session interrupt, and the start and end of each close batch.
// interrupted closes once wantInterrupts interrupts have been issued.
type cancelOrderLog struct {
	mu             sync.Mutex
	ops            []string
	wantInterrupts int
	interrupts     int
	interrupted    chan struct{}
}

func newCancelOrderLog(wantInterrupts int) *cancelOrderLog {
	return &cancelOrderLog{wantInterrupts: wantInterrupts, interrupted: make(chan struct{})}
}

func (l *cancelOrderLog) interrupt(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, "interrupt "+name)
	l.interrupts++
	if l.interrupts == l.wantInterrupts {
		close(l.interrupted)
	}
}

func (l *cancelOrderLog) add(op string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, op)
}

func (l *cancelOrderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ops)
}

func (l *cancelOrderLog) index(prefix string) int {
	return slices.IndexFunc(l.snapshot(), func(op string) bool { return strings.HasPrefix(op, prefix) })
}

// slowCloseStore stands in for a bd-backed store, where every close costs a
// subprocess call. Each close batch holds until every expected session
// interrupt has been issued, so a cancel whose interrupts wait on its closes
// stalls on every batch until closeStall runs out, and the recorded order
// shows it.
type slowCloseStore struct {
	beads.Store
	runID string
	log   *cancelOrderLog
}

// closeStall bounds how long one close batch waits for an interrupt. It is
// spent only when the cancel orders the interrupt after the closes.
const closeStall = 2 * time.Second

func (s slowCloseStore) markOf(id string, kvs map[string]string) {
	if id == s.runID && kvs["gc.cancel_requested"] != "" {
		s.log.add("mark run")
	}
}

func (s slowCloseStore) SetMetadata(id, key, value string) error {
	s.markOf(id, map[string]string{key: value})
	return s.Store.SetMetadata(id, key, value)
}

func (s slowCloseStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.markOf(id, kvs)
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s slowCloseStore) CloseAll(ids []string, kvs map[string]string) (int, error) {
	for _, id := range ids {
		s.markOf(id, kvs)
	}
	s.log.add("close begin " + strings.Join(ids, ","))
	select {
	case <-s.log.interrupted:
	case <-time.After(closeStall):
	}
	n, err := s.Store.CloseAll(ids, kvs)
	s.log.add("close end " + strings.Join(ids, ","))
	return n, err
}

// interruptRecordingProvider is the fake runtime recording each interrupt in
// the cancel order log.
type interruptRecordingProvider struct {
	*runtime.Fake
	log *cancelOrderLog
}

func (p interruptRecordingProvider) Interrupt(name string) error {
	p.log.interrupt(name)
	return p.Fake.Interrupt(name)
}

// TestRunCancelMarksRunThenInterruptsBeforeClosesFinish guards the live gap in
// which a canceled run's steps kept executing for 10 to 21 seconds after the
// cancel request: the cancel closed the whole graph, one bd call at a time,
// and only then interrupted the sessions working it, so a short step could
// finish and land its effects inside that window. The cancel must first make
// the run durably canceling (the gc.cancel_requested mark on the run root that
// the claim gate and the dispatcher's control gate stop on), then interrupt the
// sessions holding its in-progress steps without waiting for the graph closes,
// which run alongside.
func TestRunCancelMarksRunThenInterruptsBeforeClosesFinish(t *testing.T) {
	f := newRunCancelInterruptFixture(t)
	// The run step's and the drain item step's holders.
	log := newCancelOrderLog(2)
	f.fs.stores["myrig"] = slowCloseStore{Store: f.fs.stores["myrig"], runID: f.runID, log: log}
	f.fs.sessionProvider = interruptRecordingProvider{Fake: f.fs.sp, log: log}
	s := &Server{state: f.fs}

	out, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     f.runID,
	})
	if err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	ops := log.snapshot()
	t.Logf("cancel order: %q", ops)
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("status = %q, want canceled", out.Body.Status)
	}

	if len(ops) == 0 || ops[0] != "mark run" {
		t.Fatalf("first operation = %q, want the run root marked canceling before any interrupt or close", ops)
	}
	firstCloseEnd := log.index("close end ")
	interrupts := 0
	for i, op := range ops {
		if !strings.HasPrefix(op, "interrupt ") {
			continue
		}
		interrupts++
		if firstCloseEnd >= 0 && i > firstCloseEnd {
			t.Errorf("%s at op %d came after a close batch finished at op %d; the interrupts must not wait for the graph closes (ops = %q)", op, i, firstCloseEnd, ops)
		}
	}
	if interrupts != 2 {
		t.Errorf("interrupts issued = %d, want 2; ops = %q", interrupts, ops)
	}
	for _, owner := range []session.Info{f.stepWorker, f.itemWorker} {
		if got := interruptCount(f.fs.sp, owner.SessionName); got != 1 {
			t.Errorf("session %s interrupts = %d, want 1", owner.Title, got)
		}
	}
	root, err := f.fs.stores["myrig"].Get(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "closed" || root.Metadata["gc.outcome"] != "canceled" {
		t.Errorf("run root = status %q outcome %q, want closed canceled", root.Status, root.Metadata["gc.outcome"])
	}
}

// failCloseOnceStore fails the first close batch that includes failID, standing
// in for a store write that fails partway through a cancel.
type failCloseOnceStore struct {
	beads.Store
	failID string
	failed *bool
}

func (s failCloseOnceStore) CloseAll(ids []string, kvs map[string]string) (int, error) {
	if !*s.failed && slices.Contains(ids, s.failID) {
		*s.failed = true
		return 0, errors.New("simulated store write failure")
	}
	return s.Store.CloseAll(ids, kvs)
}

// TestRunCancelPartialFailureStopsClaimsAndRetryCompletes covers a cancel
// that fails after its first write. The run root's canceling mark is that
// write, so even with every close still pending the run is stopped: the claim
// gate refuses the open steps, the run's own and those of a drain item
// workflow whose root is still open, because both chains reach the marked run
// root. The 5xx is retryable, and the retry completes the wind-down.
func TestRunCancelPartialFailureStopsClaimsAndRetryCompletes(t *testing.T) {
	fs := newFakeState(t)
	mem := fs.stores["myrig"]
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := mem.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	run := create(beads.Bead{Title: "implement run", Type: "molecule", Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}})
	runStep := create(beads.Bead{Title: "run step", Type: "task", Metadata: map[string]string{"gc.root_bead_id": run.ID}})
	drain := create(beads.Bead{Title: "drain work", Type: "task", Metadata: map[string]string{"gc.root_bead_id": run.ID, "gc.kind": "drain"}})
	item := create(beads.Bead{Title: "do-work item", Type: "molecule", Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.drain_control_id": drain.ID}})
	itemStep := create(beads.Bead{Title: "item step", Type: "task", Metadata: map[string]string{"gc.root_bead_id": item.ID}})

	failed := false
	// The first close of the cancel (the drain item's steps, deepest first)
	// fails, so the canceling mark is the only write that lands.
	fs.stores["myrig"] = failCloseOnceStore{Store: mem, failID: itemStep.ID, failed: &failed}
	s := &Server{state: fs}
	input := &RunCancelInput{CityScope: CityScope{CityName: "test-city"}, RunID: run.ID}

	if _, err := s.humaHandleRunCancel(context.Background(), input); err == nil || !strings.Contains(err.Error(), "run cancel failed") {
		t.Fatalf("cancel with a failing close err = %v, want a retryable cancel-failed 5xx", err)
	}
	if !failed {
		t.Fatal("the simulated close failure never fired")
	}
	runAfter, err := mem.Get(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runAfter.Status == "closed" || runAfter.Metadata["gc.cancel_requested"] != "true" {
		t.Fatalf("run root = status %q gc.cancel_requested %q, want open and marked canceling", runAfter.Status, runAfter.Metadata["gc.cancel_requested"])
	}
	for _, id := range []string{runStep.ID, itemStep.ID} {
		step, err := mem.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if step.Status == "closed" {
			t.Fatalf("step %s closed; this test needs the failure to land before any close", id)
		}
		stop, err := molecule.WorkflowStopReason(mem, step)
		if err != nil {
			t.Fatal(err)
		}
		if !stop.Stopped() || !stop.Canceled || stop.RootID != run.ID {
			t.Errorf("claim gate for open step %s = %+v, want it stopped as canceled by run root %s", id, stop, run.ID)
		}
	}

	out, err := s.humaHandleRunCancel(context.Background(), input)
	if err != nil {
		t.Fatalf("retried cancel error: %v", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("retried cancel status = %q, want canceled", out.Body.Status)
	}
	// run + run step + drain + item + item step.
	if out.Body.Closed != 5 {
		t.Errorf("retried cancel closed = %d, want 5", out.Body.Closed)
	}
	for _, id := range []string{run.ID, runStep.ID, drain.ID, item.ID, itemStep.ID} {
		b, err := mem.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != "closed" || b.Metadata["gc.outcome"] != "canceled" {
			t.Errorf("%s (%s) = status %q outcome %q, want closed canceled", id, b.Title, b.Status, b.Metadata["gc.outcome"])
		}
	}
}
