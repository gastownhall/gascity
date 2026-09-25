package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

func TestHandleSessionResetRequestsFreshRestart(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "reset-test")
	pokesBefore := fs.pokeCount

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/reset", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		ID     string `json:"id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" || body.ID != info.ID {
		t.Fatalf("reset response = %+v, want status ok id %q", body, info.ID)
	}

	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	if got := b.Metadata["restart_requested"]; got != "true" {
		t.Errorf("restart_requested = %q, want true", got)
	}
	if got := b.Metadata["continuation_reset_pending"]; got != "true" {
		t.Errorf("continuation_reset_pending = %q, want true", got)
	}
	if fs.pokeCount != pokesBefore+1 {
		t.Errorf("pokeCount = %d, want %d", fs.pokeCount, pokesBefore+1)
	}

	evs, err := fs.eventProv.List(events.Filter{Type: events.WorkerOperation})
	if err != nil {
		t.Fatalf("List events: %v", err)
	}
	var found bool
	for _, ev := range evs {
		var p WorkerOperationEventPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("decode worker.operation payload: %v", err)
		}
		if p.Operation == "reset" && p.SessionID == info.ID {
			found = true
			if p.Result != "succeeded" {
				t.Errorf("reset operation result = %q, want succeeded", p.Result)
			}
		}
	}
	if !found {
		t.Errorf("no worker.operation reset event recorded for %s", info.ID)
	}
}

func TestHandleSessionResetNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/nonexistent/reset"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("reset nonexistent status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if fs.pokeCount != 0 {
		t.Errorf("pokeCount = %d, want 0 for an unknown session", fs.pokeCount)
	}
}

func TestHandleSessionResetClosedSessionConflicts(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "reset-closed-test")
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pokesBefore := fs.pokeCount

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/reset", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("reset closed status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	if got := b.Metadata["continuation_reset_pending"]; got != "" {
		t.Errorf("continuation_reset_pending = %q on a closed session, want unset", got)
	}
	if fs.pokeCount != pokesBefore {
		t.Errorf("pokeCount = %d, want %d (no poke on a rejected reset)", fs.pokeCount, pokesBefore)
	}
}

func TestHandleSessionResetRequiresCSRFHeader(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "reset-csrf-test")

	req := httptest.NewRequest(http.MethodPost, cityURL(fs, "/session/")+info.ID+"/reset", strings.NewReader(""))
	// No X-GC-Request header.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("reset without CSRF status = %d, want %d; body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	if got := b.Metadata["restart_requested"]; got != "" {
		t.Errorf("restart_requested = %q after CSRF rejection, want unset", got)
	}
}
