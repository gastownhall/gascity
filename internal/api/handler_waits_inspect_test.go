package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// getWaitInspect drives the real GET /v0/city/{cityName}/wait/{id} handler
// end-to-end (no hand-built HTTP mock) and returns the recorder.
func getWaitInspect(t *testing.T, state State, id string) *httptest.ResponseRecorder {
	t.Helper()
	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, "/wait/"+id), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type waitInspectProblem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

func decodeWaitProblem(t *testing.T, rec *httptest.ResponseRecorder) waitInspectProblem {
	t.Helper()
	var p waitInspectProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body=%q)", err, rec.Body.String())
	}
	return p
}

// TestWaitInspect_Success is the happy path: a durable wait resolves to 200 with
// its WaitView body.
func TestWaitInspect_Success(t *testing.T) {
	wait := subSecondWaitBead("w-ok", time.Date(2026, 3, 2, 4, 5, 6, 0, time.UTC))
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStoreFrom(1, []beads.Bead{wait}, nil)

	rec := getWaitInspect(t, fs, "w-ok")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var view struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode WaitView: %v (body=%q)", err, rec.Body.String())
	}
	if view.ID != "w-ok" {
		t.Fatalf("id = %q, want w-ok", view.ID)
	}
}

// TestWaitInspect_MissingIDIsWaitNotFound is the finding-4 regression: an absent
// wait id must carry the wait-not-found problem type, not session-not-found
// (which humaStoreError would otherwise emit for the wrapped beads.ErrNotFound).
func TestWaitInspect_MissingIDIsWaitNotFound(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()

	rec := getWaitInspect(t, fs, "nonexistent")
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404; body=%q", rec.Code, rec.Body.String())
	}
	p := decodeWaitProblem(t, rec)
	if p.Type != "urn:gascity:error:wait-not-found" {
		t.Fatalf("type = %q, want urn:gascity:error:wait-not-found", p.Type)
	}
}

// TestWaitInspect_ExistingNonWaitIsWaitNotFound covers the pre-existing not-a-wait
// branch: an id that resolves to a non-wait bead keeps the machine-matchable
// "not_a_wait:" detail the CLI branches on, under the same wait-not-found type.
func TestWaitInspect_ExistingNonWaitIsWaitNotFound(t *testing.T) {
	task := beads.Bead{ID: "t-1", Type: "task", Status: "open", Title: "not a wait"}
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStoreFrom(1, []beads.Bead{task}, nil)

	rec := getWaitInspect(t, fs, "t-1")
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404; body=%q", rec.Code, rec.Body.String())
	}
	p := decodeWaitProblem(t, rec)
	if p.Type != "urn:gascity:error:wait-not-found" {
		t.Fatalf("type = %q, want wait-not-found", p.Type)
	}
	if !strings.HasPrefix(p.Detail, "not_a_wait:") {
		t.Fatalf("detail = %q, want not_a_wait: prefix", p.Detail)
	}
}

// TestWaitInspect_NoStoreIsServiceUnavailable covers the unconfigured-store 503.
func TestWaitInspect_NoStoreIsServiceUnavailable(t *testing.T) {
	fs := newFakeState(t) // cityBeadStore nil by default → no session store configured
	rec := getWaitInspect(t, fs, "w-any")
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503; body=%q", rec.Code, rec.Body.String())
	}
	p := decodeWaitProblem(t, rec)
	if p.Type != "urn:gascity:error:service-unavailable" {
		t.Fatalf("type = %q, want service-unavailable", p.Type)
	}
}

// TestWaitInspect_CacheNotLiveIs503 covers the cacheLiveOr503 gate: an unprimed
// CachingStore reports IsLive()==false and yields a store-unavailable 503.
func TestWaitInspect_CacheNotLiveIs503(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewCachingStoreForTest(beads.NewMemStore(), nil) // unprimed → not live

	rec := getWaitInspect(t, fs, "w-any")
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503; body=%q", rec.Code, rec.Body.String())
	}
	p := decodeWaitProblem(t, rec)
	if p.Type != "urn:gascity:error:store-unavailable" {
		t.Fatalf("type = %q, want store-unavailable (cacheLiveOr503)", p.Type)
	}
	if !strings.Contains(p.Detail, "cache_not_live") {
		t.Fatalf("detail = %q, want cache_not_live", p.Detail)
	}
}
