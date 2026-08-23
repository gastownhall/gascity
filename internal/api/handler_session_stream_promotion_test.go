package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestSessionStreamResumeTokenPrefersLastEventID(t *testing.T) {
	for _, tc := range []struct {
		name        string
		lastEventID string
		afterCursor string
		want        string
	}{
		{name: "query only", afterCursor: "query-cursor", want: "query-cursor"},
		{name: "header only", lastEventID: "header-cursor", want: "header-cursor"},
		{name: "header wins", lastEventID: " header-cursor ", afterCursor: "query-cursor", want: "header-cursor"},
		{name: "blank header falls back", lastEventID: "  ", afterCursor: " query-cursor ", want: "query-cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionStreamResumeToken(tc.lastEventID, tc.afterCursor); got != tc.want {
				t.Fatalf("sessionStreamResumeToken(%q, %q) = %q, want %q", tc.lastEventID, tc.afterCursor, got, tc.want)
			}
		})
	}
}

func TestStructuredPeekPromotionClearsResolvedPendingInteraction(t *testing.T) {
	for _, transport := range []string{"legacy", "huma"} {
		t.Run(transport, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			handle := &structuredPromotionHandle{}
			info := session.Info{
				ID:          "session-1",
				SessionKey:  "provider-session-1",
				SessionName: "worker-1",
				Template:    "worker",
				Provider:    "test",
			}
			srv := &Server{}
			done := make(chan struct{})
			cleared := make(chan struct{}, 1)

			switch transport {
			case "legacy":
				rec := newSyncResponseRecorder()
				go func() {
					srv.streamSessionPeekStructured(ctx, rec, info, handle, false, "")
					close(done)
				}()
				go func() {
					deadline := time.NewTicker(5 * time.Millisecond)
					defer deadline.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-deadline.C:
							if strings.Contains(rec.BodyString(), "event: pending_cleared") {
								cleared <- struct{}{}
								return
							}
						}
					}
				}()
			case "huma":
				go func() {
					srv.streamSessionPeekStructuredHuma(ctx, func(msg StringIDMessage) error {
						if _, ok := msg.Data.(SessionPendingClearedEvent); ok {
							cleared <- struct{}{}
						}
						return nil
					}, info, handle, false, "")
					close(done)
				}()
			}

			select {
			case <-cleared:
			case <-time.After(250 * time.Millisecond):
				t.Fatal("fallback-to-history promotion did not clear the resolved pending interaction")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("structured stream did not stop after cancellation")
			}
		})
	}
}

func TestStructuredPeekEmitsSameRequestPendingUpdates(t *testing.T) {
	for _, transport := range []string{"legacy", "huma"} {
		t.Run(transport, func(t *testing.T) {
			fs := newSessionFakeState(t)
			srv := New(fs)
			info := session.Info{ID: "session-1", SessionName: "worker-1", Template: "worker", Provider: "test"}
			handle := &mutableStructuredPendingHandle{output: "fallback output"}
			handle.SetPending(&worker.PendingInteraction{RequestID: "request-1", Kind: "approval", Prompt: "Proceed?"})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			var waitForPrompt func(string)

			switch transport {
			case "legacy":
				rec := newSyncResponseRecorder()
				go func() {
					srv.streamSessionPeekStructured(ctx, rec, info, handle, false, "")
					close(done)
				}()
				waitForPrompt = func(prompt string) {
					if body := waitForRecorderSubstring(t, rec, prompt, testutil.GoroutineRaceTimeout); !strings.Contains(body, prompt) {
						t.Fatalf("structured stream body missing pending prompt %q: %s", prompt, body)
					}
				}
			case "huma":
				prompts := make(chan string, 2)
				go func() {
					srv.streamSessionPeekStructuredHuma(ctx, func(msg StringIDMessage) error {
						raw, _ := json.Marshal(msg.Data)
						var pending struct {
							Prompt string `json:"prompt"`
						}
						if json.Unmarshal(raw, &pending) == nil && pending.Prompt != "" {
							prompts <- pending.Prompt
						}
						return nil
					}, info, handle, false, "")
					close(done)
				}()
				waitForPrompt = func(want string) {
					select {
					case got := <-prompts:
						if got != want {
							t.Fatalf("pending prompt = %q, want %q", got, want)
						}
					case <-time.After(testutil.GoroutineRaceTimeout):
						t.Fatalf("structured stream missing pending prompt %q", want)
					}
				}
			}

			waitForPrompt("Proceed?")
			handle.SetPending(&worker.PendingInteraction{RequestID: "request-1", Kind: "approval", Prompt: "Updated prompt"})
			fs.eventProv.(*events.Fake).Record(events.Event{Type: events.WorkerOperation, Subject: info.ID})
			waitForPrompt("Updated prompt")

			cancel()
			select {
			case <-done:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("structured stream did not stop after cancellation")
			}
		})
	}
}

type structuredPromotionHandle struct {
	worker.Handle

	mu           sync.Mutex
	pendingReads int
}

func (h *structuredPromotionHandle) Peek(context.Context, int) (string, error) {
	return "fallback output", nil
}

func (h *structuredPromotionHandle) History(context.Context, worker.HistoryRequest) (*worker.HistorySnapshot, error) {
	return &worker.HistorySnapshot{
		GCSessionID:           "provider-session-1",
		LogicalConversationID: "provider-session-1",
		ProviderSessionID:     "provider-session-1",
		TranscriptStreamID:    "stream-1",
		Generation:            worker.Generation{ID: "generation-1"},
		Cursor:                worker.Cursor{AfterEntryID: "history-1"},
		Continuity:            worker.Continuity{Status: worker.ContinuityStatusContinuous},
		TailState: worker.TailState{
			Activity:    worker.TailActivityIdle,
			LastEntryID: "history-1",
		},
		Entries: []worker.HistoryEntry{{
			ID:     "history-1",
			Kind:   "assistant",
			Actor:  worker.ActorAssistant,
			Status: worker.ResultStatusFinal,
			Text:   "history output",
		}},
	}, nil
}

func (h *structuredPromotionHandle) Pending(context.Context) (*worker.PendingInteraction, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pendingReads++
	if h.pendingReads == 1 {
		return &worker.PendingInteraction{RequestID: "request-1", Kind: "approval", Prompt: "Proceed?"}, nil
	}
	return nil, nil
}

func (h *structuredPromotionHandle) TranscriptPath(context.Context) (string, error) {
	return "", errors.New("no transcript path for synthetic promotion test")
}

var _ worker.Handle = (*structuredPromotionHandle)(nil)

type mutableStructuredPendingHandle struct {
	worker.Handle

	mu      sync.Mutex
	output  string
	pending *worker.PendingInteraction
}

func (h *mutableStructuredPendingHandle) Peek(context.Context, int) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.output, nil
}

func (h *mutableStructuredPendingHandle) History(context.Context, worker.HistoryRequest) (*worker.HistorySnapshot, error) {
	return nil, worker.ErrHistoryUnavailable
}

func (h *mutableStructuredPendingHandle) Pending(context.Context) (*worker.PendingInteraction, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return clonePendingInteraction(h.pending), nil
}

func (h *mutableStructuredPendingHandle) TranscriptPath(context.Context) (string, error) {
	return "", worker.ErrHistoryUnavailable
}

func (h *mutableStructuredPendingHandle) SetPending(pending *worker.PendingInteraction) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pending = clonePendingInteraction(pending)
}

func clonePendingInteraction(pending *worker.PendingInteraction) *worker.PendingInteraction {
	if pending == nil {
		return nil
	}
	cloned := *pending
	cloned.Options = append([]string(nil), pending.Options...)
	cloned.Metadata = cloneStringMap(pending.Metadata)
	return &cloned
}

var _ worker.Handle = (*mutableStructuredPendingHandle)(nil)
