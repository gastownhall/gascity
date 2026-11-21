package api

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// failRouteStore refuses the gc.routed_to write for one bead with err (a
// generic failure when err is nil), so a convoy sling routes every other child
// and fails that one.
type failRouteStore struct {
	beads.Store
	failID string
	err    error
}

func (s failRouteStore) SetMetadata(id, key, value string) error {
	if id == s.failID && key == beadmeta.RoutedToMetadataKey {
		if s.err != nil {
			return s.err
		}
		return errors.New("injected route failure")
	}
	return s.Store.SetMetadata(id, key, value)
}

// TestSlingConvoyPartialFailureReturnsPerChildOutcomes pins the partial
// convoy contract: when some children were routed and others failed, the
// sling already changed state, so POST /sling answers with the per-child
// outcomes (status "partial") instead of a bare error that hides which
// children were routed and makes a whole-request retry unsafe. gc sling
// reports the same per-child lines and exits non-zero.
func TestSlingConvoyPartialFailureReturnsPerChildOutcomes(t *testing.T) {
	h, state := newSlingTestServer(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	var children []beads.Bead
	for _, title := range []string{"first", "second", "done"} {
		child, err := store.Create(beads.Bead{Title: title, Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	if err := store.Close(children[2].ID); err != nil {
		t.Fatal(err)
	}
	routedChild, failedChild, closedChild := children[0], children[1], children[2]
	state.stores["myrig"] = failRouteStore{Store: store, failID: failedChild.ID}

	rec, resp := postSlingJSON(t, h, state, `{"target":"myrig/worker","bead":"`+convoy.ID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a partial result; body = %s", rec.Code, rec.Body.String())
	}
	if resp.Status != "partial" {
		t.Fatalf("status field = %q, want partial", resp.Status)
	}
	if resp.Batch == nil || resp.Batch.Routed != 1 || resp.Batch.Failed != 1 || resp.Batch.Total != 3 {
		t.Fatalf("batch = %+v, want 1 routed, 1 failed of 3", resp.Batch)
	}
	byID := map[string]SlingChildOutcome{}
	for _, c := range resp.Children {
		byID[c.BeadID] = c
	}
	if len(byID) != 3 {
		t.Fatalf("children = %+v, want all 3 children reported", resp.Children)
	}
	if got := byID[routedChild.ID]; got.Outcome != "routed" || got.Reason != "" {
		t.Fatalf("routed child = %+v, want outcome routed", got)
	}
	if got := byID[failedChild.ID]; got.Outcome != "failed" || got.Reason == "" {
		t.Fatalf("failed child = %+v, want outcome failed with a reason", got)
	}
	if got := byID[closedChild.ID]; got.Outcome != "skipped" || got.Status != "closed" {
		t.Fatalf("closed child = %+v, want outcome skipped with status closed", got)
	}
	got, err := store.Get(routedChild.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "myrig/worker" {
		t.Fatalf("routed child gc.routed_to = %q, want myrig/worker", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// TestSlingConvoyPartialFailureCarriesChildConflict: a child refused because
// its source bead already has a live workflow still yields 200 partial, and its
// outcome names the blocking workflows and the cleanup command, the detail a
// whole-request conflict puts in its 409 problem. gc sling exits 3 on it, as it
// does locally.
func TestSlingConvoyPartialFailureCarriesChildConflict(t *testing.T) {
	h, state := newSlingTestServer(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	var children []beads.Bead
	for _, title := range []string{"routed", "conflicted"} {
		child, err := store.Create(beads.Bead{Title: title, Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	routedChild, conflictedChild := children[0], children[1]
	state.stores["myrig"] = failRouteStore{
		Store:  store,
		failID: conflictedChild.ID,
		err:    &sourceworkflow.ConflictError{SourceBeadID: conflictedChild.ID, WorkflowIDs: []string{"wf-1"}},
	}

	rec, resp := postSlingJSON(t, h, state, `{"target":"myrig/worker","bead":"`+convoy.ID+`"}`)
	if rec.Code != http.StatusOK || resp.Status != SlingStatusPartial {
		t.Fatalf("status = %d/%q, want 200 partial; body = %s", rec.Code, resp.Status, rec.Body.String())
	}
	byID := map[string]SlingChildOutcome{}
	for _, c := range resp.Children {
		byID[c.BeadID] = c
	}
	if got := byID[routedChild.ID]; got.Outcome != "routed" || got.Conflict != nil {
		t.Fatalf("routed child = %+v, want outcome routed with no conflict", got)
	}
	got := byID[conflictedChild.ID]
	if got.Outcome != "failed" || got.Reason == "" {
		t.Fatalf("conflicted child = %+v, want outcome failed with a reason", got)
	}
	want := &SlingChildConflict{
		SourceBeadID:        conflictedChild.ID,
		BlockingWorkflowIDs: []string{"wf-1"},
		Hint:                "gc workflow delete-source " + conflictedChild.ID + " --store-ref rig:myrig --apply",
	}
	if !reflect.DeepEqual(got.Conflict, want) {
		t.Fatalf("conflicted child conflict = %+v, want %+v", got.Conflict, want)
	}
}

// TestSlingConvoyAllChildrenFailedStaysAnError keeps a batch that routed
// nothing an error: nothing changed, so a retry is safe and the caller gets
// the failure, as before.
func TestSlingConvoyAllChildrenFailedStaysAnError(t *testing.T) {
	h, state := newSlingTestServer(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{Title: "only", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	state.stores["myrig"] = failRouteStore{Store: store, failID: child.ID}

	rec, _ := postSlingJSON(t, h, state, `{"target":"myrig/worker","bead":"`+convoy.ID+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when no child was routed; body = %s", rec.Code, rec.Body.String())
	}
}

// TestSlingConvoySuccessReportsChildren: a fully routed convoy also lists its
// children, so a client never has to re-derive them.
func TestSlingConvoySuccessReportsChildren(t *testing.T) {
	h, state := newSlingTestServer(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{Title: "only", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	rec, resp := postSlingJSON(t, h, state, `{"target":"myrig/worker","bead":"`+convoy.ID+`"}`)
	if rec.Code != http.StatusOK || resp.Status != "slung" {
		t.Fatalf("status = %d/%q, want 200 slung; body = %s", rec.Code, resp.Status, rec.Body.String())
	}
	if len(resp.Children) != 1 || resp.Children[0].BeadID != child.ID || resp.Children[0].Outcome != "routed" {
		t.Fatalf("children = %+v, want the one routed child", resp.Children)
	}
}
