// Package mail defines the pluggable mail provider interface for Gas City.
// The primary extension point is the exec script protocol (see
// internal/mail/exec); the Go interface exists for code organization and
// testability.
package mail //nolint:revive // internal package, always imported qualified

import (
	"errors"
	"time"
)

// ErrAlreadyArchived is returned by [Provider.Archive] when the message
// has already been archived. CLI code uses this to print a distinct message.
var ErrAlreadyArchived = errors.New("already archived")

// ErrNotFound is returned when a message ID does not exist.
var ErrNotFound = errors.New("message not found")

const (
	// FromSessionIDMetadataKey stores the stable session bead ID used for
	// reply routing when a message's display sender may later be renamed.
	FromSessionIDMetadataKey = "mail.from_session_id"
	// FromDisplayMetadataKey stores the human-readable sender captured when
	// the message was created.
	FromDisplayMetadataKey = "mail.from_display"
	// ToSessionIDMetadataKey stores the stable recipient session bead ID used
	// for routing replies while keeping the public To field human-readable.
	ToSessionIDMetadataKey = "mail.to_session_id"
	// ToDisplayMetadataKey stores the human-readable recipient captured when
	// the message was created.
	ToDisplayMetadataKey = "mail.to_display"
)

// Message represents a mail message between agents or humans.
type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	Read      bool      `json:"read"`
	ThreadID  string    `json:"thread_id,omitempty"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Priority  int       `json:"priority,omitempty"`
	CC        []string  `json:"cc,omitempty"`
	Rig       string    `json:"rig,omitempty"`
}

// ArchiveResult is one message's outcome in a batch [Provider.ArchiveMany] or
// [Provider.DeleteMany] call. Err is nil for a newly-closed message,
// [ErrAlreadyArchived] for an idempotent re-close, or a provider error.
type ArchiveResult struct {
	ID  string
	Err error
}

// Provider is the internal interface for mail backends. Implementations
// include beadmail (built-in default backed by beads.Store) and exec
// (user-supplied script via fork/exec).
type Provider interface {
	// Send creates a message. Subject is the summary line, body is the
	// full content. Returns the created message with assigned ID.
	Send(from, to, subject, body string) (Message, error)

	// Inbox returns unread messages for the recipient.
	Inbox(recipient string) ([]Message, error)

	// Get retrieves a message by ID without marking it read.
	Get(id string) (Message, error)

	// Read retrieves a message by ID and marks it as read.
	// The message remains in the store (not closed).
	Read(id string) (Message, error)

	// MarkRead marks a message as read (adds "read" label).
	MarkRead(id string) error

	// MarkUnread marks a message as unread (removes "read" label).
	MarkUnread(id string) error

	// Archive closes a message bead (removes from all views).
	Archive(id string) error

	// ArchiveMany archives a batch of messages in one round-trip where the
	// backend supports it, returning per-id results in input order.
	// Implementations MUST preserve per-id error reporting.
	ArchiveMany(ids []string) ([]ArchiveResult, error)

	// Delete is an alias for Archive (closes the bead).
	Delete(id string) error

	// DeleteMany deletes a batch of messages in one round-trip where the
	// backend supports it, returning per-id results in input order.
	// Implementations MUST preserve delete semantics and per-id error
	// reporting.
	DeleteMany(ids []string) ([]ArchiveResult, error)

	// Check returns unread messages without marking them read.
	Check(recipient string) ([]Message, error)

	// Reply creates a reply to an existing message. Inherits ThreadID
	// from the original, sets ReplyTo to the original's ID.
	Reply(id, from, subject, body string) (Message, error)

	// Thread returns all messages sharing a thread ID, ordered by time.
	Thread(threadID string) ([]Message, error)

	// All returns all open messages (read and unread) for the recipient.
	All(recipient string) ([]Message, error)

	// Count returns (total, unread) message counts for a recipient.
	Count(recipient string) (total int, unread int, err error)
}
