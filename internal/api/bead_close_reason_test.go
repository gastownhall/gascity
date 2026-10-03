package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

// These tests pin the supervisor API's view of why a bead was closed
// (gastownhall/gascity#2663): GET /bead/{id} and GET /beads return
// close_reason, POST /bead/{id}/close accepts {"reason": "..."}, and the
// bead.closed event carries the reason.

const apiCloseReason = "fixed in commit abc123; tests pass"

// closedPayloads collects the decoded bead of every bead.closed the city's
// caching store announces.
type closedPayloads struct {
	mu    sync.Mutex
	beads []beads.Bead
}

func (c *closedPayloads) onChange(t *testing.T) func(eventType, beadID string, payload json.RawMessage) {
	return func(eventType, beadID string, payload json.RawMessage) {
		if eventType != "bead.closed" {
			return
		}
		b, ok := beads.DecodeBeadEventPayload(payload)
		if !ok {
			t.Errorf("decode bead.closed payload for %s: %s", beadID, payload)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.beads = append(c.beads, b)
	}
}

func (c *closedPayloads) only(t *testing.T, id string) beads.Bead {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var found []beads.Bead
	for _, b := range c.beads {
		if b.ID == id {
			found = append(found, b)
		}
	}
	if len(found) != 1 {
		t.Fatalf("bead.closed events for %s = %d, want 1: %+v", id, len(found), c.beads)
	}
	return found[0]
}

func getBeadJSON(t *testing.T, h http.Handler, state State, id string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/bead/"+id), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /bead/%s = %d: %s", id, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode GET /bead/%s: %v", id, err)
	}
	return body
}

// A close made outside the API (bd close --reason lands in the store that
// backs the city) is visible on GET /bead/{id} and GET /beads.
func TestBeadGetAndListReturnCloseReason(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	closed, err := store.Create(beads.Bead{Title: "closed out of band"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	open, err := store.Create(beads.Bead{Title: "still open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(closed.ID, "close_reason", apiCloseReason); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h := newTestCityHandler(t, state)

	if got := getBeadJSON(t, h, state, closed.ID)["close_reason"]; got != apiCloseReason {
		t.Fatalf("GET close_reason = %#v, want %q", got, apiCloseReason)
	}
	if _, present := getBeadJSON(t, h, state, open.ID)["close_reason"]; present {
		t.Fatal("GET of an open bead carries close_reason, want it omitted")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/beads?all=true"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /beads = %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode GET /beads: %v", err)
	}
	found := false
	for _, item := range list.Items {
		if item["id"] == closed.ID {
			found = true
			if item["close_reason"] != apiCloseReason {
				t.Fatalf("GET /beads close_reason = %#v, want %q", item["close_reason"], apiCloseReason)
			}
		}
	}
	if !found {
		t.Fatalf("GET /beads?all=true did not list %s: %s", closed.ID, rec.Body.String())
	}
}

// POST /bead/{id}/close {"reason": "..."} records the reason in the store, on
// GET, and on the bead.closed event.
func TestBeadCloseRouteRecordsReason(t *testing.T) {
	state := newFakeState(t)
	mem := beads.NewMemStore()
	events := &closedPayloads{}
	cache := beads.NewCachingStoreForTest(mem, events.onChange(t))
	state.stores["myrig"] = cache
	bead, err := cache.Create(beads.Bead{Title: "close me with a reason"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := newTestCityHandler(t, state)

	body := `{"reason":"  ` + apiCloseReason + `  "}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/bead/"+bead.ID+"/close"), bytes.NewBufferString(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST close = %d: %s", rec.Code, rec.Body.String())
	}

	stored, err := mem.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get from backing store: %v", err)
	}
	if stored.Status != "closed" {
		t.Fatalf("stored status = %q, want closed", stored.Status)
	}
	if stored.CloseReason != apiCloseReason {
		t.Fatalf("stored CloseReason = %q, want %q", stored.CloseReason, apiCloseReason)
	}
	if got := getBeadJSON(t, h, state, bead.ID)["close_reason"]; got != apiCloseReason {
		t.Fatalf("GET close_reason = %#v, want %q", got, apiCloseReason)
	}
	if ev := events.only(t, bead.ID); ev.CloseReason != apiCloseReason {
		t.Fatalf("bead.closed close_reason = %q, want %q", ev.CloseReason, apiCloseReason)
	}
}

// The body stays optional: clients that send nothing, an empty object, or a
// blank reason still close the bead, with no reason recorded.
func TestBeadCloseRouteWithoutReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		body io.Reader
	}{
		{name: "no body", body: nil},
		{name: "empty object", body: bytes.NewBufferString(`{}`)},
		{name: "blank reason", body: bytes.NewBufferString(`{"reason":"   "}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newFakeState(t)
			store := state.stores["myrig"]
			bead, err := store.Create(beads.Bead{Title: "close me"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			h := newTestCityHandler(t, state)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newPostRequest(cityURL(state, "/bead/"+bead.ID+"/close"), tc.body))
			if rec.Code != http.StatusOK {
				t.Fatalf("POST close = %d: %s", rec.Code, rec.Body.String())
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "closed" {
				t.Fatalf("status = %q, want closed", got.Status)
			}
			if got.CloseReason != "" {
				t.Fatalf("CloseReason = %q, want empty", got.CloseReason)
			}
			if _, stamped := got.Metadata["close_reason"]; stamped {
				t.Fatalf("metadata.close_reason stamped without a reason: %v", got.Metadata)
			}
		})
	}
}

// The Go API client (used by gc against a running supervisor) keeps the reason
// when it translates a wire bead back into beads.Bead.
func TestBeadFromGenCarriesCloseReason(t *testing.T) {
	reason := apiCloseReason
	got := beadFromGen(genclient.Bead{Id: "gc-1", Title: "done", Status: "closed", IssueType: "task", CloseReason: &reason})
	if got.CloseReason != apiCloseReason {
		t.Fatalf("CloseReason = %q, want %q", got.CloseReason, apiCloseReason)
	}
}
