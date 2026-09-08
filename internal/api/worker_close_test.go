// Close-route acceptance for the typed ADR-0009 work record. The tests assert
// that the server validates ownership and commits metadata plus closure through
// AtomicConditionalCloser as one operation.
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

// atomicCloserHiddenStoreDraft embeds the Store INTERFACE, so neither
// CloseIfMatch nor CloseWithMetadataIfMatch is promoted: the handler sees a
// backend that cannot prove the two writes share one transaction.
type atomicCloserHiddenStoreDraft struct {
	beads.Store
}

func workerClose(t *testing.T, h http.Handler, state State, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := newPostRequest(cityURL(state, "/worker/close"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// workerAtomicStoreDraft is the store the close acceptance needs, and the one
// correction cr-gdeav.5.4 had to make to cr-gdeav.5.3's scaffolds: the drafts
// built every close case on plain MemStore, and plain MemStore CANNOT satisfy
// the acceptance these files themselves pin — "Plain MemStore intentionally
// does not expose this narrower capability"
// (internal/beads/atomic_close_memstore.go:8-10), so a correct handler answers
// 501 against it and the happy paths could only have failed for the wrong
// reason. The fixture is what changed, not a single assertion: this wraps the
// real upstream atomic closer (beads.NewAtomicCloseMemStore) and DELEGATES the
// terminal write to it, exactly as CachingStore exposes it through
// cachingAtomicConditionalCloser rather than re-implementing it
// (internal/beads/caching_store_conditional.go:42-47).
type workerAtomicStoreDraft struct {
	beads.Store
}

// Claim mirrors the store contract the claim route delegates to (the same
// single-winner, holder-idempotent shape as claimingMemStoreDraft).
func (s *workerAtomicStoreDraft) Claim(id, assignee string) (beads.Bead, bool, error) {
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

// The ConditionalWriter legs forward to the backing store's own seam, which is
// what lets a claim be stamped under a revision fence on this fixture too:
// workerAtomicStoreDraft embeds the Store INTERFACE, so the optional capability
// is not promoted through it and has to be stated (the same rule
// beads.ConditionalWriterFor documents for the typed class wrappers).
func (s *workerAtomicStoreDraft) conditionalWriter() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.Store)
}

func (s *workerAtomicStoreDraft) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	writer, ok := s.conditionalWriter()
	if !ok {
		return beads.ErrConditionalWriteUnsupported
	}
	return writer.UpdateIfMatch(id, expectedRevision, opts)
}

func (s *workerAtomicStoreDraft) CloseIfMatch(id string, expectedRevision int64) error {
	writer, ok := s.conditionalWriter()
	if !ok {
		return beads.ErrConditionalWriteUnsupported
	}
	return writer.CloseIfMatch(id, expectedRevision)
}

func (s *workerAtomicStoreDraft) DeleteIfMatch(id string, expectedRevision int64) error {
	writer, ok := s.conditionalWriter()
	if !ok {
		return beads.ErrConditionalWriteUnsupported
	}
	return writer.DeleteIfMatch(id, expectedRevision)
}

func (s *workerAtomicStoreDraft) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	writer, ok := s.conditionalWriter()
	if !ok {
		return false, beads.ErrConditionalWriteUnsupported
	}
	return writer.CompareAndSetMetadataKey(id, key, expected, next)
}

// CloseWithMetadataIfMatch forwards to the backing store's own atomic closer and
// refuses if it has none — the double reports the capability it actually has.
func (s *workerAtomicStoreDraft) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (beads.Bead, error) {
	closer, ok := beads.AtomicConditionalCloserFor(s.Store)
	if !ok {
		return beads.Bead{}, beads.ErrConditionalWriteUnsupported
	}
	return closer.CloseWithMetadataIfMatch(id, expectedRevision, metadata)
}

// newWorkerClaimedBead puts a bead into the state a real close starts from:
// created, claimed by this identity, in_progress.
func newWorkerClaimedBead(t *testing.T, state *fakeState, h http.Handler) (beads.Bead, beads.Store) {
	t.Helper()
	rigStore := &workerAtomicStoreDraft{Store: beads.NewAtomicCloseMemStore()}
	created, err := rigStore.Create(beads.Bead{Title: "close me properly", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cityBeadStore = rigStore
	workerClaim(t, h, state, created.ID)
	return created, rigStore
}

// TestWorkerCloseCarriesTheTypedWorkRecord is the acceptance line: a remote
// close lands gc.work_outcome + gc.work_commit + gc.work_branch and the status
// flip in ONE write, so no observer ever sees a closed bead without its record.
func TestWorkerCloseCarriesTheTypedWorkRecord(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	created, rigStore := newWorkerClaimedBead(t, state, h)

	rec := workerClose(t, h, state, `{"assignee":"worker-local-3-pool","bead_id":"`+created.ID+`","outcome":"shipped","commit":"0123456789abcdef0123456789abcdef01234567","branch":"main","reason":"landed the fix"}`)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("POST /worker/close is not routed at all (404); body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	stored, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Status != "closed" {
		t.Fatalf("status = %q, want closed", stored.Status)
	}
	if got := stored.Metadata[beadmeta.WorkOutcomeMetadataKey]; got != beadmeta.WorkOutcomeShipped {
		t.Errorf("%s = %q, want %q", beadmeta.WorkOutcomeMetadataKey, got, beadmeta.WorkOutcomeShipped)
	}
	if got := stored.Metadata[beadmeta.WorkCommitMetadataKey]; got != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("%s = %q, want the stamped commit", beadmeta.WorkCommitMetadataKey, got)
	}
	if got := stored.Metadata[beadmeta.WorkBranchMetadataKey]; got != "main" {
		t.Errorf("%s = %q, want the stamped branch", beadmeta.WorkBranchMetadataKey, got)
	}
}

// TestWorkerCloseRejectsAnUntypedClose: the verb refuses rather than persisting
// the bare status flip that the current POST /bead/{id}/close is content to
// write. The bead must still be open and still be held, so the caller can retry
// with a disposition.
func TestWorkerCloseRejectsAnUntypedClose(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	created, rigStore := newWorkerClaimedBead(t, state, h)

	rec := workerClose(t, h, state, `{"assignee":"worker-local-3-pool","bead_id":"`+created.ID+`","reason":"drained"}`)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("untyped close = %d, want 400/422, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Status != "in_progress" {
		t.Fatalf("a refused close moved status to %q; the caller must still own a workable bead", stored.Status)
	}
}

// TestWorkerCloseRejectsAnOutcomeOutsideTheEnum: the vocabulary is
// shipped|no-op|blocked|abandoned (owned in cmd/gc/work_record_gate.go's
// validWorkOutcome). The bead that cannot be closed by anyone until it is
// retyped is the failure this rejects — writing staged/partial/skipped to
// gc.work_outcome strands the record.
func TestWorkerCloseRejectsAnOutcomeOutsideTheEnum(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	created, rigStore := newWorkerClaimedBead(t, state, h)

	rec := workerClose(t, h, state, `{"assignee":"worker-local-3-pool","bead_id":"`+created.ID+`","outcome":"staged","reason":"half done"}`)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("outcome=staged close = %d, want 400/422, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Status != "in_progress" || stored.Metadata[beadmeta.WorkOutcomeMetadataKey] == "staged" {
		t.Fatalf("an invalid enum value was persisted: status=%q %s=%q", stored.Status, beadmeta.WorkOutcomeMetadataKey, stored.Metadata[beadmeta.WorkOutcomeMetadataKey])
	}
}

// TestWorkerCloseShippedRequiresCommitAndBranch mirrors the client gate's second
// rule server-side: "shipped" must point at an artifact. Presence is what the
// HTTP layer can check; REACHABILITY of the commit on the branch is a git
// question, and the PR must state in its doc where that check lives (the local
// gate does it in cmd/gc/work_record_gate.go) — see README open question 3.
func TestWorkerCloseShippedRequiresCommitAndBranch(t *testing.T) {
	for _, body := range []string{
		`{"assignee":"worker-local-3-pool","bead_id":"BEAD","outcome":"shipped","branch":"main","reason":"shipped it"}`,
		`{"assignee":"worker-local-3-pool","bead_id":"BEAD","outcome":"shipped","commit":"0123456789abcdef0123456789abcdef01234567","reason":"shipped it"}`,
		`{"assignee":"worker-local-3-pool","bead_id":"BEAD","outcome":"shipped","reason":"shipped it"}`,
	} {
		state := newFakeState(t)
		h := newTestCityHandler(t, state)
		created, rigStore := newWorkerClaimedBead(t, state, h)

		rec := workerClose(t, h, state, strings.Replace(body, "BEAD", created.ID, 1))
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("shipped without commit+branch (%s) = %d, want 400/422, body: %s", body, rec.Code, rec.Body.String())
		}
		stored, err := rigStore.Get(created.ID)
		if err != nil {
			t.Fatalf("store Get: %v", err)
		}
		if stored.Status != "in_progress" {
			t.Fatalf("an unsatisfiable shipped close persisted status %q for body %s", stored.Status, body)
		}
	}
}

// TestWorkerCloseAcceptsANoOpWithJustAReason: a disposition that ships nothing
// is legal and must not be pushed into a fake sha. This is the case the city's
// own close-out helper exists for (assets/bin/close-out.sh), and the reason the
// gate is not "always require a commit".
func TestWorkerCloseAcceptsANoOpWithJustAReason(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	created, rigStore := newWorkerClaimedBead(t, state, h)

	rec := workerClose(t, h, state, `{"assignee":"worker-local-3-pool","bead_id":"`+created.ID+`","outcome":"no-op","reason":"already shipped on main; dedupe"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op close = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Status != "closed" || stored.Metadata[beadmeta.WorkOutcomeMetadataKey] != beadmeta.WorkOutcomeNoOp {
		t.Fatalf("no-op close landed status=%q %s=%q", stored.Status, beadmeta.WorkOutcomeMetadataKey, stored.Metadata[beadmeta.WorkOutcomeMetadataKey])
	}
	if _, ok := stored.Metadata[beadmeta.WorkCommitMetadataKey]; ok {
		t.Fatalf("a no-op close must not invent %s", beadmeta.WorkCommitMetadataKey)
	}
}

// TestWorkerCloseReturnsTheRecordItWrote is the "verification" half of the
// acceptance, stated so it is checkable: the close response carries the typed
// record it persisted, so the caller can confirm what landed instead of issuing
// a second read that could observe a different bead on a dual-resident store
// (the #6015 class of problem cr-gdeav.5.2 §4 carries into this design).
func TestWorkerCloseReturnsTheRecordItWrote(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	created, _ := newWorkerClaimedBead(t, state, h)

	rec := workerClose(t, h, state, `{"assignee":"worker-local-3-pool","bead_id":"`+created.ID+`","outcome":"no-op","reason":"nothing to ship"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("close = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Status string     `json:"status"`
		Bead   beads.Bead `json:"bead"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode close output: %v (body: %s)", err, rec.Body.String())
	}
	if out.Bead.Status != "closed" {
		t.Errorf("close output bead status = %q, want closed", out.Bead.Status)
	}
	if got := out.Bead.Metadata[beadmeta.WorkOutcomeMetadataKey]; got != beadmeta.WorkOutcomeNoOp {
		t.Errorf("close output did not echo the record: %s = %q", beadmeta.WorkOutcomeMetadataKey, got)
	}
}

// TestWorkerCloseRefusesWhenTheStoreCannotProveAtomicity is the split-write
// guard. Without AtomicConditionalCloser the handler has two options: refuse,
// or write metadata and then close as two calls. The second is the bug — it is
// how a bead ends up closed with no work record — so the route must report the
// capability as unavailable.
func TestWorkerCloseRefusesWhenTheStoreCannotProveAtomicity(t *testing.T) {
	base := beads.NewMemStore()
	created, err := base.Create(beads.Bead{Title: "no atomic closer", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	assignee := "worker-local-3-pool"
	if err := base.Update(created.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": atomicCloserHiddenStoreDraft{Store: base}}
	h := newTestCityHandler(t, state)

	rec := workerClose(t, h, state, `{"assignee":"worker-local-3-pool","bead_id":"`+created.ID+`","outcome":"no-op","reason":"prove atomicity first"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("close on a store without an atomic conditional closer = %d, want 501, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.Status != "in_progress" {
		t.Fatalf("refused close wrote status %q — the split write this test exists to prevent", stored.Status)
	}
	if _, ok := stored.Metadata[beadmeta.WorkOutcomeMetadataKey]; ok {
		t.Fatalf("refused close wrote %s without closing — metadata landed, close did not", beadmeta.WorkOutcomeMetadataKey)
	}
}
