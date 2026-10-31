package beads

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// These tests pin Bead.CloseReason through the store adapters that read it
// and the caching store that announces it (gastownhall/gascity#2663): a client
// of the supervisor API sees why work finished on GET and on bead.closed,
// whether the close went through the cache or around it (bd close --reason).

const testCloseReason = "fixed in commit abc123; tests pass"

func TestNativeDoltStoreGetReadsCloseReason(t *testing.T) {
	t.Parallel()
	storage := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return []*beadslib.Issue{{
				ID:          "gc-closed",
				Title:       "closed with a reason",
				Status:      beadslib.StatusClosed,
				IssueType:   beadslib.TypeTask,
				Priority:    2,
				CloseReason: testCloseReason,
			}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Get("gc-closed")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CloseReason != testCloseReason {
		t.Fatalf("CloseReason = %q, want %q", got.CloseReason, testCloseReason)
	}
}

// A migration copy (gc storage migrate) creates the closed row through
// nativeIssueFromBead; the reason has to survive that copy.
func TestNativeIssueFromBeadCarriesCloseReason(t *testing.T) {
	t.Parallel()
	issue, err := nativeIssueFromBead(Bead{ID: "gc-1", Title: "copied", Status: "closed", CloseReason: testCloseReason})
	if err != nil {
		t.Fatalf("nativeIssueFromBead: %v", err)
	}
	if issue.CloseReason != testCloseReason {
		t.Fatalf("issue.CloseReason = %q, want %q", issue.CloseReason, testCloseReason)
	}
}

// closeReasonRecorder keeps the decoded bead of every bead.closed the cache
// announces.
type closeReasonRecorder struct {
	mu     sync.Mutex
	closed []Bead
}

func (r *closeReasonRecorder) onChange(t *testing.T) func(eventType, beadID string, payload json.RawMessage) {
	return func(eventType, beadID string, payload json.RawMessage) {
		if eventType != "bead.closed" {
			return
		}
		b, ok := DecodeBeadEventPayload(payload)
		if !ok {
			t.Errorf("decode bead.closed payload for %s: %s", beadID, payload)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.closed = append(r.closed, b)
	}
}

func (r *closeReasonRecorder) only(t *testing.T, id string) Bead {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var found []Bead
	for _, b := range r.closed {
		if b.ID == id {
			found = append(found, b)
		}
	}
	if len(found) != 1 {
		t.Fatalf("bead.closed events for %s = %d, want 1: %+v", id, len(found), r.closed)
	}
	return found[0]
}

func newPrimedCloseReasonCache(t *testing.T) (*MemStore, *CachingStore, *closeReasonRecorder, Bead) {
	t.Helper()
	mem := NewMemStore()
	seed, err := mem.Create(Bead{Title: "plain task", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := &closeReasonRecorder{}
	cs := NewCachingStoreForTest(mem, rec.onChange(t))
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return mem, cs, rec, seed
}

// A close through the cache (the API close route) carries the reason on the
// cached row and on its bead.closed.
func TestCachingStoreCloseCarriesCloseReason(t *testing.T) {
	t.Parallel()
	_, cs, rec, seed := newPrimedCloseReasonCache(t)

	if err := cs.SetMetadata(seed.ID, "close_reason", testCloseReason); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := cs.Close(seed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := cs.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CloseReason != testCloseReason {
		t.Fatalf("cached CloseReason = %q, want %q", got.CloseReason, testCloseReason)
	}
	if ev := rec.only(t, seed.ID); ev.CloseReason != testCloseReason {
		t.Fatalf("bead.closed CloseReason = %q, want %q", ev.CloseReason, testCloseReason)
	}
}

// A close made around the cache (bd close --reason) reaches Get and bead.closed
// once the reconcile pass sees it.
func TestExternalCloseReasonSeenByReconcile(t *testing.T) {
	t.Parallel()
	mem, cs, rec, seed := newPrimedCloseReasonCache(t)

	if err := mem.SetMetadata(seed.ID, "close_reason", testCloseReason); err != nil {
		t.Fatalf("external SetMetadata: %v", err)
	}
	if err := mem.Close(seed.ID); err != nil {
		t.Fatalf("external close: %v", err)
	}
	cs.runReconciliation()

	if ev := rec.only(t, seed.ID); ev.CloseReason != testCloseReason {
		t.Fatalf("bead.closed CloseReason = %q, want %q", ev.CloseReason, testCloseReason)
	}
	got, err := cs.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CloseReason != testCloseReason {
		t.Fatalf("Get CloseReason = %q, want %q", got.CloseReason, testCloseReason)
	}
}

// A bd hook event names the fields it carries; close_reason is one of them.
func TestMergeCacheEventPatchAppliesCloseReason(t *testing.T) {
	t.Parallel()
	base := Bead{ID: "gc-1", Title: "task", Status: "open"}
	payload := json.RawMessage(`{"id":"gc-1","status":"closed","close_reason":"` + testCloseReason + `"}`)
	patch, fields := decodeCloseReasonPatch(t, payload)

	merged := mergeCacheEventPatch(base, patch, fields)
	if merged.CloseReason != testCloseReason {
		t.Fatalf("merged CloseReason = %q, want %q", merged.CloseReason, testCloseReason)
	}
	if !cacheEventConflictsCurrent(base, patch, fields) {
		t.Fatal("a patch that sets close_reason must conflict with a cached row that lacks it")
	}
}

// bd omits an empty close_reason, so a reopen event that carries only the new
// status must still drop the old reason.
func TestMergeCacheEventPatchReopenClearsCloseReason(t *testing.T) {
	t.Parallel()
	base := Bead{ID: "gc-1", Title: "task", Status: "closed", CloseReason: testCloseReason}
	patch, fields := decodeCloseReasonPatch(t, json.RawMessage(`{"id":"gc-1","status":"open"}`))

	merged := mergeCacheEventPatch(base, patch, fields)
	if merged.CloseReason != "" {
		t.Fatalf("merged CloseReason after reopen = %q, want empty", merged.CloseReason)
	}
}

func TestBeadChangedSeesCloseReason(t *testing.T) {
	t.Parallel()
	old := Bead{ID: "gc-1", Status: "closed"}
	fresh := old
	fresh.CloseReason = testCloseReason
	if !beadChanged(old, fresh, false) {
		t.Fatal("beadChanged ignored a close_reason change")
	}
}

func TestSetBeadStatusClearsCloseReasonWhenNotClosed(t *testing.T) {
	t.Parallel()
	b := Bead{ID: "gc-1", Status: "closed", CloseReason: testCloseReason}
	setBeadStatus(&b, "open")
	if b.CloseReason != "" {
		t.Fatalf("CloseReason after setBeadStatus(open) = %q, want empty", b.CloseReason)
	}
}

func decodeCloseReasonPatch(t *testing.T, payload json.RawMessage) (Bead, map[string]json.RawMessage) {
	t.Helper()
	patch, ok := DecodeBeadEventPayload(payload)
	if !ok {
		t.Fatalf("DecodeBeadEventPayload(%s) failed", payload)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	return patch, fields
}
