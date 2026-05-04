package mail //nolint:revive // internal package, always imported qualified

import (
	"crypto/rand"
	"fmt"
	"sort"
	"sync"
	"time"
)

// fakeMsg tracks a message with its read/archived status.
type fakeMsg struct {
	msg      Message
	read     bool
	archived bool
}

// Fake is an in-memory mail provider for testing. It records messages and
// supports all Provider operations. Safe for concurrent use.
//
// When broken is true (via [NewFailFake]), all operations return errors.
type Fake struct {
	mu       sync.Mutex
	messages []fakeMsg
	seq      int
	broken   bool
}

// NewFake returns a ready-to-use in-memory mail provider.
func NewFake() *Fake {
	return &Fake{}
}

// NewFailFake returns a mail provider where all operations return errors.
// Useful for testing error paths.
func NewFailFake() *Fake {
	return &Fake{broken: true}
}

// Send creates a message in memory.
func (f *Fake) Send(from, to, subject, body string) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return Message{}, fmt.Errorf("mail provider unavailable")
	}
	f.seq++
	threadID := fakeThreadID()
	m := Message{
		ID:        fmt.Sprintf("fake-%d", f.seq),
		From:      from,
		To:        to,
		Subject:   subject,
		Body:      body,
		CreatedAt: time.Now(),
		ThreadID:  threadID,
	}
	f.messages = append(f.messages, fakeMsg{msg: m})
	return m, nil
}

// Inbox returns unread, non-archived messages for the recipient.
func (f *Fake) Inbox(recipient string) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return nil, fmt.Errorf("mail provider unavailable")
	}
	var result []Message
	for _, fm := range f.messages {
		if fm.msg.To == recipient && !fm.read && !fm.archived {
			result = append(result, fm.msg)
		}
	}
	return result, nil
}

// Get returns a message by ID without marking it as read.
func (f *Fake) Get(id string) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return Message{}, fmt.Errorf("mail provider unavailable")
	}
	for _, fm := range f.messages {
		if fm.msg.ID == id {
			msg := fm.msg
			msg.Read = fm.read
			return msg, nil
		}
	}
	return Message{}, fmt.Errorf("getting message %q: %w", id, ErrNotFound)
}

// Read returns a message by ID and marks it as read.
func (f *Fake) Read(id string) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return Message{}, fmt.Errorf("mail provider unavailable")
	}
	for i := range f.messages {
		if f.messages[i].msg.ID == id {
			f.messages[i].read = true
			msg := f.messages[i].msg
			msg.Read = true
			return msg, nil
		}
	}
	return Message{}, fmt.Errorf("reading message %q: %w", id, ErrNotFound)
}

// MarkRead marks a message as read.
func (f *Fake) MarkRead(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return fmt.Errorf("mail provider unavailable")
	}
	for i := range f.messages {
		if f.messages[i].msg.ID == id {
			f.messages[i].read = true
			return nil
		}
	}
	return fmt.Errorf("marking message %q read: %w", id, ErrNotFound)
}

// MarkUnread marks a message as unread.
func (f *Fake) MarkUnread(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return fmt.Errorf("mail provider unavailable")
	}
	for i := range f.messages {
		if f.messages[i].msg.ID == id {
			f.messages[i].read = false
			return nil
		}
	}
	return fmt.Errorf("marking message %q unread: %w", id, ErrNotFound)
}

// Archive closes a message without reading it.
func (f *Fake) Archive(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return fmt.Errorf("mail provider unavailable")
	}
	for i := range f.messages {
		if f.messages[i].msg.ID == id {
			if f.messages[i].archived {
				return ErrAlreadyArchived
			}
			f.messages[i].archived = true
			return nil
		}
	}
	return fmt.Errorf("archiving message %q: %w", id, ErrNotFound)
}

// Delete is an alias for Archive.
func (f *Fake) Delete(id string) error {
	return f.Archive(id)
}

// ArchiveMany archives a batch of messages by looping over [Fake.Archive],
// preserving per-id error reporting including [ErrAlreadyArchived].
func (f *Fake) ArchiveMany(ids []string) ([]ArchiveResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	results := make([]ArchiveResult, len(ids))
	for i, id := range ids {
		results[i] = ArchiveResult{ID: id, Err: f.Archive(id)}
	}
	return results, nil
}

// DeleteMany deletes a batch of messages by looping over [Fake.Delete],
// preserving per-id error reporting including [ErrAlreadyArchived].
func (f *Fake) DeleteMany(ids []string) ([]ArchiveResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	results := make([]ArchiveResult, len(ids))
	for i, id := range ids {
		results[i] = ArchiveResult{ID: id, Err: f.Delete(id)}
	}
	return results, nil
}

// All returns all open messages (read and unread) for the recipient.
func (f *Fake) All(recipient string) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return nil, fmt.Errorf("mail provider unavailable")
	}
	var result []Message
	for _, fm := range f.messages {
		if fm.msg.To == recipient && !fm.archived {
			msg := fm.msg
			msg.Read = fm.read
			result = append(result, msg)
		}
	}
	return result, nil
}

// Check returns unread messages for the recipient without marking them read.
func (f *Fake) Check(recipient string) ([]Message, error) {
	return f.Inbox(recipient)
}

// Reply creates a reply to an existing message.
func (f *Fake) Reply(id, from, subject, body string) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return Message{}, fmt.Errorf("mail provider unavailable")
	}

	var original *fakeMsg
	for i := range f.messages {
		if f.messages[i].msg.ID == id {
			original = &f.messages[i]
			break
		}
	}
	if original == nil {
		return Message{}, fmt.Errorf("replying to %q: %w", id, ErrNotFound)
	}

	threadID := original.msg.ThreadID
	if threadID == "" {
		threadID = fakeThreadID()
	}

	f.seq++
	m := Message{
		ID:        fmt.Sprintf("fake-%d", f.seq),
		From:      from,
		To:        original.msg.From, // reply to sender
		Subject:   subject,
		Body:      body,
		CreatedAt: time.Now(),
		ThreadID:  threadID,
		ReplyTo:   id,
	}
	f.messages = append(f.messages, fakeMsg{msg: m})
	return m, nil
}

// Thread returns all messages sharing a thread ID, ordered by time.
func (f *Fake) Thread(threadID string) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return nil, fmt.Errorf("mail provider unavailable")
	}
	var result []Message
	for _, fm := range f.messages {
		if fm.msg.ThreadID == threadID {
			msg := fm.msg
			msg.Read = fm.read
			result = append(result, msg)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

// Count returns (total, unread) message counts for a recipient.
func (f *Fake) Count(recipient string) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return 0, 0, fmt.Errorf("mail provider unavailable")
	}
	var total, unread int
	for _, fm := range f.messages {
		if fm.msg.To == recipient && !fm.archived {
			total++
			if !fm.read {
				unread++
			}
		}
	}
	return total, unread, nil
}

// Messages returns a copy of all messages currently stored, regardless of status.
func (f *Fake) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]Message, len(f.messages))
	for i, fm := range f.messages {
		result[i] = fm.msg
	}
	return result
}

// fakeThreadID generates a simple thread ID for the fake provider.
func fakeThreadID() string {
	b := make([]byte, 6)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("thread-%x", b)
}
