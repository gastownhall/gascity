package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

// transcriptRecoveryFaultStore wraps the mail store and injects faults into
// transcript writes: entry creates that commit and then report a transport
// error, failed reads of a transcript entry, and next_sequence writes that
// fail unwritten. It forwards pinned create and the conditional writes
// (metadata compare-and-set included), so transcripts allocate atomically.
type transcriptRecoveryFaultStore struct {
	beads.Store

	mu                     sync.Mutex
	commitThenFailCreates  int
	failEntryGets          int
	failNextSequenceWrites int
}

func (f *transcriptRecoveryFaultStore) Create(b beads.Bead) (beads.Bead, error) {
	return f.createEntry(b, f.Store.Create)
}

func (f *transcriptRecoveryFaultStore) CreatePinned(b beads.Bead) (beads.Bead, error) {
	pinned, ok := f.Store.(interface {
		CreatePinned(beads.Bead) (beads.Bead, error)
	})
	if !ok {
		return beads.Bead{}, fmt.Errorf("wrapped store %T has no pinned create", f.Store)
	}
	return f.createEntry(b, pinned.CreatePinned)
}

func (f *transcriptRecoveryFaultStore) createEntry(b beads.Bead, create func(beads.Bead) (beads.Bead, error)) (beads.Bead, error) {
	created, err := create(b)
	if err != nil || !slices.Contains(b.Labels, "gc:extmsg-transcript") {
		return created, err
	}
	if f.take(&f.commitThenFailCreates) {
		return beads.Bead{}, errors.New("invalid connection")
	}
	return created, nil
}

func (f *transcriptRecoveryFaultStore) Get(id string) (beads.Bead, error) {
	b, err := f.Store.Get(id)
	if err == nil && slices.Contains(b.Labels, "gc:extmsg-transcript") && f.take(&f.failEntryGets) {
		return beads.Bead{}, errors.New("read timed out")
	}
	return b, err
}

func (f *transcriptRecoveryFaultStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if key == "next_sequence" && f.take(&f.failNextSequenceWrites) {
		return false, errors.New("next_sequence write failed")
	}
	cas, ok := beads.As[beads.MetadataCASWriter](f.Store)
	if !ok {
		return false, beads.ErrConditionalWriteUnsupported
	}
	return cas.CompareAndSetMetadataKey(id, key, expected, next)
}

func (f *transcriptRecoveryFaultStore) conditionalWriter() (beads.ConditionalWriter, error) {
	writer, ok := beads.ConditionalWriterFor(f.Store)
	if !ok {
		return nil, beads.ErrConditionalWriteUnsupported
	}
	return writer, nil
}

func (f *transcriptRecoveryFaultStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	writer, err := f.conditionalWriter()
	if err != nil {
		return err
	}
	return writer.UpdateIfMatch(id, expectedRevision, opts)
}

func (f *transcriptRecoveryFaultStore) CloseIfMatch(id string, expectedRevision int64) error {
	writer, err := f.conditionalWriter()
	if err != nil {
		return err
	}
	return writer.CloseIfMatch(id, expectedRevision)
}

func (f *transcriptRecoveryFaultStore) DeleteIfMatch(id string, expectedRevision int64) error {
	writer, err := f.conditionalWriter()
	if err != nil {
		return err
	}
	return writer.DeleteIfMatch(id, expectedRevision)
}

func (f *transcriptRecoveryFaultStore) SetMetadata(id, key, value string) error {
	return f.SetMetadataBatch(id, map[string]string{key: value})
}

func (f *transcriptRecoveryFaultStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, bumps := kvs["next_sequence"]; bumps && f.take(&f.failNextSequenceWrites) {
		return errors.New("next_sequence write failed")
	}
	return f.Store.SetMetadataBatch(id, kvs)
}

func (f *transcriptRecoveryFaultStore) take(counter *int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if *counter > 0 {
		*counter--
		return true
	}
	return false
}

func (f *transcriptRecoveryFaultStore) set(apply func(f *transcriptRecoveryFaultStore)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apply(f)
}

// transcriptRecoveryHarness is a server whose extmsg mail store is a fault
// store, with a conversation bound to a session and a peer member that the
// inbound notification reaches.
type transcriptRecoveryHarness struct {
	fs       *fakeState
	srv      *Server
	fault    *transcriptRecoveryFaultStore
	services extmsg.Services
	ref      extmsg.ConversationRef
}

func newTranscriptRecoveryHarness(t *testing.T) *transcriptRecoveryHarness {
	t.Helper()
	fs := newSessionFakeState(t)
	srv := New(fs)
	t.Cleanup(srv.waitForBackground)
	fault := &transcriptRecoveryFaultStore{Store: fs.cityBeadStore}
	services := extmsg.NewServices(beadstest.Mail(fault), beadstest.Sessions(fs.cityBeadStore))
	fs.extmsgSvc = &services
	fs.adapterReg = extmsg.NewAdapterRegistry()

	source := createTestSession(t, fs.cityBeadStore, fs.sp, "Bound")
	ref := extmsg.ConversationRef{
		ScopeID:        "guild-1",
		Provider:       "discord",
		AccountID:      "acct-1",
		ConversationID: "thread-1",
		Kind:           extmsg.ConversationThread,
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	now := time.Now().UTC()
	if _, err := services.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		SessionID:    source.ID,
		Now:          now,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if _, err := services.Transcript.EnsureMembership(context.Background(), extmsg.EnsureMembershipInput{
		Caller:         caller,
		Conversation:   ref,
		SessionID:      "myrig/worker",
		BackfillPolicy: extmsg.MembershipBackfillSinceJoin,
		Owner:          extmsg.MembershipOwnerManual,
		Now:            now,
	}); err != nil {
		t.Fatalf("EnsureMembership(peer): %v", err)
	}
	return &transcriptRecoveryHarness{fs: fs, srv: srv, fault: fault, services: services, ref: ref}
}

func (h *transcriptRecoveryHarness) postInbound(t *testing.T, providerMessageID, text string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"provider_message_id": providerMessageID,
			"conversation": map[string]any{
				"scope_id":        h.ref.ScopeID,
				"provider":        h.ref.Provider,
				"account_id":      h.ref.AccountID,
				"conversation_id": h.ref.ConversationID,
				"kind":            h.ref.Kind,
			},
			"actor":       map[string]any{"id": "u1", "display_name": "User", "is_bot": false},
			"text":        text,
			"received_at": time.Now().UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("Marshal(body): %v", err)
	}
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, newPostRequest(cityURL(h.fs, "/extmsg/inbound"), strings.NewReader(string(body))))
	h.srv.waitForBackground()
	return rec
}

// notifications counts, per session, the nudges carrying text.
func (h *transcriptRecoveryHarness) notifications(text string) map[string]int {
	out := map[string]int{}
	for _, call := range h.fs.sp.SnapshotCalls() {
		if call.Method == "Nudge" && strings.Contains(call.Message, text) {
			out[call.Name]++
		}
	}
	return out
}

func (h *transcriptRecoveryHarness) peerSessionName(t *testing.T) string {
	t.Helper()
	peerID, err := session.ResolveSessionID(beadstest.Sessions(h.fs.cityBeadStore), "myrig/worker")
	if err != nil {
		t.Fatalf("ResolveSessionID(myrig/worker): %v", err)
	}
	peer, err := h.fs.cityBeadStore.Get(peerID)
	if err != nil {
		t.Fatalf("Get(peer): %v", err)
	}
	return peer.Metadata["session_name"]
}

func (h *transcriptRecoveryHarness) inboundEvents(t *testing.T) int {
	t.Helper()
	evts, err := h.fs.eventProv.List(events.Filter{Type: events.ExtMsgInbound})
	if err != nil {
		t.Fatalf("List(events): %v", err)
	}
	return len(evts)
}

func (h *transcriptRecoveryHarness) transcript(t *testing.T) []extmsg.ConversationTranscriptRecord {
	t.Helper()
	got, err := h.services.Transcript.List(context.Background(), extmsg.ListTranscriptInput{
		Caller:       extmsg.Caller{Kind: extmsg.CallerController, ID: "test"},
		Conversation: h.ref,
		Limit:        100,
	})
	if err != nil {
		t.Fatalf("List(transcript): %v", err)
	}
	return got
}

func (h *transcriptRecoveryHarness) storedEntries(t *testing.T) int {
	t.Helper()
	items, err := h.fs.cityBeadStore.List(beads.ListQuery{Label: "gc:extmsg-transcript", IncludeClosed: true})
	if err != nil {
		t.Fatalf("List(transcript entries): %v", err)
	}
	return len(items)
}

// TestExtMsgInboundTranscriptRecoveryRecoverableFault posts a normalized
// inbound whose transcript entry create commits and reports an error, and
// whose read-back of the slot fails once: the request recovers the entry, so
// it answers 2xx with one stored entry, one inbound event and one
// notification per member.
func TestExtMsgInboundTranscriptRecoveryRecoverableFault(t *testing.T) {
	h := newTranscriptRecoveryHarness(t)
	h.fault.set(func(f *transcriptRecoveryFaultStore) {
		f.commitThenFailCreates = 1
		f.failEntryGets = 1
	})

	rec := h.postInbound(t, "m1", "hello recovered")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if n := h.storedEntries(t); n != 1 {
		t.Fatalf("stored %d transcript entries, want 1", n)
	}
	if got := h.transcript(t); len(got) != 1 || got[0].ProviderMessageID != "m1" || got[0].Sequence != 1 {
		t.Fatalf("transcript = %#v, want m1 at sequence 1", got)
	}
	if n := h.inboundEvents(t); n != 1 {
		t.Fatalf("ExtMsgInbound events = %d, want 1", n)
	}
	notified := h.notifications("hello recovered")
	if notified[h.peerSessionName(t)] != 1 {
		t.Fatalf("peer notifications = %v, want one for %q", notified, h.peerSessionName(t))
	}
	for name, n := range notified {
		if n != 1 {
			t.Fatalf("session %q notified %d times, want once; all %v", name, n, notified)
		}
	}
}

// TestExtMsgInboundTranscriptRecoveryPersistentFault posts a normalized
// inbound whose head advance fails on every attempt: the request answers 5xx
// with a contextual error and notifies nobody, and a later retry of the same
// message publishes the entry the failed request stored, without a duplicate.
func TestExtMsgInboundTranscriptRecoveryPersistentFault(t *testing.T) {
	h := newTranscriptRecoveryHarness(t)
	h.fault.set(func(f *transcriptRecoveryFaultStore) { f.failNextSequenceWrites = 1000 })

	rec := h.postInbound(t, "m1", "hello persistent")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "appending transcript") {
		t.Fatalf("error body = %s, want the transcript append's context", rec.Body.String())
	}
	if notified := h.notifications("hello persistent"); len(notified) != 0 {
		t.Fatalf("failed inbound notified %v, want nobody", notified)
	}
	if n := h.inboundEvents(t); n != 0 {
		t.Fatalf("ExtMsgInbound events = %d after a failed inbound, want 0", n)
	}

	h.fault.set(func(f *transcriptRecoveryFaultStore) { f.failNextSequenceWrites = 0 })
	rec = h.postInbound(t, "m1", "hello persistent")
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if n := h.storedEntries(t); n != 1 {
		t.Fatalf("stored %d transcript entries after the retry, want 1 (no duplicate)", n)
	}
	if got := h.transcript(t); len(got) != 1 || got[0].ProviderMessageID != "m1" {
		t.Fatalf("transcript after the retry = %#v, want the stored m1 entry published", got)
	}
	notified := h.notifications("hello persistent")
	if notified[h.peerSessionName(t)] != 1 {
		t.Fatalf("peer notifications after the retry = %v, want one for %q", notified, h.peerSessionName(t))
	}
}
