package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestBeadReopenLeavesASessionsRuntimeLease (mc-dkblm): the bead API
// refuses a session row's reopen before anything is written, so the closed
// row stays closed with its lease record as it was; the session verbs own
// a session row's reopen.
func TestBeadReopenLeavesASessionsRuntimeLease(t *testing.T) {
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
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reopen status = %d: %s, want 422", rec.Code, rec.Body.String())
	}
	got, _ := store.Get(b.ID)
	if got.Status != "closed" || got.Metadata[session.RuntimeLeaseHolderKey] != "h/1/n" || got.Metadata[session.RuntimeLeaseEpochKey] != "3" {
		t.Fatalf("session = %s %v, want closed with its lease record untouched", got.Status, got.Metadata)
	}
}
