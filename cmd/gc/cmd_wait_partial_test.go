package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
)

// waitPartialListStore returns its seeded rows alongside a beads.PartialResultError
// from List, modeling the degraded read the CLI fallback must tolerate.
type waitPartialListStore struct {
	beads.Store
	rows []beads.Bead
}

func (s waitPartialListStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return s.rows, &beads.PartialResultError{Op: "bd list", Err: errors.New("skipped 1 corrupt wait")}
}

// TestRouteWaitList_APIPartialShowsRowsAndNotice drives the CLI's typed /waits
// rung against a 200 response carrying partial=true + partial_errors and asserts
// the surviving row still renders to stdout while the degradation is surfaced on
// stderr (matching the generic /beads partial UX) rather than failing.
func TestRouteWaitList_APIPartialShowsRowsAndNotice(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"waits": []map[string]any{
				{"id": "w-partial", "session_id": "s-1", "kind": "deps", "state": "ready", "status": "open"},
			},
			"capped":         false,
			"partial":        true,
			"partial_errors": []string{"bd list: skipped 1 corrupt wait"},
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeWaitList(t.TempDir(), c, "", "", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("routeWaitList exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "w-partial") {
		t.Fatalf("stdout missing surviving wait row:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "showing partial results") {
		t.Fatalf("stderr missing partial degradation notice:\n%s", stderr.String())
	}
}

// TestWaitListFallbackDataFoldsPartialRows exercises the exact store call the
// local fallback (doWaitListFallback) makes: on a PartialResultError the front
// door returns the surviving rows so the fallback can display them instead of
// exiting 1.
func TestWaitListFallbackDataFoldsPartialRows(t *testing.T) {
	wait := beads.Bead{
		ID:       "w-partial",
		Type:     waitBeadType,
		Status:   "open",
		Labels:   []string{waitBeadLabel, "session:s-1"},
		Metadata: map[string]string{"session_id": "s-1", "state": waitStateReady, "kind": "deps"},
	}
	store := waitPartialListStore{Store: beads.NewMemStore(), rows: []beads.Bead{wait}}

	got, err := sessionFrontDoor(store).ListWaits("", "")
	if !beads.IsPartialResult(err) {
		t.Fatalf("err = %v, want PartialResultError folded through", err)
	}
	if len(got) != 1 || got[0].ID != "w-partial" {
		t.Fatalf("waits = %+v, want the surviving w-partial row preserved", got)
	}
}
