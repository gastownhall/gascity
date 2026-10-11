package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// partialWaitState wires a wait bead behind a store whose List returns the row
// alongside a beads.PartialResultError, so the /waits handler must degrade (200 +
// partial metadata) rather than 500. Reuses failingBeadStore from
// handler_beads_partial_test.go (same fixture technique).
func partialWaitState(t *testing.T) *fakeState {
	t.Helper()
	wait := subSecondWaitBead("w-partial", time.Date(2026, 3, 2, 4, 5, 6, 0, time.UTC))
	fs := newFakeState(t)
	fs.cityBeadStore = &failingBeadStore{
		Store:      beads.NewMemStore(),
		listResult: []beads.Bead{wait},
		listErr: &beads.PartialResultError{
			Op:  "bd list",
			Err: errors.New("skipped 1 corrupt wait"),
		},
	}
	return fs
}

// TestWaitListPreservesPartialResultRows is the real supervisor/generated-client
// regression for finding 3: a PartialResultError carrying reachable rows answers
// 200 with the surviving waits and partial metadata set, instead of 500-ing and
// hiding them.
func TestWaitListPreservesPartialResultRows(t *testing.T) {
	fs := partialWaitState(t)
	ts := httptest.NewServer(newTestCityHandler(t, fs))
	t.Cleanup(ts.Close)
	c := NewCityScopedClient(ts.URL, fs.CityName())

	cr, err := c.ListWaits("", "")
	if err != nil {
		t.Fatalf("ListWaits returned client error %v; a partial result must surface as 200", err)
	}
	if !cr.Body.Partial {
		t.Fatalf("Body.Partial = false, want true")
	}
	if len(cr.Body.PartialErrors) == 0 {
		t.Fatalf("Body.PartialErrors empty, want the degraded read surfaced")
	}
	if len(cr.Body.Waits) != 1 || cr.Body.Waits[0].ID != "w-partial" {
		t.Fatalf("Waits = %+v, want the surviving partial row preserved", cr.Body.Waits)
	}
}

// TestWaitListPartialWireBody pins the raw wire contract: 200 status, the
// surviving row, and partial=true + partial_errors on the body (mirrors
// TestBeadListPreservesPartialResultRows).
func TestWaitListPartialWireBody(t *testing.T) {
	fs := partialWaitState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/waits"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (handler must degrade, not fail); body=%q", rec.Code, rec.Body.String())
	}

	var body struct {
		Waits []struct {
			ID string `json:"id"`
		} `json:"waits"`
		Partial       bool     `json:"partial"`
		PartialErrors []string `json:"partial_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if !body.Partial {
		t.Errorf("partial = false, want true")
	}
	if len(body.PartialErrors) == 0 {
		t.Errorf("partial_errors empty, want the degraded read surfaced")
	}
	if len(body.Waits) != 1 || body.Waits[0].ID != "w-partial" {
		t.Errorf("waits = %+v, want the surviving partial row", body.Waits)
	}
}
