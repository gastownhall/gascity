package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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

// TestRunCancelAtomicRootCloseFailureRollsBackMarker guards the partial-close
// finding on an atomic store: when the root's close fails AFTER its cancel marker
// was written in the same transaction, the atomic Tx rolls the marker back. The
// root stays open with no half-set gc.cancel_requested / gc.outcome=canceled, so
// nothing strands it projecting "canceling"; the caller still gets a retryable
// 5xx. This exercises the metadata-write-succeeds-then-close-fails path that a
// fake failing CloseAll before mutation cannot reach.
func TestRunCancelAtomicRootCloseFailureRollsBackMarker(t *testing.T) {
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
	// transactional close fails after its marker write, and AtomicTx=true forces
	// the whole Tx to roll back.
	fs.stores["myrig"] = txCloseFailStore{Store: mem, failCloseID: root.ID}
	s := &Server{state: fs}

	_, err = s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     root.ID,
	})
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
	if got := after.Metadata["gc.cancel_requested"]; got != "" {
		t.Errorf("root gc.cancel_requested = %q, want empty — the atomic Tx must roll the marker back on a failed close", got)
	}
	if got := after.Metadata["gc.outcome"]; got == "canceled" {
		t.Error("root gc.outcome was rewritten to canceled despite the close failing; the marker must roll back")
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
