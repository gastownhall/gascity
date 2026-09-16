package extmsg

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestSubscriberRegistry_PublishDelivered(t *testing.T) {
	r := NewSubscriberRegistry()
	ref := testConversationRef()

	ch, cancel := r.Subscribe(ref, 8)
	defer cancel()

	event := NewSSEMessageEvent("hello", "sess-1", ref, 1, time.Now())
	receipt := r.Publish(ref, event)
	if !receipt.Delivered {
		t.Fatalf("Publish: Delivered = false, want true")
	}
	if receipt.Dropped != 0 {
		t.Fatalf("Publish: Dropped = %d, want 0", receipt.Dropped)
	}

	got, ok := <-ch
	if !ok {
		t.Fatal("channel closed unexpectedly")
	}
	msg, ok := got.(SSEMessageEvent)
	if !ok {
		t.Fatalf("received event type %T, want SSEMessageEvent", got)
	}
	if msg.Text != "hello" {
		t.Fatalf("Text = %q, want %q", msg.Text, "hello")
	}
}

func TestSubscriberRegistry_PublishNoSubscriberNotDelivered(t *testing.T) {
	r := NewSubscriberRegistry()
	ref := testConversationRef()

	event := NewSSEMessageEvent("hi", "sess-1", ref, 1, time.Now())
	receipt := r.Publish(ref, event)
	if receipt.Delivered {
		t.Fatal("Publish with no subscriber: Delivered = true, want false")
	}
}

func TestSubscriberRegistry_DropOldestOnBufferFull(t *testing.T) {
	r := NewSubscriberRegistry()
	ref := testConversationRef()

	bufSize := 2
	ch, cancel := r.Subscribe(ref, bufSize)
	defer cancel()

	// Fill the buffer.
	r.Publish(ref, NewSSEMessageEvent("msg1", "s", ref, 1, time.Now()))
	r.Publish(ref, NewSSEMessageEvent("msg2", "s", ref, 2, time.Now()))

	// One more: buffer is full, oldest should be dropped.
	receipt := r.Publish(ref, NewSSEMessageEvent("msg3", "s", ref, 3, time.Now()))

	if receipt.Dropped == 0 {
		t.Fatal("expected a drop when buffer is full, got Dropped=0")
	}
	if r.DropCount(ref) == 0 {
		t.Fatal("DropCount should be non-zero after overflow")
	}

	// Drain the channel; should see msg2 and msg3 (msg1 was dropped).
	var texts []string
	for len(ch) > 0 {
		e := <-ch
		if msg, ok := e.(SSEMessageEvent); ok {
			texts = append(texts, msg.Text)
		}
	}
	if len(texts) != bufSize {
		t.Fatalf("expected %d events in channel, got %d: %v", bufSize, len(texts), texts)
	}
	if texts[0] == "msg1" {
		t.Fatalf("oldest event was not dropped; got %v", texts)
	}
}

func TestSubscriberRegistry_CancelRemovesEntry(t *testing.T) {
	r := NewSubscriberRegistry()
	ref := testConversationRef()

	_, cancel := r.Subscribe(ref, 4)
	cancel()

	// After cancel, Publish should not deliver.
	event := NewSSEMessageEvent("after-cancel", "s", ref, 1, time.Now())
	receipt := r.Publish(ref, event)
	if receipt.Delivered {
		t.Fatal("Publish after cancel: Delivered = true, want false")
	}
}

func TestSubscriberRegistry_PublishDoesNotPanicOnConcurrentCancel(t *testing.T) {
	// Regression: Publish must never send on a closed channel (panic) when a
	// cancel races it — the exact crash that happens when a session replies
	// at the same moment its SSE client disconnects. Run under -race to also
	// catch the data race the previous unlocked send carried.
	for iter := 0; iter < 300; iter++ {
		r := NewSubscriberRegistry()
		ref := testConversationRef()
		_, cancel := r.Subscribe(ref, 1)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// A send on a closed channel panics; recover and fail the test so
			// the regression is caught even without -race.
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("Publish panicked under concurrent cancel: %v", rec)
				}
			}()
			for i := 0; i < 64; i++ {
				r.Publish(ref, NewSSEMessageEvent("x", "s", ref, int64(i), time.Now()))
			}
		}()
		go func() {
			defer wg.Done()
			cancel()
		}()
		wg.Wait()
	}
}

func TestSubscriberRegistry_ResubscribeClosesOldChannel(t *testing.T) {
	r := NewSubscriberRegistry()
	ref := testConversationRef()

	oldCh, _ := r.Subscribe(ref, 4)
	// A second Subscribe for the same conversation must replace the first
	// without self-deadlocking (the old code called old.cancel() while holding
	// r.mu, which re-locks r.mu). Reaching the next line at all proves there is
	// no deadlock.
	newCh, cancel := r.Subscribe(ref, 4)
	defer cancel()

	// The replaced channel must be closed, not leaked.
	select {
	case _, ok := <-oldCh:
		if ok {
			t.Fatal("old channel delivered an event; want closed")
		}
	case <-time.After(time.Second):
		t.Fatal("old channel was not closed after resubscribe")
	}

	// The new channel is live and receives publishes.
	receipt := r.Publish(ref, NewSSEMessageEvent("hi", "s", ref, 1, time.Now()))
	if !receipt.Delivered {
		t.Fatal("Publish to resubscribed channel: Delivered = false, want true")
	}
	got, ok := <-newCh
	if !ok {
		t.Fatal("new channel closed unexpectedly")
	}
	if msg, ok := got.(SSEMessageEvent); !ok || msg.Text != "hi" {
		t.Fatalf("new channel got %#v, want message %q", got, "hi")
	}
}

func TestLLMClientAdapter_PublishRoutesToRegistry(t *testing.T) {
	registry := NewSubscriberRegistry()
	ref := testConversationRef()
	ref.Provider = ProviderLLMClient
	ref.AccountID = "client-bead-1"

	adapter := NewLLMClientAdapter("client-bead-1", registry)
	if adapter.Name() == "" {
		t.Fatal("adapter Name() is empty")
	}
	caps := adapter.Capabilities()
	if caps.SupportsChildConversations {
		t.Fatal("llm-client adapter should not support child conversations")
	}
	if caps.SupportsAttachments {
		t.Fatal("llm-client adapter should not support attachments")
	}

	ch, cancel := registry.Subscribe(ref, 4)
	defer cancel()

	req := PublishRequest{
		SessionID:    "sess-mayor",
		Conversation: ref,
		Text:         "the answer is 42",
	}
	receipt, err := adapter.Publish(t.Context(), req)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if receipt == nil {
		t.Fatal("Publish returned nil receipt")
	}
	if !receipt.Delivered {
		t.Fatalf("Publish: Delivered = false, want true")
	}

	got, ok := <-ch
	if !ok {
		t.Fatal("channel closed unexpectedly")
	}
	msg, ok := got.(SSEMessageEvent)
	if !ok {
		t.Fatalf("received event type %T, want SSEMessageEvent", got)
	}
	if msg.Text != "the answer is 42" {
		t.Fatalf("Text = %q, want %q", msg.Text, "the answer is 42")
	}
	if msg.SessionID != "sess-mayor" {
		t.Fatalf("SessionID = %q, want %q", msg.SessionID, "sess-mayor")
	}
}

func TestLLMClientAdapter_PublishNoSubscriberDeliveredFalse(t *testing.T) {
	registry := NewSubscriberRegistry()
	ref := testConversationRef()
	ref.Provider = ProviderLLMClient
	ref.AccountID = "client-bead-1"

	adapter := NewLLMClientAdapter("client-bead-1", registry)
	req := PublishRequest{
		SessionID:    "sess-mayor",
		Conversation: ref,
		Text:         "will not be delivered",
	}
	receipt, err := adapter.Publish(t.Context(), req)
	if err != nil {
		t.Fatalf("Publish with no subscriber: unexpected error: %v", err)
	}
	if receipt.Delivered {
		t.Fatal("Publish with no subscriber: Delivered = true, want false")
	}
}

func TestSSEEventRoundTrip(t *testing.T) {
	t.Run("message", func(t *testing.T) {
		ts := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
		ref := ConversationRef{
			Provider:       ProviderLLMClient,
			AccountID:      "client-1",
			ConversationID: "conv-abc",
			Kind:           ConversationDM,
		}
		orig := NewSSEMessageEvent("hello", "sess-1", ref, 42, ts)
		b, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got SSEMessageEvent
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Version != "1" {
			t.Errorf("Version = %q, want 1", got.Version)
		}
		if got.Event != SSEEventTypeMessage {
			t.Errorf("Event = %q, want %q", got.Event, SSEEventTypeMessage)
		}
		if got.Text != "hello" {
			t.Errorf("Text = %q, want hello", got.Text)
		}
		if got.Sequence != 42 {
			t.Errorf("Sequence = %d, want 42", got.Sequence)
		}
		if !got.CreatedAt.Equal(ts) {
			t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, ts)
		}
	})

	t.Run("heartbeat", func(t *testing.T) {
		ts := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
		orig := NewSSEHeartbeatEvent(ts)
		b, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got SSEHeartbeatEvent
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Version != "1" {
			t.Errorf("Version = %q, want 1", got.Version)
		}
		if got.Event != SSEEventTypeHeartbeat {
			t.Errorf("Event = %q, want %q", got.Event, SSEEventTypeHeartbeat)
		}
		if !got.TS.Equal(ts) {
			t.Errorf("TS = %v, want %v", got.TS, ts)
		}
	})

	t.Run("error_retryable", func(t *testing.T) {
		ms := int64(5000)
		orig := NewSSEErrorEvent("session_stopped", "Session has stopped.", true, &ms)
		b, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got SSEErrorEvent
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Version != "1" {
			t.Errorf("Version = %q, want 1", got.Version)
		}
		if got.Event != SSEEventTypeError {
			t.Errorf("Event = %q, want %q", got.Event, SSEEventTypeError)
		}
		if !got.Retryable {
			t.Error("Retryable = false, want true")
		}
		if got.RetryAfterMs == nil || *got.RetryAfterMs != 5000 {
			t.Errorf("RetryAfterMs = %v, want 5000", got.RetryAfterMs)
		}
	})

	t.Run("error_non_retryable_omits_retry_after_ms", func(t *testing.T) {
		orig := NewSSEErrorEvent("auth_failed", "Invalid token.", false, nil)
		b, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Contains(b, []byte("retry_after_ms")) {
			t.Errorf("non-retryable error should omit retry_after_ms, got: %s", b)
		}
		var got SSEErrorEvent
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Retryable {
			t.Error("Retryable = true, want false")
		}
		if got.RetryAfterMs != nil {
			t.Errorf("RetryAfterMs = %v, want nil", got.RetryAfterMs)
		}
	})
}
