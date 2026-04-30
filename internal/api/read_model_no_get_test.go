package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

type getCountingStore struct {
	beads.Store
	gets atomic.Int64
}

func (s *getCountingStore) Get(id string) (beads.Bead, error) {
	s.gets.Add(1)
	return s.Store.Get(id)
}

func TestSessionListUsesLoadedSessionBeadsWithoutPerSessionGet(t *testing.T) {
	fs := newSessionFakeState(t)
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session A")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session B")
	counting := &getCountingStore{Store: fs.cityBeadStore}
	fs.cityBeadStore = counting

	h := newTestCityHandler(t, fs)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := counting.gets.Load(); got != 0 {
		t.Fatalf("store.Get calls = %d, want 0 for session list read model", got)
	}
}

func TestSessionListDoesNotProbePendingInteractions(t *testing.T) {
	fs := newSessionFakeState(t)
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session A")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session B")
	fs.sp.Calls = nil

	h := newTestCityHandler(t, fs)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, call := range fs.sp.Calls {
		if call.Method == "Pending" {
			t.Fatalf("session list called Pending for %s; calls=%#v", call.Name, fs.sp.Calls)
		}
	}
}

func TestRigListUsesProviderStateWithoutSessionStoreGet(t *testing.T) {
	state := newFakeState(t)
	counting := &getCountingStore{Store: beads.NewMemStore()}
	state.cityBeadStore = counting
	if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatalf("start provider session: %v", err)
	}

	h := newTestCityHandler(t, state)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/rigs"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp struct {
		Items []rigResponse `json:"items"`
		Total int           `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("rig response total/items = %d/%d, want 1/1", resp.Total, len(resp.Items))
	}
	if resp.Items[0].RunningCount != 1 {
		t.Fatalf("RunningCount = %d, want 1", resp.Items[0].RunningCount)
	}
	if got := counting.gets.Load(); got != 0 {
		t.Fatalf("store.Get calls = %d, want 0 for rig list read model", got)
	}
}
