package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// transcriptCreateFaultStore drives the inbound handler's single bounded
// replay: commitAndFail arms a one-shot fault that commits the transcript
// entry create to the backing store and then answers it with an error (the
// ambiguous-create case), while failAll makes every transcript entry create
// fail outright (a fault that persists through the replay).
type transcriptCreateFaultStore struct {
	beads.Store
	commitAndFail atomic.Bool
	failAll       atomic.Bool
	attempts      atomic.Int32
}

func beadHasTranscriptLabel(b beads.Bead) bool {
	for _, label := range b.Labels {
		if label == "gc:extmsg-transcript" {
			return true
		}
	}
	return false
}

func (s *transcriptCreateFaultStore) Create(b beads.Bead) (beads.Bead, error) {
	if beadHasTranscriptLabel(b) {
		s.attempts.Add(1)
		if s.failAll.Load() {
			return beads.Bead{}, errors.New("i/o timeout")
		}
		if s.commitAndFail.CompareAndSwap(true, false) {
			if _, err := s.Store.Create(b); err != nil {
				return beads.Bead{}, err
			}
			return beads.Bead{}, errors.New("invalid connection")
		}
	}
	return s.Store.Create(b)
}

// newInboundReplayFixture builds the handler stack over a faulting transcript
// store: one bound member session, so a successful inbound delivery both
// stores exactly one transcript entry and schedules exactly one member
// notification.
func newInboundReplayFixture(t *testing.T, store *transcriptCreateFaultStore) (*fakeState, *Server, *extmsg.Services, extmsg.ConversationRef, string) {
	t.Helper()
	fs := newSessionFakeState(t)
	srv := New(fs)
	t.Cleanup(srv.waitForBackground)

	services := extmsg.NewServices(beadstest.Mail(store), beadstest.Sessions(fs.cityBeadStore))
	fs.extmsgSvc = &services
	fs.adapterReg = extmsg.NewAdapterRegistry()

	worker := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker")
	ref := extmsg.ConversationRef{
		ScopeID:        "guild-1",
		Provider:       "discord",
		AccountID:      "acct-1",
		ConversationID: "thread-replay",
		Kind:           extmsg.ConversationThread,
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	now := time.Now().UTC()
	if _, err := services.Bindings.Bind(t.Context(), caller, extmsg.BindInput{
		Conversation: ref,
		SessionID:    worker.ID,
		Now:          now,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if _, err := services.Transcript.EnsureMembership(t.Context(), extmsg.EnsureMembershipInput{
		Caller:       caller,
		Conversation: ref,
		SessionID:    worker.ID,
		Now:          now,
	}); err != nil {
		t.Fatalf("EnsureMembership: %v", err)
	}
	return fs, srv, &services, ref, worker.SessionName
}

// TestExtMsgInboundReplayRecoversStoredButErroredTranscriptAppend pins the
// adapter-facing contract for the ambiguous transcript append: the entry
// commits but the handler's first invocation fails, so the inbound handler
// replays HandleInboundNormalized exactly once. The replay must find the
// stored entry (no second create), answer 200 with it, and schedule exactly
// one member notification — a message the provider already accepted must not
// be double-delivered or silently dropped.
func TestExtMsgInboundReplayRecoversStoredButErroredTranscriptAppend(t *testing.T) {
	store := &transcriptCreateFaultStore{Store: beads.NewMemStore()}
	store.commitAndFail.Store(true)
	fs, srv, services, ref, sessionName := newInboundReplayFixture(t, store)

	rec := postExtMsg(t, fs, srv, "/extmsg/inbound", map[string]any{
		"message": map[string]any{
			"provider_message_id": "msg-replay-1",
			"conversation":        conversationBody(ref),
			"actor":               map[string]any{"id": "user-1", "display_name": "User One", "is_bot": false},
			"text":                "hello replay",
			"received_at":         time.Now().UTC().Format(time.RFC3339),
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var result extmsg.InboundResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.TranscriptEntry == nil {
		t.Fatalf("response carries no transcript entry: %+v", result)
	}
	if result.TranscriptEntry.ProviderMessageID != "msg-replay-1" {
		t.Fatalf("transcript entry provider message id = %q", result.TranscriptEntry.ProviderMessageID)
	}

	if got := store.attempts.Load(); got != 1 {
		t.Fatalf("transcript create attempts = %d, want 1 (the replay reuses the stored entry)", got)
	}
	entries, err := services.Transcript.List(t.Context(), extmsg.ListTranscriptInput{
		Caller:       extmsg.Caller{Kind: extmsg.CallerController, ID: "test"},
		Conversation: ref,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stored %d transcript entries, want 1", len(entries))
	}

	srv.waitForBackground()
	if got := countSessionNudges(fs.sp, sessionName, "hello replay"); got != 1 {
		t.Fatalf("member notifications = %d, want 1", got)
	}
}
