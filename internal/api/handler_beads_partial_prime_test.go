package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBeadListDefaultIncludesBlockedRowsDuringPartialCachePrime(t *testing.T) {
	t.Parallel()

	state := newFakeState(t)
	backing := beads.NewMemStore()
	for _, status := range []string{"open", "in_progress", "blocked"} {
		bead, err := backing.Create(beads.Bead{Title: status})
		if err != nil {
			t.Fatalf("Create(%s): %v", status, err)
		}
		if status != "open" {
			if err := backing.Update(bead.ID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatalf("Update(%s): %v", status, err)
			}
		}
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	state.stores["myrig"] = cache

	recorder := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(
		recorder,
		httptest.NewRequest("GET", cityURL(state, "/beads"), nil),
	)
	if recorder.Code != 200 {
		t.Fatalf("GET /beads status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	statuses := make(map[string]bool, len(response.Items))
	for _, item := range response.Items {
		statuses[item.Status] = true
	}
	if response.Total != 3 || !statuses["open"] || !statuses["in_progress"] || !statuses["blocked"] {
		t.Fatalf("GET /beads total=%d statuses=%v, want all three nonclosed statuses", response.Total, statuses)
	}
}
