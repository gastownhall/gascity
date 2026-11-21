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

// TestReproBeadListIgnoresIDsQuery pins what GET /v0/city/{cityName}/beads
// does with a query parameter it does not declare. BeadListInput has no ids
// selector (beads.ListQuery.IDs exists but the handler never sets it), and the
// operation does not set RejectUnknownQueryParameters, so ?ids=<id> is dropped
// and the answer is the unfiltered page. The same holds for a misspelled
// declared filter (?stauts=closed). /work/ready, by contrast, refuses an
// unknown parameter with 422 before touching a store.
//
// The test passes if the server either honors ?ids= or refuses it; it fails
// on the current behavior (silently widened answer).
func TestReproBeadListIgnoresIDsQuery(t *testing.T) {
	state := newFakeState(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id string, minutes int) beads.Bead {
		return beads.Bead{ID: id, Title: id, Status: "open", Type: "task", CreatedAt: base.Add(time.Duration(minutes) * time.Minute)}
	}
	state.stores["arig"] = beads.NewMemStoreFrom(0, []beads.Bead{mk("aa-1", 1), mk("aa-2", 2), mk("aa-3", 3)}, nil)
	h := newTestCityHandler(t, state)

	get := func(url string) (int, string, []string) {
		t.Helper()
		req := httptest.NewRequest("GET", cityURL(state, url), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		var resp struct {
			Items []beads.Bead `json:"items"`
			Total int          `json:"total"`
		}
		_ = json.Unmarshal([]byte(body), &resp)
		ids := []string{}
		for _, b := range resp.Items {
			ids = append(ids, b.ID)
		}
		if rec.Code == http.StatusOK {
			t.Logf("GET %s -> %d items=%v total=%d", url, rec.Code, ids, resp.Total)
		} else {
			t.Logf("GET %s -> %d body=%s", url, rec.Code, strings.TrimSpace(body))
		}
		return rec.Code, body, ids
	}

	// Control: the unfiltered list.
	code, _, all := get("/beads")
	if code != http.StatusOK || len(all) != 3 {
		t.Fatalf("control GET /beads: code %d ids %v, want 200 and 3 beads", code, all)
	}

	// 1. ?ids=aa-2: want the one bead, or a 422 naming the unknown parameter.
	code, _, got := get("/beads?ids=aa-2")
	switch {
	case code == http.StatusUnprocessableEntity:
		t.Logf("?ids= refused as an unknown parameter (422)")
	case code == http.StatusOK && len(got) == 1 && got[0] == "aa-2":
		t.Logf("?ids= honored")
	default:
		t.Errorf("GET /beads?ids=aa-2: want the one bead or a 422, got %d with %v (the ids parameter was silently dropped)", code, got)
	}

	// 2. A misspelled declared filter widens the answer the same way.
	code, _, got = get("/beads?stauts=closed")
	if code == http.StatusOK && len(got) == len(all) {
		t.Errorf("GET /beads?stauts=closed: unknown parameter ignored, answer widened to every bead %v", got)
	}

	// 3. Contrast: the sibling route refuses an unknown parameter.
	code, body, _ := get("/work/ready?ids=aa-2")
	if code != http.StatusUnprocessableEntity {
		t.Logf("note: /work/ready?ids= answered %d, not 422: %s", code, strings.TrimSpace(body))
	}

	// 4. The served OpenAPI spec: which query parameters GET /beads declares.
	req := httptest.NewRequest("GET", "/openapi.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var spec struct {
			Paths map[string]map[string]struct {
				Parameters []struct {
					Name string `json:"name"`
					In   string `json:"in"`
				} `json:"parameters"`
			} `json:"paths"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
			t.Logf("spec decode: %v", err)
		} else {
			var names []string
			for _, p := range spec.Paths["/v0/city/{cityName}/beads"]["get"].Parameters {
				if p.In == "query" {
					names = append(names, p.Name)
				}
			}
			t.Logf("openapi.json: GET /v0/city/{cityName}/beads query parameters = %v", names)
		}
	} else {
		t.Logf("GET /openapi.json -> %d", rec.Code)
	}
}
