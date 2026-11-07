package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

// inboundReplayFaultStore wraps the extmsg mail store with switchable faults
// on transcript entry creates and on reads of the entries it created.
type inboundReplayFaultStore struct {
	beads.Store

	mu sync.Mutex
	// commitThenFailCreates is how many more entry creates commit and then
	// report an error, as an ambiguous write does.
	commitThenFailCreates int
	// failCreates makes every entry create fail without committing.
	failCreates bool
	// failGets is how many more Gets of an entry this store created fail.
	failGets     int
	entryCreates int
	createdIDs   map[string]bool
}

func (f *inboundReplayFaultStore) Create(b beads.Bead) (beads.Bead, error) {
	if !slices.Contains(b.Labels, "gc:extmsg-transcript") {
		return f.Store.Create(b)
	}
	f.mu.Lock()
	f.entryCreates++
	failCreate := f.failCreates
	commitThenFail := f.commitThenFailCreates > 0
	if commitThenFail {
		f.commitThenFailCreates--
	}
	f.mu.Unlock()
	if failCreate {
		return beads.Bead{}, errors.New("i/o timeout")
	}
	created, err := f.Store.Create(b)
	if err != nil {
		return created, err
	}
	f.mu.Lock()
	f.createdIDs[created.ID] = true
	f.mu.Unlock()
	if commitThenFail {
		return beads.Bead{}, errors.New("invalid connection")
	}
	return created, nil
}

func (f *inboundReplayFaultStore) Get(id string) (beads.Bead, error) {
	f.mu.Lock()
	fail := f.failGets > 0 && f.createdIDs[id]
	if fail {
		f.failGets--
	}
	f.mu.Unlock()
	if fail {
		return beads.Bead{}, errors.New("i/o timeout")
	}
	return f.Store.Get(id)
}

// newInboundReplayFixture binds a running session to a conversation whose
// transcript lives in a pin-honoring store behind fault.
func newInboundReplayFixture(t *testing.T, fault *inboundReplayFaultStore) (*fakeState, *Server, session.Info, extmsg.ConversationRef) {
	t.Helper()
	fs := newSessionFakeState(t)
	srv := New(fs)
	t.Cleanup(srv.waitForBackground)
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	fault.Store = mem
	fault.createdIDs = map[string]bool{}
	services := extmsg.NewServices(beadstest.Mail(fault), beadstest.Sessions(fs.cityBeadStore))
	fs.extmsgSvc = &services
	fs.adapterReg = extmsg.NewAdapterRegistry()
	ref := extmsg.ConversationRef{
		ScopeID:        "scope-1",
		Provider:       "telegram",
		AccountID:      "acct-1",
		ConversationID: "chat-1",
		Kind:           extmsg.ConversationDM,
	}
	worker := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker")
	if _, err := services.Bindings.Bind(context.Background(), extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, extmsg.BindInput{
		Conversation: ref,
		SessionID:    worker.ID,
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return fs, srv, worker, ref
}

func postInboundReplayMessage(t *testing.T, fs *fakeState, srv *Server, ref extmsg.ConversationRef, providerMessageID string) *httptest.ResponseRecorder {
	t.Helper()
	return postExtMsg(t, fs, srv, "/extmsg/inbound", map[string]any{
		"message": map[string]any{
			"provider_message_id": providerMessageID,
			"conversation":        conversationBody(ref),
			"actor":               map[string]any{"id": "u1", "display_name": "User", "is_bot": false},
			"text":                "hello",
			"received_at":         time.Now().UTC().Format(time.RFC3339),
		},
	})
}

func countNudges(fs *fakeState, sessionName string) int {
	n := 0
	for _, call := range fs.sp.SnapshotCalls() {
		if call.Method == "Nudge" && call.Name == sessionName {
			n++
		}
	}
	return n
}

func storedTranscriptEntries(t *testing.T, store beads.Store) int {
	t.Helper()
	entries, err := store.List(beads.ListQuery{Label: "gc:extmsg-transcript"})
	if err != nil {
		t.Fatalf("List(entries): %v", err)
	}
	return len(entries)
}

// TestHandleExtMsgInboundReplaysStoredButFailedAppend: the entry create
// commits but errors, and the read-back of its pinned ID fails, so the first
// HandleInboundNormalized fails. The handler's single replay finds the stored
// entry by provider message ID and succeeds: the request answers 200 with one
// stored message and notifies the bound session once.
func TestHandleExtMsgInboundReplaysStoredButFailedAppend(t *testing.T) {
	fault := &inboundReplayFaultStore{commitThenFailCreates: 1, failGets: 1}
	fs, srv, worker, ref := newInboundReplayFixture(t, fault)

	rec := postInboundReplayMessage(t, fs, srv, ref, "m1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	srv.waitForBackground()
	if got := storedTranscriptEntries(t, fault.Store); got != 1 {
		t.Fatalf("stored entries = %d, want 1", got)
	}
	if got := countNudges(fs, worker.SessionName); got != 1 {
		t.Fatalf("nudges to bound session = %d, want 1; calls=%#v", got, fs.sp.SnapshotCalls())
	}
}
