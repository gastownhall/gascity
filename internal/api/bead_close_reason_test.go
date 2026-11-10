package api

import (
	"bytes"
	"encoding/json"
	"errors"
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

func postBeadAction(t *testing.T, h http.Handler, state State, id, action string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/bead/"+id+"/"+action), body))
	return rec
}

// bdShapedReopenStore reopens the way bd does: the bead's close_reason goes,
// but metadata.close_reason stays behind.
type bdShapedReopenStore struct {
	beads.Store
}

func (s bdShapedReopenStore) Reopen(id string) error {
	closed, err := s.Get(id)
	if err != nil {
		return err
	}
	if err := s.Store.Reopen(id); err != nil {
		return err
	}
	if reason, ok := closed.Metadata["close_reason"]; ok {
		return s.SetMetadata(id, "close_reason", reason)
	}
	return nil
}

// A reason recorded by an earlier close must not resurface when a reopened
// bead is closed again without one, whether or not the store's reopen dropped
// the metadata the reason was recorded from.
func TestBeadCloseRouteWithoutReasonAfterReopenDropsTheOldReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store beads.Store
	}{
		{name: "reopen drops the metadata", store: beads.NewMemStore()},
		{name: "reopen keeps the metadata", store: bdShapedReopenStore{Store: beads.NewMemStore()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newFakeState(t)
			state.stores["myrig"] = tc.store
			bead, err := tc.store.Create(beads.Bead{Title: "close, reopen, close"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			h := newTestCityHandler(t, state)

			if rec := postBeadAction(t, h, state, bead.ID, "close", bytes.NewBufferString(`{"reason":"duplicate of gc-0"}`)); rec.Code != http.StatusOK {
				t.Fatalf("first close = %d: %s", rec.Code, rec.Body.String())
			}
			if rec := postBeadAction(t, h, state, bead.ID, "reopen", nil); rec.Code != http.StatusOK {
				t.Fatalf("reopen = %d: %s", rec.Code, rec.Body.String())
			}
			if rec := postBeadAction(t, h, state, bead.ID, "close", nil); rec.Code != http.StatusOK {
				t.Fatalf("second close = %d: %s", rec.Code, rec.Body.String())
			}

			got, err := tc.store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "closed" || got.CloseReason != "" {
				t.Fatalf("after close without a reason: status=%q CloseReason=%q, want closed with no reason", got.Status, got.CloseReason)
			}
			if reason, present := getBeadJSON(t, h, state, bead.ID)["close_reason"]; present {
				t.Fatalf("GET close_reason = %#v, want it omitted", reason)
			}
		})
	}
}

// Closing a bead that is already closed changes nothing, so a reason sent with
// it must not be stamped beside the reason the bead's close recorded.
func TestBeadCloseRouteOnClosedBeadKeepsItsReason(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	bead, err := store.Create(beads.Bead{Title: "closed twice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := newTestCityHandler(t, state)

	if rec := postBeadAction(t, h, state, bead.ID, "close", bytes.NewBufferString(`{"reason":"`+apiCloseReason+`"}`)); rec.Code != http.StatusOK {
		t.Fatalf("first close = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postBeadAction(t, h, state, bead.ID, "close", bytes.NewBufferString(`{"reason":"superseded by a later fix"}`)); rec.Code != http.StatusOK {
		t.Fatalf("second close = %d: %s", rec.Code, rec.Body.String())
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CloseReason != apiCloseReason || got.Metadata["close_reason"] != apiCloseReason {
		t.Fatalf("after closing again: CloseReason=%q metadata close_reason=%q, want both %q", got.CloseReason, got.Metadata["close_reason"], apiCloseReason)
	}
}

// failingCloseStore refuses every Close, standing in for a store whose close
// write fails after the handler stamped the reason.
type failingCloseStore struct {
	beads.Store
}

func (failingCloseStore) Close(string) error { return errors.New("close refused") }

// A close that fails must not leave the still-open bead carrying the reason
// the handler stamped for it.
func TestBeadCloseRouteFailedCloseDoesNotLeaveAReason(t *testing.T) {
	state := newFakeState(t)
	mem := beads.NewMemStore()
	state.stores["myrig"] = failingCloseStore{Store: mem}
	bead, err := mem.Create(beads.Bead{Title: "close fails"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := newTestCityHandler(t, state)

	rec := postBeadAction(t, h, state, bead.ID, "close", bytes.NewBufferString(`{"reason":"`+apiCloseReason+`"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST close = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	got, err := mem.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("status = closed, want the failed close to leave it open")
	}
	if reason := got.Metadata["close_reason"]; reason != "" {
		t.Fatalf("metadata.close_reason = %q on a bead whose close failed, want it empty", reason)
	}
}
