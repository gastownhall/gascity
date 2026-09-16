package extmsg

import "sync"

// SSEPublishReceipt from SubscriberRegistry.Publish tracks whether the event was
// delivered and how many events were dropped on a full buffer.
type SSEPublishReceipt struct {
	Delivered bool
	Dropped   int
}

// subscriberEntry holds the channel and drop counter for one subscriber.
type subscriberEntry struct {
	ch    chan SSEEvent
	drops int
}

// SubscriberRegistry is a goroutine-safe registry of SSE event subscribers
// keyed by ConversationRef. At most one subscriber per conversation is
// supported; a new Subscribe call replaces a stale entry.
type SubscriberRegistry struct {
	mu          sync.Mutex
	subscribers map[string]*subscriberEntry
}

// NewSubscriberRegistry creates an empty subscriber registry.
func NewSubscriberRegistry() *SubscriberRegistry {
	return &SubscriberRegistry{
		subscribers: make(map[string]*subscriberEntry),
	}
}

// Subscribe creates a buffered channel for the given conversation. A second
// call for the same conversation cancels the first entry and replaces it.
// The returned cancel func removes the entry from the registry and closes
// the channel.
func (r *SubscriberRegistry) Subscribe(ref ConversationRef, bufSize int) (<-chan SSEEvent, func()) {
	key := conversationLockKey(ref)
	ch := make(chan SSEEvent, bufSize)

	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if e, ok := r.subscribers[key]; ok && e.ch == ch {
				delete(r.subscribers, key)
				close(ch)
			}
		})
	}

	r.mu.Lock()
	// Replace-on-resubscribe: close the previous channel directly while we
	// hold r.mu. We must not call the old entry's cancel here — it re-locks
	// r.mu (non-reentrant) and would self-deadlock, and once the map entry is
	// overwritten below, the old cancel's "still the live entry" guard would
	// skip the close and leak the old channel. Closing under r.mu is
	// serialized with Publish (which sends under r.mu), so no send can race
	// this close.
	if old, ok := r.subscribers[key]; ok {
		delete(r.subscribers, key)
		close(old.ch)
	}
	r.subscribers[key] = &subscriberEntry{ch: ch}
	r.mu.Unlock()

	return ch, cancel
}

// Publish delivers an event to the subscriber for the given conversation.
// If there is no subscriber, Delivered is false and no error is returned.
// If the subscriber's buffer is full, the oldest queued event is dropped
// and the new event is written; the drop count is tracked on the entry.
// Publish is non-blocking.
func (r *SubscriberRegistry) Publish(ref ConversationRef, event SSEEvent) SSEPublishReceipt {
	key := conversationLockKey(ref)

	// The send happens under r.mu. The cancel path closes the channel and
	// removes the entry from the map under the same lock, so an entry found
	// here is guaranteed to have an open channel — a send on a closed channel
	// panics even inside a select/default. These channel ops are all
	// non-blocking, so holding the lock for them is cheap and makes the
	// publish/cancel race impossible.
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.subscribers[key]
	if !ok {
		return SSEPublishReceipt{Delivered: false}
	}

	dropped := 0
	// Non-blocking send: if the buffer is full, drop the oldest queued event
	// and retry once.
	select {
	case entry.ch <- event:
	default:
		select {
		case <-entry.ch:
			dropped = 1
		default:
		}
		select {
		case entry.ch <- event:
		default:
			dropped++
			entry.drops += dropped
			return SSEPublishReceipt{Delivered: false, Dropped: dropped}
		}
	}

	entry.drops += dropped
	return SSEPublishReceipt{Delivered: true, Dropped: dropped}
}

// DropCount returns the number of events dropped for the conversation due to
// buffer overflow.
func (r *SubscriberRegistry) DropCount(ref ConversationRef) int {
	key := conversationLockKey(ref)
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.subscribers[key]; ok {
		return e.drops
	}
	return 0
}
