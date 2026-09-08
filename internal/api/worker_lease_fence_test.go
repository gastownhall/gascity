package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The cr-gdeav.5.5 bounce was specific: every lease write in the worker family
// sat behind an unconditional Store.Update, so a stale claim stamp or a delayed
// heartbeat could land gc.lease_owner / gc.claimed_at on a bead a different seat
// already held, and nothing in the suite proved otherwise. These are the
// stale/race cases that pin the fence instead of the happy path.
//
// The device is the same in all of them: a store double that lets ANOTHER SEAT
// move the bead in the exact window the handler cannot protect — after its read,
// before its conditional write. A correct handler loses the write. The draft
// behavior would have stamped it, which is the lost update these tests keep
// fixed.

// racingWorkerStoreDraft wraps a real store, adds the capabilities the routes
// delegate to, and fires `race` exactly once — after the first Get the WRAPPER
// serves, i.e. after the handler has picked the revision it is about to CAS
// against. Delegation goes to the inner store directly, so the claim's own
// internal reads never arm the race.
type racingWorkerStoreDraft struct {
	// The Store interface is embedded so the double satisfies beads.Store in
	// full; `inner` is the same store, used by the delegated methods below so
	// their own reads never arm the race.
	beads.Store
	inner beads.Store
	mu    sync.Mutex
	race  func()
	armed bool
}

func (s *racingWorkerStoreDraft) Get(id string) (beads.Bead, error) {
	bead, err := s.inner.Get(id)
	if err != nil {
		return bead, err
	}
	s.mu.Lock()
	fire := s.armed && s.race != nil
	s.armed = false
	s.mu.Unlock()
	if fire {
		s.race()
	}
	return bead, nil
}

func (s *racingWorkerStoreDraft) Claim(id, assignee string) (beads.Bead, bool, error) {
	cur, err := s.inner.Get(id)
	if err != nil {
		return beads.Bead{}, false, err
	}
	if cur.Assignee != "" && cur.Assignee != assignee {
		return cur, false, nil
	}
	status := "in_progress"
	if err := s.inner.Update(id, beads.UpdateOpts{Assignee: &assignee, Status: &status}); err != nil {
		return beads.Bead{}, false, err
	}
	if cur.Assignee == assignee && cur.Status == status {
		return cur, false, nil
	}
	final, err := s.inner.Get(id)
	return final, err == nil, err
}

func (s *racingWorkerStoreDraft) ReleaseIfCurrent(id, assignee string) (bool, error) {
	releaser, ok := s.inner.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, beads.ErrConditionalWriteUnsupported
	}
	return releaser.ReleaseIfCurrent(id, assignee)
}

// The four ConditionalWriter legs delegate to the backing store's own seam, the
// same way CloseWithMetadataIfMatch below does: the double reports the
// capability it actually has, and a store with no seam reports that rather than
// the route inventing one.
func (s *racingWorkerStoreDraft) conditionalWriter() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.inner)
}

func (s *racingWorkerStoreDraft) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	writer, ok := s.conditionalWriter()
	if !ok {
		return beads.ErrConditionalWriteUnsupported
	}
	return writer.UpdateIfMatch(id, expectedRevision, opts)
}

func (s *racingWorkerStoreDraft) CloseIfMatch(id string, expectedRevision int64) error {
	writer, ok := s.conditionalWriter()
	if !ok {
		return beads.ErrConditionalWriteUnsupported
	}
	return writer.CloseIfMatch(id, expectedRevision)
}

func (s *racingWorkerStoreDraft) DeleteIfMatch(id string, expectedRevision int64) error {
	writer, ok := s.conditionalWriter()
	if !ok {
		return beads.ErrConditionalWriteUnsupported
	}
	return writer.DeleteIfMatch(id, expectedRevision)
}

func (s *racingWorkerStoreDraft) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	writer, ok := s.conditionalWriter()
	if !ok {
		return false, beads.ErrConditionalWriteUnsupported
	}
	return writer.CompareAndSetMetadataKey(id, key, expected, next)
}

func (s *racingWorkerStoreDraft) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (beads.Bead, error) {
	closer, ok := beads.AtomicConditionalCloserFor(s.inner)
	if !ok {
		return beads.Bead{}, beads.ErrConditionalWriteUnsupported
	}
	return closer.CloseWithMetadataIfMatch(id, expectedRevision, metadata)
}

// reclaimByOtherSeat is the moving bead: the holder loses the assignment and a
// different seat takes it, all in the window between the handler's read and its
// conditional write.
func reclaimByOtherSeat(t *testing.T, inner beads.Store, beadID, holder, other string) {
	t.Helper()
	released, err := inner.(beads.ConditionalAssignmentReleaser).ReleaseIfCurrent(beadID, holder)
	if err != nil || !released {
		t.Fatalf("setup race release = %v, %v", released, err)
	}
	// The new seat stamps its own lease, as a claim through /worker/claim would
	// (gc.claimed_at stays whatever the first claim wrote — it is write-once).
	status := "in_progress"
	if err := inner.Update(beadID, beads.UpdateOpts{
		Assignee: &other,
		Status:   &status,
		Metadata: map[string]string{beadmeta.LeaseOwnerMetadataKey: other},
	}); err != nil {
		t.Fatalf("setup race claim: %v", err)
	}
}

func heartbeatBody(beadID string) string {
	return `{"session_id":"gcg-session-1","assignee":"worker-local-3-pool","bead_id":"` + beadID + `"}`
}

func postWorker(t *testing.T, h http.Handler, state State, tail, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := newPostRequest(cityURL(state, tail), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestWorkerHeartbeatCannotStampOverANewerHolder is the regression the bounce
// named: the heartbeat's holder check passes on the row it read, then the bead
// moves. The fenced write must lose, and the NEW holder's lease keys must be
// exactly what they were — the whole point of fencing the lease stamp.
func TestWorkerHeartbeatCannotStampOverANewerHolder(t *testing.T) {
	const holder, other = "worker-local-3-pool", "worker-local-9-pool"
	inner := beads.NewAtomicCloseMemStore()
	created, err := inner.Create(beads.Bead{Title: "moving", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	// Arming the race only after the setup claim means it fires on the
	// heartbeat's own read, which is the window under test.
	raced := &racingWorkerStoreDraft{Store: inner, inner: inner}
	state.stores = map[string]beads.Store{"myrig": raced}
	state.cityBeadStore = raced
	h := newTestCityHandler(t, state)

	if rec := postWorker(t, h, state, "/worker/claim", heartbeatBody(created.ID)); rec.Code != http.StatusOK {
		t.Fatalf("setup claim = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	raced.race = func() { reclaimByOtherSeat(t, inner, created.ID, holder, other) }
	raced.armed = true

	rec := postWorker(t, h, state, "/worker/heartbeat", heartbeatBody(created.ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("heartbeat over a moved bead = %d, want 409, body: %s", rec.Code, rec.Body.String())
	}
	after, err := inner.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after refused heartbeat: %v", err)
	}
	if got := after.Metadata[beadmeta.LeaseOwnerMetadataKey]; got != other {
		t.Fatalf("%s = %q after a refused heartbeat, want the new holder %q — not stamping over a newer lease is the fence's whole purpose",
			beadmeta.LeaseOwnerMetadataKey, got, other)
	}
	if after.Assignee != other {
		t.Fatalf("the refused heartbeat moved ownership to %q", after.Assignee)
	}
}

// TestWorkerClaimCannotStampLeaseOverANewerHolder is the same race on the other
// entry point: the claim won, then the bead moved before the lease stamp landed.
func TestWorkerClaimCannotStampLeaseOverANewerHolder(t *testing.T) {
	const holder, other = "worker-local-3-pool", "worker-local-9-pool"
	state := newFakeState(t)
	inner := beads.NewAtomicCloseMemStore()
	created, err := inner.Create(beads.Bead{Title: "claim race", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	raced := &racingWorkerStoreDraft{Store: inner, inner: inner, armed: true, race: func() {
		// A different seat takes the bead after this claim's own write.
		status, newHolder := "in_progress", other
		if err := inner.Update(created.ID, beads.UpdateOpts{Assignee: &newHolder, Status: &status}); err != nil {
			t.Fatalf("race update: %v", err)
		}
	}}
	state.stores = map[string]beads.Store{"myrig": raced}
	state.cityBeadStore = raced
	h := newTestCityHandler(t, state)

	rec := postWorker(t, h, state, "/worker/claim", heartbeatBody(created.ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("claim over a moved bead = %d, want 409, body: %s", rec.Code, rec.Body.String())
	}
	after, err := inner.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after refused claim: %v", err)
	}
	if got := after.Metadata[beadmeta.LeaseOwnerMetadataKey]; got == holder {
		t.Fatalf("a losing claim stamped %s=%q over the seat that actually holds the bead", beadmeta.LeaseOwnerMetadataKey, got)
	}
}

// TestWorkerClaimRefusesWhenTheStoreCannotFenceTheLease pins the up-front
// capability gate. A store that can claim but cannot do a revision-fenced write
// must be told 501 BEFORE the claim — otherwise the bead is left assigned with
// no lease, which is the leaseless-claim state no reaper can see (the exact
// hazard gc.claimed_at / gc.lease_owner exist to remove).
func TestWorkerClaimRefusesWhenTheStoreCannotFenceTheLease(t *testing.T) {
	base := beads.NewMemStore()
	created, err := base.Create(beads.Bead{Title: "unfenceable", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": &unfenceableClaimStoreDraft{Store: base}}
	state.cityBeadStore = &unfenceableClaimStoreDraft{Store: base}
	h := newTestCityHandler(t, state)

	rec := postWorker(t, h, state, "/worker/claim", heartbeatBody(created.ID))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("claim on a store with no conditional write = %d, want 501, body: %s", rec.Code, rec.Body.String())
	}
	after, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after refusal: %v", err)
	}
	if after.Assignee != "" || after.Status != "open" {
		t.Fatalf("the refusal claimed anyway: assignee=%q status=%q — a 501 that also wrote is the partial failure this gate exists to prevent",
			after.Assignee, after.Status)
	}
}

// unfenceableClaimStoreDraft embeds the Store INTERFACE, so UpdateIfMatch is not
// promoted: the store honestly reports that it cannot fence a write, while still
// exposing the two-argument claim.
type unfenceableClaimStoreDraft struct {
	beads.Store
}

func (s *unfenceableClaimStoreDraft) Claim(id, assignee string) (beads.Bead, bool, error) {
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
		return cur, false, nil
	}
	final, err := s.Get(id)
	return final, err == nil, err
}

// TestWorkerCloseRequiresTheHolderThatClaimedBead closes the hole the bounce
// named: fencing only the revision lets any caller close any bead and attribute
// the work to whatever identity it typed.
func TestWorkerCloseRequiresTheHolderThatClaimedTheBead(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	created, store := newWorkerClaimedBead(t, state, h)

	rec := workerClose(t, h, state, `{"assignee":"worker-local-9-pool","bead_id":"`+created.ID+`","outcome":"no-op","reason":"not mine to close"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stranger close = %d, want 409, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Status != "in_progress" {
		t.Fatalf("a refused close moved status to %q", stored.Status)
	}
	if _, ok := stored.Metadata[beadmeta.WorkOutcomeMetadataKey]; ok {
		t.Fatalf("a refused close wrote %s — a stranger wrote the work record", beadmeta.WorkOutcomeMetadataKey)
	}
}

// TestWorkerCloseRequiresAHolder: a body with no holder is refused rather than
// closed under whatever identity the payload's record happens to name.
func TestWorkerCloseRequiresAHolder(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	created, store := newWorkerClaimedBead(t, state, h)

	rec := workerClose(t, h, state, `{"bead_id":"`+created.ID+`","outcome":"no-op","reason":"no holder named"}`)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("holderless close = %d, want 400/422, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Status != "in_progress" {
		t.Fatalf("a refused close moved status to %q", stored.Status)
	}
}

// TestWorkerCloseRefusesAnotherSessionOfTheSameName covers the session half of
// the ownership contract: a bead the claim stamped with a session cannot be
// closed by a different session wearing the same pool identity.
func TestWorkerCloseRefusesAnotherSessionOfTheSameName(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	created, store := newWorkerClaimedBead(t, state, h)

	if err := store.Update(created.ID, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.SessionIDMetadataKey: "gcg-session-1",
	}}); err != nil {
		t.Fatalf("setup session stamp: %v", err)
	}
	body := `{"assignee":"worker-local-3-pool","session_id":"gcg-session-2","bead_id":"` + created.ID + `","outcome":"no-op","reason":"wrong seat"}`
	rec := workerClose(t, h, state, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("other-session close = %d, want 409, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Status != "in_progress" {
		t.Fatalf("a refused close moved status to %q", stored.Status)
	}
}

// TestWorkerClosePersistsTheCloseReason settles the draft's open item: the
// reason is NOT a promise that evaporates. beads.Bead has no reason column, so
// the reason rides in the same atomic write as the close, under "close_reason" —
// the key gc already writes close reasons under (beadmail's retention sweep,
// cmd/gc/cmd_convoy.go:1758, internal/dispatch/runtime.go:348) and the one
// BdStore forwards to bd's own --reason (internal/beads/bdstore.go:2415,2439).
func TestWorkerClosePersistsTheCloseReason(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	created, store := newWorkerClaimedBead(t, state, h)

	const reason = "already shipped on main; dedupe of cr-other.1"
	rec := workerClose(t, h, state, `{"assignee":"worker-local-3-pool","bead_id":"`+created.ID+`","outcome":"no-op","reason":"`+reason+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Status != "closed" {
		t.Fatalf("close did not persist status: %q", stored.Status)
	}
	if got := stored.Metadata["close_reason"]; got != reason {
		t.Fatalf("close_reason = %q, want %q — a typed close whose disposition text is not on the bead is still uncitable", got, reason)
	}
}

// TestWorkerCloseIsIdempotentForTheHolder: a worker whose close response was
// lost must be able to retry. The second call reports the first rather than a
// conflict, and must not mint a second revision or rewrite the record.
func TestWorkerCloseIsIdempotentForTheHolder(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	created, store := newWorkerClaimedBead(t, state, h)
	body := `{"assignee":"worker-local-3-pool","bead_id":"` + created.ID + `","outcome":"no-op","reason":"nothing to ship"}`

	if rec := workerClose(t, h, state, body); rec.Code != http.StatusOK {
		t.Fatalf("first close = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	first, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	rec := workerClose(t, h, state, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry close = %d, want 200 (a lost response must be retryable), body: %s", rec.Code, rec.Body.String())
	}
	var decoded struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode retry body %q: %v", rec.Body.String(), err)
	}
	if decoded.Status != "already_closed" {
		t.Fatalf("retry status = %q, want already_closed", decoded.Status)
	}
	again, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get after retry: %v", err)
	}
	if again.Revision != first.Revision {
		t.Fatalf("a retried close minted a new revision %d -> %d", first.Revision, again.Revision)
	}
}

// TestWorkerCloseCannotCloseOverAMovedBead is the close's own race: the bead
// moves between the ownership read and the terminal write, so the revision
// fence — not the check — is what stops the stranger.
func TestWorkerCloseCannotCloseOverAMovedBead(t *testing.T) {
	const holder, other = "worker-local-3-pool", "worker-local-9-pool"
	state := newFakeState(t)
	inner := beads.NewAtomicCloseMemStore()
	created, err := inner.Create(beads.Bead{Title: "close race", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status, holderName := "in_progress", holder
	if err := inner.Update(created.ID, beads.UpdateOpts{Assignee: &holderName, Status: &status}); err != nil {
		t.Fatalf("setup Update: %v", err)
	}
	raced := &racingWorkerStoreDraft{Store: inner, inner: inner, armed: true, race: func() {
		otherStatus, newHolder := "in_progress", other
		if err := inner.Update(created.ID, beads.UpdateOpts{Assignee: &newHolder, Status: &otherStatus}); err != nil {
			t.Fatalf("race update: %v", err)
		}
	}}
	state.stores = map[string]beads.Store{"myrig": raced}
	state.cityBeadStore = raced
	h := newTestCityHandler(t, state)

	rec := workerClose(t, h, state, `{"assignee":"`+holder+`","bead_id":"`+created.ID+`","outcome":"no-op","reason":"the bead moved"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("close over a moved bead = %d, want 409, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := inner.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after refused close: %v", err)
	}
	if stored.Status != "in_progress" {
		t.Fatalf("a close that lost its fence wrote status %q", stored.Status)
	}
	if stored.Assignee != other {
		t.Fatalf("the loser closed the new holder's bead: assignee=%q", stored.Assignee)
	}
}
