// Route-level acceptance for remote worker claims. The tests use the real city
// handler and assert the persisted ownership and lease metadata.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// claimingMemStoreDraft is the test double for the store capability the claim
// handler must delegate to. The capability is real upstream and merely
// unexported over HTTP: beadsAssignmentClaimer
// (internal/storebinding/beads_adapter.go:492-494) with a sqlite implementation
// (internal/beads/sqlite_store_claim.go:22) and an equivalent test double
// (internal/storebinding/beads_adapter_graph_capability_test.go:87). It is
// deliberately NOT beads.MemStore plus a call to a method MemStore does not
// have at this commit — the whole point of the double is that the handler
// discovers the capability by assertion and refuses when it is absent, which is
// the second test below.
type claimingMemStoreDraft struct {
	*beads.MemStore
	claimCalls []string
}

// Claim mirrors the contract: single winner, idempotent for the holder already
// named, and the claim is what moves the bead to in_progress.
func (s *claimingMemStoreDraft) Claim(id, assignee string) (beads.Bead, bool, error) {
	s.claimCalls = append(s.claimCalls, id+"|"+assignee)
	cur, err := s.Get(id)
	if err != nil {
		return beads.Bead{}, false, err
	}
	if cur.Assignee != "" && cur.Assignee != assignee {
		return cur, false, nil
	}
	status := "in_progress"
	if err := s.Update(id, beads.UpdateOpts{Assignee: &assignee, Status: &status}); err != nil {
		return beads.Bead{}, false, err
	}
	if cur.Assignee == assignee && cur.Status == status {
		return cur, false, nil // already held: idempotent, not a fresh acquisition
	}
	// cr-gdeav.5.4: the draft returned Get's two values from a three-value
	// contract. The bead comes back with ok=true — this call acquired it.
	final, err := s.Get(id)
	return final, err == nil, err
}

// TestWorkerClaimSetsAssigneeAndReturnsTheBeadWhole is the primary acceptance:
// POST /v0/city/{city}/worker/claim takes a routed-open bead for a named
// identity, and the response carries the bead the store persisted so the caller
// does not need a second read to learn the token it must echo back later.
//
// Handler under construction: humaHandleWorkerClaim (new file
// internal/api/handler_worker.go), delegating to
// internal/storebinding/beads_adapter.go:500-505.
func TestWorkerClaimSetsAssigneeAndReturnsTheBeadWhole(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "routed to me", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)

	body := `{"session_id":"gcg-session-1","assignee":"worker-local-3-pool","bead_id":"` + created.ID + `"}`
	req := newPostRequest(cityURL(state, "/worker/claim"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("POST /worker/claim is not routed at all (404); body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("claim status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Status string     `json:"status"`
		Bead   beads.Bead `json:"bead"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode claim output: %v (body: %s)", err, rec.Body.String())
	}
	if out.Status != "claimed" {
		t.Errorf("claim status field = %q, want %q", out.Status, "claimed")
	}
	if out.Bead.ID != created.ID {
		t.Errorf("claim output bead id = %q, want %q", out.Bead.ID, created.ID)
	}

	stored, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Assignee != "worker-local-3-pool" {
		t.Errorf("store assignee = %q, want the claiming identity", stored.Assignee)
	}
	if stored.Status != "in_progress" {
		t.Errorf("store status = %q, want in_progress — a claim that leaves the bead open is re-claimable by every other lane", stored.Status)
	}
}

// TestWorkerClaimIsSingleWinner pins the property the current wire surface
// cannot express: POST /bead/{id}/assign is a plain
// store.Update(id, UpdateOpts{Assignee:&assignee})
// (internal/api/huma_handlers_beads.go:806) — two workers both get 200 and the
// second silently owns the bead. The worker claim must refuse the loser.
func TestWorkerClaimIsSingleWinner(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "one winner", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)

	claim := func(assignee string) *httptest.ResponseRecorder {
		body := `{"session_id":"gcg-session-` + assignee + `","assignee":"` + assignee +
			`","bead_id":"` + created.ID + `"}`
		req := newPostRequest(cityURL(state, "/worker/claim"), strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := claim("worker-local-3-pool"); rec.Code != http.StatusOK {
		t.Fatalf("first claim status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	loser := claim("worker-local-4-pool")
	if loser.Code != http.StatusConflict {
		t.Fatalf("losing claim status = %d, want 409, body: %s", loser.Code, loser.Body.String())
	}
	stored, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Assignee != "worker-local-3-pool" {
		t.Fatalf("loser displaced the winner: assignee = %q", stored.Assignee)
	}
}

// TestWorkerClaimIsIdempotentForTheSameHolder: a retry after a dropped response
// must not be a conflict. This is the reason the claim is a two-argument CAS on
// the store rather than a read-then-write (the refusal to emulate is explicit at
// internal/storebinding/beads_adapter.go:487-491).
func TestWorkerClaimIsIdempotentForTheSameHolder(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "retry me", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)

	claim := func() *httptest.ResponseRecorder {
		body := `{"session_id":"gcg-session-1","assignee":"worker-local-3-pool","bead_id":"` + created.ID + `"}`
		req := newPostRequest(cityURL(state, "/worker/claim"), strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	first := claim()
	if first.Code != http.StatusOK {
		t.Fatalf("first claim status = %d, want 200, body: %s", first.Code, first.Body.String())
	}
	second := claim()
	if second.Code != http.StatusOK {
		t.Fatalf("repeated claim status = %d, want 200 (idempotent), body: %s", second.Code, second.Body.String())
	}
}

// TestWorkerClaimRefusesInsteadOfEmulatingWhenStoreLacksTheCapability: a store
// without the two-argument claim must produce a typed 501, never a
// read-then-write that looks like a claim. Emulation is exactly the loss of the
// single-winner guarantee, and the adapter upstream already refuses that way
// (unsupportedBeadsCapability, internal/storebinding/beads_adapter.go:502-504).
func TestWorkerClaimRefusesInsteadOfEmulatingWhenStoreLacksTheCapability(t *testing.T) {
	plainStore := beads.NewMemStore() // no Claim method — plain MemStore.
	created, err := plainStore.Create(beads.Bead{Title: "unsupported", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": plainStore}
	state.cityBeadStore = plainStore
	h := newTestCityHandler(t, state)

	body := `{"session_id":"gcg-session-1","assignee":"worker-local-3-pool","bead_id":"` + created.ID + `"}`
	req := newPostRequest(cityURL(state, "/worker/claim"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("claim on a capability-less store = %d, want 501, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := plainStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Assignee != "" || stored.Status != "open" {
		t.Fatalf("refused claim must write nothing; got assignee=%q status=%q", stored.Assignee, stored.Status)
	}
}

// TestWorkerClaimExposesAPreconditionToken is the prerequisite named in
// cr-gdeav.5.2 §4: a CAS whose token cannot be read is decoration.
// beads.Bead.Revision and ClaimFence are both tagged json:"-"
// (internal/beads/beads.go:174 and :186), so nothing on the wire carries them,
// and `rg If-Match docs/reference/schema/openapi.json` is empty. Whatever the
// PR chooses — an opaque ETag/X-GC-Revision plus If-Match, or an echoed
// expected-assignee pointer — this test pins that the CLAIM response is where a
// worker learns it, without needing a follow-up read.
func TestWorkerClaimExposesAPreconditionToken(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "token", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)

	body := `{"session_id":"gcg-session-1","assignee":"worker-local-3-pool","bead_id":"` + created.ID + `"}`
	req := newPostRequest(cityURL(state, "/worker/claim"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("claim status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	token := rec.Header().Get("ETag")
	if token == "" {
		token = rec.Header().Get("X-Gc-Revision")
	}
	if token == "" {
		t.Fatalf("claim response carries no precondition token; a remote worker cannot then issue any conditional write. headers: %v", rec.Header())
	}
}

// TestWorkerClaimLeaseKeysAreStampedOnTheBead names the two metadata keys the
// live reapers read — gc.claimed_at (internal/beadmeta/keys.go:61) and
// gc.lease_owner (internal/beadmeta/keys.go:177). Both ARE on the wire, so this
// is checkable over HTTP, and it is the difference between a claim a reaper can
// see and one it cannot (see worker_heartbeat_test.go for the lease half).
func TestWorkerClaimStampsWithAClaimInstantTheCityCanRead(t *testing.T) {
	rigStore := &claimingMemStoreDraft{MemStore: beads.NewMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "lease", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	h := newTestCityHandler(t, state)

	body := `{"session_id":"gcg-session-1","assignee":"worker-local-3-pool","bead_id":"` + created.ID + `"}`
	req := newPostRequest(cityURL(state, "/worker/claim"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	stored, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Metadata[beadmeta.ClaimedAtMetadataKey] == "" {
		t.Fatalf("claim wrote no %s; a leaseless claim is invisible to every live reaper", beadmeta.ClaimedAtMetadataKey)
	}
}
