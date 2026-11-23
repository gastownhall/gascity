package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBeadListRejectsUnknownQueryParameters pins that GET
// /v0/city/{cityName}/beads refuses a query parameter it does not declare
// with a 422 naming the parameter, the same as /work/ready. An undeclared
// selector (?ids=) or a misspelled filter (?stauts=) must error rather than
// silently widen the answer to every bead (gastownhall/gascity#8426).
// Declared filters and the embedded pagination parameters must still be
// accepted.
func TestBeadListRejectsUnknownQueryParameters(t *testing.T) {
	state := newFakeState(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id string, minutes int) beads.Bead {
		return beads.Bead{ID: id, Title: id, Status: "open", Type: "task", CreatedAt: base.Add(time.Duration(minutes) * time.Minute)}
	}
	state.stores["arig"] = beads.NewMemStoreFrom(0, []beads.Bead{mk("aa-1", 1), mk("aa-2", 2), mk("aa-3", 3)}, nil)
	h := newTestCityHandler(t, state)

	get := func(url string) (int, []byte) {
		t.Helper()
		req := httptest.NewRequest("GET", cityURL(state, url), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	listIDs := func(url string, body []byte) []string {
		t.Helper()
		var resp struct {
			Items []beads.Bead `json:"items"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("GET %s: decoding list body: %v (body=%s)", url, err, body)
		}
		ids := make([]string, 0, len(resp.Items))
		for _, b := range resp.Items {
			ids = append(ids, b.ID)
		}
		return ids
	}
	wantRejected := func(url, location string) {
		t.Helper()
		code, body := get(url)
		if code != http.StatusUnprocessableEntity {
			t.Errorf("GET %s: code %d, want 422 (body=%s)", url, code, strings.TrimSpace(string(body)))
			return
		}
		var problem struct {
			Errors []struct {
				Location string `json:"location"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(body, &problem); err != nil {
			t.Fatalf("GET %s: decoding problem body: %v (body=%s)", url, err, body)
		}
		for _, e := range problem.Errors {
			if e.Location == location {
				return
			}
		}
		t.Errorf("GET %s: problem errors %+v have no location %q", url, problem.Errors, location)
	}
	wantOK := func(url string) []string {
		t.Helper()
		code, body := get(url)
		if code != http.StatusOK {
			t.Fatalf("GET %s: code %d, want 200 (body=%s)", url, code, strings.TrimSpace(string(body)))
		}
		return listIDs(url, body)
	}

	// Control: the unfiltered list.
	if all := wantOK("/beads"); len(all) != 3 {
		t.Fatalf("control GET /beads: ids %v, want 3 beads", all)
	}

	// Undeclared and misspelled parameters are refused, naming the parameter.
	wantRejected("/beads?ids=aa-2", "query.ids")
	wantRejected("/beads?stauts=closed", "query.stauts")

	// Declared filters and embedded pagination params still work.
	if got := wantOK("/beads?status=open&limit=2"); len(got) != 2 {
		t.Errorf("GET /beads?status=open&limit=2: ids %v, want 2 beads", got)
	}
	wantOK("/beads?all=true")
	if got := wantOK("/beads?limit=1"); len(got) != 1 {
		t.Errorf("GET /beads?limit=1: ids %v, want 1 bead", got)
	}
	wantOK("/beads?cursor=")
}
