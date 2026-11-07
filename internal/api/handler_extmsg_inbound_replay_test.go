package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin the inbound handler's answer to a transcript write that
// landed although the store reported it failed. Out-of-process adapters may
// drop a message on 5xx rather than retry it, and the member notification
// runs only after success, so a 5xx there leaves a stored message that wakes
// nobody. The handler replays the message once; the replay resolves the stored
// entry by provider_message_id, and the members are notified.

// isExtmsgTranscriptEntry reports whether b is a transcript entry (not the
// transcript-state bead, which Append also creates on first touch).
func isExtmsgTranscriptEntry(b beads.Bead) bool {
	return len(b.Labels) > 0 && b.Labels[0] == "gc:extmsg-transcript"
}

// transcriptFaultStore injects transcript-write faults over a real store.
// commitThenFail makes the first transcript-entry Create commit and then
// report a transport error. failBumps fails that many next_sequence writes.
// failCreates fails every transcript-entry Create without storing it.
type transcriptFaultStore struct {
	beads.Store
	mu             sync.Mutex
	commitThenFail bool
	failBumps      int
	failCreates    bool
	entryCreates   int
}

func (s *transcriptFaultStore) Create(b beads.Bead) (beads.Bead, error) {
	if !isExtmsgTranscriptEntry(b) {
		return s.Store.Create(b)
	}
	s.mu.Lock()
	s.entryCreates++
	failCreate := s.failCreates
	commitThenFail := s.commitThenFail
	s.commitThenFail = false
	s.mu.Unlock()
	if failCreate {
		return beads.Bead{}, errors.New("bd create: exit status 1: invalid connection")
	}
	created, err := s.Store.Create(b)
	if err != nil || !commitThenFail {
		return created, err
	}
	return beads.Bead{}, errors.New("bd create: exit status 1: invalid connection")
}

func (s *transcriptFaultStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs["next_sequence"]; ok {
		s.mu.Lock()
		fail := s.failBumps > 0
		if fail {
			s.failBumps--
		}
		s.mu.Unlock()
		if fail {
			return errors.New("bd update: exit status 1: i/o timeout")
		}
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *transcriptFaultStore) entryCreateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entryCreates
}

type inboundReplayFixture struct {
	fs          *fakeState
	srv         *Server
	store       *transcriptFaultStore
	services    *extmsg.Services
	ref         extmsg.ConversationRef
	sessionName string
}

// newInboundReplayFixture binds a running session to a conversation, with
// every extmsg write going through a transcriptFaultStore over the city store.
// The city store honors pinned bead IDs, as production bead stores do.
func newInboundReplayFixture(t *testing.T) *inboundReplayFixture {
	t.Helper()
	fs := newSessionFakeState(t)
	if mem, ok := fs.cityBeadStore.(*beads.MemStore); ok {
		mem.HonorExplicitIDs = true
	}
	srv := New(fs)
	t.Cleanup(srv.waitForBackground)
	store := &transcriptFaultStore{Store: fs.cityBeadStore}
	services := extmsg.NewServices(store)
	fs.extmsgSvc = &services
	fs.adapterReg = extmsg.NewAdapterRegistry()

	bound := createTestSession(t, fs.cityBeadStore, fs.sp, "Bound")
	ref := extmsg.ConversationRef{
		ScopeID:        "guild-1",
		Provider:       "discord",
		AccountID:      "acct-1",
		ConversationID: "thread-replay",
		Kind:           extmsg.ConversationThread,
	}
	if _, err := services.Bindings.Bind(context.Background(), extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, extmsg.BindInput{
		Conversation: ref,
		SessionID:    bound.ID,
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(bound.ID)
	if err != nil {
		t.Fatalf("Get(bound session): %v", err)
	}
	name := bead.Metadata["session_name"]
	if name == "" {
		t.Fatal("bound session missing session_name")
	}
	return &inboundReplayFixture{fs: fs, srv: srv, store: store, services: &services, ref: ref, sessionName: name}
}

func (f *inboundReplayFixture) postInbound(t *testing.T, providerMessageID, text string) (int, string) {
	t.Helper()
	message := map[string]any{
		"conversation": conversationBody(f.ref),
		"actor":        map[string]any{"id": "user-1", "display_name": "User One", "is_bot": false},
		"text":         text,
		"received_at":  time.Now().UTC().Format(time.RFC3339),
	}
	message["provider_message_id"] = providerMessageID
	rec := postExtMsg(t, f.fs, f.srv, "/extmsg/inbound", map[string]any{"message": message})
	f.srv.waitForBackground()
	return rec.Code, rec.Body.String()
}

// nudgesWith counts nudges to the bound session that carry text.
func (f *inboundReplayFixture) nudgesWith(text string) int {
	n := 0
	for _, call := range f.fs.sp.SnapshotCalls() {
		if call.Method == "Nudge" && call.Name == f.sessionName && strings.Contains(call.Message, text) {
			n++
		}
	}
	return n
}

// visibleEntries lists the transcript as a range reader sees it.
func (f *inboundReplayFixture) visibleEntries(t *testing.T) []extmsg.ConversationTranscriptRecord {
	t.Helper()
	entries, err := f.services.Transcript.List(context.Background(), extmsg.ListTranscriptInput{
		Caller:       extmsg.Caller{Kind: extmsg.CallerController, ID: "test"},
		Conversation: f.ref,
		Limit:        50,
	})
	if err != nil {
		t.Fatalf("Transcript.List: %v", err)
	}
	return entries
}

// requireOneVisibleEntry checks that providerMessageID is stored exactly once
// and that range readers see it.
func requireOneVisibleEntry(t *testing.T, store beads.Store, entries []extmsg.ConversationTranscriptRecord, providerMessageID string) {
	t.Helper()
	stored, err := store.List(beads.ListQuery{Label: "gc:extmsg-transcript"})
	if err != nil {
		t.Fatalf("list transcript entries: %v", err)
	}
	copies := 0
	for _, b := range stored {
		if b.Metadata["provider_message_id"] == providerMessageID {
			copies++
		}
	}
	if copies != 1 {
		t.Fatalf("stored copies of %s = %d, want 1", providerMessageID, copies)
	}
	for _, e := range entries {
		if e.ProviderMessageID == providerMessageID {
			return
		}
	}
	t.Fatalf("%s is stored but hidden from range readers; visible entries: %+v", providerMessageID, entries)
}

func TestHandleExtMsgInboundReplaysCommittedButFailedCreateAndNotifies(t *testing.T) {
	f := newInboundReplayFixture(t)
	f.store.commitThenFail = true

	code, body := f.postInbound(t, "msg-landed", "landed despite the error")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", code, http.StatusOK, body)
	}
	if got := f.nudgesWith("landed despite the error"); got != 1 {
		t.Fatalf("nudges to the bound session = %d, want 1; calls=%#v", got, f.fs.sp.SnapshotCalls())
	}
	requireOneVisibleEntry(t, f.fs.cityBeadStore, f.visibleEntries(t), "msg-landed")
}

func TestHandleExtMsgInboundReplaysFailedSequenceAdvanceAndNotifies(t *testing.T) {
	f := newInboundReplayFixture(t)
	f.store.failBumps = 1

	code, body := f.postInbound(t, "msg-unbumped", "stored before the advance failed")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", code, http.StatusOK, body)
	}
	if got := f.nudgesWith("stored before the advance failed"); got != 1 {
		t.Fatalf("nudges to the bound session = %d, want 1; calls=%#v", got, f.fs.sp.SnapshotCalls())
	}
	entries := f.visibleEntries(t)
	requireOneVisibleEntry(t, f.fs.cityBeadStore, entries, "msg-unbumped")

	// The next message takes the next sequence instead of reusing the
	// replayed entry's.
	code, body = f.postInbound(t, "msg-next", "the next message")
	if code != http.StatusOK {
		t.Fatalf("next status = %d, want %d; body: %s", code, http.StatusOK, body)
	}
	seqs := map[int64]string{}
	for _, e := range f.visibleEntries(t) {
		if prev, dup := seqs[e.Sequence]; dup {
			t.Fatalf("sequence %d holds both %s and %s", e.Sequence, prev, e.ProviderMessageID)
		}
		seqs[e.Sequence] = e.ProviderMessageID
	}
	if len(seqs) != 2 {
		t.Fatalf("visible sequences = %v, want two entries", seqs)
	}
}

func TestHandleExtMsgInboundUnstoredMessageStill500AfterReplay(t *testing.T) {
	f := newInboundReplayFixture(t)
	f.store.failCreates = true

	code, body := f.postInbound(t, "msg-lost", "never stored")
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", code, http.StatusInternalServerError, body)
	}
	if got := f.store.entryCreateCount(); got != 2 {
		t.Fatalf("transcript entry creates = %d, want 2 (the attempt and one replay)", got)
	}
	if got := f.nudgesWith("never stored"); got != 0 {
		t.Fatalf("nudges for an unstored message = %d, want 0", got)
	}
}

func TestHandleExtMsgInboundWithoutProviderMessageIDIsNotReplayed(t *testing.T) {
	f := newInboundReplayFixture(t)
	f.store.failCreates = true

	// The schema requires provider_message_id; a blank one still carries no
	// identity a replay could resolve by.
	code, body := f.postInbound(t, "   ", "no provider id")
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", code, http.StatusInternalServerError, body)
	}
	if got := f.store.entryCreateCount(); got != 1 {
		t.Fatalf("transcript entry creates = %d, want 1: without a provider message id a replay could store the message twice", got)
	}
}

// TestHandleExtMsgInboundReplayKeepsBoundSessionResolvable guards the fixture:
// the session the tests count nudges for is the one bound to the conversation.
func TestHandleExtMsgInboundReplayKeepsBoundSessionResolvable(t *testing.T) {
	f := newInboundReplayFixture(t)
	binding, err := f.services.Bindings.ResolveByConversation(context.Background(), f.ref)
	if err != nil || binding == nil {
		t.Fatalf("ResolveByConversation = %+v, %v; want the fixture binding", binding, err)
	}
	id, err := session.ResolveSessionID(f.fs.cityBeadStore, binding.SessionID)
	if err != nil || id != binding.SessionID {
		t.Fatalf("ResolveSessionID(%s) = %q, %v", binding.SessionID, id, err)
	}
}
