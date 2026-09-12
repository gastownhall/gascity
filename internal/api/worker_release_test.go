// Release-route acceptance for the existing
// ConditionalAssignmentReleaser.ReleaseIfCurrent primitive. The route is only
// transport for that compare-and-set operation, never a second implementation.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// releasingHiddenStoreDraft hides the conditional-release capability by
// embedding the INTERFACE rather than the concrete store: only Store's methods
// are promoted, so the handler's capability assertion fails the same way it
// would against a backend that genuinely lacks it.
type releasingHiddenStoreDraft struct {
	beads.Store
}

// workerRelease is DELETE /worker/claim with a body — the shape the edge family
// already settled on, kept here so the PR's route name is pinned by the test
// rather than invented in the review lane.
func workerRelease(t *testing.T, h http.Handler, state State, beadID, assignee string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"session_id":"gcg-session-1","assignee":"` + assignee + `","bead_id":"` + beadID + `"}`
	req := httptest.NewRequest(http.MethodDelete, cityURL(state, "/worker/claim"), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// cr-gdeav.5.4: the draft sent the DELETE bare and so asserted the CSRF
	// middleware's 403 rather than the release verb. X-GC-Request is what
	// newPostRequest sets for every other mutation in this package
	// (internal/api/fake_state_test.go:29-32) — the always-installed middleware
	// requires it on every mutating route, so a release test without it never
	// reaches the handler it is meant to exercise.
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func releaseStatus(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode release output: %v (body: %s)", err, rec.Body.String())
	}
	return out.Status
}

// TestWorkerReleaseReturnsTheBeadToThePool: the happy path clears the holder and
// puts the bead back where a claim-candidate scan can find it.
func TestWorkerReleaseReturnsTheBeadToThePool(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "hand back", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)
	workerClaim(t, h, state, created.ID)

	rec := workerRelease(t, h, state, created.ID, "worker-local-3-pool")
	if rec.Code == http.StatusNotFound {
		t.Fatalf("DELETE /worker/claim is not routed at all (404); body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("release status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if got := releaseStatus(t, rec); got != "released" {
		t.Errorf("release status field = %q, want %q", got, "released")
	}

	stored, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Assignee != "" {
		t.Errorf("assignee after release = %q, want empty", stored.Assignee)
	}
	if stored.Status != "open" {
		t.Errorf("status after release = %q, want open — an in_progress bead with no holder is work no lane can claim", stored.Status)
	}
}

// TestWorkerReleaseReleasesOnlyTheHolder is the whole point of the CAS: an
// identity that is not the current holder gets a reported skip, and the real
// holder's claim survives. This is the failure mode the city has already been
// burned by — a reaper keyed on holder "liveness" stripped a live crew's claim
// twice on 2026-08-22 because the releaser did not compare the pointer it was
// given (AGENTS.md, lease-sweep retirement 8ee1796).
func TestWorkerReleaseReleasesOnlyTheHolder(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "not yours to give back", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)
	workerClaim(t, h, state, created.ID)

	rec := workerRelease(t, h, state, created.ID, "worker-local-9-pool")
	if rec.Code != http.StatusOK {
		t.Fatalf("non-holder release = %d, want 200 with a skipped result, body: %s", rec.Code, rec.Body.String())
	}
	if got := releaseStatus(t, rec); got != "skipped" {
		t.Fatalf("non-holder release status = %q, want %q", got, "skipped")
	}
	stored, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Assignee != "worker-local-3-pool" || stored.Status != "in_progress" {
		t.Fatalf("a stranger's release moved the holder's claim: assignee=%q status=%q", stored.Assignee, stored.Status)
	}
}

// TestWorkerReleaseSkipIsNotAnError pins the response vocabulary specifically,
// because the caller's unwind path depends on it: a worker whose stdout pipe
// closed mid-startup must release its own claim, and it has to tell "I gave it
// back" from "someone else already holds it" WITHOUT treating the second as a
// failure that would mask the real reason it is exiting.
func TestWorkerReleaseSkipIsNotAnError(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "unwind", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)

	// Nothing was ever claimed: the skip must be a 200 result, not a 4xx.
	rec := workerRelease(t, h, state, created.ID, "worker-local-3-pool")
	if rec.Code != http.StatusOK {
		t.Fatalf("release of an unclaimed bead = %d, want 200 (a skip is a result, not an error), body: %s", rec.Code, rec.Body.String())
	}
	if got := releaseStatus(t, rec); got != "skipped" {
		t.Fatalf("release status = %q, want %q", got, "skipped")
	}
}

// TestWorkerReleaseRefusesInsteadOfEmulatingWhenStoreLacksTheCapability: the
// adapter's own rule, lifted to the route — a store without the capability
// reports unavailable rather than emulating the CAS with a read-then-write
// (internal/storebinding/beads_adapter.go:495-499, :509-512).
func TestWorkerReleaseRefusesInsteadOfEmulatingWhenStoreLacksTheCapability(t *testing.T) {
	base := beads.NewMemStore()
	created, err := base.Create(beads.Bead{Title: "unsupported", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	assignee := "worker-local-3-pool"
	if err := base.Update(created.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": releasingHiddenStoreDraft{Store: base}}
	state.cityBeadStore = releasingHiddenStoreDraft{Store: base}
	h := newTestCityHandler(t, state)

	rec := workerRelease(t, h, state, created.ID, assignee)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("release on a capability-less store = %d, want 501, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Assignee != assignee || stored.Status != status {
		t.Fatalf("refused release must write nothing; got assignee=%q status=%q — an emulated release is the lost-update this capability exists to prevent", stored.Assignee, stored.Status)
	}
}
