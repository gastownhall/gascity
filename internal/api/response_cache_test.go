package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

type countingStore struct {
	beads.Store

	listCalls           int
	listByLabelCalls    int
	listByAssigneeCalls int
}

func (s *countingStore) List() ([]beads.Bead, error) {
	s.listCalls++
	return s.Store.List()
}

func (s *countingStore) ListByLabel(label string, limit int) ([]beads.Bead, error) {
	s.listByLabelCalls++
	return s.Store.ListByLabel(label, limit)
}

func (s *countingStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	s.listByAssigneeCalls++
	return s.Store.ListByAssignee(assignee, status, limit)
}

func TestHandleStatusCachesUntilIndexChanges(t *testing.T) {
	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	srv := New(state)

	req := httptest.NewRequest(http.MethodGet, "/v0/status", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", rec.Code)
	}

	if store.listCalls != 1 {
		t.Fatalf("List calls after cached repeat = %d, want 1", store.listCalls)
	}

	state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("third status = %d, want 200", rec.Code)
	}
	if store.listCalls != 2 {
		t.Fatalf("List calls after index change = %d, want 2", store.listCalls)
	}
}

func TestHandleAgentListCachesUntilIndexChanges(t *testing.T) {
	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	srv := New(state)

	req := httptest.NewRequest(http.MethodGet, "/v0/agents", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first agents = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second agents = %d, want 200", rec.Code)
	}

	if store.listByAssigneeCalls != 1 {
		t.Fatalf("ListByAssignee calls after cached repeat = %d, want 1", store.listByAssigneeCalls)
	}

	state.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("third agents = %d, want 200", rec.Code)
	}
	if store.listByAssigneeCalls != 2 {
		t.Fatalf("ListByAssignee calls after index change = %d, want 2", store.listByAssigneeCalls)
	}
}

func TestHandleOrdersFeedCachesUntilIndexChanges(t *testing.T) {
	t.Skip("orders/feed endpoint not yet implemented")
	state := newFakeState(t)
	rigStore := &countingStore{Store: beads.NewMemStore()}
	cityStore := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = rigStore
	state.cityBeadStore = cityStore

	_, err := rigStore.Create(beads.Bead{
		Title: "Adopt PR",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-123",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}

	srv := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/orders/feed?scope_kind=rig&scope_ref=myrig", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first feed = %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal first feed: %v", err)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second feed = %d, want 200", rec.Code)
	}
	if rigStore.listCalls != 1 {
		t.Fatalf("rig List calls after cached repeat = %d, want 1", rigStore.listCalls)
	}
	if cityStore.listByLabelCalls != 1 {
		t.Fatalf("city ListByLabel calls after cached repeat = %d, want 1", cityStore.listByLabelCalls)
	}

	state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("third feed = %d, want 200", rec.Code)
	}
	if rigStore.listCalls != 2 {
		t.Fatalf("rig List calls after index change = %d, want 2", rigStore.listCalls)
	}
	if cityStore.listByLabelCalls != 2 {
		t.Fatalf("city ListByLabel calls after index change = %d, want 2", cityStore.listByLabelCalls)
	}
}
