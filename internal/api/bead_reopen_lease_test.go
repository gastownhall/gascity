package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestBeadReopenClearsASessionsRuntimeLease: reopening a closed session bead
// through the bead API clears its runtime lease record first, keeping the
// epoch, so the reopened row hands no old holder its lease.
func TestBeadReopenClearsASessionsRuntimeLease(t *testing.T) {
	state := newFakeState(t)
	store := newPrefixedAliasStore("myrig-")
	state.stores["myrig"] = store
	b, err := store.Create(beads.Bead{
		Title: "worker", Type: session.BeadType, Labels: []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "worker", session.RuntimeLeaseHolderKey: "h/1/n", session.RuntimeLeaseEpochKey: "3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(rec, newPostRequest(cityURL(state, "/bead/")+b.ID+"/reopen", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reopen status = %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := store.Get(b.ID)
	if got.Status != "open" || got.Metadata[session.RuntimeLeaseHolderKey] != "" || got.Metadata[session.RuntimeLeaseEpochKey] != "3" {
		t.Fatalf("reopened session = %s %v, want open with no lease holder and the epoch kept", got.Status, got.Metadata)
	}
}
