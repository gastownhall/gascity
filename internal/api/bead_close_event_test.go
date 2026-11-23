package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Both API spellings of "close this bead" must announce bead.closed exactly
// once through the caching store that serves the city, so a client watching
// the event stream for closes sees them whichever route the closer used
// (gastownhall/gascity#6860).
func TestBeadCloseRoutesEmitBeadClosedOnce(t *testing.T) {
	cases := []struct {
		name string
		path func(id string) string
		body string
	}{
		{name: "close route", path: func(id string) string { return "/bead/" + id + "/close" }, body: `{}`},
		{name: "update status=closed", path: func(id string) string { return "/bead/" + id + "/update" }, body: `{"status":"closed"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newFakeState(t)
			var mu sync.Mutex
			var emitted []string
			cache := beads.NewCachingStoreForTest(beads.NewMemStore(), func(eventType, _ string, payload json.RawMessage) {
				var b beads.Bead
				if err := json.Unmarshal(payload, &b); err != nil {
					t.Errorf("decode %s payload: %v", eventType, err)
				}
				mu.Lock()
				defer mu.Unlock()
				emitted = append(emitted, eventType+"/"+b.Status)
			})
			state.stores["myrig"] = cache
			bead, err := cache.Create(beads.Bead{Title: "plain task"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			mu.Lock()
			emitted = nil
			mu.Unlock()

			h := newTestCityHandler(t, state)
			req := newPostRequest(cityURL(state, tc.path(bead.ID)), bytes.NewBufferString(tc.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			mu.Lock()
			defer mu.Unlock()
			closedEvents := 0
			for _, e := range emitted {
				switch e {
				case "bead.closed/closed":
					closedEvents++
				case "bead.updated/closed":
					t.Fatalf("close announced as bead.updated(status=closed); events=%v", emitted)
				}
			}
			if closedEvents != 1 {
				t.Fatalf("bead.closed events = %d, want exactly 1; events=%v", closedEvents, emitted)
			}
		})
	}
}
