// Heartbeat-route acceptance. Store-tier fields (Revision, ClaimFence) are
// json:"-", so persistence and fencing are asserted through the in-process
// store rather than inferred from the wire response.
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// workerClaim is the setup every heartbeat case needs. Kept a helper rather
// than inlined so a heartbeat test cannot pass by skipping the claim.
func workerClaim(t *testing.T, h http.Handler, state State, beadID string) {
	t.Helper()
	body := `{"session_id":"gcg-session-1","assignee":"worker-local-3-pool","bead_id":"` + beadID + `"}`
	req := newPostRequest(cityURL(state, "/worker/claim"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup claim status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
}

func workerHeartbeat(t *testing.T, h http.Handler, state State, beadID, sessionID, assignee string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"session_id":"` + sessionID + `","assignee":"` + assignee + `","bead_id":"` + beadID + `"}`
	req := newPostRequest(cityURL(state, "/worker/heartbeat"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestWorkerHeartbeatPersistsLeaseRefresh is the core acceptance: the verb
// persists a lease refresh under the claim fence while retaining the original
// first-claim instant (gc.claimed_at, internal/beadmeta/keys.go:61).
func TestWorkerHeartbeatPersistsLeaseRefresh(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "keep me", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)
	workerClaim(t, h, state, created.ID)

	before, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	stamped := before.Metadata[beadmeta.ClaimedAtMetadataKey]
	if stamped == "" {
		t.Fatalf("setup claim stamped no %s — nothing for a heartbeat to refresh", beadmeta.ClaimedAtMetadataKey)
	}
	// cr-gdeav.5.4: the draft cited revBefore without binding it — the snapshot
	// this case compares the post-heartbeat revision against is `before`.
	revBefore := before.Revision

	rec := workerHeartbeat(t, h, state, created.ID, "gcg-session-1", "worker-local-3-pool")
	if rec.Code == http.StatusNotFound {
		t.Fatalf("POST /worker/heartbeat is not routed at all (404); body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	after, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get after heartbeat: %v", err)
	}
	// The freshness proof is the store's Revision, not the stamp string:
	// gc.claimed_at is RFC3339 at second resolution, so a heartbeat inside the
	// claim's own second cannot be distinguished by the text, and a test that
	// slept to see it would be flaky in the other direction. Revision is the
	// store-internal optimistic-concurrency token (internal/beads/beads.go:160-174)
	// and moves with any persisted mutation, so it proves the write landed.
	if after.Revision == revBefore {
		t.Fatalf("heartbeat returned 200 but persisted nothing (revision stayed %d): a route that advertises liveness without writing a lease is worse than no route", revBefore)
	}
	renewed := after.Metadata[beadmeta.ClaimedAtMetadataKey]
	if renewed == "" {
		t.Fatalf("heartbeat cleared %s instead of refreshing it", beadmeta.ClaimedAtMetadataKey)
	}
	if renewed < stamped {
		t.Fatalf("heartbeat moved %s backwards: %q -> %q", beadmeta.ClaimedAtMetadataKey, stamped, renewed)
	}
	if after.Assignee != "worker-local-3-pool" || after.Status != "in_progress" {
		t.Fatalf("heartbeat moved ownership: assignee=%q status=%q", after.Assignee, after.Status)
	}
}

// TestWorkerHeartbeatIsNotAnOwnershipTransition pins the fence semantics rather
// than the stamp: ClaimFence bumps ONLY on ownership transitions — a
// claim/unclaim/release, an assignee change, or a reopen — and never on a
// content mutation (internal/beads/beads.go:176-186). A heartbeat that bumps it
// would make every concurrent guarded release race its own holder's liveness
// signal.
func TestWorkerHeartbeatDoesNotBumpTheClaimFence(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "fence", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)
	workerClaim(t, h, state, created.ID)

	between, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	rec := workerHeartbeat(t, h, state, created.ID, "gcg-session-1", "worker-local-3-pool")
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	after, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get after heartbeat: %v", err)
	}
	if after.ClaimFence != between.ClaimFence {
		t.Fatalf("heartbeat bumped ClaimFence %d -> %d; a stale incarnation's guarded release must not be disarmed by the holder's liveness traffic",
			between.ClaimFence, after.ClaimFence)
	}
}

// TestWorkerHeartbeatRejectedForNonHolder: the verb is fenced to the holder. A
// second identity refreshing a bead it does not hold is refused, and the real
// holder's lease is untouched.
func TestWorkerHeartbeatRejectedForNonHolder(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "not yours", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)
	workerClaim(t, h, state, created.ID)

	held, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}

	rec := workerHeartbeat(t, h, state, created.ID, "gcg-session-2", "worker-local-4-pool")
	if rec.Code != http.StatusConflict && rec.Code != http.StatusForbidden {
		t.Fatalf("non-holder heartbeat = %d, want 409 or 403, body: %s", rec.Code, rec.Body.String())
	}
	after, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get after refusal: %v", err)
	}
	if after.Metadata[beadmeta.ClaimedAtMetadataKey] != held.Metadata[beadmeta.ClaimedAtMetadataKey] {
		t.Fatalf("a refused heartbeat refreshed the holder's lease")
	}
	if after.Metadata[beadmeta.LeaseOwnerMetadataKey] == "worker-local-4-pool" {
		t.Fatalf("a refused heartbeat took the lease: %s = %q", beadmeta.LeaseOwnerMetadataKey, after.Metadata[beadmeta.LeaseOwnerMetadataKey])
	}
}

// TestWorkerHeartbeatNamesTheLeaseItRefreshed is the reaper-visibility half of
// the acceptance, stated over the keys gc's own reapers read rather than over
// bd's lease table (which lives outside this repo — see the file header): after
// a heartbeat the bead must name its holder in gc.lease_owner
// (internal/beadmeta/keys.go:177) as well as carrying the refreshed instant, or
// the four live reapers (bd reclaim's lease table, releaseOrphanedPoolAssignments,
// the poisoned-assignment leg, the naming watchdogs) all read the claim as
// leaseless and the bead is stranded when the seat dies.
func TestWorkerHeartbeatNamesTheLeaseOwner(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "reapers", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)
	workerClaim(t, h, state, created.ID)

	rec := workerHeartbeat(t, h, state, created.ID, "gcg-session-1", "worker-local-3-pool")
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	after, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if got := after.Metadata[beadmeta.LeaseOwnerMetadataKey]; got != "worker-local-3-pool" {
		t.Fatalf("%s = %q, want the claiming identity — without it the claim is leaseless to every live reaper",
			beadmeta.LeaseOwnerMetadataKey, got)
	}
}
